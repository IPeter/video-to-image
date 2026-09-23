package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

func TestReplaceScenesAndList(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)

	in := []store.Scene{ // out of idx order on purpose
		{Idx: 2, StartSec: 20.5, EndSec: 31.25, ShotID: "interior", Relevance: 0.6, Reason: "cabin", RawStart: "00:20.5", RawEnd: "00:31"},
		{Idx: 0, JobID: id, StartSec: 0, EndSec: 4.2, ShotID: "front_three_quarter", Relevance: 0.95, Reason: "front", RawStart: "0:00", RawEnd: "0:04.2"},
		{Idx: 1, StartSec: 4.2, EndSec: 12.3, ShotID: "side", Relevance: 0.85, Reason: "side", RawStart: "4.2", RawEnd: "12.3"},
	}
	require.NoError(t, s.ReplaceScenes(ctx, id, in))

	got, err := s.ListScenes(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, 3)
	for i, want := range []store.Scene{in[1], in[2], in[0]} {
		assert.NotZero(t, got[i].ID)
		want.ID = got[i].ID
		want.JobID = id
		assert.Equal(t, want, got[i])
	}

	// Replacing drops the old scenes and every candidate of the job, including ones
	// without a scene.
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{
		{ID: ids.NewCandidate(), SceneIdx: 0, ShotID: "front_three_quarter", TimestampSec: 1},
		{ID: ids.NewCandidate(), SceneIdx: -1, ShotID: "side", TimestampSec: 5},
	}))
	require.NoError(t, s.ReplaceScenes(ctx, id, []store.Scene{{Idx: 0, StartSec: 1, EndSec: 2, ShotID: "rear", Relevance: 0.5}}))
	got2, err := s.ListScenes(ctx, id)
	require.NoError(t, err)
	require.Len(t, got2, 1)
	assert.Equal(t, "rear", got2[0].ShotID)
	for _, old := range got {
		assert.NotEqual(t, old.ID, got2[0].ID)
	}
	cands, err := s.ListCandidates(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, cands)

	// Empty input clears.
	require.NoError(t, s.ReplaceScenes(ctx, id, nil))
	got3, err := s.ListScenes(ctx, id)
	require.NoError(t, err)
	assert.NotNil(t, got3)
	assert.Empty(t, got3)
}

func TestReplaceScenesCascadeDeletesCandidates(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	require.NoError(t, s.ReplaceScenes(ctx, id, []store.Scene{{Idx: 0, StartSec: 0, EndSec: 1, ShotID: "front", Relevance: 1}}))
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{{ID: ids.NewCandidate(), SceneIdx: 0, ShotID: "front", TimestampSec: 0.5}}))
	// The schema's ON DELETE CASCADE also holds for deletes outside the store.
	_, err := pool.Exec(ctx, `DELETE FROM scenes WHERE job_id = $1`, id)
	require.NoError(t, err)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM candidate_frames WHERE job_id = $1`, id))
}

func TestReplaceScenesFailureKeepsPreviousScenes(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	require.NoError(t, s.ReplaceScenes(ctx, id, []store.Scene{{Idx: 0, StartSec: 0, EndSec: 1, ShotID: "front", Relevance: 1}}))
	err := s.ReplaceScenes(ctx, id, []store.Scene{
		{Idx: 3, StartSec: 0, EndSec: 1, ShotID: "a", Relevance: 1},
		{Idx: 3, StartSec: 1, EndSec: 2, ShotID: "b", Relevance: 1}, // duplicate (job_id, idx)
	})
	require.Error(t, err)
	got, err := s.ListScenes(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "front", got[0].ShotID)

	assert.Error(t, s.ReplaceScenes(ctx, id, []store.Scene{{Idx: 0, JobID: ids.NewJob(), ShotID: "x"}}), "foreign job id")
	require.ErrorIs(t, s.ReplaceScenes(ctx, ids.NewJob(), []store.Scene{{Idx: 0, ShotID: "x"}}), store.ErrNotFound)
}

func TestReplaceCandidatesRoundTrip(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	require.NoError(t, s.ReplaceScenes(ctx, id, []store.Scene{
		{Idx: 0, StartSec: 0, EndSec: 4, ShotID: "front", Relevance: 1},
		{Idx: 1, StartSec: 4, EndSec: 8, ShotID: "rear", Relevance: 1},
	}))
	scenes, err := s.ListScenes(ctx, id)
	require.NoError(t, err)
	sceneID := map[int]int64{scenes[0].Idx: scenes[0].ID, scenes[1].Idx: scenes[1].ID}

	c1, c2, c3, c4, c5 := ids.NewCandidate(), ids.NewCandidate(), ids.NewCandidate(), ids.NewCandidate(), ids.NewCandidate()
	in := []store.CandidateFrame{
		// c2 is a duplicate of c3, which comes later in the batch (deferred FK).
		{ID: c2, JobID: id, SceneIdx: 1, ShotID: "rear", TimestampSec: 5.5, Sharpness: 120.5, SharpnessZ: -0.75,
			ExposurePenalty: 0.1, DHash: 1 << 63, DuplicateOf: c3, RejectedReason: "duplicate", Score: 0.25},
		{ID: c1, SceneIdx: 0, ShotID: "front", TimestampSec: 1.25, Sharpness: 300, SharpnessZ: 1.5,
			DHash: math.MaxUint64, Score: 0.9, Selected: true},
		{ID: c3, SceneIdx: 1, ShotID: "rear", TimestampSec: 6, Sharpness: 250, SharpnessZ: 1.25,
			DHash: 0xDEADBEEFCAFEBABE, Score: 0.8},
		{ID: c4, SceneIdx: -1, ShotID: "coverage", TimestampSec: 9, DHash: 0, RejectedReason: "blurry"},
		{ID: c5, SceneIdx: 7, ShotID: "unknown_scene", TimestampSec: 10, DHash: 42, RejectedReason: "exposure"}, // no scene 7 → NULL
	}
	require.NoError(t, s.ReplaceCandidates(ctx, id, in))

	got, err := s.ListCandidates(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, 5)
	ptr := func(v int64) *int64 { return &v }
	want := []store.CandidateFrame{in[1], in[0], in[2], in[3], in[4]} // timestamp order
	want[0].SceneID = ptr(sceneID[0])
	want[1].SceneID = ptr(sceneID[1])
	want[2].SceneID = ptr(sceneID[1])
	want[4].SceneIdx = -1 // stored NULL, read back as -1
	for i := range want {
		want[i].JobID = id
		assert.Equal(t, want[i], got[i], "candidate %d", i)
	}
	assert.Equal(t, uint64(1<<63), got[1].DHash)
	assert.Equal(t, uint64(math.MaxUint64), got[0].DHash)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM candidate_frames WHERE id = $1 AND dhash = -1`, c1), "stored bit-for-bit")
	assert.Equal(t, 2, countRows(t, pool, `SELECT count(*) FROM candidate_frames WHERE job_id = $1 AND scene_id IS NULL`, id))
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM candidate_frames WHERE id = $1 AND duplicate_of = $2`, c2, c3))
}

func TestReplaceCandidatesReplacesAndDropsImages(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	other := processingJob(t, s)
	old := ids.NewCandidate()
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{{ID: old, SceneIdx: -1, ShotID: "front", TimestampSec: 1}}))
	require.NoError(t, s.ReplaceCandidates(ctx, other, []store.CandidateFrame{{ID: ids.NewCandidate(), SceneIdx: -1, ShotID: "front", TimestampSec: 1}}))
	require.NoError(t, s.CompleteJob(ctx, id, store.Completion{Images: []store.SelectedImage{{ID: ids.NewImage(), CandidateID: old, Idx: 1, ShotID: "front"}}}))

	fresh := ids.NewCandidate()
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{{ID: fresh, SceneIdx: -1, ShotID: "rear", TimestampSec: 2}}))
	got, err := s.ListCandidates(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, fresh, got[0].ID)
	imgs, err := s.ListSelectedImages(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, imgs, "selected images of the job are deleted")
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM candidate_frames WHERE job_id = $1`, other), "other jobs untouched")

	require.NoError(t, s.ReplaceCandidates(ctx, id, nil))
	got, err = s.ListCandidates(ctx, id)
	require.NoError(t, err)
	assert.NotNil(t, got)
	assert.Empty(t, got)
}

func TestReplaceCandidatesBadDuplicateRollsBack(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	keep := ids.NewCandidate()
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{{ID: keep, SceneIdx: -1, ShotID: "front", TimestampSec: 1}}))

	err := s.ReplaceCandidates(ctx, id, []store.CandidateFrame{
		{ID: ids.NewCandidate(), SceneIdx: -1, ShotID: "front", TimestampSec: 1, DuplicateOf: ids.NewCandidate()},
	})
	require.Error(t, err, "duplicate_of must name a candidate (checked at commit)")
	assert.NotErrorIs(t, err, store.ErrNotFound)
	got, err := s.ListCandidates(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, keep, got[0].ID)

	assert.Error(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{{ShotID: "no id"}}))
	assert.Error(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{{ID: ids.NewCandidate(), JobID: ids.NewJob()}}))
	require.ErrorIs(t, s.ReplaceCandidates(ctx, ids.NewJob(), []store.CandidateFrame{{ID: ids.NewCandidate(), SceneIdx: -1}}), store.ErrNotFound)
}

func TestReplaceCandidatesBulkOrdering(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	require.NoError(t, s.ReplaceScenes(ctx, id, []store.Scene{{Idx: 0, StartSec: 0, EndSec: 60, ShotID: "front", Relevance: 1}}))

	const n = 300
	cands := make([]store.CandidateFrame, 0, n)
	for i := range n {
		// Reverse timestamp order, with pairs sharing a timestamp to exercise the id tiebreak.
		c := store.CandidateFrame{
			ID: fmt.Sprintf("cf_%04d", n-i), SceneIdx: 0, ShotID: "front",
			TimestampSec: float64((n-i)/2) * 0.2, DHash: uint64(i) << 40, Score: float64(i) / n,
		}
		if i > 0 && i%10 == 0 {
			c.DuplicateOf = cands[i-1].ID
			c.RejectedReason = "duplicate"
		}
		cands = append(cands, c)
	}
	require.NoError(t, s.ReplaceCandidates(ctx, id, cands))
	got, err := s.ListCandidates(ctx, id)
	require.NoError(t, err)
	require.Len(t, got, n)
	for i := 1; i < n; i++ {
		prev, cur := got[i-1], got[i]
		ordered := prev.TimestampSec < cur.TimestampSec || (prev.TimestampSec == cur.TimestampSec && prev.ID < cur.ID)
		assert.True(t, ordered, "row %d (%v %s) before row %d (%v %s)", i-1, prev.TimestampSec, prev.ID, i, cur.TimestampSec, cur.ID)
	}
	assert.Equal(t, "cf_0001", got[0].ID)
}
