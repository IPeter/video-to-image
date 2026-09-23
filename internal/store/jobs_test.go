package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

// testPipelineArgs stands in for queue.PipelineArgs (same kind) without importing the
// queue package.
type testPipelineArgs struct {
	JobID string `json:"jobId"`
}

func (testPipelineArgs) Kind() string { return "pipeline" }

func TestCreateJobInsertsJobAndPendingSteps(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()

	in := newJobInput()
	before := time.Now().Add(-time.Minute)
	require.NoError(t, s.CreateJob(ctx, in, nil))

	j, err := s.GetJob(ctx, in.ID)
	require.NoError(t, err)
	assert.Equal(t, in.ID, j.ID)
	assert.Equal(t, "cars", j.DomainKey)
	assert.Equal(t, store.StatusQueued, j.Status)
	assert.True(t, j.ExpiresAt.Equal(in.ExpiresAt), "expires_at %v != %v", j.ExpiresAt, in.ExpiresAt)
	assert.Equal(t, time.UTC, j.CreatedAt.Location())
	assert.True(t, j.CreatedAt.After(before))
	assert.Equal(t, j.CreatedAt, j.UpdatedAt)
	assert.Equal(t, "iphash", j.ClientIPHash)
	assert.Equal(t, in.VideoPath, j.VideoPath)
	assert.Equal(t, testVideoInfo(), j.VideoInfo)
	assert.Equal(t, "gemini-3.5-flash-lite", j.ModelName)
	assert.Nil(t, j.Gate)
	assert.NotNil(t, j.MissingShots)
	assert.Empty(t, j.MissingShots)
	assert.Empty(t, j.ZipPath)
	assert.Empty(t, j.ErrorCode)
	assert.Empty(t, j.ErrorMessage)
	assert.Empty(t, j.InternalLog)
	assert.Zero(t, j.CostUSD)

	steps, err := s.ListSteps(ctx, in.ID)
	require.NoError(t, err)
	require.Len(t, steps, len(store.StepNames))
	for i, st := range steps {
		assert.Equal(t, store.StepNames[i], st.Name)
		assert.Equal(t, in.ID, st.JobID)
		assert.Equal(t, store.StepPending, st.Status)
		assert.Nil(t, st.StartedAt)
		assert.Nil(t, st.FinishedAt)
		assert.Nil(t, st.DurationMs)
		assert.NotNil(t, st.Meta)
		assert.Empty(t, st.Meta)
	}
}

func TestCreateJobEmptyVideoPathStoresNull(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	id := createJob(t, s, withVideoPath(""))
	assert.Equal(t, 1, countRows(t, pool, "SELECT count(*) FROM jobs WHERE id = $1 AND video_path IS NULL", id))
}

func TestCreateJobEnqueueSharesTheTransaction(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()

	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{}) // insert-only
	require.NoError(t, err)

	in := newJobInput()
	var enqueueCalled bool
	err = s.CreateJob(ctx, in, func(ctx context.Context, tx pgx.Tx) error {
		enqueueCalled = true
		// The job row is uncommitted: only the same transaction can see it, so this
		// FK-checked insert succeeds only on the shared tx.
		if _, err := tx.Exec(ctx,
			`INSERT INTO usage_events (job_id, domain_key, step, model) VALUES ($1, 'cars', 'probe', 'm')`, in.ID); err != nil {
			return err
		}
		var steps int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM job_steps WHERE job_id = $1`, in.ID).Scan(&steps); err != nil {
			return err
		}
		if steps != len(store.StepNames) {
			return fmt.Errorf("steps not visible in tx: %d", steps)
		}
		// Nothing is visible outside the transaction yet.
		var outside int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM jobs WHERE id = $1`, in.ID).Scan(&outside); err != nil {
			return err
		}
		if outside != 0 {
			return errors.New("job visible outside the transaction before commit")
		}
		_, err := rc.InsertTx(ctx, tx, testPipelineArgs{JobID: in.ID}, nil)
		return err
	})
	require.NoError(t, err)
	require.True(t, enqueueCalled)

	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM jobs WHERE id = $1`, in.ID))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM usage_events WHERE job_id = $1`, in.ID))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM river_job WHERE kind = 'pipeline' AND args->>'jobId' = $1`, in.ID))
}

func TestCreateJobEnqueueErrorCommitsNothing(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()

	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	require.NoError(t, err)

	boom := errors.New("boom")
	in := newJobInput()
	err = s.CreateJob(ctx, in, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO usage_events (job_id, domain_key, step, model) VALUES ($1, 'cars', 'probe', 'm')`, in.ID); err != nil {
			return err
		}
		if _, err := rc.InsertTx(ctx, tx, testPipelineArgs{JobID: in.ID}, nil); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)

	_, err = s.GetJob(ctx, in.ID)
	require.ErrorIs(t, err, store.ErrNotFound)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM jobs`))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM job_steps`))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM usage_events`))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM river_job`))
}

func TestCreateJobEnqueuePanicRollsBack(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	in := newJobInput()
	require.Panics(t, func() {
		_ = s.CreateJob(context.Background(), in, func(context.Context, pgx.Tx) error { panic("enqueue exploded") })
	})
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM jobs`))
	// The pool is still usable afterwards.
	createJob(t, s)
}

func TestCreateJobRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	for name, mut := range map[string]func(*store.NewJob){
		"empty id":     func(j *store.NewJob) { j.ID = "" },
		"empty domain": func(j *store.NewJob) { j.DomainKey = "" },
		"zero expiry":  func(j *store.NewJob) { j.ExpiresAt = time.Time{} },
	} {
		in := newJobInput(mut)
		assert.Error(t, s.CreateJob(ctx, in, nil), name)
	}
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM jobs`))

	in := newJobInput()
	require.NoError(t, s.CreateJob(ctx, in, nil))
	enqueued := false
	err := s.CreateJob(ctx, in, func(context.Context, pgx.Tx) error { enqueued = true; return nil })
	assert.Error(t, err, "duplicate id")
	assert.False(t, enqueued, "enqueue must not run when the insert fails")
	assert.Equal(t, len(store.StepNames), countRows(t, pool, `SELECT count(*) FROM job_steps`))
}

func TestGetJobNotFound(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	_, err := s.GetJob(context.Background(), ids.NewJob())
	require.ErrorIs(t, err, store.ErrNotFound)
}

func TestJobLifecycleCompleted(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()

	id := createJob(t, s, withModel("configured-model"))
	require.NoError(t, s.StartProcessing(ctx, id))
	require.NoError(t, s.StartProcessing(ctx, id), "re-delivery is a no-op")
	assert.Equal(t, store.StatusProcessing, jobStatus(t, s, id))

	gate := store.Gate{Passed: true, Confidence: 0.93, Reason: "a car walkaround", DetectedSubject: "car"}
	require.NoError(t, s.SetAnalysis(ctx, id, gate, "Red hatchback, exterior and interior.", "gemini-3.5-flash-lite"))
	require.NoError(t, s.ReplaceScenes(ctx, id, []store.Scene{
		{Idx: 0, StartSec: 0, EndSec: 4, ShotID: "front_three_quarter", Relevance: 0.9},
		{Idx: 1, StartSec: 4, EndSec: 9, ShotID: "rear", Relevance: 0.8},
	}))
	candA, candB, candC := ids.NewCandidate(), ids.NewCandidate(), ids.NewCandidate()
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{
		{ID: candA, SceneIdx: 0, ShotID: "front_three_quarter", TimestampSec: 1.5, Score: 0.7},
		{ID: candB, SceneIdx: 1, ShotID: "rear", TimestampSec: 6.25, Score: 0.8},
		{ID: candC, SceneIdx: 1, ShotID: "rear", TimestampSec: 7, Score: 0.2},
	}))

	img1, img2 := ids.NewImage(), ids.NewImage()
	images := []store.SelectedImage{ // deliberately not in idx order
		{ID: img2, CandidateID: candB, Idx: 2, ShotID: "rear", TimestampSec: 6.25, Width: 1920, Height: 1080,
			Reason: "clean rear", FileName: "cars_x_2_rear_00-06.jpg", ImagePath: "cars/jobs/" + id + "/images/" + img2 + ".jpg",
			ThumbPath: "cars/jobs/" + id + "/thumbs/" + img2 + ".jpg", Bytes: 345_678},
		{ID: img1, JobID: id, CandidateID: candA, Idx: 1, ShotID: "front_three_quarter", TimestampSec: 1.5, Width: 1920, Height: 1080,
			Reason: "sharp front", FileName: "cars_x_1_front_00-01.jpg", ImagePath: "cars/jobs/" + id + "/images/" + img1 + ".jpg",
			ThumbPath: "cars/jobs/" + id + "/thumbs/" + img1 + ".jpg", Bytes: 456_789},
	}
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{
		Explanation:  "Two shots found.",
		MissingShots: []string{"interior", "dashboard"},
		ZipPath:      "cars/jobs/" + id + "/export.zip",
		Images:       images,
	}))

	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusCompleted, j.Status)
	require.NotNil(t, j.Gate)
	assert.Equal(t, gate, *j.Gate)
	assert.Equal(t, "Red hatchback, exterior and interior.", j.VideoSummary)
	assert.Equal(t, "gemini-3.5-flash-lite", j.ModelName)
	assert.Equal(t, "Two shots found.", j.ResultExplanation)
	assert.Equal(t, []string{"interior", "dashboard"}, j.MissingShots)
	assert.Equal(t, "cars/jobs/"+id+"/export.zip", j.ZipPath)
	assert.NotEmpty(t, j.VideoPath, "completed jobs keep the original until retention expires")
	assert.False(t, j.UpdatedAt.Before(j.CreatedAt))

	got, err := s.ListSelectedImages(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, 2)
	want1, want2 := images[1], images[0]
	want2.JobID = id
	assert.Equal(t, want1, got[0])
	assert.Equal(t, want2, got[1])

	one, err := s.GetSelectedImage(ctx, id, img2)
	require.NoError(t, err)
	assert.Equal(t, want2, *one)
	_, err = s.GetSelectedImage(ctx, id, ids.NewImage())
	assert.ErrorIs(t, err, store.ErrNotFound)
	other := createJob(t, s)
	_, err = s.GetSelectedImage(ctx, other, img1)
	assert.ErrorIs(t, err, store.ErrNotFound, "image of another job")

	cands, err := s.ListCandidates(ctx, id)
	require.NoError(t, err)
	selected := map[string]bool{}
	for _, c := range cands {
		selected[c.ID] = c.Selected
	}
	assert.Equal(t, map[string]bool{candA: true, candB: true, candC: false}, selected)

	// Completed is terminal.
	err = s.CompleteJob(ctx, id, store.Completion{})
	require.ErrorIs(t, err, store.ErrConflict)
	got, err = s.ListSelectedImages(ctx, id)
	require.NoError(t, err)
	assert.Len(t, got, 2, "a rejected CompleteJob writes nothing")
}

func TestCompleteJobResetsOtherCandidatesAndAllowsNoImages(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	c1, c2 := ids.NewCandidate(), ids.NewCandidate()
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{
		{ID: c1, SceneIdx: -1, ShotID: "front", TimestampSec: 1, Selected: true},
		{ID: c2, SceneIdx: -1, ShotID: "rear", TimestampSec: 2, Selected: true},
	}))
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{Explanation: "nothing usable", MissingShots: nil}))

	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusCompleted, j.Status)
	assert.Empty(t, j.ZipPath)
	assert.Equal(t, []string{}, j.MissingShots)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM jobs WHERE id = $1 AND zip_path IS NULL`, id))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM candidate_frames WHERE job_id = $1 AND selected`, id))
	imgs, err := s.ListSelectedImages(ctx, id)
	require.NoError(t, err)
	assert.NotNil(t, imgs)
	assert.Empty(t, imgs)
}

func TestCompleteJobFailureRollsBackStatus(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	cand := ids.NewCandidate()
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{{ID: cand, SceneIdx: -1, ShotID: "front", TimestampSec: 1}}))

	// The second image references a candidate that does not exist: the FK fails after
	// the status update, and the whole transaction must roll back.
	err := s.CompleteJob(ctx, id, store.Completion{Images: []store.SelectedImage{
		{ID: ids.NewImage(), CandidateID: cand, Idx: 1, ShotID: "front", Width: 10, Height: 10},
		{ID: ids.NewImage(), CandidateID: ids.NewCandidate(), Idx: 2, ShotID: "rear", Width: 10, Height: 10},
	}})
	require.Error(t, err)
	assert.NotErrorIs(t, err, store.ErrConflict)
	assert.Equal(t, store.StatusProcessing, jobStatus(t, s, id))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM selected_images`))
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM candidate_frames WHERE selected`))

	// Input validation happens before anything is written.
	for name, im := range map[string]store.SelectedImage{
		"zero idx":      {ID: ids.NewImage(), Idx: 0, ShotID: "front"},
		"empty id":      {Idx: 1, ShotID: "front"},
		"foreign job":   {ID: ids.NewImage(), JobID: ids.NewJob(), Idx: 1, ShotID: "front"},
		"bytes too big": {ID: ids.NewImage(), Idx: 1, ShotID: "front", Bytes: 1 << 40},
	} {
		err := s.CompleteJob(ctx, id, store.Completion{Images: []store.SelectedImage{im}})
		assert.Error(t, err, name)
		assert.Equal(t, store.StatusProcessing, jobStatus(t, s, id), name)
	}
}

func TestCompleteJobDuplicateIdxFails(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	err := s.CompleteJob(ctx, id, store.Completion{Images: []store.SelectedImage{
		{ID: ids.NewImage(), Idx: 1, ShotID: "front"},
		{ID: ids.NewImage(), Idx: 1, ShotID: "rear"},
	}})
	require.Error(t, err)
	assert.Equal(t, store.StatusProcessing, jobStatus(t, s, id))
}

// TestStatusTransitions drives every transition method from every status.
func TestStatusTransitions(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	gate := store.Gate{Passed: false, Confidence: 0.8, Reason: "not a car", DetectedSubject: "dog"}

	// setup returns a job in the given status, reached through the public API.
	setup := func(t *testing.T, st store.JobStatus) string {
		id := createJob(t, s)
		switch st {
		case store.StatusQueued:
		case store.StatusProcessing:
			require.NoError(t, s.StartProcessing(ctx, id))
		case store.StatusCompleted:
			require.NoError(t, s.StartProcessing(ctx, id))
			require.NoError(t, s.CompleteJob(ctx, id, store.Completion{}))
		case store.StatusRejected:
			require.NoError(t, s.StartProcessing(ctx, id))
			require.NoError(t, s.RejectJob(ctx, id, gate, "a dog"))
		case store.StatusFailed:
			require.NoError(t, s.FailJob(ctx, id, "PROCESSING_ERROR", "failed", ""))
		case store.StatusExpired:
			require.NoError(t, s.ExpireJob(ctx, id))
		}
		require.Equal(t, st, jobStatus(t, s, id))
		return id
	}

	ops := map[string]func(id string) error{
		"StartProcessing": func(id string) error { return s.StartProcessing(ctx, id) },
		"SetAnalysis":     func(id string) error { return s.SetAnalysis(ctx, id, gate, "summary", "m") },
		"CompleteJob":     func(id string) error { return s.CompleteJob(ctx, id, store.Completion{Explanation: "x"}) },
		"RejectJob":       func(id string) error { return s.RejectJob(ctx, id, gate, "summary") },
		"FailJob":         func(id string) error { return s.FailJob(ctx, id, "C", "m", "log") },
		"ExpireJob":       func(id string) error { return s.ExpireJob(ctx, id) },
		"SaveAnalysis": func(id string) error {
			return s.SaveAnalysis(ctx, id, store.AnalysisResult{Gate: gate, Summary: "summary", ModelName: "m"})
		},
		"SaveAnalysisReject": func(id string) error {
			return s.SaveAnalysis(ctx, id, store.AnalysisResult{Gate: gate, Summary: "summary", Reject: true})
		},
	}

	const conflict = store.JobStatus("") // marker: expect ErrConflict, status unchanged
	Q, P, C, R, F, E := store.StatusQueued, store.StatusProcessing, store.StatusCompleted, store.StatusRejected, store.StatusFailed, store.StatusExpired
	// want[op][from] = resulting status, or conflict.
	want := map[string]map[store.JobStatus]store.JobStatus{
		"StartProcessing":    {Q: P, P: P, C: conflict, R: conflict, F: conflict, E: conflict},
		"SetAnalysis":        {Q: conflict, P: P, C: conflict, R: conflict, F: conflict, E: conflict},
		"CompleteJob":        {Q: conflict, P: C, C: conflict, R: conflict, F: conflict, E: conflict},
		"RejectJob":          {Q: conflict, P: R, C: conflict, R: conflict, F: conflict, E: conflict},
		"FailJob":            {Q: F, P: F, C: conflict, R: conflict, F: conflict, E: conflict},
		"ExpireJob":          {Q: E, P: E, C: E, R: E, F: E, E: E},
		"SaveAnalysis":       {Q: conflict, P: P, C: conflict, R: conflict, F: conflict, E: conflict},
		"SaveAnalysisReject": {Q: conflict, P: R, C: conflict, R: conflict, F: conflict, E: conflict},
	}

	for opName, op := range ops {
		for _, from := range []store.JobStatus{Q, P, C, R, F, E} {
			t.Run(opName+"/from_"+string(from), func(t *testing.T) {
				id := setup(t, from)
				before, err := s.GetJob(ctx, id)
				require.NoError(t, err)
				err = op(id)
				expected := want[opName][from]
				after, gerr := s.GetJob(ctx, id)
				require.NoError(t, gerr)
				if expected == conflict {
					require.ErrorIs(t, err, store.ErrConflict)
					assert.NotErrorIs(t, err, store.ErrNotFound)
					assert.Equal(t, *before, *after, "a rejected transition must not modify the job")
					return
				}
				require.NoError(t, err)
				assert.Equal(t, expected, after.Status)
			})
		}
		t.Run(opName+"/missing", func(t *testing.T) {
			require.ErrorIs(t, op(ids.NewJob()), store.ErrNotFound)
		})
	}
}

func TestNoOpTransitionsDoNotTouchTheRow(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	// Backdate updated_at so a write would be visible.
	_, err := pool.Exec(ctx, `UPDATE jobs SET updated_at = now() - interval '1 hour' WHERE id = $1`, id)
	require.NoError(t, err)
	before, err := s.GetJob(ctx, id)
	require.NoError(t, err)

	require.NoError(t, s.StartProcessing(ctx, id))
	after, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, before.UpdatedAt, after.UpdatedAt, "re-delivered StartProcessing is a no-op")

	require.NoError(t, s.ExpireJob(ctx, id))
	expired, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.True(t, expired.UpdatedAt.After(before.UpdatedAt), "ExpireJob bumps updated_at")
	require.NoError(t, s.ExpireJob(ctx, id))
	again, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, expired.UpdatedAt, again.UpdatedAt, "expiring twice is a no-op")
}

func TestSetAnalysisEmptyModelKeepsCreationModel(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s, withModel("from-domain-config"))
	require.NoError(t, s.SetAnalysis(ctx, id, store.Gate{Passed: true}, "s", ""))
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "from-domain-config", j.ModelName)
	require.NotNil(t, j.Gate)
	assert.True(t, j.Gate.Passed)
}

func TestModelTextWithNulAndInvalidUTF8IsStored(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	gate := store.Gate{Passed: true, Confidence: 1, Reason: "nul\x00here", DetectedSubject: "bad\xffbyte"}
	require.NoError(t, s.SetAnalysis(ctx, id, gate, "summary\x00with nul", "m"))
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "summary�with nul", j.VideoSummary)
	assert.Equal(t, "nul�here", j.Gate.Reason)
	assert.Equal(t, "bad�byte", j.Gate.DetectedSubject)
	require.NoError(t, s.FinishStep(ctx, id, store.StepNameAnalyze, store.StepDone, map[string]any{"raw": "a\x00b"}))
}

func TestRejectJobStoresGateAndNullsVideo(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	gate := store.Gate{Passed: false, Confidence: 0.97, Reason: "This video shows a kitchen.", DetectedSubject: "kitchen"}
	require.NoError(t, s.RejectJob(ctx, id, gate, "A kitchen tour."))

	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusRejected, j.Status)
	require.NotNil(t, j.Gate)
	assert.Equal(t, gate, *j.Gate)
	assert.Equal(t, "A kitchen tour.", j.VideoSummary)
	assert.Empty(t, j.VideoPath)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM jobs WHERE id = $1 AND video_path IS NULL`, id))
}

func TestFailJobStoresErrorAndNullsVideo(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()

	id := processingJob(t, s)
	require.NoError(t, s.AppendInternalLog(ctx, id, "attempt 1: 503"))
	require.NoError(t, s.FailJob(ctx, id, "MODEL_UNAVAILABLE", "The model is unavailable, try again later.", "attempt 2: 503"))
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusFailed, j.Status)
	assert.Equal(t, "MODEL_UNAVAILABLE", j.ErrorCode)
	assert.Equal(t, "The model is unavailable, try again later.", j.ErrorMessage)
	assert.Equal(t, "attempt 1: 503\nattempt 2: 503\n", j.InternalLog)
	assert.Empty(t, j.VideoPath)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM jobs WHERE id = $1 AND video_path IS NULL`, id))

	// From queued, with no log and no message.
	q := createJob(t, s)
	require.NoError(t, s.FailJob(ctx, q, "CANNOT_DECODE", "", ""))
	j, err = s.GetJob(ctx, q)
	require.NoError(t, err)
	assert.Equal(t, store.StatusFailed, j.Status)
	assert.Equal(t, "CANNOT_DECODE", j.ErrorCode)
	assert.Empty(t, j.ErrorMessage)
	assert.Empty(t, j.InternalLog)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM jobs WHERE id = $1 AND error_message IS NULL`, q))
}

func TestExpireJobNullsPathsKeepsMetadata(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	require.NoError(t, s.ReplaceScenes(ctx, id, []store.Scene{{Idx: 0, StartSec: 0, EndSec: 3, ShotID: "front", Relevance: 1}}))
	cand := ids.NewCandidate()
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{{ID: cand, SceneIdx: 0, ShotID: "front", TimestampSec: 1}}))
	img := ids.NewImage()
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{
		ZipPath: "cars/jobs/" + id + "/export.zip",
		Images: []store.SelectedImage{{ID: img, CandidateID: cand, Idx: 1, ShotID: "front", TimestampSec: 1, Width: 100, Height: 50,
			FileName: "f.jpg", ImagePath: "cars/jobs/" + id + "/images/" + img + ".jpg", ThumbPath: "cars/jobs/" + id + "/thumbs/" + img + ".jpg", Bytes: 10}},
	}))
	require.NoError(t, s.InsertUsage(ctx, store.UsageEvent{JobID: id, DomainKey: "cars", Step: "analyze", Model: "m", CostUSD: 0.01}))

	require.NoError(t, s.ExpireJob(ctx, id))
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusExpired, j.Status)
	assert.Empty(t, j.VideoPath)
	assert.Empty(t, j.ZipPath)
	assert.Equal(t, 0.01, j.CostUSD)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM jobs WHERE id = $1 AND video_path IS NULL AND zip_path IS NULL`, id))

	imgs, err := s.ListSelectedImages(ctx, id)
	require.NoError(t, err)
	require.Len(t, imgs, 1, "image rows are kept")
	assert.Empty(t, imgs[0].ImagePath)
	assert.Empty(t, imgs[0].ThumbPath)
	assert.Equal(t, "f.jpg", imgs[0].FileName)
	assert.Equal(t, cand, imgs[0].CandidateID)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM selected_images WHERE image_path IS NULL AND thumb_path IS NULL`))

	scenes, err := s.ListScenes(ctx, id)
	require.NoError(t, err)
	assert.Len(t, scenes, 1)
	cands, err := s.ListCandidates(ctx, id)
	require.NoError(t, err)
	assert.Len(t, cands, 1)
	steps, err := s.ListSteps(ctx, id)
	require.NoError(t, err)
	assert.Len(t, steps, len(store.StepNames))

	// Idempotent.
	require.NoError(t, s.ExpireJob(ctx, id))
	assert.Equal(t, store.StatusExpired, jobStatus(t, s, id))
	require.ErrorIs(t, s.ExpireJob(ctx, ids.NewJob()), store.ErrNotFound)
}

func TestExpireJobLeavesOtherJobsAlone(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	a, b := processingJob(t, s), processingJob(t, s)
	for _, id := range []string{a, b} {
		require.NoError(t, s.CompleteJob(ctx, id, store.Completion{ZipPath: id + "/export.zip", Images: []store.SelectedImage{
			{ID: ids.NewImage(), Idx: 1, ShotID: "front", ImagePath: id + "/i.jpg", ThumbPath: id + "/t.jpg"},
		}}))
	}
	require.NoError(t, s.ExpireJob(ctx, a))
	jb, err := s.GetJob(ctx, b)
	require.NoError(t, err)
	assert.Equal(t, store.StatusCompleted, jb.Status)
	assert.Equal(t, b+"/export.zip", jb.ZipPath)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM selected_images WHERE job_id = $1 AND image_path IS NOT NULL`, b))
}

func TestListExpiredJobs(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	oldest := createJob(t, s, withExpiry(now.Add(-3*time.Hour)), withDomain("realestate"))
	middle := createJob(t, s, withExpiry(now.Add(-2*time.Hour)))
	newest := processingJob(t, s, withExpiry(now.Add(-time.Minute)))
	createJob(t, s, withExpiry(now.Add(time.Hour))) // not yet expired
	gone := createJob(t, s, withExpiry(now.Add(-4*time.Hour)))
	require.NoError(t, s.ExpireJob(ctx, gone)) // already expired: excluded

	refs, err := s.ListExpiredJobs(ctx, now, 10)
	require.NoError(t, err)
	assert.Equal(t, []store.JobRef{
		{ID: oldest, DomainKey: "realestate", Status: store.StatusQueued},
		{ID: middle, DomainKey: "cars", Status: store.StatusQueued},
		{ID: newest, DomainKey: "cars", Status: store.StatusProcessing},
	}, refs)

	refs, err = s.ListExpiredJobs(ctx, now, 2)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, oldest, refs[0].ID)
	assert.Equal(t, middle, refs[1].ID)

	refs, err = s.ListExpiredJobs(ctx, now, 0)
	require.NoError(t, err)
	assert.NotNil(t, refs)
	assert.Empty(t, refs)

	// "now" is a parameter: an earlier now sees fewer jobs.
	refs, err = s.ListExpiredJobs(ctx, now.Add(-150*time.Minute), 10)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, oldest, refs[0].ID)

	require.NoError(t, s.ExpireJob(ctx, oldest))
	refs, err = s.ListExpiredJobs(ctx, now, 10)
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, middle, refs[0].ID)
}

func TestAppendInternalLog(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := createJob(t, s)
	readLog := func() string {
		j, err := s.GetJob(ctx, id)
		require.NoError(t, err)
		return j.InternalLog
	}

	require.NoError(t, s.AppendInternalLog(ctx, id, "line1"))
	require.NoError(t, s.AppendInternalLog(ctx, id, "line2\n"))
	require.NoError(t, s.AppendInternalLog(ctx, id, ""))
	assert.Equal(t, "line1\nline2\n", readLog())

	require.NoError(t, s.AppendInternalLog(ctx, id, "nul\x00 and bad \xff byte"))
	assert.Equal(t, "line1\nline2\nnul� and bad � byte\n", readLog())

	require.ErrorIs(t, s.AppendInternalLog(ctx, ids.NewJob(), "x"), store.ErrNotFound)
}

func TestAppendInternalLogKeepsLast64KiB(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	readLog := func(id string) string {
		j, err := s.GetJob(ctx, id)
		require.NoError(t, err)
		return j.InternalLog
	}
	// expectTail is the reference rule: the last MaxInternalLogBytes bytes, starting at
	// a character boundary.
	expectTail := func(full string) string {
		if len(full) <= store.MaxInternalLogBytes {
			return full
		}
		cut := len(full) - store.MaxInternalLogBytes
		for !utf8.RuneStart(full[cut]) {
			cut++
		}
		return full[cut:]
	}

	t.Run("ascii across appends", func(t *testing.T) {
		id := createJob(t, s)
		a := strings.Repeat("a", 40<<10)
		b := strings.Repeat("b", 30<<10) + "END"
		require.NoError(t, s.AppendInternalLog(ctx, id, a))
		require.NoError(t, s.AppendInternalLog(ctx, id, b))
		got := readLog(id)
		assert.Len(t, got, store.MaxInternalLogBytes)
		assert.Equal(t, expectTail(a+"\n"+b+"\n"), got)
		assert.True(t, strings.HasSuffix(got, "END\n"))
	})

	t.Run("single oversized append", func(t *testing.T) {
		id := createJob(t, s)
		big := strings.Repeat("x", 100<<10) + "tail"
		require.NoError(t, s.AppendInternalLog(ctx, id, big))
		got := readLog(id)
		assert.Len(t, got, store.MaxInternalLogBytes)
		assert.True(t, strings.HasSuffix(got, "xtail\n"))
	})

	t.Run("multibyte cut stays valid utf8", func(t *testing.T) {
		for _, prefix := range []string{"", "a", "ab"} { // shift the cut through a 3-byte rune
			id := createJob(t, s)
			first := prefix + strings.Repeat("€", 12_000) // 36 KB
			second := strings.Repeat("€", 12_000) + "ok"
			require.NoError(t, s.AppendInternalLog(ctx, id, first))
			require.NoError(t, s.AppendInternalLog(ctx, id, second))
			got := readLog(id)
			assert.True(t, utf8.ValidString(got))
			assert.LessOrEqual(t, len(got), store.MaxInternalLogBytes)
			assert.GreaterOrEqual(t, len(got), store.MaxInternalLogBytes-3)
			assert.Equal(t, expectTail(first+"\n"+second+"\n"), got, "prefix %q", prefix)
		}
	})

	t.Run("FailJob uses the same cap", func(t *testing.T) {
		id := processingJob(t, s)
		require.NoError(t, s.AppendInternalLog(ctx, id, strings.Repeat("s", 60<<10)))
		stderr := strings.Repeat("e", 10<<10)
		require.NoError(t, s.FailJob(ctx, id, "PROCESSING_ERROR", "Processing failed.", stderr))
		got := readLog(id)
		assert.Len(t, got, store.MaxInternalLogBytes)
		assert.True(t, strings.HasSuffix(got, stderr+"\n"))
	})
}

func TestAppendInternalLogConcurrentAppendsAreNotLost(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := createJob(t, s)
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.AppendInternalLog(ctx, id, fmt.Sprintf("line-%02d", i))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSuffix(j.InternalLog, "\n"), "\n")
	assert.Len(t, lines, n)
	for i := range n {
		assert.Contains(t, lines, fmt.Sprintf("line-%02d", i))
	}
}

func TestCanceledContext(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	in := newJobInput()
	err := s.CreateJob(ctx, in, nil)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM jobs`))
	_, err = s.GetJob(ctx, in.ID)
	require.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, store.ErrNotFound)
}
