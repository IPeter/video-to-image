package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/ids"
	"video-to-image/internal/store"
)

func stepStatuses(t *testing.T, s *store.Store, id string) map[string]store.StepStatus {
	t.Helper()
	got := map[string]store.StepStatus{}
	for _, st := range mustSteps(t, s, id) {
		got[st.Name] = st.Status
	}
	return got
}

func TestSaveAnalysisAcceptedWritesEverything(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s, withModel("creation-model"))
	// Leftovers of an earlier attempt are replaced.
	require.NoError(t, s.ReplaceScenes(ctx, id, []store.Scene{{Idx: 0, StartSec: 0, EndSec: 1, ShotID: "old", Relevance: 1}}))
	require.NoError(t, s.ReplaceCandidates(ctx, id, []store.CandidateFrame{{ID: ids.NewCandidate(), SceneIdx: 0, ShotID: "old", TimestampSec: 0.5}}))
	require.NoError(t, s.StartStep(ctx, id, store.StepNamePreview))
	require.NoError(t, s.FinishStep(ctx, id, store.StepNamePreview, store.StepDone, nil))
	require.NoError(t, s.StartStep(ctx, id, store.StepNameAnalyze))

	gate := store.Gate{Passed: true, Confidence: 0.93, Reason: "a car", DetectedSubject: "car"}
	require.NoError(t, s.SaveAnalysis(ctx, id, store.AnalysisResult{
		Gate: gate, Summary: "A red hatchback.", ModelName: "gemini-x",
		Scenes: []store.Scene{
			{Idx: 0, StartSec: 1, EndSec: 2.5, ShotID: "front", Relevance: 0.9, Reason: "front", RawStart: "00:01", RawEnd: "00:02.5"},
			{Idx: 1, StartSec: 4, EndSec: 5, ShotID: "rear", Relevance: 0.8},
		},
		StepMeta: map[string]any{"accepted": true, "scenes": 2},
	}))

	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusProcessing, j.Status)
	require.NotNil(t, j.Gate)
	assert.Equal(t, gate, *j.Gate)
	assert.Equal(t, "A red hatchback.", j.VideoSummary)
	assert.Equal(t, "gemini-x", j.ModelName)
	assert.NotEmpty(t, j.VideoPath, "an accepted video keeps its original")

	scenes, err := s.ListScenes(ctx, id)
	require.NoError(t, err)
	require.Len(t, scenes, 2)
	assert.Equal(t, "front", scenes[0].ShotID)
	assert.Equal(t, "00:02.5", scenes[0].RawEnd)
	assert.Zero(t, countRows(t, pool, `SELECT count(*) FROM candidate_frames WHERE job_id = $1`, id), "old candidates are gone")

	analyze := stepByName(t, s, id, store.StepNameAnalyze)
	assert.Equal(t, store.StepDone, analyze.Status)
	assert.NotNil(t, analyze.FinishedAt)
	assert.NotNil(t, analyze.DurationMs)
	assert.Equal(t, map[string]any{"accepted": true, "scenes": float64(2)}, analyze.Meta)
	assert.Equal(t, map[string]store.StepStatus{
		store.StepNamePreview: store.StepDone,
		store.StepNameAnalyze: store.StepDone,
		store.StepNameExtract: store.StepPending,
		store.StepNameScore:   store.StepPending,
		store.StepNameSelect:  store.StepPending,
		store.StepNameExport:  store.StepPending,
	}, stepStatuses(t, s, id))

	// Saving again (a retry after an ambiguous commit) is allowed while processing, and an
	// empty model name keeps the stored one.
	require.NoError(t, s.SaveAnalysis(ctx, id, store.AnalysisResult{Gate: gate, Summary: "again"}))
	j, err = s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "gemini-x", j.ModelName)
	assert.Equal(t, "again", j.VideoSummary)
	scenes, err = s.ListScenes(ctx, id)
	require.NoError(t, err)
	assert.Empty(t, scenes)
}

func TestSaveAnalysisRejectIsOneTransition(t *testing.T) {
	t.Parallel()
	s, pool := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	require.NoError(t, s.StartStep(ctx, id, store.StepNamePreview))
	require.NoError(t, s.FinishStep(ctx, id, store.StepNamePreview, store.StepDone, nil))
	require.NoError(t, s.StartStep(ctx, id, store.StepNameAnalyze))

	gate := store.Gate{Passed: false, Confidence: 0.97, Reason: "a kitchen", DetectedSubject: "kitchen\x00"}
	require.NoError(t, s.SaveAnalysis(ctx, id, store.AnalysisResult{
		Gate: gate, Summary: "A kitchen tour.", ModelName: "m", Reject: true,
		StepMeta: map[string]any{"accepted": false},
	}))

	j, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, store.StatusRejected, j.Status)
	require.NotNil(t, j.Gate)
	assert.Equal(t, "a kitchen", j.Gate.Reason)
	assert.Equal(t, "kitchen�", j.Gate.DetectedSubject, "NUL is made storable")
	assert.Equal(t, "A kitchen tour.", j.VideoSummary)
	assert.Empty(t, j.VideoPath)
	assert.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM jobs WHERE id = $1 AND video_path IS NULL`, id))
	assert.Equal(t, map[string]store.StepStatus{
		store.StepNamePreview: store.StepDone,
		store.StepNameAnalyze: store.StepDone,
		store.StepNameExtract: store.StepSkipped,
		store.StepNameScore:   store.StepSkipped,
		store.StepNameSelect:  store.StepSkipped,
		store.StepNameExport:  store.StepSkipped,
	}, stepStatuses(t, s, id))
	assert.Equal(t, map[string]any{"accepted": false}, stepByName(t, s, id, store.StepNameAnalyze).Meta)

	// A second save (the rejection already committed) is a conflict and writes nothing.
	err = s.SaveAnalysis(ctx, id, store.AnalysisResult{Gate: store.Gate{Passed: true}, Summary: "x", Reject: true})
	require.ErrorIs(t, err, store.ErrConflict)
	after, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, *j, *after)
}

func TestSaveAnalysisRollsBackOnFailure(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	id := processingJob(t, s)
	require.NoError(t, s.ReplaceScenes(ctx, id, []store.Scene{{Idx: 0, StartSec: 0, EndSec: 1, ShotID: "front", Relevance: 1}}))
	require.NoError(t, s.StartStep(ctx, id, store.StepNameAnalyze))
	before, err := s.GetJob(ctx, id)
	require.NoError(t, err)

	// The duplicate (job_id, idx) fails inside the transaction, after the job UPDATE.
	err = s.SaveAnalysis(ctx, id, store.AnalysisResult{
		Gate: store.Gate{Passed: false, Confidence: 1, Reason: "r"}, Summary: "s", Reject: true,
		Scenes: []store.Scene{
			{Idx: 3, StartSec: 0, EndSec: 1, ShotID: "a", Relevance: 1},
			{Idx: 3, StartSec: 1, EndSec: 2, ShotID: "b", Relevance: 1},
		},
	})
	require.Error(t, err)
	after, err := s.GetJob(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, *before, *after, "nothing of the job row is written")
	scenes, err := s.ListScenes(ctx, id)
	require.NoError(t, err)
	require.Len(t, scenes, 1)
	assert.Equal(t, "front", scenes[0].ShotID)
	assert.Equal(t, store.StepRunning, stepByName(t, s, id, store.StepNameAnalyze).Status)

	// Input errors are reported before anything is written.
	require.Error(t, s.SaveAnalysis(ctx, id, store.AnalysisResult{StepMeta: []int{1}}), "meta must be an object")
	require.Error(t, s.SaveAnalysis(ctx, id, store.AnalysisResult{Scenes: []store.Scene{{JobID: ids.NewJob(), ShotID: "x"}}}), "foreign scene")
	assert.Equal(t, store.StatusProcessing, jobStatus(t, s, id))
	assert.Equal(t, store.StepRunning, stepByName(t, s, id, store.StepNameAnalyze).Status)
}

func TestSaveAnalysisNotProcessingWritesNothing(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t)
	ctx := context.Background()
	res := store.AnalysisResult{
		Gate: store.Gate{Passed: true, Confidence: 1}, Summary: "s",
		Scenes:   []store.Scene{{Idx: 0, StartSec: 0, EndSec: 1, ShotID: "front", Relevance: 1}},
		StepMeta: map[string]any{"x": 1},
	}
	queued := createJob(t, s)
	expired := processingJob(t, s)
	require.NoError(t, s.ExpireJob(ctx, expired)) // the user pressed "Delete now"
	for _, id := range []string{queued, expired} {
		for _, reject := range []bool{false, true} {
			res.Reject = reject
			before, err := s.GetJob(ctx, id)
			require.NoError(t, err)
			require.ErrorIs(t, s.SaveAnalysis(ctx, id, res), store.ErrConflict)
			after, err := s.GetJob(ctx, id)
			require.NoError(t, err)
			assert.Equal(t, *before, *after)
			scenes, err := s.ListScenes(ctx, id)
			require.NoError(t, err)
			assert.Empty(t, scenes)
			assert.NotEqual(t, store.StepDone, stepByName(t, s, id, store.StepNameAnalyze).Status)
		}
	}
	require.ErrorIs(t, s.SaveAnalysis(ctx, ids.NewJob(), res), store.ErrNotFound)
}
