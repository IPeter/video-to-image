package store_test

import (
	"context"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

func usage(jobID, domain string, cost float64, at time.Time) store.UsageEvent {
	return store.UsageEvent{
		JobID: jobID, DomainKey: domain, Step: "analyze", Model: "gemini-3.5-flash-lite",
		InputTokens: 1000, OutputTokens: 200, ThinkingTokens: 50, VideoSeconds: 12.5,
		CostUSD: cost, LatencyMs: 1500, CreatedAt: at,
	}
}

func TestInsertUsageAccumulatesJobCost(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	other := processingJob(t, s)

	// Ten 0.1 events: float addition would give 0.9999999999999999.
	for range 10 {
		require.NoError(t, s.InsertUsage(ctx, usage(id, "cars", 0.1, time.Time{})))
	}
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, 1.0, j.CostUSD)

	// Sub-micro-dollar input is rounded to 6 decimals once, at insert, and the job is
	// credited with the rounded amount: 0.0012345 → 0.001235.
	require.NoError(t, s.InsertUsage(ctx, usage(id, "cars", 0.0012345, time.Time{})))
	require.NoError(t, s.InsertUsage(ctx, usage(id, "cars", 0.000001, time.Time{})))
	j, err = s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, 1.001236, j.CostUSD)

	var jobCost, eventSum string
	require.NoError(t, pool.QueryRow(ctx, `SELECT cost_usd::text FROM jobs WHERE id = $1`, id).Scan(&jobCost))
	require.NoError(t, pool.QueryRow(ctx, `SELECT sum(cost_usd)::text FROM usage_events WHERE job_id = $1`, id).Scan(&eventSum))
	assert.Equal(t, "1.001236", jobCost)
	assert.Equal(t, eventSum, jobCost, "jobs.cost_usd equals the sum of its events")

	oj, err := s.GetJob(ctx, other)
	require.NoError(t, err)
	assert.Zero(t, oj.CostUSD)

	// Stored fields.
	var e struct {
		in, out, think, lat int
		vs                  float64
		step, model, domain string
		createdRecent       bool
	}
	require.NoError(t, pool.QueryRow(ctx, `SELECT input_tokens, output_tokens, thinking_tokens, latency_ms, video_seconds::float8,
		step, model, domain_key, created_at > now() - interval '1 minute' FROM usage_events WHERE job_id = $1 LIMIT 1`, id).
		Scan(&e.in, &e.out, &e.think, &e.lat, &e.vs, &e.step, &e.model, &e.domain, &e.createdRecent))
	assert.Equal(t, 1000, e.in)
	assert.Equal(t, 200, e.out)
	assert.Equal(t, 50, e.think)
	assert.Equal(t, 1500, e.lat)
	assert.Equal(t, 12.5, e.vs)
	assert.Equal(t, "analyze", e.step)
	assert.Equal(t, "gemini-3.5-flash-lite", e.model)
	assert.Equal(t, "cars", e.domain)
	assert.True(t, e.createdRecent, "zero CreatedAt means now()")
}

func TestInsertUsageWithoutJob(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	require.NoError(t, s.InsertUsage(ctx, usage("", "cars", 0.02, time.Time{})))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM usage_events WHERE job_id IS NULL`))
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Zero(t, j.CostUSD)
	total, err := s.CostSince(ctx, time.Now().Add(-time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0.02, total)
}

func TestInsertUsageErrors(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	require.ErrorIs(t, s.InsertUsage(ctx, usage(ids.NewJob(), "cars", 0.01, time.Time{})), store.ErrNotFound)
	id := createJob(t, s)
	assert.Error(t, s.InsertUsage(ctx, usage(id, "cars", math.NaN(), time.Time{})))
	assert.Error(t, s.InsertUsage(ctx, usage(id, "cars", math.Inf(1), time.Time{})))
	assert.Error(t, s.InsertUsage(ctx, usage(id, "cars", 1e7, time.Time{})), "numeric(10,6) overflow")
	big := usage(id, "cars", 0.01, time.Time{})
	big.InputTokens = math.MaxInt32 + 1
	assert.Error(t, s.InsertUsage(ctx, big))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM usage_events`))
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Zero(t, j.CostUSD, "a failed insert credits nothing")
}

func TestInsertUsageConcurrent(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.InsertUsage(ctx, usage(id, "cars", 0.05, time.Time{}))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, 1.0, j.CostUSD)
}

func TestCostSince(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := createJob(t, s)
	t0 := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	require.NoError(t, s.InsertUsage(ctx, usage(id, "cars", 0.5, t0.Add(-2*time.Hour))))
	require.NoError(t, s.InsertUsage(ctx, usage(id, "cars", 0.25, t0.Add(-time.Hour))))
	require.NoError(t, s.InsertUsage(ctx, usage("", "realestate", 0.125, t0)))

	for _, tt := range []struct {
		since time.Time
		want  float64
	}{
		{t0.Add(-3 * time.Hour), 0.875},
		{t0.Add(-time.Hour), 0.375}, // inclusive
		{t0, 0.125},
		{t0.Add(time.Nanosecond * 1000), 0},
	} {
		got, err := s.CostSince(ctx, tt.since)
		require.NoError(t, err)
		assert.Equal(t, tt.want, got, "since %v", tt.since)
	}
}

func TestUsageReportGroupsByUTCDayAndDomain(t *testing.T) {
	t.Parallel()
	base, pool := newStore(t)
	ctx := context.Background()

	// Run the report on a session whose TimeZone is far from UTC: grouping must still
	// follow UTC days.
	cfg := pool.Config().Copy()
	cfg.ConnConfig.RuntimeParams["timezone"] = "Pacific/Auckland"
	akl, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(akl.Close)
	s := store.New(akl)

	jobA := createJob(t, base)
	jobB := createJob(t, base)
	jobC := createJob(t, base, withDomain("realestate"))

	day1 := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	ev := func(job, domain string, cost float64, at time.Time, in, out, think int, vs float64) store.UsageEvent {
		e := usage(job, domain, cost, at)
		e.InputTokens, e.OutputTokens, e.ThinkingTokens, e.VideoSeconds = in, out, think, vs
		return e
	}
	events := []store.UsageEvent{
		ev(jobA, "cars", 0.01, day1.Add(10*time.Minute), 100, 10, 1, 10.5),
		ev(jobA, "cars", 0.02, day1.Add(23*time.Hour+59*time.Minute+59*time.Second), 200, 20, 2, 20.25), // last second of day 1
		ev(jobB, "cars", 0.03, day1.Add(12*time.Hour), 300, 30, 3, 30),
		ev("", "cars", 0.001, day1.Add(13*time.Hour), 5, 0, 0, 0), // no job: a call, not a job
		ev(jobC, "realestate", 0.04, day1.Add(8*time.Hour), 400, 40, 4, 40),
		ev(jobA, "cars", 0.05, day2, 500, 50, 5, 50.5),                           // first instant of day 2
		ev(jobA, "cars", 0.5, day1.Add(-time.Second), 1, 1, 1, 1),                // before from
		ev(jobA, "cars", 0.7, day2.AddDate(0, 0, 1), 1, 1, 1, 1),                 // == to: excluded
		ev(jobC, "realestate", 0.06, day2.Add(22*time.Hour), 600, 60, 6, 60.125), // day 2 other domain
	}
	for _, e := range events {
		require.NoError(t, base.InsertUsage(ctx, e))
	}

	from, to := day1, day2.AddDate(0, 0, 1)
	rows, err := s.UsageReport(ctx, from, to, "")
	require.NoError(t, err)
	assert.Equal(t, []store.UsageRow{
		{Day: day1, DomainKey: "cars", Jobs: 2, Calls: 4, InputTokens: 605, OutputTokens: 60, ThinkingTokens: 6, VideoSeconds: 60.75, CostUSD: 0.061},
		{Day: day1, DomainKey: "realestate", Jobs: 1, Calls: 1, InputTokens: 400, OutputTokens: 40, ThinkingTokens: 4, VideoSeconds: 40, CostUSD: 0.04},
		{Day: day2, DomainKey: "cars", Jobs: 1, Calls: 1, InputTokens: 500, OutputTokens: 50, ThinkingTokens: 5, VideoSeconds: 50.5, CostUSD: 0.05},
		{Day: day2, DomainKey: "realestate", Jobs: 1, Calls: 1, InputTokens: 600, OutputTokens: 60, ThinkingTokens: 6, VideoSeconds: 60.125, CostUSD: 0.06},
	}, rows)
	for _, r := range rows {
		assert.Equal(t, time.UTC, r.Day.Location())
	}

	rows, err = s.UsageReport(ctx, from, to, "realestate")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, r := range rows {
		assert.Equal(t, "realestate", r.DomainKey)
	}

	rows, err = s.UsageReport(ctx, from, to, "nope")
	require.NoError(t, err)
	assert.NotNil(t, rows)
	assert.Empty(t, rows)

	rows, err = s.UsageReport(ctx, to, from, "")
	require.NoError(t, err)
	assert.Empty(t, rows, "empty range")
}

func TestJobUsageSumsPerStepAndModel(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	other := processingJob(t, s)

	t0 := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	ev := func(job, step, model string, at time.Duration, in, out, think int, cost float64) store.UsageEvent {
		return store.UsageEvent{
			JobID: job, DomainKey: "cars", Step: step, Model: model,
			InputTokens: in, OutputTokens: out, ThinkingTokens: think, VideoSeconds: 59,
			CostUSD: cost, LatencyMs: 900, CreatedAt: t0.Add(at),
		}
	}
	// Inserted out of time order: the result follows each group's first call, not the
	// insertion order and not the (step, model) sort order.
	for _, e := range []store.UsageEvent{
		ev(id, "review", "gemini-3.5-flash", 3*time.Second, 5000, 300, 700, 0.000417),
		ev(id, "analyze", "gemini-3.5-flash-lite", 0, 12000, 800, 1500, 0.0012345), // stored as 0.001235
		ev(id, "analyze", "gemini-2.5-pro", 9*time.Second, 100, 10, 0, 0.01),       // a later fallback model
		ev(id, "review", "gemini-3.5-flash", 2*time.Second, 4000, 250, 600, 0.000333),
		ev(id, "analyze", "gemini-3.5-flash-lite", time.Second, 11000, 700, 0, 0.002),
		ev(other, "analyze", "gemini-3.5-flash-lite", -time.Second, 1, 1, 1, 0.5), // another job
		ev("", "analyze", "gemini-3.5-flash-lite", -time.Second, 1, 1, 1, 0.25),   // no job
	} {
		require.NoError(t, s.InsertUsage(ctx, e))
	}

	got, err := s.JobUsage(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, []store.UsageByStep{
		{Step: "analyze", Model: "gemini-3.5-flash-lite", Calls: 2, InputTokens: 23000, OutputTokens: 1500, ThinkingTokens: 1500, CostUSD: 0.003235},
		{Step: "review", Model: "gemini-3.5-flash", Calls: 2, InputTokens: 9000, OutputTokens: 550, ThinkingTokens: 1300, CostUSD: 0.00075},
		{Step: "analyze", Model: "gemini-2.5-pro", Calls: 1, InputTokens: 100, OutputTokens: 10, ThinkingTokens: 0, CostUSD: 0.01},
	}, got)

	// The per-step costs add up to the job's running total.
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	var sum float64
	for _, u := range got {
		sum += u.CostUSD
	}
	assert.Equal(t, j.CostUSD, math.Round(sum*1e6)/1e6)
	assert.Equal(t, 0.013985, j.CostUSD)

	og, err := s.JobUsage(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, []store.UsageByStep{
		{Step: "analyze", Model: "gemini-3.5-flash-lite", Calls: 1, InputTokens: 1, OutputTokens: 1, ThinkingTokens: 1, CostUSD: 0.5},
	}, og)
}

func TestJobUsageEmptyAndMissingJob(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()

	got, err := s.JobUsage(ctx, createJob(t, s))
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)

	got, err = s.JobUsage(ctx, ids.NewJob())
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestJobUsageCanceledContext(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	id := createJob(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.JobUsage(ctx, id)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), "store: job usage "+id)
}
