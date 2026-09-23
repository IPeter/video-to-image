package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"runtime/debug"
	"time"

	"github.com/riverqueue/river"

	"video-to-image/internal/metrics"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

// CostCeilingSnooze is how long a pipeline job waits when today's model spend has
// reached DAILY_COST_CEILING_USD.
const CostCeilingSnooze = 15 * time.Minute

// Error code and user-safe message set when River gives up on a job (pipeline.go
// CodeProcessingError; duplicated to keep queue free of the pipeline dependency).
const (
	failCode    = "PROCESSING_ERROR"
	failMessage = "Something went wrong while processing the video."
)

// failJobTimeout bounds the FailJob write (and the job lookup) after the final attempt.
// It runs on a context detached from the job's, which may already be past its deadline.
const failJobTimeout = 30 * time.Second

// pipelineRetryDelays[i] is the wait after failed attempt i+1 (later attempts reuse the
// last one): with PipelineMaxAttempts 5 a job rides out about 40 minutes of database or
// storage trouble instead of River's attempt^4 seconds (1 s, 16 s, ...).
var pipelineRetryDelays = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

// pipelineRetryJitter spreads retries by ±10% so jobs failed by the same outage do not
// all come back at the same moment.
const pipelineRetryJitter = 0.1

// PipelineWorker works PipelineArgs on the pipeline queue.
//
// Attempts: the runner sets terminal statuses itself, so an error reaching River is an
// infrastructure failure and is retried after NextRetry up to PipelineMaxAttempts.
// After the final attempt the job is marked failed (PROCESSING_ERROR) here, its steps
// are closed and its files deleted, because River then discards it and nothing else
// would move the job out of queued/processing. Jobs River discards without calling
// Work (a worker killed on the final attempt) are failed by ReconcileWorker.
//
// Snoozing (cost ceiling) does not use up attempts: River v0.47 stores a snoozed job
// with attempt-1 (and counts snoozes in metadata.snoozes), so a job can wait out any
// number of ceiling snoozes and still get all of its attempts afterwards. Likewise a
// job interrupted by a client shutdown is put back as available with attempt-1.
type PipelineWorker struct {
	river.WorkerDefaults[PipelineArgs]

	Runner Runner
	Store  Store
	// Storage deletes the files of a job failed after the final attempt (nil: they are
	// left to retention).
	Storage storage.Storage
	// DailyCostCeilingUSD > 0 snoozes jobs while CostSince(00:00 UTC) >= the ceiling.
	DailyCostCeilingUSD float64
	Logger              *slog.Logger
	Now                 func() time.Time // default time.Now
}

// Timeout bounds one pipeline run.
func (w *PipelineWorker) Timeout(*river.Job[PipelineArgs]) time.Duration { return PipelineTimeout }

// NextRetry schedules the retry of a failed attempt (River calls it for errors returned
// by Work and when its rescuer re-delivers a job whose worker died): 30 s after the
// first attempt, then 2 min, 10 min and 30 min, each ±10%. It uses the wall clock, not
// Now, because River compares it with its own clock.
func (w *PipelineWorker) NextRetry(job *river.Job[PipelineArgs]) time.Time {
	attempt := 1
	if job != nil && job.JobRow != nil {
		attempt = job.Attempt
	}
	return time.Now().Add(pipelineRetryDelay(attempt, rand.Float64()))
}

// pipelineRetryDelay is the wait after failed attempt attempt (1-based; values below 1
// count as 1). r in [0, 1) picks the jitter: 0.5 is the exact table value.
func pipelineRetryDelay(attempt int, r float64) time.Duration {
	i := min(max(attempt, 1), len(pipelineRetryDelays)) - 1
	d := pipelineRetryDelays[i]
	return d + time.Duration((2*r-1)*pipelineRetryJitter*float64(d))
}

// Work checks the cost ceiling, runs the pipeline and marks the job failed when River
// will not retry it.
func (w *PipelineWorker) Work(ctx context.Context, job *river.Job[PipelineArgs]) error {
	jobID := job.Args.JobID
	log := logger(w.Logger).With("job_id", jobID, "river_job_id", job.ID,
		"attempt", job.Attempt, "max_attempts", job.MaxAttempts)
	if jobID == "" {
		// Nothing can ever run this job; cancel instead of retrying.
		log.Error("pipeline job without job id; cancelling")
		return river.JobCancel(errors.New("queue: pipeline: empty job id"))
	}

	err := w.work(ctx, log, jobID)
	if err == nil {
		return nil
	}
	var snooze *river.JobSnoozeError
	if errors.As(err, &snooze) {
		return err
	}

	switch {
	case shutdownInterrupted(ctx):
		// River re-delivers the job without counting this attempt, and the runner
		// resumes from the first step not done. Make sure River recognises the
		// interruption even when the runner's error does not wrap the cancellation.
		log.Info("pipeline interrupted by worker shutdown; River will re-deliver it", "err", err)
		if !errors.Is(err, context.Canceled) {
			err = fmt.Errorf("%w (interrupted: %w)", err, context.Canceled)
		}
		return err
	case job.Attempt >= job.MaxAttempts || remoteCancelled(ctx):
		log.Error("pipeline failed and River will not retry it; marking job failed", "err", err)
		w.failJob(ctx, log, job, err)
		return err
	default:
		log.Warn("pipeline attempt failed; River will retry", "err", err)
		return err
	}
}

func (w *PipelineWorker) work(ctx context.Context, log *slog.Logger, jobID string) error {
	if w.DailyCostCeilingUSD > 0 {
		cost, err := w.Store.CostSince(ctx, startOfDayUTC(nowOrDefault(w.Now)))
		if err != nil {
			return fmt.Errorf("queue: pipeline %s: check cost ceiling: %w", jobID, err)
		}
		if cost >= w.DailyCostCeilingUSD {
			log.Warn("daily cost ceiling reached; snoozing pipeline job",
				"cost_usd", cost, "ceiling_usd", w.DailyCostCeilingUSD, "snooze", CostCeilingSnooze)
			return river.JobSnooze(CostCeilingSnooze)
		}
	}
	return w.run(ctx, jobID)
}

// run calls the runner, turning a panic into an error so the final-attempt handling
// still marks the job failed (River would otherwise discard it silently).
func (w *PipelineWorker) run(ctx context.Context, jobID string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("queue: pipeline %s: panic: %v\n%s", jobID, p, debug.Stack())
		}
	}()
	return w.Runner.Run(ctx, jobID)
}

// failJob marks the job failed after River's final attempt and releases it like the
// runner releases the jobs it fails itself (steps closed, files deleted). When the job
// is already failed or rejected (e.g. the runner's own FailJob committed but reported
// an error, so it never deleted the files) it is released too; a completed or expired
// job is left alone.
func (w *PipelineWorker) failJob(ctx context.Context, log *slog.Logger, job *river.Job[PipelineArgs], cause error) {
	jobID := job.Args.JobID
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failJobTimeout)
	defer cancel()
	internal := fmt.Sprintf("river attempt %d/%d failed: %v", job.Attempt, job.MaxAttempts, cause)
	failErr := w.Store.FailJob(fctx, jobID, failCode, failMessage, internal)
	switch {
	case failErr == nil, errors.Is(failErr, store.ErrConflict):
	case errors.Is(failErr, store.ErrNotFound):
		log.Warn("job row not found; nothing to mark failed")
		return
	default:
		log.Error("mark job failed; the orphan reconciler retries it", "err", failErr)
		return
	}

	j, err := w.Store.GetJob(fctx, jobID)
	if err != nil {
		log.Error("look up job to release it; retention deletes its files at expiry", "err", err)
		return
	}
	log = log.With("domain", j.DomainKey)
	if failErr == nil {
		metrics.JobsFinished.WithLabelValues(j.DomainKey, string(store.StatusFailed)).Inc()
	} else {
		if j.Status != store.StatusFailed && j.Status != store.StatusRejected {
			log.Info("job already terminal; not marking it failed", "status", j.Status)
			return
		}
		log.Info("job already terminal; making sure it is released", "status", j.Status)
	}
	if err := releaseFailedJob(ctx, w.Store, w.Storage, jobID, j.DomainKey); err != nil {
		log.Warn("release failed job", "err", err)
	}
}

// shutdownInterrupted reports whether the job's context was cancelled because the
// River client is stopping. With SoftStopTimeout set (NewWorker always sets it), the
// only cancellations of the work context are the client stop, the job timeout
// (DeadlineExceeded) and a remote JobCancel.
func shutdownInterrupted(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled) && !remoteCancelled(ctx)
}

// remoteCancelled reports whether the job was cancelled through River's JobCancel;
// River then never retries it.
func remoteCancelled(ctx context.Context) bool {
	return ctx.Err() != nil && errors.Is(context.Cause(ctx), river.ErrJobCancelledRemotely)
}

func logger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.Default()
	}
	return l
}
