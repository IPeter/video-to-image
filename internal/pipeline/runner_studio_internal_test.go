package pipeline

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/pipeline/review"
	"video-to-image/internal/pipeline/studiojob"
	"video-to-image/internal/store"
)

// Unit tests for the studio part of the export step: the subject box in the exported
// image and scheduling the studio job.

func TestExportedBox(t *testing.T) {
	box := review.Box{X0: 0.1, Y0: 0.2, X1: 0.5, Y1: 0.6} // in the frame as stored
	for _, tc := range []struct {
		name string
		e    export.Exported
		want *store.NRect
	}{
		{"copied as is", export.Exported{}, &store.NRect{X0: 0.1, Y0: 0.2, X1: 0.5, Y1: 0.6}},
		// 90° clockwise: stored (x, y) → upright (1-y, x).
		{"rotated", export.Exported{Rotation: 90, Image: export.Image{Edit: &export.Edit{RotateCW: 90}}},
			&store.NRect{X0: 0.4, Y0: 0.1, X1: 0.8, Y1: 0.5}},
		{"rotated and cropped",
			export.Exported{Rotation: 90, Cropped: true, Image: export.Image{Edit: &export.Edit{RotateCW: 90,
				Crop: &export.NRect{X0: 0.2, Y0: 0, X1: 1, Y1: 0.8}}}},
			&store.NRect{X0: 0.25, Y0: 0.125, X1: 0.75, Y1: 0.625}},
		{"cropped only", export.Exported{Cropped: true, Image: export.Image{Edit: &export.Edit{
			Crop: &export.NRect{X0: 0, Y0: 0.1, X1: 0.8, Y1: 0.9}}}},
			&store.NRect{X0: 0.125, Y0: 0.125, X1: 0.625, Y1: 0.625}},
		{"a crop that was not applied is ignored", export.Exported{Image: export.Image{Edit: &export.Edit{
			Crop: &export.NRect{X0: 0.5, Y0: 0.5, X1: 1, Y1: 1}}}},
			&store.NRect{X0: 0.1, Y0: 0.2, X1: 0.5, Y1: 0.6}},
		{"cropped away", export.Exported{Cropped: true, Image: export.Image{Edit: &export.Edit{
			Crop: &export.NRect{X0: 0.6, Y0: 0.6, X1: 1, Y1: 1}}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := exportedBox(box, tc.e)
			if tc.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.InDelta(t, tc.want.X0, got.X0, 1e-6)
			assert.InDelta(t, tc.want.Y0, got.Y0, 1e-6)
			assert.InDelta(t, tc.want.X1, got.X1, 1e-6)
			assert.InDelta(t, tc.want.Y1, got.Y1, 1e-6)
			for _, v := range []float64{got.X0, got.Y0, got.X1, got.Y1} {
				assert.GreaterOrEqual(t, v, 0.0)
				assert.LessOrEqual(t, v, 1.0)
			}
		})
	}

	rn := &run{assessments: map[string]review.Assessment{
		"good": {Box: box},
		"none": {}, // a verdict without a usable box
	}}
	assert.NotNil(t, rn.subjectBox(export.Exported{Image: export.Image{CandidateID: "good"}}))
	assert.Nil(t, rn.subjectBox(export.Exported{Image: export.Image{CandidateID: "none"}}))
	assert.Nil(t, rn.subjectBox(export.Exported{Image: export.Image{CandidateID: "unreviewed"}}))
}

type fakeEnqueuer struct {
	jobs []string
	err  error
}

func (f *fakeEnqueuer) EnqueueStudioTx(_ context.Context, tx pgx.Tx, jobID string) error {
	if tx == nil {
		return errors.New("no transaction")
	}
	f.jobs = append(f.jobs, jobID)
	return f.err
}

func TestPlanStudio(t *testing.T) {
	box := &store.NRect{X0: 0.1, Y0: 0.1, X1: 0.9, Y1: 0.9}
	dom := &domain.Config{Studio: domain.Studio{Enabled: true, Views: []string{"front", "rear"}}}
	images := func() []store.SelectedImage {
		return []store.SelectedImage{
			{ID: "img_1", ShotID: "front", SubjectBox: box},
			{ID: "img_2", ShotID: "interior_dashboard", SubjectBox: box},
			{ID: "img_3", ShotID: "rear"}, // no review box
		}
	}

	enq := &fakeEnqueuer{}
	rn := &run{Runner: &Runner{Studio: enq}, job: &store.Job{ID: "job_1"}, dom: dom}
	c := store.Completion{Images: images()}
	rn.planStudio(&c)
	assert.Equal(t, store.StudioPending, c.Studio)
	require.NotNil(t, c.Enqueue)
	assert.Nil(t, c.Images[0].Studio, "eligible: waits for the studio job")
	assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: studiojob.ReasonNotExterior}, c.Images[1].Studio)
	assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: studiojob.ReasonNoBox}, c.Images[2].Studio)
	require.NoError(t, c.Enqueue(context.Background(), &fakeTx{}))
	assert.Equal(t, []string{"job_1"}, enq.jobs)

	for name, rn := range map[string]*run{
		"no sidecar in this process": {Runner: &Runner{}, job: &store.Job{ID: "job_2"}, dom: dom},
		"studio off for the domain":  {Runner: &Runner{Studio: enq}, job: &store.Job{ID: "job_3"}, dom: &domain.Config{}},
	} {
		c := store.Completion{Images: images()}
		rn.planStudio(&c)
		assert.Empty(t, c.Studio, name)
		assert.Nil(t, c.Enqueue, name)
		for _, im := range c.Images {
			assert.Nil(t, im.Studio, "%s: nothing is recorded", name)
		}
	}
	// No eligible photo: nothing is scheduled.
	c = store.Completion{Images: images()[1:2]}
	rn.planStudio(&c)
	assert.Empty(t, c.Studio)
	assert.Nil(t, c.Enqueue)
	assert.Nil(t, c.Images[0].Studio)
}

// fakeTx satisfies pgx.Tx for the enqueue callback (never used for queries here).
type fakeTx struct{ pgx.Tx }
