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

	"video-to-image/internal/store"
)

// StudioRunner makes the studio backgrounds of one completed job
// (*studiojob.Runner). Run returns nil once the studio work has a final status (or there
// is nothing to do: the job is gone or its studio work already ended), and an error when
// River should retry: the segmentation sidecar is unreachable, or a database or storage
// failure. Photos already done are kept; a retry resumes after them.
type StudioRunner interface {
	Run(ctx context.Context, jobID string) error
}

// StudioFailReason is the reason photos without a studio outcome get when River gives up
// on the studio job (shown to users; the original photos are unaffected).
const StudioFailReason = "The studio background could not be made for this photo."

// studioRetryDelays[i] is the wait after failed attempt i+1: long enough for a sidecar
// restart (model load and warm-up) to finish.
var studioRetryDelays = []time.Duration{30 * time.Second, 2 * time.Minute}

// StudioWorker works StudioArgs on the studio queue.
//
// Attempts: per-photo failures are recorded on the photo by the runner, so an error
// reaching River means the sidecar was unreachable or the database/storage failed; it is
// retried after NextRetry up to StudioMaxAttempts. After the final attempt the job's
// studio work is marked failed here (store.FailStudio: studio_status failed, photos
// without an outcome failed with StudioFailReason); the job itself and its original
// photos are never touched. Studio work River discards without calling Work is failed by
// ReconcileWorker.
type StudioWorker struct {
	river.WorkerDefaults[StudioArgs]

	Runner StudioRunner
	Store  Store
	Logger *slog.Logger
}

// Timeout bounds one studio run.
func (w *StudioWorker) Timeout(*river.Job[StudioArgs]) time.Duration { return StudioTimeout }

// NextRetry schedules the retry of a failed attempt: 30 s after the first, then 2 min,
// each ±10%.
func (w *StudioWorker) NextRetry(job *river.Job[StudioArgs]) time.Time {
	attempt := 1
	if job != nil && job.JobRow != nil {
		attempt = job.Attempt
	}
	return time.Now().Add(studioRetryDelay(attempt, rand.Float64()))
}

// studioRetryDelay is the wait after failed attempt attempt (1-based); r in [0, 1) picks
// the jitter (0.5 is the exact table value).
func studioRetryDelay(attempt int, r float64) time.Duration {
	i := min(max(attempt, 1), len(studioRetryDelays)) - 1
	d := studioRetryDelays[i]
	return d + time.Duration((2*r-1)*pipelineRetryJitter*float64(d))
}

// Work runs the studio job and marks its studio work failed when River will not retry.
func (w *StudioWorker) Work(ctx context.Context, job *river.Job[StudioArgs]) error {
	jobID := job.Args.JobID
	log := logger(w.Logger).With("job_id", jobID, "river_job_id", job.ID,
		"attempt", job.Attempt, "max_attempts", job.MaxAttempts)
	if jobID == "" {
		log.Error("studio job without job id; cancelling")
		return river.JobCancel(errors.New("queue: studio: empty job id"))
	}

	err := w.run(ctx, jobID)
	switch {
	case err == nil:
		return nil
	case shutdownInterrupted(ctx):
		// Re-delivered without counting this attempt; the runner resumes after the
		// photos already done.
		log.Info("studio job interrupted by worker shutdown; River will re-deliver it", "err", err)
		if !errors.Is(err, context.Canceled) {
			err = fmt.Errorf("%w (interrupted: %w)", err, context.Canceled)
		}
		return err
	case job.Attempt >= job.MaxAttempts || remoteCancelled(ctx):
		log.Error("studio job failed and River will not retry it; marking studio work failed", "err", err)
		w.failStudio(ctx, log, job, err)
		return err
	default:
		log.Warn("studio attempt failed; River will retry", "err", err)
		return err
	}
}

// run calls the runner, turning a panic into an error so the final-attempt handling
// still records the failure.
func (w *StudioWorker) run(ctx context.Context, jobID string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("queue: studio %s: panic: %v\n%s", jobID, p, debug.Stack())
		}
	}()
	return w.Runner.Run(ctx, jobID)
}

// failStudio records the final failure on a context detached from the job's.
func (w *StudioWorker) failStudio(ctx context.Context, log *slog.Logger, job *river.Job[StudioArgs], cause error) {
	jobID := job.Args.JobID
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), failJobTimeout)
	defer cancel()
	err := w.Store.FailStudio(fctx, jobID, StudioFailReason)
	switch {
	case err == nil:
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		log.Info("studio work no longer pending or running; nothing to mark failed", "err", err)
		return
	default:
		log.Error("mark studio work failed; the orphan reconciler retries it", "err", err)
		return
	}
	internal := fmt.Sprintf("studio: river attempt %d/%d failed: %v", job.Attempt, job.MaxAttempts, cause)
	if err := w.Store.AppendInternalLog(fctx, jobID, internal); err != nil {
		log.Warn("append studio failure to the internal log", "err", err)
	}
}
