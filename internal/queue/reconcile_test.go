package queue

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/store"
	"video-to-image/internal/testutil/pgtest"
)

func TestReconcileFailsOrphanedJobs(t *testing.T) {
	t.Parallel()
	domain := "rec-basic"
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		orphans: []store.JobRef{
			{ID: "job_a", DomainKey: domain, Status: store.StatusProcessing},
			{ID: "job_b", DomainKey: domain, Status: store.StatusProcessing}, // became terminal meanwhile
			{ID: "job_c", DomainKey: domain, Status: store.StatusQueued},     // FailJob errors
			{ID: "job_d", DomainKey: domain, Status: store.StatusProcessing}, // file deletion fails
			{ID: "job_e", Status: store.StatusProcessing},                    // incomplete ref
		},
		failErrs: map[string]error{
			"job_b": fmt.Errorf("store: fail job job_b: status completed: %w", store.ErrConflict),
			"job_c": errors.New("db timeout"),
		},
	}
	fs.addJob("job_a", domain, store.StatusProcessing)
	fs.addJob("job_d", domain, store.StatusProcessing)
	st := &fakeStorage{failOn: []string{"/job_d/"}}
	w := &ReconcileWorker{Store: fs, Storage: st, Grace: 20 * time.Minute, Logger: discardLogger(), Now: func() time.Time { return now }}
	before := finishedCounter(t, domain, store.StatusFailed)

	n, err := w.Reconcile(context.Background())

	assert.Equal(t, 2, n, "job_a and job_d were marked failed")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 marked failed, 3 errors")
	assert.Contains(t, err.Error(), "fail job job_c: db timeout")
	assert.Contains(t, err.Error(), "delete files of job job_d")
	assert.Contains(t, err.Error(), `job "job_e": incomplete job ref`)
	assert.NotContains(t, err.Error(), "job_b", "a job that is no longer active is skipped silently")

	fs.mu.Lock()
	assert.Equal(t, []listCall{{Now: now.Add(-20 * time.Minute), Limit: ReconcileBatchSize}}, fs.orphanCalls)
	fs.mu.Unlock()
	fails := fs.failCalls()
	require.Len(t, fails, 4, "the incomplete ref is never failed")
	for i, id := range []string{"job_a", "job_b", "job_c", "job_d"} {
		assert.Equal(t, failCall{
			ID: id, Code: "PROCESSING_ERROR", Message: "Something went wrong while processing the video.",
			InternalLog: "reconciler: River gave up on this job (no active pipeline job)",
		}, fails[i])
	}
	assert.Equal(t, store.StatusFailed, fs.jobStatus("job_a"))
	assert.Equal(t, store.StatusFailed, fs.jobStatus("job_d"))
	assert.Equal(t, []string{"job_a", "job_d"}, fs.skippedJobs(), "open steps are closed")
	assert.Equal(t, []string{domain + "/jobs/job_a/"}, st.deletedPrefixes(), "failed jobs keep no files")
	assert.Equal(t, 2.0, finishedCounter(t, domain, store.StatusFailed)-before)

	// The next run finds only the job whose FailJob failed.
	fs.mu.Lock()
	delete(fs.failErrs, "job_c")
	fs.orphans = fs.orphans[:4]
	fs.mu.Unlock()
	fs.addJob("job_b", domain, store.StatusCompleted)
	n, err = w.Reconcile(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{domain + "/jobs/job_a/", domain + "/jobs/job_c/"}, st.deletedPrefixes())
}

func TestReconcileDefaultsAndErrors(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	t.Run("default grace and nothing to do", func(t *testing.T) {
		t.Parallel()
		fs := &fakeStore{}
		w := &ReconcileWorker{Store: fs, Storage: &fakeStorage{}, Logger: discardLogger(), Now: func() time.Time { return now }}
		require.NoError(t, w.Work(context.Background(), nil))
		fs.mu.Lock()
		defer fs.mu.Unlock()
		assert.Equal(t, []listCall{{Now: now.Add(-15 * time.Minute), Limit: 100}}, fs.orphanCalls)
		assert.Empty(t, fs.fails)
	})
	t.Run("list error", func(t *testing.T) {
		t.Parallel()
		w := &ReconcileWorker{Store: &fakeStore{orphanErr: errInfra}, Storage: &fakeStorage{}, Logger: discardLogger()}
		n, err := w.Reconcile(context.Background())
		require.ErrorIs(t, err, errInfra)
		assert.Zero(t, n)
	})
	t.Run("cancelled run stops before failing jobs", func(t *testing.T) {
		t.Parallel()
		fs := &fakeStore{orphans: []store.JobRef{{ID: "job_x", DomainKey: "rec-cancel", Status: store.StatusProcessing}}}
		w := &ReconcileWorker{Store: fs, Storage: &fakeStorage{}, Logger: discardLogger()}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		n, err := w.Reconcile(ctx)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, n)
		assert.Empty(t, fs.failCalls())
	})
	t.Run("step and file errors are both reported", func(t *testing.T) {
		t.Parallel()
		fs := &fakeStore{
			orphans: []store.JobRef{{ID: "job_y", DomainKey: "rec-release", Status: store.StatusProcessing}},
			skipErr: errors.New("skip failed"),
		}
		st := &fakeStorage{}
		w := &ReconcileWorker{Store: fs, Storage: st, Logger: discardLogger()}
		n, err := w.Reconcile(context.Background())
		assert.Equal(t, 1, n)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "skip failed")
		assert.Equal(t, []string{"rec-release/jobs/job_y/"}, st.deletedPrefixes(), "a step error does not keep the files")
	})
}

func TestReconcileArgs(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "reconcile_orphans", ReconcileArgs{}.Kind())
	opts := ReconcileArgs{}.InsertOpts()
	assert.Equal(t, QueueMaintenance, opts.Queue)
	assert.Equal(t, 1, opts.MaxAttempts)
	assert.Equal(t, reconcileTimeout, (&ReconcileWorker{}).Timeout(nil))
}

// ---- integration: real store, real River tables ----

// skipUnlessOrphanQuery skips while store.ListOrphanedJobs is still the contract stub.
func skipUnlessOrphanQuery(t *testing.T, s *store.Store) {
	t.Helper()
	if _, err := s.ListOrphanedJobs(context.Background(), time.Now(), 1); err != nil {
		if strings.Contains(err.Error(), "not implemented yet") {
			t.Skipf("store.ListOrphanedJobs is not implemented yet: %v", err)
		}
		require.NoError(t, err)
	}
}

type orphanCase struct {
	id         string
	status     store.JobStatus // jobs row
	riverState string          // "" = no River pipeline job
	recent     bool            // updated within the grace period
	wantFailed bool
}

var orphanCases = []orphanCase{
	{id: "orph_missing_processing", status: store.StatusProcessing, wantFailed: true},
	{id: "orph_missing_queued", status: store.StatusQueued, wantFailed: true},
	{id: "orph_discarded", status: store.StatusProcessing, riverState: "discarded", wantFailed: true},
	{id: "orph_cancelled", status: store.StatusQueued, riverState: "cancelled", wantFailed: true},
	{id: "orph_river_completed", status: store.StatusProcessing, riverState: "completed", wantFailed: true},
	{id: "live_available", status: store.StatusQueued, riverState: "available"},
	{id: "live_running", status: store.StatusProcessing, riverState: "running"},
	{id: "live_retryable", status: store.StatusProcessing, riverState: "retryable"},
	{id: "live_scheduled", status: store.StatusQueued, riverState: "scheduled"},
	{id: "recent_missing", status: store.StatusProcessing, recent: true},
	{id: "done_failed", status: store.StatusFailed},
}

// seedOrphanCases creates the jobs of cases with their River pipeline jobs in the given
// states, backdating every job not marked recent past the grace period.
func seedOrphanCases(t *testing.T, pool *pgxpool.Pool, s *store.Store, domain string, cases []orphanCase) {
	t.Helper()
	ctx := context.Background()
	ins, err := NewInsertClient(pool, discardLogger())
	require.NoError(t, err)
	var old []string
	for _, c := range cases {
		var enqueue func(context.Context, pgx.Tx) error
		if c.riverState != "" {
			enqueue = func(ctx context.Context, tx pgx.Tx) error { return ins.EnqueuePipelineTx(ctx, tx, c.id) }
		}
		require.NoError(t, s.CreateJob(ctx, store.NewJob{
			ID: c.id, DomainKey: domain, ExpiresAt: time.Now().Add(24 * time.Hour),
			VideoPath: domain + "/jobs/" + c.id + "/original.mp4", ModelName: "test-model",
		}, enqueue))
		switch c.status {
		case store.StatusQueued:
		case store.StatusProcessing:
			require.NoError(t, s.StartProcessing(ctx, c.id))
			require.NoError(t, s.StartStep(ctx, c.id, store.StepNames[0]))
		case store.StatusFailed:
			require.NoError(t, s.FailJob(ctx, c.id, "PROCESSING_ERROR", "failed", ""))
		default:
			t.Fatalf("unsupported seed status %s", c.status)
		}
		if c.riverState != "" && c.riverState != "available" {
			setRiverState(t, pool, c.id, c.riverState)
		}
		if !c.recent {
			old = append(old, c.id)
		}
	}
	_, err = pool.Exec(ctx, `UPDATE jobs SET updated_at = now() - interval '1 hour' WHERE id = ANY($1)`, old)
	require.NoError(t, err)
}

// setRiverState moves the pipeline job of jobID to state as River itself would.
func setRiverState(t *testing.T, pool *pgxpool.Pool, jobID, state string) {
	t.Helper()
	var set string
	switch state {
	case "discarded", "cancelled", "completed":
		set = "finalized_at = now(), attempt = max_attempts, attempted_at = now() - interval '2 hours'"
	case "running":
		set = "attempt = 1, attempted_at = now()"
	case "retryable", "scheduled":
		set = "scheduled_at = now() + interval '1 hour'"
	default:
		t.Fatalf("unsupported river state %s", state)
	}
	tag, err := pool.Exec(context.Background(),
		`UPDATE river_job SET state = '`+state+`'::river_job_state, `+set+`
		 WHERE kind = 'pipeline' AND args->>'jobId' = $1`, jobID)
	require.NoError(t, err)
	require.Equal(t, int64(1), tag.RowsAffected(), "pipeline job of %s", jobID)
}

func TestReconcileWithStore(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	s := store.New(pool)
	skipUnlessOrphanQuery(t, s)
	domain := "rec-store"
	seedOrphanCases(t, pool, s, domain, orphanCases)

	st := &fakeStorage{}
	w := &ReconcileWorker{Store: s, Storage: st, Logger: discardLogger()}
	before := finishedCounter(t, domain, store.StatusFailed)

	n, err := w.Reconcile(ctx)
	require.NoError(t, err)

	var wantPrefixes []string
	for _, c := range orphanCases {
		j, err := s.GetJob(ctx, c.id)
		require.NoError(t, err)
		if !c.wantFailed {
			assert.Equal(t, c.status, j.Status, "%s is left alone", c.id)
			continue
		}
		wantPrefixes = append(wantPrefixes, domain+"/jobs/"+c.id+"/")
		assert.Equal(t, store.StatusFailed, j.Status, c.id)
		assert.Equal(t, "PROCESSING_ERROR", j.ErrorCode, c.id)
		assert.Equal(t, "Something went wrong while processing the video.", j.ErrorMessage, c.id)
		assert.Contains(t, j.InternalLog, "reconciler: River gave up on this job", c.id)
		assert.Empty(t, j.VideoPath, c.id)
		steps, err := s.ListSteps(ctx, c.id)
		require.NoError(t, err)
		for _, step := range steps {
			assert.NotContains(t, []store.StepStatus{store.StepPending, store.StepRunning}, step.Status,
				"%s: step %s left open", c.id, step.Name)
		}
	}
	assert.Equal(t, len(wantPrefixes), n)
	got := st.deletedPrefixes()
	slices.Sort(got)
	slices.Sort(wantPrefixes)
	assert.Equal(t, wantPrefixes, got)
	assert.Equal(t, float64(len(wantPrefixes)), finishedCounter(t, domain, store.StatusFailed)-before)

	n, err = w.Reconcile(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "a second run finds nothing")
}

// The reconciler is a periodic job that also runs on start, through a real River
// client and the real store.
func TestWorkerReconcilesOrphansOnStart(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	s := store.New(pool)
	skipUnlessOrphanQuery(t, s)
	domain := "rec-worker"
	cases := []orphanCase{
		{id: "orph_w_discarded", status: store.StatusProcessing, riverState: "discarded", wantFailed: true},
		{id: "orph_w_missing", status: store.StatusProcessing, wantFailed: true},
		{id: "live_w_available", status: store.StatusQueued, riverState: "available"},
	}
	seedOrphanCases(t, pool, s, domain, cases)

	// The available job is fetched and blocks in the runner (running: still active)
	// until the test ends.
	release := make(chan struct{})
	r := newFakeRunner(func(ctx context.Context, _ string) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	st := &fakeStorage{}
	cfg := testWorkerConfig(r, &fakeStore{})
	cfg.Store, cfg.Storage = s, st
	startWorker(t, pool, cfg)
	t.Cleanup(func() { close(release) }) // runs before the worker's Stop

	for _, c := range cases {
		if !c.wantFailed {
			continue
		}
		require.Eventually(t, func() bool {
			j, err := s.GetJob(ctx, c.id)
			return err == nil && j.Status == store.StatusFailed
		}, waitFor, 50*time.Millisecond, "%s is failed by the reconciler", c.id)
	}
	assert.Equal(t, "live_w_available", recv(t, r.calls))
	j, err := s.GetJob(ctx, "live_w_available")
	require.NoError(t, err)
	assert.Equal(t, store.StatusQueued, j.Status, "a job River still works is never reconciled")
	require.Eventually(t, func() bool { return len(st.deletedPrefixes()) == 2 }, waitFor, 20*time.Millisecond)
	got := st.deletedPrefixes()
	slices.Sort(got)
	assert.Equal(t, []string{domain + "/jobs/orph_w_discarded/", domain + "/jobs/orph_w_missing/"}, got)
}
