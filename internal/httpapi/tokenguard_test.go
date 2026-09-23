package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
)

func TestInternalTokenFailuresAreThrottled(t *testing.T) {
	e := newEnv(t)
	for range tokenFailuresPerMinute {
		requireError(t, e.do(usageRequest(nil, "guess")), http.StatusUnauthorized, CodeUnauthorized)
	}
	rec := e.do(usageRequest(nil, "guess"))
	requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	assert.Equal(t, "6", rec.Header().Get("Retry-After"), "10 per minute: one every 6 s")

	// While throttled even the right token is refused: otherwise the one correct guess
	// would still be recognisable.
	requireError(t, e.do(usageRequest(nil, testToken)), http.StatusTooManyRequests, CodeRateLimited)
	// Domain routes with the header are refused too; without the header nothing changes.
	requireError(t, e.get("/v1/domains/cars", headerInternalToken, testToken), http.StatusTooManyRequests, CodeRateLimited)
	assert.Equal(t, http.StatusOK, e.get("/v1/domains/cars").Code)
	requireError(t, e.do(usageRequest(nil, "")), http.StatusUnauthorized, CodeUnauthorized)

	// Another client is unaffected.
	r := usageRequest(nil, testToken)
	r.RemoteAddr = "198.51.100.9:1"
	assert.Equal(t, http.StatusOK, e.do(r).Code)

	// One token refills every 6 s; the right token then works (and costs nothing).
	e.clock.Advance(6 * time.Second)
	assert.Equal(t, http.StatusOK, e.do(usageRequest(nil, testToken)).Code)
	assert.Equal(t, http.StatusOK, e.do(usageRequest(nil, testToken)).Code)
	requireError(t, e.do(usageRequest(nil, "guess")), http.StatusUnauthorized, CodeUnauthorized)
	requireError(t, e.do(usageRequest(nil, testToken)), http.StatusTooManyRequests, CodeRateLimited)

	var warned int
	for _, l := range e.logs.lines(t) {
		if l["msg"] == "httpapi: invalid X-Internal-Token" {
			warned++
			assert.Len(t, l["client_ip_hash"], 64, "the client is identified by hash, not raw IP")
		}
	}
	assert.Equal(t, tokenFailuresPerMinute+1, warned)
}

func TestInternalTokenFailuresCountOnDomainRoutes(t *testing.T) {
	e := newEnv(t)
	path := "/v1/domains/cars/jobs/" + ids.NewJob()
	for range tokenFailuresPerMinute {
		// A wrong token is served as an ordinary request (the browser Origin still works).
		requireError(t, e.get(path, headerInternalToken, "guess"), http.StatusNotFound, CodeJobNotFound)
	}
	requireError(t, e.get(path, headerInternalToken, "guess"), http.StatusTooManyRequests, CodeRateLimited)

	// A wrong token grants nothing: an Origin-less DELETE still needs the real one.
	e2 := newEnv(t)
	r := httptest.NewRequest(http.MethodDelete, path, nil)
	r.Header.Set(headerInternalToken, "guess")
	requireError(t, e2.do(r), http.StatusForbidden, CodeOriginNotAllowed)
}

func TestInternalTokenValidDoesNotCount(t *testing.T) {
	e := newEnv(t)
	for range 3 * tokenFailuresPerMinute {
		require.Equal(t, http.StatusOK, e.do(usageRequest(nil, testToken)).Code)
	}
	assert.Zero(t, e.srv.tokenFailures.size())
}
