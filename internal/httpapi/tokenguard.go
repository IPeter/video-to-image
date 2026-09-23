package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// tokenGuard validates X-Internal-Token once per request and records the result for
// the handlers (internalFrom). The token bypasses Origin checks and rate limits, so
// guessing it must not be free: every wrong value costs the client one of
// tokenFailuresPerMinute tokens, and once they are spent EVERY request carrying the
// header is refused with 429 before the token is even compared (otherwise the one
// correct guess would still get through). Requests without the header are untouched,
// and browsers never send it (it is not in the CORS allow list).
func (s *Server) tokenGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(headerInternalToken)
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		key := s.rateKey(r)
		if blocked, wait := s.tokenFailures.blocked(key); blocked {
			s.d.Logger.DebugContext(ctx, "httpapi: X-Internal-Token attempts throttled", "client_ip_hash", s.ipHash(r))
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
			writeAPIError(w, NewAPIError(http.StatusTooManyRequests, CodeRateLimited,
				"too many invalid X-Internal-Token attempts, please retry later"))
			return
		}
		if s.validInternalToken(token) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKeyInternal, true)))
			return
		}
		s.tokenFailures.allow(key, tokenFailuresPerMinute, time.Minute)
		s.d.Logger.WarnContext(ctx, "httpapi: invalid X-Internal-Token", "client_ip_hash", s.ipHash(r), "path", truncate(r.URL.Path, 256))
		next.ServeHTTP(w, r) // served as an ordinary, unauthenticated request
	})
}

// internalFrom reports whether tokenGuard accepted the request's X-Internal-Token.
func internalFrom(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyInternal).(bool)
	return v
}
