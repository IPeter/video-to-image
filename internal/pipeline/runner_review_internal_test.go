package pipeline

import (
	"encoding/json"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/pipeline/extract"
	"video-to-image/internal/pipeline/review"
	"video-to-image/internal/pipeline/score"
	"video-to-image/internal/pipeline/selectr"
	"video-to-image/internal/store"
)

// Unit tests for the review bookkeeping of the select step: stored verdicts and
// orientation answers, the rotate-only fallback and the cut-off hint.

func cand(id string, scene int, shot string, ts float64) score.Scored {
	return score.Scored{Candidate: extract.Candidate{ID: id, SceneIdx: scene, ShotID: shot, TimestampSec: ts}}
}

// roundTrip marshals v the way job_steps.meta stores it and decodes it back as generic
// JSON, like ListSteps does.
func roundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	var out any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

func TestExtractConcurrency(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		configured, procs, threads, workers int
		want                                int
	}{
		{"configured wins", 3, 4, 2, 2, 3},
		{"home-k3s: 4 CPUs, 2 threads, 2 workers", 0, 4, 2, 2, 1},
		{"8 CPUs, 2 threads, 1 worker", 0, 8, 2, 1, 4},
		{"16 CPUs, 2 threads, 2 workers", 0, 16, 2, 2, 4},
		{"fewer CPUs than one worker needs", 0, 1, 2, 2, 1},
		{"workers 0 counts as 1", 0, 8, 4, 0, 2},
		{"uncapped ffmpeg uses every core", 0, 16, 0, 1, 1},
		{"negative is auto", -1, 8, 2, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ExtractConcurrency(tc.configured, tc.procs, tc.threads, tc.workers))
		})
	}
}

func TestAssessmentsFromMeta(t *testing.T) {
	first := []score.Scored{cand("a1", 0, "front_three_quarter", 1.0), cand("a2", 1, "side_profile", 5.0), cand("a3", 1, "side_profile", 6.0)}
	assess := map[string]review.Assessment{
		"a1": {CandidateID: "a1", RotateCW: 90, View: "front_three_quarter", FullyVisible: true, Quality: 3,
			Box: review.Box{X0: 0.1, Y0: 0.2, X1: 0.8, Y1: 0.9}, Usable: true},
		"a2": {CandidateID: "a2", View: "side_profile", FullyVisible: true, Quality: 4, Usable: false, Issues: "glare"},
		"a3": {CandidateID: "a3", View: "side_profile", FullyVisible: true, Quality: 4, Usable: true, DuplicateOf: "a2"},
	}
	stored := roundTrip(t, assessmentsToMeta(assess, first))

	t.Run("maps onto new IDs and recomputes usable", func(t *testing.T) {
		// A retry re-extracts the same frames with new IDs (timestamps off by float32 noise).
		retry := []score.Scored{cand("b1", 0, "front_three_quarter", 1.0000004), cand("b2", 1, "side_profile", 5.0), cand("b3", 1, "side_profile", 6.0)}
		got, n, ok := assessmentsFromMeta(stored, retry, 4)
		require.True(t, ok)
		assert.Equal(t, 3, n)
		require.Len(t, got, 3)
		assert.Equal(t, 90, got["b1"].RotateCW)
		assert.Equal(t, review.Box{X0: 0.1, Y0: 0.2, X1: 0.8, Y1: 0.9}, got["b1"].Box)
		assert.False(t, got["b1"].Usable, "quality 3 is below the current minimum 4, whatever was stored")
		assert.True(t, got["b2"].Usable, "fully visible, quality 4: usable with minimum 4 although stored unusable")
		assert.Equal(t, "glare", got["b2"].Issues)
		assert.Equal(t, "b2", got["b3"].DuplicateOf, "sameAs follows the new IDs")
	})

	t.Run("stale timestamps are not reused", func(t *testing.T) {
		// A config change moved the sampling grid: nothing matches.
		retry := []score.Scored{cand("b1", 0, "front_three_quarter", 1.3333), cand("b2", 1, "side_profile", 5.3333), cand("b3", 1, "side_profile", 6.3333)}
		_, n, ok := assessmentsFromMeta(stored, retry, 3)
		assert.False(t, ok)
		assert.Equal(t, 3, n)
	})

	t.Run("one unmatched verdict is enough to rerun", func(t *testing.T) {
		retry := []score.Scored{cand("b1", 0, "front_three_quarter", 1.0), cand("b2", 1, "side_profile", 5.0), cand("b3", 1, "side_profile", 6.5)}
		_, _, ok := assessmentsFromMeta(stored, retry, 3)
		assert.False(t, ok)
	})

	t.Run("a view none frame is never usable", func(t *testing.T) {
		raw := roundTrip(t, []storedAssessment{{ShotID: "side_profile", TimestampSec: 5, View: review.ViewNone, FullyVisible: true, Quality: 5, SameAs: -1}})
		got, _, ok := assessmentsFromMeta(raw, first, 3)
		require.True(t, ok)
		assert.False(t, got["a2"].Usable)
	})

	t.Run("meta written before this round still loads", func(t *testing.T) {
		var legacy any
		require.NoError(t, json.Unmarshal([]byte(`[{"shotId":"side_profile","timestampSec":5,"rotateCw":0,"view":"side_profile",`+
			`"fullyVisible":true,"quality":4,"box":[0,0,0,0],"usable":true,"sameAs":-1}]`), &legacy))
		got, n, ok := assessmentsFromMeta(legacy, first, 3)
		require.True(t, ok)
		assert.Equal(t, 1, n)
		assert.True(t, got["a2"].Usable)
	})

	t.Run("meta without sameAs links no duplicates", func(t *testing.T) {
		// As stored before near-duplicate links existed (job_01M37ED8AVR3GQ0NNB6DQ5V1RE):
		// no sameAs key, which must not read as "a duplicate of the first frame".
		var legacy any
		require.NoError(t, json.Unmarshal([]byte(`[
		  {"box":[0,0,0,0],"view":"front_three_quarter","shotId":"front_three_quarter","usable":true,"quality":4,"rotateCw":0,"fullyVisible":true,"timestampSec":1},
		  {"box":[0,0,0,0],"view":"side_profile","shotId":"side_profile","usable":true,"quality":4,"rotateCw":0,"fullyVisible":true,"timestampSec":5},
		  {"box":[0,0,0,0],"view":"side_profile","shotId":"side_profile","usable":true,"quality":4,"rotateCw":0,"fullyVisible":true,"timestampSec":6}
		]`), &legacy))
		got, _, ok := assessmentsFromMeta(legacy, first, 3)
		require.True(t, ok)
		require.Len(t, got, 3)
		for id, a := range got {
			assert.Empty(t, a.DuplicateOf, id)
		}
	})

	t.Run("one shot at the same time in two scenes", func(t *testing.T) {
		// Two scenes of one shot whose padded ranges overlap sample the same timestamps.
		twice := []score.Scored{cand("a1", 0, "front", 3.0), cand("a2", 1, "front", 3.0)}
		verdicts := map[string]review.Assessment{
			"a1": {CandidateID: "a1", View: "front", FullyVisible: true, Quality: 4, RotateCW: 90},
			"a2": {CandidateID: "a2", View: "front", FullyVisible: true, Quality: 2, RotateCW: 270, DuplicateOf: "a1"},
		}
		retry := []score.Scored{cand("b1", 0, "front", 3.0), cand("b2", 1, "front", 3.0)}
		got, _, ok := assessmentsFromMeta(roundTrip(t, assessmentsToMeta(verdicts, twice)), retry, 3)
		require.True(t, ok, "each verdict maps onto its own scene's frame")
		assert.Equal(t, 90, got["b1"].RotateCW)
		assert.Equal(t, 270, got["b2"].RotateCW)
		assert.Equal(t, "b1", got["b2"].DuplicateOf)

		// Meta stored without the scene pairs up in stored (= candidate) order.
		var legacy any
		require.NoError(t, json.Unmarshal([]byte(`[
		  {"view":"front","shotId":"front","quality":4,"rotateCw":90,"fullyVisible":true,"timestampSec":3,"sameAs":-1},
		  {"view":"front","shotId":"front","quality":2,"rotateCw":270,"fullyVisible":true,"timestampSec":3,"sameAs":0}
		]`), &legacy))
		got, _, ok = assessmentsFromMeta(legacy, retry, 3)
		require.True(t, ok)
		assert.Equal(t, 90, got["b1"].RotateCW)
		assert.Equal(t, 270, got["b2"].RotateCW)

		// A second verdict for a frame that exists once does not match.
		_, _, ok = assessmentsFromMeta(legacy, retry[:1], 3)
		assert.False(t, ok)
	})

	t.Run("undecodable", func(t *testing.T) {
		_, _, ok := assessmentsFromMeta(map[string]any{"not": "a list"}, first, 3)
		assert.False(t, ok)
	})
}

func TestRotationsFromMeta(t *testing.T) {
	first := []score.Scored{cand("a1", 0, "front_three_quarter", 1.0), cand("a2", 1, "side_profile", 5.0)}
	stored := roundTrip(t, rotationsToMeta(map[string]int{"a1": 270, "a2": 0}, first))

	retry := []score.Scored{cand("b1", 0, "front_three_quarter", 1.0), cand("b2", 1, "side_profile", 5.0), cand("b3", 1, "side_profile", 6.0)}
	assert.Equal(t, map[string]int{"b1": 270, "b2": 0}, rotationsFromMeta(stored, retry), "a known 0 is kept")

	bad := roundTrip(t, []storedRotation{
		{ShotID: "front_three_quarter", TimestampSec: 1, RotateCW: 45},
		{ShotID: "side_profile", TimestampSec: 5, RotateCW: 360},
		{ShotID: "side_profile", TimestampSec: 6, RotateCW: 90},
		{ShotID: "side_profile", TimestampSec: 9, RotateCW: 90}, // matches no frame
	})
	assert.Equal(t, map[string]int{"b3": 90}, rotationsFromMeta(bad, retry), "only right angles of known frames")
	assert.Nil(t, rotationsFromMeta(nil, retry))
	assert.Nil(t, rotationsFromMeta("garbage", retry))

	// One shot at the same time in two scenes (overlapping padded ranges): each answer
	// goes to its own scene's frame, not both to the first.
	twice := []score.Scored{cand("a1", 0, "front", 3.0), cand("a2", 1, "front", 3.0)}
	again := []score.Scored{cand("b1", 0, "front", 3.0), cand("b2", 1, "front", 3.0)}
	assert.Equal(t, map[string]int{"b1": 90, "b2": 270},
		rotationsFromMeta(roundTrip(t, rotationsToMeta(map[string]int{"a1": 90, "a2": 270}, twice)), again))
	assert.Equal(t, map[string]int{"b2": 270},
		rotationsFromMeta(roundTrip(t, rotationsToMeta(map[string]int{"a2": 270}, twice)), again), "matched by scene")
}

func TestEditForFallsBackToOrientation(t *testing.T) {
	scored := []score.Scored{
		cand("s1", 0, "front_three_quarter", 1.0), // oriented: 90
		cand("s2", 0, "front_three_quarter", 1.8), // 0.8 s from s1
		cand("s3", 0, "front_three_quarter", 2.5), // 1.5 s from s1: too far
		cand("s4", 1, "side_profile", 1.2),        // another scene
		cand("s5", 1, "side_profile", 3.0),        // oriented: 0
		cand("s6", 1, "side_profile", 3.5),        // nearest oriented frame says upright
	}
	rn := &run{
		Runner: &Runner{}, log: slog.Default(), dom: &domain.Config{}, scored: scored,
		rotations: map[string]int{"s1": 90, "s5": 0},
	}
	want := map[string]*export.Edit{
		"s1": export.NewEdit(90, nil),
		"s2": export.NewEdit(90, nil),
		"s3": nil,
		"s4": nil,
		"s5": nil,
		"s6": nil,
	}
	for _, c := range scored {
		got, err := rn.editFor(c)
		require.NoError(t, err)
		assert.Equal(t, want[c.ID], got, c.ID)
	}

	// The nearest oriented frame of the scene wins.
	rn.rotations = map[string]int{"s1": 90, "s3": 180}
	rot, ok := rn.rotationFor(scored[1])
	assert.True(t, ok)
	assert.Equal(t, 180, rot, "s3 is 0.7 s away, s1 0.8 s")
	rot, _ = rn.rotationFor(cand("s7", 0, "front_three_quarter", 1.3))
	assert.Equal(t, 90, rot)
	_, ok = rn.rotationFor(cand("s8", 2, "rear_three_quarter", 1.0))
	assert.False(t, ok, "no oriented frame in that scene")

	// A verdict wins over the orientation answer.
	rn.assessments = map[string]review.Assessment{"s1": {CandidateID: "s1", RotateCW: 270}}
	got, err := rn.editFor(scored[0])
	require.NoError(t, err)
	assert.Equal(t, export.NewEdit(270, nil), got)
}

func TestCutOffHint(t *testing.T) {
	shots := []domain.Shot{{ID: "front_three_quarter", Label: "Front 3/4"}, {ID: "side_profile", Label: "Side profile"}, {ID: "rear_three_quarter", Label: "Rear 3/4"}}
	sel := &selectr.Output{MissingShots: shots}
	assess := map[string]review.Assessment{
		// Every take of the front 3/4 was cut off.
		"f1": {View: "front_three_quarter", FullyVisible: false, Quality: 4},
		"f2": {View: "front_three_quarter", FullyVisible: false, Quality: 2},
		// The side profile had a whole but blurry take and a cut-off one: blur is the problem.
		"s1": {View: "side_profile", FullyVisible: true, Quality: 1},
		"s2": {View: "side_profile", FullyVisible: false, Quality: 4},
		// The rear 3/4 was never seen by the review.
	}
	hint := cutOffHint(sel, assess)
	assert.Equal(t, "The frame edge cut off part of the subject in every take of: Front 3/4. "+
		"Film these views from further away so the whole subject fits in the picture.", hint)

	delete(assess, "f1")
	delete(assess, "f2")
	assert.Empty(t, cutOffHint(sel, assess), "a view with a fully visible take gets no cut-off hint")
	assert.Empty(t, cutOffHint(sel, nil))
	assert.Empty(t, cutOffHint(&selectr.Output{}, assess))
}

func TestReuseReview(t *testing.T) {
	scored := []score.Scored{cand("a1", 0, "front_three_quarter", 1.0), cand("a2", 1, "side_profile", 5.0)}
	verdicts := roundTrip(t, assessmentsToMeta(map[string]review.Assessment{
		"x1": {View: "front_three_quarter", FullyVisible: true, Quality: 4, Usable: true},
		"x2": {View: "side_profile", FullyVisible: false, Quality: 4},
	}, []score.Scored{cand("x1", 0, "front_three_quarter", 1.0), cand("x2", 1, "side_profile", 5.0)}))
	stale := roundTrip(t, []storedAssessment{{ShotID: "side_profile", TimestampSec: 5.25, View: "side_profile", SameAs: -1}})
	rotations := roundTrip(t, []storedRotation{{ShotID: "side_profile", TimestampSec: 5, RotateCW: 270}})

	for _, tc := range []struct {
		name          string
		prev          map[string]any
		done          bool
		verdicts      int // len of the reused assessments (-1: nil)
		failure       string
		wantRotations map[string]int
	}{
		{name: "nothing stored", prev: map[string]any{}, done: false, verdicts: -1},
		{name: "verdicts", prev: map[string]any{"review": verdicts, "reviewSent": 2.0, "reviewRotations": rotations},
			done: true, verdicts: 2, wantRotations: map[string]int{"a2": 270}},
		{name: "verdicts from before this round", prev: map[string]any{"review": verdicts, "reviewSent": 2.0, "reviewUsable": 1.0},
			done: true, verdicts: 2},
		{name: "empty verdicts from before this round", prev: map[string]any{"review": []any{}, "reviewSent": 2.0, "reviewUsable": 0.0},
			done: true, verdicts: -1, failure: "tooFew"},
		{name: "error from before this round", prev: map[string]any{"reviewError": "gemini 503", "reviewSent": 2.0},
			done: false, verdicts: -1},
		{name: "bad output", prev: map[string]any{"reviewError": "bad", "reviewFailure": "badOutput", "reviewRotations": rotations},
			done: true, verdicts: -1, failure: "badOutput", wantRotations: map[string]int{"a2": 270}},
		{name: "too few", prev: map[string]any{"reviewError": "review answered 0 of 2 frames", "reviewFailure": "tooFew"},
			done: true, verdicts: -1, failure: "tooFew"},
		{name: "other", prev: map[string]any{"reviewError": "review: no frame could be prepared", "reviewFailure": "other"},
			done: true, verdicts: -1, failure: "other"},
		{name: "unavailable", prev: map[string]any{"reviewError": "gemini 503", "reviewFailure": "unavailable", "reviewRotations": rotations},
			done: false, verdicts: -1},
		{name: "judged without orientation", prev: map[string]any{"review": verdicts, "reviewOrientationError": "not json"},
			done: false, verdicts: -1},
		{name: "stale verdicts", prev: map[string]any{"review": stale, "reviewSent": 1.0}, done: false, verdicts: -1},
		{name: "null keys", prev: map[string]any{"review": nil, "reviewFailure": nil}, done: false, verdicts: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rn := &run{
				Runner: &Runner{}, log: slog.Default(), dom: &domain.Config{},
				steps: map[string]store.Step{store.StepNameSelect: {Name: store.StepNameSelect, Meta: tc.prev}},
			}
			meta := map[string]any{}
			assess, done := rn.reuseReview(scored, meta)
			require.Equal(t, tc.done, done)
			if tc.verdicts < 0 {
				assert.Nil(t, assess)
			} else {
				assert.Len(t, assess, tc.verdicts)
			}
			if !done {
				assert.Empty(t, meta, "nothing reused")
				return
			}
			assert.Equal(t, true, meta["reviewReused"])
			if tc.failure != "" {
				assert.Equal(t, tc.failure, meta["reviewFailure"])
				assert.NotEmpty(t, meta["reviewError"])
				assert.NotContains(t, meta, "review")
			} else {
				assert.NotContains(t, meta, "reviewFailure")
				assert.Equal(t, 1, meta["reviewUsable"], "recomputed")
			}
			assert.Equal(t, tc.wantRotations, rn.rotations)
		})
	}
}
