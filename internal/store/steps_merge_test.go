package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

func TestMergeStepMeta(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	other := processingJob(t, s)

	require.NoError(t, s.StartStep(ctx, id, store.StepNameSelect))
	require.NoError(t, s.FinishStep(ctx, id, store.StepNameSelect, store.StepFailed, map[string]any{"keep": "x", "drop": 1, "old": 1}))
	before := stepByName(t, s, id, store.StepNameSelect)

	require.NoError(t, s.MergeStepMeta(ctx, id, store.StepNameSelect, map[string]any{
		"old":  []int{1, 2},
		"new":  map[string]int{"a": 1},
		"drop": nil,
		"gone": nil, // removing an absent key is a no-op
	}))
	st := stepByName(t, s, id, store.StepNameSelect)
	assert.Equal(t, map[string]any{
		"keep": "x",
		"old":  []any{float64(1), float64(2)},
		"new":  map[string]any{"a": float64(1)},
	}, st.Meta)
	assert.Equal(t, store.StepFailed, st.Status, "status and times are unchanged")
	assert.Equal(t, before.FinishedAt, st.FinishedAt)
	assert.Equal(t, before.DurationMs, st.DurationMs)

	// Only removals, and an empty merge.
	require.NoError(t, s.MergeStepMeta(ctx, id, store.StepNameSelect, map[string]any{"new": nil}))
	require.NoError(t, s.MergeStepMeta(ctx, id, store.StepNameSelect, nil))
	assert.Equal(t, map[string]any{"keep": "x", "old": []any{float64(1), float64(2)}}, stepByName(t, s, id, store.StepNameSelect).Meta)

	for _, st := range mustSteps(t, s, other) {
		assert.Empty(t, st.Meta, "other jobs are untouched")
	}

	require.ErrorIs(t, s.MergeStepMeta(ctx, ids.NewJob(), store.StepNameSelect, map[string]any{"a": 1}), store.ErrNotFound)
	require.ErrorIs(t, s.MergeStepMeta(ctx, id, "nope", map[string]any{"a": 1}), store.ErrNotFound)
	assert.Error(t, s.MergeStepMeta(ctx, id, store.StepNameSelect, map[string]any{"bad": func() {}}), "unmarshalable meta")
}
