package score

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"runtime"
	"slices"

	"golang.org/x/sync/errgroup"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/extract"
)

// Plan defaults (plan/03-pipeline.md step 4).
//
// The blur floor is job-wide, as the plan says, but it never rejects the sharpest frame
// of a scene (among the frames that passed the exposure check). Laplacian variance
// depends on the content as much as on focus: a short, well-focused but low-texture
// scene (a dim dashboard next to a detailed exterior) can sit entirely below the job's
// 15th percentile, and a global floor alone would then wipe out every candidate of a
// shot the model found. The floor is meant to drop truly blurry frames; a scene's best
// frame is the best the video has for that moment, so it stays eligible (it may still
// be a duplicate, and the per-scene z-score still ranks it) and only the blurry
// outliers around it are dropped.
const (
	DefaultBlurPercentile      = 0.15
	DefaultExposureRejectShare = 0.40
	// MinCandidatesForBlurFloor: jobs with fewer candidates skip the blur floor. A
	// percentile of two or three frames says nothing about blur, and dropping any of
	// them would leave a tiny job with even fewer images to choose from.
	MinCandidatesForBlurFloor = 4
)

// DefaultParams returns Params for a domain with the plan defaults and the domain's
// shot priorities (ShotRanks).
func DefaultParams(d *domain.Config) Params {
	return Params{
		DedupeMaxHamming:    d.Selection.DedupeMaxHamming,
		BlurPercentile:      DefaultBlurPercentile,
		ExposureRejectShare: DefaultExposureRejectShare,
		ShotRank:            ShotRanks(d),
	}
}

// ShotRanks returns the dedupe priority of the domain's shots (Params.ShotRank): the
// required shots in list order, then the optional ones in list order, numbered from 0
// (a repeated ID, which domain validation rejects, is ranked once). Required shots come
// first because losing one to a near-duplicate of an optional shot turns into a
// "Missing:" line in the result, while the optional shot costs nothing when left out.
func ShotRanks(d *domain.Config) map[string]int {
	ranks := make(map[string]int, len(d.Shots))
	for _, required := range []bool{true, false} {
		for _, sh := range d.Shots {
			if _, seen := ranks[sh.ID]; !seen && sh.Required == required {
				ranks[sh.ID] = len(ranks)
			}
		}
	}
	return ranks
}

// Run scores every candidate (bounded worker pool) and marks rejections. The result has
// the same length and order as cands.
//
// Measurement (decode, grayscale, sharpness, exposure, dHash) runs on p.Workers
// goroutines (runtime.NumCPU() when 0). An unreadable or undecodable scoring copy is a
// terminal error naming the candidate and file. The classification that follows is
// single-threaded and deterministic; see Classify.
func Run(ctx context.Context, cands []extract.Candidate, p Params) ([]Scored, error) {
	return run(ctx, cands, p, measureFile)
}

// run is Run with the per-file measurement injectable, so tests can observe the pool.
func run(ctx context.Context, cands []extract.Candidate, p Params, measureFn func(string) (measurement, error)) ([]Scored, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("score: %w", err)
	}
	workers := p.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	out := make([]Scored, len(cands))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for i := range cands {
		if gctx.Err() != nil {
			break // a worker failed or ctx was cancelled; stop scheduling
		}
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			c := cands[i]
			m, err := measureFn(c.SmallPath)
			if err != nil {
				return fmt.Errorf("candidate %s: %w", c.ID, err)
			}
			out[i] = Scored{
				Candidate:       c,
				Sharpness:       m.Sharpness,
				ExposurePenalty: m.ExposurePenalty,
				DHash:           m.DHash,
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("score: %w", err)
	}
	// Workers that were already running may all have succeeded after a cancellation
	// stopped the scheduling loop; the result would then have zero-valued holes.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("score: %w", err)
	}
	Classify(out, p)
	return out, nil
}

// Classify fills SharpnessZ, Rejected and DuplicateOf of already-measured candidates
// in place, deterministically:
//
//  1. SharpnessZ per SceneIdx (population σ; 0 when the scene has <2 frames or σ=0).
//  2. Exposure: ExposurePenalty > ExposureRejectShare (0 → 0.40) → RejectExposure.
//  3. Blur floor: the nearest-rank BlurPercentile (0 → 0.15) of the Sharpness of ALL
//     candidates; frames strictly below it → RejectBlurry, except the sharpest
//     not-yet-rejected frame of each scene (ties: earlier TimestampSec, ID), which the
//     floor never rejects (see the package constants). Skipped entirely when there are
//     fewer than MinCandidatesForBlurFloor candidates.
//  4. Dedupe among the still-unrejected, in order of shot priority (ShotRank; equal
//     for every shot when it is nil), then descending Sharpness (ties: lower SceneIdx,
//     earlier TimestampSec, ID). Each frame is compared with the kept frames of its own
//     scene first, then with those of other scenes; the first within DedupeMaxHamming
//     makes it RejectDuplicate with DuplicateOf = that frame's ID. So of a duplicate
//     pair from one shot (or from shots of equal rank) the sharper frame is kept, and
//     of a pair from two shots the frame of the higher-priority shot, even when it is
//     the softer one: the selector enforces distinctness among the chosen images
//     anyway, and a required shot must not lose all its candidates to an optional one.
//
// A frame gets at most one reason, with the precedence exposure, blurry, duplicate.
// Previous SharpnessZ/Rejected/DuplicateOf values are overwritten.
func Classify(scored []Scored, p Params) {
	for i := range scored {
		scored[i].SharpnessZ = 0
		scored[i].Rejected = ""
		scored[i].DuplicateOf = ""
	}
	sceneZScores(scored)
	rejectExposure(scored, p.ExposureRejectShare)
	rejectBlurry(scored, p.BlurPercentile)
	dedupe(scored, p.DedupeMaxHamming, p.ShotRank)
}

// sceneZScores sets SharpnessZ within each scene.
func sceneZScores(scored []Scored) {
	byScene := make(map[int][]int)
	for i, s := range scored {
		byScene[s.SceneIdx] = append(byScene[s.SceneIdx], i)
	}
	for _, idx := range byScene {
		n := len(idx)
		if n < 2 {
			continue
		}
		// Sum in sorted order so the result does not depend on input order.
		vals := make([]float64, n)
		for k, i := range idx {
			vals[k] = scored[i].Sharpness
		}
		slices.Sort(vals)
		var sum float64
		for _, v := range vals {
			sum += v
		}
		mean := sum / float64(n)
		var ss float64
		for _, v := range vals {
			d := v - mean
			ss += d * d
		}
		sigma := math.Sqrt(ss / float64(n))
		// Equal values can leave a rounding-sized σ; treat it as zero spread.
		if sigma <= 1e-9*math.Max(1, math.Abs(mean)) || math.IsNaN(sigma) {
			continue
		}
		for _, i := range idx {
			scored[i].SharpnessZ = (scored[i].Sharpness - mean) / sigma
		}
	}
}

func rejectExposure(scored []Scored, share float64) {
	if !(share > 0) { // 0, negative or NaN → default
		share = DefaultExposureRejectShare
	}
	for i := range scored {
		if scored[i].ExposurePenalty > share {
			scored[i].Rejected = RejectExposure
		}
	}
}

// rejectBlurry applies the job-wide blur floor, sparing each scene's best frame.
func rejectBlurry(scored []Scored, pct float64) {
	floor, ok := blurFloor(scored, pct)
	if !ok {
		return
	}
	best := sceneBest(scored)
	for i := range scored {
		s := &scored[i]
		if s.Rejected == "" && s.Sharpness < floor && best[s.SceneIdx] != i {
			s.Rejected = RejectBlurry
		}
	}
}

// sceneBest returns, per SceneIdx, the index of the sharpest frame not rejected so far
// (ties: earlier TimestampSec, then ID, then input index).
func sceneBest(scored []Scored) map[int]int {
	best := make(map[int]int)
	for i := range scored {
		s := &scored[i]
		if s.Rejected != "" {
			continue
		}
		j, ok := best[s.SceneIdx]
		if !ok || sharperFirst(scored, i, j) < 0 {
			best[s.SceneIdx] = i
		}
	}
	return best
}

// sharperFirst orders frames by descending Sharpness, then earlier TimestampSec, then
// ID, then input index.
func sharperFirst(scored []Scored, a, b int) int {
	x, y := &scored[a], &scored[b]
	if c := cmp.Compare(y.Sharpness, x.Sharpness); c != 0 {
		return c
	}
	if c := cmp.Compare(x.TimestampSec, y.TimestampSec); c != 0 {
		return c
	}
	if c := cmp.Compare(x.ID, y.ID); c != 0 {
		return c
	}
	return cmp.Compare(a, b)
}

// blurFloor returns the nearest-rank pct percentile of the Sharpness of all candidates,
// and false when the floor does not apply (fewer than MinCandidatesForBlurFloor).
func blurFloor(scored []Scored, pct float64) (float64, bool) {
	n := len(scored)
	if n < MinCandidatesForBlurFloor {
		return 0, false
	}
	if !(pct > 0) { // 0, negative or NaN → default
		pct = DefaultBlurPercentile
	}
	pct = math.Min(pct, 1)
	vals := make([]float64, n)
	for i, s := range scored {
		vals[i] = s.Sharpness
	}
	slices.Sort(vals)
	// Nearest rank: the smallest value with at least pct·n values at or below it. The
	// epsilon stops 0.15·20 = 3.0000000000000004 from rounding up to rank 4.
	rank := int(math.Ceil(pct*float64(n) - 1e-9))
	rank = max(1, min(rank, n))
	return vals[rank-1], true
}

// dedupe marks near-duplicates among the unrejected candidates. Frames are visited in
// keep-preference order (shot priority, then sharpness), so a visited frame can only
// duplicate a frame that is preferred over it.
func dedupe(scored []Scored, maxHamming int, shotRank map[string]int) {
	rank := func(shotID string) int {
		if r, ok := shotRank[shotID]; ok {
			return r
		}
		return math.MaxInt // "other" and shots outside the domain: after every listed shot
	}
	order := make([]int, 0, len(scored))
	for i := range scored {
		if scored[i].Rejected == "" {
			order = append(order, i)
		}
	}
	slices.SortFunc(order, func(a, b int) int {
		x, y := &scored[a], &scored[b]
		if c := cmp.Compare(rank(x.ShotID), rank(y.ShotID)); c != 0 {
			return c
		}
		if c := cmp.Compare(y.Sharpness, x.Sharpness); c != 0 {
			return c
		}
		if c := cmp.Compare(x.SceneIdx, y.SceneIdx); c != 0 {
			return c
		}
		if c := cmp.Compare(x.TimestampSec, y.TimestampSec); c != 0 {
			return c
		}
		if c := cmp.Compare(x.ID, y.ID); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})

	var kept []int                 // all kept frames, in keep-preference order
	keptByScene := map[int][]int{} // the same, per scene
	for _, i := range order {
		c := &scored[i]
		dup := -1
		for _, k := range keptByScene[c.SceneIdx] {
			if Hamming(c.DHash, scored[k].DHash) <= maxHamming {
				dup = k
				break
			}
		}
		if dup < 0 {
			for _, k := range kept {
				if scored[k].SceneIdx != c.SceneIdx && Hamming(c.DHash, scored[k].DHash) <= maxHamming {
					dup = k
					break
				}
			}
		}
		if dup >= 0 {
			c.Rejected = RejectDuplicate
			c.DuplicateOf = scored[dup].ID
			continue
		}
		kept = append(kept, i)
		keptByScene[c.SceneIdx] = append(keptByScene[c.SceneIdx], i)
	}
}
