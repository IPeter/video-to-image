package selectr

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/extract"
	"video-to-image/internal/pipeline/score"
)

// Car-like shot list: four required exterior/interior shots, then optional ones.
var testShots = []domain.Shot{
	{ID: "front_three_quarter", Label: "Front 3/4", Required: true},
	{ID: "side_profile", Label: "Side profile", Required: true},
	{ID: "rear_three_quarter", Label: "Rear 3/4", Required: true},
	{ID: "interior_dashboard", Label: "Dashboard", Required: true},
	{ID: "front", Label: "Front"},
	{ID: "rear", Label: "Rear"},
	{ID: "interior_front_seats", Label: "Front seats"},
	{ID: "interior_rear_seats", Label: "Rear seats"},
	{ID: "odometer", Label: "Odometer"},
	{ID: "wheel", Label: "Wheel & tyre"},
}

var carWeights = domain.Weights{Relevance: 0.5, Sharpness: 0.4, Exposure: 0.1}

// params is the car-like setup, one image per shot (cars.yaml: maxPerShot 1).
func params(imageCount int) Params {
	return Params{ImageCount: imageCount, Shots: testShots, Weights: carWeights, DedupeMaxHamming: 6, MaxPerShot: 1}
}

// code spreads the bits of k over bytes, so any two codes are ≥ 8 bits apart (> the
// dedupe threshold of 6), and code(k)^1 is a near-duplicate of code(k) only.
func code(k int) uint64 {
	var h uint64
	for j := range 8 {
		if k>>j&1 == 1 {
			h |= 0xFF << (8 * j)
		}
	}
	return h
}

// mk builds a scored candidate with score 0.5·rel + 0.4·z (exposure 0).
func mk(id, shot string, ts, rel, z float64, hashCode int) score.Scored {
	return score.Scored{
		Candidate: extract.Candidate{
			ID: id, ShotID: shot, TimestampSec: ts, Relevance: rel, SceneReason: "reason " + id,
		},
		SharpnessZ: z,
		DHash:      code(hashCode),
	}
}

func ids(out *Output) []string {
	r := make([]string, len(out.Images))
	for i, s := range out.Images {
		r[i] = s.Candidate.ID
	}
	return r
}

// selShots returns the ShotID of every selected image.
func selShots(out *Output) []string {
	r := make([]string, len(out.Images))
	for i, s := range out.Images {
		r[i] = s.ShotID
	}
	return r
}

func shotIDs(shots []domain.Shot) []string {
	r := make([]string, len(shots))
	for i, s := range shots {
		r[i] = s.ID
	}
	return r
}

func TestCodesAreDistinct(t *testing.T) {
	for a := range 64 {
		for b := a + 1; b < 64; b++ {
			require.Greater(t, score.Hamming(code(a), code(b)), 6)
			require.Greater(t, score.Hamming(code(a)^1, code(b)), 6)
		}
		require.LessOrEqual(t, score.Hamming(code(a)^1, code(a)), 6)
	}
}

func TestScore(t *testing.T) {
	cases := []struct {
		rel, z, exp float64
		w           domain.Weights
		want        float64
	}{
		{0.9, 1.5, 0.1, carWeights, 0.5*0.9 + 0.4*1.5 - 0.1*0.1},
		{0.5, -2, 0, carWeights, 0.25 - 0.8},
		{1, 0, 1, domain.Weights{Exposure: 1}, -1},
		{0.7, 3, 0.2, domain.Weights{}, 0},
	}
	for _, tc := range cases {
		c := score.Scored{Candidate: extract.Candidate{Relevance: tc.rel}, SharpnessZ: tc.z, ExposurePenalty: tc.exp}
		assert.InDelta(t, tc.want, Score(c, tc.w), 1e-12)
	}
}

func TestParamsForCars(t *testing.T) {
	reg, err := domain.LoadDir(filepath.Join("..", "..", "..", "configs", "domains"))
	require.NoError(t, err)
	cars, ok := reg.Get("cars")
	require.True(t, ok)
	p := ParamsFor(cars)
	assert.Equal(t, cars.Selection.ImageCount, p.ImageCount)
	assert.Equal(t, cars.Shots, p.Shots)
	assert.Equal(t, cars.Selection.Weights, p.Weights)
	assert.Equal(t, cars.Selection.DedupeMaxHamming, p.DedupeMaxHamming)
	assert.Equal(t, MinGapSec, p.MinGapSec)
	assert.Equal(t, cars.Selection.EffectiveMaxPerShot(), p.MaxPerShot)
	assert.Equal(t, 1, p.MaxPerShot, "cars: one photo per angle")
	assert.Nil(t, p.Preferred)
	assert.Nil(t, p.Assessments)

	noCap := *cars
	noCap.Selection.MaxPerShot = 0
	assert.Equal(t, 0, ParamsFor(&noCap).MaxPerShot, "a domain without maxPerShot: no cap")
}

func TestSelectFullCoverage(t *testing.T) {
	var cands []score.Scored
	for k, sh := range testShots {
		base := float64(10 * k)
		cands = append(cands,
			mk(sh.ID+"_best", sh.ID, base+5, 0.9, 1, 2*k),
			mk(sh.ID+"_second", sh.ID, base+1, 0.6, 0, 2*k+1),
		)
	}
	// A very high-scoring "other" frame must not displace a shot during coverage.
	cands = append(cands, mk("other_top", domain.OtherShotID, 300, 1, 3, 40))

	out := Select(cands, params(8))

	want := []string{}
	for _, sh := range testShots[:8] {
		want = append(want, sh.ID+"_best")
	}
	assert.Equal(t, want, ids(out))
	assert.Empty(t, out.MissingShots)
	assert.Equal(t, "8 images selected.", out.Explanation)
	assert.Len(t, out.Scores, len(cands))

	img := out.Images[0]
	assert.Equal(t, "front_three_quarter", img.ShotID)
	assert.Equal(t, "reason front_three_quarter_best", img.Reason)
	assert.InDelta(t, 0.5*0.9+0.4*1, img.Score, 1e-12)
	assert.InDelta(t, out.Scores[img.Candidate.ID], img.Score, 0)
	assert.InDelta(t, 0.5*1+0.4*3, out.Scores["other_top"], 1e-12)
}

func TestSelectFewerThanImageCount(t *testing.T) {
	cands := []score.Scored{
		mk("f34", "front_three_quarter", 2, 0.9, 1, 1),
		mk("side", "side_profile", 8, 0.8, 0.5, 2),
		mk("front_a", "front", 14, 0.7, 0.2, 3),
		mk("front_b", "front", 16, 0.6, 0.1, 4),
		mk("oth", domain.OtherShotID, 20, 0.5, 0, 5),
	}
	out := Select(cands, params(8))
	// front_b is a second "front" view: the cap of 1 (params) keeps it out.
	assert.Equal(t, []string{"f34", "side", "front_a", "oth"}, ids(out))
	assert.Equal(t, []string{"rear_three_quarter", "interior_dashboard"}, shotIDs(out.MissingShots))
	assert.Equal(t, "Only 4 distinct high-quality views were found. Missing: Rear 3/4, Dashboard.", out.Explanation)

	p := params(8)
	p.MaxPerShot = 2
	out = Select(cands, p)
	assert.Equal(t, []string{"f34", "side", "front_a", "front_b", "oth"}, ids(out))
	assert.Equal(t, "Only 5 distinct high-quality views were found. Missing: Rear 3/4, Dashboard.", out.Explanation)
}

func TestSelectFullButRequiredShotsMissing(t *testing.T) {
	cands := []score.Scored{
		mk("f34", "front_three_quarter", 2, 0.9, 1, 1),
		mk("side", "side_profile", 8, 0.8, 0.5, 2),
		mk("front", "front", 14, 0.7, 0.2, 3),
		mk("rear", "rear", 20, 0.7, 0.2, 4),
	}
	out := Select(cands, params(3))
	assert.Equal(t, []string{"f34", "side", "front"}, ids(out))
	assert.Equal(t, "3 images selected. Not clearly visible in the video: Rear 3/4, Dashboard.", out.Explanation)
}

func TestSelectDuplicatesAndGapInCoverage(t *testing.T) {
	cands := []score.Scored{
		mk("a1", "front_three_quarter", 10, 0.9, 1, 1),
		// side_profile: best is a hash duplicate of a1, next is 0.5 s from a1, next is
		// exactly 1 s from a1 → the third one covers the shot.
		mk("b_dup", "side_profile", 30, 0.9, 1, 0),
		mk("b_close", "side_profile", 10.5, 0.8, 1, 2),
		mk("b_ok", "side_profile", 11, 0.7, 1, 3),
	}
	cands[1].DHash = code(1) ^ 1
	out := Select(cands, params(2))
	assert.Equal(t, []string{"a1", "b_ok"}, ids(out))
}

func TestSelectDuplicatesAndGapInFill(t *testing.T) {
	cands := []score.Scored{
		mk("a1", "front_three_quarter", 10, 0.9, 1, 1),
		mk("f_dup", "front_three_quarter", 40, 0.9, 0.9, 0), // hash twin of a1
		mk("f_near", "front_three_quarter", 10.99, 0.9, 0.8, 2),
		mk("f_ok", "front_three_quarter", 9, 0.9, 0.7, 3), // exactly 1 s before a1
		mk("f_near_ok", "front_three_quarter", 9.5, 0.9, 0.6, 4),
		mk("o1", domain.OtherShotID, 20, 0.5, 0, 5),
		mk("o1_twin", domain.OtherShotID, 50, 0.5, -0.1, 6),
	}
	cands[1].DHash = code(1) ^ 1
	cands[6].DHash = code(5) ^ 1
	p := params(8)
	p.MaxPerShot = 10 // this test is about distinctness, not the per-shot cap
	out := Select(cands, p)
	// a1 by coverage; fill in score order: f_dup (dup), f_near (0.99 s), f_ok (ok),
	// f_near_ok (0.5 s from f_ok), o1 (ok), o1_twin (dup of o1).
	assert.Equal(t, []string{"f_ok", "a1", "o1"}, ids(out))
	assert.Equal(t, "Only 3 distinct high-quality views were found. Missing: Side profile, Rear 3/4, Dashboard.", out.Explanation)

	// With the cap of 1 (params) the fill pass adds no second front 3/4 or other frame.
	out = Select(cands, params(8))
	assert.Equal(t, []string{"a1", "o1"}, ids(out))
}

func TestSelectMinGap(t *testing.T) {
	cands := []score.Scored{
		mk("a", "front_three_quarter", 10, 0.9, 1, 1),
		mk("b", "front_three_quarter", 12, 0.9, 0.5, 2),
		mk("c", "front_three_quarter", 13, 0.9, 0.2, 3),
	}
	p := params(8)
	p.MaxPerShot = 3 // this test is about the gap, not the per-shot cap
	p.MinGapSec = 0  // → MinGapSec (1 s): all three are ≥ 1 s apart
	assert.Equal(t, []string{"a", "b", "c"}, ids(Select(cands, p)))

	p.MinGapSec = 2.5 // b is 2 s from a; c is 3 s from a
	assert.Equal(t, []string{"a", "c"}, ids(Select(cands, p)))
}

func TestSelectHammingBoundary(t *testing.T) {
	cands := []score.Scored{
		mk("a", "front_three_quarter", 10, 0.9, 1, 0),
		mk("at", "side_profile", 20, 0.9, 1, 0),
		mk("over", "rear_three_quarter", 30, 0.9, 1, 0),
	}
	cands[1].DHash = 0x3F // 6 bits from a → not distinct
	cands[2].DHash = 0x7F // 7 bits from a, 1 from "at" (which is not selected)
	out := Select(cands, params(8))
	assert.Equal(t, []string{"a", "over"}, ids(out))
}

func TestSelectNeverSelectsRejected(t *testing.T) {
	cands := []score.Scored{
		mk("blurry", "front_three_quarter", 1, 1, 3, 1),
		mk("exposed", "front_three_quarter", 5, 1, 3, 2),
		mk("dup", "side_profile", 9, 1, 3, 3),
		mk("ok", "front_three_quarter", 13, 0.2, -1, 4),
		mk("other_rej", domain.OtherShotID, 17, 1, 3, 5),
	}
	cands[0].Rejected = score.RejectBlurry
	cands[1].Rejected = score.RejectExposure
	cands[2].Rejected = score.RejectDuplicate
	cands[2].DuplicateOf = "ok"
	cands[4].Rejected = score.RejectBlurry
	out := Select(cands, params(8))
	assert.Equal(t, []string{"ok"}, ids(out))
	assert.Len(t, out.Scores, 5, "rejected candidates are scored too")
	assert.Equal(t, "Only 1 distinct high-quality view was found. Missing: Side profile, Rear 3/4, Dashboard.", out.Explanation)
}

func TestSelectPreferred(t *testing.T) {
	base := func() []score.Scored {
		return []score.Scored{
			mk("f_top", "front_three_quarter", 10, 0.9, 1.5, 1),
			mk("f_pick", "front_three_quarter", 20, 0.6, -0.5, 2),
			mk("s_top", "side_profile", 30, 0.9, 1, 3),
		}
	}
	cases := []struct {
		name      string
		preferred map[string]string
		mutate    func([]score.Scored)
		want      []string
	}{
		{"no preference → best score", nil, nil, []string{"f_top"}},
		{"preferred wins with lower score", map[string]string{"front_three_quarter": "f_pick"}, nil, []string{"f_pick"}},
		{"rejected preference ignored", map[string]string{"front_three_quarter": "f_pick"},
			func(c []score.Scored) { c[1].Rejected = score.RejectBlurry }, []string{"f_top"}},
		{"preference from another shot ignored", map[string]string{"front_three_quarter": "s_top"}, nil, []string{"f_top"}},
		{"unknown preference ignored", map[string]string{"front_three_quarter": "cf_nope"}, nil, []string{"f_top"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := base()
			if tc.mutate != nil {
				tc.mutate(c)
			}
			p := params(1)
			p.Preferred = tc.preferred
			assert.Equal(t, tc.want, ids(Select(c, p)))
		})
	}

	t.Run("non-distinct preference falls back", func(t *testing.T) {
		// The side_profile pick duplicates the already-chosen front frame.
		c := []score.Scored{
			mk("f", "front_three_quarter", 10, 0.9, 1, 1),
			mk("s_top", "side_profile", 30, 0.9, 1, 3),
			mk("s_pick", "side_profile", 40, 0.5, 0, 0),
		}
		c[2].DHash = code(1) ^ 1
		p := params(2)
		p.Preferred = map[string]string{"side_profile": "s_pick"}
		assert.Equal(t, []string{"f", "s_top"}, ids(Select(c, p)))
	})

	t.Run("preferred first, best-scoring still eligible for fill", func(t *testing.T) {
		p := params(8)
		p.MaxPerShot = 2
		p.Preferred = map[string]string{"front_three_quarter": "f_pick"}
		out := Select(base(), p)
		assert.Equal(t, []string{"f_top", "f_pick", "s_top"}, ids(out), "f_top joins in the fill pass")
	})

	t.Run("preferred first, the cap keeps the best-scoring out", func(t *testing.T) {
		p := params(8)
		p.Preferred = map[string]string{"front_three_quarter": "f_pick"}
		out := Select(base(), p)
		assert.Equal(t, []string{"f_pick", "s_top"}, ids(out))
	})
}

func TestSelectOrdering(t *testing.T) {
	shots := []domain.Shot{
		{ID: "a", Label: "A", Required: true},
		{ID: "b", Label: "B"},
		{ID: "c", Label: "C"},
	}
	cands := []score.Scored{
		mk("c1", "c", 1, 0.9, 1, 1),
		mk("b5", "b", 5, 0.9, 1, 2),
		mk("a9", "a", 9, 0.9, 1, 3),
		mk("other0", domain.OtherShotID, 0, 0.9, 1, 4),
		mk("unknown3", "zzz", 3, 0.9, 1, 5),
		mk("a20", "a", 20, 0.1, 0, 6),
		mk("b15", "b", 15, 0.1, 0, 7),
	}
	out := Select(cands, Params{ImageCount: 10, Shots: shots, Weights: carWeights, DedupeMaxHamming: 6, MaxPerShot: 2})
	assert.Equal(t, []string{"a9", "a20", "b5", "b15", "c1", "other0", "unknown3"}, ids(out))
	assert.Equal(t, []string{"a", "a", "b", "b", "c", domain.OtherShotID, "zzz"}, selShots(out))
	assert.Empty(t, out.MissingShots)
	assert.Equal(t, "Only 7 distinct high-quality views were found.", out.Explanation)
}

func TestSelectZeroCandidates(t *testing.T) {
	for _, cands := range [][]score.Scored{nil, {}} {
		out := Select(cands, params(8))
		assert.NotNil(t, out.Images)
		assert.Empty(t, out.Images)
		assert.NotNil(t, out.Scores)
		assert.Empty(t, out.Scores)
		assert.Equal(t, []string{"front_three_quarter", "side_profile", "rear_three_quarter", "interior_dashboard"}, shotIDs(out.MissingShots))
		assert.Equal(t, "No suitable scenes found.", out.Explanation)
	}
}

func TestSelectAllRejected(t *testing.T) {
	c := mk("x", "front_three_quarter", 1, 1, 1, 1)
	c.Rejected = score.RejectExposure
	out := Select([]score.Scored{c}, params(8))
	assert.Empty(t, out.Images)
	assert.Equal(t, NoScenesExplanation, out.Explanation)
	assert.Contains(t, out.Scores, "x")
}

func TestSelectDeterministic(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	shotPool := append(shotIDs(testShots), domain.OtherShotID, "not_in_list")
	cands := randomCands(r, 80, shotPool)
	for _, maxPerShot := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("maxPerShot=%d", maxPerShot), func(t *testing.T) {
			p := params(8)
			p.MaxPerShot = maxPerShot
			p.Preferred = map[string]string{"side_profile": cands[7].ID, "front": cands[11].ID}
			want := Select(cands, p)
			require.NotEmpty(t, want.Images)

			for range 25 {
				got := Select(shuffle(r, cands), p)
				assert.Equal(t, want, got)
			}
			limit := maxPerShot
			if limit <= 0 {
				limit = len(cands) // no cap
			}
			assertInvariants(t, want, limit)
			for _, img := range want.Images {
				assert.Equal(t, img.Candidate.ShotID, img.ShotID, "no review: the scene's shot")
			}
		})
	}
}

// randomCands builds n candidates with many ties: frames within 1 s of each other,
// equal scores, identical hashes; about one in eight is rejected.
func randomCands(r *rand.Rand, n int, shotPool []string) []score.Scored {
	cands := make([]score.Scored, 0, n)
	for i := range n {
		c := mk(fmt.Sprintf("cf_%03d", i), shotPool[r.IntN(len(shotPool))],
			float64(r.IntN(240))/4, // many frames within 1 s of each other
			float64(r.IntN(4))/4,   // coarse values → many exact score ties
			float64(r.IntN(5)-2)/2,
			r.IntN(40)) // repeated codes → identical hashes
		if r.IntN(8) == 0 {
			c.Rejected = score.RejectBlurry
		}
		cands = append(cands, c)
	}
	return cands
}

func shuffle(r *rand.Rand, cands []score.Scored) []score.Scored {
	out := append([]score.Scored(nil), cands...)
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}

// assertInvariants checks that no rejected frame was selected, all selected frames are
// pairwise distinct, and no shot has more than maxPerShot images.
func assertInvariants(t *testing.T, out *Output, maxPerShot int) {
	t.Helper()
	perShot := map[string]int{}
	for i, a := range out.Images {
		assert.Empty(t, a.Candidate.Rejected)
		perShot[a.ShotID]++
		for _, b := range out.Images[i+1:] {
			assert.Greater(t, score.Hamming(a.Candidate.DHash, b.Candidate.DHash), 6)
			d := a.Candidate.TimestampSec - b.Candidate.TimestampSec
			assert.GreaterOrEqual(t, max(d, -d), 1.0)
		}
	}
	for shot, n := range perShot {
		assert.LessOrEqual(t, n, maxPerShot, "shot %s", shot)
	}
}

func TestSelectDoesNotMutateInput(t *testing.T) {
	cands := []score.Scored{
		mk("b", "side_profile", 5, 0.9, 1, 2),
		mk("a", "front_three_quarter", 1, 0.9, 1, 1),
	}
	before := append([]score.Scored(nil), cands...)
	Select(cands, params(8))
	assert.Equal(t, before, cands)
}

func TestExplain(t *testing.T) {
	two := []domain.Shot{{ID: "rear_three_quarter", Label: "Rear 3/4"}, {ID: "interior_dashboard", Label: "Dashboard"}}
	noLabel := []domain.Shot{{ID: "trunk"}}
	cases := []struct {
		n, count int
		missing  []domain.Shot
		want     string
	}{
		{0, 8, nil, "No suitable scenes found."},
		{0, 8, two, "No suitable scenes found."},
		{8, 8, nil, "8 images selected."},
		{1, 1, nil, "1 image selected."},
		{8, 8, two, "8 images selected. Not clearly visible in the video: Rear 3/4, Dashboard."},
		{1, 1, two[:1], "1 image selected. Not clearly visible in the video: Rear 3/4."},
		{5, 8, two, "Only 5 distinct high-quality views were found. Missing: Rear 3/4, Dashboard."},
		{5, 8, nil, "Only 5 distinct high-quality views were found."},
		{1, 8, nil, "Only 1 distinct high-quality view was found."},
		{1, 8, noLabel, "Only 1 distinct high-quality view was found. Missing: trunk."},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, explain(tc.n, tc.count, tc.missing), "n=%d count=%d", tc.n, tc.count)
	}
}

func TestSelectTieBreaks(t *testing.T) {
	cands := []score.Scored{
		// Same score and timestamp: the lower ID covers the shot, the other is 0 s away.
		mk("b", "front_three_quarter", 5, 0.5, 0.5, 1),
		mk("a", "front_three_quarter", 5, 0.5, 0.5, 2),
		// Same score: the earlier timestamp covers the shot, the later one joins in fill.
		mk("late", "side_profile", 20, 0.5, 0.5, 3),
		mk("early", "side_profile", 12, 0.5, 0.5, 4),
	}
	p := params(2)
	assert.Equal(t, []string{"a", "early"}, ids(Select(cands, p)))

	p.ImageCount = 8
	assert.Equal(t, []string{"a", "early"}, ids(Select(cands, p)), "the cap keeps late out")
	p.MaxPerShot = 2
	assert.Equal(t, []string{"a", "early", "late"}, ids(Select(cands, p)))
}

func TestSelectShotListedTwice(t *testing.T) {
	shots := []domain.Shot{
		{ID: "a", Label: "A", Required: true},
		{ID: "a", Label: "A", Required: true},
		{ID: "b", Label: "B", Required: true},
	}
	cands := []score.Scored{
		mk("a1", "a", 1, 0.9, 1, 1),
		mk("a2", "a", 5, 0.8, 1, 2),
	}
	p := Params{ImageCount: 2, Shots: shots, Weights: carWeights, DedupeMaxHamming: 6, MaxPerShot: 1}
	// The duplicate entry neither takes a second coverage slot nor lifts the cap.
	out := Select(cands, p)
	assert.Equal(t, []string{"a1"}, ids(out))
	assert.Equal(t, []string{"b"}, shotIDs(out.MissingShots))
	assert.Equal(t, "Only 1 distinct high-quality view was found. Missing: B.", out.Explanation)

	// With room for two, a2 arrives via fill.
	p.MaxPerShot = 2
	out = Select(cands, p)
	assert.Equal(t, []string{"a1", "a2"}, ids(out))
	assert.Equal(t, "2 images selected. Not clearly visible in the video: B.", out.Explanation)
}

// The review can link two frames of DIFFERENT shots as the same view (a dashboard frame
// labelled "odometer"): pixel hashes and the time gap miss this, the link must not.
func TestSelectReviewDuplicateLink(t *testing.T) {
	dash := mk("dash", "interior_dashboard", 31.5, 0.9, 1, 1)
	odo := mk("odo", "odometer", 33.0, 0.9, 1, 2) // a different hash: far apart in pixels
	p := Params{
		ImageCount: 8, MaxPerShot: 1, MinGapSec: MinGapSec, DedupeMaxHamming: 6,
		Shots: []domain.Shot{
			{ID: "interior_dashboard", Label: "Dashboard", Required: true},
			{ID: "odometer", Label: "Odometer"},
		},
		Weights: domain.Weights{Relevance: 0.5, Sharpness: 0.4, Exposure: 0.1},
		Assessments: map[string]Assessment{
			"dash": {View: "interior_dashboard", Usable: true, Quality: 4},
			"odo":  {View: "odometer", Usable: true, Quality: 4, DuplicateOf: "dash"},
		},
	}
	out := Select([]score.Scored{dash, odo}, p)
	require.Len(t, out.Images, 1)
	assert.Equal(t, "dash", out.Images[0].Candidate.ID)

	p.Assessments["odo"] = Assessment{View: "odometer", Usable: true, Quality: 4}
	assert.Len(t, Select([]score.Scored{dash, odo}, p).Images, 2, "without the link both are kept")
}
