package extract

import (
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/media"
)

func TestPlanSingleRange(t *testing.T) {
	type want struct {
		padStart, padEnd, fps float64
		expected              int
	}
	tests := []struct {
		name      string
		dur       float64
		sampleFPS int
		r         Range
		want      *want // nil: skipped
	}{
		{"padding on both sides", 20, 4, Range{StartSec: 4, EndSec: 9}, &want{3.5, 9.5, 4, 24}},
		{"clamped at zero", 20, 4, Range{StartSec: 0.2, EndSec: 3}, &want{0, 3.5, 4, 14}},
		{"starts at zero", 20, 4, Range{StartSec: 0, EndSec: 2}, &want{0, 2.5, 4, 10}},
		{"clamped at duration", 10, 4, Range{StartSec: 8, EndSec: 10}, &want{7.5, 10, 4, 10}},
		{"end past duration: clamped length is short → 8 fps", 10, 4, Range{StartSec: 9, EndSec: 12}, &want{8.5, 10, 8, 12}},
		{"short range bumps to 8 fps", 20, 4, Range{StartSec: 5, EndSec: 6}, &want{4.5, 6.5, 8, 16}},
		{"exactly ShortRangeSec is not short", 20, 4, Range{StartSec: 5, EndSec: 6.5}, &want{4.5, 7, 4, 10}},
		{"high sample fps is never lowered for short ranges", 20, 10, Range{StartSec: 5, EndSec: 6}, &want{4.5, 6.5, 10, 20}},
		{"sample fps 0 → default 4", 20, 0, Range{StartSec: 4, EndSec: 9}, &want{3.5, 9.5, 4, 24}},
		{"negative sample fps → default 4", 20, -3, Range{StartSec: 4, EndSec: 9}, &want{3.5, 9.5, 4, 24}},
		{"instant (start == end) is kept as a 1 s window", 20, 4, Range{StartSec: 3, EndSec: 3}, &want{2.5, 3.5, 8, 8}},
		{"tiny tail window keeps one frame", 10, 4, Range{StartSec: 9.99, EndSec: 9.99}, &want{9.49, 10, 8, 4}},
		{"negative start clamps", 20, 4, Range{StartSec: -2, EndSec: 3}, &want{0, 3.5, 4, 14}},
		{"start == duration → skipped", 10, 4, Range{StartSec: 10, EndSec: 11}, nil},
		{"start > duration → skipped", 10, 4, Range{StartSec: 12, EndSec: 14}, nil},
		{"end < start → skipped", 20, 4, Range{StartSec: 6, EndSec: 5}, nil},
		{"entirely before zero → skipped", 20, 4, Range{StartSec: -3, EndSec: -1}, nil},
		{"NaN start → skipped", 20, 4, Range{StartSec: math.NaN(), EndSec: 5}, nil},
		{"Inf end → skipped", 20, 4, Range{StartSec: 1, EndSec: math.Inf(1)}, nil},
		{"zero duration video → skipped", 0, 4, Range{StartSec: 0, EndSec: 1}, nil},
		{"NaN duration video → skipped", math.NaN(), 4, Range{StartSec: 0, EndSec: 1}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.r.SceneIdx, tt.r.ShotID, tt.r.Relevance, tt.r.Reason = 7, "rear", 0.8, "clear rear view"
			plans := Plan(Input{DurationSec: tt.dur, Ranges: []Range{tt.r}, SampleFPS: tt.sampleFPS, MaxCandidates: 300})
			if tt.want == nil {
				assert.Empty(t, plans)
				return
			}
			require.Len(t, plans, 1)
			p := plans[0]
			assert.Equal(t, tt.r, p.Range, "range fields are carried through")
			assert.InDelta(t, tt.want.padStart, p.PadStartSec, 1e-9)
			assert.InDelta(t, tt.want.padEnd, p.PadEndSec, 1e-9)
			assert.InDelta(t, tt.want.fps, p.FPS, 1e-9)
			assert.Equal(t, tt.want.expected, p.Expected)
		})
	}
}

func TestPlanKeepsInputOrder(t *testing.T) {
	plans := Plan(Input{
		DurationSec: 30,
		Ranges: []Range{
			{SceneIdx: 2, StartSec: 20, EndSec: 22},
			{SceneIdx: 0, StartSec: 1, EndSec: 3},
			{SceneIdx: 1, StartSec: 40, EndSec: 42}, // skipped
			{SceneIdx: 3, StartSec: 10, EndSec: 12},
		},
		SampleFPS: 4, MaxCandidates: 300,
	})
	require.Len(t, plans, 3)
	assert.Equal(t, []int{2, 0, 3}, []int{plans[0].SceneIdx, plans[1].SceneIdx, plans[2].SceneIdx})
}

func TestPlanScalesProportionallyToCap(t *testing.T) {
	in := Input{
		DurationSec: 120,
		Ranges: []Range{
			{SceneIdx: 0, StartSec: 5, EndSec: 14},    // 10 s padded @4 → 40
			{SceneIdx: 1, StartSec: 30, EndSec: 39},   // 40
			{SceneIdx: 2, StartSec: 60, EndSec: 69},   // 40
			{SceneIdx: 3, StartSec: 90, EndSec: 91},   // short: 2 s padded @8 → 16
			{SceneIdx: 4, StartSec: 100, EndSec: 118}, // 19 s padded @4 → 76
		},
		SampleFPS:     4,
		MaxCandidates: 60,
	}
	unscaled := Plan(Input{DurationSec: in.DurationSec, Ranges: in.Ranges, SampleFPS: 4, MaxCandidates: 10_000})
	require.Len(t, unscaled, 5)
	assert.Equal(t, 212, totalExpected(unscaled))

	plans := Plan(in)
	require.Len(t, plans, 5, "ranges are never dropped when each can keep a frame")
	total := totalExpected(plans)
	assert.LessOrEqual(t, total, 60)
	assert.GreaterOrEqual(t, total, 60-len(plans), "the cap is used, not wasted")

	s := plans[0].FPS / unscaled[0].FPS
	assert.Less(t, s, 1.0)
	assert.GreaterOrEqual(t, s, 60.0/212-1e-9, "at least the naive MaxCandidates/Σ factor")
	for i, p := range plans {
		assert.InDelta(t, s, p.FPS/unscaled[i].FPS, 1e-12, "same factor for every range (%d)", i)
		assert.Equal(t, unscaled[i].PadStartSec, p.PadStartSec, "ranges are not truncated")
		assert.Equal(t, unscaled[i].PadEndSec, p.PadEndSec, "ranges are not truncated")
		assert.GreaterOrEqual(t, p.Expected, 1)
		assert.Equal(t, media.ExpectedFrames(p.PadEndSec-p.PadStartSec, p.FPS), p.Expected)
	}
	assert.InDelta(t, 2.0, plans[3].FPS/plans[0].FPS, 1e-12, "the short-range bump survives scaling")
}

func TestPlanCapHitsOneFrameMinimum(t *testing.T) {
	var ranges []Range
	for i := range 5 {
		ranges = append(ranges, Range{SceneIdx: i, StartSec: float64(i * 20), EndSec: float64(i*20 + 10)})
	}
	plans := Plan(Input{DurationSec: 120, Ranges: ranges, SampleFPS: 4, MaxCandidates: 5})
	require.Len(t, plans, 5)
	for _, p := range plans {
		assert.Equal(t, 1, p.Expected)
		assert.Greater(t, p.FPS, 0.0)
	}
	assert.Equal(t, 5, totalExpected(plans))

	// One extra slot goes to whichever range reaches two frames first (the longest).
	ranges[2].EndSec = 58
	plans = Plan(Input{DurationSec: 120, Ranges: ranges, SampleFPS: 4, MaxCandidates: 6})
	require.Len(t, plans, 5)
	assert.Equal(t, 6, totalExpected(plans))
	assert.Equal(t, 2, plans[2].Expected)
}

func TestPlanMoreRangesThanCapKeepsMostRelevant(t *testing.T) {
	ranges := []Range{
		{SceneIdx: 0, StartSec: 1, EndSec: 3, Relevance: 0.1},
		{SceneIdx: 1, StartSec: 5, EndSec: 7, Relevance: 0.9},
		{SceneIdx: 2, StartSec: 9, EndSec: 11, Relevance: 0.5},
		{SceneIdx: 3, StartSec: 13, EndSec: 15, Relevance: 0.9},
		{SceneIdx: 4, StartSec: 17, EndSec: 19, Relevance: 0.7},
	}
	plans := Plan(Input{DurationSec: 30, Ranges: ranges, SampleFPS: 4, MaxCandidates: 3})
	require.Len(t, plans, 3)
	assert.Equal(t, []int{1, 3, 4}, []int{plans[0].SceneIdx, plans[1].SceneIdx, plans[2].SceneIdx}, "input order kept")
	assert.Equal(t, 3, totalExpected(plans))

	// Ties are broken by input order.
	plans = Plan(Input{DurationSec: 30, Ranges: ranges, SampleFPS: 4, MaxCandidates: 1})
	require.Len(t, plans, 1)
	assert.Equal(t, 1, plans[0].SceneIdx)
}

func TestPlanDefaultCap(t *testing.T) {
	// 170 s of ranges at 4 fps (680 frames) against the default cap of 300.
	var ranges []Range
	for i := range 17 {
		ranges = append(ranges, Range{SceneIdx: i, StartSec: float64(i * 10), EndSec: float64(i*10 + 9)})
	}
	plans := Plan(Input{DurationSec: 180, Ranges: ranges, SampleFPS: 4, MaxCandidates: 0})
	require.Len(t, plans, 17)
	assert.LessOrEqual(t, totalExpected(plans), DefaultMaxCandidates)
	assert.Greater(t, totalExpected(plans), DefaultMaxCandidates-17)
}

func TestPlanEmpty(t *testing.T) {
	assert.Empty(t, Plan(Input{DurationSec: 10}))
	assert.Empty(t, Plan(Input{}))
}

// TestPlanInvariants checks Plan's guarantees on many random inputs.
func TestPlanInvariants(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 7))
	for iter := range 2000 {
		dur := 1 + r.Float64()*180
		n := r.IntN(25)
		ranges := make([]Range, n)
		for i := range ranges {
			start := r.Float64()*(dur+10) - 5
			ranges[i] = Range{SceneIdx: i, StartSec: start, EndSec: start + r.Float64()*20 - 1, Relevance: r.Float64()}
		}
		in := Input{DurationSec: dur, Ranges: ranges, SampleFPS: r.IntN(12), MaxCandidates: 1 + r.IntN(400)}
		plans := Plan(in)

		require.LessOrEqual(t, totalExpected(plans), in.MaxCandidates, "iter %d", iter)
		require.LessOrEqual(t, len(plans), n)
		for _, p := range plans {
			require.GreaterOrEqual(t, p.PadStartSec, 0.0)
			require.LessOrEqual(t, p.PadEndSec, dur)
			require.Greater(t, p.PadEndSec, p.PadStartSec)
			require.Less(t, p.StartSec, dur)
			require.GreaterOrEqual(t, p.EndSec, p.StartSec)
			require.Greater(t, p.FPS, 0.0)
			require.LessOrEqual(t, p.FPS, math.Max(float64(max(in.SampleFPS, 0)), ShortRangeFPS)+1e-9)
			require.GreaterOrEqual(t, p.Expected, 1)
			require.Equal(t, media.ExpectedFrames(p.PadEndSec-p.PadStartSec, p.FPS), p.Expected)
		}
	}
}
