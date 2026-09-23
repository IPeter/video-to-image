package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// Client is the insert side of River. `serve` uses an insert-only Client
// (NewInsertClient); `serve --embed-worker` and `work` use Worker.Client, which wraps
// the worker's own River client. It satisfies httpapi.Enqueuer and is safe for
// concurrent use.
type Client struct {
	river *river.Client[pgx.Tx]
}

// NewInsertClient returns a River client with no queues or workers: it can insert
// jobs but never works them, so it must not be started. The River tables must exist
// (store.Migrate).
func NewInsertClient(pool *pgxpool.Pool, logger *slog.Logger) (*Client, error) {
	if pool == nil {
		return nil, errors.New("queue: insert client: nil pool")
	}
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: riverLogger(logger)})
	if err != nil {
		return nil, fmt.Errorf("queue: insert client: %w", err)
	}
	return &Client{river: rc}, nil
}

// EnqueuePipelineTx inserts the pipeline job for jobID inside tx, so the jobs row and
// the River job commit or roll back together. PipelineArgs is unique by args: when a
// pipeline job for jobID is already queued or running, the insert is skipped and nil
// is returned.
func (c *Client) EnqueuePipelineTx(ctx context.Context, tx pgx.Tx, jobID string) error {
	if jobID == "" {
		return errors.New("queue: enqueue pipeline: empty job id")
	}
	if tx == nil {
		return fmt.Errorf("queue: enqueue pipeline %s: nil transaction", jobID)
	}
	if _, err := c.river.InsertTx(ctx, tx, PipelineArgs{JobID: jobID}, nil); err != nil {
		return fmt.Errorf("queue: enqueue pipeline %s: %w", jobID, err)
	}
	return nil
}

// River exposes the underlying River client (queue inspection, tests).
func (c *Client) River() *river.Client[pgx.Tx] { return c.river }

// riverLogger tags River's own log lines so they can be told apart from the app's.
func riverLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		logger = slog.Default()
	}
	return logger.With("component", "river")
}
