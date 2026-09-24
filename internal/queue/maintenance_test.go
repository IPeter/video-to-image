package queue

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/metrics"
	"video-to-image/internal/store"
)

// expiredRefs returns n refs in domain, ids "<prefix>-000".
func expiredRefs(domain, prefix string, n int) []store.JobRef {
	out := make([]store.JobRef, n)
	for i := range out {
		out[i] = store.JobRef{ID: fmt.Sprintf("%s-%03d", prefix, i), DomainKey: domain, Status: store.StatusCompleted}
	}
	return out
}

func expiredCounter(t *testing.T, domain string) float64 {
	t.Helper()
	return metricValue(t, metrics.JobsFinished.WithLabelValues(domain, "expired"))
}

func TestRetentionExpiresJobsAndDeletesFiles(t *testing.T) {
	t.Parallel()
	domain := "ret-basic"
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	fs := &fakeStore{jobs: []store.JobRef{
		{ID: "job_a", DomainKey: domain, Status: store.StatusCompleted},
		{ID: "job_b", DomainKey: domain, Status: store.StatusRejected},
		{ID: "job_c", DomainKey: domain, Status: store.StatusFailed},
	}}
	st := &fakeStorage{failOn: []string{"/job_b/"}}
	w := &RetentionWorker{Store: fs, Storage: st, Logger: discardLogger(), Now: func() time.Time { return now }}
	before := expiredCounter(t, domain)

	n, err := w.ExpireDue(context.Background())

	require.Error(t, err, "the failure is reported so River records it")
	assert.Contains(t, err.Error(), "job_b")
	assert.Contains(t, err.Error(), "2 expired, 1 failed")
	assert.Equal(t, 2, n)
	assert.Equal(t, []string{domain + "/jobs/job_a/", domain + "/jobs/job_c/"}, st.deletedPrefixes(),
		"a failing job does not stop the jobs after it")
	fs.mu.Lock()
	assert.Equal(t, []string{"job_a", "job_c"}, fs.expireIDs, "files must be deleted before the job is expired")
	require.NotEmpty(t, fs.listCalls)
	assert.Equal(t, listCall{Now: now, Limit: RetentionBatchSize}, fs.listCalls[0])
	fs.mu.Unlock()
	assert.Equal(t, 2.0, expiredCounter(t, domain)-before)
}

func TestRetentionNothingToDo(t *testing.T) {
	t.Parallel()
	fs := &fakeStore{}
	w := &RetentionWorker{Store: fs, Storage: &fakeStorage{}, Logger: discardLogger()}

	require.NoError(t, w.Work(context.Background(), nil))
	assert.Equal(t, 1, fs.listCallCount())
}

func TestRetentionPagesThroughAllExpiredJobs(t *testing.T) {
	t.Parallel()
	domain := "ret-pages"
	fs := &fakeStore{jobs: expiredRefs(domain, "job", 250)}
	st := &fakeStorage{}
	w := &RetentionWorker{Store: fs, Storage: st, Logger: discardLogger()}
	before := expiredCounter(t, domain)

	n, err := w.ExpireDue(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 250, n)
	assert.Len(t, st.deletedPrefixes(), 250)
	assert.Equal(t, 3, fs.listCallCount(), "100 + 100 + 50: the short batch ends the run")
	assert.Equal(t, 250.0, expiredCounter(t, domain)-before)
}

func TestRetentionFailuresDoNotHideLaterJobs(t *testing.T) {
	t.Parallel()
	domain := "ret-fail"
	// The 120 oldest jobs fail every time; without a growing limit they would fill
	// every batch and the 30 after them would never be reached.
	jobs := append(expiredRefs(domain, "bad", 120), expiredRefs(domain, "good", 30)...)
	fs := &fakeStore{jobs: jobs}
	st := &fakeStorage{failOn: []string{"/bad-"}}
	w := &RetentionWorker{Store: fs, Storage: st, Logger: discardLogger()}

	n, err := w.ExpireDue(context.Background())

	require.Error(t, err)
	assert.Equal(t, 30, n)
	assert.Contains(t, err.Error(), "30 expired, 120 failed")
	assert.Contains(t, err.Error(), "and 110 more errors", "the error is capped")
	fs.mu.Lock()
	defer fs.mu.Unlock()
	assert.Len(t, fs.expireIDs, 30)
	assert.Equal(t, []int{100, 200}, []int{fs.listCalls[0].Limit, fs.listCalls[1].Limit})
	assert.LessOrEqual(t, len(fs.listCalls), 3)
}

func TestRetentionStopsAtBatchCap(t *testing.T) {
	t.Parallel()
	fs := &fakeStore{jobs: expiredRefs("ret-cap", "job", RetentionBatchSize*RetentionMaxBatches+10)}
	w := &RetentionWorker{Store: fs, Storage: &fakeStorage{}, Logger: discardLogger()}

	n, err := w.ExpireDue(context.Background())

	require.NoError(t, err)
	assert.Equal(t, RetentionBatchSize*RetentionMaxBatches, n, "the rest waits for the next run")
	assert.Equal(t, RetentionMaxBatches, fs.listCallCount())

	n, err = w.ExpireDue(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 10, n)
}

func TestRetentionExpireJobErrorLeavesOthers(t *testing.T) {
	t.Parallel()
	fs := &fakeStore{
		jobs:       expiredRefs("ret-exp", "job", 3),
		expireErrs: map[string]error{"job-001": errors.New("db timeout")},
	}
	w := &RetentionWorker{Store: fs, Storage: &fakeStorage{}, Logger: discardLogger()}

	n, err := w.ExpireDue(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "expire job job-001: db timeout")
	assert.Equal(t, 2, n)
}

func TestRetentionListErrorAndCancellation(t *testing.T) {
	t.Parallel()
	t.Run("list error", func(t *testing.T) {
		t.Parallel()
		fs := &fakeStore{listErr: errInfra}
		w := &RetentionWorker{Store: fs, Storage: &fakeStorage{}, Logger: discardLogger()}
		_, err := w.ExpireDue(context.Background())
		require.ErrorIs(t, err, errInfra)
	})
	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		fs := &fakeStore{jobs: expiredRefs("ret-cancel", "job", 5)}
		w := &RetentionWorker{Store: fs, Storage: &fakeStorage{}, Logger: discardLogger()}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		n, err := w.ExpireDue(ctx)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, n)
		assert.Zero(t, fs.listCallCount())
	})
}

func TestRetentionRejectsIncompleteRef(t *testing.T) {
	t.Parallel()
	fs := &fakeStore{jobs: []store.JobRef{
		{ID: "job_x", Status: store.StatusCompleted},
		{ID: "job_y", DomainKey: "ret-inc", Status: store.StatusCompleted},
	}}
	st := &fakeStorage{}
	w := &RetentionWorker{Store: fs, Storage: st, Logger: discardLogger()}

	n, err := w.ExpireDue(context.Background())

	require.Error(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"ret-inc/jobs/job_y/"}, st.deletedPrefixes(), "no prefix is derived from an empty domain")
}

func TestFilesSweepWorker(t *testing.T) {
	t.Parallel()
	t.Run("nil sweeper is a no-op", func(t *testing.T) {
		t.Parallel()
		w := &FilesSweepWorker{Logger: discardLogger()}
		require.NoError(t, w.Work(context.Background(), nil))
	})
	t.Run("sweeps with the configured age", func(t *testing.T) {
		t.Parallel()
		s := &fakeSweeper{deleted: 4}
		w := &FilesSweepWorker{Sweeper: s, OlderThan: 2 * time.Hour, Logger: discardLogger()}
		require.NoError(t, w.Work(context.Background(), nil))
		assert.Equal(t, 1, s.calls)
		assert.Equal(t, 2*time.Hour, s.olderThan)
	})
	t.Run("error is returned", func(t *testing.T) {
		t.Parallel()
		s := &fakeSweeper{deleted: 1, err: errInfra}
		w := &FilesSweepWorker{Sweeper: s, OlderThan: time.Hour, Logger: discardLogger()}
		err := w.Work(context.Background(), nil)
		require.ErrorIs(t, err, errInfra)
		assert.Contains(t, err.Error(), "deleted 1")
	})
}

// Cost guard tests share the global DailyCostUSD / QueuePaused gauges: not parallel.

func TestCostGuardWithFakeQueues(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC) }
	tests := []struct {
		name        string
		ceiling     float64
		cost        float64
		q           fakeQueues
		wantPaused  bool
		wantPauses  int
		wantResumes int
		wantGauge   float64
		wantErr     bool
	}{
		{name: "above ceiling pauses", ceiling: 10, cost: 12, wantPaused: true, wantPauses: 1, wantGauge: 1},
		{name: "at ceiling pauses", ceiling: 10, cost: 10, wantPaused: true, wantPauses: 1, wantGauge: 1},
		{name: "already paused stays paused", ceiling: 10, cost: 11, q: fakeQueues{paused: true}, wantPaused: true, wantGauge: 1},
		{name: "below ceiling resumes", ceiling: 10, cost: 3, q: fakeQueues{paused: true}, wantResumes: 1},
		{name: "below ceiling and running is a no-op", ceiling: 10, cost: 3},
		{name: "disabled ceiling resumes", ceiling: 0, cost: 100, q: fakeQueues{paused: true}, wantResumes: 1},
		{name: "disabled ceiling and running is a no-op", ceiling: 0, cost: 100},
		{name: "unregistered queue is not paused", ceiling: 10, cost: 3, q: fakeQueues{missing: true}},
		{name: "unregistered queue cannot be paused", ceiling: 10, cost: 30, q: fakeQueues{missing: true}, wantErr: true, wantGauge: -1},
		{name: "queue lookup error", ceiling: 10, cost: 3, q: fakeQueues{getErr: errInfra}, wantErr: true, wantGauge: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metrics.QueuePaused.WithLabelValues(QueuePipeline).Set(-1)
			q := tt.q
			fs := &fakeStore{cost: tt.cost}
			// No studio worker registered the studio queue: it is left alone.
			w := &CostGuardWorker{Store: fs, Queues: fakeQueueSet{QueuePipeline: &q}, DailyCostCeilingUSD: tt.ceiling, Logger: discardLogger(), Now: now}

			err := w.Work(context.Background(), nil)

			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantPaused, q.paused)
			assert.Equal(t, tt.wantPauses, q.pauses)
			assert.Equal(t, tt.wantResumes, q.resumes)
			assert.Equal(t, tt.wantGauge, metricValue(t, metrics.QueuePaused.WithLabelValues(QueuePipeline)))
			assert.Equal(t, tt.cost, metricValue(t, metrics.DailyCostUSD))
			fs.mu.Lock()
			assert.Equal(t, []time.Time{time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)}, fs.costSince)
			fs.mu.Unlock()
		})
	}
}

func TestCostGuardPausesAndResumesStudioQueue(t *testing.T) {
	pipeline, studio := &fakeQueues{}, &fakeQueues{}
	fs := &fakeStore{cost: 12}
	w := &CostGuardWorker{Store: fs, Queues: fakeQueueSet{QueuePipeline: pipeline, QueueStudio: studio}, DailyCostCeilingUSD: 10, Logger: discardLogger()}

	require.NoError(t, w.Check(context.Background()))
	assert.True(t, pipeline.paused)
	assert.True(t, studio.paused, "studio QA calls count against the same ceiling")
	assert.Equal(t, 1.0, metricValue(t, metrics.QueuePaused.WithLabelValues(QueueStudio)))

	fs.setCost(2)
	require.NoError(t, w.Check(context.Background()))
	assert.False(t, pipeline.paused)
	assert.False(t, studio.paused)
	assert.Equal(t, 1, studio.resumes)
	assert.Equal(t, 0.0, metricValue(t, metrics.QueuePaused.WithLabelValues(QueueStudio)))

	// A studio lookup error is reported, but the pipeline queue is still handled.
	fs.setCost(20)
	studio.getErr = errInfra
	err := w.Check(context.Background())
	require.ErrorIs(t, err, errInfra)
	assert.True(t, pipeline.paused)

	// A pipeline error does not keep the studio queue running either.
	pipeline.paused, pipeline.getErr, studio.getErr = false, errInfra, nil
	require.ErrorIs(t, w.Check(context.Background()), errInfra)
	assert.True(t, studio.paused)
}

func TestCostGuardCostError(t *testing.T) {
	q := &fakeQueues{paused: true}
	w := &CostGuardWorker{Store: &fakeStore{costErr: errInfra}, Queues: fakeQueueSet{QueuePipeline: q}, DailyCostCeilingUSD: 1, Logger: discardLogger()}

	require.ErrorIs(t, w.Work(context.Background(), nil), errInfra)
	assert.True(t, q.paused, "the queue state is left alone when the cost is unknown")
	assert.Zero(t, q.resumes)
}

func TestStatsWorkerStoreErrorsAreReported(t *testing.T) {
	fs := &fakeStore{tableErr: errInfra, countErr: errors.New("count failed")}
	w := &StatsWorker{Store: fs, DB: failingQuerier{}, Logger: discardLogger()}

	err := w.Refresh(context.Background())

	require.ErrorIs(t, err, errInfra)
	assert.Contains(t, err.Error(), "count failed")
	assert.Contains(t, err.Error(), "queue depth")
}

// A worker still processing the job can upload after the first sweep listed the prefix
// and complete the job before ExpireJob: the second sweep after ExpireJob removes what
// it uploaded (review finding delete-vs-complete-race-orphans-files).
func TestRetentionSweepsAgainAfterExpiringActiveJob(t *testing.T) {
	t.Parallel()
	domain := "ret-race"
	prefix := domain + "/jobs/job_run/"
	fs := &fakeStore{jobs: []store.JobRef{{ID: "job_run", DomainKey: domain, Status: store.StatusProcessing}}}
	st := &fakeStorage{}
	st.put(prefix + "original.mp4")
	st.put(prefix + "images/img_old.jpg")
	var events []string
	st.afterDelete = func(call int, _ string) {
		fs.mu.Lock()
		expired := fs.expired["job_run"]
		fs.mu.Unlock()
		events = append(events, fmt.Sprintf("delete %d (expired=%v)", call, expired))
		if call == 1 {
			// The worker's last uploads, then its CompleteJob commits.
			st.put(prefix + "images/img_new.jpg")
			st.put(prefix + "thumbs/img_new.jpg")
			st.put(prefix + "manifest.json")
			st.put(prefix + "export.zip")
		}
	}
	w := &RetentionWorker{Store: fs, Storage: st, Logger: discardLogger()}

	n, err := w.ExpireDue(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{"delete 1 (expired=false)", "delete 2 (expired=true)"}, events,
		"files are deleted, the job expired, then its files swept again")
	assert.Empty(t, st.objectKeys(), "nothing the worker uploaded survives")
}

func TestRetentionSingleSweepForJobsAlreadyTerminal(t *testing.T) {
	t.Parallel()
	domain := "ret-terminal"
	var jobs []store.JobRef
	for _, s := range []store.JobStatus{store.StatusCompleted, store.StatusRejected, store.StatusFailed} {
		jobs = append(jobs, store.JobRef{ID: "job_" + string(s), DomainKey: domain, Status: s})
	}
	jobs = append(jobs, store.JobRef{ID: "job_queued", DomainKey: domain, Status: store.StatusQueued})
	st := &fakeStorage{}
	w := &RetentionWorker{Store: &fakeStore{jobs: jobs}, Storage: st, Logger: discardLogger()}

	n, err := w.ExpireDue(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 4, n)
	assert.Equal(t, []string{
		domain + "/jobs/job_completed/", domain + "/jobs/job_rejected/", domain + "/jobs/job_failed/",
		domain + "/jobs/job_queued/", domain + "/jobs/job_queued/",
	}, st.deletedPrefixes(), "only a job that was not terminal when listed is swept twice")
}

func TestRetentionSecondSweepFailureIsBestEffort(t *testing.T) {
	t.Parallel()
	domain := "ret-second"
	fs := &fakeStore{jobs: []store.JobRef{{ID: "job_p", DomainKey: domain, Status: store.StatusProcessing}}}
	st := &fakeStorage{}
	st.afterDelete = func(call int, _ string) {
		if call == 1 {
			st.mu.Lock()
			st.failOn = []string{"/job_p/"}
			st.mu.Unlock()
		}
	}
	w := &RetentionWorker{Store: fs, Storage: st, Logger: discardLogger()}
	before := expiredCounter(t, domain)

	n, err := w.ExpireDue(context.Background())

	require.NoError(t, err, "the job is expired either way")
	assert.Equal(t, 1, n)
	assert.Equal(t, 1.0, expiredCounter(t, domain)-before)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	assert.True(t, fs.expired["job_p"])
	st.mu.Lock()
	defer st.mu.Unlock()
	assert.Len(t, st.deleteCtx, 2, "the second sweep was attempted")
}
