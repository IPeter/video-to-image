package httpapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/store"
)

func usageRequest(query url.Values, token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/internal/usage?"+query.Encode(), nil)
	if token != "" {
		r.Header.Set(headerInternalToken, token)
	}
	return r
}

func day(s string) time.Time {
	d, err := time.Parse(dayLayout, s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestUsageAuth(t *testing.T) {
	e := newEnv(t)
	for _, tok := range []string{"", "wrong", testToken + " "} {
		requireError(t, e.do(usageRequest(nil, tok)), http.StatusUnauthorized, CodeUnauthorized)
	}
	assert.Empty(t, e.store.usageCalls)

	// Without a configured token the endpoint is closed, whatever is sent.
	e = newEnv(t, func(_ *testEnv, d *Deps) { d.InternalToken = "" })
	requireError(t, e.do(usageRequest(nil, "")), http.StatusUnauthorized, CodeUnauthorized)
	requireError(t, e.do(usageRequest(nil, "anything")), http.StatusUnauthorized, CodeUnauthorized)

	// Browser origins get no CORS headers here.
	e = newEnv(t)
	r := usageRequest(nil, testToken)
	r.Header.Set("Origin", testOrigin)
	rec := e.do(r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Empty(t, rec.Header().Get("Access-Control-Allow-Origin"))
}

func TestUsageDefaultsAndRange(t *testing.T) {
	e := newEnv(t)
	e.clock.Advance(13 * time.Hour) // 2026-09-23 23:00 UTC is still the 23rd
	rec := e.do(usageRequest(nil, testToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rep := decodeJSON[UsageReport](t, rec)
	assert.Equal(t, day("2026-09-17"), rep.From)
	assert.Equal(t, day("2026-09-23"), rep.To)
	assert.NotNil(t, rep.Rows)
	assert.Empty(t, rep.Rows)
	require.Len(t, e.store.usageCalls, 1)
	assert.Equal(t, usageCall{from: day("2026-09-17"), to: day("2026-09-24"), domainKey: ""}, e.store.usageCalls[0],
		"to is inclusive: the store gets the exclusive end")

	rec = e.do(usageRequest(url.Values{"from": {"2026-01-01"}, "to": {"2026-01-31"}, "domainKey": {"cars"}}, testToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, usageCall{from: day("2026-01-01"), to: day("2026-02-01"), domainKey: "cars"}, e.store.usageCalls[1])

	// Only "to" given: from defaults to a week ending then.
	rec = e.do(usageRequest(url.Values{"to": {"2026-03-10"}}, testToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, day("2026-03-04"), e.store.usageCalls[2].from)

	// A single day and the maximum range are fine.
	for _, q := range []url.Values{
		{"from": {"2026-09-01"}, "to": {"2026-09-01"}},
		{"from": {"2024-01-01"}, "to": {"2024-12-31"}}, // 366 days (leap year)
	} {
		assert.Equal(t, http.StatusOK, e.do(usageRequest(q, testToken)).Code, q.Encode())
	}
}

func TestUsageValidation(t *testing.T) {
	e := newEnv(t)
	for _, q := range []url.Values{
		{"from": {"2026-9-1"}},
		{"from": {"yesterday"}},
		{"to": {"2026-02-30"}},
		{"to": {"2026-09-23T00:00:00Z"}},
		{"from": {"2026-09-10"}, "to": {"2026-09-09"}},
		{"from": {"2025-01-01"}, "to": {"2026-01-02"}}, // 367 days
	} {
		requireError(t, e.do(usageRequest(q, testToken)), http.StatusBadRequest, CodeInvalidRequest)
	}
	assert.Empty(t, e.store.usageCalls)
}

func TestUsageAggregation(t *testing.T) {
	e := newEnv(t)
	e.store.usageRows = []store.UsageRow{
		{Day: day("2026-09-22"), DomainKey: "cars", Jobs: 3, Calls: 4, InputTokens: 1000, OutputTokens: 200, ThinkingTokens: 0, VideoSeconds: 120.5, CostUSD: 0.1234561},
		{Day: day("2026-09-22").In(time.FixedZone("CEST", 2*3600)), DomainKey: "homes", Jobs: 1, Calls: 1, InputTokens: 10, OutputTokens: 2, VideoSeconds: 30, CostUSD: 0.0000003},
		{Day: day("2026-09-23"), DomainKey: "cars", Jobs: 1, Calls: 1, InputTokens: 5, OutputTokens: 1, ThinkingTokens: 7, VideoSeconds: 10, CostUSD: 0.2},
	}
	rec := e.do(usageRequest(nil, testToken))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rep := decodeJSON[UsageReport](t, rec)
	require.Len(t, rep.Rows, 3)
	assert.Equal(t, UsageRow{Day: "2026-09-22", DomainKey: "cars", Jobs: 3, Calls: 4, InputTokens: 1000, OutputTokens: 200,
		VideoSeconds: 120.5, CostUSD: 0.1234561}, rep.Rows[0])
	assert.Equal(t, "2026-09-22", rep.Rows[1].Day, "days are formatted in UTC")
	assert.Equal(t, int64(7), rep.Rows[2].ThinkingTokens)
	assert.Equal(t, 0.323456, rep.TotalCostUSD, "sum rounded to 6 decimals")
}

func TestUsageStoreError(t *testing.T) {
	e := newEnv(t)
	e.store.usageErr = errors.New("relation usage_events does not exist")
	rec := e.do(usageRequest(nil, testToken))
	requireError(t, rec, http.StatusInternalServerError, CodeInternal)
	assert.NotContains(t, rec.Body.String(), "usage_events")
}
