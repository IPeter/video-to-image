package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/metrics"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

// Retention batching: jobs are listed RetentionBatchSize at a time (plus the jobs that
// already failed in the same run), at most RetentionMaxBatches batches per run; the
// rest waits for the next run.
const (
	RetentionBatchSize  = 100
	RetentionMaxBatches = 50
)

// maxReportedErrors caps the per-job errors joined into a maintenance job's error,
// which River stores in river_job.errors.
const maxReportedErrors = 10

// Maintenance job timeouts (the River client default is one minute).
const (
	retentionTimeout  = 10 * time.Minute
	filesSweepTimeout = 10 * time.Minute
	costGuardTimeout  = time.Minute
	statsTimeout      = time.Minute
)

// RetentionWorker deletes the files of jobs past expires_at and marks them expired
// (plan/06-data-model.md "Retention and cleanup"). Metadata and scoring rows are kept.
type RetentionWorker struct {
	river.WorkerDefaults[RetentionArgs]

	Store   Store
	Storage storage.Storage
	Logger  *slog.Logger
	Now     func() time.Time // default time.Now
}

// Timeout bounds one retention run.
func (w *RetentionWorker) Timeout(*river.Job[RetentionArgs]) time.Duration { return retentionTimeout }

// Work runs ExpireDue.
func (w *RetentionWorker) Work(ctx context.Context, _ *river.Job[RetentionArgs]) error {
	_, err := w.ExpireDue(ctx)
	return err
}

// ExpireDue expires every job whose expires_at is before now: Storage.DeletePrefix of
// the job's prefix, then Store.ExpireJob and, for a job that was still queued or
// processing, a second DeletePrefix (see expire). A job whose files cannot be deleted
// is left as is (retried next run) and does not stop the others; the returned error
// joins the per-job failures. It returns the number of jobs expired.
func (w *RetentionWorker) ExpireDue(ctx context.Context) (int, error) {
	log := logger(w.Logger)
	now := nowOrDefault(w.Now) // fixed for the whole run so the loop terminates

	var (
		errs      []error
		failed    int
		expired   int
		attempted = map[string]bool{}
		capped    = true
	)
	for range RetentionMaxBatches {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			capped = false
			break
		}
		// Jobs that failed earlier in this run stay unexpired and are listed again
		// (there is no cursor), so the limit grows by their number: each batch still
		// brings up to RetentionBatchSize jobs not attempted yet.
		limit := RetentionBatchSize + failed
		refs, err := w.Store.ListExpiredJobs(ctx, now, limit)
		if err != nil {
			errs = append(errs, fmt.Errorf("list expired jobs: %w", err))
			capped = false
			break
		}
		fresh := 0
		for _, ref := range refs {
			// Failed earlier in this run: retried on the next run, not now.
			if attempted[ref.ID] {
				continue
			}
			attempted[ref.ID] = true
			fresh++
			if err := w.expire(ctx, log, ref); err != nil {
				failed++
				log.Warn("retention: expire job failed", "job_id", ref.ID, "domain", ref.DomainKey, "err", err)
				errs = append(errs, err)
				continue
			}
			expired++
			metrics.JobsFinished.WithLabelValues(ref.DomainKey, string(store.StatusExpired)).Inc()
			log.Debug("retention: job expired", "job_id", ref.ID, "domain", ref.DomainKey, "previous_status", ref.Status)
		}
		if len(refs) < limit || fresh == 0 {
			capped = false
			break
		}
	}
	if capped {
		log.Warn("retention: batch cap reached; remaining jobs are expired on the next run",
			"batches", RetentionMaxBatches, "batch_size", RetentionBatchSize)
	}

	if expired > 0 || failed > 0 {
		log.Info("retention cleanup done", "expired", expired, "failed", failed)
	}
	if len(errs) == 0 {
		return expired, nil
	}
	return expired, fmt.Errorf("queue: retention: %d expired, %d failed: %w", expired, failed, joinCapped(errs))
}

// expire deletes the job's files and marks it expired.
//
// Deleting first keeps the job listed until its files are gone, so a failed deletion
// is retried on the next run. But a worker still processing the job can upload images,
// thumbnails, the manifest and the ZIP after the first sweep listed the prefix and
// commit CompleteJob before ExpireJob; ExpireJob then flips completed → expired and the
// job is never listed again. So a job that was queued or processing when listed is
// swept a second time after ExpireJob: every upload that preceded a successful
// CompleteJob happened before ExpireJob, and from ExpireJob on the worker's own
// CompleteJob (or step check) fails with ErrConflict and it deletes its uploads itself.
// The same holds for a completed job whose studio work was pending or running.
// The second sweep is best effort (logged); the bucket lifecycle rule is the last net.
func (w *RetentionWorker) expire(ctx context.Context, log *slog.Logger, ref store.JobRef) error {
	if ref.ID == "" || ref.DomainKey == "" {
		// Never derive a storage prefix from an incomplete ref.
		return fmt.Errorf("job %q: incomplete job ref (domain %q)", ref.ID, ref.DomainKey)
	}
	prefix := storage.JobPrefix(ref.DomainKey, ref.ID)
	if err := w.Storage.DeletePrefix(ctx, prefix); err != nil {
		return fmt.Errorf("delete files of job %s: %w", ref.ID, err)
	}
	if err := w.Store.ExpireJob(ctx, ref.ID); err != nil {
		return fmt.Errorf("expire job %s: %w", ref.ID, err)
	}
	// A job that was already terminal when listed has no worker uploading any more,
	// unless its studio work was pending or running (studio variants and studio.zip are
	// uploaded after completion; from ExpireJob on the studio worker's writes fail and it
	// deletes its own uploads).
	if !ref.Status.Terminal() || ref.StudioStatus.Active() {
		if err := w.Storage.DeletePrefix(ctx, prefix); err != nil {
			log.Warn("retention: second file sweep after expiring failed; leftovers stay until the bucket lifecycle rule",
				"job_id", ref.ID, "domain", ref.DomainKey, "prefix", prefix, "err", err)
		}
	}
	return nil
}

// joinCapped joins errs, keeping the first maxReportedErrors.
func joinCapped(errs []error) error {
	if len(errs) <= maxReportedErrors {
		return errors.Join(errs...)
	}
	kept := slices.Clone(errs[:maxReportedErrors])
	kept = append(kept, fmt.Errorf("and %d more errors", len(errs)-maxReportedErrors))
	return errors.Join(kept...)
}

// FilesSweepWorker deletes model-provider uploads that outlived their job (the
// analyze step releases them itself; this is the safety net).
type FilesSweepWorker struct {
	river.WorkerDefaults[FilesSweepArgs]

	Sweeper   analyzer.Sweeper // nil: nothing to sweep
	OlderThan time.Duration
	Logger    *slog.Logger
}

// Timeout bounds one sweep.
func (w *FilesSweepWorker) Timeout(*river.Job[FilesSweepArgs]) time.Duration {
	return filesSweepTimeout
}

// Work sweeps leaked uploads older than OlderThan; a nil Sweeper is a no-op.
func (w *FilesSweepWorker) Work(ctx context.Context, _ *river.Job[FilesSweepArgs]) error {
	if w.Sweeper == nil {
		return nil
	}
	log := logger(w.Logger)
	n, err := w.Sweeper.SweepFiles(ctx, w.OlderThan)
	if err != nil {
		log.Warn("model files sweep failed", "deleted", n, "older_than", w.OlderThan, "err", err)
		return fmt.Errorf("queue: files sweep (deleted %d): %w", n, err)
	}
	log.Info("model files sweep done", "deleted", n, "older_than", w.OlderThan)
	return nil
}

// CostGuardWorker compares today's model spend with the daily ceiling and pauses the
// pipeline and studio queues above it (running jobs finish; nothing new is fetched by
// any worker process) and resumes them below it. A ceiling <= 0 disables the guard and
// makes sure the queues are running. The PipelineWorker's own ceiling check (and the
// studio runner's, before each paid check) covers the gap between two guard runs.
type CostGuardWorker struct {
	river.WorkerDefaults[CostGuardArgs]

	Store               Store
	Queues              QueueController
	DailyCostCeilingUSD float64
	Logger              *slog.Logger
	Now                 func() time.Time // default time.Now
}

// Timeout bounds one guard pass.
func (w *CostGuardWorker) Timeout(*river.Job[CostGuardArgs]) time.Duration { return costGuardTimeout }

// Work runs Check.
func (w *CostGuardWorker) Work(ctx context.Context, _ *river.Job[CostGuardArgs]) error {
	return w.Check(ctx)
}

// guardedQueues are the queues whose jobs spend model money. The pipeline queue is
// registered by every worker process; the studio queue only by processes with a
// segmentation sidecar, so it may legitimately be missing from river_queue.
var guardedQueues = []struct {
	name     string
	optional bool // not registered yet → nothing to pause
}{
	{QueuePipeline, false},
	{QueueStudio, true},
}

// Check runs one guard pass over every guarded queue; a queue that fails does not stop
// the others (the error joins their failures).
func (w *CostGuardWorker) Check(ctx context.Context) error {
	cost, err := w.Store.CostSince(ctx, startOfDayUTC(nowOrDefault(w.Now)))
	if err != nil {
		return fmt.Errorf("queue: cost guard: %w", err)
	}
	metrics.DailyCostUSD.Set(cost)
	over := w.DailyCostCeilingUSD > 0 && cost >= w.DailyCostCeilingUSD
	var errs []error
	for _, q := range guardedQueues {
		if over {
			errs = append(errs, w.pause(ctx, q.name, q.optional, cost))
		} else {
			errs = append(errs, w.resume(ctx, q.name, cost))
		}
	}
	return errors.Join(errs...)
}

// pause pauses the queue. River's QueuePause is idempotent (an already paused queue
// keeps its paused_at); the only error it reports for a valid name is
// rivertype.ErrNotFound when no client has registered the queue in river_queue yet,
// which is an error unless the queue is optional.
func (w *CostGuardWorker) pause(ctx context.Context, name string, optional bool, cost float64) error {
	q, err := w.Queues.QueueGet(ctx, name)
	if err != nil {
		if errors.Is(err, rivertype.ErrNotFound) {
			if optional {
				return nil
			}
			return fmt.Errorf("queue: cost guard: pause %s: queue not registered yet: %w", name, err)
		}
		return fmt.Errorf("queue: cost guard: get %s: %w", name, err)
	}
	if q.PausedAt == nil {
		if err := w.Queues.QueuePause(ctx, name, nil); err != nil {
			return fmt.Errorf("queue: cost guard: pause %s: %w", name, err)
		}
		logger(w.Logger).Warn("daily cost ceiling reached; queue paused", "queue", name,
			"cost_usd", cost, "ceiling_usd", w.DailyCostCeilingUSD)
	}
	metrics.QueuePaused.WithLabelValues(name).Set(1)
	return nil
}

// resume resumes the queue when it is paused. A queue missing from river_queue is not
// paused.
func (w *CostGuardWorker) resume(ctx context.Context, name string, cost float64) error {
	q, err := w.Queues.QueueGet(ctx, name)
	switch {
	case errors.Is(err, rivertype.ErrNotFound):
	case err != nil:
		return fmt.Errorf("queue: cost guard: get %s: %w", name, err)
	case q.PausedAt != nil:
		if err := w.Queues.QueueResume(ctx, name, nil); err != nil && !errors.Is(err, rivertype.ErrNotFound) {
			return fmt.Errorf("queue: cost guard: resume %s: %w", name, err)
		}
		logger(w.Logger).Info("queue resumed", "queue", name, "cost_usd", cost, "ceiling_usd", w.DailyCostCeilingUSD)
	}
	metrics.QueuePaused.WithLabelValues(name).Set(0)
	return nil
}

// queueDepthSQL counts River jobs per queue and state.
const queueDepthSQL = `SELECT queue, state::text, count(*) FROM river_job GROUP BY 1, 2`

// StatsWorker refreshes the table-size and queue-depth gauges and logs job counts by
// status. A failing part does not stop the others.
type StatsWorker struct {
	river.WorkerDefaults[StatsArgs]

	Store  Store
	DB     Querier // the pool holding the River tables
	Logger *slog.Logger
}

// Timeout bounds one stats pass.
func (w *StatsWorker) Timeout(*river.Job[StatsArgs]) time.Duration { return statsTimeout }

// Work runs Refresh.
func (w *StatsWorker) Work(ctx context.Context, _ *river.Job[StatsArgs]) error {
	return w.Refresh(ctx)
}

// Refresh runs one stats pass.
func (w *StatsWorker) Refresh(ctx context.Context) error {
	log := logger(w.Logger)
	var errs []error

	if rows, err := w.Store.TableRowEstimates(ctx); err != nil {
		errs = append(errs, fmt.Errorf("table row estimates: %w", err))
	} else {
		for table, n := range rows {
			metrics.TableRows.WithLabelValues(table).Set(float64(n))
		}
	}

	if depth, err := w.queueDepth(ctx); err != nil {
		errs = append(errs, fmt.Errorf("queue depth: %w", err))
	} else {
		// Every queue × state gets a series (0 when empty) so graphs have no gaps;
		// the vector is reset first so queues that disappeared drop out.
		queues := []string{QueuePipeline, QueueStudio, QueueMaintenance}
		for q := range depth {
			if !slices.Contains(queues, q) {
				queues = append(queues, q)
			}
		}
		metrics.QueueJobs.Reset()
		for _, q := range queues {
			for _, st := range rivertype.JobStates() {
				metrics.QueueJobs.WithLabelValues(q, string(st)).Set(float64(depth[q][string(st)]))
			}
		}
	}

	if counts, err := w.Store.CountJobsByStatus(ctx); err != nil {
		errs = append(errs, fmt.Errorf("count jobs by status: %w", err))
	} else {
		attrs := make([]any, 0, 2*len(store.JobStatuses))
		for _, st := range store.JobStatuses {
			attrs = append(attrs, string(st), counts[st])
			metrics.JobsByStatus.WithLabelValues(string(st)).Set(float64(counts[st]))
		}
		log.Info("job counts", attrs...)
	}

	if len(errs) > 0 {
		return fmt.Errorf("queue: stats: %w", errors.Join(errs...))
	}
	return nil
}

func (w *StatsWorker) queueDepth(ctx context.Context) (map[string]map[string]int64, error) {
	rows, err := w.DB.Query(ctx, queueDepthSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int64{}
	for rows.Next() {
		var (
			queue, state string
			n            int64
		)
		if err := rows.Scan(&queue, &state, &n); err != nil {
			return nil, err
		}
		if out[queue] == nil {
			out[queue] = map[string]int64{}
		}
		out[queue][state] = n
	}
	return out, rows.Err()
}
