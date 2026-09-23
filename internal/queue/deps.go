package queue

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"video-to-image/internal/store"
)

// Runner runs the pipeline for one job (*pipeline.Runner). Run returns nil once it has
// set a terminal job status itself (completed, rejected, failed), and an error only
// for infrastructure failures (database, storage, cancellation) that River should
// retry.
type Runner interface {
	Run(ctx context.Context, jobID string) error
}

// Store is the subset of *store.Store the workers use.
type Store interface {
	GetJob(ctx context.Context, id string) (*store.Job, error)
	FailJob(ctx context.Context, id, code, message, internalLog string) error
	SkipRemainingSteps(ctx context.Context, jobID string) error
	ListOrphanedJobs(ctx context.Context, idleSince time.Time, limit int) ([]store.JobRef, error)
	ListExpiredJobs(ctx context.Context, now time.Time, limit int) ([]store.JobRef, error)
	ExpireJob(ctx context.Context, id string) error
	CostSince(ctx context.Context, since time.Time) (float64, error)
	TableRowEstimates(ctx context.Context) (map[string]int64, error)
	CountJobsByStatus(ctx context.Context) (map[store.JobStatus]int64, error)
}

var _ Store = (*store.Store)(nil)

// QueueController pauses and resumes River queues (*river.Client[pgx.Tx]). Pausing is
// stored in river_queue, so it applies to every worker process sharing the database.
type QueueController interface {
	QueueGet(ctx context.Context, name string) (*rivertype.Queue, error)
	QueuePause(ctx context.Context, name string, opts *river.QueuePauseOpts) error
	QueueResume(ctx context.Context, name string, opts *river.QueuePauseOpts) error
}

var _ QueueController = (*river.Client[pgx.Tx])(nil)

// Querier runs a read query (*pgxpool.Pool, pgx.Tx).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// startOfDayUTC is 00:00 UTC of t's UTC day: the window of the daily cost ceiling.
func startOfDayUTC(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func nowOrDefault(now func() time.Time) time.Time {
	if now == nil {
		return time.Now()
	}
	return now()
}
