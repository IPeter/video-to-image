package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"video-to-image/internal/domain"
	"video-to-image/internal/metrics"
)

const (
	domainsPrefix = "/v1/domains/"

	corsAllowMethods    = "GET, POST, DELETE, OPTIONS"
	corsAllowHeaders    = "Content-Type"
	corsExposeHeaders   = "Content-Disposition, Retry-After, Location"
	corsMaxAgeSeconds   = "600"
	headerInternalToken = "X-Internal-Token"
)

// routeClass selects the rate limit of a domain-scoped request.
type routeClass int

const (
	classOther  routeClass = iota
	classUpload            // POST /v1/domains/{key}/jobs
	classStatus            // GET  /v1/domains/{key}/jobs/... (status, images, ZIP)
)

func classify(method, rest string) routeClass {
	switch {
	case method == http.MethodPost && rest == "jobs":
		return classUpload
	case (method == http.MethodGet || method == http.MethodHead) && strings.HasPrefix(rest, "jobs/"):
		return classStatus
	}
	return classOther
}

// splitDomainPath splits /v1/domains/{key}/{rest...}. ok is false for paths outside
// /v1/domains/.
func splitDomainPath(p string) (key, rest string, ok bool) {
	after, ok := strings.CutPrefix(p, domainsPrefix)
	if !ok {
		return "", "", false
	}
	key, rest, _ = strings.Cut(after, "/")
	return key, rest, true
}

// domainGuard runs on every path under /v1/domains/{key}:
//
//   - an unknown key is 404 DOMAIN_NOT_FOUND; the *domain.Config goes into the context;
//   - a valid X-Internal-Token (eval CLI, server-side callers) bypasses the Origin
//     checks and the rate limits;
//   - CORS preflights are answered here from the domain's Origin allowlist;
//   - a request with an Origin header must carry an allowed one (403 ORIGIN_NOT_ALLOWED);
//   - a request WITHOUT an Origin header may only GET/HEAD (image tags, downloads,
//     server-side fetches of the public config). Unsafe methods need the internal
//     token: browsers always send Origin on cross-origin POST/DELETE, so a browser
//     request can never take this path, and a misconfigured frontend still fails
//     loudly instead of being let through;
//   - POST jobs and the GET job/file routes are rate limited per domain + client
//     (rateKey: the IPv4 address or the IPv6 /64).
func (s *Server) domainGuard(lim *guardLimiters) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key, rest, ok := splitDomainPath(routingPath(r))
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			var dc *domain.Config
			if s.d.Registry != nil {
				dc, _ = s.d.Registry.Get(key)
			}
			if dc == nil {
				writeAPIError(w, errDomainNotFound())
				return
			}
			ctx := withDomain(r.Context(), dc)
			noteDomain(ctx, dc.Key)
			r = r.WithContext(ctx)

			h := w.Header()
			h.Add("Vary", "Origin") // responses differ by Origin; keep shared caches honest
			origin := r.Header.Get("Origin")
			class := classify(r.Method, rest)

			if r.Method == http.MethodOptions && origin != "" && r.Header.Get("Access-Control-Request-Method") != "" {
				if !dc.OriginAllowed(origin) {
					s.guardReject(w, r, dc, classOther, errOriginNotAllowed())
					return
				}
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", corsAllowMethods)
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
				h.Set("Access-Control-Max-Age", corsMaxAgeSeconds)
				w.WriteHeader(http.StatusNoContent)
				return
			}

			internal := internalFrom(ctx) // tokenGuard validated X-Internal-Token
			switch {
			case origin != "" && dc.OriginAllowed(origin):
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
			case internal:
			case origin != "":
				s.guardReject(w, r, dc, class, errOriginNotAllowed())
				return
			case r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions:
				s.guardReject(w, r, dc, class, NewAPIError(http.StatusForbidden, CodeOriginNotAllowed,
					"requests without an Origin header need X-Internal-Token"))
				return
			}

			if !internal {
				var allowed bool
				var wait time.Duration
				switch class {
				case classUpload:
					allowed, wait = lim.uploads.allow(dc.Key+"|"+s.rateKey(r), dc.Limits.RatePerIPPerHour, time.Hour)
				case classStatus:
					allowed, wait = lim.status.allow(dc.Key+"|"+s.rateKey(r), s.d.StatusRatePerMinute, time.Minute)
				default:
					allowed = true
				}
				if !allowed {
					h.Set("Retry-After", strconv.Itoa(retryAfterSeconds(wait)))
					s.guardReject(w, r, dc, class, NewAPIError(http.StatusTooManyRequests, CodeRateLimited,
						"too many requests, please retry later"))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// guardReject writes a guard rejection; rejected uploads are counted and logged like
// every other upload rejection.
func (s *Server) guardReject(w http.ResponseWriter, r *http.Request, dc *domain.Config, class routeClass, e *APIError) {
	if class == classUpload {
		s.countRejection(r.Context(), dc, e)
	}
	writeAPIError(w, e)
}

// countRejection records an upload rejected before a job was created.
func (s *Server) countRejection(ctx context.Context, dc *domain.Config, e *APIError, attrs ...any) {
	metrics.UploadsRejected.WithLabelValues(dc.Key, string(e.Err.Code)).Inc()
	args := append([]any{"domain", dc.Key, "code", string(e.Err.Code), "status", e.GetStatus()}, attrs...)
	s.d.Logger.InfoContext(ctx, "httpapi: upload rejected", args...)
}

func withDomain(ctx context.Context, dc *domain.Config) context.Context {
	return context.WithValue(ctx, ctxKeyDomain, dc)
}

func domainFrom(ctx context.Context) *domain.Config {
	dc, _ := ctx.Value(ctxKeyDomain).(*domain.Config)
	return dc
}

// domainFor returns the domain the guard resolved for this request, falling back to
// a registry lookup of key.
func (s *Server) domainFor(ctx context.Context, key string) (*domain.Config, error) {
	if dc := domainFrom(ctx); dc != nil && dc.Key == key {
		return dc, nil
	}
	if s.d.Registry != nil {
		if dc, ok := s.d.Registry.Get(key); ok {
			return dc, nil
		}
	}
	return nil, errDomainNotFound()
}

func errDomainNotFound() *APIError {
	return NewAPIError(http.StatusNotFound, CodeDomainNotFound, "unknown domain")
}

func errOriginNotAllowed() *APIError {
	return NewAPIError(http.StatusForbidden, CodeOriginNotAllowed, "origin not allowed for this domain")
}

// getDomain serves the frontend-safe config (no prompts, model names or weights).
func (s *Server) getDomain(ctx context.Context, in *DomainPathInput) (*GetDomainOutput, error) {
	dc, err := s.domainFor(ctx, in.DomainKey)
	if err != nil {
		return nil, s.apiError(ctx, "get domain", err)
	}
	return &GetDomainOutput{CacheControl: "public, max-age=300", Body: dc.Public()}, nil
}
