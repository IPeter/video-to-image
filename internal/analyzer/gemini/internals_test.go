package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"video-to-image/internal/analyzer"
)

func TestConvertSchema(t *testing.T) {
	zero, one := 0.0, 1.0
	in := &analyzer.Schema{
		Type:             "object",
		Description:      "root",
		PropertyOrdering: []string{"gate", "scenes"},
		Required:         []string{"gate", "scenes"},
		Properties: map[string]*analyzer.Schema{
			"gate": {
				Type:     "OBJECT",
				Required: []string{"passed"},
				Properties: map[string]*analyzer.Schema{
					"passed":     {Type: "boolean"},
					"confidence": {Type: "number", Minimum: &zero, Maximum: &one},
				},
			},
			"scenes": {
				Type: "array",
				Items: &analyzer.Schema{
					Type:       "object",
					Properties: map[string]*analyzer.Schema{"shotId": {Type: "string", Enum: []string{"front", "other"}}, "n": {Type: "integer", Nullable: true}},
				},
			},
			"any": {Description: "untyped"},
		},
	}
	out, err := ConvertSchema(in)
	require.NoError(t, err)

	assert.Equal(t, genai.TypeObject, out.Type)
	assert.Equal(t, "root", out.Description)
	assert.Equal(t, []string{"gate", "scenes"}, out.PropertyOrdering)
	assert.Equal(t, []string{"gate", "scenes"}, out.Required)
	gate := out.Properties["gate"]
	assert.Equal(t, genai.TypeObject, gate.Type)
	assert.Equal(t, genai.TypeBoolean, gate.Properties["passed"].Type)
	conf := gate.Properties["confidence"]
	assert.Equal(t, genai.TypeNumber, conf.Type)
	require.NotNil(t, conf.Minimum)
	require.NotNil(t, conf.Maximum)
	assert.Equal(t, 0.0, *conf.Minimum)
	assert.Equal(t, 1.0, *conf.Maximum)
	assert.NotSame(t, &zero, conf.Minimum, "pointers are copied, not shared")
	scenes := out.Properties["scenes"]
	assert.Equal(t, genai.TypeArray, scenes.Type)
	shot := scenes.Items.Properties["shotId"]
	assert.Equal(t, genai.TypeString, shot.Type)
	assert.Equal(t, []string{"front", "other"}, shot.Enum)
	assert.Equal(t, "enum", shot.Format)
	n := scenes.Items.Properties["n"]
	assert.Equal(t, genai.TypeInteger, n.Type)
	require.NotNil(t, n.Nullable)
	assert.True(t, *n.Nullable)
	assert.Nil(t, gate.Nullable, "false Nullable is omitted")
	assert.Equal(t, genai.Type(""), out.Properties["any"].Type)

	// The input is not aliased.
	in.Required[0] = "mutated"
	assert.Equal(t, "gate", out.Required[0])

	// The JSON the SDK sends uses the API's field names and upper-case types.
	b, err := json.Marshal(out)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"type":"OBJECT"`)
	assert.Contains(t, string(b), `"propertyOrdering":["gate","scenes"]`)

	nilOut, err := ConvertSchema(nil)
	require.NoError(t, err)
	assert.Nil(t, nilOut)
}

func TestConvertSchemaErrors(t *testing.T) {
	lo, hi := 2.0, 1.0
	cyclic := &analyzer.Schema{Type: "array"}
	cyclic.Items = cyclic
	tests := map[string]*analyzer.Schema{
		"unknown type":        {Type: "map"},
		"nested unknown type": {Type: "object", Properties: map[string]*analyzer.Schema{"x": {Type: "tuple"}}},
		"nil property":        {Type: "object", Properties: map[string]*analyzer.Schema{"x": nil}},
		"undeclared required": {Type: "object", Required: []string{"x"}},
		"undeclared ordering": {Type: "object", Properties: map[string]*analyzer.Schema{"a": {Type: "string"}}, PropertyOrdering: []string{"a", "b"}},
		"min > max":           {Type: "number", Minimum: &lo, Maximum: &hi},
		"cyclic":              cyclic,
	}
	for name, s := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ConvertSchema(s)
			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), "gemini: response schema: $"), err.Error())
		})
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want errKind
	}{
		{"429", apiErr(429, ""), kindTransient},
		{"408", apiErr(408, ""), kindTransient},
		{"500", apiErr(500, ""), kindTransient},
		{"503 wrapped", fmt.Errorf("x: %w", apiErr(503, "")), kindTransient},
		{"400", apiErr(400, ""), kindFatal},
		{"403", apiErr(403, ""), kindFatal},
		{"404 pointer", &genai.APIError{Code: 404}, kindFatal},
		{"string 429", errors.New("Failed to create file. Ran into an error: Error 429, Message: q, Status: , Details: []"), kindTransient},
		{"string 400", errors.New("Failed to create file. Ran into an error: Error 400, Message: q, Status: , Details: []"), kindFatal},
		{"APIError without code", genai.APIError{Message: "?"}, kindTransient},
		{"network", errors.New("dial tcp: connection refused"), kindTransient},
		{"local", localErrorf("open: %w", os.ErrNotExist), kindLocal},
		{"fatal tag", fatalErrorf("x"), kindFatal},
		{"transient tag beats status", transientErrorf("x: %w", apiErr(400, "")), kindTransient},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, classify(tt.err), tt.name)
	}
	assert.True(t, isGone(apiErr(404, "")))
	assert.True(t, isGone(apiErr(403, "")))
	assert.False(t, isGone(apiErr(500, "")))
	assert.False(t, isGone(errors.New("boom")))
	assert.Equal(t, "HTTP 429 RESOURCE_EXHAUSTED", describe(apiErr(429, "RESOURCE_EXHAUSTED")))
	assert.Equal(t, "HTTP 503", describe(genai.APIError{Code: 503, Status: "503 Service Unavailable"}))
}

func TestRetryHintAndBackoff(t *testing.T) {
	withHint := genai.APIError{Code: 429, Details: []map[string]any{
		{"@type": "type.googleapis.com/google.rpc.QuotaFailure"},
		{"@type": "type.googleapis.com/google.rpc.RetryInfo", "retryDelay": "7s"},
	}}
	assert.Equal(t, 7*time.Second, retryHint(fmt.Errorf("wrapped: %w", withHint)))
	assert.Zero(t, retryHint(apiErr(429, "")))
	assert.Zero(t, retryHint(errors.New("x")))
	assert.Zero(t, retryHint(nil))

	c := newClient(&fakeAPI{}, Options{BackoffBase: 100 * time.Millisecond, MaxBackoff: time.Second, Logger: discardLogger()})
	for i := 0; i < 200; i++ {
		d1 := c.backoff(1, 0)
		assert.GreaterOrEqual(t, d1, 50*time.Millisecond)
		assert.LessOrEqual(t, d1, 100*time.Millisecond)
		d3 := c.backoff(3, 0)
		assert.GreaterOrEqual(t, d3, 200*time.Millisecond)
		assert.LessOrEqual(t, d3, 400*time.Millisecond)
		d10 := c.backoff(10, 0)
		assert.GreaterOrEqual(t, d10, 500*time.Millisecond)
		assert.LessOrEqual(t, d10, time.Second, "capped at MaxBackoff")
	}
	assert.Equal(t, 800*time.Millisecond, c.backoff(1, 800*time.Millisecond), "server hint wins when longer")
	assert.Equal(t, time.Second, c.backoff(1, time.Minute), "hint is capped too")
}

func TestReleaseFile(t *testing.T) {
	t.Run("empty handle", func(t *testing.T) {
		fake := &fakeAPI{}
		require.NoError(t, testClient(fake).ReleaseFile(context.Background(), ""))
		assert.Empty(t, fake.deleted())
	})
	t.Run("deletes", func(t *testing.T) {
		fake := &fakeAPI{}
		require.NoError(t, testClient(fake).ReleaseFile(context.Background(), "files/abc"))
		assert.Equal(t, []string{"files/abc"}, fake.deleted())
	})
	for _, code := range []int{403, 404} {
		t.Run(fmt.Sprintf("gone %d", code), func(t *testing.T) {
			fake := &fakeAPI{onDelete: func(string) error { return apiErr(code, "") }}
			require.NoError(t, testClient(fake).ReleaseFile(context.Background(), "files/old"))
			assert.Len(t, fake.deleted(), 1)
		})
	}
	t.Run("transient then ok", func(t *testing.T) {
		n := 0
		fake := &fakeAPI{onDelete: func(string) error {
			n++
			if n == 1 {
				return apiErr(500, "")
			}
			return nil
		}}
		require.NoError(t, testClient(fake).ReleaseFile(context.Background(), "files/abc"))
		assert.Len(t, fake.deleted(), 2)
	})
	t.Run("always failing", func(t *testing.T) {
		fake := &fakeAPI{onDelete: func(string) error { return apiErr(503, "") }}
		err := testClient(fake).ReleaseFile(context.Background(), "files/abc")
		assert.ErrorIs(t, err, analyzer.ErrUnavailable)
		assert.Len(t, fake.deleted(), 3)
	})
	t.Run("rejected", func(t *testing.T) {
		fake := &fakeAPI{onDelete: func(string) error { return apiErr(400, "") }}
		err := testClient(fake).ReleaseFile(context.Background(), "files/abc")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "release file files/abc")
		assert.Len(t, fake.deleted(), 1)
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fake := &fakeAPI{onDelete: func(string) error { cancel(); return context.Canceled }}
		err := testClient(fake).ReleaseFile(ctx, "files/abc")
		assert.Equal(t, context.Canceled, err)
	})
}

func TestSweepFiles(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	file := func(name, display string, age time.Duration) *genai.File {
		return &genai.File{Name: name, DisplayName: display, CreateTime: now.Add(-age)}
	}
	fake := &fakeAPI{
		list: []*genai.File{
			file("files/old1", "v2i-analyze-job_1", 7*time.Hour),
			file("files/old2", "v2i-rerank-job_2", 30*time.Hour),
			file("files/new", "v2i-analyze-job_3", time.Hour),
			file("files/foreign", "someone-elses-file", 40*time.Hour),
			file("files/unnamed", "", 40*time.Hour),
			file("files/gone", "v2i-analyze-job_4", 8*time.Hour),
			{Name: "files/notime", DisplayName: "v2i-x"},
			nil,
		},
		onDelete: func(name string) error {
			if name == "files/gone" {
				return apiErr(404, "NOT_FOUND")
			}
			return nil
		},
	}
	c := testClient(fake)
	c.now = func() time.Time { return now }
	n, err := c.SweepFiles(context.Background(), 6*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "old1 and old2; gone was already deleted")
	assert.ElementsMatch(t, []string{"files/old1", "files/old2", "files/gone"}, fake.deleted(),
		"only this app's old files are touched")

	t.Run("delete failures are collected", func(t *testing.T) {
		fake := &fakeAPI{
			list: []*genai.File{file("files/a", "v2i-a", 10*time.Hour), file("files/b", "v2i-b", 10*time.Hour)},
			onDelete: func(name string) error {
				if name == "files/a" {
					return apiErr(500, "INTERNAL")
				}
				return nil
			},
		}
		c := testClient(fake)
		c.now = func() time.Time { return now }
		n, err := c.SweepFiles(context.Background(), time.Hour)
		require.Error(t, err)
		assert.Equal(t, 1, n)
		assert.Contains(t, err.Error(), "1 of 2 deletes failed")
		assert.Contains(t, err.Error(), "files/a")
	})
	t.Run("list error", func(t *testing.T) {
		fake := &fakeAPI{list: []*genai.File{file("files/a", "v2i-a", 10*time.Hour)}, listErr: apiErr(500, "")}
		c := testClient(fake)
		c.now = func() time.Time { return now }
		n, err := c.SweepFiles(context.Background(), time.Hour)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "list:")
		assert.Equal(t, 1, n, "stale files found before the listing failed are still deleted")
		assert.Equal(t, []string{"files/a"}, fake.deleted())
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		fake := &fakeAPI{listErr: context.Canceled}
		_, err := testClient(fake).SweepFiles(ctx, time.Hour)
		assert.Equal(t, context.Canceled, err)
	})
	t.Run("non-positive age", func(t *testing.T) {
		_, err := testClient(&fakeAPI{}).SweepFiles(context.Background(), 0)
		assert.Error(t, err)
	})
}
