// Package queue wires River: job args, the insert-only client used by `serve`, the
// worker client used by `work`, and the periodic maintenance jobs.
package queue

import (
	"time"

	"github.com/riverqueue/river"
)

// Queues (plan/06-data-model.md "River tables").
const (
	QueuePipeline    = "pipeline"    // MaxWorkers = V2I_PIPELINE_WORKERS (2)
	QueueMaintenance = "maintenance" // MaxWorkers = 1
	// QueueStudio runs the studio backgrounds after a job completed (MaxWorkers =
	// V2I_STUDIO_WORKERS, 1). Only workers with a segmentation sidecar work it.
	QueueStudio = "studio"
)

// PipelineMaxAttempts: River retries the whole job only on infrastructure errors
// (DB, storage, a worker killed mid-job). Pipeline errors are terminal and set the job
// status explicitly. Retries are cheap because the runner resumes after analyze, and
// PipelineWorker.NextRetry spaces them out (30 s, 2 min, 10 min, 30 min) so a short
// database or storage restart does not use up every attempt.
const PipelineMaxAttempts = 5

// PipelineTimeout bounds one pipeline run.
const PipelineTimeout = 20 * time.Minute

// PipelineArgs runs the pipeline for one job.
type PipelineArgs struct {
	JobID string `json:"jobId"`
}

func (PipelineArgs) Kind() string { return "pipeline" }

func (PipelineArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueuePipeline,
		MaxAttempts: PipelineMaxAttempts,
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// StudioMaxAttempts: River retries a studio job when the segmentation sidecar is
// unreachable (or on database/storage errors); per-photo failures are recorded on the
// photo and do not fail the job. Retries resume after the photos already done.
const StudioMaxAttempts = 3

// StudioTimeout bounds one studio run (about 8 photos at the 45 s per-photo limit).
const StudioTimeout = 10 * time.Minute

// StudioArgs makes the studio backgrounds of one completed job. The pipeline inserts it
// in the transaction that completes the job (jobs.studio_status = pending).
type StudioArgs struct {
	JobID string `json:"jobId"`
}

func (StudioArgs) Kind() string { return "studio" }

func (StudioArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue:       QueueStudio,
		MaxAttempts: StudioMaxAttempts,
		UniqueOpts:  river.UniqueOpts{ByArgs: true},
	}
}

// RetentionArgs: periodic (every 10 min) deletion of expired jobs' files.
type RetentionArgs struct{}

func (RetentionArgs) Kind() string { return "retention_cleanup" }
func (RetentionArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 3}
}

// ReconcileArgs: periodic (every 10 min) failing of queued/processing jobs whose River
// pipeline job is gone (discarded, cancelled or deleted without Work marking the job
// failed, e.g. a worker killed on the final attempt), and of studio work left pending or
// running without a River studio job.
type ReconcileArgs struct{}

func (ReconcileArgs) Kind() string { return "reconcile_orphans" }

// InsertOpts: one attempt; the next periodic run is the retry.
func (ReconcileArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 1}
}

// FilesSweepArgs: daily (and on worker start) deletion of leaked model provider uploads
// older than 2 hours.
type FilesSweepArgs struct{}

func (FilesSweepArgs) Kind() string { return "model_files_sweep" }
func (FilesSweepArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 2}
}

// CostGuardArgs: periodic (every 5 min) check of today's model spend against
// DAILY_COST_CEILING_USD; pauses the pipeline and studio queues above it, resumes them
// below it.
type CostGuardArgs struct{}

func (CostGuardArgs) Kind() string { return "cost_guard" }
func (CostGuardArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 1}
}

// StatsArgs: periodic (every 5 min) refresh of table-size and queue-depth gauges.
type StatsArgs struct{}

func (StatsArgs) Kind() string { return "stats" }
func (StatsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{Queue: QueueMaintenance, MaxAttempts: 1}
}
