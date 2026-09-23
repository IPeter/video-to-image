package queue

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/metrics"
	"video-to-image/internal/store"
	"video-to-image/internal/testutil/pgtest"
)

// The tests in this file start River clients and touch the global gauges; they do
// not run in parallel.

const waitFor = 15 * time.Second

func testWorkerConfig(r Runner, fs *fakeStore) WorkerConfig {
	return WorkerConfig{
		Runner: r, Store: fs, Storage: &fakeStorage{}, Logger: discardLogger(),
		riverConfig: func(c *river.Config) {
			c.TestOnly = true // no staggered maintenance start
			c.FetchCooldown = 10 * time.Millisecond
			c.FetchPollInterval = 100 * time.Millisecond
		},
	}
}

func startWorker(t *testing.T, pool *pgxpool.Pool, cfg WorkerConfig) *Worker {
	t.Helper()
	w, err := NewWorker(pool, cfg)
	require.NoError(t, err)
	require.NoError(t, w.Start(context.Background()))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		assert.NoError(t, w.Stop(ctx))
	})
	return w
}

// enqueue runs EnqueuePipelineTx in its own committed transaction.
func enqueue(t *testing.T, pool *pgxpool.Pool, c *Client, jobID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return c.EnqueuePipelineTx(ctx, tx, jobID)
	}))
}

type riverJob struct {
	State       string
	Attempt     int
	MaxAttempts int
	Queue       string
}

// pipelineRows returns the River pipeline jobs for jobID.
func pipelineRows(t *testing.T, pool *pgxpool.Pool, jobID string) []riverJob {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT state::text, attempt, max_attempts, queue FROM river_job
		 WHERE kind = 'pipeline' AND args->>'jobId' = $1 ORDER BY id`, jobID)
	require.NoError(t, err)
	out, err := pgx.CollectRows(rows, pgx.RowToStructByPos[riverJob])
	require.NoError(t, err)
	return out
}

func waitState(t *testing.T, pool *pgxpool.Pool, jobID, state string) riverJob {
	t.Helper()
	var last []riverJob
	require.Eventually(t, func() bool {
		last = pipelineRows(t, pool, jobID)
		return len(last) == 1 && last[0].State == state
	}, waitFor, 20*time.Millisecond, "river job for %s never reached %s (last %+v)", jobID, state, last)
	return last[0]
}

func recv(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitFor):
		t.Fatal("timed out waiting for the runner")
		return ""
	}
}

func TestEnqueuePipelineTx(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	c, err := NewInsertClient(pool, discardLogger())
	require.NoError(t, err)

	t.Run("committed with the transaction", func(t *testing.T) {
		enqueue(t, pool, c, "job_commit")
		rows := pipelineRows(t, pool, "job_commit")
		require.Len(t, rows, 1)
		assert.Equal(t, riverJob{State: "available", Attempt: 0, MaxAttempts: PipelineMaxAttempts, Queue: QueuePipeline}, rows[0])
	})
	t.Run("rolled back with the transaction", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, c.EnqueuePipelineTx(ctx, tx, "job_rollback"))
		require.NoError(t, tx.Rollback(ctx))
		assert.Empty(t, pipelineRows(t, pool, "job_rollback"))
	})
	t.Run("duplicate is skipped", func(t *testing.T) {
		enqueue(t, pool, c, "job_dup")
		enqueue(t, pool, c, "job_dup")
		assert.Len(t, pipelineRows(t, pool, "job_dup"), 1)
	})
	t.Run("invalid input", func(t *testing.T) {
		require.NoError(t, pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			assert.Error(t, c.EnqueuePipelineTx(ctx, tx, ""))
			return nil
		}))
		assert.Error(t, c.EnqueuePipelineTx(ctx, nil, "job_x"))
	})
	t.Run("insert-only client cannot work jobs", func(t *testing.T) {
		assert.Error(t, c.River().Start(ctx))
	})
}

func TestNewInsertClientNilPool(t *testing.T) {
	_, err := NewInsertClient(nil, nil)
	require.Error(t, err)
}

func TestNewWorkerValidation(t *testing.T) {
	pool := &pgxpool.Pool{} // never used: validation fails first or the client is not started
	valid := func() WorkerConfig { return testWorkerConfig(newFakeRunner(nil), &fakeStore{}) }
	tests := []struct {
		name   string
		mutate func(*WorkerConfig)
	}{
		{"no runner", func(c *WorkerConfig) { c.Runner = nil }},
		{"no store", func(c *WorkerConfig) { c.Store = nil }},
		{"no storage", func(c *WorkerConfig) { c.Storage = nil }},
		{"negative workers", func(c *WorkerConfig) { c.PipelineWorkers = -1 }},
		{"negative interval", func(c *WorkerConfig) { c.StatsInterval = -time.Second }},
		{"negative orphan grace", func(c *WorkerConfig) { c.OrphanGrace = -time.Second }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid()
			tt.mutate(&cfg)
			_, err := NewWorker(pool, cfg)
			require.Error(t, err)
		})
	}
	_, err := NewWorker(nil, valid())
	require.Error(t, err)

	cfg := valid()
	require.NoError(t, cfg.withDefaults())
	assert.Equal(t, DefaultPipelineWorkers, cfg.PipelineWorkers)
	assert.Equal(t, 10*time.Minute, cfg.RetentionInterval)
	assert.Equal(t, 10*time.Minute, cfg.ReconcileInterval)
	assert.Equal(t, 15*time.Minute, cfg.OrphanGrace)
	assert.Equal(t, 5*time.Minute, cfg.CostGuardInterval)
	assert.Equal(t, 5*time.Minute, cfg.StatsInterval)
	assert.Equal(t, 24*time.Hour, cfg.SweepInterval)
	assert.Equal(t, 2*time.Hour, cfg.SweepOlderThan)
}

func TestWorkerWorksPipelineJobEnqueuedByInsertClient(t *testing.T) {
	pool := pgtest.New(t)
	fs := &fakeStore{}
	r := newFakeRunner(nil)
	startWorker(t, pool, testWorkerConfig(r, fs))

	ins, err := NewInsertClient(pool, discardLogger())
	require.NoError(t, err)
	enqueue(t, pool, ins, "job_e2e")

	assert.Equal(t, "job_e2e", recv(t, r.calls))
	got := waitState(t, pool, "job_e2e", "completed")
	assert.Equal(t, 1, got.Attempt)
	assert.Empty(t, fs.failCalls())
}

func TestWorkerClientEnqueuesThroughWorker(t *testing.T) {
	pool := pgtest.New(t)
	r := newFakeRunner(nil)
	w := startWorker(t, pool, testWorkerConfig(r, &fakeStore{}))

	enqueue(t, pool, w.Client(), "job_embed")

	assert.Equal(t, "job_embed", recv(t, r.calls))
	waitState(t, pool, "job_embed", "completed")
}

func TestWorkerFinalAttemptFailsJob(t *testing.T) {
	pool := pgtest.New(t)
	fs := &fakeStore{}
	fs.addJob("job_final", "wk-final", store.StatusProcessing)
	st := &fakeStorage{}
	r := newFakeRunner(func(context.Context, string) error { return errInfra })
	cfg := testWorkerConfig(r, fs)
	cfg.Storage = st
	w := startWorker(t, pool, cfg)

	// One attempt, so the first failure is final (River's retry backoff is too slow
	// for a test; the attempt logic itself is covered in pipeline_worker_test.go).
	_, err := w.River().Insert(context.Background(), PipelineArgs{JobID: "job_final"}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)

	assert.Equal(t, "job_final", recv(t, r.calls))
	got := waitState(t, pool, "job_final", "discarded")
	assert.Equal(t, 1, got.Attempt)
	fails := fs.failCalls()
	require.Len(t, fails, 1)
	assert.Equal(t, failCall{
		ID: "job_final", Code: "PROCESSING_ERROR", Message: "Something went wrong while processing the video.",
		InternalLog: "river attempt 1/1 failed: " + errInfra.Error(),
	}, fails[0])
	assert.Equal(t, []string{"job_final"}, fs.skippedJobs(), "the running step is closed")
	assert.Equal(t, []string{"wk-final/jobs/job_final/"}, st.deletedPrefixes(), "NewWorker gives the pipeline worker the storage")
}

func TestWorkerRetryableErrorIsRetriedAfterBackoff(t *testing.T) {
	pool := pgtest.New(t)
	fs := &fakeStore{}
	r := newFakeRunner(func(context.Context, string) error { return errInfra })
	w := startWorker(t, pool, testWorkerConfig(r, fs))

	t0 := time.Now()
	enqueue(t, pool, w.Client(), "job_retry")

	// PipelineWorker.NextRetry schedules the second attempt 30 s ± 10% later (River's
	// default would retry after about 1 s and use up the attempts in ~17 s).
	assert.Equal(t, "job_retry", recv(t, r.calls))
	got := waitState(t, pool, "job_retry", "retryable")
	assert.Equal(t, 1, got.Attempt)
	assert.Equal(t, PipelineMaxAttempts, got.MaxAttempts)
	assert.Equal(t, 5, got.MaxAttempts)
	var scheduledAt time.Time
	require.NoError(t, pool.QueryRow(context.Background(),
		`SELECT scheduled_at FROM river_job WHERE kind = 'pipeline' AND args->>'jobId' = $1`, "job_retry").Scan(&scheduledAt))
	delay := scheduledAt.Sub(t0)
	assert.Greater(t, delay, 26*time.Second, "retry delay %s", delay)
	assert.Less(t, delay, 35*time.Second, "retry delay %s", delay)

	time.Sleep(1500 * time.Millisecond)
	assert.Empty(t, r.calls, "no second attempt within River's default backoff")
	assert.Empty(t, fs.failCalls(), "a retryable failure does not touch the job row")
}

func TestWorkerRunsPeriodicMaintenanceOnStart(t *testing.T) {
	pool := pgtest.New(t)
	fs := &fakeStore{tableRows: map[string]int64{"jobs": 1}}
	sw := &fakeSweeper{}
	cfg := testWorkerConfig(newFakeRunner(nil), fs)
	cfg.Sweeper = sw
	startWorker(t, pool, cfg)

	require.Eventually(t, func() bool {
		return fs.listCallCount() > 0 && fs.orphanCallCount() > 0 && fs.costCalls() > 0 &&
			fs.statsCallCount() > 0 && sw.callCount() > 0
	}, waitFor, 20*time.Millisecond, "retention, reconciler, cost guard, stats and files sweep run on start")

	var kinds []string
	rows, err := pool.Query(context.Background(), `SELECT DISTINCT kind FROM river_job WHERE queue = $1 ORDER BY kind`, QueueMaintenance)
	require.NoError(t, err)
	kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
	require.NoError(t, err)
	assert.Equal(t, []string{"cost_guard", "model_files_sweep", "reconcile_orphans", "retention_cleanup", "stats"}, kinds)
}

func TestWorkerStopInterruptsRunningJob(t *testing.T) {
	pool := pgtest.New(t)
	fs := &fakeStore{}
	started := make(chan struct{}, 1)
	var runErr = make(chan error, 1)
	r := newFakeRunner(func(ctx context.Context, _ string) error {
		started <- struct{}{}
		<-ctx.Done()
		runErr <- ctx.Err()
		return fmt.Errorf("pipeline: extract: %w", ctx.Err())
	})
	w, err := NewWorker(pool, testWorkerConfig(r, fs))
	require.NoError(t, err)
	require.NoError(t, w.Start(context.Background()))

	// MaxAttempts 1: were the interruption counted as an attempt, the job would be
	// discarded and marked failed.
	_, err = w.River().Insert(context.Background(), PipelineArgs{JobID: "job_stop"}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(waitFor):
		t.Fatal("job never started")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	require.NoError(t, w.Stop(ctx))
	elapsed := time.Since(t0)

	assert.GreaterOrEqual(t, elapsed, 250*time.Millisecond, "running jobs get the graceful window first")
	assert.Less(t, elapsed, 5*time.Second, "Stop returns promptly once the window is over")
	assert.ErrorIs(t, <-runErr, context.Canceled, "the running job was cancelled")
	got := pipelineRows(t, pool, "job_stop")
	require.Len(t, got, 1)
	assert.Equal(t, riverJob{State: "available", Attempt: 0, MaxAttempts: 1, Queue: QueuePipeline}, got[0],
		"the interrupted job goes back to available without using up its attempt")
	assert.Empty(t, fs.failCalls())

	require.NoError(t, w.Stop(context.Background()), "a second Stop is a no-op")
}

func TestWorkerStopWaitsForRunningJob(t *testing.T) {
	pool := pgtest.New(t)
	release := make(chan struct{})
	r := newFakeRunner(func(ctx context.Context, _ string) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	w, err := NewWorker(pool, testWorkerConfig(r, &fakeStore{}))
	require.NoError(t, err)
	require.NoError(t, w.Start(context.Background()))
	enqueue(t, pool, w.Client(), "job_graceful")
	recv(t, r.calls)

	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NoError(t, w.Stop(ctx))

	got := pipelineRows(t, pool, "job_graceful")
	require.Len(t, got, 1)
	assert.Equal(t, "completed", got[0].State, "a graceful stop lets the running job finish")
}

func TestWorkerStopIdleAndUnstarted(t *testing.T) {
	pool := pgtest.New(t)

	unstarted, err := NewWorker(pool, testWorkerConfig(newFakeRunner(nil), &fakeStore{}))
	require.NoError(t, err)
	require.NoError(t, unstarted.Stop(context.Background()))

	w, err := NewWorker(pool, testWorkerConfig(newFakeRunner(nil), &fakeStore{}))
	require.NoError(t, err)
	require.NoError(t, w.Start(context.Background()))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	require.NoError(t, w.Stop(ctx))
	assert.Less(t, time.Since(t0), 3*time.Second)
}

func TestCostGuardPausesAndResumesPipelineQueue(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	fs := &fakeStore{cost: 5}
	r := newFakeRunner(nil)
	cfg := testWorkerConfig(r, fs)
	cfg.DailyCostCeilingUSD = 1
	w := startWorker(t, pool, cfg)

	// The cost guard runs on start and pauses the queue for every worker process.
	require.Eventually(t, func() bool {
		q, err := w.River().QueueGet(ctx, QueuePipeline)
		return err == nil && q.PausedAt != nil
	}, waitFor, 20*time.Millisecond, "pipeline queue paused")
	assert.Equal(t, 1.0, metricValue(t, metrics.QueuePaused.WithLabelValues(QueuePipeline)))
	assert.Equal(t, 5.0, metricValue(t, metrics.DailyCostUSD))

	enqueue(t, pool, w.Client(), "job_paused")
	time.Sleep(time.Second)
	got := pipelineRows(t, pool, "job_paused")
	require.Len(t, got, 1)
	// Not fetched at all: the pipeline worker's own check would have snoozed it
	// (state scheduled) had the paused queue handed it out.
	assert.Equal(t, "available", got[0].State)
	assert.Empty(t, r.calls)

	// Spend drops below the ceiling (e.g. a new UTC day): the next pass resumes.
	fs.setCost(0.5)
	guard := &CostGuardWorker{Store: fs, Queues: w.River(), DailyCostCeilingUSD: 1, Logger: discardLogger()}
	require.NoError(t, guard.Check(ctx))
	q, err := w.River().QueueGet(ctx, QueuePipeline)
	require.NoError(t, err)
	assert.Nil(t, q.PausedAt)
	assert.Equal(t, 0.0, metricValue(t, metrics.QueuePaused.WithLabelValues(QueuePipeline)))

	assert.Equal(t, "job_paused", recv(t, r.calls))
	waitState(t, pool, "job_paused", "completed")

	// Idempotent both ways against the real client.
	require.NoError(t, guard.Check(ctx))
	fs.setCost(3)
	require.NoError(t, guard.Check(ctx))
	require.NoError(t, guard.Check(ctx))
	q, err = w.River().QueueGet(ctx, QueuePipeline)
	require.NoError(t, err)
	assert.NotNil(t, q.PausedAt)
	guard.DailyCostCeilingUSD = 0 // disabling the guard resumes the queue
	require.NoError(t, guard.Check(ctx))
	q, err = w.River().QueueGet(ctx, QueuePipeline)
	require.NoError(t, err)
	assert.Nil(t, q.PausedAt)
}

func TestCostGuardUnregisteredQueue(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	c, err := NewInsertClient(pool, discardLogger())
	require.NoError(t, err)
	_, err = c.River().QueueGet(ctx, QueuePipeline)
	require.ErrorIs(t, err, rivertype.ErrNotFound, "no worker has registered the queue yet")

	guard := &CostGuardWorker{Store: &fakeStore{cost: 9}, Queues: c.River(), DailyCostCeilingUSD: 1, Logger: discardLogger()}
	err = guard.Check(ctx)
	require.ErrorIs(t, err, rivertype.ErrNotFound)

	guard.DailyCostCeilingUSD = 100
	require.NoError(t, guard.Check(ctx), "nothing to resume")
}

func TestStatsWorkerSetsGauges(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	c, err := NewInsertClient(pool, discardLogger())
	require.NoError(t, err)
	enqueue(t, pool, c, "job_s1")
	enqueue(t, pool, c, "job_s2")
	_, err = c.River().Insert(ctx, StatsArgs{}, nil)
	require.NoError(t, err)
	_, err = c.River().Insert(ctx, RetentionArgs{}, &river.InsertOpts{ScheduledAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)

	metrics.QueueJobs.WithLabelValues("gone", "available").Set(7) // stale series
	fs := &fakeStore{
		tableRows: map[string]int64{"jobs": 12, "candidate_frames": 3400},
	}
	w := &StatsWorker{Store: fs, DB: pool, Logger: discardLogger()}
	require.NoError(t, w.Work(ctx, nil))

	assert.Equal(t, 12.0, metricValue(t, metrics.TableRows.WithLabelValues("jobs")))
	assert.Equal(t, 3400.0, metricValue(t, metrics.TableRows.WithLabelValues("candidate_frames")))
	assert.Equal(t, 2.0, metricValue(t, metrics.QueueJobs.WithLabelValues(QueuePipeline, "available")))
	assert.Equal(t, 1.0, metricValue(t, metrics.QueueJobs.WithLabelValues(QueueMaintenance, "available")))
	assert.Equal(t, 1.0, metricValue(t, metrics.QueueJobs.WithLabelValues(QueueMaintenance, "scheduled")))
	assert.Equal(t, 0.0, metricValue(t, metrics.QueueJobs.WithLabelValues(QueuePipeline, "running")))
	assert.Equal(t, 2*len(rivertype.JobStates()), seriesCount(metrics.QueueJobs),
		"every queue × state has a series and stale queues are dropped")
}

func TestStatsWorkerQueryFailureStillSetsTableRows(t *testing.T) {
	fs := &fakeStore{tableRows: map[string]int64{"usage_events": 77}}
	w := &StatsWorker{Store: fs, DB: failingQuerier{}, Logger: discardLogger()}

	err := w.Refresh(context.Background())

	require.Error(t, err)
	assert.False(t, errors.Is(err, errInfra))
	assert.Equal(t, 77.0, metricValue(t, metrics.TableRows.WithLabelValues("usage_events")))
}
