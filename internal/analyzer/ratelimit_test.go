package analyzer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingAnalyzer counts Generate calls; it is safe for concurrent use.
type countingAnalyzer struct {
	calls    atomic.Int64
	released []string
	mu       sync.Mutex
}

func (c *countingAnalyzer) Generate(_ context.Context, req *Request) (*Response, error) {
	c.calls.Add(1)
	return &Response{Text: "ok", Usage: Usage{Model: req.Model}}, nil
}

func (c *countingAnalyzer) ReleaseFile(_ context.Context, handle string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = append(c.released, handle)
	return nil
}

func (c *countingAnalyzer) Name() string { return "counting" }

// sweepingAnalyzer additionally implements Sweeper.
type sweepingAnalyzer struct {
	countingAnalyzer
	sweptOlderThan time.Duration
}

func (s *sweepingAnalyzer) SweepFiles(_ context.Context, olderThan time.Duration) (int, error) {
	s.sweptOlderThan = olderThan
	return 7, nil
}

func TestWithRateLimitDisabled(t *testing.T) {
	inner := &countingAnalyzer{}
	for _, n := range []int{0, -1, -100} {
		assert.Same(t, inner, WithRateLimit(inner, n), "callsPerMinute=%d", n)
	}
	assert.Nil(t, WithRateLimit(nil, 10))
}

func TestWithRateLimitKeepsSweeper(t *testing.T) {
	plain := WithRateLimit(&countingAnalyzer{}, 60)
	_, ok := plain.(Sweeper)
	assert.False(t, ok, "a non-sweeper must not gain a Sweeper method")

	inner := &sweepingAnalyzer{}
	wrapped := WithRateLimit(inner, 60)
	s, ok := wrapped.(Sweeper)
	require.True(t, ok, "the wrapper must keep the inner Sweeper")
	n, err := s.SweepFiles(context.Background(), 6*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 7, n)
	assert.Equal(t, 6*time.Hour, inner.sweptOlderThan)
}

func TestWithRateLimitForwards(t *testing.T) {
	inner := &countingAnalyzer{}
	wrapped := WithRateLimit(inner, 60)
	assert.Equal(t, "counting", wrapped.Name())
	require.NoError(t, wrapped.ReleaseFile(context.Background(), "files/abc"))
	assert.Equal(t, []string{"files/abc"}, inner.released)

	resp, err := wrapped.Generate(context.Background(), &Request{Model: "m"})
	require.NoError(t, err)
	assert.Equal(t, "ok", resp.Text)
	assert.Equal(t, "m", resp.Usage.Model)

	u, ok := wrapped.(interface{ Unwrap() Analyzer })
	require.True(t, ok)
	assert.Same(t, inner, u.Unwrap())
}

func TestWithRateLimitBurstThenDeadline(t *testing.T) {
	tests := []struct {
		perMinute int
		burst     int
	}{
		{perMinute: 60, burst: 10}, // 1/s, burst 60/6
		{perMinute: 5, burst: 1},   // burst floor is 1
		{perMinute: 6, burst: 1},
	}
	for _, tt := range tests {
		inner := &countingAnalyzer{}
		a := WithRateLimit(inner, tt.perMinute)
		for i := 0; i < tt.burst; i++ {
			_, err := a.Generate(context.Background(), &Request{})
			require.NoError(t, err)
		}
		// The next token is ≥ 1 s away, so a 100 ms deadline cannot be met: the wrapper
		// must fail fast without calling the provider.
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		start := time.Now()
		_, err := a.Generate(ctx, &Request{})
		cancel()
		require.Error(t, err)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(start), 90*time.Millisecond, "should not sleep until the deadline")
		assert.Equal(t, int64(tt.burst), inner.calls.Load(), "perMinute=%d", tt.perMinute)
	}
}

func TestWithRateLimitWaitsForRefill(t *testing.T) {
	inner := &countingAnalyzer{}
	a := WithRateLimit(inner, 600) // 10/s, burst 100
	burstStart := time.Now()
	for i := 0; i < 100; i++ {
		_, err := a.Generate(context.Background(), &Request{})
		require.NoError(t, err)
	}
	if time.Since(burstStart) > 40*time.Millisecond {
		t.Skip("machine too slow for a meaningful timing assertion")
	}
	start := time.Now()
	_, err := a.Generate(context.Background(), &Request{})
	require.NoError(t, err)
	// The next token is ~100 ms after the burst began.
	assert.GreaterOrEqual(t, time.Since(start), 50*time.Millisecond, "the 101st call must wait for a refill")
	assert.Equal(t, int64(101), inner.calls.Load())
}

func TestWithRateLimitContext(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		inner := &countingAnalyzer{}
		a := WithRateLimit(inner, 60)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := a.Generate(ctx, &Request{})
		assert.Equal(t, context.Canceled, err, "context errors are returned as-is")
		assert.Zero(t, inner.calls.Load())
	})
	t.Run("canceled while waiting", func(t *testing.T) {
		inner := &countingAnalyzer{}
		a := WithRateLimit(inner, 6) // burst 1, next token in 10 s
		_, err := a.Generate(context.Background(), &Request{})
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := a.Generate(ctx, &Request{})
			done <- err
		}()
		time.Sleep(20 * time.Millisecond)
		cancel()
		select {
		case err := <-done:
			assert.True(t, errors.Is(err, context.Canceled), "got %v", err)
		case <-time.After(2 * time.Second):
			t.Fatal("Generate did not return after cancel")
		}
		assert.Equal(t, int64(1), inner.calls.Load())
	})
}

func TestWithRateLimitConcurrent(t *testing.T) {
	inner := &countingAnalyzer{}
	a := WithRateLimit(inner, 60_000)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Go(func() {
			_, err := a.Generate(context.Background(), &Request{})
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	assert.Equal(t, int64(64), inner.calls.Load())
}
