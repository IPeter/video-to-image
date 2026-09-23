package recording

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"

	"video-to-image/internal/analyzer"
)

// Replayer answers requests from recordings made by Recorder, with no network. It is
// safe for concurrent use.
type Replayer struct {
	dir string
	log *slog.Logger
}

var _ analyzer.Analyzer = (*Replayer)(nil)

// NewReplayer returns a Replayer reading from dir, which must exist. A nil logger means
// slog.Default().
func NewReplayer(dir string, logger *slog.Logger) (*Replayer, error) {
	if dir == "" {
		return nil, errors.New("recording: recordings directory is required")
	}
	st, err := os.Stat(dir)
	if err != nil {
		return nil, fmt.Errorf("recording: recordings dir: %w", err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("recording: recordings dir %q is not a directory", dir)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Replayer{dir: dir, log: logger}, nil
}

// Name returns "recorded".
func (p *Replayer) Name() string { return "recorded" }

// Generate returns the recorded text and usage for req.CacheKey. A missing recording
// (or an empty key, or a recording made for another key or step) is an error wrapping
// analyzer.ErrNoRecording. FileHandles has one empty entry per part.
func (p *Replayer) Generate(ctx context.Context, req *analyzer.Request) (*analyzer.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("recording: nil request")
	}
	if req.CacheKey == "" {
		return nil, fmt.Errorf("recording: request has no cache key: %w", analyzer.ErrNoRecording)
	}
	path := Path(p.dir, req.CacheKey)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("recording: %w: cache key %q (expected %s)", analyzer.ErrNoRecording, req.CacheKey, path)
	}
	if err != nil {
		return nil, fmt.Errorf("recording: read %s: %w", path, err)
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("recording: decode %s: %w", path, err)
	}
	if rec.CacheKey != req.CacheKey {
		return nil, fmt.Errorf("recording: %w: %s holds cache key %q, want %q", analyzer.ErrNoRecording, path, rec.CacheKey, req.CacheKey)
	}
	if rec.Step != "" && req.Step != "" && rec.Step != req.Step {
		return nil, fmt.Errorf("recording: %w: %s was recorded for step %q, want %q", analyzer.ErrNoRecording, path, rec.Step, req.Step)
	}
	if rec.PromptSHA256 != "" && rec.PromptSHA256 != promptFingerprint(req) {
		p.log.WarnContext(ctx, "recording: prompt changed since recording; replaying the old answer",
			"job_id", req.JobID, "domain", req.DomainKey, "step", req.Step,
			"cache_key", req.CacheKey, "recorded_at", rec.RecordedAt)
	}
	return &analyzer.Response{
		Text:        rec.Text,
		Usage:       rec.Usage.analyzer(),
		FileHandles: make([]string, len(req.Parts)),
	}, nil
}

// ReleaseFile is a no-op: nothing is uploaded during replay.
func (p *Replayer) ReleaseFile(context.Context, string) error { return nil }
