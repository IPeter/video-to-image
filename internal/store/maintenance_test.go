package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/store"
)

func TestTableRowEstimates(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()

	est, err := s.TableRowEstimates(ctx)
	require.NoError(t, err)
	keys := make([]string, 0, len(est))
	for k := range est {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"jobs", "job_steps", "scenes", "candidate_frames", "selected_images", "usage_events"}, keys)
	assert.ElementsMatch(t, store.RowEstimateTables, keys)
	for k, v := range est {
		assert.Zero(t, v, "%s: never analyzed (reltuples -1) reports 0", k)
	}

	for range 3 {
		createJob(t, s)
	}
	_, err = pool.Exec(ctx, `ANALYZE jobs, job_steps`)
	require.NoError(t, err)
	est, err = s.TableRowEstimates(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(3), est["jobs"])
	assert.Equal(t, int64(3*len(store.StepNames)), est["job_steps"])
	assert.Zero(t, est["usage_events"])
}

func TestCountJobsByStatus(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()

	counts, err := s.CountJobsByStatus(ctx)
	require.NoError(t, err)
	assert.Len(t, counts, len(store.JobStatuses))
	for st, n := range counts {
		assert.Zero(t, n, st)
	}

	createJob(t, s)
	createJob(t, s)
	processingJob(t, s)
	failed := createJob(t, s)
	require.NoError(t, s.FailJob(ctx, failed, "X", "x", ""))

	counts, err = s.CountJobsByStatus(ctx)
	require.NoError(t, err)
	assert.Equal(t, map[store.JobStatus]int64{
		store.StatusQueued:     2,
		store.StatusProcessing: 1,
		store.StatusCompleted:  0,
		store.StatusRejected:   0,
		store.StatusFailed:     1,
		store.StatusExpired:    0,
	}, counts)
}
