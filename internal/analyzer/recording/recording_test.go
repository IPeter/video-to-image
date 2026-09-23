package recording

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
)

// stubAnalyzer returns a fixed answer (or error) and records what it was asked.
type stubAnalyzer struct {
	mu       sync.Mutex
	text     string
	err      error
	calls    int
	released []string
}

func (s *stubAnalyzer) Generate(_ context.Context, req *analyzer.Request) (*analyzer.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return &analyzer.Response{
		Text: s.text,
		Usage: analyzer.Usage{
			Model: "gemini-3.5-flash-lite-001", InputTokens: 1234, OutputTokens: 56, ThinkingTokens: 7,
			Latency: 1500 * time.Millisecond,
		},
		FileHandles: []string{"files/abc", ""}[:len(req.Parts)],
	}, nil
}

func (s *stubAnalyzer) ReleaseFile(_ context.Context, handle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, handle)
	return nil
}

func (s *stubAnalyzer) Name() string { return "stub" }

type sweepingStub struct {
	stubAnalyzer
	olderThan time.Duration
}

func (s *sweepingStub) SweepFiles(_ context.Context, olderThan time.Duration) (int, error) {
	s.olderThan = olderThan
	return 3, nil
}

func request(key string) *analyzer.Request {
	return &analyzer.Request{
		Step: analyzer.StepAnalyze, JobID: "job_1", DomainKey: "cars", CacheKey: key,
		Model: "gemini-3.5-flash-lite", SystemPrompt: "system",
		Parts: []analyzer.Part{
			{FilePath: "/tmp/preview.mp4", MIMEType: "video/mp4", VideoFPS: 2},
			{Text: "user prompt"},
		},
		ResponseSchema: &analyzer.Schema{Type: "object"},
		SourceName:     "customer-car.mp4",
	}
}

func TestFileName(t *testing.T) {
	assert.Equal(t, "abc.json", FileName("abc"))
	assert.Equal(t, "a-b_c.1.json", FileName("a-b_c.1"))

	key := strings.Repeat("0123456789abcdef", 4) + ":cars:analyze"
	name := FileName(key)
	assert.True(t, strings.HasPrefix(name, strings.Repeat("0123456789abcdef", 4)+"_cars_analyze+"), name)
	assert.Regexp(t, `^[a-f0-9]{64}_cars_analyze\+[0-9a-f]{16}\.json$`, name)

	// Keys that sanitise to the same characters must still get distinct files.
	keys := []string{"a:b", "a/b", "a_b", "a b", "A_b", "a_B", "a\\b", "a:b ", "ä_b", "a:b:c", "a_b_c"}
	seen := map[string]string{}
	for _, k := range keys {
		n := FileName(k)
		if prev, dup := seen[strings.ToLower(n)]; dup {
			t.Fatalf("keys %q and %q map to the same file %q (case-insensitively)", prev, k, n)
		}
		seen[strings.ToLower(n)] = k
		assert.Regexp(t, `^[A-Za-z0-9._+-]+\.json$`, n, "unsafe file name for %q", k)
	}

	// Path traversal is impossible: no separator survives.
	for _, k := range []string{"../../etc/passwd", "/abs/path", `..\..\win`, "a/../../b"} {
		p := Path("/rec", k)
		assert.Equal(t, "/rec", filepath.Dir(p), "key %q escapes the dir: %s", k, p)
	}

	// Very long keys are truncated (plus hash) to stay within file-name limits.
	long := strings.Repeat("x", 1000)
	assert.LessOrEqual(t, len(FileName(long)), 200)
	assert.NotEqual(t, FileName(long), FileName(long+"y"))
	assert.Equal(t, FileName(long), FileName(long), "deterministic")
}

func TestRecorderWritesRecording(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "recordings")
	inner := &stubAnalyzer{text: `{"gate":{"passed":true}}`}
	rec, err := NewRecorder(inner, dir, nil)
	require.NoError(t, err)
	fixed := time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)
	rec.now = func() time.Time { return fixed }
	assert.Equal(t, "recording", rec.Name())
	assert.Equal(t, dir, rec.Dir())

	req := request("sha256abc:cars:analyze")
	resp, err := rec.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, `{"gate":{"passed":true}}`, resp.Text)
	assert.Equal(t, []string{"files/abc", ""}, resp.FileHandles, "inner response is returned unchanged")

	data, err := os.ReadFile(Path(dir, req.CacheKey))
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, "sha256abc:cars:analyze", got["cacheKey"])
	assert.Equal(t, "analyze", got["step"])
	assert.Equal(t, "gemini-3.5-flash-lite", got["model"])
	assert.Equal(t, `{"gate":{"passed":true}}`, got["text"])
	assert.Equal(t, "2026-09-23T10:00:00Z", got["recordedAt"])
	assert.Equal(t, map[string]any{
		"model": "gemini-3.5-flash-lite-001", "inputTokens": 1234.0, "outputTokens": 56.0,
		"thinkingTokens": 7.0, "latencyMs": 1500.0,
	}, got["usage"])
	assert.NotEmpty(t, got["promptSha256"])
	assert.NotContains(t, string(data), "customer-car", "the source name is never recorded")

	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, FileName(req.CacheKey), entries[0].Name())
}

func TestRecorderSkips(t *testing.T) {
	t.Run("empty cache key", func(t *testing.T) {
		dir := t.TempDir()
		rec, err := NewRecorder(&stubAnalyzer{text: "x"}, dir, nil)
		require.NoError(t, err)
		_, err = rec.Generate(context.Background(), request(""))
		require.NoError(t, err)
		entries, _ := os.ReadDir(dir)
		assert.Empty(t, entries)
	})
	t.Run("inner error", func(t *testing.T) {
		dir := t.TempDir()
		innerErr := fmt.Errorf("gemini: boom: %w", analyzer.ErrUnavailable)
		rec, err := NewRecorder(&stubAnalyzer{err: innerErr}, dir, nil)
		require.NoError(t, err)
		resp, err := rec.Generate(context.Background(), request("k"))
		assert.Nil(t, resp)
		assert.Same(t, innerErr, err, "inner errors are returned unchanged")
		assert.ErrorIs(t, err, analyzer.ErrUnavailable)
		entries, _ := os.ReadDir(dir)
		assert.Empty(t, entries)
	})
	t.Run("context error passes through", func(t *testing.T) {
		rec, err := NewRecorder(&stubAnalyzer{err: context.Canceled}, t.TempDir(), nil)
		require.NoError(t, err)
		_, err = rec.Generate(context.Background(), request("k"))
		assert.Equal(t, context.Canceled, err)
	})
}

func TestRecorderOverwritesAndIsConcurrencySafe(t *testing.T) {
	dir := t.TempDir()
	inner := &stubAnalyzer{text: "first"}
	rec, err := NewRecorder(inner, dir, nil)
	require.NoError(t, err)
	_, err = rec.Generate(context.Background(), request("k"))
	require.NoError(t, err)
	inner.mu.Lock()
	inner.text = "second"
	inner.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			_, err := rec.Generate(context.Background(), request("k"))
			assert.NoError(t, err)
		})
	}
	wg.Wait()

	var r Record
	data, err := os.ReadFile(Path(dir, "k"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &r), "concurrent writers must never leave a torn file")
	assert.Equal(t, "second", r.Text)
	entries, _ := os.ReadDir(dir)
	assert.Len(t, entries, 1, "no temp files left behind")
}

func TestRecorderWriteFailureIsLogged(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	rec, err := NewRecorder(&stubAnalyzer{text: "ok"}, dir, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(dir)) // make the write fail

	resp, err := rec.Generate(context.Background(), request("k"))
	require.NoError(t, err, "a failed write must not fail the (already paid for) model call")
	assert.Equal(t, "ok", resp.Text)
	assert.Contains(t, logs.String(), "write recording failed")
}

func TestRecorderForwards(t *testing.T) {
	inner := &sweepingStub{}
	rec, err := NewRecorder(inner, t.TempDir(), nil)
	require.NoError(t, err)
	require.NoError(t, rec.ReleaseFile(context.Background(), "files/x"))
	assert.Equal(t, []string{"files/x"}, inner.released)
	n, err := rec.SweepFiles(context.Background(), time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, time.Hour, inner.olderThan)

	plain, err := NewRecorder(&stubAnalyzer{}, t.TempDir(), nil)
	require.NoError(t, err)
	n, err = plain.SweepFiles(context.Background(), time.Hour)
	require.NoError(t, err)
	assert.Zero(t, n)

	_, err = NewRecorder(nil, t.TempDir(), nil)
	assert.Error(t, err)
	_, err = NewRecorder(&stubAnalyzer{}, "", nil)
	assert.Error(t, err)
}

func TestReplayRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rec, err := NewRecorder(&stubAnalyzer{text: `{"scenes":[]}`}, dir, nil)
	require.NoError(t, err)
	req := request("sha:cars:analyze")
	_, err = rec.Generate(context.Background(), req)
	require.NoError(t, err)

	var logs bytes.Buffer
	rp, err := NewReplayer(dir, slog.New(slog.NewTextHandler(&logs, nil)))
	require.NoError(t, err)
	assert.Equal(t, "recorded", rp.Name())
	resp, err := rp.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, `{"scenes":[]}`, resp.Text)
	assert.Equal(t, analyzer.Usage{
		Model: "gemini-3.5-flash-lite-001", InputTokens: 1234, OutputTokens: 56, ThinkingTokens: 7,
		Latency: 1500 * time.Millisecond,
	}, resp.Usage)
	assert.Equal(t, []string{"", ""}, resp.FileHandles)
	assert.Empty(t, logs.String(), "matching prompt must not warn")
	assert.NoError(t, rp.ReleaseFile(context.Background(), "files/abc"))

	// A changed prompt still replays, with a warning.
	changed := request("sha:cars:analyze")
	changed.Parts[1].Text = "a newer prompt"
	resp, err = rp.Generate(context.Background(), changed)
	require.NoError(t, err)
	assert.Equal(t, `{"scenes":[]}`, resp.Text)
	assert.Contains(t, logs.String(), "prompt changed since recording")
}

func TestReplayErrors(t *testing.T) {
	dir := t.TempDir()
	rp, err := NewReplayer(dir, nil)
	require.NoError(t, err)

	_, err = rp.Generate(context.Background(), request("missing"))
	assert.ErrorIs(t, err, analyzer.ErrNoRecording)
	assert.Contains(t, err.Error(), `"missing"`)

	_, err = rp.Generate(context.Background(), request(""))
	assert.ErrorIs(t, err, analyzer.ErrNoRecording)

	// A file for another key (e.g. copied by hand) is not a match.
	writeRecord(t, dir, "k1", Record{CacheKey: "other", Step: analyzer.StepAnalyze, Text: "x"})
	_, err = rp.Generate(context.Background(), request("k1"))
	assert.ErrorIs(t, err, analyzer.ErrNoRecording)

	// A recording of another step is not a match.
	writeRecord(t, dir, "k2", Record{CacheKey: "k2", Step: analyzer.StepRerank, Text: "x"})
	_, err = rp.Generate(context.Background(), request("k2"))
	assert.ErrorIs(t, err, analyzer.ErrNoRecording)

	// A corrupt file is an error, but not "no recording".
	require.NoError(t, os.WriteFile(Path(dir, "k3"), []byte("{not json"), 0o600))
	_, err = rp.Generate(context.Background(), request("k3"))
	require.Error(t, err)
	assert.False(t, errors.Is(err, analyzer.ErrNoRecording))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = rp.Generate(ctx, request("k2"))
	assert.Equal(t, context.Canceled, err)

	_, err = rp.Generate(context.Background(), nil)
	assert.Error(t, err)

	_, err = NewReplayer(filepath.Join(dir, "nope"), nil)
	assert.Error(t, err)
	_, err = NewReplayer(Path(dir, "k3"), nil)
	assert.Error(t, err, "a file is not a recordings directory")
	_, err = NewReplayer("", nil)
	assert.Error(t, err)
}

func writeRecord(t *testing.T, dir, key string, r Record) {
	t.Helper()
	data, err := json.Marshal(r)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(Path(dir, key), data, 0o600))
}
