package analyzer

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/time/rate"
)

// WithRateLimit returns an Analyzer that waits on one global token bucket before each
// Generate call, so at most callsPerMinute calls start per minute across all goroutines
// sharing the returned value (plan/03-pipeline.md "Cost controls"). The bucket refills
// at callsPerMinute/60 tokens per second and holds max(1, callsPerMinute/6) tokens, so a
// quiet worker may start a short burst of calls at once.
//
// callsPerMinute <= 0 disables limiting and returns a unchanged. If a implements
// Sweeper, the returned value does too. Provider-internal retries (e.g. gemini's
// 429/5xx backoff) are not counted; they are already spaced by their own backoff.
//
// A context that ends while waiting returns ctx.Err(). A context whose deadline is too
// close for a token to become available fails immediately with an error wrapping
// context.DeadlineExceeded, without calling a.
func WithRateLimit(a Analyzer, callsPerMinute int) Analyzer {
	if a == nil || callsPerMinute <= 0 {
		return a
	}
	rl := &rateLimited{
		inner:   a,
		limiter: rate.NewLimiter(rate.Limit(float64(callsPerMinute)/60), max(1, callsPerMinute/6)),
	}
	if s, ok := a.(Sweeper); ok {
		return &rateLimitedSweeper{rateLimited: rl, sweeper: s}
	}
	return rl
}

// rateLimited is the WithRateLimit wrapper for analyzers without file uploads.
type rateLimited struct {
	inner   Analyzer
	limiter *rate.Limiter
}

// Generate waits for a token, then delegates to the wrapped analyzer.
func (r *rateLimited) Generate(ctx context.Context, req *Request) (*Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.limiter.Wait(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		// rate.Limiter reports "would exceed context deadline" before the deadline has
		// actually passed; surface it as a deadline error so callers treat it as one.
		return nil, fmt.Errorf("analyzer: rate limit: no model call slot before the context deadline: %w", context.DeadlineExceeded)
	}
	return r.inner.Generate(ctx, req)
}

// ReleaseFile is not rate limited; it is not a model call.
func (r *rateLimited) ReleaseFile(ctx context.Context, handle string) error {
	return r.inner.ReleaseFile(ctx, handle)
}

// Name returns the wrapped analyzer's name.
func (r *rateLimited) Name() string { return r.inner.Name() }

// Unwrap returns the wrapped analyzer.
func (r *rateLimited) Unwrap() Analyzer { return r.inner }

// rateLimitedSweeper keeps the Sweeper capability of the wrapped analyzer.
type rateLimitedSweeper struct {
	*rateLimited
	sweeper Sweeper
}

// SweepFiles delegates to the wrapped analyzer; it is not rate limited.
func (r *rateLimitedSweeper) SweepFiles(ctx context.Context, olderThan time.Duration) (int, error) {
	return r.sweeper.SweepFiles(ctx, olderThan)
}
