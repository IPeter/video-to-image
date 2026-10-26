package extract

import (
	"math"
	"sort"

	"video-to-image/internal/media"
)

// Defaults applied by Plan when the domain config leaves a value unset (≤ 0).
const (
	DefaultSampleFPS     = 4
	DefaultMaxCandidates = 300
)

// Plan computes padded ranges and per-range fps. If the total expected frames would
// exceed MaxCandidates, every range's fps is lowered proportionally (never truncating
// ranges). Pure function; no I/O.
//
// Per range, in input order:
//   - Ranges with non-finite bounds, EndSec < StartSec, or StartSec ≥ DurationSec are
//     skipped, as are ranges whose padded window is empty. A zero-length range (a
//     single instant) is kept: padding turns it into a 1 s window.
//   - PadStartSec = max(0, StartSec − pad), PadEndSec = min(DurationSec, EndSec + pad),
//     with pad = Input.PaddingSec (PaddingSec when ≤ 0).
//   - FPS = SampleFPS (DefaultSampleFPS when ≤ 0); ranges whose unpadded length,
//     clamped to the video, is below ShortRangeSec use max(FPS, ShortRangeFPS).
//   - Expected = media.ExpectedFrames(PadEnd − PadStart, FPS) = max(1, ⌊len·FPS⌋),
//     exactly what media.ExtractFrames writes for a window inside the video.
//
// Cap (MaxCandidates, DefaultMaxCandidates when ≤ 0): when Σ Expected exceeds it,
// every FPS is multiplied by the same factor s ∈ (0, 1), the largest one for which
// Σ Expected ≤ MaxCandidates (found by bisection, because the one-frame minimum
// makes Σ Expected non-linear in s; s is at least roughly MaxCandidates/Σ when no
// range sits at the minimum). Every kept range keeps at least one frame. When there
// are more ranges than MaxCandidates, so even one frame each would exceed the cap,
// only the MaxCandidates most relevant ranges are kept (ties: earlier in the input
// first), still in input order.
func Plan(in Input) []RangePlan {
	dur := in.DurationSec
	if !finite(dur) || dur <= 0 {
		return nil
	}
	baseFPS := float64(in.SampleFPS)
	if baseFPS <= 0 {
		baseFPS = DefaultSampleFPS
	}
	maxCand := in.MaxCandidates
	if maxCand <= 0 {
		maxCand = DefaultMaxCandidates
	}

	pad := in.PaddingSec
	if !(pad > 0) || math.IsInf(pad, 0) {
		pad = PaddingSec
	}
	plans := make([]RangePlan, 0, len(in.Ranges))
	for _, r := range in.Ranges {
		if !finite(r.StartSec) || !finite(r.EndSec) || r.EndSec < r.StartSec || r.StartSec >= dur {
			continue
		}
		padStart := math.Max(0, r.StartSec-pad)
		padEnd := math.Min(dur, r.EndSec+pad)
		if padEnd-padStart <= 0 {
			continue
		}
		fps := baseFPS
		if unpadded := math.Min(r.EndSec, dur) - math.Max(r.StartSec, 0); unpadded < ShortRangeSec {
			fps = math.Max(fps, ShortRangeFPS)
		}
		plans = append(plans, RangePlan{
			Range:       r,
			PadStartSec: padStart,
			PadEndSec:   padEnd,
			FPS:         fps,
			Expected:    media.ExpectedFrames(padEnd-padStart, fps),
		})
	}

	if len(plans) > maxCand {
		plans = mostRelevant(plans, maxCand)
	}
	if totalExpected(plans) > maxCand {
		s := capScale(plans, maxCand)
		for i := range plans {
			plans[i].FPS *= s
			plans[i].Expected = media.ExpectedFrames(plans[i].PadEndSec-plans[i].PadStartSec, plans[i].FPS)
		}
	}
	return plans
}

// capScale returns the largest s ∈ (0, 1) with Σ ExpectedFrames(len_i, fps_i·s) ≤
// maxCand. It requires len(plans) ≤ maxCand, so tiny s (one frame per range) fits.
func capScale(plans []RangePlan, maxCand int) float64 {
	fits := func(s float64) bool {
		n := 0
		for _, p := range plans {
			n += media.ExpectedFrames(p.PadEndSec-p.PadStartSec, p.FPS*s)
		}
		return n <= maxCand
	}
	lo, hi := 0.0, 1.0 // fits(lo) (in the limit), !fits(hi)
	if guess := float64(maxCand) / float64(totalExpected(plans)); fits(guess) {
		lo = guess
	} else {
		hi = guess
	}
	for range 64 {
		mid := (lo + hi) / 2
		if mid <= lo || mid >= hi {
			break
		}
		if fits(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	if lo == 0 {
		// Unreachable when len(plans) ≤ maxCand; fall back to the smallest positive
		// factor bisection could reach so fps stays positive.
		lo = hi / 2
	}
	return lo
}

// mostRelevant keeps the n most relevant plans (stable on ties), in their original order.
func mostRelevant(plans []RangePlan, n int) []RangePlan {
	idx := make([]int, len(plans))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return plans[idx[a]].Relevance > plans[idx[b]].Relevance })
	keep := idx[:n]
	sort.Ints(keep)
	out := make([]RangePlan, 0, n)
	for _, i := range keep {
		out = append(out, plans[i])
	}
	return out
}

func totalExpected(plans []RangePlan) int {
	n := 0
	for _, p := range plans {
		n += p.Expected
	}
	return n
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
