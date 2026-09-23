package httpapi

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

func testUsageRows() []store.UsageByStep {
	return []store.UsageByStep{
		{Step: "analyze", Model: "gemini-3.5-flash-lite", Calls: 1, InputTokens: 24_580, OutputTokens: 1_210, ThinkingTokens: 3_050, CostUSD: 0.1},
		{Step: "review", Model: "gemini-3.5-flash", Calls: 2, InputTokens: 9_000, OutputTokens: 550, ThinkingTokens: 1_300, CostUSD: 0.2},
		{Step: "analyze", Model: "gemini-2.5-pro", Calls: 1, InputTokens: 100, OutputTokens: 10, CostUSD: 0.0000004},
	}
}

func TestBuildUsage(t *testing.T) {
	got := buildUsage(testUsageRows())
	assert.Equal(t, JobUsage{
		Calls:          4,
		InputTokens:    33_680,
		OutputTokens:   1_770,
		ThinkingTokens: 4_350,
		TotalTokens:    39_800,
		CostUSD:        0.3, // not 0.30000040000000003
		TestMode:       false,
		CostComplete:   false, // gemini-3.5-flash and gemini-2.5-pro have no price
		Steps: []UsageStep{
			{Step: "analyze", Model: "gemini-3.5-flash-lite", Calls: 1, InputTokens: 24_580, OutputTokens: 1_210, ThinkingTokens: 3_050, CostUSD: 0.1, Priced: true},
			{Step: "review", Model: "gemini-3.5-flash", Calls: 2, InputTokens: 9_000, OutputTokens: 550, ThinkingTokens: 1_300, CostUSD: 0.2},
			{Step: "analyze", Model: "gemini-2.5-pro", Calls: 1, InputTokens: 100, OutputTokens: 10, CostUSD: 0},
		},
	}, got, "steps keep the store's order; costs are rounded to 6 decimals")

	// Rounding applies per step and to the total (half away from zero).
	got = buildUsage([]store.UsageByStep{{Step: "analyze", Model: "m", Calls: 1, CostUSD: 0.0012345678}})
	assert.Equal(t, 0.001235, got.Steps[0].CostUSD)
	assert.Equal(t, 0.001235, got.CostUSD)

	for name, rows := range map[string][]store.UsageByStep{"nil": nil, "empty": {}} {
		zero := buildUsage(rows)
		assert.Equal(t, JobUsage{Steps: []UsageStep{}, CostComplete: true}, zero, name, "no calls: nothing to price, not test mode")
		assert.NotNil(t, zero.Steps, name)
	}
}

func TestBuildUsagePricedAndTestMode(t *testing.T) {
	fakeStep := func(step, model string) store.UsageByStep {
		return store.UsageByStep{Step: step, Model: model, Calls: 1, InputTokens: 3500, OutputTokens: 300}
	}
	for _, tc := range []struct {
		name               string
		rows               []store.UsageByStep
		priced             []bool
		testMode, complete bool
	}{
		{
			name:   "test analyzer only: free, and complete",
			rows:   []store.UsageByStep{fakeStep("analyze", "fake:gemini-3.5-flash-lite"), fakeStep("review", "fake:gemini-3.8-flash")},
			priced: []bool{true, true}, testMode: true, complete: true,
		},
		{
			name:   "real, priced models (versioned name, models/ prefix)",
			rows:   []store.UsageByStep{fakeStep("analyze", "gemini-3.5-flash-lite-preview-10-2026"), fakeStep("review", "models/gemini-3.8-flash")},
			priced: []bool{true, true}, testMode: false, complete: true,
		},
		{
			// Recorded at cost 0 by the worker, but Google billed it: not "free test mode".
			name:   "a real model missing from the price table",
			rows:   []store.UsageByStep{fakeStep("analyze", "gemini-9-ultra")},
			priced: []bool{false}, testMode: false, complete: false,
		},
		{
			name:   "test analyzer mixed with a real model",
			rows:   []store.UsageByStep{fakeStep("analyze", "fake:gemini-3.5-flash-lite"), fakeStep("review", "gemini-3.8-flash")},
			priced: []bool{true, true}, testMode: false, complete: true,
		},
		{
			name:   "the prefix must lead",
			rows:   []store.UsageByStep{fakeStep("analyze", "gemini-fake:x"), fakeStep("review", "FAKE:x")},
			priced: []bool{false, false}, testMode: false, complete: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildUsage(tc.rows)
			var priced []bool
			for _, st := range got.Steps {
				priced = append(priced, st.Priced)
			}
			assert.Equal(t, tc.priced, priced)
			assert.Equal(t, tc.testMode, got.TestMode, "testMode")
			assert.Equal(t, tc.complete, got.CostComplete, "costComplete")
		})
	}
}

func TestGetJobReportsUsageForEveryStatus(t *testing.T) {
	for _, status := range []store.JobStatus{
		store.StatusQueued, store.StatusProcessing, store.StatusCompleted, store.StatusRejected, store.StatusFailed,
	} {
		t.Run(string(status), func(t *testing.T) {
			e := newEnv(t)
			j := e.store.addJob(store.Job{DomainKey: "cars", Status: status})
			e.store.setJobUsage(j.ID, testUsageRows()...)
			e.store.setJobUsage(ids.NewJob(), store.UsageByStep{Step: "analyze", Model: "other job", Calls: 9, CostUSD: 9})

			got := decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+j.ID))
			assert.Equal(t, buildUsage(testUsageRows()), got.Usage)
			assert.Equal(t, int64(39_800), got.Usage.TotalTokens)
			assert.Equal(t, 0.3, got.Usage.CostUSD)
			assert.Contains(t, e.store.jobUsageCalls, j.ID)
		})
	}
}

func TestGetJobZeroUsage(t *testing.T) {
	e := newEnv(t)
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusProcessing})

	raw := rawJob(t, e, j.ID)
	require.Contains(t, raw, "usage")
	assert.Equal(t, map[string]any{
		"calls": 0.0, "inputTokens": 0.0, "outputTokens": 0.0, "thinkingTokens": 0.0, "totalTokens": 0.0,
		"costUsd": 0.0, "testMode": false, "costComplete": true, "steps": []any{},
	}, raw["usage"], "zero usage is reported with an empty steps list, not null")
}

func TestGetJobUsageFlagsOnTheWire(t *testing.T) {
	e := newEnv(t)
	test := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted})
	e.store.setJobUsage(test.ID, store.UsageByStep{Step: "analyze", Model: "fake:gemini-3.5-flash-lite", Calls: 1, InputTokens: 3500, OutputTokens: 300})
	unpriced := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted})
	e.store.setJobUsage(unpriced.ID, store.UsageByStep{Step: "analyze", Model: "gemini-9-ultra", Calls: 1, InputTokens: 3500, OutputTokens: 300})

	usage := rawJob(t, e, test.ID)["usage"].(map[string]any)
	assert.Equal(t, true, usage["testMode"])
	assert.Equal(t, true, usage["costComplete"])
	assert.Equal(t, true, usage["steps"].([]any)[0].(map[string]any)["priced"])

	usage = rawJob(t, e, unpriced.ID)["usage"].(map[string]any)
	assert.Equal(t, 0.0, usage["costUsd"])
	assert.Equal(t, false, usage["testMode"], "cost 0 of a real model is not free test mode")
	assert.Equal(t, false, usage["costComplete"])
	assert.Equal(t, false, usage["steps"].([]any)[0].(map[string]any)["priced"])
}

func TestGetJobUsageErrorReportsZero(t *testing.T) {
	e := newEnv(t)
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted})
	e.store.setJobUsage(j.ID, testUsageRows()...)
	e.store.jobUsageErr = errors.New("store: job usage: pq: connection reset by 10.0.0.9")

	rec := e.get("/v1/domains/cars/jobs/" + j.ID)
	require.Equal(t, http.StatusOK, rec.Code, "usage never fails the poll: %s", rec.Body.String())
	got := decodeJSON[Job](t, rec)
	assert.Equal(t, JobUsage{Steps: []UsageStep{}}, got.Usage, "zero, with the cost marked incomplete (unknown)")
	assert.Equal(t, JobCompleted, got.Status)
	require.NotNil(t, got.Result, "the rest of the payload is intact")
	assert.NotContains(t, rec.Body.String(), "10.0.0.9")

	var logged bool
	for _, l := range e.logs.lines(t) {
		if l["msg"] == "httpapi: get job: usage unavailable, reporting zero" {
			logged = true
			assert.Equal(t, "WARN", l["level"])
			assert.Equal(t, j.ID, l["job_id"])
			assert.Contains(t, l["err"], "10.0.0.9")
		}
	}
	assert.True(t, logged, "the error is logged")
}

func TestGetJobImageEdits(t *testing.T) {
	e := newEnv(t)
	id := ids.NewJob()
	e.store.addJob(store.Job{ID: id, DomainKey: "cars", Status: store.StatusCompleted})
	plain, rotated, cropped := ids.NewImage(), ids.NewImage(), ids.NewImage()
	e.store.addImages(id,
		store.SelectedImage{ID: plain, JobID: id, Idx: 1, ShotID: "front_three_quarter", Width: 1920, Height: 1440},
		store.SelectedImage{ID: rotated, JobID: id, Idx: 2, ShotID: "rear_three_quarter", Width: 1920, Height: 1440, Rotation: 90},
		store.SelectedImage{ID: cropped, JobID: id, Idx: 3, ShotID: "interior", Width: 1600, Height: 1200, Rotation: 270,
			Crop: &store.NRect{X0: 0.1, Y0: 0.2, X1: 0.9, Y1: 0.8}},
	)

	got := decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+id))
	require.NotNil(t, got.Result)
	require.Len(t, got.Result.Images, 3)
	type edit struct {
		rotation int
		cropped  bool
	}
	var edits []edit
	for _, im := range got.Result.Images {
		edits = append(edits, edit{im.Rotation, im.Cropped})
	}
	assert.Equal(t, []edit{{0, false}, {90, false}, {270, true}}, edits)

	// Zero values are sent, not omitted, and the crop rectangle itself stays private.
	raw := rawJob(t, e, id)
	first := raw["result"].(map[string]any)["images"].([]any)[0].(map[string]any)
	assert.Equal(t, 0.0, first["rotation"])
	assert.Equal(t, false, first["cropped"])
	assert.NotContains(t, first, "crop")
}
