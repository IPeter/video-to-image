package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"video-to-image/internal/store"
	"video-to-image/internal/testutil/pgtest"
)

func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	for _, table := range []string{"jobs", "job_steps", "scenes", "candidate_frames", "selected_images", "usage_events", "river_job"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists))
		require.True(t, exists, table)
	}
	require.NoError(t, store.Migrate(ctx, pool, nil))
}
