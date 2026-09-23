package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/store"
)

// otherKindArgs has the pipeline's args shape but another kind.
type otherKindArgs struct {
	JobID string `json:"jobId"`
}

func (otherKindArgs) Kind() string { return "not_pipeline" }

// setRiverState moves a River job to state the way River would (finalized_at is set for
// the finalized states, as River's check constraint requires).
func setRiverState(t *testing.T, pool *pgxpool.Pool, riverID int64, state string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		UPDATE river_job
		SET state = $2::river_job_state,
		    finalized_at = CASE WHEN $2 IN ('cancelled', 'completed', 'discarded') THEN now() END
		WHERE id = $1`, riverID, state)
	require.NoError(t, err)
}

// backdate sets the job's updated_at to ago before now.
func backdate(t *testing.T, pool *pgxpool.Pool, id string, ago time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE jobs SET updated_at = now() - make_interval(secs => $2) WHERE id = $1`, id, ago.Seconds())
	require.NoError(t, err)
}

func TestListOrphanedJobs(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{}) // insert-only
	require.NoError(t, err)

	enqueue := func(id, state string) {
		t.Helper()
		res, err := rc.Insert(ctx, testPipelineArgs{JobID: id}, nil)
		require.NoError(t, err)
		if state != "available" {
			setRiverState(t, pool, res.Job.ID, state)
		}
	}
	// job creates a processing job whose pipeline work is in the given River states
	// ("" = never enqueued) and was last updated ago before now.
	job := func(ago time.Duration, states ...string) string {
		t.Helper()
		id := processingJob(t, s)
		for _, st := range states {
			enqueue(id, st)
		}
		backdate(t, pool, id, ago)
		return id
	}

	const old = 2 * time.Hour
	var (
		available = job(old, "available")
		running   = job(old, "running")
		retryable = job(old, "retryable")
		scheduled = job(old, "scheduled")
		pending   = job(old, "pending")
		// A discarded attempt followed by a fresh active one is not orphaned.
		reEnqueued = job(old, "discarded", "available")

		discarded = job(old+4*time.Minute, "discarded")
		cancelled = job(old+3*time.Minute, "cancelled")
		completed = job(old+2*time.Minute, "completed") // River finished but the job never became terminal
		missing   = job(old + 1*time.Minute)            // no River job at all (deleted)
		recent    = job(time.Minute)                    // within the grace period
	)

	// A queued job whose River job is gone is orphaned too.
	queued := createJob(t, s)
	backdate(t, pool, queued, old+5*time.Minute)

	// Active work of another kind, or for another job, does not count.
	otherKind := job(old - time.Minute)
	_, err = rc.Insert(ctx, otherKindArgs{JobID: otherKind}, nil)
	require.NoError(t, err)

	// Terminal jobs are never orphaned, whatever River says.
	for _, st := range []store.JobStatus{store.StatusCompleted, store.StatusRejected, store.StatusFailed, store.StatusExpired} {
		id := processingJob(t, s)
		switch st {
		case store.StatusCompleted:
			require.NoError(t, s.CompleteJob(ctx, id, store.Completion{}))
		case store.StatusRejected:
			require.NoError(t, s.RejectJob(ctx, id, store.Gate{}, ""))
		case store.StatusFailed:
			require.NoError(t, s.FailJob(ctx, id, "PROCESSING_ERROR", "m", ""))
		case store.StatusExpired:
			require.NoError(t, s.ExpireJob(ctx, id))
		}
		enqueue(id, "discarded")
		backdate(t, pool, id, 3*old)
	}

	idleSince := time.Now().Add(-10 * time.Minute)
	got, err := s.ListOrphanedJobs(ctx, idleSince, 100)
	require.NoError(t, err)
	var gotIDs []string
	for _, r := range got {
		gotIDs = append(gotIDs, r.ID)
		assert.Equal(t, "cars", r.DomainKey)
	}
	// Oldest update first.
	assert.Equal(t, []string{queued, discarded, cancelled, completed, missing, otherKind}, gotIDs)
	assert.Equal(t, store.StatusQueued, got[0].Status)
	assert.Equal(t, store.StatusProcessing, got[1].Status)
	for _, id := range []string{available, running, retryable, scheduled, pending, reEnqueued, recent} {
		assert.NotContains(t, gotIDs, id)
	}

	// The limit keeps the oldest.
	got, err = s.ListOrphanedJobs(ctx, idleSince, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, queued, got[0].ID)
	assert.Equal(t, discarded, got[1].ID)

	// idleSince in the future includes the recent job.
	got, err = s.ListOrphanedJobs(ctx, time.Now().Add(time.Minute), 100)
	require.NoError(t, err)
	gotIDs = gotIDs[:0]
	for _, r := range got {
		gotIDs = append(gotIDs, r.ID)
	}
	assert.Contains(t, gotIDs, recent)

	got, err = s.ListOrphanedJobs(ctx, idleSince, 0)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}
