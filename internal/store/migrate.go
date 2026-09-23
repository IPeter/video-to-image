package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"

	"video-to-image/migrations"
)

// migrateLockKey is the Postgres advisory lock that serialises concurrent migrators
// (e.g. the api and worker init containers starting at the same time).
const migrateLockKey int64 = 0x7632695f6d696772 // "v2i_migr"

// Migrate runs River's migrations, then the app's goose migrations. Both are
// idempotent, and concurrent callers are serialised by a session advisory lock, so
// every pod can safely run it before starting.
func Migrate(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire lock connection: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		return fmt.Errorf("migrate: advisory lock: %w", err)
	}
	defer func() {
		// Unlock on a fresh context: ctx may be cancelled, and a lock left on a pooled
		// session would block the next migrator until the connection is closed.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, "SELECT pg_advisory_unlock($1)", migrateLockKey); err != nil {
			logger.Warn("migrate: advisory unlock failed; closing the session", "err", err)
			_ = conn.Conn().Close(uctx)
		}
	}()

	rm, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Logger: logger})
	if err != nil {
		return fmt.Errorf("migrate: river: %w", err)
	}
	res, err := rm.Migrate(ctx, rivermigrate.DirectionUp, nil)
	if err != nil {
		return fmt.Errorf("migrate: river up: %w", err)
	}
	for _, v := range res.Versions {
		logger.Info("river migration applied", "version", v.Version)
	}

	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("migrate: goose: %w", err)
	}
	results, err := p.Up(ctx)
	if err != nil {
		return fmt.Errorf("migrate: goose up: %w", err)
	}
	for _, r := range results {
		logger.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path, "took", r.Duration)
	}
	return nil
}
