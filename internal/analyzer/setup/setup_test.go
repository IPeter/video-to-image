package setup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/analyzer/fake"
	"video-to-image/internal/analyzer/gemini"
	"video-to-image/internal/analyzer/recording"
	"video-to-image/internal/config"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func baseConfig(t *testing.T, name string) *config.Config {
	return &config.Config{
		Analyzer:            name,
		GeminiAPIKey:        "test-key",
		GeminiBaseURL:       "http://127.0.0.1:1/", // never contacted at construction
		RecordingsDir:       filepath.Join(t.TempDir(), "recordings"),
		ModelCallsPerMinute: 30,
	}
}

type unwrapper interface{ Unwrap() analyzer.Analyzer }

func TestFromConfigGemini(t *testing.T) {
	a, err := FromConfig(context.Background(), baseConfig(t, config.AnalyzerGemini), quietLogger())
	require.NoError(t, err)
	assert.Equal(t, "gemini", a.Name())
	_, ok := a.(analyzer.Sweeper)
	assert.True(t, ok, "the rate-limited client must still sweep files")
	u, ok := a.(unwrapper)
	require.True(t, ok, "wrapped in the rate limit")
	assert.IsType(t, &gemini.Client{}, u.Unwrap())

	cfg := baseConfig(t, config.AnalyzerGemini)
	cfg.ModelCallsPerMinute = 0
	a, err = FromConfig(context.Background(), cfg, nil)
	require.NoError(t, err)
	assert.IsType(t, &gemini.Client{}, a, "0 calls per minute disables the limiter")

	cfg = baseConfig(t, config.AnalyzerGemini)
	cfg.GeminiAPIKey = ""
	_, err = FromConfig(context.Background(), cfg, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GEMINI_API_KEY")
}

func TestFromConfigRecording(t *testing.T) {
	cfg := baseConfig(t, config.AnalyzerRecording)
	a, err := FromConfig(context.Background(), cfg, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, "recording", a.Name())
	_, ok := a.(analyzer.Sweeper)
	assert.True(t, ok)
	u, ok := a.(unwrapper)
	require.True(t, ok)
	assert.IsType(t, &recording.Recorder{}, u.Unwrap())
	st, err := os.Stat(cfg.RecordingsDir)
	require.NoError(t, err, "the recordings dir is created")
	assert.True(t, st.IsDir())
}

func TestFromConfigRecorded(t *testing.T) {
	cfg := baseConfig(t, config.AnalyzerRecorded)
	cfg.GeminiAPIKey = "" // not needed offline
	_, err := FromConfig(context.Background(), cfg, quietLogger())
	require.Error(t, err, "the recordings dir must exist")

	require.NoError(t, os.MkdirAll(cfg.RecordingsDir, 0o755))
	a, err := FromConfig(context.Background(), cfg, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, "recorded", a.Name())
	assert.IsType(t, &recording.Replayer{}, a, "replay is free: no rate limit")

	_, err = a.Generate(context.Background(), &analyzer.Request{Step: analyzer.StepAnalyze, CacheKey: "nope"})
	assert.ErrorIs(t, err, analyzer.ErrNoRecording)
}

func TestFromConfigFake(t *testing.T) {
	cfg := baseConfig(t, config.AnalyzerFake)
	cfg.GeminiAPIKey = ""
	cfg.FakeDelay = 5 * time.Millisecond
	a, err := FromConfig(context.Background(), cfg, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, "fake", a.Name())
	assert.IsType(t, &fake.Analyzer{}, a, "the fake is free: no rate limit")
	resp, err := a.Generate(context.Background(), &analyzer.Request{Step: analyzer.StepAnalyze, DurationSec: 20, ShotIDs: []string{"front"}})
	require.NoError(t, err)
	assert.Equal(t, 5*time.Millisecond, resp.Usage.Latency)
	assert.Contains(t, resp.Text, `"shotId":"front"`)

	canned := filepath.Join(t.TempDir(), "canned.json")
	require.NoError(t, os.WriteFile(canned, []byte(`{"canned":true}`), 0o600))
	cfg.FakeResponseFile = canned
	a, err = FromConfig(context.Background(), cfg, quietLogger())
	require.NoError(t, err)
	resp, err = a.Generate(context.Background(), &analyzer.Request{Step: analyzer.StepAnalyze})
	require.NoError(t, err)
	assert.Equal(t, `{"canned":true}`, resp.Text)

	cfg.FakeResponseFile = filepath.Join(t.TempDir(), "missing.json")
	_, err = FromConfig(context.Background(), cfg, quietLogger())
	assert.Error(t, err)
}

func TestFromConfigInvalid(t *testing.T) {
	_, err := FromConfig(context.Background(), nil, nil)
	assert.Error(t, err)

	_, err = FromConfig(context.Background(), baseConfig(t, "openai"), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unknown analyzer "openai"`)
}

// TestRecordThenReplay records a real (httptest) Gemini answer in recording mode and
// replays it offline in recorded mode.
func TestRecordThenReplay(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, ":generateContent") {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"{\"picks\":[]}"}]},"finishReason":"STOP"}],
			"usageMetadata":{"promptTokenCount":321,"candidatesTokenCount":9},"modelVersion":"gemini-3.5-flash-lite"}`)
	}))
	defer srv.Close()

	cfg := baseConfig(t, config.AnalyzerRecording)
	cfg.GeminiBaseURL = srv.URL + "/"
	rec, err := FromConfig(context.Background(), cfg, quietLogger())
	require.NoError(t, err)
	req := &analyzer.Request{
		Step: analyzer.StepRerank, CacheKey: "sha256:cars:rerank", Model: "gemini-3.5-flash-lite",
		Parts: []analyzer.Part{{Text: "pick the best"}},
	}
	resp, err := rec.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, `{"picks":[]}`, resp.Text)
	assert.Equal(t, int32(1), calls.Load())

	cfg.Analyzer = config.AnalyzerRecorded
	cfg.GeminiBaseURL = "http://127.0.0.1:1/"
	rp, err := FromConfig(context.Background(), cfg, quietLogger())
	require.NoError(t, err)
	replayed, err := rp.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, resp.Text, replayed.Text)
	assert.Equal(t, 321, replayed.Usage.InputTokens)
	assert.Equal(t, 9, replayed.Usage.OutputTokens)
	assert.Equal(t, "gemini-3.5-flash-lite", replayed.Usage.Model)
	assert.Equal(t, int32(1), calls.Load(), "replay makes no network call")

	_, err = rp.Generate(context.Background(), &analyzer.Request{Step: analyzer.StepRerank, CacheKey: "other"})
	assert.True(t, errors.Is(err, analyzer.ErrNoRecording))
}
