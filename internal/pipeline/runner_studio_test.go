package pipeline_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/studiojob"
	"video-to-image/internal/store"
)

// recordingEnqueuer stands in for queue.Client: it records the studio jobs inserted and
// the job's studio status seen inside the completion's transaction.
type recordingEnqueuer struct {
	mu     sync.Mutex
	jobs   []string
	status []string
	err    error
}

func (r *recordingEnqueuer) EnqueueStudioTx(ctx context.Context, tx pgx.Tx, jobID string) error {
	var st string
	if err := tx.QueryRow(ctx, `SELECT studio_status FROM jobs WHERE id = $1`, jobID).Scan(&st); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs = append(r.jobs, jobID)
	r.status = append(r.status, st)
	return r.err
}

// The fake review boxes every frame at box_2d [150, 100, 850, 900] (y0, x0, y1, x1 of
// 1000); the 1280x720 test frames are upright and too small for a 4:3 crop, so the box
// in the exported image is the same box.
var fakeReviewBox = store.NRect{X0: 0.1, Y0: 0.15, X1: 0.9, Y1: 0.85}

func TestRunSchedulesStudioForEligiblePhotos(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	job := e.createJob("car.mp4")
	enq := &recordingEnqueuer{}
	r := e.runner(newFake(t))
	r.Studio = enq

	require.NoError(t, r.Run(ctx, job.ID))

	got, err := e.store.GetJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	assert.Equal(t, store.StudioPending, got.StudioStatus)
	assert.Equal(t, []string{job.ID}, enq.jobs, "the studio job is inserted once")
	assert.Equal(t, []string{"pending"}, enq.status, "in the transaction that completes the job")

	cars, _ := e.reg.Get("cars")
	imgs, err := e.store.ListSelectedImages(ctx, job.ID)
	require.NoError(t, err)
	eligible := 0
	for _, im := range imgs {
		require.NotNil(t, im.SubjectBox, im.ShotID)
		assert.InDelta(t, fakeReviewBox.X0, im.SubjectBox.X0, 1e-6, im.ShotID)
		assert.InDelta(t, fakeReviewBox.Y0, im.SubjectBox.Y0, 1e-6, im.ShotID)
		assert.InDelta(t, fakeReviewBox.X1, im.SubjectBox.X1, 1e-6, im.ShotID)
		assert.InDelta(t, fakeReviewBox.Y1, im.SubjectBox.Y1, 1e-6, im.ShotID)
		if slices.Contains(cars.Studio.Views, im.ShotID) {
			eligible++
			assert.Nil(t, im.Studio, "%s waits for the studio job", im.ShotID)
			continue
		}
		assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: studiojob.ReasonNotExterior}, im.Studio, im.ShotID)
	}
	assert.Positive(t, eligible, "the fake walkaround has exterior views")
}

func TestRunWithoutStudio(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, e *env, enq *recordingEnqueuer) {
		t.Helper()
		job := e.createJob("car.mp4")
		r := e.runner(newFake(t))
		if enq != nil {
			r.Studio = enq
		}
		require.NoError(t, r.Run(ctx, job.ID))
		got, err := e.store.GetJob(ctx, job.ID)
		require.NoError(t, err)
		require.Equal(t, store.StatusCompleted, got.Status)
		assert.Equal(t, store.StudioNone, got.StudioStatus)
		imgs, err := e.store.ListSelectedImages(ctx, job.ID)
		require.NoError(t, err)
		require.NotEmpty(t, imgs)
		for _, im := range imgs {
			assert.Nil(t, im.Studio, "nothing is recorded without studio work")
			assert.NotNil(t, im.SubjectBox, "the subject box is stored either way")
		}
		if enq != nil {
			assert.Empty(t, enq.jobs)
		}
	}
	t.Run("no sidecar in this process", func(t *testing.T) {
		check(t, newEnv(t), nil)
	})
	t.Run("studio off for the domain", func(t *testing.T) {
		e := newEnv(t)
		e.setDomain(func(c *domain.Config) { c.Studio.Enabled = false })
		check(t, e, &recordingEnqueuer{})
	})
	t.Run("no eligible view selected", func(t *testing.T) {
		e := newEnv(t)
		e.setDomain(func(c *domain.Config) { c.Studio.Views = []string{"engine_bay"} })
		check(t, e, &recordingEnqueuer{})
	})
}

// The studio job is inserted with the completion: when that fails, the job is not
// completed either, and River's retry completes it (and schedules the studio job) then.
func TestRunStudioEnqueueFailureRollsBackTheCompletion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	job := e.createJob("car.mp4")
	enq := &recordingEnqueuer{err: errors.New("river: insert failed")}
	r := e.runner(newFake(t))
	r.Studio = enq

	err := r.Run(ctx, job.ID)
	require.ErrorContains(t, err, "river: insert failed")
	got, err := e.store.GetJob(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StatusProcessing, got.Status)
	assert.Equal(t, store.StudioNone, got.StudioStatus)
	imgs, err := e.store.ListSelectedImages(ctx, job.ID)
	require.NoError(t, err)
	assert.Empty(t, imgs)

	enq.err = nil
	require.NoError(t, r.Run(ctx, job.ID))
	got, err = e.store.GetJob(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StatusCompleted, got.Status)
	assert.Equal(t, store.StudioPending, got.StudioStatus)
	assert.Len(t, enq.jobs, 2)
}
