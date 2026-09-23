package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

func TestKeyedLimiterBurstAndRefill(t *testing.T) {
	c := &fakeClock{t: testNow}
	l := newKeyedLimiter(c.Now, 0)

	for i := range 3 {
		ok, _ := l.allow("k", 3, time.Minute)
		require.True(t, ok, "request %d within the burst", i)
	}
	ok, wait := l.allow("k", 3, time.Minute)
	require.False(t, ok)
	assert.InDelta(t, 20*time.Second, wait, float64(time.Millisecond), "one token every 20 s")

	// Denied requests do not consume tokens: still exactly 20 s until the next one.
	ok, wait = l.allow("k", 3, time.Minute)
	require.False(t, ok)
	assert.InDelta(t, 20*time.Second, wait, float64(time.Millisecond))

	c.Advance(19 * time.Second)
	ok, wait = l.allow("k", 3, time.Minute)
	require.False(t, ok)
	assert.InDelta(t, time.Second, wait, float64(time.Millisecond))

	c.Advance(time.Second)
	ok, _ = l.allow("k", 3, time.Minute)
	assert.True(t, ok)
	ok, _ = l.allow("k", 3, time.Minute)
	assert.False(t, ok)

	// Keys are independent.
	ok, _ = l.allow("other", 3, time.Minute)
	assert.True(t, ok)
}

func TestKeyedLimiterDisabled(t *testing.T) {
	l := newKeyedLimiter(nil, 0)
	for range 100 {
		ok, _ := l.allow("k", 0, time.Minute)
		require.True(t, ok)
	}
	assert.Zero(t, l.size())
}

func TestKeyedLimiterEvictsIdleBuckets(t *testing.T) {
	c := &fakeClock{t: testNow}
	l := newKeyedLimiter(c.Now, 0)
	for i := range 50 {
		l.allow(fmt.Sprint("ip", i), 5, time.Minute)
	}
	l.allow("busy", 5, time.Minute)
	require.Equal(t, 51, l.size())

	// Within the period nothing is dropped.
	c.Advance(59 * time.Second)
	l.allow("busy", 5, time.Minute)
	assert.Equal(t, 51, l.size())

	// A full period idle → refilled → dropped on the next sweep; "busy" stays.
	c.Advance(2 * time.Second)
	l.allow("busy", 5, time.Minute)
	assert.Equal(t, 1, l.size())

	// A dropped key behaves exactly like a refilled one.
	for range 5 {
		ok, _ := l.allow("ip0", 5, time.Minute)
		assert.True(t, ok)
	}
	ok, _ := l.allow("ip0", 5, time.Minute)
	assert.False(t, ok)
}

func TestKeyedLimiterIsBounded(t *testing.T) {
	c := &fakeClock{t: testNow}
	l := newKeyedLimiter(c.Now, 10)
	for i := range 1000 {
		ok, _ := l.allow(fmt.Sprint("ip", i), 1, time.Hour)
		require.True(t, ok)
		require.LessOrEqual(t, l.size(), 10)
	}
}

func TestKeyedLimiterConcurrent(t *testing.T) {
	l := newKeyedLimiter(nil, 0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 20 {
		wg.Go(func() {
			for range 50 {
				if ok, _ := l.allow("shared", 100, time.Hour); ok {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	assert.Equal(t, 100, allowed, "exactly the burst gets through")
}

func TestRetryAfterSeconds(t *testing.T) {
	assert.Equal(t, 1, retryAfterSeconds(0))
	assert.Equal(t, 1, retryAfterSeconds(time.Nanosecond))
	assert.Equal(t, 1, retryAfterSeconds(time.Second))
	assert.Equal(t, 2, retryAfterSeconds(1001*time.Millisecond))
	assert.Equal(t, 1800, retryAfterSeconds(30*time.Minute))
}

func TestUploadRateLimit(t *testing.T) {
	e := newEnv(t, func(e *testEnv, _ *Deps) { e.cars.Limits.RatePerIPPerHour = 2 })
	upload := func(hdr ...string) *httptest.ResponseRecorder {
		r := uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")
		for i := 0; i+1 < len(hdr); i += 2 {
			r.Header.Set(hdr[i], hdr[i+1])
		}
		return e.do(r)
	}
	before := rejected(t, "cars", CodeRateLimited)

	require.Equal(t, http.StatusAccepted, upload().Code)
	require.Equal(t, http.StatusAccepted, upload().Code)
	rec := upload()
	requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
	assert.Equal(t, "1800", rec.Header().Get("Retry-After"), "2 per hour → one every 30 min")
	assert.Equal(t, testOrigin, rec.Header().Get("Access-Control-Allow-Origin"), "the browser can read Retry-After")
	assert.Equal(t, before+1, rejected(t, "cars", CodeRateLimited))
	assert.Len(t, e.store.createdJobs(), 2)

	// Rejected uploads also spend tokens (abuse is not free).
	e2 := newEnv(t, func(e *testEnv, _ *Deps) { e.cars.Limits.RatePerIPPerHour = 1 })
	r := uploadRequest(t, "cars", []byte("text"), "a.txt")
	requireError(t, e2.do(r), http.StatusBadRequest, CodeUnsupportedFormat)
	requireError(t, e2.do(uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")), http.StatusTooManyRequests, CodeRateLimited)

	// Another IP has its own bucket; the internal token is never limited.
	rOther := uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")
	rOther.RemoteAddr = "198.51.100.7:1000"
	assert.Equal(t, http.StatusAccepted, e.do(rOther).Code)
	for range 3 {
		assert.Equal(t, http.StatusAccepted, upload(headerInternalToken, testToken).Code)
	}

	// Status polling uses a different limiter.
	assert.Equal(t, http.StatusNotFound, e.get("/v1/domains/cars/jobs/"+ids.NewJob()).Code)

	// Half an hour later one more upload fits.
	e.clock.Advance(30 * time.Minute)
	assert.Equal(t, http.StatusAccepted, upload().Code)
	requireError(t, upload(), http.StatusTooManyRequests, CodeRateLimited)
}

func TestStatusRateLimit(t *testing.T) {
	e := newEnv(t, func(_ *testEnv, d *Deps) { d.StatusRatePerMinute = 3 })
	j := e.store.addJob(store.Job{DomainKey: "cars", Status: store.StatusCompleted})
	paths := []string{
		"/v1/domains/cars/jobs/" + j.ID,
		"/v1/domains/cars/jobs/" + j.ID + "/images/" + ids.NewImage(),
		"/v1/domains/cars/jobs/" + j.ID + "/download.zip",
	}
	for _, p := range paths {
		assert.NotEqual(t, http.StatusTooManyRequests, e.get(p).Code, p)
	}
	for _, p := range paths {
		rec := e.get(p)
		requireError(t, rec, http.StatusTooManyRequests, CodeRateLimited)
		assert.Equal(t, "20", rec.Header().Get("Retry-After"), p)
	}
	// The public domain config is cacheable and not limited; nor is the other domain.
	assert.Equal(t, http.StatusOK, e.get("/v1/domains/cars").Code)
	assert.Equal(t, http.StatusNotFound, e.get("/v1/domains/homes/jobs/"+ids.NewJob(), "Origin", "https://homes.example.com").Code)
	// Nor is the internal token.
	assert.Equal(t, http.StatusOK, e.get(paths[0], headerInternalToken, testToken).Code)

	e.clock.Advance(20 * time.Second)
	assert.Equal(t, http.StatusOK, e.get(paths[0]).Code)
	requireError(t, e.get(paths[0]), http.StatusTooManyRequests, CodeRateLimited)
}

func TestKeyedLimiterEvictsLeastRecentlyUsed(t *testing.T) {
	c := &fakeClock{t: testNow}
	l := newKeyedLimiter(c.Now, 3)
	for _, k := range []string{"a", "b", "c"} {
		l.allow(k, 1, time.Hour)
	}
	c.Advance(time.Second)
	l.allow("a", 1, time.Hour) // a is now the most recently used; b the least
	l.allow("d", 1, time.Hour)
	require.Equal(t, 3, l.size())

	ok, _ := l.allow("a", 1, time.Hour)
	assert.False(t, ok, "a was kept: its bucket is still empty")
	ok, _ = l.allow("b", 1, time.Hour)
	assert.True(t, ok, "b was evicted (least recently used) and starts full again")
}

func TestKeyedLimiterKeepsThrottledActiveClient(t *testing.T) {
	// An attacker rotating keys to fill the map must not be able to reset its own
	// (actively used, empty) bucket: eviction is LRU, not arbitrary.
	c := &fakeClock{t: testNow}
	l := newKeyedLimiter(c.Now, 100)
	ok, _ := l.allow("attacker", 1, time.Hour)
	require.True(t, ok)
	for i := range 10_000 {
		l.allow(fmt.Sprint("rotated-", i), 1, time.Hour)
		if i%50 == 0 {
			ok, _ := l.allow("attacker", 1, time.Hour)
			require.False(t, ok, "iteration %d: the throttled bucket survived", i)
		}
	}
	assert.LessOrEqual(t, l.size(), 100)
}

func TestKeyedLimiterFullMapInsertIsCheap(t *testing.T) {
	// Before: every new key on a full map swept all entries under the mutex (~1-2 ms
	// per request at 100k entries). Now eviction is O(1) and idle sweeps are periodic.
	const entries, inserts = 50_000, 5_000
	c := &fakeClock{t: testNow}
	l := newKeyedLimiter(c.Now, entries)
	for i := range entries {
		l.allow(fmt.Sprint("fill-", i), 1, time.Hour)
	}
	require.Equal(t, entries, l.size())

	start := time.Now()
	for i := range inserts {
		l.allow(fmt.Sprint("new-", i), 1, time.Hour)
	}
	elapsed := time.Since(start)
	assert.Equal(t, entries, l.size())
	// A full sweep per insert would take seconds here; O(1) eviction takes a few ms
	// (even with -race). The bound is generous to stay stable on slow CI machines.
	assert.Less(t, elapsed, time.Second, "%d inserts into a full limiter took %s (%s each)", inserts, elapsed, elapsed/inserts)
}

func TestKeyedLimiterSweepIsPeriodic(t *testing.T) {
	c := &fakeClock{t: testNow}
	l := newKeyedLimiter(c.Now, 0)
	l.allow("old", 5, time.Minute)
	c.Advance(30 * time.Second)
	l.allow("new", 5, time.Minute)
	c.Advance(31 * time.Second) // old idle 61 s, new 31 s; first sweep is due
	l.allow("new", 5, time.Minute)
	assert.Equal(t, 1, l.size(), "idle bucket dropped, active one kept")

	// Right after a sweep nothing is looked at until limiterSweepEvery passed.
	l.allow("x", 5, time.Minute)
	c.Advance(limiterSweepEvery - time.Second)
	l.allow("new", 5, time.Minute)
	assert.Equal(t, 2, l.size(), "x is idle but the next sweep is not due")
	c.Advance(time.Second)
	l.allow("new", 5, time.Minute)
	assert.Equal(t, 1, l.size())
}

func TestKeyedLimiterBlocked(t *testing.T) {
	c := &fakeClock{t: testNow}
	l := newKeyedLimiter(c.Now, 0)
	blocked, _ := l.blocked("k")
	assert.False(t, blocked, "unknown key")
	assert.Zero(t, l.size(), "peeking creates no bucket")

	l.allow("k", 2, time.Minute)
	blocked, _ = l.blocked("k")
	assert.False(t, blocked, "one token left")
	l.allow("k", 2, time.Minute)
	blocked, wait := l.blocked("k")
	assert.True(t, blocked)
	assert.InDelta(t, 30*time.Second, wait, float64(time.Millisecond))
	blocked, _ = l.blocked("k")
	assert.True(t, blocked, "peeking takes no token (and refills nothing)")

	c.Advance(30 * time.Second)
	blocked, _ = l.blocked("k")
	assert.False(t, blocked)
}

func TestRateLimitUsesProxyHeadersOnlyFromTrustedProxies(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprint("trusted=", trusted), func(t *testing.T) {
			e := newEnv(t, func(_ *testEnv, d *Deps) {
				d.StatusRatePerMinute = 1
				if trusted {
					d.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")} // testRemoteAddr
				}
			})
			get := func(xff string) int {
				r := httptest.NewRequest(http.MethodGet, "/v1/domains/cars/jobs/"+ids.NewJob(), nil)
				r.Header.Set("X-Forwarded-For", xff)
				return e.do(r).Code
			}
			assert.Equal(t, http.StatusNotFound, get("203.0.113.1"))
			if trusted {
				assert.Equal(t, http.StatusNotFound, get("203.0.113.2"), "distinct clients behind the proxy")
				assert.Equal(t, http.StatusTooManyRequests, get("198.51.100.99, 203.0.113.1"),
					"a client-supplied leftmost entry does not buy a fresh bucket")
				assert.Equal(t, http.StatusTooManyRequests, get("10.9.9.1, 203.0.113.1, 192.0.2.77"),
					"trusted hops are skipped from the right")
			} else {
				assert.Equal(t, http.StatusTooManyRequests, get("203.0.113.2"), "spoofed header ignored")
			}
		})
	}
}

func TestUploadRateLimitSharesIPv6Slash64(t *testing.T) {
	e := newEnv(t, func(e *testEnv, _ *Deps) { e.cars.Limits.RatePerIPPerHour = 1 })
	upload := func(remote string) *httptest.ResponseRecorder {
		r := uploadRequest(t, "cars", fakeMP4(5000), "a.mp4")
		r.RemoteAddr = remote
		return e.do(r)
	}
	require.Equal(t, http.StatusAccepted, upload("[2001:db8:1:2::1]:5555").Code)
	for _, addr := range []string{"[2001:db8:1:2::2]:5555", "[2001:db8:1:2:ffff:ffff:ffff:ffff]:1"} {
		requireError(t, upload(addr), http.StatusTooManyRequests, CodeRateLimited)
	}
	assert.Equal(t, http.StatusAccepted, upload("[2001:db8:1:3::1]:5555").Code, "another /64 is another client")

	// IPv4 and its IPv4-mapped IPv6 form are the same client.
	require.Equal(t, http.StatusAccepted, upload("198.51.100.7:1000").Code)
	requireError(t, upload("[::ffff:198.51.100.7]:1000"), http.StatusTooManyRequests, CodeRateLimited)

	// The stored hash stays per address.
	created := e.store.createdJobs()
	require.Len(t, created, 3)
	assert.Equal(t, testIPHash("2001:db8:1:2::1"), created[0].ClientIPHash)
	assert.Equal(t, testIPHash("198.51.100.7"), created[2].ClientIPHash)
}

func TestRateKey(t *testing.T) {
	s := &Server{}
	for remote, want := range map[string]string{
		"192.0.2.1:1234":              "192.0.2.1",
		"[::ffff:192.0.2.1]:1":        "192.0.2.1",
		"[2001:db8:1:2:3:4:5:6]:443":  "2001:db8:1:2::/64",
		"[2001:db8:1:2::9%eth0]:443":  "2001:db8:1:2::/64",
		"[fe80::1]:1":                 "fe80::/64",
		"@unix":                       "@unix",
		"2001:db8:aaaa:bbbb:cccc::1":  "2001:db8:aaaa:bbbb::/64",
		"[2001:db8:aaaa:bbbc::1]:443": "2001:db8:aaaa:bbbc::/64",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		assert.Equal(t, want, s.rateKey(r), remote)
	}
}

func TestClientIP(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("fd00::/8")}
	manyHops := "198.51.100.1"
	for i := range 40 {
		manyHops += fmt.Sprintf(", 10.0.1.%d", i)
	}
	cases := []struct {
		name    string
		trusted []netip.Prefix
		remote  string
		xff     []string
		realIP  string
		want    string
	}{
		{name: "remote addr", remote: "192.0.2.1:1234", want: "192.0.2.1"},
		{name: "remote ipv6", remote: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "remote without port", remote: "192.0.2.1", want: "192.0.2.1"},
		{name: "mapped ipv4", remote: "[::ffff:192.0.2.1]:1", want: "192.0.2.1"},
		{name: "unparsable remote", remote: "@unix", want: "@unix"},
		{name: "no trusted proxies: xff ignored", remote: "10.0.0.2:1", xff: []string{"203.0.113.9"}, want: "10.0.0.2"},
		{name: "no trusted proxies: real ip ignored", remote: "10.0.0.2:1", realIP: "203.0.113.9", want: "10.0.0.2"},
		{name: "untrusted peer: xff ignored", trusted: trusted, remote: "192.0.2.1:1", xff: []string{"203.0.113.9"}, want: "192.0.2.1"},
		{name: "untrusted peer: real ip ignored", trusted: trusted, remote: "192.0.2.1:1", realIP: "203.0.113.9", want: "192.0.2.1"},
		{name: "trusted peer: single entry", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"203.0.113.9"}, want: "203.0.113.9"},
		{name: "spoofed leftmost entry ignored", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"198.51.100.1, 203.0.113.9"}, want: "203.0.113.9"},
		{name: "trusted hops skipped from the right", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"198.51.100.1, 203.0.113.9, 10.0.0.1"}, want: "203.0.113.9"},
		{name: "several header lines", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"198.51.100.1", "203.0.113.9, 10.0.0.1"}, want: "203.0.113.9"},
		{name: "all trusted: leftmost", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"10.0.0.5, 10.0.0.1"}, want: "10.0.0.5"},
		{name: "garbage stops at the last trusted hop", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"198.51.100.1, garbage, 10.0.0.1"}, want: "10.0.0.1"},
		{name: "garbage rightmost: the peer", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"198.51.100.1, unknown"}, want: "10.0.0.2"},
		{name: "empty entries skipped", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"198.51.100.1,, 203.0.113.9 ,"}, want: "203.0.113.9"},
		{name: "xff with port", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"203.0.113.9:5555"}, want: "203.0.113.9"},
		{name: "xff bracketed v6 with port", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"[2001:db8::7]:443"}, want: "2001:db8::7"},
		{name: "xff mapped v4", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"::ffff:203.0.113.9"}, want: "203.0.113.9"},
		{name: "trusted v6 peer", trusted: trusted, remote: "[fd00::1]:443", xff: []string{"2001:db8::9, fd12::3"}, want: "2001:db8::9"},
		{name: "trusted mapped peer", trusted: trusted, remote: "[::ffff:10.0.0.2]:1", xff: []string{"203.0.113.9"}, want: "203.0.113.9"},
		{name: "real ip when no xff", trusted: trusted, remote: "10.0.0.2:1", realIP: "198.51.100.3", want: "198.51.100.3"},
		{name: "real ip ignored when xff present", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"203.0.113.9"}, realIP: "198.51.100.3", want: "203.0.113.9"},
		{name: "blank xff counts as absent", trusted: trusted, remote: "10.0.0.2:1", xff: []string{"", " , "}, realIP: "198.51.100.3", want: "198.51.100.3"},
		{name: "nothing valid", trusted: trusted, remote: "10.0.0.2:1", xff: []string{""}, realIP: "x", want: "10.0.0.2"},
		{name: "hop limit", trusted: trusted, remote: "10.0.0.2:1", xff: []string{manyHops}, want: "10.0.1.8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{d: Deps{TrustedProxies: tc.trusted}, ipKey: []byte(testSalt)}
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if tc.realIP != "" {
				r.Header.Set("X-Real-IP", tc.realIP)
			}
			assert.Equal(t, tc.want, s.clientIP(r))
			assert.Equal(t, testIPHash(tc.want), s.ipHash(r))
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	got, err := ParseTrustedProxies([]string{" 10.42.0.0/16 ", "", "192.168.1.7", "fd00::/8", "2001:db8::1", "::ffff:172.16.0.0/108", "10.1.2.3/8"})
	require.NoError(t, err)
	assert.Equal(t, []netip.Prefix{
		netip.MustParsePrefix("10.42.0.0/16"),
		netip.MustParsePrefix("192.168.1.7/32"),
		netip.MustParsePrefix("fd00::/8"),
		netip.MustParsePrefix("2001:db8::1/128"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("10.0.0.0/8"),
	}, got)

	none, err := ParseTrustedProxies(nil)
	require.NoError(t, err)
	assert.Empty(t, none)

	for _, bad := range []string{"10.0.0.0/33", "not-an-ip", "10.0.0.1:80", "fe80::1%eth0/64"} {
		_, err := ParseTrustedProxies([]string{bad})
		assert.Error(t, err, bad)
	}
}

func TestIPHashIsKeyedHMAC(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.23:4000"
	hashWith := func(salt string) string { return New(Deps{Registry: domain.NewRegistry(), IPHashSalt: salt}).ipHash(r) }

	a1, a2, b := hashWith("a"), hashWith("a"), hashWith("b")
	assert.Equal(t, a1, a2, "stable for one key, across servers and restarts")
	assert.NotEqual(t, a1, b, "different keys give different hashes")
	assert.Len(t, a1, 64)
	m := hmac.New(sha256.New, []byte("a"))
	m.Write([]byte("198.51.100.23"))
	assert.Equal(t, hex.EncodeToString(m.Sum(nil)), a1, "HMAC-SHA256(key, ip)")
	plain := sha256.Sum256([]byte("a198.51.100.23"))
	assert.NotEqual(t, hex.EncodeToString(plain[:]), a1, "not the old salted plain SHA-256")
}

func TestIPHashWithoutSaltUsesRandomKey(t *testing.T) {
	var logs syncBuffer
	log := slog.New(slog.NewJSONHandler(&logs, nil))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	s1 := New(Deps{Registry: domain.NewRegistry(), Logger: log})
	s2 := New(Deps{Registry: domain.NewRegistry(), Logger: log})

	assert.Len(t, s1.ipKey, ipHashKeyBytes)
	assert.Equal(t, s1.ipHash(r), s1.ipHash(r), "stable within a process")
	assert.NotEqual(t, s1.ipHash(r), s2.ipHash(r), "a fresh random key per process")
	unkeyed := hmac.New(sha256.New, nil)
	unkeyed.Write([]byte(s1.clientIP(r)))
	assert.NotEqual(t, hex.EncodeToString(unkeyed.Sum(nil)), s1.ipHash(r), "never an empty key")
	assert.Contains(t, logs.String(), "V2I_IP_HASH_SALT is not set")
	assert.Equal(t, 2, strings.Count(logs.String(), `"level":"WARN"`))
}
