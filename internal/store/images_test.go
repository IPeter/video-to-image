package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

func TestSelectedImageRotationAndCropRoundTrip(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)

	upright, rotated, both := ids.NewImage(), ids.NewImage(), ids.NewImage()
	images := []store.SelectedImage{
		{ID: upright, Idx: 1, ShotID: "front_three_quarter", TimestampSec: 1, Width: 1920, Height: 1440, FileName: "a.jpg"},
		{ID: rotated, Idx: 2, ShotID: "rear_three_quarter", TimestampSec: 11, Width: 1440, Height: 1920, FileName: "b.jpg",
			Rotation: 90},
		{ID: both, Idx: 3, ShotID: "front_seats", TimestampSec: 39, Width: 1600, Height: 1200, FileName: "c.jpg",
			Rotation: 270, Crop: &store.NRect{X0: 0.1, Y0: 0.125, X1: 0.9, Y1: 0.8}},
	}
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{Images: images}))

	got, err := s.ListSelectedImages(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i := range images {
		want := images[i]
		want.JobID = id
		assert.Equal(t, want, got[i], want.ID)
	}
	assert.Nil(t, got[0].Crop)
	assert.Zero(t, got[0].Rotation)

	one, err := s.GetSelectedImage(ctx, id, both)
	require.NoError(t, err)
	assert.Equal(t, 270, one.Rotation)
	require.NotNil(t, one.Crop)
	assert.Equal(t, store.NRect{X0: 0.1, Y0: 0.125, X1: 0.9, Y1: 0.8}, *one.Crop)

	// Stored shape: NULL when uncropped, a {x0,y0,x1,y1} object otherwise.
	assert.Equal(t, 2, countRows(t, pool, `SELECT count(*) FROM selected_images WHERE job_id = $1 AND crop IS NULL`, id))
	var x0, y1 float64
	var keys string
	require.NoError(t, pool.QueryRow(ctx, `SELECT (crop->>'x0')::float8, (crop->>'y1')::float8,
		(SELECT string_agg(k, ',' ORDER BY k) FROM jsonb_object_keys(crop) AS k)
		FROM selected_images WHERE id = $1`, both).Scan(&x0, &y1, &keys))
	assert.Equal(t, 0.1, x0)
	assert.Equal(t, 0.8, y1)
	assert.Equal(t, "x0,x1,y0,y1", keys)
	var rotations []int
	rows, err := pool.Query(ctx, `SELECT rotation FROM selected_images WHERE job_id = $1 ORDER BY idx`, id)
	require.NoError(t, err)
	for rows.Next() {
		var r int
		require.NoError(t, rows.Scan(&r))
		rotations = append(rotations, r)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []int{0, 90, 270}, rotations)

	// Expiry keeps the edit metadata.
	require.NoError(t, s.ExpireJob(ctx, id))
	one, err = s.GetSelectedImage(ctx, id, both)
	require.NoError(t, err)
	assert.Equal(t, 270, one.Rotation)
	assert.NotNil(t, one.Crop)
}

func TestSelectedImageJSONNullCropReadsAsNil(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	img := ids.NewImage()
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{Images: []store.SelectedImage{
		{ID: img, Idx: 1, ShotID: "front", Crop: &store.NRect{X0: 0, Y0: 0, X1: 0.5, Y1: 0.5}},
	}}))
	_, err := pool.Exec(ctx, `UPDATE selected_images SET crop = 'null'::jsonb WHERE id = $1`, img)
	require.NoError(t, err)
	got, err := s.GetSelectedImage(ctx, id, img)
	require.NoError(t, err)
	assert.Nil(t, got.Crop)

	// A crop that is not an object is reported, not silently dropped.
	_, err = pool.Exec(ctx, `UPDATE selected_images SET crop = '[0, 0, 1, 1]'::jsonb WHERE id = $1`, img)
	require.NoError(t, err)
	_, err = s.GetSelectedImage(ctx, id, img)
	assert.Error(t, err)
	_, err = s.ListSelectedImages(ctx, id)
	assert.Error(t, err)
}

func TestCompleteJobRejectsInvalidEdits(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	crop := func(x0, y0, x1, y1 float64) *store.NRect { return &store.NRect{X0: x0, Y0: y0, X1: x1, Y1: y1} }
	for name, im := range map[string]store.SelectedImage{
		"rotation 45":       {Rotation: 45},
		"rotation -90":      {Rotation: -90},
		"rotation 360":      {Rotation: 360},
		"crop beyond 1":     {Crop: crop(0, 0, 1.5, 1)},
		"crop below 0":      {Crop: crop(-0.1, 0, 1, 1)},
		"crop inverted":     {Crop: crop(0.8, 0, 0.2, 1)},
		"crop empty height": {Crop: crop(0, 0.5, 1, 0.5)},
		"crop NaN":          {Crop: crop(0, 0, math.NaN(), 1)},
		"crop +Inf":         {Crop: crop(0, 0, 1, math.Inf(1))},
	} {
		im.ID, im.Idx, im.ShotID = ids.NewImage(), 1, "front"
		err := s.CompleteJob(ctx, id, store.Completion{Images: []store.SelectedImage{im}})
		assert.Error(t, err, name)
		assert.Equal(t, store.StatusProcessing, jobStatus(t, s, id), name)
	}
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM selected_images`))

	// The full frame and every quarter turn are accepted.
	var images []store.SelectedImage
	for i, rot := range []int{0, 90, 180, 270} {
		images = append(images, store.SelectedImage{ID: ids.NewImage(), Idx: i + 1, ShotID: "front", Rotation: rot, Crop: crop(0, 0, 1, 1)})
	}
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{Images: images}))
	assert.Equal(t, 4, countRows(t, pool, `SELECT count(*) FROM selected_images WHERE job_id = $1 AND crop IS NOT NULL`, id))
}
