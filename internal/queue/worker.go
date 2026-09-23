package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/storage"
)

// Worker defaults (plan/06-data-model.md, plan/03-pipeline.md).
const (
	DefaultPipelineWorkers   = 2
	DefaultRetentionInterval = 10 * time.Minute
	DefaultReconcileInterval = 10 * time.Minute
	// DefaultOrphanGrace: a job is only treated as orphaned once its row has not been
	// updated for this long, so a job whose pipeline job is being inserted or finalized
	// right now is never caught.
	DefaultOrphanGrace       = 15 * time.Minute
	DefaultCostGuardInterval = 5 * time.Minute
	DefaultStatsInterval     = 5 * time.Minute
	DefaultSweepInterval     = 24 * time.Hour
	DefaultSweepOlderThan    = 2 * time.Hour
)

// hardStopTimeout bounds the wait for jobs to return after Stop cancelled them.
const hardStopTimeout = 10 * time.Second

// rescueStuckJobsAfter: a pipeline job still `running` this long after it started
// belongs to a dead worker (pod killed mid-job) and is re-delivered. It must exceed
// PipelineTimeout.
const rescueStuckJobsAfter = PipelineTimeout + 10*time.Minute

// WorkerConfig configures NewWorker. Zero durations and counts take the defaults above.
type WorkerConfig struct {
	Runner  Runner          // required
	Store   Store           // required
	Storage storage.Storage // required (retention, reconciler, final-attempt cleanup)
	Sweeper analyzer.Sweeper
	// PipelineWorkers is the pipeline queue's MaxWorkers in this process.
	PipelineWorkers int
	// DailyCostCeilingUSD > 0 enables the cost guard and the per-job ceiling check.
	DailyCostCeilingUSD float64
	Logger              *slog.Logger

	RetentionInterval time.Duration
	ReconcileInterval time.Duration
	// OrphanGrace: the reconciler only fails jobs not updated for this long.
	OrphanGrace       time.Duration
	CostGuardInterval time.Duration
	StatsInterval     time.Duration
	SweepInterval     time.Duration
	SweepOlderThan    time.Duration

	Now func() time.Time // default time.Now

	// riverConfig, when set, adjusts the River config before the client is built
	// (tests: TestOnly, poll intervals).
	riverConfig func(*river.Config)
}

// Worker is the River client that works the pipeline and maintenance queues and
// enqueues the periodic maintenance jobs. Periodic jobs are only inserted by the
// elected leader among all worker processes.
type Worker struct {
	river  *river.Client[pgx.Tx]
	client *Client
	logger *slog.Logger
}

// NewWorker builds the worker client. The River tables must exist (store.Migrate).
//
// Queues: pipeline (MaxWorkers = PipelineWorkers) and maintenance (MaxWorkers = 1).
// Periodic jobs, each also run on start (River keeps periodic schedules only in memory,
// so a job that only ran after a full interval would never run on a worker restarted
// more often than that): retention, orphan reconciler, cost guard, stats and, when
// Sweeper is set, the model files sweep.
func NewWorker(pool *pgxpool.Pool, cfg WorkerConfig) (*Worker, error) {
	if pool == nil {
		return nil, errors.New("queue: worker: nil pool")
	}
	if err := cfg.withDefaults(); err != nil {
		return nil, err
	}
	log := cfg.Logger

	pipelineW := &PipelineWorker{
		Runner: cfg.Runner, Store: cfg.Store, Storage: cfg.Storage, DailyCostCeilingUSD: cfg.DailyCostCeilingUSD,
		Logger: log.With("worker", PipelineArgs{}.Kind()), Now: cfg.Now,
	}
	retentionW := &RetentionWorker{
		Store: cfg.Store, Storage: cfg.Storage, Logger: log.With("worker", RetentionArgs{}.Kind()), Now: cfg.Now,
	}
	reconcileW := &ReconcileWorker{
		Store: cfg.Store, Storage: cfg.Storage, Grace: cfg.OrphanGrace,
		Logger: log.With("worker", ReconcileArgs{}.Kind()), Now: cfg.Now,
	}
	sweepW := &FilesSweepWorker{
		Sweeper: cfg.Sweeper, OlderThan: cfg.SweepOlderThan, Logger: log.With("worker", FilesSweepArgs{}.Kind()),
	}
	costW := &CostGuardWorker{
		Store: cfg.Store, DailyCostCeilingUSD: cfg.DailyCostCeilingUSD,
		Logger: log.With("worker", CostGuardArgs{}.Kind()), Now: cfg.Now,
	}
	statsW := &StatsWorker{Store: cfg.Store, DB: pool, Logger: log.With("worker", StatsArgs{}.Kind())}

	workers := river.NewWorkers()
	if err := errors.Join(
		river.AddWorkerSafely(workers, pipelineW),
		river.AddWorkerSafely(workers, retentionW),
		river.AddWorkerSafely(workers, reconcileW),
		river.AddWorkerSafely(workers, sweepW),
		river.AddWorkerSafely(workers, costW),
		river.AddWorkerSafely(workers, statsW),
	); err != nil {
		return nil, fmt.Errorf("queue: worker: register workers: %w", err)
	}

	periodic := []*river.PeriodicJob{
		periodicJob(cfg.RetentionInterval, RetentionArgs{}, true),
		periodicJob(cfg.ReconcileInterval, ReconcileArgs{}, true),
		periodicJob(cfg.CostGuardInterval, CostGuardArgs{}, true),
		periodicJob(cfg.StatsInterval, StatsArgs{}, true),
	}
	if cfg.Sweeper != nil {
		// Cheap: it only deletes uploads older than SweepOlderThan.
		periodic = append(periodic, periodicJob(cfg.SweepInterval, FilesSweepArgs{}, true))
	}

	rcfg := &river.Config{
		Logger: riverLogger(log),
		Queues: map[string]river.QueueConfig{
			QueuePipeline:    {MaxWorkers: cfg.PipelineWorkers},
			QueueMaintenance: {MaxWorkers: 1},
		},
		Workers:              workers,
		PeriodicJobs:         periodic,
		RescueStuckJobsAfter: rescueStuckJobsAfter,
		// A soft stop timeout makes cancelling Start's context a graceful stop (running
		// jobs continue) instead of an immediate cancellation, and every shutdown
		// cancellation then carries River's stop cause, so interrupted jobs go back
		// to available without using up an attempt. Stop(ctx) escalates earlier when
		// its ctx expires.
		SoftStopTimeout: PipelineTimeout,
	}
	if cfg.riverConfig != nil {
		cfg.riverConfig(rcfg)
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), rcfg)
	if err != nil {
		return nil, fmt.Errorf("queue: worker: %w", err)
	}
	costW.Queues = rc

	return &Worker{river: rc, client: &Client{river: rc}, logger: log}, nil
}

func (c *WorkerConfig) withDefaults() error {
	switch {
	case c.Runner == nil:
		return errors.New("queue: worker: Runner is required")
	case c.Store == nil:
		return errors.New("queue: worker: Store is required")
	case c.Storage == nil:
		return errors.New("queue: worker: Storage is required")
	case c.PipelineWorkers < 0:
		return fmt.Errorf("queue: worker: PipelineWorkers must not be negative (got %d)", c.PipelineWorkers)
	}
	if c.PipelineWorkers == 0 {
		c.PipelineWorkers = DefaultPipelineWorkers
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	for _, d := range []struct {
		name string
		v    *time.Duration
		def  time.Duration
	}{
		{"RetentionInterval", &c.RetentionInterval, DefaultRetentionInterval},
		{"ReconcileInterval", &c.ReconcileInterval, DefaultReconcileInterval},
		{"OrphanGrace", &c.OrphanGrace, DefaultOrphanGrace},
		{"CostGuardInterval", &c.CostGuardInterval, DefaultCostGuardInterval},
		{"StatsInterval", &c.StatsInterval, DefaultStatsInterval},
		{"SweepInterval", &c.SweepInterval, DefaultSweepInterval},
		{"SweepOlderThan", &c.SweepOlderThan, DefaultSweepOlderThan},
	} {
		if *d.v < 0 {
			return fmt.Errorf("queue: worker: %s must not be negative (got %s)", d.name, *d.v)
		}
		if *d.v == 0 {
			*d.v = d.def
		}
	}
	return nil
}

func periodicJob(every time.Duration, args river.JobArgs, runOnStart bool) *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(every),
		func() (river.JobArgs, *river.InsertOpts) { return args, nil },
		&river.PeriodicJobOpts{ID: args.Kind(), RunOnStart: runOnStart},
	)
}

// Start begins fetching and working jobs in the background. Cancelling ctx starts a
// graceful stop like Stop, but without the escalation deadline; prefer Stop.
func (w *Worker) Start(ctx context.Context) error {
	if err := w.river.Start(ctx); err != nil {
		return fmt.Errorf("queue: worker: start: %w", err)
	}
	return nil
}

// Stop stops fetching and waits for running jobs to finish until ctx is done; then it
// cancels them (the pipeline resumes on re-delivery) and waits a little longer for them
// to return. It returns nil once the client has stopped. Stopping a worker that was
// never started is a no-op.
func (w *Worker) Stop(ctx context.Context) error {
	err := w.river.Stop(ctx)
	if err == nil {
		return nil
	}
	if ctx.Err() == nil {
		return fmt.Errorf("queue: worker: stop: %w", err)
	}
	w.logger.Warn("queue: graceful stop timed out; cancelling running jobs", "err", err)
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), hardStopTimeout)
	defer cancel()
	if err := w.river.StopAndCancel(hctx); err != nil {
		return fmt.Errorf("queue: worker: stop and cancel: %w", err)
	}
	return nil
}

// Client enqueues through the worker's River client (serve --embed-worker).
func (w *Worker) Client() *Client { return w.client }

// River exposes the underlying River client.
func (w *Worker) River() *river.Client[pgx.Tx] { return w.river }
