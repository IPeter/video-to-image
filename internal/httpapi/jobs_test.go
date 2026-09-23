package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

func intp(v int) *int { return &v }

func steps(jobID string, statuses ...store.StepStatus) []store.Step {
	out := make([]store.Step, 0, len(statuses))
	for i, st := range statuses {
		s := store.Step{JobID: jobID, Name: store.StepNames[i], Status: st}
		if st == store.StepDone {
			s.DurationMs = intp(1000 * (i + 1))
		}
		out = append(out, s)
	}
	return out
}

func stepStatuses(p Progress) []StepStatus {
	out := make([]StepStatus, len(p.Steps))
	for i, s := range p.Steps {
		out[i] = s.Status
	}
	return out
}

// rawJob returns the response as generic JSON, to check nulls and empty lists.
func rawJob(t *testing.T, e *testEnv, jobID string) map[string]any {
	t.Helper()
	rec := e.get("/v1/domains/cars/jobs/" + jobID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var m map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &m))
	return m
}

func TestGetJobQueued(t *testing.T) {
	e := newEnv(t)
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusQueued,
		VideoInfo: media.VideoInfo{DurationSec: 74.2, Width: 1920, Height: 1080, FPS: 29.97, SHA256: "abc", OriginalName: "x.mp4"}})

	rec := e.get("/v1/domains/cars/jobs/" + j.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	got := decodeJSON[Job](t, rec)
	assert.Equal(t, j.ID, got.JobID)
	assert.Equal(t, "cars", got.DomainKey)
	assert.Equal(t, JobQueued, got.Status)
	assert.Equal(t, testNow, got.CreatedAt)
	assert.Equal(t, testNow.Add(24*time.Hour), got.ExpiresAt)
	assert.Equal(t, VideoInfo{DurationSec: 74.2, Width: 1920, Height: 1080, FPS: 29.97}, got.Video)
	assert.Equal(t, 0, got.Progress.Percent)
	assert.Nil(t, got.Progress.Step)
	require.Len(t, got.Progress.Steps, 6)
	for i, s := range got.Progress.Steps {
		assert.Equal(t, StepName(store.StepNames[i]), s.Name)
		assert.Equal(t, StepStatus("pending"), s.Status)
		assert.Nil(t, s.Ms)
	}

	raw := rawJob(t, e, j.ID)
	assert.Nil(t, raw["result"])
	assert.Nil(t, raw["rejection"])
	assert.Nil(t, raw["error"])
	assert.Contains(t, raw, "result", "null fields are present, not omitted")
	progress := raw["progress"].(map[string]any)
	assert.Contains(t, progress, "step")
	assert.Nil(t, progress["step"])
	assert.NotContains(t, rec.Body.String(), "sha256", "internal video fields stay private")
	assert.NotContains(t, rec.Body.String(), "x.mp4")
}

func TestGetJobProcessing(t *testing.T) {
	e := newEnv(t)
	id := ids.NewJob()
	e.store.addJob(store.Job{ID: id, DomainKey: "cars", Status: store.StatusProcessing},
		steps(id, store.StepDone, store.StepRunning, store.StepPending, store.StepPending, store.StepPending, store.StepPending)...)

	got := decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+id))
	assert.Equal(t, JobProcessing, got.Status)
	require.NotNil(t, got.Progress.Step)
	assert.Equal(t, StepName("analyze"), *got.Progress.Step)
	assert.Equal(t, 30, got.Progress.Percent) // preview 10 + half of analyze 40
	require.NotNil(t, got.Progress.Steps[0].Ms)
	assert.Equal(t, 1000, *got.Progress.Steps[0].Ms)
	assert.Nil(t, got.Progress.Steps[1].Ms)
	assert.Nil(t, got.Result)

	raw := rawJob(t, e, id)
	stepsRaw := raw["progress"].(map[string]any)["steps"].([]any)
	assert.Contains(t, stepsRaw[0].(map[string]any), "ms")
	assert.NotContains(t, stepsRaw[1].(map[string]any), "ms", "ms is omitted until the step finished")
}

func TestBuildProgress(t *testing.T) {
	D, R, P, S, F := store.StepDone, store.StepRunning, store.StepPending, store.StepSkipped, store.StepFailed
	cases := []struct {
		name    string
		status  store.JobStatus
		steps   []store.StepStatus
		percent int
		step    string
	}{
		{"all pending", store.StatusQueued, []store.StepStatus{P, P, P, P, P, P}, 0, ""},
		{"preview running", store.StatusProcessing, []store.StepStatus{R, P, P, P, P, P}, 5, "preview"},
		{"extract running", store.StatusProcessing, []store.StepStatus{D, D, R, P, P, P}, 60, "extract"},
		{"score running", store.StatusProcessing, []store.StepStatus{D, D, D, R, P, P}, 77, "score"},
		{"export running", store.StatusProcessing, []store.StepStatus{D, D, D, D, D, R}, 95, "export"},
		{"all done but not terminal yet", store.StatusProcessing, []store.StepStatus{D, D, D, D, D, D}, 100, ""},
		{"completed", store.StatusCompleted, []store.StepStatus{D, D, D, D, D, D}, 100, ""},
		{"rejected skips the rest", store.StatusRejected, []store.StepStatus{D, D, S, S, S, S}, 100, ""},
		{"failed mid-way", store.StatusFailed, []store.StepStatus{D, F, P, P, P, P}, 100, ""},
		{"skipped counts fully", store.StatusProcessing, []store.StepStatus{D, D, S, S, S, R}, 95, "export"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := buildProgress(tc.status, steps("job_x", tc.steps...))
			assert.Equal(t, tc.percent, p.Percent)
			if tc.step == "" {
				assert.Nil(t, p.Step)
			} else {
				require.NotNil(t, p.Step)
				assert.Equal(t, StepName(tc.step), *p.Step)
			}
			require.Len(t, p.Steps, 6)
			for i, st := range tc.steps {
				assert.Equal(t, StepStatus(st), p.Steps[i].Status)
			}
		})
	}
}

func TestBuildProgressOrdersAndFillsSteps(t *testing.T) {
	// Rows in any order, one missing and one unknown: output is always the six
	// pipeline steps in order.
	in := []store.Step{
		{Name: store.StepNameScore, Status: store.StepRunning},
		{Name: store.StepNamePreview, Status: store.StepDone, DurationMs: intp(5)},
		{Name: "mystery", Status: store.StepDone},
		{Name: store.StepNameAnalyze, Status: store.StepDone},
		{Name: store.StepNameExtract, Status: store.StepDone},
		{Name: store.StepNameSelect, Status: store.StepPending},
	}
	p := buildProgress(store.StatusProcessing, in)
	names := make([]StepName, len(p.Steps))
	for i, s := range p.Steps {
		names[i] = s.Name
	}
	assert.Equal(t, []StepName{"preview", "analyze", "extract", "score", "select", "export"}, names)
	assert.Equal(t, []StepStatus{"done", "done", "done", "running", "pending", "pending"}, stepStatuses(p))
	assert.Equal(t, 77, p.Percent)
	assert.Equal(t, 5, *p.Steps[0].Ms)

	empty := buildProgress(store.StatusQueued, nil)
	assert.Len(t, empty.Steps, 6)
	assert.NotNil(t, empty.Steps)
}

func TestGetJobCompleted(t *testing.T) {
	e := newEnv(t)
	id := ids.NewJob()
	e.store.addJob(store.Job{
		ID: id, DomainKey: "cars", Status: store.StatusCompleted,
		VideoSummary:      "Walkaround of a silver sedan.",
		ResultExplanation: "3 images selected. The rear 3/4 angle was not clearly visible.",
		MissingShots:      []string{"rear_three_quarter", "unknown_shot"},
		ZipPath:           storage.ZipKey("cars", id),
	}, steps(id, store.StepDone, store.StepDone, store.StepDone, store.StepDone, store.StepDone, store.StepDone)...)
	img1, img2, img3 := ids.NewImage(), ids.NewImage(), ids.NewImage()
	e.store.addImages(id,
		store.SelectedImage{ID: img3, JobID: id, Idx: 3, ShotID: "other", TimestampSec: 3725.5, Width: 1280, Height: 720, Reason: "detail", FileName: "c.jpg"},
		store.SelectedImage{ID: img1, JobID: id, Idx: 1, ShotID: "front_three_quarter", TimestampSec: 6.25, Width: 1920, Height: 1080,
			Reason: "Whole car visible at 45°.", FileName: "cars_X_1_front_three_quarter_00-06.jpg", ImagePath: "k1", ThumbPath: "t1"},
		store.SelectedImage{ID: img2, JobID: id, Idx: 2, ShotID: "side_profile", TimestampSec: 59.9999999, Width: 1920, Height: 1080, FileName: "b.jpg"},
	)

	got := decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+id))
	assert.Equal(t, JobCompleted, got.Status)
	assert.Equal(t, 100, got.Progress.Percent)
	assert.Nil(t, got.Progress.Step)
	assert.Nil(t, got.Rejection)
	assert.Nil(t, got.Error)
	require.NotNil(t, got.Result)
	res := got.Result
	assert.Equal(t, "Walkaround of a silver sedan.", res.Summary)
	assert.Equal(t, "3 images selected. The rear 3/4 angle was not clearly visible.", res.Explanation)

	base := "/v1/domains/cars/jobs/" + id
	require.Len(t, res.Images, 3)
	assert.Equal(t, ResultImage{
		ID: img1, Index: 1, ShotID: "front_three_quarter", ShotLabel: "Front 3/4",
		TimestampSec: 6.25, TimestampLabel: "00:06", Width: 1920, Height: 1080,
		Reason: "Whole car visible at 45°.", FileName: "cars_X_1_front_three_quarter_00-06.jpg",
		URL:          base + "/images/" + img1,
		DownloadURL:  base + "/images/" + img1 + "?download=1",
		ThumbnailURL: base + "/images/" + img1 + "/thumb",
	}, res.Images[0])
	assert.Equal(t, img2, res.Images[1].ID, "ordered by index")
	assert.Equal(t, "Side profile", res.Images[1].ShotLabel)
	assert.Equal(t, "01:00", res.Images[1].TimestampLabel, "float error absorbed")
	assert.Equal(t, "Other", res.Images[2].ShotLabel)
	assert.Equal(t, "1:02:05", res.Images[2].TimestampLabel)

	assert.Equal(t, []ShotRef{{ID: "rear_three_quarter", Label: "Rear 3/4"}, {ID: "unknown_shot", Label: "unknown_shot"}}, res.MissingShots)
	require.NotNil(t, res.ZipURL)
	assert.Equal(t, base+"/download.zip", *res.ZipURL)
}

func TestGetJobCompletedWithoutImages(t *testing.T) {
	e := newEnv(t)
	id := ids.NewJob()
	// A ZIP path without images must still not advertise a ZIP.
	e.store.addJob(store.Job{ID: id, DomainKey: "cars", Status: store.StatusCompleted,
		ResultExplanation: "No suitable scenes found.", ZipPath: storage.ZipKey("cars", id)})

	raw := rawJob(t, e, id)
	result := raw["result"].(map[string]any)
	assert.Equal(t, []any{}, result["images"], "empty list, not null")
	assert.Equal(t, []any{}, result["missingShots"], "empty list, not null")
	assert.Contains(t, result, "zipUrl")
	assert.Nil(t, result["zipUrl"])
	assert.Equal(t, "No suitable scenes found.", result["explanation"])
	assert.EqualValues(t, 100, raw["progress"].(map[string]any)["percent"])
}

func TestGetJobRejected(t *testing.T) {
	e := newEnv(t)
	withGate := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusRejected,
		Gate: &store.Gate{Passed: false, Confidence: 0.95, Reason: "The video shows a kitchen interior, not a vehicle.", DetectedSubject: "room interior"}})
	noGate := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusRejected})
	emptyReason := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusRejected, Gate: &store.Gate{DetectedSubject: "a boat"}})

	got := decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+withGate.ID))
	assert.Equal(t, JobRejected, got.Status)
	assert.Nil(t, got.Result)
	assert.Nil(t, got.Error)
	require.NotNil(t, got.Rejection)
	assert.Equal(t, "The video shows a kitchen interior, not a vehicle.", got.Rejection.Reason)
	assert.Equal(t, "room interior", got.Rejection.DetectedSubject)
	assert.Equal(t, 100, got.Progress.Percent)
	assert.NotContains(t, e.get("/v1/domains/cars/jobs/"+withGate.ID).Body.String(), "confidence")

	got = decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+noGate.ID))
	require.NotNil(t, got.Rejection)
	assert.Equal(t, genericRejectionReason, got.Rejection.Reason)
	assert.Empty(t, got.Rejection.DetectedSubject)

	got = decodeJSON[Job](t, e.get("/v1/domains/cars/jobs/"+emptyReason.ID))
	assert.Equal(t, genericRejectionReason, got.Rejection.Reason)
	assert.Equal(t, "a boat", got.Rejection.DetectedSubject)
}

func TestGetJobFailed(t *testing.T) {
	cases := []struct {
		code, message string
		wantCode      JobErrorCode
		wantMessage   string
	}{
		{"MODEL_UNAVAILABLE", "The analysis service is busy.", "MODEL_UNAVAILABLE", "The analysis service is busy."},
		{"MODEL_BAD_OUTPUT", "", "MODEL_BAD_OUTPUT", jobErrorMessages["MODEL_BAD_OUTPUT"]},
		{"PROCESSING_ERROR", "Something broke.", "PROCESSING_ERROR", "Something broke."},
		{"DISK_FULL", "", "PROCESSING_ERROR", jobErrorMessages["PROCESSING_ERROR"]},
		{"", "", "PROCESSING_ERROR", jobErrorMessages["PROCESSING_ERROR"]},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			e := newEnv(t)
			j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusFailed, ErrorCode: tc.code, ErrorMessage: tc.message,
				InternalLog: "stderr: SECRET STACK"})
			rec := e.get("/v1/domains/cars/jobs/" + j.ID)
			got := decodeJSON[Job](t, rec)
			assert.Equal(t, JobFailed, got.Status)
			assert.Nil(t, got.Result)
			assert.Nil(t, got.Rejection)
			require.NotNil(t, got.Error)
			assert.Equal(t, tc.wantCode, got.Error.Code)
			assert.Equal(t, tc.wantMessage, got.Error.Message)
			assert.NotEmpty(t, got.Error.Message)
			assert.NotContains(t, rec.Body.String(), "SECRET STACK")
		})
	}
}

func TestGetJobNotVisible(t *testing.T) {
	e := newEnv(t)
	expired := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusExpired})
	homes := e.store.addJob(store.Job{DomainKey: "homes", Status: store.StatusCompleted})

	requireError(t, e.get("/v1/domains/cars/jobs/"+expired.ID), http.StatusGone, CodeExpired)
	requireError(t, e.get("/v1/domains/cars/jobs/"+homes.ID), http.StatusNotFound, CodeJobNotFound)
	requireError(t, e.get("/v1/domains/cars/jobs/"+ids.NewJob()), http.StatusNotFound, CodeJobNotFound)

	calls := e.store.getJobCalls
	for _, bad := range []string{"job_123", "img_" + ids.NewJob()[4:], "job_" + "0000000000000000000000000!", "x"} {
		requireError(t, e.get("/v1/domains/cars/jobs/"+bad), http.StatusNotFound, CodeJobNotFound)
	}
	assert.Equal(t, calls, e.store.getJobCalls, "malformed IDs never reach the database")

	// The owning domain sees it (from its own origin).
	rec := e.get("/v1/domains/homes/jobs/"+homes.ID, "Origin", "https://homes.example.com")
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestGetJobStoreErrorIsOpaque(t *testing.T) {
	e := newEnv(t)
	e.store.getJobErr = errors.New("pq: connection refused to 10.0.0.5")
	rec := e.get("/v1/domains/cars/jobs/" + ids.NewJob())
	requireError(t, rec, http.StatusInternalServerError, CodeInternal)
	assert.NotContains(t, rec.Body.String(), "10.0.0.5")
	assert.Contains(t, e.logs.String(), "10.0.0.5", "details are logged")
}

func TestDeleteJob(t *testing.T) {
	e := newEnv(t)
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted})
	other := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted})
	e.storage.put(storage.OriginalKey("cars", j.ID, "mp4"), []byte("v"))
	e.storage.put(storage.ImageKey("cars", j.ID, "img_1"), []byte("i"))
	e.storage.put(storage.ZipKey("cars", j.ID), []byte("z"))
	e.storage.put(storage.ZipKey("cars", other.ID), []byte("z"))

	del := func(path string, hdr ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodDelete, path, nil)
		r.Header.Set("Origin", testOrigin)
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return e.do(r)
	}

	rec := del("/v1/domains/cars/jobs/" + j.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Body.String())
	prefix := storage.JobPrefix("cars", j.ID)
	assert.Equal(t, []string{prefix, prefix}, e.storage.deletedPrefixes, "files, expire, files again")
	assert.Equal(t, []string{storage.ZipKey("cars", other.ID)}, e.storage.keys(), "other jobs keep their files")
	assert.Equal(t, []string{j.ID}, e.store.expired)
	requireError(t, e.get("/v1/domains/cars/jobs/"+j.ID), http.StatusGone, CodeExpired)

	// Again: already expired → 204, nothing else happens.
	rec = del("/v1/domains/cars/jobs/" + j.ID)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Len(t, e.storage.deletedPrefixes, 2)
	assert.Len(t, e.store.expired, 1)

	// Unknown, malformed and foreign jobs are 404.
	requireError(t, del("/v1/domains/cars/jobs/"+ids.NewJob()), http.StatusNotFound, CodeJobNotFound)
	requireError(t, del("/v1/domains/cars/jobs/nope"), http.StatusNotFound, CodeJobNotFound)
	homes := e.store.addJob(store.Job{DomainKey: "homes", Status: store.StatusCompleted})
	requireError(t, del("/v1/domains/cars/jobs/"+homes.ID), http.StatusNotFound, CodeJobNotFound)
	assert.Len(t, e.storage.deletedPrefixes, 2, "nothing deleted for rejected requests")
}

func TestDeleteJobStorageFailureKeepsJob(t *testing.T) {
	e := newEnv(t)
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted})
	e.storage.deleteErr = errors.New("s3 unavailable")
	r := httptest.NewRequest(http.MethodDelete, "/v1/domains/cars/jobs/"+j.ID, nil)
	r.Header.Set("Origin", testOrigin)
	requireError(t, e.do(r), http.StatusInternalServerError, CodeInternal)
	assert.Empty(t, e.store.expired, "not expired, so the user can retry")

	e.storage.deleteErr = nil
	e.store.expireErr = errors.New("db down")
	requireError(t, e.do(r.Clone(r.Context())), http.StatusInternalServerError, CodeInternal)
}

func TestDeleteJobRemovesFilesUploadedDuringTheDelete(t *testing.T) {
	// A worker finishing the job uploads its images and ZIP after the API listed the
	// prefix, and commits CompleteJob before the API's ExpireJob. The second sweep
	// (after the status flip) must remove them; afterwards CompleteJob would conflict
	// and the worker cleans up itself.
	e := newEnv(t)
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusProcessing})
	e.storage.put(storage.OriginalKey("cars", j.ID, "mp4"), []byte("v"))
	late := []string{storage.ImageKey("cars", j.ID, "img_1"), storage.ZipKey("cars", j.ID)}
	e.store.beforeExpire = func(id string) {
		for _, k := range late {
			e.storage.put(k, []byte("uploaded by the worker"))
		}
	}

	r := httptest.NewRequest(http.MethodDelete, "/v1/domains/cars/jobs/"+j.ID, nil)
	r.Header.Set("Origin", testOrigin)
	rec := e.do(r)

	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Empty(t, e.storage.keys(), "nothing of the job is left in storage")
	assert.Equal(t, []string{j.ID}, e.store.expired)
}

func TestDeleteJobSecondSweepIsBestEffort(t *testing.T) {
	e := newEnv(t)
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted})
	e.storage.deletePrefixErr = func(n int) error {
		if n == 1 {
			return errors.New("s3 hiccup")
		}
		return nil
	}
	r := httptest.NewRequest(http.MethodDelete, "/v1/domains/cars/jobs/"+j.ID, nil)
	r.Header.Set("Origin", testOrigin)
	rec := e.do(r)

	require.Equal(t, http.StatusNoContent, rec.Code, "the job is expired and its files were deleted once")
	assert.Equal(t, []string{j.ID}, e.store.expired)
	assert.Contains(t, e.logs.String(), "second file sweep failed")
	assert.Contains(t, e.logs.String(), "s3 hiccup")
}

func TestGetJobRejectedLowConfidencePass(t *testing.T) {
	// The model said "yes, a car" with confidence 0.5 < minConfidence: the job is
	// rejected, and the model's affirmative reason/subject must not be shown with it.
	e := newEnv(t, func(e *testEnv, _ *Deps) {
		e.cars.Gate.Subject = "a single passenger vehicle (car, SUV, van or pickup) being shown or walked around,\nas for a sales listing"
		e.cars.Gate.MinConfidence = 0.6
	})
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusRejected,
		Gate: &store.Gate{Passed: true, Confidence: 0.5, Reason: "A silver sedan on a lot.", DetectedSubject: "car"}})

	rec := e.get("/v1/domains/cars/jobs/" + j.ID)
	got := decodeJSON[Job](t, rec)
	require.NotNil(t, got.Rejection)
	assert.Equal(t, "We couldn't tell with enough certainty that this video shows a single passenger vehicle "+
		"being shown or walked around. Try a steadier, well-lit video that keeps the subject in frame.", got.Rejection.Reason)
	assert.Empty(t, got.Rejection.DetectedSubject)
	assert.NotContains(t, rec.Body.String(), "silver sedan")

	raw := rawJob(t, e, j.ID)
	assert.Contains(t, raw["rejection"], "detectedSubject", "the field is still present (empty)")
}

func TestShortSubject(t *testing.T) {
	cases := map[string]string{
		"a single passenger vehicle (car, SUV, van or pickup) being shown or walked around, as for a sales listing": "a single passenger vehicle being shown or walked around",
		"a house or apartment, inside or outside": "a house or apartment",
		"  a boat  ":                 "a boat",
		"a cars":                     "a cars",
		"a dog; any breed":           "a dog",
		"":                           genericSubject,
		"(only a parenthetical)":     genericSubject,
		"an unbalanced (paren, here": genericSubject,
		strings.Repeat("very ", 30) + "long subject without breaks": genericSubject,
	}
	for in, want := range cases {
		assert.Equal(t, want, shortSubject(in), "%q", in)
	}
}
