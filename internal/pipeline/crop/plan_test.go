package crop

import (
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/domain"
)

var defaults = Params{Aspect: 4.0 / 3, MarginPct: 6, MinShortSidePx: 1080}

// px builds a normalised crop from upright pixel edges.
func px(uw, uh, x0, y0, x1, y1 int) *[4]float64 {
	return &[4]float64{float64(x0) / float64(uw), float64(y0) / float64(uh), float64(x1) / float64(uw), float64(y1) / float64(uh)}
}

func TestPlan(t *testing.T) {
	nan := math.NaN()
	for _, tc := range []struct {
		name   string
		frame  Frame
		params Params
		want   Result
	}{
		{
			name:  "portrait frame, wide car: landscape 4:3 crop of the full width",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.02, 0.40, 0.98, 0.62}},
			want: Result{Crop: px(2160, 3840, 0, 1148, 2160, 2768), Width: 2160, Height: 1620,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "sideways car in a portrait frame: rotate 90 then crop",
			frame: Frame{Width: 2160, Height: 3840, RotateCW: 90, Box: [4]float64{0.35, 0.3, 0.65, 0.7}},
			want: Result{RotateCW: 90, Crop: px(3840, 2160, 1059, 434, 2781, 1726), Width: 1722, Height: 1292,
				Reason: "cropped to 4:3 around the subject"},
		},
		// One stored box (top-left of a landscape frame) under every rotation.
		{
			name:  "rotation 0",
			frame: Frame{Width: 3840, Height: 2160, Box: [4]float64{0.1, 0.1, 0.3, 0.4}},
			want: Result{Crop: px(3840, 2160, 48, 0, 1488, 1080), Width: 1440, Height: 1080,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "rotation 90",
			frame: Frame{Width: 3840, Height: 2160, RotateCW: 90, Box: [4]float64{0.1, 0.1, 0.3, 0.4}},
			want: Result{RotateCW: 90, Crop: px(2160, 3840, 720, 228, 2160, 1308), Width: 1440, Height: 1080,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "rotation 180",
			frame: Frame{Width: 3840, Height: 2160, RotateCW: 180, Box: [4]float64{0.1, 0.1, 0.3, 0.4}},
			want: Result{RotateCW: 180, Crop: px(3840, 2160, 2352, 1080, 3792, 2160), Width: 1440, Height: 1080,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "rotation 270",
			frame: Frame{Width: 3840, Height: 2160, RotateCW: 270, Box: [4]float64{0.1, 0.1, 0.3, 0.4}},
			want: Result{RotateCW: 270, Crop: px(2160, 3840, 0, 2532, 1440, 3612), Width: 1440, Height: 1080,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "negative rotation is normalised",
			frame: Frame{Width: 3840, Height: 2160, RotateCW: -90, Box: [4]float64{0.1, 0.1, 0.3, 0.4}},
			want: Result{RotateCW: 270, Crop: px(2160, 3840, 0, 2532, 1440, 3612), Width: 1440, Height: 1080,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "rotation over 360 is normalised",
			frame: Frame{Width: 3840, Height: 2160, RotateCW: 450, Box: [4]float64{0.1, 0.1, 0.3, 0.4}},
			want: Result{RotateCW: 90, Crop: px(2160, 3840, 720, 228, 2160, 1308), Width: 1440, Height: 1080,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "box at the right edge: the crop is shifted inside",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.3, 0.5, 1.0, 0.6}},
			want: Result{Crop: px(2160, 3840, 556, 1510, 2160, 2714), Width: 1604, Height: 1204,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "box at the top edge",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.1, 0, 0.9, 0.2}},
			want: Result{Crop: px(2160, 3840, 112, 0, 2048, 1452), Width: 1936, Height: 1452,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "box slightly outside the frame is clamped",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.1, -0.05, 0.9, 0.2}},
			want: Result{Crop: px(2160, 3840, 112, 0, 2048, 1452), Width: 1936, Height: 1452,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "box too tall for 4:3 in a portrait frame",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.2, 0.2, 0.8, 0.8}},
			want:  Result{Width: 2160, Height: 3840, Reason: "subject does not fit 4:3 without cutting it"},
		},
		{
			name:  "wide subject in a 16:9 frame does not fit 4:3",
			frame: Frame{Width: 2160, Height: 3840, RotateCW: 90, Box: [4]float64{0.3, 0.1, 0.7, 0.9}},
			want:  Result{RotateCW: 90, Width: 3840, Height: 2160, Reason: "subject does not fit 4:3 without cutting it"},
		},
		{
			name:  "tiny box is grown to the minimum size",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.45, 0.45, 0.55, 0.5}},
			want: Result{Crop: px(2160, 3840, 360, 1284, 1800, 2364), Width: 1440, Height: 1080,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:   "no minimum size: tiny box, tiny crop",
			frame:  Frame{Width: 2160, Height: 3840, Box: [4]float64{0.45, 0.45, 0.55, 0.5}},
			params: Params{Aspect: 4.0 / 3, MarginPct: 6},
			want: Result{Crop: px(2160, 3840, 934, 1715, 1226, 1933), Width: 292, Height: 218,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "frame smaller than the minimum",
			frame: Frame{Width: 1280, Height: 720, Box: [4]float64{0.4, 0.4, 0.6, 0.6}},
			want:  Result{Width: 1280, Height: 720, Reason: "result would be smaller than 1080 px"},
		},
		// A crop that changes the orientation lowers the floor to what the frame allows.
		{
			name:  "1080p portrait: the floor drops to the widest 4:3 crop",
			frame: Frame{Width: 1080, Height: 1920, Box: [4]float64{0.05, 0.45, 0.95, 0.55}},
			want: Result{Crop: px(1080, 1920, 0, 555, 1080, 1365), Width: 1080, Height: 810,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "sideways 1080p frame turned portrait: the floor drops to the widest 4:3 crop",
			frame: Frame{Width: 1920, Height: 1080, RotateCW: 90, Box: [4]float64{0.45, 0.05, 0.55, 0.95}},
			want: Result{RotateCW: 90, Crop: px(1080, 1920, 0, 555, 1080, 1365), Width: 1080, Height: 810,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "4K portrait: a 1620 px short side is reachable, the floor stays",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.05, 0.45, 0.95, 0.55}},
			want: Result{Crop: px(2160, 3840, 0, 1110, 2160, 2730), Width: 2160, Height: 1620,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "720p portrait: cropped to the widest 4:3 rectangle",
			frame: Frame{Width: 720, Height: 1280, Box: [4]float64{0.05, 0.45, 0.95, 0.55}},
			want: Result{Crop: px(720, 1280, 0, 370, 720, 910), Width: 720, Height: 540,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "720p portrait, tiny box: grown to the lowered floor",
			frame: Frame{Width: 720, Height: 1280, Box: [4]float64{0.45, 0.45, 0.55, 0.5}},
			want: Result{Crop: px(720, 1280, 0, 338, 720, 878), Width: 720, Height: 540,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:   "720p portrait with an 810 floor",
			frame:  Frame{Width: 720, Height: 1280, Box: [4]float64{0.45, 0.45, 0.55, 0.5}},
			params: Params{Aspect: 4.0 / 3, MarginPct: 6, MinShortSidePx: 810},
			want: Result{Crop: px(720, 1280, 0, 338, 720, 878), Width: 720, Height: 540,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "480p portrait: the widest 4:3 crop (360 px) is below the absolute floor",
			frame: Frame{Width: 480, Height: 854, Box: [4]float64{0.05, 0.45, 0.95, 0.55}},
			want:  Result{Width: 480, Height: 854, Reason: "result would be smaller than 1080 px"},
		},
		{
			name:   "portrait target from a landscape frame: the floor drops too",
			frame:  Frame{Width: 1920, Height: 1080, Box: [4]float64{0.45, 0.05, 0.55, 0.95}},
			params: Params{Aspect: 3.0 / 4, MarginPct: 6, MinShortSidePx: 1080},
			want: Result{Crop: px(1920, 1080, 555, 0, 1365, 1080), Width: 810, Height: 1080,
				Reason: "cropped to 3:4 around the subject"},
		},
		{
			name:   "square target does not change the orientation: the full floor applies",
			frame:  Frame{Width: 720, Height: 1280, Box: [4]float64{0.05, 0.45, 0.95, 0.55}},
			params: Params{Aspect: 1, MarginPct: 6, MinShortSidePx: 1080},
			want:   Result{Width: 720, Height: 1280, Reason: "result would be smaller than 1080 px"},
		},
		{
			name:   "an 810 floor on a landscape frame",
			frame:  Frame{Width: 3840, Height: 2160, Box: [4]float64{0.45, 0.45, 0.55, 0.5}},
			params: Params{Aspect: 4.0 / 3, MarginPct: 6, MinShortSidePx: 810},
			want: Result{Crop: px(3840, 2160, 1380, 621, 2460, 1431), Width: 1080, Height: 810,
				Reason: "cropped to 4:3 around the subject"},
		},
		// The margin is best effort: it shrinks rather than giving up the crop.
		{
			name:  "wide subject in a 16:9 frame: the margin shrinks to fit 4:3",
			frame: Frame{Width: 3840, Height: 2160, Box: [4]float64{0.12, 0.05, 0.85, 0.95}},
			// Box 460.8-3264 px wide: 38.4 px of margin on each side makes it 2880 = 2160*4/3.
			want: Result{Crop: px(3840, 2160, 422, 0, 3302, 2160), Width: 2880, Height: 2160,
				Reason: "cropped to 4:3 around the subject with a reduced margin"},
		},
		{
			name:  "subject at the left edge: the margin shrinks on the free side only",
			frame: Frame{Width: 3840, Height: 2160, Box: [4]float64{0, 0.05, 0.74, 0.95}},
			want: Result{Crop: px(3840, 2160, 0, 0, 2880, 2160), Width: 2880, Height: 2160,
				Reason: "cropped to 4:3 around the subject with a reduced margin"},
		},
		{
			name:  "tall subject in a portrait frame: the margin shrinks vertically",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.1, 0.3, 0.9, 0.7}},
			// Box 1536 px tall: 42 px above and below makes it 1620 = 2160*3/4.
			want: Result{Crop: px(2160, 3840, 0, 1110, 2160, 2730), Width: 2160, Height: 1620,
				Reason: "cropped to 4:3 around the subject with a reduced margin"},
		},
		{
			name:  "subject wider than 4:3 of the frame height even without a margin",
			frame: Frame{Width: 3840, Height: 2160, Box: [4]float64{0.1, 0.05, 0.9, 0.95}},
			want:  Result{Width: 3840, Height: 2160, Reason: "subject does not fit 4:3 without cutting it"},
		},
		{
			name:  "already landscape 4:3, wide box: nearly unchanged",
			frame: Frame{Width: 4032, Height: 3024, Box: [4]float64{0.05, 0.05, 0.95, 0.95}},
			want:  Result{Width: 4032, Height: 3024, Reason: "nearly unchanged"},
		},
		{
			name:  "3:2 frame trimmed to 4:3 keeps 89% of the area: cropped",
			frame: Frame{Width: 3600, Height: 2400, Box: [4]float64{0.12, 0.05, 0.88, 0.95}},
			want: Result{Crop: px(3600, 2400, 200, 0, 3400, 2400), Width: 3200, Height: 2400,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:   "portrait target",
			frame:  Frame{Width: 2160, Height: 3840, Box: [4]float64{0.1, 0.3, 0.9, 0.7}},
			params: Params{Aspect: 3.0 / 4, MarginPct: 6, MinShortSidePx: 1080},
			want: Result{Crop: px(2160, 3840, 112, 629, 2048, 3211), Width: 1936, Height: 2582,
				Reason: "cropped to 3:4 around the subject"},
		},
		{
			name:  "zero box: rotate only",
			frame: Frame{Width: 2160, Height: 3840, RotateCW: 90},
			want:  Result{RotateCW: 90, Width: 3840, Height: 2160, Reason: "no subject box"},
		},
		{
			name:  "reversed box",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.8, 0.2, 0.2, 0.4}},
			want:  Result{Width: 2160, Height: 3840, Reason: "no subject box"},
		},
		{
			name:  "box entirely outside the frame",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{1.2, 0.2, 1.5, 0.4}},
			want:  Result{Width: 2160, Height: 3840, Reason: "no subject box"},
		},
		{
			name:  "NaN box",
			frame: Frame{Width: 2160, Height: 3840, RotateCW: 180, Box: [4]float64{0.1, nan, 0.9, 0.7}},
			want:  Result{RotateCW: 180, Width: 2160, Height: 3840, Reason: "no subject box"},
		},
		{
			name:  "infinite box",
			frame: Frame{Width: 2160, Height: 3840, Box: [4]float64{0.1, 0.1, math.Inf(1), 0.7}},
			want:  Result{Width: 2160, Height: 3840, Reason: "no subject box"},
		},
		{
			name:   "NaN aspect",
			frame:  Frame{Width: 2160, Height: 3840, Box: [4]float64{0.02, 0.40, 0.98, 0.62}},
			params: Params{Aspect: nan, MarginPct: 6, MinShortSidePx: 1080},
			want:   Result{Width: 2160, Height: 3840, Reason: "invalid aspect"},
		},
		{
			name:   "zero aspect",
			frame:  Frame{Width: 2160, Height: 3840, Box: [4]float64{0.02, 0.40, 0.98, 0.62}},
			params: Params{MarginPct: 6, MinShortSidePx: 1080},
			want:   Result{Width: 2160, Height: 3840, Reason: "invalid aspect"},
		},
		{
			name:   "NaN margin counts as none",
			frame:  Frame{Width: 2160, Height: 3840, Box: [4]float64{0.02, 0.40, 0.98, 0.62}},
			params: Params{Aspect: 4.0 / 3, MarginPct: nan, MinShortSidePx: 1080},
			// No margin: the box is 2073.6 px wide → 2074x1556 around its centre.
			want: Result{Crop: px(2160, 3840, 43, 1180, 2117, 2736), Width: 2074, Height: 1556,
				Reason: "cropped to 4:3 around the subject"},
		},
		{
			name:  "invalid rotation",
			frame: Frame{Width: 2160, Height: 3840, RotateCW: 45, Box: [4]float64{0.02, 0.40, 0.98, 0.62}},
			want:  Result{Width: 2160, Height: 3840, Reason: "invalid rotation 45"},
		},
		{
			name:  "invalid frame size",
			frame: Frame{Width: 0, Height: 3840, RotateCW: 90, Box: [4]float64{0.02, 0.40, 0.98, 0.62}},
			want:  Result{RotateCW: 90, Reason: "invalid frame size"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.params
			if p == (Params{}) {
				p = defaults
			}
			got := Plan(tc.frame, p)
			assert.Equal(t, tc.want.RotateCW, got.RotateCW, "rotation")
			assert.Equal(t, tc.want.Width, got.Width, "width")
			assert.Equal(t, tc.want.Height, got.Height, "height")
			assert.Equal(t, tc.want.Reason, got.Reason, "reason")
			if tc.want.Crop == nil {
				assert.Nil(t, got.Crop)
				return
			}
			require.NotNil(t, got.Crop)
			assert.InDeltaSlice(t, tc.want.Crop[:], got.Crop[:], 1e-12)
			checkInvariants(t, tc.frame, p, got)
		})
	}
}

// upright maps a stored normalised point into the frame turned rot degrees clockwise
// by repeating the 90° step (x, y) → (1-y, x).
func upright(x, y float64, rot int) (float64, float64) {
	for range rot / 90 {
		x, y = 1-y, x
	}
	return x, y
}

// uprightPx is the frame's subject box in upright pixels.
func uprightPx(f Frame, rot, uw, uh int) (x0, y0, x1, y1 float64) {
	ax, ay := upright(clamp01(f.Box[0]), clamp01(f.Box[1]), rot)
	bx, by := upright(clamp01(f.Box[2]), clamp01(f.Box[3]), rot)
	return min(ax, bx) * float64(uw), min(ay, by) * float64(uh), max(ax, bx) * float64(uw), max(ay, by) * float64(uh)
}

// reorients reports whether a p.Aspect crop changes the orientation of an upright
// uw x uh frame, and the largest short side such a crop can have in it.
func reorients(uw, uh int, p Params) (bool, float64) {
	w := min(float64(uw), float64(uh)*p.Aspect)
	return p.Aspect > 1 && uh > uw || p.Aspect < 1 && uw > uh, min(w, w/p.Aspect)
}

// minShortSide is a lower bound for the short side of a crop: MinShortSidePx, or for
// a crop that changes the orientation the largest achievable short side (within the
// even-pixel snap) when that is smaller, but never below ReorientedMinShortSidePx.
func minShortSide(uw, uh int, p Params) int {
	re, best := reorients(uw, uh, p)
	if !re {
		return p.MinShortSidePx
	}
	return min(p.MinShortSidePx, max(ReorientedMinShortSidePx, int(best)-2))
}

// checkInvariants verifies a crop against the plan's promises, independently of its
// arithmetic.
func checkInvariants(t *testing.T, f Frame, p Params, got Result) {
	t.Helper()
	rot := (f.RotateCW%360 + 360) % 360
	uw, uh := f.Width, f.Height
	if rot == 90 || rot == 270 {
		uw, uh = uh, uw
	}
	c := got.Crop
	require.True(t, 0 <= c[0] && c[0] < c[2] && c[2] <= 1 && 0 <= c[1] && c[1] < c[3] && c[3] <= 1, "crop inside [0,1]: %v", *c)

	// Whole pixels, even sides, matching Width/Height.
	x0, y0 := c[0]*float64(uw), c[1]*float64(uh)
	x1, y1 := c[2]*float64(uw), c[3]*float64(uh)
	for _, v := range []float64{x0, y0, x1, y1} {
		assert.InDelta(t, math.Round(v), v, 1e-6, "crop edges on whole pixels")
	}
	assert.Equal(t, got.Width, int(math.Round(x1-x0)))
	assert.Equal(t, got.Height, int(math.Round(y1-y0)))
	assert.Zero(t, got.Width%2, "even width")
	assert.Zero(t, got.Height%2, "even height")
	assert.GreaterOrEqual(t, min(got.Width, got.Height), minShortSide(uw, uh, p), "minimum short side")
	assert.InDelta(t, p.Aspect*float64(got.Height), float64(got.Width), 2+2*p.Aspect, "aspect")

	// The subject box (upright) is inside the crop.
	bx0, by0, bx1, by1 := uprightPx(f, rot, uw, uh)
	const tol = 1.0 // pixels
	assert.LessOrEqual(t, x0, bx0+tol, "left")
	assert.LessOrEqual(t, y0, by0+tol, "top")
	assert.GreaterOrEqual(t, x1, bx1-tol, "right")
	assert.GreaterOrEqual(t, y1, by1-tol, "bottom")

	// With the margin where the frame allows, unless the Aspect rectangle around the
	// margined box would not fit: then the margin shrinks only as far as needed, so the
	// crop spans the frame's height (box too wide) or width (box too tall).
	margin := 0.0
	if p.MarginPct > 0 {
		margin = p.MarginPct / 100 * max(bx1-bx0, by1-by0)
	}
	ex0, ey0 := max(0, bx0-margin), max(0, by0-margin)
	ex1, ey1 := min(float64(uw), bx1+margin), min(float64(uh), by1+margin)
	if x0 > ex0+tol || y0 > ey0+tol || x1 < ex1-tol || y1 < ey1-tol {
		assert.True(t, ex1-ex0 > float64(uh)*p.Aspect-tol || ey1-ey0 > float64(uw)/p.Aspect-tol,
			"margin reduced although it fits: %v", *c)
		assert.True(t, got.Height == evenFloor(uh) || got.Width == evenFloor(uw),
			"reduced margin but the crop does not span the frame: %dx%d", got.Width, got.Height)
		assert.Contains(t, got.Reason, "with a reduced margin")
	}

	oriented := uw >= uh
	if p.Aspect < 1 {
		oriented = uh >= uw
	}
	assert.False(t, oriented && float64(got.Width*got.Height) >= NearlyUnchangedShare*float64(uw*uh), "not nearly unchanged")
}

// TestPlanInvariants checks random frames, boxes, rotations and parameters.
func TestPlanInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	aspects := []float64{4.0 / 3, 3.0 / 2, 16.0 / 9, 1, 3.0 / 4}
	videoSizes := [][2]int{{1920, 1080}, {1080, 1920}, {1280, 720}, {720, 1280}, {3840, 2160}, {2160, 3840}, {854, 480}, {480, 854}}
	var cropped, notCropped, reduced, reoriented int
	for i := range 40000 {
		f := Frame{
			Width:    200 + rng.IntN(4800),
			Height:   200 + rng.IntN(4800),
			RotateCW: 90 * rng.IntN(4),
		}
		if rng.IntN(2) == 0 {
			sz := videoSizes[rng.IntN(len(videoSizes))]
			f.Width, f.Height = sz[0], sz[1]
		}
		x0, y0 := rng.Float64(), rng.Float64()
		f.Box = [4]float64{x0, y0, x0 + rng.Float64()*(1.05-x0), y0 + rng.Float64()*(1.05-y0)}
		p := Params{
			Aspect:         aspects[rng.IntN(len(aspects))],
			MarginPct:      rng.Float64() * 20,
			MinShortSidePx: []int{0, 300, 480, 810, 1080}[rng.IntN(5)],
		}
		got := Plan(f, p)
		name := fmt.Sprintf("#%d %+v %+v", i, f, p)
		require.NotEmpty(t, got.Reason, name)
		require.Equal(t, f.RotateCW, got.RotateCW, name)
		uw, uh := f.Width, f.Height
		if f.RotateCW%180 != 0 {
			uw, uh = uh, uw
		}

		// "Does not fit" exactly when even the bare box's Aspect rectangle exceeds the frame.
		bx0, by0, bx1, by1 := uprightPx(f, f.RotateCW, uw, uh)
		overW, overH := bx1-bx0-float64(uh)*p.Aspect, by1-by0-float64(uw)/p.Aspect
		valid := bx1 > bx0 && by1 > by0
		const tol = 1e-3 // pixels
		if strings.HasPrefix(got.Reason, "subject does not fit") {
			require.True(t, overW > -tol || overH > -tol, "%s: the bare box fits: %+v", name, got)
		} else if valid && (overW > tol || overH > tol) {
			require.Failf(t, "the bare box does not fit, yet no such reason", "%s: %+v", name, got)
		}
		// A crop that changes the orientation is never too small when the frame's widest
		// Aspect rectangle reaches min(MinShortSidePx, ReorientedMinShortSidePx).
		if re, best := reorients(uw, uh, p); re && valid && overW < -tol && overH < -tol &&
			best >= float64(min(p.MinShortSidePx, ReorientedMinShortSidePx)+2) {
			require.NotNil(t, got.Crop, "%s: %+v", name, got)
			reoriented++
			if min(got.Width, got.Height) < p.MinShortSidePx {
				assert.GreaterOrEqual(t, min(got.Width, got.Height), ReorientedMinShortSidePx, name)
			}
		}
		if strings.HasPrefix(got.Reason, "result would be smaller than") {
			assert.Equal(t, fmt.Sprintf("result would be smaller than %d px", p.MinShortSidePx), got.Reason,
				"a lowered floor is always reachable: %s", name)
		}

		if got.Crop == nil {
			notCropped++
			require.Equal(t, uw, got.Width, name)
			require.Equal(t, uh, got.Height, name)
			continue
		}
		cropped++
		if strings.HasSuffix(got.Reason, "with a reduced margin") {
			reduced++
		}
		checkInvariants(t, f, p, got)
		if t.Failed() {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	assert.Greater(t, cropped, 1000)
	assert.Greater(t, notCropped, 1000)
	assert.Greater(t, reduced, 500)
	assert.Greater(t, reoriented, 1000)
}

// TestCarsFloor pins what configs/domains/cars.yaml and the domain.Crop docs promise:
// portrait frames turn landscape whatever minShortSidePx is (down to 480 px), and the
// configured floor also bounds crops of landscape frames.
func TestCarsFloor(t *testing.T) {
	reg, err := domain.LoadDir(filepath.Join("..", "..", "..", "configs", "domains"))
	require.NoError(t, err)
	cars, ok := reg.Get("cars")
	require.True(t, ok)
	p, err := ParamsFor(cars.Crop)
	require.NoError(t, err)
	assert.Equal(t, 480, ReorientedMinShortSidePx, "documented on domain.Crop.MinShortSidePx")

	portrait := [4]float64{0.45, 0.45, 0.55, 0.5} // a small subject: the floor decides
	for _, floor := range []int{p.MinShortSidePx, 1080} {
		q := p
		q.MinShortSidePx = floor
		for _, tc := range []struct {
			frame Frame
			w, h  int
		}{
			{Frame{Width: 1080, Height: 1920, Box: portrait}, 1080, 810},
			{Frame{Width: 1920, Height: 1080, RotateCW: 270, Box: portrait}, 1080, 810},
			{Frame{Width: 720, Height: 1280, Box: portrait}, 720, 540},
		} {
			got := Plan(tc.frame, q)
			require.NotNil(t, got.Crop, "floor %d, %+v: %s", floor, tc.frame, got.Reason)
			assert.Equal(t, [2]int{tc.w, tc.h}, [2]int{got.Width, got.Height}, "floor %d, %+v", floor, tc.frame)
		}
	}
	got := Plan(Frame{Width: 3840, Height: 2160, Box: portrait}, p)
	assert.Equal(t, p.MinShortSidePx, min(got.Width, got.Height), "a small subject in a landscape frame: the configured floor")
}

func TestParamsFor(t *testing.T) {
	p, err := ParamsFor(domain.Crop{Enabled: true, Aspect: "4:3"})
	require.NoError(t, err)
	assert.Equal(t, Params{Aspect: 4.0 / 3, MarginPct: DefaultMarginPct, MinShortSidePx: DefaultMinShortSidePx}, p)

	p, err = ParamsFor(domain.Crop{Aspect: "16:9", MarginPct: 2.5, MinShortSidePx: 720})
	require.NoError(t, err)
	assert.Equal(t, Params{Aspect: 16.0 / 9, MarginPct: 2.5, MinShortSidePx: 720}, p)

	p, err = ParamsFor(domain.Crop{})
	require.NoError(t, err)
	assert.InDelta(t, 4.0/3, p.Aspect, 1e-12, "empty aspect → 4:3")

	_, err = ParamsFor(domain.Crop{Aspect: "wide"})
	assert.ErrorContains(t, err, "crop: aspect")
}

func TestAspectLabel(t *testing.T) {
	for a, want := range map[float64]string{4.0 / 3: "4:3", 16.0 / 9: "16:9", 1: "1:1", 3.0 / 4: "3:4", 1.5: "3:2", math.Pi: "3.14:1"} {
		assert.Equal(t, want, aspectLabel(a))
	}
}
