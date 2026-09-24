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

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
	"video-to-image/internal/testutil/pgtest"
)

func TestStudioArgs(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "studio", StudioArgs{}.Kind())
	opts := StudioArgs{}.InsertOpts()
	assert.Equal(t, QueueStudio, opts.Queue)
	assert.Equal(t, 3, opts.MaxAttempts)
	assert.True(t, opts.UniqueOpts.ByArgs)
	assert.Equal(t, 10*time.Minute, (&StudioWorker{}).Timeout(studioJob("j", 1, 3)))
	assert.Greater(t, rescueStuckJobsAfter, StudioTimeout, "a running studio job is never rescued while it may still run")
}

func TestStudioRetryDelay(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 30*time.Second, studioRetryDelay(1, 0.5))
	assert.Equal(t, 2*time.Minute, studioRetryDelay(2, 0.5))
	assert.Equal(t, 2*time.Minute, studioRetryDelay(7, 0.5), "later attempts reuse the last delay")
	assert.Equal(t, 30*time.Second, studioRetryDelay(0, 0.5))
	assert.Equal(t, 27*time.Second, studioRetryDelay(1, 0))
	assert.InDelta(t, float64(33*time.Second), float64(studioRetryDelay(1, 0.9999999)), float64(time.Millisecond))
	next := (&StudioWorker{}).NextRetry(studioJob("j", 2, 3))
	assert.WithinDuration(t, time.Now().Add(2*time.Minute), next, 13*time.Second)
}

func TestStudioWorkerAttempts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		runErr      error
		failErr     error
		attempt     int
		maxAttempts int
		wantFail    bool // FailStudio called
		wantLog     bool // the failure is appended to internal_log
	}{
		{name: "success", attempt: 1, maxAttempts: 3},
		{name: "success on final attempt", attempt: 3, maxAttempts: 3},
		{name: "error is retried", runErr: errInfra, attempt: 1, maxAttempts: 3},
		{name: "error on second attempt is retried", runErr: errInfra, attempt: 2, maxAttempts: 3},
		{name: "final attempt fails the studio work", runErr: errInfra, attempt: 3, maxAttempts: 3, wantFail: true, wantLog: true},
		{
			name: "studio work already ended", runErr: errInfra, attempt: 3, maxAttempts: 3, wantFail: true,
			failErr: fmt.Errorf("store: fail studio j: status completed, studio done: %w", store.ErrConflict),
		},
		{name: "store failure keeps the runner's error", runErr: errInfra, attempt: 3, maxAttempts: 3, wantFail: true, failErr: errors.New("db down")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := &fakeStore{studioFailErr: tt.failErr}
			r := newFakeRunner(func(context.Context, string) error { return tt.runErr })
			w := &StudioWorker{Runner: r, Store: fs, Logger: discardLogger()}

			err := w.Work(context.Background(), studioJob("job_s", tt.attempt, tt.maxAttempts))

			assert.Equal(t, "job_s", <-r.calls)
			if tt.runErr != nil {
				require.ErrorIs(t, err, tt.runErr)
			} else {
				require.NoError(t, err)
			}
			if !tt.wantFail {
				assert.Empty(t, fs.studioFailCalls())
				assert.Empty(t, fs.failCalls(), "the job itself is never failed")
				return
			}
			assert.Equal(t, []failCall{{ID: "job_s", Message: StudioFailReason}}, fs.studioFailCalls())
			assert.Empty(t, fs.failCalls(), "the job itself is never failed")
			logs := fs.internalLogs()
			if !tt.wantLog {
				assert.Empty(t, logs)
				return
			}
			require.Len(t, logs, 1)
			assert.Equal(t, "job_s", logs[0].ID)
			assert.Contains(t, logs[0].InternalLog, "studio: river attempt 3/3 failed: "+errInfra.Error())
		})
	}
}

func TestStudioWorkerEdgeCases(t *testing.T) {
	t.Parallel()
	t.Run("empty job id is cancelled", func(t *testing.T) {
		t.Parallel()
		w := &StudioWorker{Runner: newFakeRunner(nil), Store: &fakeStore{}, Logger: discardLogger()}
		err := w.Work(context.Background(), studioJob("", 1, 3))
		var cancel *rivertype.JobCancelError
		require.ErrorAs(t, err, &cancel)
	})
	t.Run("panic is an error and fails the final attempt", func(t *testing.T) {
		t.Parallel()
		fs := &fakeStore{}
		r := newFakeRunner(func(context.Context, string) error { panic("boom") })
		w := &StudioWorker{Runner: r, Store: fs, Logger: discardLogger()}
		err := w.Work(context.Background(), studioJob("job_p", 3, 3))
		require.ErrorContains(t, err, "panic: boom")
		assert.Len(t, fs.studioFailCalls(), 1)
	})
	t.Run("shutdown re-delivers without failing", func(t *testing.T) {
		t.Parallel()
		fs := &fakeStore{}
		ctx, cancel := context.WithCancel(context.Background())
		r := newFakeRunner(func(context.Context, string) error {
			cancel()
			return errors.New("studio: sidecar call interrupted")
		})
		w := &StudioWorker{Runner: r, Store: fs, Logger: discardLogger()}
		err := w.Work(ctx, studioJob("job_i", 3, 3))
		require.ErrorIs(t, err, context.Canceled, "River must see the interruption")
		assert.Empty(t, fs.studioFailCalls(), "an interrupted final attempt is not a failure")
	})
	t.Run("timeout on the final attempt fails on a live context", func(t *testing.T) {
		t.Parallel()
		fs := &fakeStore{}
		r := newFakeRunner(func(ctx context.Context, _ string) error {
			<-ctx.Done()
			return fmt.Errorf("studio: mask: %w", ctx.Err())
		})
		w := &StudioWorker{Runner: r, Store: fs, Logger: discardLogger()}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		err := w.Work(ctx, studioJob("job_t", 3, 3))
		require.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Len(t, fs.studioFailCalls(), 1)
	})
}

func TestEnqueueStudio(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	c, err := NewInsertClient(pool, discardLogger())
	require.NoError(t, err)
	rows := func(jobID string) []riverJob {
		t.Helper()
		r, err := pool.Query(ctx, `SELECT state::text, attempt, max_attempts, queue FROM river_job
			WHERE kind = 'studio' AND args->>'jobId' = $1 ORDER BY id`, jobID)
		require.NoError(t, err)
		out, err := pgx.CollectRows(r, pgx.RowToStructByPos[riverJob])
		require.NoError(t, err)
		return out
	}

	require.NoError(t, pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return c.EnqueueStudioTx(ctx, tx, "job_st") }))
	assert.Equal(t, []riverJob{{State: "available", MaxAttempts: StudioMaxAttempts, Queue: QueueStudio}}, rows("job_st"))
	require.NoError(t, c.EnqueueStudio(ctx, "job_st"), "a duplicate is skipped")
	assert.Len(t, rows("job_st"), 1)

	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, c.EnqueueStudioTx(ctx, tx, "job_rb"))
	require.NoError(t, tx.Rollback(ctx))
	assert.Empty(t, rows("job_rb"))

	require.NoError(t, c.EnqueueStudio(ctx, "job_direct"))
	assert.Len(t, rows("job_direct"), 1)

	assert.Error(t, c.EnqueueStudio(ctx, ""))
	assert.Error(t, c.EnqueueStudioTx(ctx, nil, "job_x"))
	require.NoError(t, pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		assert.Error(t, c.EnqueueStudioTx(ctx, tx, ""))
		return nil
	}))
}

// The studio queue is worked only by a process with a StudioRunner.
func TestWorkerRunsStudioQueueOnlyWithStudioRunner(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	ins, err := NewInsertClient(pool, discardLogger())
	require.NoError(t, err)
	state := func(jobID string) string {
		var s string
		require.NoError(t, pool.QueryRow(ctx, `SELECT state::text FROM river_job WHERE kind = 'studio' AND args->>'jobId' = $1`, jobID).Scan(&s))
		return s
	}

	// Without a studio runner the job waits and the queue is not registered.
	startWorker(t, pool, testWorkerConfig(newFakeRunner(nil), &fakeStore{}))
	require.NoError(t, ins.EnqueueStudio(ctx, "job_wait"))
	time.Sleep(time.Second)
	assert.Equal(t, "available", state("job_wait"))
	var queues []string
	r, err := pool.Query(ctx, `SELECT name FROM river_queue ORDER BY name`)
	require.NoError(t, err)
	queues, err = pgx.CollectRows(r, pgx.RowTo[string])
	require.NoError(t, err)
	assert.NotContains(t, queues, QueueStudio)

	// A worker with a studio runner picks it up.
	studio := newFakeRunner(nil)
	pipeline := newFakeRunner(nil)
	cfg := testWorkerConfig(pipeline, &fakeStore{})
	cfg.StudioRunner = studio
	startWorker(t, pool, cfg)
	assert.Equal(t, "job_wait", recv(t, studio.calls))
	require.Eventually(t, func() bool { return state("job_wait") == "completed" }, waitFor, 20*time.Millisecond)
	assert.Empty(t, pipeline.calls, "studio jobs never reach the pipeline runner")

	q, err := ins.River().QueueGet(ctx, QueueStudio)
	require.NoError(t, err)
	assert.Equal(t, QueueStudio, q.Name)
}

func TestWorkerStudioFinalAttemptFailsStudioWork(t *testing.T) {
	pool := pgtest.New(t)
	fs := &fakeStore{}
	studio := newFakeRunner(func(context.Context, string) error { return errInfra })
	cfg := testWorkerConfig(newFakeRunner(nil), fs)
	cfg.StudioRunner = studio
	w := startWorker(t, pool, cfg)

	_, err := w.River().Insert(context.Background(), StudioArgs{JobID: "job_sf"}, &river.InsertOpts{MaxAttempts: 1})
	require.NoError(t, err)
	assert.Equal(t, "job_sf", recv(t, studio.calls))
	require.Eventually(t, func() bool { return len(fs.studioFailCalls()) == 1 }, waitFor, 20*time.Millisecond)
	assert.Equal(t, []failCall{{ID: "job_sf", Message: StudioFailReason}}, fs.studioFailCalls())
	assert.Empty(t, fs.failCalls())
}

func TestNewWorkerStudioWorkersValidation(t *testing.T) {
	cfg := testWorkerConfig(newFakeRunner(nil), &fakeStore{})
	cfg.StudioWorkers = -1
	_, err := NewWorker(&pgxpool.Pool{}, cfg) // never used: validation fails first
	require.Error(t, err)
	cfg.StudioWorkers = 0
	require.NoError(t, cfg.withDefaults())
	assert.Equal(t, DefaultStudioWorkers, cfg.StudioWorkers)
}

func TestReconcileFailsOrphanedStudioWork(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		studioByID: map[string]store.StudioStatus{
			"job_a": store.StudioRunning, "job_b": store.StudioPending, "job_c": store.StudioDone,
		},
		studioOrphans: []store.JobRef{
			{ID: "job_a", DomainKey: "rec-studio", Status: store.StatusCompleted, StudioStatus: store.StudioRunning},
			{ID: "job_b", DomainKey: "rec-studio", Status: store.StatusCompleted, StudioStatus: store.StudioPending},
			{ID: "job_gone", DomainKey: "rec-studio", Status: store.StatusCompleted, StudioStatus: store.StudioPending},
		},
	}
	st := &fakeStorage{}
	w := &ReconcileWorker{Store: fs, Storage: st, Grace: 20 * time.Minute, Logger: discardLogger(), Now: func() time.Time { return now }}

	n, err := w.Reconcile(context.Background())

	require.NoError(t, err, "a job deleted meanwhile is skipped silently")
	assert.Equal(t, 2, n)
	assert.Equal(t, []failCall{
		{ID: "job_a", Message: StudioFailReason}, {ID: "job_b", Message: StudioFailReason}, {ID: "job_gone", Message: StudioFailReason},
	}, fs.studioFailCalls())
	assert.Empty(t, fs.failCalls(), "the jobs themselves stay completed")
	assert.Empty(t, st.deletedPrefixes(), "their files are kept")
	fs.mu.Lock()
	assert.Equal(t, []listCall{{Now: now.Add(-20 * time.Minute), Limit: ReconcileBatchSize}}, fs.studioOrphanCall)
	fs.mu.Unlock()

	// Errors are reported without stopping the pipeline part.
	fs = &fakeStore{studioOrphanErr: errInfra}
	w.Store = fs
	_, err = w.Reconcile(context.Background())
	require.ErrorIs(t, err, errInfra)
	fs = &fakeStore{studioFailErr: errors.New("db timeout"), studioOrphans: []store.JobRef{{ID: "job_x", DomainKey: "d"}}}
	w.Store = fs
	n, err = w.Reconcile(context.Background())
	require.ErrorContains(t, err, "fail studio job_x: db timeout")
	assert.Zero(t, n)
}

// The real store: studio work whose River job is gone is failed, live work is kept.
func TestReconcileStudioWithStore(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	s := store.New(pool)
	ins, err := NewInsertClient(pool, discardLogger())
	require.NoError(t, err)
	complete := func(id string, enqueue bool) string {
		t.Helper()
		require.NoError(t, s.CreateJob(ctx, store.NewJob{ID: id, DomainKey: "cars", ExpiresAt: time.Now().Add(time.Hour)}, nil))
		require.NoError(t, s.StartProcessing(ctx, id))
		c := store.Completion{Studio: store.StudioPending, Images: []store.SelectedImage{{ID: ids.NewImage(), Idx: 1, ShotID: "front"}}}
		if enqueue {
			c.Enqueue = func(ctx context.Context, tx pgx.Tx) error { return ins.EnqueueStudioTx(ctx, tx, id) }
		}
		require.NoError(t, s.CompleteJob(ctx, id, c))
		return id
	}
	live := complete("job_live", true)
	orphan := complete("job_orphan", false)
	_, err = pool.Exec(ctx, `UPDATE jobs SET updated_at = now() - interval '1 hour'`)
	require.NoError(t, err)

	w := &ReconcileWorker{Store: s, Storage: &fakeStorage{}, Logger: discardLogger()}
	n, err := w.Reconcile(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	j, err := s.GetJob(ctx, orphan)
	require.NoError(t, err)
	assert.Equal(t, store.StudioFailed, j.StudioStatus)
	assert.Equal(t, store.StatusCompleted, j.Status)
	imgs, err := s.ListSelectedImages(ctx, orphan)
	require.NoError(t, err)
	require.Len(t, imgs, 1)
	assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioFailed, Reason: StudioFailReason}, imgs[0].Studio)
	j, err = s.GetJob(ctx, live)
	require.NoError(t, err)
	assert.Equal(t, store.StudioPending, j.StudioStatus, "a studio job River still has is left alone")
}

// A completed job whose studio work is pending or running can still receive studio
// uploads after the first sweep: it is swept again after ExpireJob.
func TestRetentionSweepsAgainForActiveStudioWork(t *testing.T) {
	t.Parallel()
	domain := "ret-studio"
	fs := &fakeStore{jobs: []store.JobRef{
		{ID: "job_run", DomainKey: domain, Status: store.StatusCompleted, StudioStatus: store.StudioRunning},
		{ID: "job_pend", DomainKey: domain, Status: store.StatusCompleted, StudioStatus: store.StudioPending},
		{ID: "job_done", DomainKey: domain, Status: store.StatusCompleted, StudioStatus: store.StudioDone},
		{ID: "job_none", DomainKey: domain, Status: store.StatusCompleted, StudioStatus: store.StudioNone},
	}}
	st := &fakeStorage{}
	prefix := domain + "/jobs/job_run/"
	st.put(prefix + "images/img_1.jpg")
	st.afterDelete = func(call int, p string) {
		if call == 1 && p == prefix {
			// The studio worker's uploads after the first sweep.
			st.put(prefix + "studio/grey/img_1.jpg")
			st.put(prefix + "studio/grey/thumbs/img_1.jpg")
			st.put(prefix + "studio.zip")
		}
	}
	w := &RetentionWorker{Store: fs, Storage: st, Logger: discardLogger()}

	n, err := w.ExpireDue(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, []string{
		domain + "/jobs/job_run/", domain + "/jobs/job_run/",
		domain + "/jobs/job_pend/", domain + "/jobs/job_pend/",
		domain + "/jobs/job_done/", domain + "/jobs/job_none/",
	}, st.deletedPrefixes())
	assert.Empty(t, st.objectKeys(), "studio files and studio.zip are under the job prefix and go with it")
}
