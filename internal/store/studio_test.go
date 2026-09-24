package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

// testStudioArgs stands in for queue.StudioArgs (same kind and args shape).
type testStudioArgs struct {
	JobID string `json:"jobId"`
}

func (testStudioArgs) Kind() string { return "studio" }

// studioJob completes a job with two images, the first eligible (no studio outcome
// yet) and the second skipped at completion, with the given studio status.
func studioJob(t *testing.T, s *store.Store, status store.StudioStatus) (jobID, eligible, skipped string) {
	t.Helper()
	jobID = processingJob(t, s)
	eligible, skipped = ids.NewImage(), ids.NewImage()
	require.NoError(t, s.CompleteJob(context.Background(), jobID, store.Completion{
		ZipPath: "cars/jobs/" + jobID + "/export.zip",
		Studio:  status,
		Images: []store.SelectedImage{
			{ID: eligible, Idx: 1, ShotID: "front_three_quarter", ImagePath: "i1.jpg", ThumbPath: "t1.jpg",
				SubjectBox: &store.NRect{X0: 0.1, Y0: 0.2, X1: 0.9, Y1: 0.8}},
			{ID: skipped, Idx: 2, ShotID: "interior_dashboard", ImagePath: "i2.jpg", ThumbPath: "t2.jpg",
				Studio: &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: "not an exterior view"}},
		},
	}))
	return jobID, eligible, skipped
}

func imageStudio(t *testing.T, s *store.Store, jobID, imageID string) *store.ImageStudio {
	t.Helper()
	im, err := s.GetSelectedImage(context.Background(), jobID, imageID)
	require.NoError(t, err)
	return im.Studio
}

func readyOutcome(jobID, imageID string) store.ImageStudio {
	prefix := "cars/jobs/" + jobID + "/studio/"
	return store.ImageStudio{
		Status: store.ImageStudioReady,
		Styles: map[store.Style]store.StudioVariant{
			"grey":  {Key: prefix + "grey/" + imageID + ".jpg", ThumbKey: prefix + "grey/thumbs/" + imageID + ".jpg", Width: 1600, Height: 1200, Bytes: 512_000},
			"light": {Key: prefix + "light/" + imageID + ".jpg", ThumbKey: prefix + "light/thumbs/" + imageID + ".jpg", Width: 1600, Height: 1200, Bytes: 498_000},
		},
		Diagnostics: json.RawMessage(`{"stats": {"iou": 0.99}, "qa": {"ok": true}}`),
	}
}

func TestCompleteJobSetsStudioPendingAndEnqueuesInTheSameTransaction(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{}) // insert-only
	require.NoError(t, err)

	id := processingJob(t, s)
	var sawTx bool
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{
		Studio: store.StudioPending,
		Images: []store.SelectedImage{{ID: ids.NewImage(), Idx: 1, ShotID: "front"}},
		Enqueue: func(ctx context.Context, tx pgx.Tx) error {
			sawTx = tx != nil
			// The job row is already updated inside the transaction.
			var studio string
			if err := tx.QueryRow(ctx, `SELECT studio_status FROM jobs WHERE id = $1`, id).Scan(&studio); err != nil {
				return err
			}
			if studio != "pending" {
				return errors.New("studio status not written before Enqueue: " + studio)
			}
			_, err := rc.InsertTx(ctx, tx, testStudioArgs{JobID: id}, nil)
			return err
		},
	}))
	assert.True(t, sawTx)
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusCompleted, j.Status)
	assert.Equal(t, store.StudioPending, j.StudioStatus)
	assert.Empty(t, j.StudioZipPath)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM river_job WHERE kind = 'studio' AND args->>'jobId' = $1`, id))

	// An Enqueue failure rolls the whole completion back.
	other := processingJob(t, s)
	err = s.CompleteJob(ctx, other, store.Completion{
		Studio: store.StudioPending,
		Images: []store.SelectedImage{{ID: ids.NewImage(), Idx: 1, ShotID: "front"}},
		Enqueue: func(ctx context.Context, tx pgx.Tx) error {
			if _, err := rc.InsertTx(ctx, tx, testStudioArgs{JobID: other}, nil); err != nil {
				return err
			}
			return errors.New("river down")
		},
	})
	require.ErrorContains(t, err, "river down")
	j, err = s.GetJob(ctx, other)
	require.NoError(t, err)
	assert.Equal(t, store.StatusProcessing, j.Status)
	assert.Equal(t, store.StudioNone, j.StudioStatus)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM selected_images WHERE job_id = $1`, other))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM river_job WHERE args->>'jobId' = $1`, other))

	// Without Studio the status stays none; only none and pending are allowed.
	plain := processingJob(t, s)
	require.NoError(t, s.CompleteJob(ctx, plain, store.Completion{}))
	j, err = s.GetJob(ctx, plain)
	require.NoError(t, err)
	assert.Equal(t, store.StudioNone, j.StudioStatus)
	bad := processingJob(t, s)
	require.Error(t, s.CompleteJob(ctx, bad, store.Completion{Studio: store.StudioDone}))
	assert.Equal(t, store.StatusProcessing, jobStatus(t, s, bad))
}

func TestSubjectBoxAndStudioRoundTrip(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	jobID, eligible, skipped := studioJob(t, s, store.StudioPending)

	got, err := s.ListSelectedImages(ctx, jobID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, &store.NRect{X0: 0.1, Y0: 0.2, X1: 0.9, Y1: 0.8}, got[0].SubjectBox)
	assert.Nil(t, got[0].Studio, "not processed yet")
	assert.Nil(t, got[1].SubjectBox)
	assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: "not an exterior view"}, got[1].Studio)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM selected_images WHERE id = $1 AND subject_box IS NULL AND studio IS NOT NULL`, skipped))

	require.NoError(t, s.StartStudio(ctx, jobID))
	want := readyOutcome(jobID, eligible)
	require.NoError(t, s.SetImageStudio(ctx, jobID, eligible, want))
	st := imageStudio(t, s, jobID, eligible)
	require.NotNil(t, st)
	assert.Equal(t, want.Status, st.Status)
	assert.Equal(t, want.Styles, st.Styles)
	assert.JSONEq(t, string(want.Diagnostics), string(st.Diagnostics))
	var keys string
	require.NoError(t, pool.QueryRow(ctx, `SELECT string_agg(k, ',' ORDER BY k) FROM selected_images, jsonb_object_keys(studio) AS k WHERE id = $1`,
		eligible).Scan(&keys))
	assert.Equal(t, "diagnostics,status,styles", keys, "empty reason is omitted")

	// Invalid values are refused before anything is written.
	for name, im := range map[string]store.SelectedImage{
		"box outside [0,1]": {ID: ids.NewImage(), Idx: 1, SubjectBox: &store.NRect{X0: -0.1, Y0: 0, X1: 1, Y1: 1}},
		"empty box":         {ID: ids.NewImage(), Idx: 1, SubjectBox: &store.NRect{X0: 0.5, Y0: 0, X1: 0.5, Y1: 1}},
		"unknown status":    {ID: ids.NewImage(), Idx: 1, Studio: &store.ImageStudio{Status: "pending"}},
		"bad diagnostics":   {ID: ids.NewImage(), Idx: 1, Studio: &store.ImageStudio{Status: "failed", Diagnostics: json.RawMessage(`{`)}},
	} {
		id := processingJob(t, s)
		assert.Error(t, s.CompleteJob(ctx, id, store.Completion{Images: []store.SelectedImage{im}}), name)
		assert.Equal(t, store.StatusProcessing, jobStatus(t, s, id), name)
	}
	assert.Error(t, s.SetImageStudio(ctx, jobID, eligible, store.ImageStudio{Status: "maybe"}))
}

func TestStudioLifecycle(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()

	// Nothing requested: the studio job cannot start.
	none, _, _ := studioJob(t, s, store.StudioNone)
	require.ErrorIs(t, s.StartStudio(ctx, none), store.ErrConflict)
	require.ErrorIs(t, s.StartStudio(ctx, "job_missing"), store.ErrNotFound)

	jobID, eligible, skipped := studioJob(t, s, store.StudioPending)
	// Not running yet: outcomes are refused.
	require.ErrorIs(t, s.SetImageStudio(ctx, jobID, eligible, readyOutcome(jobID, eligible)), store.ErrConflict)

	require.NoError(t, s.StartStudio(ctx, jobID))
	require.NoError(t, s.StartStudio(ctx, jobID), "re-delivery: running is a no-op")
	j, err := s.GetJob(ctx, jobID)
	require.NoError(t, err)
	assert.Equal(t, store.StudioRunning, j.StudioStatus)

	require.NoError(t, s.SetImageStudio(ctx, jobID, eligible, readyOutcome(jobID, eligible)))
	require.ErrorIs(t, s.SetImageStudio(ctx, jobID, ids.NewImage(), readyOutcome(jobID, eligible)), store.ErrNotFound)
	require.ErrorIs(t, s.SetImageStudio(ctx, "job_missing", eligible, readyOutcome(jobID, eligible)), store.ErrNotFound)

	require.Error(t, s.FinishStudio(ctx, jobID, store.StudioFinish{Status: store.StudioRunning}), "not a final status")
	zip := "cars/jobs/" + jobID + "/studio.zip"
	require.NoError(t, s.FinishStudio(ctx, jobID, store.StudioFinish{Status: store.StudioDone, ZipPath: zip}))
	j, err = s.GetJob(ctx, jobID)
	require.NoError(t, err)
	assert.Equal(t, store.StatusCompleted, j.Status, "the job's own status is untouched")
	assert.Equal(t, store.StudioDone, j.StudioStatus)
	assert.Equal(t, zip, j.StudioZipPath)
	assert.Equal(t, store.ImageStudioReady, imageStudio(t, s, jobID, eligible).Status)
	assert.Equal(t, store.ImageStudioSkipped, imageStudio(t, s, jobID, skipped).Status)

	// Done is final.
	require.ErrorIs(t, s.StartStudio(ctx, jobID), store.ErrConflict)
	require.ErrorIs(t, s.FinishStudio(ctx, jobID, store.StudioFinish{Status: store.StudioDone}), store.ErrConflict)
	require.ErrorIs(t, s.SetImageStudio(ctx, jobID, eligible, store.ImageStudio{Status: store.ImageStudioFailed}), store.ErrConflict)
	require.ErrorIs(t, s.FailStudio(ctx, jobID, "late"), store.ErrConflict)
	require.ErrorIs(t, s.FinishStudio(ctx, "job_missing", store.StudioFinish{Status: store.StudioDone}), store.ErrNotFound)
}

func TestFinishStudioFillsUnprocessedImages(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	jobID, eligible, skipped := studioJob(t, s, store.StudioPending)
	require.NoError(t, s.StartStudio(ctx, jobID))
	require.NoError(t, s.FinishStudio(ctx, jobID, store.StudioFinish{
		Status:      store.StudioSkipped,
		Unprocessed: &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: "daily budget"},
	}))
	j, err := s.GetJob(ctx, jobID)
	require.NoError(t, err)
	assert.Equal(t, store.StudioSkipped, j.StudioStatus)
	assert.Empty(t, j.StudioZipPath)
	assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: "daily budget"}, imageStudio(t, s, jobID, eligible))
	assert.Equal(t, "not an exterior view", imageStudio(t, s, jobID, skipped).Reason, "decided images keep their outcome")
}

func TestFailStudio(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()

	// Pending (never started) and running both fail; decided images keep their outcome.
	for _, start := range []bool{false, true} {
		jobID, eligible, skipped := studioJob(t, s, store.StudioPending)
		if start {
			require.NoError(t, s.StartStudio(ctx, jobID))
		}
		require.NoError(t, s.FailStudio(ctx, jobID, "The studio background could not be made."))
		j, err := s.GetJob(ctx, jobID)
		require.NoError(t, err)
		assert.Equal(t, store.StudioFailed, j.StudioStatus)
		assert.Equal(t, store.StatusCompleted, j.Status)
		assert.Equal(t, &store.ImageStudio{Status: store.ImageStudioFailed, Reason: "The studio background could not be made."},
			imageStudio(t, s, jobID, eligible))
		assert.Equal(t, store.ImageStudioSkipped, imageStudio(t, s, jobID, skipped).Status)
		require.ErrorIs(t, s.FailStudio(ctx, jobID, "again"), store.ErrConflict)
	}
	none, _, _ := studioJob(t, s, store.StudioNone)
	require.ErrorIs(t, s.FailStudio(ctx, none, "x"), store.ErrConflict)
	require.ErrorIs(t, s.FailStudio(ctx, "job_missing", "x"), store.ErrNotFound)
}

func TestExpireJobEndsStudioWork(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	jobID, eligible, skipped := studioJob(t, s, store.StudioPending)
	require.NoError(t, s.StartStudio(ctx, jobID))
	require.NoError(t, s.SetImageStudio(ctx, jobID, eligible, readyOutcome(jobID, eligible)))

	// A finished job with a studio.zip loses the key on expiry too.
	done, doneImg, _ := studioJob(t, s, store.StudioPending)
	require.NoError(t, s.StartStudio(ctx, done))
	require.NoError(t, s.SetImageStudio(ctx, done, doneImg, readyOutcome(done, doneImg)))
	require.NoError(t, s.FinishStudio(ctx, done, store.StudioFinish{Status: store.StudioDone, ZipPath: "cars/jobs/" + done + "/studio.zip"}))

	for _, id := range []string{jobID, done} {
		require.NoError(t, s.ExpireJob(ctx, id))
	}
	j, err := s.GetJob(ctx, jobID)
	require.NoError(t, err)
	assert.Equal(t, store.StatusExpired, j.Status)
	assert.Equal(t, store.StudioSkipped, j.StudioStatus, "running studio work ends as skipped")
	st := imageStudio(t, s, jobID, eligible)
	require.NotNil(t, st)
	assert.Equal(t, store.ImageStudioReady, st.Status, "the outcome is kept as metadata")
	assert.Nil(t, st.Styles, "the variants' storage keys go with the files")
	assert.NotEmpty(t, st.Diagnostics)
	assert.Equal(t, store.ImageStudioSkipped, imageStudio(t, s, jobID, skipped).Status)

	j, err = s.GetJob(ctx, done)
	require.NoError(t, err)
	assert.Equal(t, store.StudioDone, j.StudioStatus, "finished studio work keeps its status")
	assert.Empty(t, j.StudioZipPath)
	assert.Nil(t, imageStudio(t, s, done, doneImg).Styles)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM selected_images WHERE job_id IN ($1, $2) AND studio -> 'styles' IS NOT NULL`, jobID, done))

	// The studio worker cannot write anything any more.
	require.ErrorIs(t, s.StartStudio(ctx, jobID), store.ErrConflict)
	require.ErrorIs(t, s.SetImageStudio(ctx, jobID, eligible, readyOutcome(jobID, eligible)), store.ErrConflict)
	require.ErrorIs(t, s.FinishStudio(ctx, jobID, store.StudioFinish{Status: store.StudioDone}), store.ErrConflict)

	// Retention lists the studio status (the second sweep depends on it).
	pendingExpired, _, _ := studioJob(t, s, store.StudioPending)
	_, err = pool.Exec(ctx, `UPDATE jobs SET expires_at = now() - interval '1 hour' WHERE id = $1`, pendingExpired)
	require.NoError(t, err)
	refs, err := s.ListExpiredJobs(ctx, time.Now(), 10)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, store.JobRef{ID: pendingExpired, DomainKey: "cars", Status: store.StatusCompleted, StudioStatus: store.StudioPending}, refs[0])
}

func TestListOrphanedStudioJobs(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{}) // insert-only
	require.NoError(t, err)

	// job completes a job with studio work pending (running when start), its River studio
	// jobs in the given states ("" = none), last updated ago before now.
	job := func(ago time.Duration, start bool, states ...string) string {
		t.Helper()
		id, _, _ := studioJob(t, s, store.StudioPending)
		if start {
			require.NoError(t, s.StartStudio(ctx, id))
		}
		for _, st := range states {
			res, err := rc.Insert(ctx, testStudioArgs{JobID: id}, nil)
			require.NoError(t, err)
			if st != "available" {
				setRiverState(t, pool, res.Job.ID, st)
			}
		}
		backdate(t, pool, id, ago)
		return id
	}
	const old = 2 * time.Hour
	var (
		available  = job(old, false, "available")
		running    = job(old, true, "running")
		retryable  = job(old, true, "retryable")
		reEnqueued = job(old, true, "discarded", "available")

		discarded = job(old+3*time.Minute, true, "discarded")
		missing   = job(old+2*time.Minute, false) // no River job at all
		cancelled = job(old+time.Minute, false, "cancelled")
		recent    = job(time.Minute, true, "discarded")
	)
	// Pipeline work for the job does not count as studio work.
	pipelineOnly := job(old-time.Minute, false)
	_, err = rc.Insert(ctx, testPipelineArgs{JobID: pipelineOnly}, nil)
	require.NoError(t, err)
	// Jobs whose studio work is not pending or running are never orphaned.
	for _, st := range []store.StudioStatus{store.StudioNone, store.StudioDone, store.StudioFailed} {
		initial := store.StudioPending
		if st == store.StudioNone {
			initial = store.StudioNone
		}
		id, _, _ := studioJob(t, s, initial)
		switch st {
		case store.StudioDone:
			require.NoError(t, s.StartStudio(ctx, id))
			require.NoError(t, s.FinishStudio(ctx, id, store.StudioFinish{Status: store.StudioDone}))
		case store.StudioFailed:
			require.NoError(t, s.FailStudio(ctx, id, "x"))
		}
		backdate(t, pool, id, 3*old)
	}

	got, err := s.ListOrphanedStudioJobs(ctx, time.Now().Add(-10*time.Minute), 100)
	require.NoError(t, err)
	var gotIDs []string
	for _, r := range got {
		gotIDs = append(gotIDs, r.ID)
		assert.Equal(t, "cars", r.DomainKey)
		assert.Equal(t, store.StatusCompleted, r.Status)
	}
	assert.Equal(t, []string{discarded, missing, cancelled, pipelineOnly}, gotIDs, "oldest update first")
	assert.Equal(t, store.StudioRunning, got[0].StudioStatus)
	assert.Equal(t, store.StudioPending, got[1].StudioStatus)
	for _, id := range []string{available, running, retryable, reEnqueued, recent} {
		assert.NotContains(t, gotIDs, id)
	}

	got, err = s.ListOrphanedStudioJobs(ctx, time.Now().Add(-10*time.Minute), 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, discarded, got[0].ID)
	got, err = s.ListOrphanedStudioJobs(ctx, time.Now(), 0)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}
