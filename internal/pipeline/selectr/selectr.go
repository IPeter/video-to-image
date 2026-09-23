package selectr

import (
	"cmp"
	"math"
	"slices"
	"strings"

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
		MaxPerShot:       d.Selection.EffectiveMaxPerShot(),
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
// Rejected candidates are never selected. When the model review ran (Assessments
// non-nil), only candidates it assessed as usable are eligible, and each one counts
// as the view the review saw (Assessment.View; views outside the shot list count as
// "other") instead of its scene's shot: that is its effective shot.
//
// A candidate can be selected only if it is distinct from every frame already chosen
// (dHash Hamming distance > DedupeMaxHamming, at least MinGapSec (0 → MinGapSec) apart
// in time and not linked to it by the review's sameAs, directly or through a chain of
// links) and, when MaxPerShot > 0, fewer than MaxPerShot frames of its effective shot
// are chosen. The cap applies to "other" and to shots outside the list too.
//
// Candidates are ranked by review quality desc (all equal without a review), then
// score desc, timestamp asc, ID.
//
//  1. Coverage: for each shot in priority order, until ImageCount is reached, take the
//     first selectable candidate of that effective shot, trying the re-rank's preferred
//     candidate first and then by rank.
//  2. Fill: until ImageCount is reached, add the remaining eligible candidates of any
//     shot (including "other" and shots outside the list) by rank, skipping any that
//     are not selectable.
//  3. Missing: the required shots with no selected image, in priority order.
//
// Images are ordered by shot priority (shots outside the list and "other" last), then
// timestamp, then ID. Selected.ShotID is the effective shot. Selected.Reason is the
// review's reason when it gave one, else the scene's; the scene's reason describes the
// scene's shot, so a frame the review re-classified gets a neutral text for its
// effective shot instead (reclassifiedReason). The result is deterministic for any
// input order as long as candidate IDs are unique.
func Select(cands []score.Scored, p Params) *Output {
	out := &Output{
		Images:       []Selected{},
		Scores:       make(map[string]float64, len(cands)),
		MissingShots: []domain.Shot{},
	}
	s := newSelector(cands, p)
	for i, c := range cands {
		out.Scores[c.ID] = s.scores[i]
	}

	s.coverage()
	s.fill()

	seen := make(map[string]bool, len(p.Shots))
	for _, sh := range p.Shots {
		if seen[sh.ID] {
			continue
		}
		seen[sh.ID] = true
		if sh.Required && s.perShot[sh.ID] == 0 {
			out.MissingShots = append(out.MissingShots, sh)
		}
	}

	s.orderForDisplay()
	for _, i := range s.chosen {
		c := cands[i]
		reason := c.SceneReason
		switch a := p.Assessments[c.ID]; {
		case a.Reason != "":
			reason = a.Reason
		case s.reclassified[i]:
			reason = s.reclassifiedReason(s.shot[i])
		}
		out.Images = append(out.Images, Selected{
			Candidate: c,
			ShotID:    s.shot[i],
			Score:     s.scores[i],
			Reason:    reason,
		})
	}
	out.Explanation = explain(len(out.Images), p.ImageCount, out.MissingShots)
	return out
}

// selector holds the state of one Select call. Candidates are referred to by index.
type selector struct {
	cands        []score.Scored
	scores       []float64
	shot         []string // effective shot ID
	reclassified []bool   // the review's view is not the scene's shot (unlisted → "other" on both sides)
	quality      []int    // review quality, 0 without a review
	usable       []bool   // eligible apart from being taken: not rejected, usable if reviewed
	taken        []bool
	chosen       []int                  // selection order until orderForDisplay
	perShot      map[string]int         // chosen frames by effective shot
	shots        map[string]domain.Shot // listed shots by ID (the first entry of an ID)
	viewGroup    map[string]string      // sameAs-linked candidate ID → its group (viewGroups)
	p            Params
	gap          float64
	maxShots     int // <= 0: no cap
}

func newSelector(cands []score.Scored, p Params) *selector {
	s := &selector{
		cands:        cands,
		scores:       make([]float64, len(cands)),
		shot:         make([]string, len(cands)),
		reclassified: make([]bool, len(cands)),
		quality:      make([]int, len(cands)),
		usable:       make([]bool, len(cands)),
		taken:        make([]bool, len(cands)),
		perShot:      make(map[string]int),
		shots:        make(map[string]domain.Shot, len(p.Shots)),
		viewGroup:    viewGroups(p.Assessments),
		p:            p,
		gap:          p.MinGapSec,
		maxShots:     p.MaxPerShot,
	}
	if s.gap <= 0 {
		s.gap = MinGapSec
	}
	for _, sh := range p.Shots {
		if _, dup := s.shots[sh.ID]; !dup {
			s.shots[sh.ID] = sh
		}
	}
	// With a review, views outside the shot list count as "other".
	effective := func(shot string) string {
		if _, listed := s.shots[shot]; listed {
			return shot
		}
		return domain.OtherShotID
	}
	for i, c := range cands {
		s.scores[i] = Score(c, p.Weights)
		s.shot[i] = c.ShotID
		s.usable[i] = c.Rejected == ""
		if p.Assessments == nil {
			continue
		}
		a, ok := p.Assessments[c.ID]
		s.usable[i] = s.usable[i] && ok && a.Usable
		s.quality[i] = a.Quality
		s.shot[i] = effective(a.View)
		s.reclassified[i] = ok && s.shot[i] != effective(c.ShotID)
	}
	return s
}

// otherReason is the reason of a frame the review re-classified as "other".
const otherReason = "Other view"

// reclassifiedReason is the reason of a selected frame whose effective shot is not the
// scene's: "<label> view" of the effective shot (what the review matched the frame to);
// otherReason for "other". Shot descriptions are not used: they are guidance written for
// the model ("this is the hero shot", "screen lit"), not a caption of a checked frame.
func (s *selector) reclassifiedReason(shot string) string {
	sh, ok := s.shots[shot]
	if !ok {
		return otherReason
	}
	if l := strings.TrimSpace(sh.Label); l != "" {
		return l + " view"
	}
	return sh.ID + " view"
}

// viewGroups resolves the review's DuplicateOf links transitively (union-find by
// candidate ID): it maps every candidate ID at either end of a link to a representative
// of its group, so b sameAs a and c sameAs b put a, b and c in one group. The model
// gives one sameAs per frame, often only to the nearest earlier frame, so a direct-link
// check alone could still select a and c. nil without links.
func viewGroups(as map[string]Assessment) map[string]string {
	parent := make(map[string]string)
	find := func(id string) string {
		root := id
		for {
			p, ok := parent[root]
			if !ok {
				parent[root] = root
				break
			}
			if p == root {
				break
			}
			root = p
		}
		for id != root { // path compression
			next := parent[id]
			parent[id] = root
			id = next
		}
		return root
	}
	for id, a := range as {
		if a.DuplicateOf == "" {
			continue
		}
		if ra, rb := find(id), find(a.DuplicateOf); ra != rb {
			parent[ra] = rb
		}
	}
	if len(parent) == 0 {
		return nil
	}
	groups := make(map[string]string, len(parent))
	for id := range parent {
		groups[id] = find(id)
	}
	return groups
}

func (s *selector) full() bool { return len(s.chosen) >= s.p.ImageCount }

func (s *selector) eligible(i int) bool { return !s.taken[i] && s.usable[i] }

// selectable reports whether eligible candidate i can join the frames chosen so far:
// its effective shot is under the cap (if any) and it is distinct from all of them.
func (s *selector) selectable(i int) bool {
	return (s.maxShots <= 0 || s.perShot[s.shot[i]] < s.maxShots) && s.distinct(i)
}

func (s *selector) take(i int) {
	s.taken[i] = true
	s.chosen = append(s.chosen, i)
	s.perShot[s.shot[i]]++
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
		if s.sameView(x.ID, y.ID) {
			return false
		}
		if math.Abs(x.TimestampSec-y.TimestampSec) < s.gap-gapEpsilon {
			return false
		}
	}
	return true
}

// byRank orders candidate indices by review quality desc, score desc, then timestamp
// asc, then ID (and input index as a last resort, for duplicate IDs).
func (s *selector) byRank(a, b int) int {
	if c := cmp.Compare(s.quality[b], s.quality[a]); c != 0 {
		return c
	}
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
			byShot[s.shot[i]] = append(byShot[s.shot[i]], i)
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
		slices.SortFunc(group, s.byRank)
		if pref, ok := s.p.Preferred[sh.ID]; ok && pref != "" {
			if k := slices.IndexFunc(group, func(i int) bool { return s.cands[i].ID == pref }); k > 0 {
				// Move the preferred candidate to the front, keeping the rest in order.
				i := group[k]
				copy(group[1:k+1], group[:k])
				group[0] = i
			}
		}
		for _, i := range group {
			if s.selectable(i) {
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
	slices.SortFunc(rest, s.byRank)
	for _, i := range rest {
		if s.full() {
			return
		}
		// The cap and distinctness only get stricter as frames are added, so a single
		// pass in rank order equals repeatedly taking the best remaining selectable
		// candidate.
		if s.selectable(i) {
			s.take(i)
		}
	}
}

// orderForDisplay sorts the chosen frames by effective shot priority, then timestamp,
// then ID.
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
		if c := cmp.Compare(shotRank(s.shot[a]), shotRank(s.shot[b])); c != 0 {
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
	})
}

// sameView reports whether the review linked a and b as near-duplicates, directly
// (either way) or through a chain of sameAs links.
func (s *selector) sameView(a, b string) bool {
	ga, ok := s.viewGroup[a]
	if !ok {
		return false
	}
	gb, ok := s.viewGroup[b]
	return ok && ga == gb
}
