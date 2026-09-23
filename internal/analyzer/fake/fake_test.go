package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
)

// decodeStrict decodes the analyze answer, failing on fields the pipeline does not know.
func decodeStrict(t *testing.T, text string) analyzeResponse {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var out analyzeResponse
	require.NoError(t, dec.Decode(&out), "fake output must be valid JSON of the ModelResponse shape: %s", text)
	return out
}

// parseTS parses "MM:SS.ss".
func parseTS(t *testing.T, s string) float64 {
	t.Helper()
	m, sec, ok := strings.Cut(s, ":")
	require.True(t, ok, "timestamp %q has no colon", s)
	require.Len(t, sec, 5, "timestamp %q must have SS.ss seconds", s)
	require.GreaterOrEqual(t, len(m), 2, "timestamp %q must have zero-padded minutes", s)
	mi, err := strconv.Atoi(m)
	require.NoError(t, err)
	sf, err := strconv.ParseFloat(sec, 64)
	require.NoError(t, err)
	require.Less(t, sf, 60.0)
	return float64(mi)*60 + sf
}

func newFake(t *testing.T, opts Options) *Analyzer {
	t.Helper()
	a, err := New(opts)
	require.NoError(t, err)
	return a
}

func TestSyntheticScenes(t *testing.T) {
	cars := []string{
		"front_three_quarter", "side_profile", "rear_three_quarter", "interior_dashboard",
		"front", "rear", "wheel", "engine", "trunk", "seats", // 10 shots → capped at 8 scenes
	}
	tests := []struct {
		name         string
		duration     float64
		shots        []string
		wantScenes   int
		wantDuration float64
	}{
		{"cars 45s", 45, cars, 8, 45},
		{"one shot", 30, []string{"front"}, 1, 30},
		{"no shots", 20, nil, 1, 20},
		{"zero duration", 0, cars[:4], 4, DefaultDurationSec},
		{"negative duration", -3, cars[:2], 2, DefaultDurationSec},
		{"NaN duration", math.NaN(), cars[:2], 2, DefaultDurationSec},
		{"very short", 2, cars, 8, 2}, // 0.6 s floor gets clamped to the duration
		{"long", 3600, cars[:3], 3, 3600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newFake(t, Options{})
			resp, err := a.Generate(context.Background(), &analyzer.Request{
				Step: analyzer.StepAnalyze, Model: "gemini-3.5-flash-lite",
				DurationSec: tt.duration, ShotIDs: tt.shots,
				Parts: []analyzer.Part{{FilePath: "preview.mp4", MIMEType: "video/mp4"}, {Text: "prompt"}},
			})
			require.NoError(t, err)
			out := decodeStrict(t, resp.Text)

			assert.True(t, out.Gate.Passed)
			assert.Equal(t, 0.95, out.Gate.Confidence)
			assert.Equal(t, "Fake analyzer: accepted.", out.Gate.Reason)
			assert.Equal(t, "car", out.Gate.DetectedSubject)
			assert.Equal(t, "Synthetic walkaround (fake analyzer).", out.VideoSummary)
			require.Len(t, out.Scenes, tt.wantScenes)

			shots := tt.shots
			if len(shots) == 0 {
				shots = []string{"other"}
			}
			prevStart := -1.0
			for i, s := range out.Scenes {
				start, end := parseTS(t, s.Start), parseTS(t, s.End)
				assert.Less(t, start, end, "scene %d: %s-%s", i, s.Start, s.End)
				assert.GreaterOrEqual(t, start, 0.0)
				assert.LessOrEqual(t, end, tt.wantDuration+1e-9, "scene %d ends after the video", i)
				assert.Greater(t, start, prevStart, "scenes must be in time order")
				prevStart = start
				assert.Equal(t, shots[i], s.ShotID)
				assert.Equal(t, fmt.Sprintf("Fake analyzer: synthetic %s moment.", shots[i]), s.Reason)
				assert.InDelta(t, 0.95-0.05*float64(i), s.Relevance, 1e-9)
			}

			assert.Equal(t, analyzer.Usage{
				Model:        ModelPrefix + "gemini-3.5-flash-lite",
				InputTokens:  int(tt.wantDuration*100) + 500,
				OutputTokens: 300,
			}, resp.Usage)
			assert.Equal(t, []string{"", ""}, resp.FileHandles)
		})
	}
}

func TestSyntheticScenesExactValues(t *testing.T) {
	a := newFake(t, Options{})
	resp, err := a.Generate(context.Background(), &analyzer.Request{
		Step: analyzer.StepAnalyze, DurationSec: 40, ShotIDs: []string{"a", "b", "c", "d"},
	})
	require.NoError(t, err)
	out := decodeStrict(t, resp.Text)
	// seg = 10 s; start = 10i + 2; length = max(0.6, 6) = 6.
	want := [][2]string{{"00:02.00", "00:08.00"}, {"00:12.00", "00:18.00"}, {"00:22.00", "00:28.00"}, {"00:32.00", "00:38.00"}}
	require.Len(t, out.Scenes, 4)
	for i, w := range want {
		assert.Equal(t, w[0], out.Scenes[i].Start)
		assert.Equal(t, w[1], out.Scenes[i].End)
	}
	assert.Equal(t, []float64{0.95, 0.9, 0.85, 0.8},
		[]float64{out.Scenes[0].Relevance, out.Scenes[1].Relevance, out.Scenes[2].Relevance, out.Scenes[3].Relevance})
	// Deterministic: a second call returns byte-identical output.
	resp2, err := a.Generate(context.Background(), &analyzer.Request{
		Step: analyzer.StepAnalyze, DurationSec: 40, ShotIDs: []string{"a", "b", "c", "d"},
	})
	require.NoError(t, err)
	assert.Equal(t, resp.Text, resp2.Text)
}

func TestReject(t *testing.T) {
	for _, name := range []string{"reject.mp4", "My-REJECTED-video.MOV", "please_Reject_me"} {
		a := newFake(t, Options{})
		resp, err := a.Generate(context.Background(), &analyzer.Request{
			Step: analyzer.StepAnalyze, DurationSec: 30, ShotIDs: []string{"front"}, SourceName: name,
		})
		require.NoError(t, err)
		out := decodeStrict(t, resp.Text)
		assert.False(t, out.Gate.Passed, name)
		assert.Equal(t, 0.9, out.Gate.Confidence)
		assert.Equal(t, `Fake analyzer: rejected because the file name contains "reject".`, out.Gate.Reason)
		assert.Equal(t, "not a car", out.Gate.DetectedSubject)
		assert.NotNil(t, out.Scenes)
		assert.Empty(t, out.Scenes)
		assert.Contains(t, resp.Text, `"scenes":[]`, "scenes must be an empty array, not null")
	}
}

func TestResponseFile(t *testing.T) {
	canned := []byte("{\"gate\":{\"passed\":true},\n \"custom\": 1}\n")
	path := filepath.Join(t.TempDir(), "canned.json")
	require.NoError(t, os.WriteFile(path, canned, 0o600))
	a := newFake(t, Options{ResponseFile: path})

	// Returned verbatim, even for a "reject" source name (the file wins).
	for _, src := range []string{"car.mp4", "reject.mp4"} {
		resp, err := a.Generate(context.Background(), &analyzer.Request{Step: analyzer.StepAnalyze, SourceName: src})
		require.NoError(t, err)
		assert.True(t, bytes.Equal(canned, []byte(resp.Text)), "got %q", resp.Text)
	}
	// Re-rank ignores the file.
	resp, err := a.Generate(context.Background(), &analyzer.Request{Step: analyzer.StepRerank})
	require.NoError(t, err)
	assert.JSONEq(t, `{"picks":[]}`, resp.Text)

	_, err = New(Options{ResponseFile: filepath.Join(t.TempDir(), "missing.json")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fake: read response file")
}

func TestRerank(t *testing.T) {
	a := newFake(t, Options{})
	parts := []analyzer.Part{
		{Text: "Candidate cf_1"}, {FilePath: "a.jpg", MIMEType: "image/jpeg"},
		{Text: "Candidate cf_2"}, {FilePath: "b.jpg", MIMEType: "image/jpeg"},
	}
	resp, err := a.Generate(context.Background(), &analyzer.Request{Step: analyzer.StepRerank, Model: "m", Parts: parts})
	require.NoError(t, err)
	var out struct {
		Picks []json.RawMessage `json:"picks"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Text), &out))
	assert.NotNil(t, out.Picks)
	assert.Empty(t, out.Picks)
	assert.Len(t, resp.FileHandles, len(parts))
	assert.Equal(t, "fake:m", resp.Usage.Model)
	assert.Equal(t, int(DefaultDurationSec*100)+500, resp.Usage.InputTokens)
}

func TestUnknownStepAndNilRequest(t *testing.T) {
	a := newFake(t, Options{})
	_, err := a.Generate(context.Background(), &analyzer.Request{Step: "summarise"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown step "summarise"`)

	_, err = a.Generate(context.Background(), nil)
	require.Error(t, err)
}

func TestDelay(t *testing.T) {
	a := newFake(t, Options{Delay: 30 * time.Millisecond})
	start := time.Now()
	resp, err := a.Generate(context.Background(), &analyzer.Request{Step: analyzer.StepAnalyze})
	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 30*time.Millisecond)
	assert.Equal(t, 30*time.Millisecond, resp.Usage.Latency)

	t.Run("canceled during delay", func(t *testing.T) {
		a := newFake(t, Options{Delay: time.Minute})
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := a.Generate(ctx, &analyzer.Request{Step: analyzer.StepAnalyze})
		assert.Equal(t, context.DeadlineExceeded, err, "context errors are returned as-is")
		assert.Less(t, time.Since(start), 5*time.Second)
	})
	t.Run("already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newFake(t, Options{}).Generate(ctx, &analyzer.Request{Step: analyzer.StepAnalyze})
		assert.Equal(t, context.Canceled, err)
	})
}

func TestNameAndRelease(t *testing.T) {
	a := newFake(t, Options{})
	assert.Equal(t, "fake", a.Name())
	assert.NoError(t, a.ReleaseFile(context.Background(), "files/anything"))
	assert.NoError(t, a.ReleaseFile(context.Background(), ""))
	_, isSweeper := any(a).(analyzer.Sweeper)
	assert.False(t, isSweeper)
}

func TestFormatTimestamp(t *testing.T) {
	tests := map[float64]string{
		0:       "00:00.00",
		-1:      "00:00.00",
		4.25:    "00:04.25",
		9.999:   "00:10.00",
		59.999:  "01:00.00",
		60:      "01:00.00",
		125.5:   "02:05.50",
		599.994: "09:59.99",
		6000:    "100:00.00",
	}
	for in, want := range tests {
		assert.Equal(t, want, FormatTimestamp(in), "FormatTimestamp(%v)", in)
	}
	assert.Equal(t, "00:00.00", FormatTimestamp(math.NaN()))
}
