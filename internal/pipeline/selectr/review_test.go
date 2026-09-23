package selectr

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/score"
)

// ok is a usable assessment of the given view and quality, without a reason.
func ok(view string, quality int) Assessment {
	return Assessment{View: view, Usable: true, Quality: quality}
}

// labelOf returns the testShots label of a shot ID.
func labelOf(shot string) string {
	for _, sh := range testShots {
		if sh.ID == shot {
			return sh.Label
		}
	}
	return ""
}

// TestSelectOnePerShot replays the real test video's duplicates: the fill pass used to
// top up with a second frame of an already covered shot (Rear 3/4 at 00:11 and 00:12,
// Front seats at 00:39 and 00:41). With a cap of 1 every shot, "other" and shots
// outside the list included, gets at most one image, in both passes.
func TestSelectOnePerShot(t *testing.T) {
	cands := []score.Scored{
		mk("f34", "front_three_quarter", 2, 0.9, 1, 1),
		mk("side", "side_profile", 6, 0.9, 1, 2),
		mk("r34_11", "rear_three_quarter", 11, 0.9, 1, 3),
		mk("r34_12", "rear_three_quarter", 12, 0.9, 0.9, 4),
		mk("dash", "interior_dashboard", 30, 0.9, 1, 5),
		mk("seats_39", "interior_front_seats", 39, 0.9, 1, 6),
		mk("seats_41", "interior_front_seats", 41, 0.9, 0.9, 7),
		mk("other_a", domain.OtherShotID, 50, 0.5, 0, 8),
		mk("other_b", domain.OtherShotID, 55, 0.6, 0, 9), // the better "other"
		mk("zzz_a", "zzz", 60, 0.5, 0, 10),
		mk("zzz_b", "zzz", 62, 0.4, 0, 11),
	}
	out := Select(cands, params(8))
	assert.Equal(t, []string{"f34", "side", "r34_11", "dash", "seats_39", "other_b", "zzz_a"}, ids(out))
	assert.Equal(t, []string{
		"front_three_quarter", "side_profile", "rear_three_quarter", "interior_dashboard",
		"interior_front_seats", domain.OtherShotID, "zzz",
	}, selShots(out))
	assert.Empty(t, out.MissingShots)
	assert.Equal(t, "Only 7 distinct high-quality views were found.", out.Explanation)
	assert.Len(t, out.Scores, len(cands))
	assertInvariants(t, out, 1)

	// 0 (a domain without maxPerShot) and negative values mean no cap: the fill pass
	// tops up with the best remaining distinct frames of any shot, by score.
	for _, maxPerShot := range []int{0, -3} {
		p := params(8)
		p.MaxPerShot = maxPerShot
		out := Select(cands, p)
		assert.Equal(t, []string{"f34", "side", "r34_11", "r34_12", "dash", "seats_39", "seats_41", "other_b"}, ids(out), "maxPerShot=%d", maxPerShot)
		assert.Equal(t, "8 images selected.", out.Explanation)
		assertInvariants(t, out, len(cands))
	}
}

func TestSelectMaxPerShotTwo(t *testing.T) {
	cands := []score.Scored{
		mk("f1", "front_three_quarter", 10, 0.9, 1, 1),
		mk("f1_twin", "front_three_quarter", 30, 0.9, 0.9, 0), // hash twin of f1
		mk("f2", "front_three_quarter", 40, 0.9, 0.8, 2),
		mk("f3", "front_three_quarter", 50, 0.9, 0.7, 3),
		mk("o1", domain.OtherShotID, 60, 0.5, 0.3, 4),
		mk("o2", domain.OtherShotID, 70, 0.5, 0.2, 5),
		mk("o3", domain.OtherShotID, 80, 0.5, 0.1, 6),
	}
	cands[1].DHash = code(1) ^ 1
	p := params(8)
	p.MaxPerShot = 2
	out := Select(cands, p)
	// The twin is not distinct, so the second front 3/4 is the next distinct frame;
	// the third distinct one and the third "other" stay out.
	assert.Equal(t, []string{"f1", "f2", "o1", "o2"}, ids(out))
	assertInvariants(t, out, 2)
}

func TestSelectAssessmentsReclassify(t *testing.T) {
	cands := []score.Scored{
		// The scene said "front" but the frame really shows the rear 3/4, and it beats
		// the frame the scene labelled rear 3/4 on quality.
		mk("was_front", "front", 5, 0.5, 0, 1),
		mk("real_r34", "rear_three_quarter", 20, 0.9, 1, 2),
		mk("side", "side_profile", 10, 0.9, 1, 3),
		// An "other" moment that is in fact the front 3/4.
		mk("was_other", domain.OtherShotID, 30, 0.5, 0, 4),
		// A view outside the shot list and a "none" view count as "other" (capped once).
		mk("trunk", "rear", 40, 0.9, 1, 5),
		mk("nothing", "wheel", 50, 0.9, 1, 6),
	}
	p := params(8)
	p.Assessments = map[string]Assessment{
		"was_front": ok("rear_three_quarter", 5),
		"real_r34":  ok("rear_three_quarter", 3),
		"side":      ok("side_profile", 4),
		"was_other": ok("front_three_quarter", 4),
		"trunk":     ok("trunk", 4),
		"nothing":   ok("none", 3),
	}
	out := Select(cands, p)
	assert.Equal(t, []string{"was_other", "side", "was_front", "trunk"}, ids(out))
	assert.Equal(t, []string{"front_three_quarter", "side_profile", "rear_three_quarter", domain.OtherShotID}, selShots(out))
	// "rear" and "wheel" are not covered (their frames show other views) and the
	// dashboard never appeared: only the required one is missing.
	assert.Equal(t, []string{"interior_dashboard"}, shotIDs(out.MissingShots))
	assert.Equal(t, "Only 4 distinct high-quality views were found. Missing: Dashboard.", out.Explanation)
	assertInvariants(t, out, 1)
}

func TestSelectAssessmentsEligibility(t *testing.T) {
	cands := []score.Scored{
		mk("good", "front_three_quarter", 1, 0.2, 0, 1),
		mk("half_car", "side_profile", 10, 1, 3, 2),         // assessed unusable
		mk("unassessed", "rear_three_quarter", 20, 1, 3, 3), // not in the review
		mk("blurry", "interior_dashboard", 30, 1, 3, 4),     // rejected by score, usable by review
		mk("low_q", "front", 40, 1, 3, 5),                   // unusable although quality is set
	}
	cands[3].Rejected = score.RejectBlurry
	p := params(8)
	p.Assessments = map[string]Assessment{
		"good":     ok("front_three_quarter", 4),
		"half_car": {View: "side_profile", Usable: false, Quality: 5, Reason: "car cut in half"},
		"blurry":   ok("interior_dashboard", 5),
		"low_q":    {View: "front", Usable: false, Quality: 2},
	}
	out := Select(cands, p)
	assert.Equal(t, []string{"good"}, ids(out))
	assert.Equal(t, []string{"side_profile", "rear_three_quarter", "interior_dashboard"}, shotIDs(out.MissingShots))
	assert.Equal(t, "Only 1 distinct high-quality view was found. Missing: Side profile, Rear 3/4, Dashboard.", out.Explanation)
	assert.Len(t, out.Scores, len(cands), "every candidate is scored")

	t.Run("review ran but assessed nothing", func(t *testing.T) {
		p := params(8)
		p.Assessments = map[string]Assessment{}
		out := Select(cands, p)
		assert.Empty(t, out.Images)
		assert.NotNil(t, out.Images)
		assert.Equal(t, NoScenesExplanation, out.Explanation)
		assert.Equal(t, []string{"front_three_quarter", "side_profile", "rear_three_quarter", "interior_dashboard"}, shotIDs(out.MissingShots))
		assert.Len(t, out.Scores, len(cands))
	})
}

func TestSelectAssessmentsQualityBeatsScore(t *testing.T) {
	cands := []score.Scored{
		mk("f_score", "front_three_quarter", 10, 1, 3, 1), // best score, quality 3
		mk("f_q5_low", "front_three_quarter", 20, 0.1, 0, 2),
		mk("f_q5_high", "front_three_quarter", 30, 0.2, 0, 3), // quality tie → score decides
		mk("o_q4", domain.OtherShotID, 40, 0.1, -1, 4),
	}
	as := map[string]Assessment{
		"f_score":   ok("front_three_quarter", 3),
		"f_q5_low":  ok("front_three_quarter", 5),
		"f_q5_high": ok("front_three_quarter", 5),
		"o_q4":      ok(domain.OtherShotID, 4),
	}

	p := params(1)
	p.Assessments = as
	assert.Equal(t, []string{"f_q5_high"}, ids(Select(cands, p)), "coverage")

	// Fill ranks by quality across shots too: the quality-4 "other" frame beats the
	// quality-3 front 3/4 frame with a far higher score.
	p = params(3)
	p.MaxPerShot = 3
	p.Assessments = as
	assert.Equal(t, []string{"f_q5_low", "f_q5_high", "o_q4"}, ids(Select(cands, p)), "fill")

	// Without the review the score decides.
	p.Assessments = nil
	assert.Equal(t, []string{"f_score", "f_q5_low", "f_q5_high"}, ids(Select(cands, p)))
}

func TestSelectAssessmentsReason(t *testing.T) {
	cands := []score.Scored{
		mk("with", "front_three_quarter", 1, 0.9, 1, 1),
		mk("without", "side_profile", 10, 0.9, 1, 2),
	}
	p := params(8)
	p.Assessments = map[string]Assessment{
		"with":    {View: "front_three_quarter", Usable: true, Quality: 5, Reason: "Whole car, sharp, upright"},
		"without": ok("side_profile", 4),
	}
	out := Select(cands, p)
	require.Len(t, out.Images, 2)
	assert.Equal(t, "Whole car, sharp, upright", out.Images[0].Reason)
	assert.Equal(t, "reason without", out.Images[1].Reason, "empty review reason → the scene's")
}

func TestSelectAssessmentsPreferred(t *testing.T) {
	cands := []score.Scored{
		mk("f_best", "front_three_quarter", 10, 0.9, 1, 1),
		mk("f_pick", "front_three_quarter", 20, 0.1, 0, 2),
	}
	p := params(8)
	p.Preferred = map[string]string{"front_three_quarter": "f_pick"}
	p.Assessments = map[string]Assessment{"f_best": ok("front_three_quarter", 5), "f_pick": ok("front_three_quarter", 3)}
	assert.Equal(t, []string{"f_pick"}, ids(Select(cands, p)), "the preferred frame wins over quality")

	p.Assessments["f_pick"] = Assessment{View: "front_three_quarter", Quality: 3}
	assert.Equal(t, []string{"f_best"}, ids(Select(cands, p)), "an unusable preference is ignored")
}

// TestSelectNilAssessmentsUnchanged: a review that confirms every scene shot with the
// same quality and no reason selects exactly what no review selects.
func TestSelectNilAssessmentsUnchanged(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	cands := randomCands(r, 80, append(shotIDs(testShots), domain.OtherShotID))
	identity := make(map[string]Assessment, len(cands))
	for _, c := range cands {
		identity[c.ID] = ok(c.ShotID, 3)
	}
	for _, maxPerShot := range []int{1, 2} {
		p := params(8)
		p.MaxPerShot = maxPerShot
		p.Preferred = map[string]string{"side_profile": cands[7].ID}
		want := Select(cands, p)
		require.NotEmpty(t, want.Images)
		p.Assessments = identity
		assert.Equal(t, want, Select(cands, p), "maxPerShot=%d", maxPerShot)
	}
}

func TestSelectAssessmentsDeterministic(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	shotPool := append(shotIDs(testShots), domain.OtherShotID, "not_in_list")
	cands := randomCands(r, 80, shotPool)
	views := append(append([]string(nil), shotPool...), "none", "")
	as := make(map[string]Assessment)
	for _, c := range cands {
		if r.IntN(6) == 0 {
			continue // not assessed
		}
		a := Assessment{View: views[r.IntN(len(views))], Usable: r.IntN(4) != 0, Quality: 1 + r.IntN(5)}
		if r.IntN(2) == 0 {
			a.Reason = "review " + c.ID
		}
		as[c.ID] = a
	}
	for _, maxPerShot := range []int{1, 2} {
		t.Run(fmt.Sprintf("maxPerShot=%d", maxPerShot), func(t *testing.T) {
			p := params(8)
			p.MaxPerShot = maxPerShot
			p.Assessments = as
			want := Select(cands, p)
			require.NotEmpty(t, want.Images)
			for range 25 {
				assert.Equal(t, want, Select(shuffle(r, cands), p))
			}
			assertInvariants(t, want, maxPerShot)
			listed := map[string]bool{}
			for _, sh := range testShots {
				listed[sh.ID] = true
			}
			for _, img := range want.Images {
				a, found := as[img.Candidate.ID]
				require.True(t, found, "only assessed frames")
				assert.True(t, a.Usable)
				if listed[a.View] {
					assert.Equal(t, a.View, img.ShotID)
				} else {
					assert.Equal(t, domain.OtherShotID, img.ShotID)
				}
				sceneShot := img.Candidate.ShotID
				if !listed[sceneShot] {
					sceneShot = domain.OtherShotID
				}
				switch {
				case a.Reason != "":
					assert.Equal(t, a.Reason, img.Reason)
				case img.ShotID == sceneShot:
					assert.Equal(t, img.Candidate.SceneReason, img.Reason)
				case img.ShotID == domain.OtherShotID:
					assert.Equal(t, otherReason, img.Reason, "re-classified as other")
				default: // testShots have no descriptions
					assert.Equal(t, labelOf(img.ShotID)+" view", img.Reason, "re-classified")
				}
			}
		})
	}
}

// A frame the review re-classified must not keep its scene's reason, which describes
// the old view: "Front" with "Front three-quarter view with the whole car in frame"
// contradicts itself on the result card, in the alt text and in manifest.json.
func TestSelectReclassifiedReason(t *testing.T) {
	shots := []domain.Shot{
		{ID: "front_three_quarter", Label: "Front 3/4", Required: true,
			Description: "Front and one side at roughly 45°,\n  whole car in frame"},
		{ID: "front", Label: "Front"}, // no description
		{ID: "interior_dashboard", Label: "Dashboard", Description: "Dashboard, steering wheel and center console"},
		{ID: "odometer", Label: "Odometer", Description: "Instrument cluster with mileage readable"},
	}
	cands := []score.Scored{
		mk("f34_as_front", "front_three_quarter", 1, 0.9, 1, 1),
		mk("odo_as_dash", "odometer", 10, 0.9, 1, 2),
		mk("other_as_f34", domain.OtherShotID, 20, 0.9, 1, 3),
		mk("odo_as_other", "odometer", 30, 0.9, 1, 4),
		mk("zzz_as_trunk", "zzz", 40, 0.9, 1, 5), // both outside the list: "other" either way
		mk("odo_confirmed", "odometer", 50, 0.9, 1, 6),
		mk("f34_with_reason", "front_three_quarter", 60, 0.9, 1, 7),
	}
	cands[0].SceneReason = "Front three-quarter view with the whole car in frame"
	p := Params{
		ImageCount: 10, Shots: shots, Weights: carWeights, DedupeMaxHamming: 6, // no cap
		Assessments: map[string]Assessment{
			"f34_as_front":    ok("front", 4),
			"odo_as_dash":     ok("interior_dashboard", 4),
			"other_as_f34":    ok("front_three_quarter", 4),
			"odo_as_other":    ok(domain.OtherShotID, 4),
			"zzz_as_trunk":    ok("trunk", 4),
			"odo_confirmed":   ok("odometer", 4),
			"f34_with_reason": {View: "front", Usable: true, Quality: 4, Reason: "Grille and headlights"},
		},
	}
	out := Select(cands, p)
	require.Len(t, out.Images, len(cands))
	reasons := map[string]string{}
	shotOf := map[string]string{}
	for _, img := range out.Images {
		reasons[img.Candidate.ID] = img.Reason
		shotOf[img.Candidate.ID] = img.ShotID
	}
	assert.Equal(t, map[string]string{
		"f34_as_front":    "Front view",     // label fallback
		"odo_as_dash":     "Dashboard view", // descriptions are model guidance, not captions
		"other_as_f34":    "Front 3/4 view",
		"odo_as_other":    otherReason,
		"zzz_as_trunk":    "reason zzz_as_trunk",
		"odo_confirmed":   "reason odo_confirmed",
		"f34_with_reason": "Grille and headlights", // the review's own reason wins
	}, reasons)
	assert.Equal(t, "front", shotOf["f34_as_front"])
	assert.Equal(t, domain.OtherShotID, shotOf["zzz_as_trunk"])

	// Without the review nothing is re-classified.
	p.Assessments = nil
	out = Select(cands, p)
	require.NotEmpty(t, out.Images)
	for _, img := range out.Images {
		assert.Equal(t, img.Candidate.SceneReason, img.Reason)
	}
}

// The model gives one sameAs per frame, often only to the nearest earlier frame: b
// sameAs a, c sameAs b means a, b and c are one view, so a and c must not both be
// selected although neither links to the other.
func TestSelectReviewDuplicateChain(t *testing.T) {
	a := mk("a", "front", 10, 0.9, 1, 1)
	b := mk("b", "front", 12, 0.9, 1, 2)
	c := mk("c", "front_three_quarter", 14, 0.9, 1, 3)
	d := mk("d", "side_profile", 16, 0.9, 1, 4)
	e := mk("e", "rear", 18, 0.9, 1, 5) // unrelated
	cands := []score.Scored{a, b, c, d, e}

	cases := []struct {
		name  string
		links map[string]string // candidate → its sameAs
		want  []string
	}{
		// The finding's case: c (front 3/4, higher priority) is chosen first, b is
		// linked to it, and a only through b.
		{"chain", map[string]string{"b": "a", "c": "b"}, []string{"c", "d", "e"}},
		{"star", map[string]string{"b": "a", "c": "a"}, []string{"c", "d", "e"}},
		{"long chain across views", map[string]string{"b": "a", "c": "b", "d": "c"}, []string{"c", "e"}},
		{"links to unknown frames and to itself", map[string]string{"b": "cf_gone", "c": "c"}, []string{"c", "d", "a", "b", "e"}},
		{"no links", nil, []string{"c", "d", "a", "b", "e"}},
	}
	r := rand.New(rand.NewPCG(9, 9))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := params(8)
			p.MaxPerShot = 0 // a and b are both "front": only the links may keep one out
			p.Assessments = map[string]Assessment{
				"a": ok("front", 4),
				"b": ok("front", 5),
				"c": ok("front_three_quarter", 4),
				"d": ok("side_profile", 4),
				"e": ok("rear", 4),
			}
			for id, dup := range tc.links {
				as := p.Assessments[id]
				as.DuplicateOf = dup
				p.Assessments[id] = as
			}
			assert.Equal(t, tc.want, ids(Select(cands, p)))
			for range 10 {
				assert.Equal(t, tc.want, ids(Select(shuffle(r, cands), p)))
			}
		})
	}
}

func TestViewGroups(t *testing.T) {
	assert.Nil(t, viewGroups(nil))
	assert.Nil(t, viewGroups(map[string]Assessment{"a": ok("front", 3)}))

	g := viewGroups(map[string]Assessment{
		"b": {DuplicateOf: "a"},
		"c": {DuplicateOf: "b"},
		"e": {DuplicateOf: "d"},
		"f": {DuplicateOf: "f"},
		"g": {DuplicateOf: "cf_gone"},
		"h": {},
	})
	assert.Len(t, g, 8, "every ID at either end of a link")
	assert.Equal(t, g["a"], g["b"])
	assert.Equal(t, g["a"], g["c"])
	assert.Equal(t, g["d"], g["e"])
	assert.Equal(t, g["g"], g["cf_gone"])
	assert.Equal(t, "f", g["f"])
	for _, other := range []string{"d", "f", "g"} {
		assert.NotEqual(t, g["a"], g[other])
	}
	assert.NotEqual(t, g["d"], g["g"])
	assert.NotContains(t, g, "h")
}
