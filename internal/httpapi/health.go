package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// healthCheckTimeout bounds each dependency ping of /healthz.
const healthCheckTimeout = 2 * time.Second

// healthResponse is the /healthz body. FFmpeg (the exact build that parses uploads)
// is only reported to callers with a valid X-Internal-Token.
type healthResponse struct {
	OK      bool   `json:"ok"`
	FFmpeg  string `json:"ffmpeg,omitempty"`
	DB      string `json:"db"`
	Storage string `json:"storage"`
}

var errNotConfigured = errors.New("not configured")

// healthz is the readiness probe: Postgres and storage are pinged concurrently, each
// with its own timeout. Failure details go to the log, never into the response. The
// route is public (plan/05), so it is rate limited per client: every call costs two
// pings. Internal-token callers are exempt and also get the ffmpeg version.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	internal := internalFrom(ctx)
	if !internal {
		if ok, wait := s.healthLimiter.allow(s.rateKey(r), healthzPerMinute, time.Minute); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			writeAPIError(w, NewAPIError(http.StatusTooManyRequests, CodeRateLimited, "too many requests, please retry later"))
			return
		}
	}
	var dbErr, storageErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		if s.d.Store == nil {
			dbErr = errNotConfigured
			return
		}
		dbErr = pingWithTimeout(ctx, healthCheckTimeout, s.d.Store.Ping)
	})
	wg.Go(func() {
		if s.d.Storage == nil {
			storageErr = errNotConfigured
			return
		}
		storageErr = pingWithTimeout(ctx, healthCheckTimeout, s.d.Storage.Ping)
	})
	wg.Wait()

	res := healthResponse{OK: true, DB: "ok", Storage: "ok"}
	if internal {
		res.FFmpeg = s.d.FFmpegVersion
	}
	if dbErr != nil {
		res.OK, res.DB = false, "error"
		s.d.Logger.WarnContext(ctx, "httpapi: healthz: database check failed", "err", dbErr)
	}
	if storageErr != nil {
		res.OK, res.Storage = false, "error"
		s.d.Logger.WarnContext(ctx, "httpapi: healthz: storage check failed", "err", storageErr)
	}
	status := http.StatusOK
	if !res.OK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, res)
}

// pingWithTimeout runs ping under a timeout and gives up waiting when it expires,
// even if ping ignores its context, so a hung dependency cannot hang the probe.
func pingWithTimeout(ctx context.Context, timeout time.Duration, ping func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan error, 1) // buffered: a late ping never blocks forever
	go func() { done <- ping(ctx) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// livez is the liveness probe: the process is up and serving HTTP.
func (s *Server) livez(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
