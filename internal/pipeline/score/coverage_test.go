package score_test

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
	"video-to-image/internal/pipeline/selectr"
)

// Scoring must not take away the candidates the coverage pass needs. These tests run
// Classify and then selectr.Select on the shipped cars domain, as the pipeline does.

func carsDomain(t *testing.T) *domain.Config {
	t.Helper()
	reg, err := domain.LoadDir(filepath.Join("..", "..", "..", "configs", "domains"))
	require.NoError(t, err)
	d, ok := reg.Get("cars")
	require.True(t, ok)
	return d
}

// sceneSpec is one model scene: n frames at 4 fps from startSec, with the given
// sharpness per frame and a dHash per frame.
type sceneSpec struct {
	shot     string
	startSec float64
	sharp    func(i int) float64
	hash     func(i int) uint64
	n        int
}

func build(specs []sceneSpec) []score.Scored {
	var out []score.Scored
	for idx, sp := range specs {
		for i := range sp.n {
			ts := sp.startSec + float64(i)/4
			out = append(out, score.Scored{
				Candidate: extract.Candidate{
					ID: fmt.Sprintf("cf_%s_%02d", sp.shot, i), SceneIdx: idx, ShotID: sp.shot,
					Relevance: 0.9, SceneReason: sp.shot, TimestampSec: ts,
				},
				Sharpness: sp.sharp(i),
				DHash:     sp.hash(i),
			})
		}
	}
	return out
}

// randomHashes returns n hashes that are pairwise more than 6 bits apart and more than
// 6 bits from every hash in avoid.
func randomHashes(t *testing.T, seed uint64, n int, avoid ...uint64) []uint64 {
	t.Helper()
	r := rand.New(rand.NewPCG(seed, 7))
	out := make([]uint64, 0, n)
	for tries := 0; len(out) < n; tries++ {
		require.Less(t, tries, 100*n)
		h := r.Uint64()
		ok := true
		for _, o := range append(append([]uint64(nil), avoid...), out...) {
			if score.Hamming(h, o) <= 6 {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, h)
		}
	}
	return out
}

func selectFor(d *domain.Config, ss []score.Scored) *selectr.Output {
	score.Classify(ss, score.DefaultParams(d))
	return selectr.Select(ss, selectr.ParamsFor(d))
}

func shotsOf(out *selectr.Output) []string {
	var ids []string
	for _, img := range out.Images {
		ids = append(ids, img.ShotID)
	}
	return ids
}

func missingOf(out *selectr.Output) []string {
	var ids []string
	for _, sh := range out.MissingShots {
		ids = append(ids, sh.ID)
	}
	return ids
}

// A short, well-focused but low-texture dashboard scene sits entirely below the job's
// 15th-percentile blur floor (54 exterior frames around 300, 6 dashboard frames around
// 150). Its best frame must survive so the required dashboard shot is covered.
func TestLowTextureSceneKeepsItsBestFrame(t *testing.T) {
	d := carsDomain(t)
	hashes := randomHashes(t, 1, 60)
	next := 0
	hash := func(int) uint64 { next++; return hashes[next-1] }
	exterior := func(i int) float64 { return 290 + float64((i*7)%25) }
	ss := build([]sceneSpec{
		{shot: "front_three_quarter", startSec: 0, n: 18, sharp: exterior, hash: hash},
		{shot: "side_profile", startSec: 10, n: 18, sharp: exterior, hash: hash},
		{shot: "rear_three_quarter", startSec: 20, n: 18, sharp: exterior, hash: hash},
		{shot: "interior_dashboard", startSec: 40, n: 6, sharp: func(i int) float64 { return 150 + float64(i) }, hash: hash},
	})

	out := selectFor(d, ss)

	var dashKept, dashBlurry []string
	for _, s := range ss {
		if s.ShotID != "interior_dashboard" {
			continue
		}
		switch s.Rejected {
		case "":
			dashKept = append(dashKept, s.ID)
		case score.RejectBlurry:
			dashBlurry = append(dashBlurry, s.ID)
		}
	}
	assert.Equal(t, []string{"cf_interior_dashboard_05"}, dashKept, "the sharpest dashboard frame survives the floor")
	assert.Len(t, dashBlurry, 5, "the rest of the scene is still below the floor")
	assert.Contains(t, shotsOf(out), "interior_dashboard")
	assert.Empty(t, missingOf(out), out.Explanation)
}

// One sharp frame of the optional rear shot is a near-duplicate of both the side
// profile and the rear 3/4 (which are distinct from each other). Keeping the sharper
// frame of each pair would mark every candidate of both required shots as a duplicate
// of the optional one; shot priority keeps the required shots instead.
func TestCrossShotDuplicateKeepsRequiredShots(t *testing.T) {
	d := carsDomain(t)
	const (
		rearHash = uint64(0)
		sideHash = uint64(0x1F)       // 5 bits from rear
		r3qHash  = uint64(0x1F) << 10 // 5 bits from rear, 10 from side
	)
	others := randomHashes(t, 2, 24, rearHash, sideHash, r3qHash)
	next := 0
	other := func(int) uint64 { next++; return others[next-1] }
	fixed := func(h uint64) func(int) uint64 { return func(int) uint64 { return h } }
	sharp := func(base float64) func(int) float64 { return func(i int) float64 { return base - float64(i) } }
	ss := build([]sceneSpec{
		{shot: "front_three_quarter", startSec: 0, n: 12, sharp: sharp(320), hash: other},
		{shot: "side_profile", startSec: 10, n: 13, sharp: sharp(300), hash: fixed(sideHash)},
		{shot: "rear_three_quarter", startSec: 20, n: 11, sharp: sharp(300), hash: fixed(r3qHash)},
		{shot: "rear", startSec: 30, n: 13, sharp: sharp(450), hash: fixed(rearHash)},
		{shot: "interior_dashboard", startSec: 40, n: 12, sharp: sharp(310), hash: other},
	})

	out := selectFor(d, ss)

	dupOf := map[string]string{} // shot → shot of the frame it duplicates
	byID := map[string]score.Scored{}
	for _, s := range ss {
		byID[s.ID] = s
	}
	for _, s := range ss {
		if s.Rejected == score.RejectDuplicate {
			from, to := s.ShotID, byID[s.DuplicateOf].ShotID
			if from != to {
				dupOf[from] = to
			}
		}
	}
	assert.Equal(t, map[string]string{"rear": "side_profile"}, dupOf,
		"only the optional shot loses frames to another shot")
	assert.Empty(t, missingOf(out), out.Explanation)
	assert.Subset(t, shotsOf(out), []string{"front_three_quarter", "side_profile", "rear_three_quarter", "interior_dashboard"})
	assert.NotContains(t, shotsOf(out), "rear", "the selector keeps the chosen images distinct")
}
