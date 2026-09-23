package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

func httpRequests(t *testing.T, route, method, code string) float64 {
	t.Helper()
	return counterValue(t, "v2i_http_requests_total", map[string]string{"route": route, "method": method, "code": code})
}

func TestPanicRecovery(t *testing.T) {
	e := newEnv(t)
	e.store.getPanic = true
	route := "/v1/domains/{domainKey}/jobs/{jobId}"
	before := httpRequests(t, route, "GET", "500")

	rec := e.get("/v1/domains/cars/jobs/" + ids.NewJob())

	requireError(t, rec, http.StatusInternalServerError, CodeInternal)
	assert.NotContains(t, rec.Body.String(), "boom")
	assert.Equal(t, before+1, httpRequests(t, route, "GET", "500"))

	var sawPanic, sawRequest bool
	for _, l := range e.logs.lines(t) {
		switch l["msg"] {
		case "httpapi: panic serving request":
			sawPanic = true
			assert.Equal(t, "fake store: boom", l["panic"])
			assert.Contains(t, l["stack"], "runtime/debug.Stack")
		case "http request":
			sawRequest = true
			assert.EqualValues(t, 500, l["status"])
			assert.Equal(t, "ERROR", l["level"])
		}
	}
	assert.True(t, sawPanic, e.logs.String())
	assert.True(t, sawRequest, e.logs.String())
}

// panicReader panics mid-body, after the response headers went out.
type panicReader struct{}

func (panicReader) Read([]byte) (int, error) { panic("disk on fire") }
func (panicReader) Close() error             { return nil }

func TestPanicAfterHeadersAbortsTheResponse(t *testing.T) {
	f := newFileFixture(t)
	f.e.storage.getOverride = func(string) (io.ReadCloser, error) { return panicReader{}, nil }
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, f.base+"/images/"+f.imageID, nil)
	assert.PanicsWithValue(t, http.ErrAbortHandler, func() { f.e.srv.Handler().ServeHTTP(rec, r) },
		"a started response must be aborted, not completed with a truncated body")
	assert.Contains(t, f.e.logs.String(), "disk on fire")
	assert.Contains(t, f.e.logs.String(), `"msg":"http request"`, "the request is still logged")
}

func TestRequestLogLine(t *testing.T) {
	e := newEnv(t)
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusQueued})
	rec := e.get("/v1/domains/cars/jobs/"+j.ID, "X-Request-Id", "req-123")
	require.Equal(t, http.StatusOK, rec.Code)

	lines := e.logs.lines(t)
	require.NotEmpty(t, lines)
	l := lines[len(lines)-1]
	assert.Equal(t, "http request", l["msg"])
	assert.Equal(t, "INFO", l["level"])
	assert.Equal(t, "GET", l["method"])
	assert.Equal(t, "/v1/domains/{domainKey}/jobs/{jobId}", l["route"])
	assert.EqualValues(t, 200, l["status"])
	assert.EqualValues(t, rec.Body.Len(), l["bytes"])
	assert.Contains(t, l, "duration")
	assert.Equal(t, "req-123", l["request_id"])
	assert.Equal(t, "cars", l["domain"])
	assert.Equal(t, j.ID, l["job_id"])
	assert.NotContains(t, l, "path", "the raw path is only logged for unmatched routes")

	// Probes log at debug level, without domain/job.
	e.get("/livez")
	lines = e.logs.lines(t)
	l = lines[len(lines)-1]
	assert.Equal(t, "/livez", l["route"])
	assert.Equal(t, "DEBUG", l["level"])
	assert.NotContains(t, l, "domain")
	assert.NotContains(t, l, "request_id")

	e.get("/no/such/route")
	lines = e.logs.lines(t)
	l = lines[len(lines)-1]
	assert.Equal(t, unmatchedRoute, l["route"])
	assert.Equal(t, "/no/such/route", l["path"])
}

func TestMetricsUseRoutePatterns(t *testing.T) {
	e := newEnv(t)
	f := newFileFixture(t)
	jobRoute := "/v1/domains/{domainKey}/jobs/{jobId}"
	imgRoute := "/v1/domains/{domainKey}/jobs/{jobId}/images/{imageId}"
	createRoute := "/v1/domains/{domainKey}/jobs"

	b404 := httpRequests(t, jobRoute, "GET", "404")
	b200 := httpRequests(t, imgRoute, "GET", "200")
	b403 := httpRequests(t, createRoute, "POST", "403")
	b204 := httpRequests(t, createRoute, "OPTIONS", "204")
	bUnmatched := httpRequests(t, unmatchedRoute, "OTHER", "405")

	e.get("/v1/domains/cars/jobs/" + ids.NewJob())
	f.e.get(f.base + "/images/" + f.imageID)
	// Answered by the domain guard before routing: still labelled by pattern.
	e.do(httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", nil))
	pre := httptest.NewRequest(http.MethodOptions, "/v1/domains/cars/jobs", nil)
	pre.Header.Set("Origin", testOrigin)
	pre.Header.Set("Access-Control-Request-Method", "POST")
	e.do(pre)
	e.do(httptest.NewRequest("BREW", "/coffee", nil))

	assert.Equal(t, b404+1, httpRequests(t, jobRoute, "GET", "404"))
	assert.Equal(t, b200+1, httpRequests(t, imgRoute, "GET", "200"))
	assert.Equal(t, b403+1, httpRequests(t, createRoute, "POST", "403"))
	assert.Equal(t, b204+1, httpRequests(t, createRoute, "OPTIONS", "204"))
	assert.Equal(t, bUnmatched+1, httpRequests(t, unmatchedRoute, "OTHER", "405"))
	assert.Positive(t, counterValue(t, "v2i_http_request_duration_seconds", map[string]string{"route": jobRoute, "method": "GET"}))

	// No series ever carries a raw ID.
	mfs, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "v2i_http_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				assert.NotContains(t, lp.GetValue(), "job_", "%s{%s}", mf.GetName(), lp.GetName())
				assert.NotContains(t, lp.GetValue(), "img_", "%s{%s}", mf.GetName(), lp.GetName())
			}
		}
	}
}

func TestMetricsNotOnPublicRouter(t *testing.T) {
	// /metrics (cost, traffic, runtime internals) is served on the admin listener,
	// never on the API listener that the Ingress exposes.
	e := newEnv(t)
	e.get("/livez")
	for _, hdr := range [][]string{nil, {headerInternalToken, testToken}} {
		rec := e.get("/metrics", hdr...)
		requireError(t, rec, http.StatusNotFound, CodeNotFound)
		assert.NotContains(t, rec.Body.String(), "v2i_")
	}
}

func TestErrorHeadersDropped(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Length", "99")
	rec.Header().Set("Content-Disposition", "attachment")
	rec.Header().Set("ETag", `"x"`)
	rec.Header().Set("Retry-After", "5")
	writeAPIError(rec, errExpired())
	assert.Empty(t, rec.Header().Get("Content-Length"))
	assert.Empty(t, rec.Header().Get("Content-Disposition"))
	assert.Empty(t, rec.Header().Get("ETag"))
	assert.Equal(t, "5", rec.Header().Get("Retry-After"), "Retry-After belongs to the error")
	requireError(t, rec, http.StatusGone, CodeExpired)
}
