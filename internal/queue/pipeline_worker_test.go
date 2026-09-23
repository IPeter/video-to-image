package queue

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/metrics"
	"video-to-image/internal/store"
)

var errInfra = errors.New("storage: connection refused")

func newPipelineWorker(r Runner, s Store, ceiling float64) *PipelineWorker {
	return &PipelineWorker{
		Runner: r, Store: s, Storage: &fakeStorage{}, DailyCostCeilingUSD: ceiling, Logger: discardLogger(),
		Now: func() time.Time { return time.Date(2026, 9, 23, 17, 45, 0, 0, time.FixedZone("CEST", 2*3600)) },
	}
}

func TestPipelineWorkerTimeout(t *testing.T) {
	t.Parallel()
	w := &PipelineWorker{}
	assert.Equal(t, PipelineTimeout, w.Timeout(pipelineJob("j", 1, 3)))
}

func TestPipelineWorkerAttempts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		runErr      error
		failErr     error
		attempt     int
		maxAttempts int
		wantErr     bool
		wantFail    bool
	}{
		{name: "success", attempt: 1, maxAttempts: 3},
		{name: "success on final attempt", attempt: 3, maxAttempts: 3},
		{name: "error on first attempt is retried", runErr: errInfra, attempt: 1, maxAttempts: 3, wantErr: true},
		{name: "error on second attempt is retried", runErr: errInfra, attempt: 2, maxAttempts: 3, wantErr: true},
		{name: "error on final attempt fails the job", runErr: errInfra, attempt: 3, maxAttempts: 3, wantErr: true, wantFail: true},
		{name: "attempt past max fails the job", runErr: errInfra, attempt: 4, maxAttempts: 3, wantErr: true, wantFail: true},
		{name: "single attempt job", runErr: errInfra, attempt: 1, maxAttempts: 1, wantErr: true, wantFail: true},
		{
			name: "already terminal job is ignored", runErr: errInfra, attempt: 3, maxAttempts: 3,
			failErr: fmt.Errorf("store: fail job j: status completed: %w", store.ErrConflict), wantErr: true, wantFail: true,
		},
		{
			name: "store failure while failing keeps the pipeline error", runErr: errInfra, attempt: 3, maxAttempts: 3,
			failErr: errors.New("db down"), wantErr: true, wantFail: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := &fakeStore{failErr: tt.failErr}
			r := newFakeRunner(func(context.Context, string) error { return tt.runErr })
			w := newPipelineWorker(r, fs, 0)

			err := w.Work(context.Background(), pipelineJob("job_1", tt.attempt, tt.maxAttempts))

			assert.Equal(t, "job_1", <-r.calls)
			if tt.wantErr {
				require.ErrorIs(t, err, tt.runErr, "the runner's error is returned to River")
			} else {
				require.NoError(t, err)
			}
			fails := fs.failCalls()
			if !tt.wantFail {
				assert.Empty(t, fails)
				return
			}
			require.Len(t, fails, 1)
			assert.Equal(t, "job_1", fails[0].ID)
			assert.Equal(t, "PROCESSING_ERROR", fails[0].Code)
			assert.Equal(t, "Something went wrong while processing the video.", fails[0].Message)
			assert.Contains(t, fails[0].InternalLog, errInfra.Error())
			assert.Contains(t, fails[0].InternalLog, fmt.Sprintf("attempt %d/%d", tt.attempt, tt.maxAttempts))
		})
	}
}

func TestPipelineWorkerFinalAttemptAfterTimeoutStillFailsJob(t *testing.T) {
	t.Parallel()
	fs := &fakeStore{}
	fs.addJob("job_t", "pw-timeout", store.StatusProcessing)
	st := &fakeStorage{}
	r := newFakeRunner(func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return fmt.Errorf("pipeline: extract: %w", ctx.Err())
	})
	w := newPipelineWorker(r, fs, 0)
	w.Storage = st
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := w.Work(ctx, pipelineJob("job_t", 3, 3))

	require.ErrorIs(t, err, context.DeadlineExceeded)
	fails := fs.failCalls()
	require.Len(t, fails, 1, "a job timeout on the final attempt is a real failure")
	assert.Equal(t, "job_t", fails[0].ID)
	assert.Equal(t, []string{"pw-timeout/jobs/job_t/"}, st.deletedPrefixes())
	fs.mu.Lock()
	assert.NoError(t, fs.failCtx[0], "FailJob must run on a live context even though the job's deadline passed")
	fs.mu.Unlock()
	st.mu.Lock()
	assert.NoError(t, st.deleteCtx[0], "the files are deleted on a live context too")
	st.mu.Unlock()
}

func finishedCounter(t *testing.T, domain string, status store.JobStatus) float64 {
	t.Helper()
	return metricValue(t, metrics.JobsFinished.WithLabelValues(domain, string(status)))
}

// A job River gives up on keeps no files and no open step (plan/06: failed jobs delete
// their original immediately), unless it completed after all.
func TestPipelineWorkerFinalAttemptReleasesJob(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		status     store.JobStatus // "" → no job row
		failErr    error
		getErr     error
		storeFails bool // DeletePrefix fails
		wantStatus store.JobStatus
		wantMetric float64
		wantClean  bool
	}{
		{name: "processing job is failed and released", status: store.StatusProcessing,
			wantStatus: store.StatusFailed, wantMetric: 1, wantClean: true},
		{name: "queued job is failed and released", status: store.StatusQueued,
			wantStatus: store.StatusFailed, wantMetric: 1, wantClean: true},
		{name: "already failed job is released (runner's own FailJob reported an error)", status: store.StatusFailed,
			wantStatus: store.StatusFailed, wantClean: true},
		{name: "already rejected job is released", status: store.StatusRejected,
			wantStatus: store.StatusRejected, wantClean: true},
		{name: "completed job keeps its files", status: store.StatusCompleted, wantStatus: store.StatusCompleted},
		{name: "expired job is left alone", status: store.StatusExpired, wantStatus: store.StatusExpired},
		{name: "missing job row", failErr: fmt.Errorf("store: fail job: %w", store.ErrNotFound)},
		{name: "store failure while failing", status: store.StatusProcessing, failErr: errors.New("db down"),
			wantStatus: store.StatusProcessing},
		{name: "lookup failure after failing", status: store.StatusProcessing, getErr: errors.New("db down"),
			wantStatus: store.StatusFailed},
		{name: "file deletion failure is only logged", status: store.StatusProcessing, storeFails: true,
			wantStatus: store.StatusFailed, wantMetric: 1, wantClean: true},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			domain := fmt.Sprintf("pw-release-%d", i)
			fs := &fakeStore{failErr: tt.failErr, getErr: tt.getErr}
			if tt.status != "" {
				fs.addJob("job_r", domain, tt.status)
			}
			st := &fakeStorage{}
			if tt.storeFails {
				st.failOn = []string{"/job_r/"}
			}
			w := newPipelineWorker(newFakeRunner(func(context.Context, string) error { return errInfra }), fs, 0)
			w.Storage = st
			before := finishedCounter(t, domain, store.StatusFailed)

			err := w.Work(context.Background(), pipelineJob("job_r", 5, 5))

			require.ErrorIs(t, err, errInfra, "River still records the attempt's error")
			require.Len(t, fs.failCalls(), 1)
			assert.Equal(t, tt.wantStatus, fs.jobStatus("job_r"))
			assert.Equal(t, tt.wantMetric, finishedCounter(t, domain, store.StatusFailed)-before)
			if tt.wantClean {
				assert.Equal(t, []string{"job_r"}, fs.skippedJobs(), "the running step is closed")
				if !tt.storeFails {
					assert.Equal(t, []string{domain + "/jobs/job_r/"}, st.deletedPrefixes(), "the job's files are deleted")
				}
			} else {
				assert.Empty(t, fs.skippedJobs())
				assert.Empty(t, st.deletedPrefixes())
			}
		})
	}
}

func TestPipelineWorkerFinalAttemptWithoutStorage(t *testing.T) {
	t.Parallel()
	fs := &fakeStore{}
	fs.addJob("job_n", "pw-nostorage", store.StatusProcessing)
	w := newPipelineWorker(newFakeRunner(func(context.Context, string) error { return errInfra }), fs, 0)
	w.Storage = nil

	require.ErrorIs(t, w.Work(context.Background(), pipelineJob("job_n", 1, 1)), errInfra)
	assert.Equal(t, store.StatusFailed, fs.jobStatus("job_n"))
	assert.Equal(t, []string{"job_n"}, fs.skippedJobs())
}

func TestPipelineRetryDelay(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{0, 30 * time.Second}, {1, 30 * time.Second}, {2, 2 * time.Minute},
		{3, 10 * time.Minute}, {4, 30 * time.Minute}, {5, 30 * time.Minute}, {12, 30 * time.Minute},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, pipelineRetryDelay(tt.attempt, 0.5), "attempt %d without jitter", tt.attempt)
		assert.Equal(t, tt.want-tt.want/10, pipelineRetryDelay(tt.attempt, 0), "attempt %d lowest jitter", tt.attempt)
		assert.InDelta(t, float64(tt.want+tt.want/10), float64(pipelineRetryDelay(tt.attempt, 0.999999)),
			float64(time.Millisecond), "attempt %d highest jitter", tt.attempt)
	}

	// All retries fit in well under an hour, but a restart of a minute or two no
	// longer uses up every attempt (River's default would retry after 1 s, 16 s, 81 s
	// and 256 s).
	var total time.Duration
	for a := 1; a < PipelineMaxAttempts; a++ {
		total += pipelineRetryDelay(a, 0.5)
	}
	assert.Equal(t, 42*time.Minute+30*time.Second, total)
}

func TestPipelineWorkerNextRetry(t *testing.T) {
	t.Parallel()
	w := newPipelineWorker(newFakeRunner(nil), &fakeStore{}, 0) // Now is fixed; NextRetry uses the wall clock
	for attempt, base := range map[int]time.Duration{1: 30 * time.Second, 3: 10 * time.Minute} {
		before := time.Now()
		got := w.NextRetry(pipelineJob("job_n", attempt, PipelineMaxAttempts))
		after := time.Now()
		assert.False(t, got.Before(before.Add(base-base/10)), "attempt %d: %s too early", attempt, got.Sub(before))
		assert.False(t, got.After(after.Add(base+base/10)), "attempt %d: %s too late", attempt, got.Sub(before))
	}
	// A rescued job River hands over with its row still works; a zero job does not panic.
	assert.True(t, w.NextRetry(&river.Job[PipelineArgs]{JobRow: &rivertype.JobRow{Attempt: 2}}).After(time.Now()))
	assert.True(t, w.NextRetry(&river.Job[PipelineArgs]{}).After(time.Now()))
}

func TestPipelineWorkerShutdownIsNotAFinalFailure(t *testing.T) {
	t.Parallel()
	for _, runErr := range []error{
		context.Canceled,
		fmt.Errorf("pipeline: preview: %w", context.Canceled),
		errors.New("ffmpeg: signal: killed"), // does not wrap the cancellation
	} {
		t.Run(runErr.Error(), func(t *testing.T) {
			t.Parallel()
			fs := &fakeStore{}
			ctx, cancel := context.WithCancel(context.Background())
			r := newFakeRunner(func(context.Context, string) error {
				cancel() // the client stops while the job runs
				return runErr
			})
			w := newPipelineWorker(r, fs, 0)

			err := w.Work(ctx, pipelineJob("job_s", 3, 3))

			require.Error(t, err)
			assert.ErrorIs(t, err, context.Canceled, "River must see the cancellation to re-deliver without counting the attempt")
			assert.ErrorIs(t, err, runErr)
			assert.Empty(t, fs.failCalls(), "an interrupted job is re-delivered, not failed")
		})
	}
}

func TestPipelineWorkerRemoteCancelFailsJob(t *testing.T) {
	t.Parallel()
	fs := &fakeStore{}
	ctx, cancel := context.WithCancelCause(context.Background())
	r := newFakeRunner(func(ctx context.Context, _ string) error {
		cancel(river.ErrJobCancelledRemotely)
		return ctx.Err()
	})
	w := newPipelineWorker(r, fs, 0)

	err := w.Work(ctx, pipelineJob("job_c", 1, 3))

	require.Error(t, err)
	require.Len(t, fs.failCalls(), 1, "River never retries a cancelled job, so it is terminal")
}

func TestPipelineWorkerPanic(t *testing.T) {
	t.Parallel()
	tests := []struct {
		attempt  int
		wantFail bool
	}{{1, false}, {3, true}}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt %d", tt.attempt), func(t *testing.T) {
			t.Parallel()
			fs := &fakeStore{}
			r := newFakeRunner(func(context.Context, string) error { panic("nil map write") })
			w := newPipelineWorker(r, fs, 0)

			var err error
			require.NotPanics(t, func() { err = w.Work(context.Background(), pipelineJob("job_p", tt.attempt, 3)) })

			require.Error(t, err)
			assert.Contains(t, err.Error(), "panic: nil map write")
			fails := fs.failCalls()
			if tt.wantFail {
				require.Len(t, fails, 1)
				assert.Contains(t, fails[0].InternalLog, "panic: nil map write")
			} else {
				assert.Empty(t, fails)
			}
		})
	}
}

func TestPipelineWorkerEmptyJobIDIsCancelled(t *testing.T) {
	t.Parallel()
	fs := &fakeStore{}
	r := newFakeRunner(nil)
	w := newPipelineWorker(r, fs, 0)

	err := w.Work(context.Background(), pipelineJob("", 1, 3))

	var cancelErr *river.JobCancelError
	require.ErrorAs(t, err, &cancelErr)
	assert.Empty(t, r.calls)
	assert.Empty(t, fs.failCalls())
}

func TestPipelineWorkerCostCeiling(t *testing.T) {
	t.Parallel()
	// Now is 2026-09-23 17:45 CEST = 15:45 UTC → the day starts at 00:00 UTC.
	wantSince := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		ceiling    float64
		cost       float64
		wantSnooze bool
		wantQuery  bool
	}{
		{name: "disabled ceiling never queries", ceiling: 0, cost: 1000},
		{name: "negative ceiling is disabled", ceiling: -1, cost: 1000},
		{name: "below ceiling runs", ceiling: 5, cost: 4.99, wantQuery: true},
		{name: "at ceiling snoozes", ceiling: 5, cost: 5, wantQuery: true, wantSnooze: true},
		{name: "above ceiling snoozes", ceiling: 5, cost: 12.3, wantQuery: true, wantSnooze: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fs := &fakeStore{cost: tt.cost}
			r := newFakeRunner(nil)
			w := newPipelineWorker(r, fs, tt.ceiling)

			// Final attempt: a snooze must never be mistaken for a failure.
			err := w.Work(context.Background(), pipelineJob("job_c", 3, 3))

			if tt.wantSnooze {
				var snooze *river.JobSnoozeError
				require.ErrorAs(t, err, &snooze)
				assert.Equal(t, 15*time.Minute, snooze.Duration)
				assert.Empty(t, r.calls, "the runner must not run above the ceiling")
			} else {
				require.NoError(t, err)
				assert.Equal(t, "job_c", <-r.calls)
			}
			assert.Empty(t, fs.failCalls())
			fs.mu.Lock()
			defer fs.mu.Unlock()
			if tt.wantQuery {
				require.Len(t, fs.costSince, 1)
				assert.Equal(t, wantSince, fs.costSince[0])
			} else {
				assert.Empty(t, fs.costSince)
			}
		})
	}
}

func TestPipelineWorkerCostQueryError(t *testing.T) {
	t.Parallel()
	for _, attempt := range []int{1, 3} {
		t.Run(fmt.Sprintf("attempt %d", attempt), func(t *testing.T) {
			t.Parallel()
			fs := &fakeStore{costErr: errInfra}
			r := newFakeRunner(nil)
			w := newPipelineWorker(r, fs, 5)

			err := w.Work(context.Background(), pipelineJob("job_q", attempt, 3))

			require.ErrorIs(t, err, errInfra)
			assert.Empty(t, r.calls)
			assert.Len(t, fs.failCalls(), map[int]int{1: 0, 3: 1}[attempt])
		})
	}
}

func TestStartOfDayUTC(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want time.Time
	}{
		{time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 9, 23, 23, 59, 59, 999, time.UTC), time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)},
		// 01:30 in UTC+2 is still the previous UTC day.
		{time.Date(2026, 9, 24, 1, 30, 0, 0, time.FixedZone("CEST", 2*3600)), time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)},
		{time.Date(2026, 9, 23, 20, 0, 0, 0, time.FixedZone("EDT", -4*3600)), time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, startOfDayUTC(tt.in), tt.in.String())
	}
}
