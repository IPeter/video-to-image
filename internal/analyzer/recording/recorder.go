package recording

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"video-to-image/internal/analyzer"
)

// Recorder wraps a real analyzer and writes every successful response with a non-empty
// CacheKey to its directory. It is safe for concurrent use.
type Recorder struct {
	inner analyzer.Analyzer
	dir   string
	log   *slog.Logger
	now   func() time.Time
}

var (
	_ analyzer.Analyzer = (*Recorder)(nil)
	_ analyzer.Sweeper  = (*Recorder)(nil)
)

// NewRecorder returns a Recorder writing to dir, which is created if missing. A nil
// logger means slog.Default().
func NewRecorder(inner analyzer.Analyzer, dir string, logger *slog.Logger) (*Recorder, error) {
	if inner == nil {
		return nil, errors.New("recording: nil inner analyzer")
	}
	if dir == "" {
		return nil, errors.New("recording: recordings directory is required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("recording: create recordings dir: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Recorder{inner: inner, dir: dir, log: logger, now: time.Now}, nil
}

// Name returns "recording".
func (r *Recorder) Name() string { return "recording" }

// Dir is the recordings directory.
func (r *Recorder) Dir() string { return r.dir }

// Generate calls the inner analyzer and records its answer. Errors of the inner call
// are returned unchanged and nothing is recorded. A failure to write the recording is
// logged, not returned: the model call already succeeded (and was paid for), so the
// job carries on with the response.
func (r *Recorder) Generate(ctx context.Context, req *analyzer.Request) (*analyzer.Response, error) {
	resp, err := r.inner.Generate(ctx, req)
	if err != nil || resp == nil || req == nil || req.CacheKey == "" {
		return resp, err
	}
	if werr := r.write(req, resp); werr != nil {
		r.log.ErrorContext(ctx, "recording: write recording failed",
			"job_id", req.JobID, "domain", req.DomainKey, "step", req.Step,
			"cache_key", req.CacheKey, "err", werr)
	}
	return resp, nil
}

func (r *Recorder) write(req *analyzer.Request, resp *analyzer.Response) error {
	rec := Record{
		CacheKey:     req.CacheKey,
		Step:         req.Step,
		Model:        req.Model,
		Text:         resp.Text,
		Images:       imagesFrom(resp.Images),
		Usage:        usageFrom(resp.Usage),
		PromptSHA256: promptFingerprint(req),
		RecordedAt:   r.now().UTC(),
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("recording: encode: %w", err)
	}
	data = append(data, '\n')
	if err := writeAtomic(Path(r.dir, req.CacheKey), data); err != nil {
		return fmt.Errorf("recording: write %s: %w", FileName(req.CacheKey), err)
	}
	return nil
}

// ReleaseFile forwards to the inner analyzer.
func (r *Recorder) ReleaseFile(ctx context.Context, handle string) error {
	return r.inner.ReleaseFile(ctx, handle)
}

// SweepFiles forwards to the inner analyzer when it is a Sweeper; otherwise it deletes
// nothing.
func (r *Recorder) SweepFiles(ctx context.Context, olderThan time.Duration) (int, error) {
	if s, ok := r.inner.(analyzer.Sweeper); ok {
		return s.SweepFiles(ctx, olderThan)
	}
	return 0, nil
}
