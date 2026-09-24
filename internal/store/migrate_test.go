package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
	"video-to-image/internal/testutil/pgtest"
	"video-to-image/migrations"
)

func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	for _, table := range []string{"jobs", "job_steps", "scenes", "candidate_frames", "selected_images", "usage_events", "river_job"} {
		var exists bool
		require.NoError(t, pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists))
		require.True(t, exists, table)
	}
	// 00002_image_edits.
	for col, want := range map[string]string{"rotation": "integer NO 0", "crop": "jsonb YES "} {
		var got string
		require.NoError(t, pool.QueryRow(ctx, `SELECT data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
			FROM information_schema.columns WHERE table_name = 'selected_images' AND column_name = $1`, col).Scan(&got), col)
		assert.Equal(t, want, got, col)
	}
	// 00003_studio.
	for col, want := range map[string]string{"studio_status": "text NO 'none'::text", "studio_zip_path": "text YES "} {
		var got string
		require.NoError(t, pool.QueryRow(ctx, `SELECT data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
			FROM information_schema.columns WHERE table_name = 'jobs' AND column_name = $1`, col).Scan(&got), col)
		assert.Equal(t, want, got, col)
	}
	for col, want := range map[string]string{"subject_box": "jsonb YES ", "studio": "jsonb YES "} {
		var got string
		require.NoError(t, pool.QueryRow(ctx, `SELECT data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
			FROM information_schema.columns WHERE table_name = 'selected_images' AND column_name = $1`, col).Scan(&got), col)
		assert.Equal(t, want, got, col)
	}
	require.NoError(t, store.Migrate(ctx, pool, nil))
}

// 00003 rolls back and re-applies over existing rows: they read as without studio work.
func TestMigrateStudioDownAndUp(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	s := store.New(pool)
	id := processingJob(t, s)
	img := ids.NewImage()
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{Studio: store.StudioPending, Images: []store.SelectedImage{
		{ID: img, Idx: 1, ShotID: "front", SubjectBox: &store.NRect{X0: 0.1, Y0: 0.1, X1: 0.9, Y1: 0.9},
			Studio: &store.ImageStudio{Status: store.ImageStudioSkipped}},
	}}))

	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	require.NoError(t, err)
	_, err = p.DownTo(ctx, 2)
	require.NoError(t, err)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM information_schema.columns
		WHERE (table_name = 'jobs' AND column_name IN ('studio_status', 'studio_zip_path'))
		   OR (table_name = 'selected_images' AND column_name IN ('subject_box', 'studio'))`))

	require.NoError(t, store.Migrate(ctx, pool, nil))
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.StudioNone, j.StudioStatus)
	got, err := s.GetSelectedImage(ctx, id, img)
	require.NoError(t, err)
	assert.Nil(t, got.SubjectBox)
	assert.Nil(t, got.Studio)
}

// 00002 must roll back and re-apply cleanly over existing rows: rows written before it
// read as unrotated and uncropped.
func TestMigrateImageEditsDownAndUp(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	s := store.New(pool)
	id := processingJob(t, s)
	img := ids.NewImage()
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{Images: []store.SelectedImage{
		{ID: img, Idx: 1, ShotID: "front", Rotation: 90, Crop: &store.NRect{X0: 0, Y0: 0.25, X1: 1, Y1: 0.75}},
	}}))

	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	require.NoError(t, err)
	v, err := p.GetDBVersion(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 3, v)

	_, err = p.DownTo(ctx, 1)
	require.NoError(t, err)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM information_schema.columns
		WHERE table_name = 'selected_images' AND column_name IN ('rotation', 'crop')`))

	require.NoError(t, store.Migrate(ctx, pool, nil))
	got, err := s.GetSelectedImage(ctx, id, img)
	require.NoError(t, err)
	assert.Zero(t, got.Rotation)
	assert.Nil(t, got.Crop)
}

// The api and worker init containers may migrate at the same moment.
func TestMigrateConcurrentCallersAreSerialised(t *testing.T) {
	pool := pgtest.New(t)
	ctx := context.Background()
	errs := make(chan error, 4)
	for range 4 {
		go func() { errs <- store.Migrate(ctx, pool, nil) }()
	}
	for range 4 {
		require.NoError(t, <-errs)
	}
}
