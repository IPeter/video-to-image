package httpapi

import (
	"container/list"
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	// defaultMaxLimiterEntries bounds each limiter's memory: at ~250 bytes per entry
	// this is ~25 MB in the worst case.
	defaultMaxLimiterEntries = 100_000
	// limiterSweepEvery is how often idle buckets are looked for.
	limiterSweepEvery = time.Minute

	// healthzPerMinute limits /healthz per client (each call pings Postgres and
	// storage); kubelet probes come a few times a minute.
	healthzPerMinute = 60
	// tokenFailuresPerMinute is how many wrong X-Internal-Token values one client may
	// send per minute (burst the same) before every token-bearing request is refused.
	tokenFailuresPerMinute = 10
)

// keyedLimiter is an in-memory set of token buckets keyed by domain + client. Each
// bucket holds n tokens and refills n per period, so a client can spend its whole
// allowance at once and then gets one more every period/n.
//
// Memory stays bounded two ways: a bucket unused for a whole period has refilled
// completely, so dropping it is indistinguishable from keeping it (idle eviction);
// and past maxEntries the least recently used bucket is dropped to make room, which
// only ever errs on the side of letting a request through (and never hits a client
// that is actively being throttled). Buckets live in an LRU list, so both evictions
// cost O(1) per bucket: idle buckets are collected from the cold end of the list once
// per limiterSweepEvery, never by scanning every entry. Limits are per API replica.
type keyedLimiter struct {
	now        func() time.Time
	maxEntries int

	mu        sync.Mutex
	entries   map[string]*list.Element // Value: *limiterEntry
	lru       *list.List               // front = most recently used
	nextSweep time.Time
}

type limiterEntry struct {
	key      string
	lim      *rate.Limiter
	lastSeen time.Time
	idle     time.Duration // unused this long → full again → safe to drop
}

func newKeyedLimiter(now func() time.Time, maxEntries int) *keyedLimiter {
	if now == nil {
		now = time.Now
	}
	if maxEntries <= 0 {
		maxEntries = defaultMaxLimiterEntries
	}
	return &keyedLimiter{now: now, maxEntries: maxEntries, entries: map[string]*list.Element{}, lru: list.New()}
}

// allow takes one token from key's bucket (n tokens per period, burst n). When the
// bucket is empty it returns false and how long until the next token. n <= 0 or
// period <= 0 disables the limit.
func (l *keyedLimiter) allow(key string, n int, period time.Duration) (bool, time.Duration) {
	if n <= 0 || period <= 0 {
		return true, 0
	}
	limit := rate.Limit(float64(n) / period.Seconds())
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()
	if !now.Before(l.nextSweep) {
		l.sweepLocked(now)
	}
	var e *limiterEntry
	if el, ok := l.entries[key]; ok {
		e = el.Value.(*limiterEntry)
		l.lru.MoveToFront(el)
		if e.lim.Limit() != limit || e.lim.Burst() != n {
			e.lim.SetLimitAt(now, limit)
			e.lim.SetBurstAt(now, n)
		}
	} else {
		for len(l.entries) >= l.maxEntries {
			l.removeLocked(l.lru.Back())
		}
		e = &limiterEntry{key: key, lim: rate.NewLimiter(limit, n)}
		l.entries[key] = l.lru.PushFront(e)
	}
	e.lastSeen = now
	e.idle = period

	if e.lim.AllowN(now, 1) {
		return true, 0
	}
	return false, waitForToken(e.lim, now)
}

// blocked reports, without taking a token, whether key's bucket is empty and how long
// until it holds one again. An unknown key is not blocked.
func (l *keyedLimiter) blocked(key string) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.entries[key]
	if !ok {
		return false, 0
	}
	lim := el.Value.(*limiterEntry).lim
	if lim.TokensAt(now) >= 1 {
		return false, 0
	}
	return true, waitForToken(lim, now)
}

// waitForToken is how long until lim holds one whole token (at least 1 ns).
func waitForToken(lim *rate.Limiter, now time.Time) time.Duration {
	missing := 1 - lim.TokensAt(now)
	wait := time.Duration(missing / float64(lim.Limit()) * float64(time.Second))
	return max(wait, time.Nanosecond)
}

// sweepLocked drops buckets that have been idle long enough to be full again, walking
// from the least recently used end and stopping at the first bucket still in use. The
// list is ordered by lastSeen, so with one period per limiter (as every caller uses)
// this removes exactly the idle buckets; with mixed periods a long-period bucket may
// shelter older short-period ones until a later sweep, which only costs memory.
func (l *keyedLimiter) sweepLocked(now time.Time) {
	for el := l.lru.Back(); el != nil; {
		e := el.Value.(*limiterEntry)
		if now.Sub(e.lastSeen) < e.idle {
			break
		}
		prev := el.Prev()
		l.removeLocked(el)
		el = prev
	}
	l.nextSweep = now.Add(limiterSweepEvery)
}

func (l *keyedLimiter) removeLocked(el *list.Element) {
	if el == nil {
		return
	}
	delete(l.entries, el.Value.(*limiterEntry).key)
	l.lru.Remove(el)
}

// size returns the number of tracked buckets (tests, metrics).
func (l *keyedLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// retryAfterSeconds renders a wait as a Retry-After value: whole seconds, rounded up,
// at least 1.
func retryAfterSeconds(d time.Duration) int {
	return max(1, int(math.Ceil(d.Seconds())))
}

// guardLimiters are the domain guard's two limiters.
type guardLimiters struct {
	uploads *keyedLimiter // POST jobs: limits.ratePerIpPerHour per hour
	status  *keyedLimiter // GET job / file routes: StatusRatePerMinute per minute
}

func newGuardLimiters(now func() time.Time) *guardLimiters {
	return &guardLimiters{
		uploads: newKeyedLimiter(now, defaultMaxLimiterEntries),
		status:  newKeyedLimiter(now, defaultMaxLimiterEntries),
	}
}
