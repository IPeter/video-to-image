package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

func stepByName(t *testing.T, s *store.Store, jobID, name string) store.Step {
	t.Helper()
	steps, err := s.ListSteps(context.Background(), jobID)
	require.NoError(t, err)
	for _, st := range steps {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("step %s not found", name)
	return store.Step{}
}

func TestStepStartFinishDuration(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)

	require.NoError(t, s.StartStep(ctx, id, store.StepNamePreview))
	st := stepByName(t, s, id, store.StepNamePreview)
	assert.Equal(t, store.StepRunning, st.Status)
	require.NotNil(t, st.StartedAt)
	assert.Nil(t, st.FinishedAt)
	assert.Nil(t, st.DurationMs)

	// Backdate the start so the duration is deterministic.
	_, err := pool.Exec(ctx, `UPDATE job_steps SET started_at = now() - interval '1500 milliseconds' WHERE job_id = $1 AND name = $2`,
		id, store.StepNamePreview)
	require.NoError(t, err)

	type previewMeta struct {
		Width  int    `json:"width"`
		Reused bool   `json:"reused"`
		Codec  string `json:"codec"`
	}
	require.NoError(t, s.FinishStep(ctx, id, store.StepNamePreview, store.StepDone, previewMeta{Width: 640, Reused: false, Codec: "h264"}))
	st = stepByName(t, s, id, store.StepNamePreview)
	assert.Equal(t, store.StepDone, st.Status)
	require.NotNil(t, st.FinishedAt)
	require.NotNil(t, st.DurationMs)
	assert.GreaterOrEqual(t, *st.DurationMs, 1500)
	assert.Less(t, *st.DurationMs, 60_000)
	assert.False(t, st.FinishedAt.Before(*st.StartedAt))
	assert.Equal(t, map[string]any{"width": float64(640), "reused": false, "codec": "h264"}, st.Meta)

	// Restarting a step clears the finish fields and keeps the meta.
	require.NoError(t, s.StartStep(ctx, id, store.StepNamePreview))
	st = stepByName(t, s, id, store.StepNamePreview)
	assert.Equal(t, store.StepRunning, st.Status)
	assert.Nil(t, st.FinishedAt)
	assert.Nil(t, st.DurationMs)

	// Finishing a step that never started: no duration; nil meta → {}.
	require.NoError(t, s.FinishStep(ctx, id, store.StepNameAnalyze, store.StepSkipped, nil))
	st = stepByName(t, s, id, store.StepNameAnalyze)
	assert.Equal(t, store.StepSkipped, st.Status)
	assert.Nil(t, st.StartedAt)
	require.NotNil(t, st.FinishedAt)
	assert.Nil(t, st.DurationMs)
	assert.Equal(t, map[string]any{}, st.Meta)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM job_steps WHERE job_id = $1 AND name = $2 AND meta = '{}'::jsonb`,
		id, store.StepNameAnalyze))

	require.NoError(t, s.StartStep(ctx, id, store.StepNameExtract))
	require.NoError(t, s.FinishStep(ctx, id, store.StepNameExtract, store.StepFailed, map[string]any{"error": "ffmpeg exit 1"}))
	st = stepByName(t, s, id, store.StepNameExtract)
	assert.Equal(t, store.StepFailed, st.Status)
	require.NotNil(t, st.DurationMs)
	assert.GreaterOrEqual(t, *st.DurationMs, 0)
	assert.Equal(t, "ffmpeg exit 1", st.Meta["error"])
}

func TestFinishStepRejectsBadInput(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := createJob(t, s)
	for _, st := range []store.StepStatus{store.StepPending, store.StepRunning, "bogus"} {
		assert.Error(t, s.FinishStep(ctx, id, store.StepNameScore, st, nil), st)
	}
	assert.Error(t, s.FinishStep(ctx, id, store.StepNameScore, store.StepDone, []int{1, 2}), "meta must be an object")
	assert.Error(t, s.FinishStep(ctx, id, "", store.StepDone, nil))
	assert.Error(t, s.StartStep(ctx, id, ""))
	assert.Equal(t, store.StepPending, stepByName(t, s, id, store.StepNameScore).Status)
}

func TestStepsOfMissingJob(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	missing := ids.NewJob()
	require.ErrorIs(t, s.StartStep(ctx, missing, store.StepNamePreview), store.ErrNotFound)
	require.ErrorIs(t, s.FinishStep(ctx, missing, store.StepNamePreview, store.StepDone, nil), store.ErrNotFound)
	require.NoError(t, s.SkipRemainingSteps(ctx, missing))
	steps, err := s.ListSteps(ctx, missing)
	require.NoError(t, err)
	assert.NotNil(t, steps)
	assert.Empty(t, steps)
}

func TestStepNotInStepNamesIsCreatedAndListedLast(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := createJob(t, s)
	require.NoError(t, s.StartStep(ctx, id, "rerank"))
	require.NoError(t, s.FinishStep(ctx, id, "rerank", store.StepDone, map[string]int{"calls": 1}))
	steps, err := s.ListSteps(ctx, id)
	require.NoError(t, err)
	require.Len(t, steps, len(store.StepNames)+1)
	for i, name := range store.StepNames {
		assert.Equal(t, name, steps[i].Name)
	}
	last := steps[len(steps)-1]
	assert.Equal(t, "rerank", last.Name)
	assert.Equal(t, store.StepDone, last.Status)
	assert.NotNil(t, last.DurationMs)
}

func TestSkipRemainingSteps(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	other := processingJob(t, s)

	require.NoError(t, s.StartStep(ctx, id, store.StepNamePreview))
	require.NoError(t, s.FinishStep(ctx, id, store.StepNamePreview, store.StepDone, nil))
	require.NoError(t, s.StartStep(ctx, id, store.StepNameAnalyze))
	require.NoError(t, s.FinishStep(ctx, id, store.StepNameAnalyze, store.StepFailed, nil))
	require.NoError(t, s.StartStep(ctx, id, store.StepNameExtract)) // running

	require.NoError(t, s.SkipRemainingSteps(ctx, id))
	steps, err := s.ListSteps(ctx, id)
	require.NoError(t, err)
	got := map[string]store.StepStatus{}
	for _, st := range steps {
		got[st.Name] = st.Status
	}
	assert.Equal(t, map[string]store.StepStatus{
		store.StepNamePreview: store.StepDone,
		store.StepNameAnalyze: store.StepFailed,
		store.StepNameExtract: store.StepSkipped,
		store.StepNameScore:   store.StepSkipped,
		store.StepNameSelect:  store.StepSkipped,
		store.StepNameExport:  store.StepSkipped,
	}, got)

	extract := stepByName(t, s, id, store.StepNameExtract)
	require.NotNil(t, extract.FinishedAt, "an interrupted step gets a finish time")
	require.NotNil(t, extract.DurationMs, "and a duration, since it had started")
	score := stepByName(t, s, id, store.StepNameScore)
	assert.NotNil(t, score.FinishedAt)
	assert.Nil(t, score.DurationMs, "never started")

	for _, st := range mustSteps(t, s, other) {
		assert.Equal(t, store.StepPending, st.Status, "other jobs are untouched")
	}
}

func mustSteps(t *testing.T, s *store.Store, id string) []store.Step {
	t.Helper()
	steps, err := s.ListSteps(context.Background(), id)
	require.NoError(t, err)
	return steps
}
