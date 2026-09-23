package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHealthz(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(e *testEnv)
		wantStatus int
		want       map[string]any
	}{
		{
			name:       "ok",
			wantStatus: http.StatusOK,
			want:       map[string]any{"ok": true, "ffmpeg": "9.0.1", "db": "ok", "storage": "ok"},
		},
		{
			name:       "database down",
			setup:      func(e *testEnv) { e.store.pingErr = errors.New("dial tcp 10.1.2.3:5432: refused") },
			wantStatus: http.StatusServiceUnavailable,
			want:       map[string]any{"ok": false, "ffmpeg": "9.0.1", "db": "error", "storage": "ok"},
		},
		{
			name:       "storage down",
			setup:      func(e *testEnv) { e.storage.pingErr = errors.New("bucket v2i missing") },
			wantStatus: http.StatusServiceUnavailable,
			want:       map[string]any{"ok": false, "ffmpeg": "9.0.1", "db": "ok", "storage": "error"},
		},
		{
			name: "both down",
			setup: func(e *testEnv) {
				e.store.pingErr = errors.New("db")
				e.storage.pingErr = errors.New("s3")
			},
			wantStatus: http.StatusServiceUnavailable,
			want:       map[string]any{"ok": false, "ffmpeg": "9.0.1", "db": "error", "storage": "error"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			// Probes never send an Origin, and a stray one does not matter. Only an
			// internal-token caller learns the ffmpeg build.
			rec := e.get("/healthz", "Origin", "https://evil.example", headerInternalToken, testToken)
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			assert.Equal(t, tc.want, decodeJSON[map[string]any](t, rec))
			assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
			assert.NotContains(t, rec.Body.String(), "10.1.2.3", "details stay in the log")
			if tc.name == "database down" {
				assert.Contains(t, e.logs.String(), "10.1.2.3")
			}

			rec = e.get("/healthz")
			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			public := decodeJSON[map[string]any](t, rec)
			assert.NotContains(t, public, "ffmpeg", "the public body does not name the ffmpeg build")
			delete(tc.want, "ffmpeg")
			assert.Equal(t, tc.want, public)
		})
	}
}

func TestHealthzMissingDependencies(t *testing.T) {
	e := newEnv(t, func(_ *testEnv, d *Deps) { d.Store, d.Storage = nil, nil })
	rec := e.get("/healthz")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, map[string]any{"ok": false, "db": "error", "storage": "error"}, decodeJSON[map[string]any](t, rec))
}

func TestHealthzWrongTokenGetsPublicBody(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/healthz", headerInternalToken, "guess")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, decodeJSON[map[string]any](t, rec), "ffmpeg")
}

func TestHealthzRateLimitedPerClient(t *testing.T) {
	e := newEnv(t)
	for i := range healthzPerMinute {
		require.Equal(t, http.StatusOK, e.get("/healthz").Code, "probe %d", i)
	}
	rec := e.get("/healthz")
	requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	assert.Equal(t, "1", rec.Header().Get("Retry-After"), "one probe per second refills")

	// Other clients and internal-token callers are not affected; nor is /livez.
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.RemoteAddr = "198.51.100.9:1"
	assert.Equal(t, http.StatusOK, e.do(r).Code)
	assert.Equal(t, http.StatusOK, e.get("/healthz", headerInternalToken, testToken).Code)
	assert.Equal(t, http.StatusOK, e.get("/livez").Code)

	e.clock.Advance(time.Second)
	assert.Equal(t, http.StatusOK, e.get("/healthz").Code)
}

func TestHealthzHungDependencyTimesOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.store.pingHang = true // ignores its context for 10 s
	start := time.Now()
	rec := e.get("/healthz")
	elapsed := time.Since(start)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "error", decodeJSON[map[string]any](t, rec)["db"])
	assert.Less(t, elapsed, 4*time.Second, "each check is bounded by 2 s")
	assert.GreaterOrEqual(t, elapsed, healthCheckTimeout)
}

func TestPingWithTimeoutReturnsPingResult(t *testing.T) {
	err := pingWithTimeout(context.Background(), time.Second, func(ctx context.Context) error {
		_, ok := ctx.Deadline()
		assert.True(t, ok, "the ping gets a deadline")
		return nil
	})
	assert.NoError(t, err)
	boom := errors.New("boom")
	assert.ErrorIs(t, pingWithTimeout(context.Background(), time.Second, func(context.Context) error { return boom }), boom)
	err = pingWithTimeout(context.Background(), 10*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestLivez(t *testing.T) {
	e := newEnv(t, func(_ *testEnv, d *Deps) { d.Store = nil })
	rec := e.get("/livez")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"ok":true}`, rec.Body.String())
}
