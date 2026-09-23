package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"

	"video-to-image/internal/metrics"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

// ReconcileBatchSize caps the orphaned jobs failed per run; the rest waits for the
// next run.
const ReconcileBatchSize = 100

// reconcileTimeout bounds one reconciler run.
const reconcileTimeout = 5 * time.Minute

// cleanupTimeout bounds releasing a failed job (closing its steps and deleting its
// files). It runs on a context detached from the job's, so a job that was just marked
// failed never keeps its files because the run's context ended.
const cleanupTimeout = time.Minute

// reconcileInternalLog is appended to internal_log of a job failed by the reconciler.
const reconcileInternalLog = "reconciler: River gave up on this job (no active pipeline job)"

// ReconcileWorker fails jobs that are still queued or processing although River no
// longer has an active pipeline job for them: River discarded, cancelled or deleted
// the work without PipelineWorker.Work marking the job failed (a worker killed on the
// final attempt and discarded by River's rescuer, or a final FailJob that could not be
// written). Without it such a job would show a spinner until retention expires it.
type ReconcileWorker struct {
	river.WorkerDefaults[ReconcileArgs]

	Store   Store
	Storage storage.Storage // nil: files are left to retention
	// Grace: only jobs not updated for this long are orphans (<= 0: DefaultOrphanGrace).
	Grace  time.Duration
	Logger *slog.Logger
	Now    func() time.Time // default time.Now
}

// Timeout bounds one reconciler run.
func (w *ReconcileWorker) Timeout(*river.Job[ReconcileArgs]) time.Duration { return reconcileTimeout }

// Work runs Reconcile.
func (w *ReconcileWorker) Work(ctx context.Context, _ *river.Job[ReconcileArgs]) error {
	_, err := w.Reconcile(ctx)
	return err
}

// Reconcile fails up to ReconcileBatchSize orphaned jobs, oldest first: FailJob
// (PROCESSING_ERROR, the user-safe message), then SkipRemainingSteps and DeletePrefix of
// the job's files. A job that became terminal in the meantime (ErrConflict) is skipped.
// A failing job does not stop the others; the returned error joins the per-job
// failures. It returns the number of jobs marked failed.
func (w *ReconcileWorker) Reconcile(ctx context.Context) (int, error) {
	log := logger(w.Logger)
	grace := w.Grace
	if grace <= 0 {
		grace = DefaultOrphanGrace
	}
	idleSince := nowOrDefault(w.Now).Add(-grace)
	refs, err := w.Store.ListOrphanedJobs(ctx, idleSince, ReconcileBatchSize)
	if err != nil {
		return 0, fmt.Errorf("queue: reconcile: list orphaned jobs: %w", err)
	}

	var (
		errs   []error
		failed int
	)
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		marked, err := w.failOrphan(ctx, log, ref)
		if marked {
			failed++
		}
		if err != nil {
			log.Warn("reconcile: orphaned job", "job_id", ref.ID, "domain", ref.DomainKey, "marked_failed", marked, "err", err)
			errs = append(errs, err)
		}
	}
	if len(refs) >= ReconcileBatchSize {
		log.Warn("reconcile: batch full; remaining orphaned jobs are failed on the next run", "batch_size", ReconcileBatchSize)
	}
	if failed > 0 || len(errs) > 0 {
		log.Info("reconcile done", "orphans", len(refs), "marked_failed", failed, "errors", len(errs))
	}
	if len(errs) == 0 {
		return failed, nil
	}
	return failed, fmt.Errorf("queue: reconcile: %d marked failed, %d errors: %w", failed, len(errs), joinCapped(errs))
}

// failOrphan fails one orphaned job and releases it. marked reports whether this call
// moved the job to failed; err is set when that or the release failed.
func (w *ReconcileWorker) failOrphan(ctx context.Context, log *slog.Logger, ref store.JobRef) (marked bool, err error) {
	if ref.ID == "" || ref.DomainKey == "" {
		// Never derive a storage prefix from an incomplete ref.
		return false, fmt.Errorf("job %q: incomplete job ref (domain %q)", ref.ID, ref.DomainKey)
	}
	log = log.With("job_id", ref.ID, "domain", ref.DomainKey, "previous_status", ref.Status)
	// Failing first means a runner that is somehow still working the job hits
	// ErrConflict on its next status check and deletes its own uploads.
	err = w.Store.FailJob(ctx, ref.ID, failCode, failMessage, reconcileInternalLog)
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		log.Info("reconcile: job no longer active; skipping", "err", err)
		return false, nil
	case err != nil:
		return false, fmt.Errorf("fail job %s: %w", ref.ID, err)
	}
	metrics.JobsFinished.WithLabelValues(ref.DomainKey, string(store.StatusFailed)).Inc()
	log.Warn("reconcile: River gave up on the job; marked failed")
	return true, releaseFailedJob(ctx, w.Store, w.Storage, ref.ID, ref.DomainKey)
}

// releaseFailedJob does what the runner does for the jobs it fails itself: close the
// job's pending and running steps (so the progress payload names no active step) and
// delete its files (failed jobs keep no files, plan/06). It runs on a context detached
// from ctx and bounded by cleanupTimeout. Both parts run even if the other fails; the
// error joins their failures. Files that stay behind are deleted by retention at
// expiry. A nil stg skips the deletion.
func releaseFailedJob(ctx context.Context, st Store, stg storage.Storage, jobID, domainKey string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var errs []error
	if err := st.SkipRemainingSteps(ctx, jobID); err != nil {
		errs = append(errs, fmt.Errorf("skip remaining steps of job %s: %w", jobID, err))
	}
	switch {
	case stg == nil:
	case jobID == "" || domainKey == "":
		errs = append(errs, fmt.Errorf("delete files of job %q: incomplete job (domain %q)", jobID, domainKey))
	default:
		if err := stg.DeletePrefix(ctx, storage.JobPrefix(domainKey, jobID)); err != nil {
			errs = append(errs, fmt.Errorf("delete files of job %s: %w", jobID, err))
		}
	}
	return errors.Join(errs...)
}
