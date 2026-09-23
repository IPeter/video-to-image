package score

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/extract"
)

func cand(id string, scene int, ts float64, path string) extract.Candidate {
	return extract.Candidate{ID: id, SceneIdx: scene, ShotID: "front", TimestampSec: ts, SmallPath: path}
}

func scored(id string, scene int, ts, sharp, exposure float64, hash uint64) Scored {
	return Scored{
		Candidate: extract.Candidate{ID: id, SceneIdx: scene, TimestampSec: ts},
		Sharpness: sharp, ExposurePenalty: exposure, DHash: hash,
	}
}

func byID(ss []Scored) map[string]Scored {
	m := make(map[string]Scored, len(ss))
	for _, s := range ss {
		m[s.ID] = s
	}
	return m
}

// distinctHash returns well-separated 64-bit hashes (pairwise Hamming ≥ 16).
func distinctHash(i int) uint64 {
	return [...]uint64{
		0x0000000000000000, 0xFFFFFFFF00000000, 0x00000000FFFFFFFF, 0xFFFF0000FFFF0000,
		0x0000FFFF0000FFFF, 0xFF00FF00FF00FF00, 0x00FF00FF00FF00FF, 0xF0F0F0F0F0F0F0F0,
	}[i]
}

func TestDefaultParams(t *testing.T) {
	d := &domain.Config{
		Selection: domain.Selection{DedupeMaxHamming: 6},
		Shots: []domain.Shot{
			{ID: "front_three_quarter", Required: true},
			{ID: "front"},
			{ID: "side_profile", Required: true},
			{ID: "rear"},
			{ID: "interior_dashboard", Required: true},
		},
	}
	assert.Equal(t, Params{
		DedupeMaxHamming: 6, BlurPercentile: 0.15, ExposureRejectShare: 0.40,
		ShotRank: map[string]int{
			"front_three_quarter": 0, "side_profile": 1, "interior_dashboard": 2, // required, in list order
			"front": 3, "rear": 4, // then optional, in list order
		},
	}, DefaultParams(d))
	assert.Empty(t, ShotRanks(&domain.Config{}))
}

// End to end on real JPEGs: exposure, duplicate and distinct frames across two scenes.
func TestRunEndToEnd(t *testing.T) {
	dir := t.TempDir()
	base := rampUp(fixW, fixH)
	files := map[string]*image.Gray{
		"a":     base,
		"a2":    addNoise(base, 2, 9), // near-identical to a
		"b":     rampDown(fixW, fixH),
		"black": mostly(fixW, fixH, 0.8, 0),
		"white": mostly(fixW, fixH, 0.8, 255),
		"bands": bands(fixW, fixH),
	}
	paths := map[string]string{}
	for name, img := range files {
		paths[name] = writeJPEG(t, dir, name+".jpg", toRGBA(img))
	}
	cands := []extract.Candidate{
		cand("a", 0, 1.0, paths["a"]),
		cand("a2", 0, 1.25, paths["a2"]),
		cand("b", 0, 1.5, paths["b"]),
		cand("black", 1, 7.0, paths["black"]),
		cand("white", 1, 7.25, paths["white"]),
		cand("bands", 2, 12.0, paths["bands"]),
	}
	out, err := Run(context.Background(), cands, Params{DedupeMaxHamming: 6})
	require.NoError(t, err)
	require.Len(t, out, len(cands))
	for i := range cands {
		assert.Equal(t, cands[i], out[i].Candidate, "input order and candidate carried over")
	}
	got := byID(out)

	// Distinct patterns are far apart in dHash space.
	assert.Equal(t, 64, Hamming(got["a"].DHash, got["b"].DHash), "ramp up vs ramp down")
	assert.Greater(t, Hamming(got["a"].DHash, got["bands"].DHash), 6)
	assert.Greater(t, Hamming(got["b"].DHash, got["bands"].DHash), 6)

	// Near-identical pair → the less sharp one is a duplicate of the sharper.
	assert.LessOrEqual(t, Hamming(got["a"].DHash, got["a2"].DHash), 6)
	sharper, softer := got["a"], got["a2"]
	if softer.Sharpness > sharper.Sharpness {
		sharper, softer = softer, sharper
	}
	assert.Empty(t, sharper.Rejected)
	assert.Equal(t, RejectDuplicate, softer.Rejected)
	assert.Equal(t, sharper.ID, softer.DuplicateOf)

	// Crushed / blown frames are rejected for exposure.
	for _, id := range []string{"black", "white"} {
		assert.Greater(t, got[id].ExposurePenalty, 0.40, id)
		assert.Equal(t, RejectExposure, got[id].Rejected, id)
	}

	assert.Empty(t, got["b"].Rejected)
	assert.Empty(t, got["bands"].Rejected)
	assert.Zero(t, got["bands"].SharpnessZ, "single-frame scene has z=0")
	assert.NotZero(t, got["a"].SharpnessZ, "z computed within scene 0")
}

func TestRunEmpty(t *testing.T) {
	out, err := Run(context.Background(), nil, Params{})
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestRunWorkerPoolOnRealFiles(t *testing.T) {
	dir := t.TempDir()
	imgs := []*image.Gray{rampUp(fixW, fixH), rampDown(fixW, fixH), bands(fixW, fixH), noise(fixW, fixH, 5)}
	paths := make([]string, len(imgs))
	want := make([]measurement, len(imgs))
	for i, img := range imgs {
		paths[i] = writeJPEG(t, dir, fmt.Sprintf("f%d.jpg", i), toRGBA(img))
		m, err := measureFile(paths[i])
		require.NoError(t, err)
		want[i] = m
	}
	var cands []extract.Candidate
	for i := range 60 {
		cands = append(cands, cand(fmt.Sprintf("c%02d", i), i%5, float64(i)/4, paths[i%len(paths)]))
	}
	out, err := Run(context.Background(), cands, Params{DedupeMaxHamming: 6}) // Workers 0 → NumCPU
	require.NoError(t, err)
	require.Len(t, out, len(cands))
	for i, s := range out {
		assert.Equal(t, cands[i].ID, s.ID)
		m := want[i%len(paths)]
		assert.Equal(t, m.Sharpness, s.Sharpness, s.ID)
		assert.Equal(t, m.ExposurePenalty, s.ExposurePenalty, s.ID)
		assert.Equal(t, m.DHash, s.DHash, s.ID)
	}
	// Only the sharpest copy of each file survives dedupe (all copies are equally sharp;
	// the tie-break keeps the lowest scene, then earliest timestamp).
	kept := 0
	for _, s := range out {
		if s.Rejected == "" {
			kept++
		}
	}
	assert.Equal(t, len(paths), kept)
}

func TestRunRespectsWorkerLimit(t *testing.T) {
	const workers = 3
	var cur, peak, calls atomic.Int64
	measureFn := func(string) (measurement, error) {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		calls.Add(1)
		// Burn a little time so workers overlap.
		x := 0.0
		for i := range 200000 {
			x += math.Sqrt(float64(i))
		}
		cur.Add(-1)
		return measurement{Sharpness: x}, nil
	}
	cands := make([]extract.Candidate, 50)
	for i := range cands {
		cands[i] = cand(fmt.Sprintf("c%02d", i), 0, float64(i), "x")
	}
	out, err := run(context.Background(), cands, Params{Workers: workers}, measureFn)
	require.NoError(t, err)
	assert.Len(t, out, 50)
	assert.EqualValues(t, 50, calls.Load())
	assert.LessOrEqual(t, peak.Load(), int64(workers))
}

func TestRunContextCancelled(t *testing.T) {
	path := writeJPEG(t, t.TempDir(), "a.jpg", toRGBA(rampUp(fixW, fixH)))
	cands := make([]extract.Candidate, 100)
	for i := range cands {
		cands[i] = cand(fmt.Sprintf("c%03d", i), 0, float64(i), path)
	}

	t.Run("before start", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		out, err := Run(ctx, cands, Params{})
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, out)
	})

	t.Run("mid run", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int64
		measureFn := func(p string) (measurement, error) {
			if calls.Add(1) == 5 {
				cancel()
			}
			return measureFile(p)
		}
		out, err := run(ctx, cands, Params{Workers: 1}, measureFn)
		require.ErrorIs(t, err, context.Canceled)
		assert.Nil(t, out)
		assert.EqualValues(t, 5, calls.Load(), "no work scheduled after cancellation")
	})
}

func TestRunUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	good := writeJPEG(t, dir, "good.jpg", toRGBA(rampUp(fixW, fixH)))
	garbage := filepath.Join(dir, "garbage.jpg")
	require.NoError(t, os.WriteFile(garbage, []byte("not a jpeg at all"), 0o600))
	missing := filepath.Join(dir, "missing.jpg")

	for name, bad := range map[string]string{"missing": missing, "garbage": garbage} {
		t.Run(name, func(t *testing.T) {
			cands := []extract.Candidate{cand("cf_good", 0, 0, good), cand("cf_bad", 0, 1, bad), cand("cf_good2", 0, 2, good)}
			out, err := Run(context.Background(), cands, Params{Workers: 2})
			require.Error(t, err)
			assert.Nil(t, out)
			assert.Contains(t, err.Error(), bad, "error names the file")
			assert.Contains(t, err.Error(), "cf_bad", "error names the candidate")
			assert.False(t, errors.Is(err, context.Canceled))
		})
	}

	t.Run("failure stops scheduling", func(t *testing.T) {
		var calls atomic.Int64
		boom := errors.New("boom")
		measureFn := func(string) (measurement, error) {
			if calls.Add(1) == 3 {
				return measurement{}, boom
			}
			return measurement{}, nil
		}
		cands := make([]extract.Candidate, 20)
		for i := range cands {
			cands[i] = cand(fmt.Sprintf("c%02d", i), 0, float64(i), "x")
		}
		_, err := run(context.Background(), cands, Params{Workers: 1}, measureFn)
		require.ErrorIs(t, err, boom)
		assert.EqualValues(t, 3, calls.Load())
	})
}

func TestClassifySceneZScores(t *testing.T) {
	ss := []Scored{
		scored("s0a", 0, 0, 10, 0, distinctHash(0)),
		scored("s1a", 1, 5, 1000, 0, distinctHash(1)),
		scored("s0b", 0, 1, 20, 0, distinctHash(2)),
		scored("s1b", 1, 6, 1000, 0, distinctHash(3)), // scene 1: σ = 0
		scored("s0c", 0, 2, 30, 0, distinctHash(4)),
		scored("s2a", 2, 9, 5, 0, distinctHash(5)), // scene 2: n = 1
	}
	Classify(ss, Params{DedupeMaxHamming: 6, BlurPercentile: 0.01})
	got := byID(ss)
	z := math.Sqrt(1.5) // (30-20)/sqrt(200/3)
	assert.InDelta(t, -z, got["s0a"].SharpnessZ, 1e-12)
	assert.InDelta(t, 0, got["s0b"].SharpnessZ, 1e-12)
	assert.InDelta(t, z, got["s0c"].SharpnessZ, 1e-12)
	assert.Zero(t, got["s1a"].SharpnessZ)
	assert.Zero(t, got["s1b"].SharpnessZ)
	assert.Zero(t, got["s2a"].SharpnessZ)

	// Near-equal values (rounding-sized σ) are treated as no spread.
	eq := []Scored{
		scored("x", 0, 0, 0.1, 0, distinctHash(0)),
		scored("y", 0, 1, 0.1, 0, distinctHash(1)),
		scored("z", 0, 2, 0.1, 0, distinctHash(2)),
	}
	Classify(eq, Params{})
	for _, s := range eq {
		assert.Zero(t, s.SharpnessZ)
	}
}

func TestClassifyExposure(t *testing.T) {
	cases := []struct {
		name     string
		share    float64
		exposure float64
		rejected bool
	}{
		{"default, at threshold", 0, 0.40, false},
		{"default, above", 0, 0.41, true},
		{"default, far below", 0, 0.05, false},
		{"custom share", 0.2, 0.25, true},
		{"custom share below", 0.2, 0.15, false},
		{"NaN share → default", math.NaN(), 0.41, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := []Scored{scored("a", 0, 0, 100, tc.exposure, 0)}
			Classify(ss, Params{ExposureRejectShare: tc.share})
			if tc.rejected {
				assert.Equal(t, RejectExposure, ss[0].Rejected)
			} else {
				assert.Empty(t, ss[0].Rejected)
			}
		})
	}
}

func TestClassifyBlurFloor(t *testing.T) {
	// One scene, so only the percentile arithmetic is under test (the scene's sharpest
	// frame is never below the floor anyway).
	seq := func(vals ...float64) []Scored {
		ss := make([]Scored, len(vals))
		for i, v := range vals {
			ss[i] = scored(fmt.Sprintf("c%02d", i), 0, float64(i), v, 0, 0)
		}
		return ss
	}
	ramp := make([]float64, 20)
	for i := range ramp {
		ramp[i] = float64(i + 1)
	}
	cases := []struct {
		name   string
		vals   []float64
		pct    float64
		blurry []string
	}{
		// n=20: rank ceil(0.15·20)=3 → floor = 3; 1 and 2 are strictly below.
		{"default 15% of 20", ramp, 0, []string{"c00", "c01"}},
		{"explicit 15% of 20", ramp, 0.15, []string{"c00", "c01"}},
		// n=4 at 50%: rank 2 → floor 100; only 1 is below.
		{"four candidates apply the floor", []float64{300, 1, 200, 100}, 0.5, []string{"c01"}},
		// n=3: fewer than MinCandidatesForBlurFloor → no floor even at 50%.
		{"three candidates keep everything", []float64{1, 100, 200}, 0.5, nil},
		// Ties at the floor are kept (strictly below only).
		{"ties at floor kept", []float64{5, 5, 5, 5, 10}, 0.5, nil},
		// n=6 at 15%: rank 1 → floor = min → nothing strictly below.
		{"small job at 15%", []float64{9, 1, 8, 7, 6, 5}, 0.15, nil},
		{"percentile above 1 clamps to max", []float64{1, 2, 3, 4}, 1.5, []string{"c00", "c01", "c02"}},
		{"NaN percentile → default", ramp, math.NaN(), []string{"c00", "c01"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := seq(tc.vals...)
			// A negative threshold disables dedupe (no distance is ≤ -1), isolating the floor.
			Classify(ss, Params{BlurPercentile: tc.pct, DedupeMaxHamming: -1})
			var blurry []string
			for _, s := range ss {
				if s.Rejected == RejectBlurry {
					blurry = append(blurry, s.ID)
				}
			}
			assert.ElementsMatch(t, tc.blurry, blurry)
		})
	}
}

// The job-wide floor never rejects a scene's sharpest frame that passed the exposure
// check; the rest of a low-texture scene still goes.
func TestClassifyBlurFloorSparesSceneBest(t *testing.T) {
	type frame struct {
		id       string
		scene    int
		ts       float64
		sharp    float64
		exposure float64
	}
	cases := []struct {
		name   string
		frames []frame
		pct    float64
		blurry []string
		kept   []string
	}{
		{
			// Floor: rank ceil(0.6·6)=4 → 100. Scene 1 is entirely below it.
			name: "whole scene below the floor keeps its best frame",
			frames: []frame{
				{"a0", 0, 0, 100, 0}, {"a1", 0, 1, 200, 0}, {"a2", 0, 2, 300, 0},
				{"b0", 1, 9, 10, 0}, {"b1", 1, 10, 30, 0}, {"b2", 1, 11, 20, 0},
			},
			pct:    0.6,
			blurry: []string{"b0", "b2"},
			kept:   []string{"a0", "a1", "a2", "b1"},
		},
		{
			// Floor: rank ceil(0.5·5)=3 → 200. a0 is not its scene's best and goes.
			name: "single-frame scene below the floor is kept",
			frames: []frame{
				{"a0", 0, 0, 100, 0}, {"a1", 0, 1, 200, 0}, {"a2", 0, 2, 300, 0}, {"a3", 0, 3, 400, 0},
				{"b0", 1, 9, 1, 0},
			},
			pct:    0.5,
			blurry: []string{"a0"},
			kept:   []string{"a1", "a2", "a3", "b0"},
		},
		{
			// The sharpest frame of scene 1 is blown out; the protection moves to the
			// sharpest one that can actually be used.
			name: "an exposure-rejected best frame passes the protection on",
			frames: []frame{
				{"a0", 0, 0, 100, 0}, {"a1", 0, 1, 200, 0}, {"a2", 0, 2, 300, 0},
				{"b0", 1, 9, 50, 0.9}, {"b1", 1, 10, 30, 0}, {"b2", 1, 11, 20, 0},
			},
			pct:    0.6,
			blurry: []string{"b2"},
			kept:   []string{"a0", "a1", "a2", "b1"},
		},
		{
			name: "ties: the earlier frame is the scene's best",
			frames: []frame{
				{"a0", 0, 0, 100, 0}, {"a1", 0, 1, 200, 0}, {"a2", 0, 2, 300, 0},
				{"b_late", 1, 11, 30, 0}, {"b_early", 1, 10, 30, 0}, {"b_soft", 1, 12, 20, 0},
			},
			pct:    0.6,
			blurry: []string{"b_late", "b_soft"},
			kept:   []string{"a0", "a1", "a2", "b_early"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ss := make([]Scored, len(tc.frames))
			for i, f := range tc.frames {
				ss[i] = scored(f.id, f.scene, f.ts, f.sharp, f.exposure, 0)
			}
			for _, order := range [][]Scored{ss, reversed(ss)} {
				Classify(order, Params{BlurPercentile: tc.pct, DedupeMaxHamming: -1})
				var blurry, kept []string
				for _, s := range order {
					switch s.Rejected {
					case RejectBlurry:
						blurry = append(blurry, s.ID)
					case "":
						kept = append(kept, s.ID)
					}
				}
				assert.ElementsMatch(t, tc.blurry, blurry, "blurry")
				assert.ElementsMatch(t, tc.kept, kept, "kept")
			}
		})
	}
}

func reversed(ss []Scored) []Scored {
	out := make([]Scored, len(ss))
	for i, s := range ss {
		out[len(ss)-1-i] = s
	}
	return out
}

func TestClassifyPrecedence(t *testing.T) {
	h := uint64(0x123456789ABCDEF0)
	ss := []Scored{
		scored("dark_blurry", 0, 0, 1, 0.9, distinctHash(1)), // exposure wins over blurry
		scored("blurry_dup", 0, 1, 2, 0, h),                  // blurry wins over duplicate
		scored("exposed_sharp", 0, 2, 500, 0.8, h^1),         // rejected: never a kept frame
		scored("kept", 0, 3, 400, 0, h^3),                    // not a duplicate of exposed_sharp
		scored("dup", 1, 4, 300, 0, h^7),                     // duplicate of kept (cross-scene)
		scored("other", 1, 5, 350, 0, distinctHash(0)),
		scored("other2", 1, 6, 360, 0, distinctHash(2)),
		scored("other3", 2, 7, 370, 0, distinctHash(3)),
	}
	// Sorted sharpness: 1, 2, 300, 350, 360, 370, 400, 500; at 30% the nearest rank is
	// ceil(2.4) = 3, so the floor is 300 and 1 and 2 are below it.
	Classify(ss, Params{BlurPercentile: 0.3, DedupeMaxHamming: 6})
	got := byID(ss)
	assert.Equal(t, RejectExposure, got["dark_blurry"].Rejected)
	assert.Equal(t, RejectBlurry, got["blurry_dup"].Rejected)
	assert.Empty(t, got["blurry_dup"].DuplicateOf)
	assert.Equal(t, RejectExposure, got["exposed_sharp"].Rejected)
	assert.Empty(t, got["kept"].Rejected, "an exposure-rejected frame does not suppress its twin")
	assert.Equal(t, RejectDuplicate, got["dup"].Rejected)
	assert.Equal(t, "kept", got["dup"].DuplicateOf)
	for _, id := range []string{"other", "other2", "other3"} {
		assert.Empty(t, got[id].Rejected, id)
	}
}

func TestClassifyDedupe(t *testing.T) {
	const (
		low6  = uint64(0x3F)       // bits 0-5
		next6 = uint64(0x3F) << 6  // bits 6-11
		far   = 0xFFFFFFFF00000000 // 32 bits from 0
	)
	p := Params{DedupeMaxHamming: 6, BlurPercentile: 0.01}

	t.Run("same scene compared first", func(t *testing.T) {
		// k1 (scene 1) is sharper and kept first; k0 (scene 0) is 12 bits from k1, so
		// both are kept. x (scene 0) is within 6 of both, and must point at k0.
		ss := []Scored{
			scored("x", 0, 2, 100, 0, 0),
			scored("k1", 1, 9, 300, 0, low6),
			scored("k0", 0, 1, 200, 0, next6),
		}
		Classify(ss, p)
		got := byID(ss)
		assert.Empty(t, got["k1"].Rejected)
		assert.Empty(t, got["k0"].Rejected)
		assert.Equal(t, RejectDuplicate, got["x"].Rejected)
		assert.Equal(t, "k0", got["x"].DuplicateOf)
	})

	t.Run("cross scene when no same-scene match", func(t *testing.T) {
		ss := []Scored{
			scored("x", 0, 2, 100, 0, 1),
			scored("k1", 1, 9, 300, 0, 0),
			scored("k0", 0, 1, 200, 0, far),
		}
		Classify(ss, p)
		got := byID(ss)
		assert.Equal(t, "k1", got["x"].DuplicateOf)
	})

	t.Run("keeps the sharper of a pair regardless of input order", func(t *testing.T) {
		ss := []Scored{scored("soft", 0, 0, 10, 0, 0), scored("sharp", 0, 1, 20, 0, 1)}
		Classify(ss, p)
		assert.Equal(t, "sharp", ss[0].DuplicateOf)
		assert.Empty(t, ss[1].Rejected)
	})

	t.Run("hamming boundary", func(t *testing.T) {
		ss := []Scored{
			scored("k", 0, 0, 100, 0, 0),
			scored("at", 0, 1, 90, 0, low6),      // distance 6 → duplicate
			scored("over", 0, 2, 80, 0, 0x7F<<8), // distance 7 → distinct
		}
		Classify(ss, p)
		got := byID(ss)
		assert.Equal(t, "k", got["at"].DuplicateOf)
		assert.Empty(t, got["over"].Rejected)
	})

	t.Run("zero threshold only merges identical hashes", func(t *testing.T) {
		ss := []Scored{scored("a", 0, 0, 3, 0, 42), scored("b", 0, 1, 2, 0, 42), scored("c", 0, 2, 1, 0, 43)}
		Classify(ss, Params{DedupeMaxHamming: 0, BlurPercentile: 0.01})
		got := byID(ss)
		assert.Equal(t, "a", got["b"].DuplicateOf)
		assert.Empty(t, got["c"].Rejected)
	})

	t.Run("ties: lower scene, then earlier timestamp, then ID", func(t *testing.T) {
		ss := []Scored{
			scored("z_late", 0, 5, 50, 0, 0),
			scored("scene1", 1, 0, 50, 0, 0),
			scored("b_early", 0, 1, 50, 0, 0),
			scored("a_early", 0, 1, 50, 0, 0),
		}
		Classify(ss, p)
		got := byID(ss)
		assert.Empty(t, got["a_early"].Rejected)
		for _, id := range []string{"b_early", "z_late", "scene1"} {
			assert.Equal(t, "a_early", got[id].DuplicateOf, id)
		}
	})
}

func TestClassifyDedupeShotPriority(t *testing.T) {
	shot := func(s Scored, shotID string) Scored { s.ShotID = shotID; return s }
	ranks := map[string]int{"rear_three_quarter": 0, "side_profile": 1, "rear": 2}
	classify := func(t *testing.T, ss []Scored, rank map[string]int) map[string]Scored {
		t.Helper()
		p := Params{DedupeMaxHamming: 6, BlurPercentile: 0.01, ShotRank: rank}
		rev := reversed(ss)
		Classify(ss, p)
		Classify(rev, p)
		assert.ElementsMatch(t, ss, rev, "independent of input order")
		return byID(ss)
	}

	t.Run("cross-shot pair keeps the higher-priority shot, even if softer", func(t *testing.T) {
		got := classify(t, []Scored{
			shot(scored("rear_a", 0, 1, 400, 0, 0), "rear"),
			shot(scored("rear_b", 0, 2, 390, 0, 1), "rear"),
			shot(scored("r3q_a", 1, 5, 300, 0, 2), "rear_three_quarter"),
			shot(scored("r3q_b", 1, 6, 290, 0, 3), "rear_three_quarter"),
		}, ranks)
		assert.Empty(t, got["r3q_a"].Rejected)
		assert.Equal(t, "r3q_a", got["r3q_b"].DuplicateOf, "same scene: the sharper one")
		assert.Equal(t, "r3q_a", got["rear_a"].DuplicateOf)
		assert.Equal(t, "r3q_a", got["rear_b"].DuplicateOf)
	})

	t.Run("same shot across scenes keeps the sharper", func(t *testing.T) {
		got := classify(t, []Scored{
			shot(scored("s0", 0, 1, 100, 0, 0), "side_profile"),
			shot(scored("s1", 3, 9, 200, 0, 1), "side_profile"),
		}, ranks)
		assert.Empty(t, got["s1"].Rejected)
		assert.Equal(t, "s1", got["s0"].DuplicateOf)
	})

	t.Run("other and unknown shots rank last and equal", func(t *testing.T) {
		got := classify(t, []Scored{
			shot(scored("other", 0, 1, 600, 0, 0), domain.OtherShotID),
			shot(scored("wheel", 1, 5, 500, 0, 1), "wheel"),
			shot(scored("rear", 2, 9, 100, 0, 3), "rear"),
		}, ranks)
		assert.Empty(t, got["rear"].Rejected, "a listed shot beats other/unknown ones")
		assert.Equal(t, "rear", got["other"].DuplicateOf)
		assert.Equal(t, "rear", got["wheel"].DuplicateOf)

		got = classify(t, []Scored{
			shot(scored("other", 0, 1, 500, 0, 0), domain.OtherShotID),
			shot(scored("wheel", 1, 5, 600, 0, 1), "wheel"),
		}, ranks)
		assert.Empty(t, got["wheel"].Rejected, "equal rank: the sharper one")
		assert.Equal(t, "wheel", got["other"].DuplicateOf)
	})

	t.Run("nil ShotRank keeps the sharper across shots", func(t *testing.T) {
		got := classify(t, []Scored{
			shot(scored("rear_a", 0, 1, 400, 0, 0), "rear"),
			shot(scored("r3q_a", 1, 5, 300, 0, 2), "rear_three_quarter"),
		}, nil)
		assert.Empty(t, got["rear_a"].Rejected)
		assert.Equal(t, "rear_a", got["r3q_a"].DuplicateOf)
	})
}

func TestClassifyIsIdempotent(t *testing.T) {
	ss := []Scored{
		scored("a", 0, 0, 100, 0, 0), scored("b", 0, 1, 50, 0, 1),
		scored("c", 1, 2, 10, 0.9, 2), scored("d", 1, 3, 70, 0, 0xFFFFFFFF00000000),
	}
	Classify(ss, Params{DedupeMaxHamming: 6})
	first := append([]Scored(nil), ss...)
	Classify(ss, Params{DedupeMaxHamming: 6})
	assert.Equal(t, first, ss)
}

// Guard against data races in the pool: run with -race.
func TestRunConcurrentCallsAreIndependent(t *testing.T) {
	dir := t.TempDir()
	p1 := writeJPEG(t, dir, "1.jpg", toRGBA(rampUp(fixW, fixH)))
	p2 := writeJPEG(t, dir, "2.jpg", toRGBA(bands(fixW, fixH)))
	cands := []extract.Candidate{cand("a", 0, 0, p1), cand("b", 0, 2, p2), cand("c", 1, 4, p1), cand("d", 1, 6, p2)}
	var wg sync.WaitGroup
	results := make([][]Scored, 4)
	for i := range results {
		wg.Go(func() {
			out, err := Run(context.Background(), cands, Params{DedupeMaxHamming: 6, Workers: 2})
			assert.NoError(t, err)
			results[i] = out
		})
	}
	wg.Wait()
	for i := 1; i < len(results); i++ {
		assert.Equal(t, results[0], results[i])
	}
}
