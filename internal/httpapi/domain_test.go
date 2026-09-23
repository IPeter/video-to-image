package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
)

func TestDomainGuardMatrix(t *testing.T) {
	jobPathUnknown := "/v1/domains/cars/jobs/" + ids.NewJob()

	cases := []struct {
		name       string
		method     string
		path       string
		headers    map[string]string
		wantStatus int
		wantCode   ErrorCode // "" → not an error response
		wantCORS   bool      // Access-Control-Allow-Origin echoes the Origin
	}{
		{name: "unknown domain", method: "GET", path: "/v1/domains/boats",
			headers: map[string]string{"Origin": testOrigin}, wantStatus: 404, wantCode: CodeDomainNotFound},
		{name: "unknown domain on a job route", method: "GET", path: "/v1/domains/boats/jobs/" + ids.NewJob(),
			headers: map[string]string{"Origin": testOrigin}, wantStatus: 404, wantCode: CodeDomainNotFound},
		{name: "empty domain key", method: "GET", path: "/v1/domains/",
			wantStatus: 404, wantCode: CodeDomainNotFound},
		{name: "unknown domain beats the token", method: "POST", path: "/v1/domains/boats/jobs",
			headers: map[string]string{headerInternalToken: testToken}, wantStatus: 404, wantCode: CodeDomainNotFound},
		{name: "allowed origin", method: "GET", path: "/v1/domains/cars",
			headers: map[string]string{"Origin": testOrigin}, wantStatus: 200, wantCORS: true},
		{name: "second allowed origin", method: "GET", path: "/v1/domains/cars",
			headers: map[string]string{"Origin": "https://cars.example.com"}, wantStatus: 200, wantCORS: true},
		{name: "origin of another domain", method: "GET", path: "/v1/domains/cars",
			headers: map[string]string{"Origin": "https://homes.example.com"}, wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "disallowed origin", method: "GET", path: "/v1/domains/cars",
			headers: map[string]string{"Origin": "https://evil.example"}, wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "null origin", method: "GET", path: "/v1/domains/cars",
			headers: map[string]string{"Origin": "null"}, wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "disallowed origin on upload", method: "POST", path: "/v1/domains/cars/jobs",
			headers: map[string]string{"Origin": "https://evil.example"}, wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "disallowed origin on job status", method: "GET", path: jobPathUnknown,
			headers: map[string]string{"Origin": "https://evil.example"}, wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "no origin GET domain", method: "GET", path: "/v1/domains/cars", wantStatus: 200},
		{name: "no origin GET job reaches the handler", method: "GET", path: jobPathUnknown,
			wantStatus: 404, wantCode: CodeJobNotFound},
		{name: "no origin POST", method: "POST", path: "/v1/domains/cars/jobs",
			wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "no origin DELETE", method: "DELETE", path: jobPathUnknown,
			wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "no origin POST with token reaches the handler", method: "POST", path: "/v1/domains/cars/jobs",
			headers:    map[string]string{headerInternalToken: testToken, "Content-Type": "application/json"},
			wantStatus: 400, wantCode: CodeInvalidRequest},
		{name: "no origin DELETE with token reaches the handler", method: "DELETE", path: jobPathUnknown,
			headers: map[string]string{headerInternalToken: testToken}, wantStatus: 404, wantCode: CodeJobNotFound},
		{name: "disallowed origin with token", method: "GET", path: "/v1/domains/cars",
			headers: map[string]string{"Origin": "https://evil.example", headerInternalToken: testToken}, wantStatus: 200},
		{name: "wrong token", method: "POST", path: "/v1/domains/cars/jobs",
			headers: map[string]string{headerInternalToken: testToken + "x"}, wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "token prefix", method: "POST", path: "/v1/domains/cars/jobs",
			headers: map[string]string{headerInternalToken: testToken[:5]}, wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "preflight allowed", method: "OPTIONS", path: "/v1/domains/cars/jobs",
			headers: map[string]string{"Origin": testOrigin, "Access-Control-Request-Method": "POST"}, wantStatus: 204, wantCORS: true},
		{name: "preflight on a job route", method: "OPTIONS", path: jobPathUnknown,
			headers: map[string]string{"Origin": testOrigin, "Access-Control-Request-Method": "DELETE"}, wantStatus: 204, wantCORS: true},
		{name: "preflight denied", method: "OPTIONS", path: "/v1/domains/cars/jobs",
			headers:    map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST"},
			wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "preflight denied despite token", method: "OPTIONS", path: "/v1/domains/cars/jobs",
			headers:    map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "POST", headerInternalToken: testToken},
			wantStatus: 403, wantCode: CodeOriginNotAllowed},
		{name: "outside the guard", method: "GET", path: "/livez", headers: map[string]string{"Origin": "https://evil.example"}, wantStatus: 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			rec := e.do(r)
			if tc.wantCode != "" {
				requireError(t, rec, tc.wantStatus, tc.wantCode)
			} else {
				require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			}
			h := rec.Header()
			if tc.wantCORS {
				assert.Equal(t, tc.headers["Origin"], h.Get("Access-Control-Allow-Origin"))
				assert.Contains(t, h.Values("Vary"), "Origin")
			} else {
				assert.Empty(t, h.Get("Access-Control-Allow-Origin"))
			}
			if strings.HasPrefix(tc.path, "/v1/domains/cars") {
				assert.Contains(t, h.Values("Vary"), "Origin", "every domain-scoped response varies by Origin")
			}
		})
	}
}

func TestPreflightHeaders(t *testing.T) {
	e := newEnv(t)
	r := httptest.NewRequest(http.MethodOptions, "/v1/domains/cars/jobs", nil)
	r.Header.Set("Origin", testOrigin)
	r.Header.Set("Access-Control-Request-Method", "POST")
	r.Header.Set("Access-Control-Request-Headers", "content-type")
	rec := e.do(r)

	require.Equal(t, http.StatusNoContent, rec.Code)
	h := rec.Header()
	assert.Equal(t, testOrigin, h.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "GET, POST, DELETE, OPTIONS", h.Get("Access-Control-Allow-Methods"))
	assert.Equal(t, "Content-Type", h.Get("Access-Control-Allow-Headers"))
	assert.Equal(t, "600", h.Get("Access-Control-Max-Age"))
	assert.Contains(t, h.Values("Vary"), "Origin")
	assert.Empty(t, rec.Body.String())
}

func TestCORSHeadersOnActualAndErrorResponses(t *testing.T) {
	e := newEnv(t)
	// A successful response and an error response from a handler both carry the CORS
	// headers, so the browser can read the envelope and Retry-After/Location.
	for _, path := range []string{"/v1/domains/cars", "/v1/domains/cars/jobs/" + ids.NewJob()} {
		rec := e.get(path)
		h := rec.Header()
		assert.Equal(t, testOrigin, h.Get("Access-Control-Allow-Origin"), path)
		assert.Equal(t, "Content-Disposition, Retry-After, Location", h.Get("Access-Control-Expose-Headers"), path)
		assert.Contains(t, h.Values("Vary"), "Origin", path)
	}
}

func TestOriginMatchingIsNormalised(t *testing.T) {
	e := newEnv(t, func(e *testEnv, _ *Deps) {
		e.cars.AllowedOrigins = []string{"HTTPS://Cars.Example.com/"}
	})
	rec := e.do(func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/v1/domains/cars", nil)
		r.Header.Set("Origin", "https://cars.example.com")
		return r
	}())
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "https://cars.example.com", rec.Header().Get("Access-Control-Allow-Origin"), "echo the request's Origin exactly")
}

func TestNoInternalTokenConfiguredNeverBypasses(t *testing.T) {
	e := newEnv(t, func(_ *testEnv, d *Deps) { d.InternalToken = "" })
	for _, tok := range []string{"", " ", "anything"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/domains/cars/jobs", nil)
		r.Header.Set(headerInternalToken, tok)
		requireError(t, e.do(r), http.StatusForbidden, CodeOriginNotAllowed)
	}
}

func TestGetDomain(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/domains/cars")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "public, max-age=300", rec.Header().Get("Cache-Control"))
	assert.Equal(t, e.cars.Public(), decodeJSON[domain.PublicDomain](t, rec))

	body := rec.Body.String()
	for _, secret := range []string{"SECRET", "secret-model", e.cars.Gate.Subject} {
		assert.NotContains(t, body, secret, "prompts and model names never leave the server")
	}

	rec = e.get("/v1/domains/homes", "Origin", "https://homes.example.com")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "homes", decodeJSON[domain.PublicDomain](t, rec).Key)
}

func TestUnknownRoutesUseTheEnvelope(t *testing.T) {
	e := newEnv(t)
	requireError(t, e.get("/v2/nothing"), http.StatusNotFound, CodeNotFound)
	requireError(t, e.get("/v1/domains/cars/unknown"), http.StatusNotFound, CodeNotFound)

	r := httptest.NewRequest(http.MethodPut, "/v1/domains/cars/jobs/"+ids.NewJob(), nil)
	r.Header.Set(headerInternalToken, testToken)
	rec := e.do(r)
	requireError(t, rec, http.StatusMethodNotAllowed, CodeInvalidRequest)
	assert.Equal(t, "GET, DELETE", rec.Header().Get("Allow"))
}

func TestSplitDomainPath(t *testing.T) {
	cases := []struct {
		path, key, rest string
		ok              bool
	}{
		{"/v1/domains/cars", "cars", "", true},
		{"/v1/domains/cars/", "cars", "", true},
		{"/v1/domains/cars/jobs", "cars", "jobs", true},
		{"/v1/domains/cars/jobs/job_1/images/img_2", "cars", "jobs/job_1/images/img_2", true},
		{"/v1/domains/", "", "", true},
		{"/v1/domains", "", "", false},
		{"/healthz", "", "", false},
	}
	for _, tc := range cases {
		key, rest, ok := splitDomainPath(tc.path)
		assert.Equal(t, tc.ok, ok, tc.path)
		assert.Equal(t, tc.key, key, tc.path)
		assert.Equal(t, tc.rest, rest, tc.path)
	}
}

func TestClassify(t *testing.T) {
	assert.Equal(t, classUpload, classify("POST", "jobs"))
	assert.Equal(t, classStatus, classify("GET", "jobs/job_x"))
	assert.Equal(t, classStatus, classify("GET", "jobs/job_x/images/img_y/thumb"))
	assert.Equal(t, classStatus, classify("GET", "jobs/job_x/download.zip"))
	assert.Equal(t, classOther, classify("GET", ""))
	assert.Equal(t, classOther, classify("DELETE", "jobs/job_x"))
	assert.Equal(t, classOther, classify("POST", "jobs/job_x"))
}
