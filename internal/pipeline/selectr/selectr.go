package selectr

import (
	"cmp"
	"math"
	"slices"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/score"
)

// gapEpsilon absorbs float rounding in timestamps (PadStartSec + i/fps), so frames
// exactly MinGapSec apart count as far enough apart.
const gapEpsilon = 1e-9

// ParamsFor builds Params from a domain config.
func ParamsFor(d *domain.Config) Params {
	return Params{
		ImageCount:       d.Selection.ImageCount,
		Shots:            d.Shots,
		Weights:          d.Selection.Weights,
		DedupeMaxHamming: d.Selection.DedupeMaxHamming,
		MinGapSec:        MinGapSec,
	}
}

// Score = w.relevance·Relevance + w.sharpness·SharpnessZ − w.exposure·ExposurePenalty.
//
// This is the plan formula as is, without normalising the three terms (relevance and
// exposure are 0..1, SharpnessZ is unbounded around 0). Tuning the weights or the
// scaling is Phase 2 work, driven by the eval set.
func Score(c score.Scored, w domain.Weights) float64 {
	return w.Relevance*c.Relevance + w.Sharpness*c.SharpnessZ - w.Exposure*c.ExposurePenalty
}

// Select runs the coverage, fill and missing passes (plan/03-pipeline.md step 5).
// Rejected candidates are never selected.
//
// A candidate can be selected only if it is distinct from every frame already chosen:
// dHash Hamming distance > DedupeMaxHamming and at least MinGapSec (0 → MinGapSec)
// apart in time.
//
//  1. Coverage: for each shot in priority order, until ImageCount is reached, take the
//     first distinct eligible candidate of that shot, trying the re-rank's preferred
//     candidate first and then by score desc, timestamp asc, ID.
//  2. Fill: until ImageCount is reached, add the remaining eligible candidates of any
//     shot (including "other" and shots outside the list) by score desc, timestamp,
//     ID, skipping any that are not distinct.
//  3. Missing: the required shots with no selected image, in priority order.
//
// Images are ordered by shot priority (shots outside the list and "other" last), then
// timestamp, then ID. The result is deterministic for any input order as long as
// candidate IDs are unique.
func Select(cands []score.Scored, p Params) *Output {
	out := &Output{
		Images:       []Selected{},
		Scores:       make(map[string]float64, len(cands)),
		MissingShots: []domain.Shot{},
	}
	s := &selector{
		cands:  cands,
		scores: make([]float64, len(cands)),
		taken:  make([]bool, len(cands)),
		p:      p,
		gap:    p.MinGapSec,
	}
	if s.gap <= 0 {
		s.gap = MinGapSec
	}
	for i, c := range cands {
		s.scores[i] = Score(c, p.Weights)
		out.Scores[c.ID] = s.scores[i]
	}

	s.coverage()
	s.fill()

	covered := make(map[string]bool, len(s.chosen))
	for _, i := range s.chosen {
		covered[cands[i].ShotID] = true
	}
	seen := make(map[string]bool, len(p.Shots))
	for _, sh := range p.Shots {
		if seen[sh.ID] {
			continue
		}
		seen[sh.ID] = true
		if sh.Required && !covered[sh.ID] {
			out.MissingShots = append(out.MissingShots, sh)
		}
	}

	s.orderForDisplay()
	for _, i := range s.chosen {
		c := cands[i]
		out.Images = append(out.Images, Selected{
			Candidate: c,
			ShotID:    c.ShotID,
			Score:     s.scores[i],
			Reason:    c.SceneReason,
		})
	}
	out.Explanation = explain(len(out.Images), p.ImageCount, out.MissingShots)
	return out
}

// selector holds the state of one Select call. Candidates are referred to by index.
type selector struct {
	cands  []score.Scored
	scores []float64
	taken  []bool
	chosen []int // selection order until orderForDisplay
	p      Params
	gap    float64
}

func (s *selector) full() bool { return len(s.chosen) >= s.p.ImageCount }

func (s *selector) eligible(i int) bool { return !s.taken[i] && s.cands[i].Rejected == "" }

func (s *selector) take(i int) {
	s.taken[i] = true
	s.chosen = append(s.chosen, i)
}

// distinct reports whether candidate i is far enough, in dHash and in time, from every
// frame chosen so far.
func (s *selector) distinct(i int) bool {
	x := &s.cands[i]
	for _, j := range s.chosen {
		y := &s.cands[j]
		if score.Hamming(x.DHash, y.DHash) <= s.p.DedupeMaxHamming {
			return false
		}
		if math.Abs(x.TimestampSec-y.TimestampSec) < s.gap-gapEpsilon {
			return false
		}
	}
	return true
}

// byScore orders candidate indices by score desc, then timestamp asc, then ID (and
// input index as a last resort, for duplicate IDs).
func (s *selector) byScore(a, b int) int {
	if c := cmp.Compare(s.scores[b], s.scores[a]); c != 0 {
		return c
	}
	x, y := &s.cands[a], &s.cands[b]
	if c := cmp.Compare(x.TimestampSec, y.TimestampSec); c != 0 {
		return c
	}
	if c := cmp.Compare(x.ID, y.ID); c != 0 {
		return c
	}
	return cmp.Compare(a, b)
}

func (s *selector) coverage() {
	byShot := make(map[string][]int)
	for i := range s.cands {
		if s.eligible(i) {
			byShot[s.cands[i].ShotID] = append(byShot[s.cands[i].ShotID], i)
		}
	}
	seen := make(map[string]bool, len(s.p.Shots))
	for _, sh := range s.p.Shots {
		if s.full() {
			return
		}
		if seen[sh.ID] {
			continue // a shot listed twice is covered once
		}
		seen[sh.ID] = true
		group := byShot[sh.ID]
		slices.SortFunc(group, s.byScore)
		if pref, ok := s.p.Preferred[sh.ID]; ok && pref != "" {
			if k := slices.IndexFunc(group, func(i int) bool { return s.cands[i].ID == pref }); k > 0 {
				// Move the preferred candidate to the front, keeping the rest in order.
				i := group[k]
				copy(group[1:k+1], group[:k])
				group[0] = i
			}
		}
		for _, i := range group {
			if s.distinct(i) {
				s.take(i)
				break
			}
		}
	}
}

func (s *selector) fill() {
	if s.full() {
		return
	}
	rest := make([]int, 0, len(s.cands))
	for i := range s.cands {
		if s.eligible(i) {
			rest = append(rest, i)
		}
	}
	slices.SortFunc(rest, s.byScore)
	for _, i := range rest {
		if s.full() {
			return
		}
		// Distinctness only gets stricter as frames are added, so a single pass in score
		// order equals repeatedly taking the best remaining distinct candidate.
		if s.distinct(i) {
			s.take(i)
		}
	}
}

// orderForDisplay sorts the chosen frames by shot priority, then timestamp, then ID.
func (s *selector) orderForDisplay() {
	rank := make(map[string]int, len(s.p.Shots))
	for k, sh := range s.p.Shots {
		if _, ok := rank[sh.ID]; !ok {
			rank[sh.ID] = k
		}
	}
	last := len(s.p.Shots) // "other" and shots outside the list
	shotRank := func(id string) int {
		if r, ok := rank[id]; ok && id != domain.OtherShotID {
			return r
		}
		return last
	}
	slices.SortFunc(s.chosen, func(a, b int) int {
		x, y := &s.cands[a], &s.cands[b]
		if c := cmp.Compare(shotRank(x.ShotID), shotRank(y.ShotID)); c != 0 {
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
}
