package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"video-to-image/internal/store/sqlcdb"
)

// The method set, signatures and doc comments are the contract used by httpapi,
// pipeline and queue. Implementations live in jobs.go, steps.go, frames.go, images.go,
// usage.go and maintenance.go; SQL is in queries/*.sql (sqlc).
//
// Errors: ErrNotFound and ErrConflict are wrapped with context, so callers must test
// them with errors.Is.

// errForeignJob is returned when a row passed for one job names another.
var errForeignJob = errors.New("row belongs to another job")

// Store is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
	q    *sqlcdb.Queries
}

func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool, q: sqlcdb.New(pool)} }

// Pool exposes the pool (River shares it).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping checks connectivity for /healthz.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// inTx runs fn in a transaction: committed when fn returns nil, rolled back otherwise
// (also on panic).
func (s *Store) inTx(ctx context.Context, fn func(tx pgx.Tx, q *sqlcdb.Queries) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return fn(tx, s.q.WithTx(tx))
	})
}

// jobStateErr is called after a conditional UPDATE of job id matched no row. It
// returns nil when the job's current status is one of okIf (an idempotent no-op),
// ErrNotFound when the job does not exist and ErrConflict otherwise. The caller adds
// the "store: <op> <id>" context.
func jobStateErr(ctx context.Context, q *sqlcdb.Queries, id string, okIf ...JobStatus) error {
	st, err := q.GetJobStatus(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	for _, ok := range okIf {
		if JobStatus(st) == ok {
			return nil
		}
	}
	return fmt.Errorf("status %s: %w", st, ErrConflict)
}

// isMissingJob reports whether err is a foreign key violation on a job_id → jobs(id)
// reference, i.e. the referenced job does not exist.
func isMissingJob(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23503" && strings.HasSuffix(pe.ConstraintName, "job_id_fkey")
}

// wrapJobErr wraps err for op on job id, mapping a missing-job FK violation to
// ErrNotFound.
func wrapJobErr(op, id string, err error) error {
	if isMissingJob(err) {
		return fmt.Errorf("store: %s %s: %w", op, id, ErrNotFound)
	}
	return fmt.Errorf("store: %s %s: %w", op, id, err)
}
