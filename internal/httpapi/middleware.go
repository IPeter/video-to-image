package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"video-to-image/internal/metrics"
)

// Middleware order, outermost first:
//
//  1. observe: request log line + Prometheus metrics, labelled by the chi route pattern.
//  2. recoverPanics: a panicking handler becomes a 500 INTERNAL_ERROR envelope.
//  3. tokenGuard: validates X-Internal-Token once per request and throttles clients
//     that keep sending wrong ones.
//  4. domainGuard: domain lookup, Origin allowlist / CORS and rate limits for every
//     path under /v1/domains/{key}.
//
// Unknown routes and methods also answer with the error envelope.
func (s *Server) installMiddleware(r chi.Router) {
	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, NewAPIError(http.StatusNotFound, CodeNotFound, "route not found"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, req *http.Request) {
		if allow := allowedMethods(r, req); allow != "" {
			w.Header().Set("Allow", allow)
		}
		writeAPIError(w, NewAPIError(http.StatusMethodNotAllowed, CodeInvalidRequest, "method not allowed"))
	})
	r.Use(
		s.observe(r),
		s.recoverPanics,
		s.tokenGuard,
		s.domainGuard(newGuardLimiters(s.d.Now)),
	)
}

// ---- per-request state ----

type ctxKey int

const (
	ctxKeyInfo ctxKey = iota
	ctxKeyDomain
	ctxKeyInternal
)

// requestInfo collects what the request log line reports once the handler returned.
// The domain guard and the handlers fill it in; a request is served by one goroutine.
type requestInfo struct {
	domainKey string
	jobID     string
}

func infoFrom(ctx context.Context) *requestInfo {
	ri, _ := ctx.Value(ctxKeyInfo).(*requestInfo)
	return ri
}

// noteDomain records the domain key for the request log.
func noteDomain(ctx context.Context, key string) {
	if ri := infoFrom(ctx); ri != nil {
		ri.domainKey = key
	}
}

// noteJob records the job ID for the request log.
func noteJob(ctx context.Context, id string) {
	if ri := infoFrom(ctx); ri != nil {
		ri.jobID = id
	}
}

// ---- logging + metrics ----

// maxRequestIDLen bounds a client-supplied X-Request-Id in the logs.
const maxRequestIDLen = 128

// observe logs one line per request and records v2i_http_requests_total and
// v2i_http_request_duration_seconds. Metrics are labelled with the route PATTERN
// (e.g. /v1/domains/{domainKey}/jobs/{jobId}), never the raw path, so job IDs cannot
// explode the label cardinality.
func (s *Server) observe(routes chi.Routes) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ri := &requestInfo{}
			r = r.WithContext(context.WithValue(r.Context(), ctxKeyInfo, ri))
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			// Deferred so the line is written even when an http.ErrAbortHandler panic
			// unwinds through here.
			defer func() {
				elapsed := time.Since(start)
				status := ww.Status()
				if status == 0 {
					status = http.StatusOK
				}
				route := routePattern(routes, r)
				method := methodLabel(r.Method)
				metrics.HTTPRequests.WithLabelValues(route, method, strconv.Itoa(status)).Inc()
				metrics.HTTPDuration.WithLabelValues(route, method).Observe(elapsed.Seconds())

				attrs := []slog.Attr{
					slog.String("method", r.Method),
					slog.String("route", route),
					slog.Int("status", status),
					slog.Int("bytes", ww.BytesWritten()),
					slog.Duration("duration", elapsed),
				}
				if route == unmatchedRoute {
					attrs = append(attrs, slog.String("path", truncate(r.URL.Path, 256)))
				}
				if id := requestID(r); id != "" {
					attrs = append(attrs, slog.String("request_id", id))
				}
				if ri.domainKey != "" {
					attrs = append(attrs, slog.String("domain", ri.domainKey))
				}
				if ri.jobID != "" {
					attrs = append(attrs, slog.String("job_id", ri.jobID))
				}
				s.d.Logger.LogAttrs(r.Context(), logLevel(route, status), "http request", attrs...)
			}()
			next.ServeHTTP(ww, r)
		})
	}
}

const unmatchedRoute = "unmatched"

// routePattern returns the chi pattern the request was routed to. Requests the domain
// guard answered itself never reached the router, so the pattern is looked up
// separately (a preflight is labelled with the route of the method it asks about).
func routePattern(routes chi.Routes, r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		if p := rctx.RoutePattern(); p != "" {
			return p
		}
	}
	method := r.Method
	if method == http.MethodOptions {
		if m := r.Header.Get("Access-Control-Request-Method"); m != "" {
			method = m
		}
	}
	if p := routes.Find(chi.NewRouteContext(), method, routingPath(r)); p != "" {
		return p
	}
	return unmatchedRoute
}

// allowedMethods lists the methods that have a route for the request path.
func allowedMethods(routes chi.Routes, r *http.Request) string {
	var allow []string
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		if routes.Find(chi.NewRouteContext(), m, routingPath(r)) != "" {
			allow = append(allow, m)
		}
	}
	return strings.Join(allow, ", ")
}

// routingPath is the path chi routes on.
func routingPath(r *http.Request) string {
	if r.URL.RawPath != "" {
		return r.URL.RawPath
	}
	if r.URL.Path == "" {
		return "/"
	}
	return r.URL.Path
}

// methodLabel keeps arbitrary client-sent methods out of the metric labels.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodOptions:
		return m
	}
	return "OTHER"
}

// logLevel keeps probe and scrape traffic out of the info log unless it fails.
func logLevel(route string, status int) slog.Level {
	switch {
	case status >= 500:
		return slog.LevelError
	case route == "/healthz" || route == "/livez":
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

func requestID(r *http.Request) string {
	id := strings.TrimSpace(r.Header.Get("X-Request-Id"))
	return truncate(id, maxRequestIDLen)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- panic recovery ----

// recoverPanics turns a handler panic into a 500 INTERNAL_ERROR envelope and logs the
// stack. When the response has already started it aborts the connection instead, so
// the client sees a broken response rather than a silently truncated one.
func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				panic(v)
			}
			s.d.Logger.ErrorContext(r.Context(), "httpapi: panic serving request",
				"panic", fmt.Sprint(v), "method", r.Method, "path", r.URL.Path, "stack", string(debug.Stack()))
			if ww, ok := w.(middleware.WrapResponseWriter); ok && ww.Status() != 0 {
				panic(http.ErrAbortHandler)
			}
			writeAPIError(w, NewAPIError(http.StatusInternalServerError, CodeInternal, "internal error"))
		}()
		next.ServeHTTP(w, r)
	})
}

// ---- error helpers ----

// writeAPIError writes the envelope after dropping entity headers a handler may have
// set for the success response it did not send.
func writeAPIError(w http.ResponseWriter, e *APIError) {
	h := w.Header()
	for _, k := range []string{"Content-Length", "Content-Disposition", "Content-Encoding", "ETag", "Last-Modified", "Location"} {
		h.Del(k)
	}
	WriteError(w, e)
}

// errInternal is the envelope for unexpected failures; details stay in the log.
func errInternal() *APIError {
	return NewAPIError(http.StatusInternalServerError, CodeInternal, "internal error")
}

// apiError converts any error to an envelope: *APIError values pass through, anything
// else is logged with op and becomes a generic 500.
func (s *Server) apiError(ctx context.Context, op string, err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	s.d.Logger.ErrorContext(ctx, "httpapi: "+op, "err", err)
	return errInternal()
}

// writeErr writes err on a raw route (see apiError).
func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, op string, err error) {
	writeAPIError(w, s.apiError(r.Context(), op, err))
}
