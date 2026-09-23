package crop

import (
	"fmt"
	"math"

	"video-to-image/internal/domain"
)

// Defaults for domain.Crop zero values (see its field docs).
const (
	DefaultAspect         = "4:3"
	DefaultMarginPct      = 6.0
	DefaultMinShortSidePx = 1080
)

// ReorientedMinShortSidePx is the lowest the short-side floor is lowered to for a crop
// that changes the frame's orientation (see Params): below it the frame is exported
// uncropped as before.
const ReorientedMinShortSidePx = 480

// NearlyUnchangedShare is the kept-area share at or above which a crop is skipped when
// the upright frame already has the target orientation: re-encoding to trim a few
// pixels off an already well-framed photo is not worth it.
const NearlyUnchangedShare = 0.92

// ParamsFor turns a domain's crop block into Params, replacing zero values with the
// defaults above.
func ParamsFor(c domain.Crop) (Params, error) {
	aspect := c.Aspect
	if aspect == "" {
		aspect = DefaultAspect
	}
	a, err := domain.ParseAspect(aspect)
	if err != nil {
		return Params{}, fmt.Errorf("crop: aspect: %w", err)
	}
	p := Params{Aspect: a, MarginPct: c.MarginPct, MinShortSidePx: c.MinShortSidePx}
	if p.MarginPct == 0 {
		p.MarginPct = DefaultMarginPct
	}
	if p.MinShortSidePx == 0 {
		p.MinShortSidePx = DefaultMinShortSidePx
	}
	return p, nil
}

// Plan computes the reframing (see the package doc and Result).
//
// All geometry is in pixels of the upright frame (Width/Height swapped for a 90° or
// 270° rotation). The subject box is moved from the stored frame into the upright
// one, grown by the margin on every side (clamped to the frame), and covered by the
// smallest Aspect-shaped rectangle centred on it. The margin is best effort: when that
// rectangle does not fit the frame, the largest smaller margin that fits is used, down
// to none. The rectangle is then grown around the same centre to the short-side floor
// (see Params) when it is smaller, shifted to lie inside the frame and snapped to whole
// pixels with even sides. There is no crop (Crop nil, Width/Height the upright frame)
// when even the bare box's rectangle does not fit the frame (the subject would be cut),
// when the result would be smaller than the floor, when the box is missing or invalid,
// or when the crop would keep at least NearlyUnchangedShare of an upright frame that
// already has the target orientation. A 90° rotation maps a stored point (x, y) to
// (1-y, x).
func Plan(f Frame, p Params) Result {
	rot, ok := normRotation(f.RotateCW)
	if !ok {
		return Result{Width: f.Width, Height: f.Height, Reason: fmt.Sprintf("invalid rotation %d", f.RotateCW)}
	}
	if f.Width <= 0 || f.Height <= 0 {
		return Result{RotateCW: rot, Reason: "invalid frame size"}
	}
	uw, uh := f.Width, f.Height
	if rot == 90 || rot == 270 {
		uw, uh = uh, uw
	}
	res := Result{RotateCW: rot, Width: uw, Height: uh}
	aspect := p.Aspect
	if !(aspect > 0) || math.IsInf(aspect, 0) {
		res.Reason = "invalid aspect"
		return res
	}
	box, ok := uprightBox(f.Box, rot)
	if !ok {
		res.Reason = "no subject box"
		return res
	}
	label := aspectLabel(aspect)

	W, H := float64(uw), float64(uh)
	bx0, by0, bx1, by1 := box[0]*W, box[1]*H, box[2]*W, box[3]*H
	margin := p.MarginPct
	if !(margin > 0) || math.IsInf(margin, 0) {
		margin = 0
	}
	m := margin / 100 * max(bx1-bx0, by1-by0)
	cropped := fmt.Sprintf("cropped to %s around the subject", label)

	// The Aspect rectangle around a box fits the frame exactly when the box is at most
	// H*aspect wide and W/aspect tall (the box itself is clamped to the frame).
	maxW, maxH := H*aspect, W/aspect
	const eps = 1e-6
	if bx1-bx0 > maxW+eps || by1-by0 > maxH+eps {
		res.Reason = fmt.Sprintf("subject does not fit %s without cutting it", label)
		return res
	}
	if fm := min(maxMargin(bx0, bx1, W, maxW), maxMargin(by0, by1, H, maxH)); fm < m {
		m = fm
		cropped += " with a reduced margin"
	}
	ex0, ey0 := max(0, bx0-m), max(0, by0-m)
	ex1, ey1 := min(W, bx1+m), min(H, by1+m)
	ew, eh := ex1-ex0, ey1-ey0

	// The smallest Aspect rectangle around the expanded box.
	tw, th := ew, eh
	if ew/eh >= aspect {
		th = ew / aspect
	} else {
		tw = eh * aspect
	}
	fits := func(w, h float64) bool { return w <= W+eps && h <= H+eps }
	floor := shortSideFloor(uw, uh, aspect, p.MinShortSidePx)
	tooSmall := fmt.Sprintf("result would be smaller than %d px", floor)
	if minSide := float64(floor); minSide > 0 && min(tw, th) < minSide {
		k := minSide / min(tw, th)
		tw, th = tw*k, th*k
		if !fits(tw, th) {
			res.Reason = tooSmall
			return res
		}
	}

	// Whole pixels, even sides (encoders and chroma subsampling prefer them), inside
	// the frame.
	pw := min(max(evenCeil(tw), 2), evenFloor(uw))
	ph := min(max(evenCeil(th), 2), evenFloor(uh))
	switch {
	case pw <= 0 || ph <= 0:
		res.Reason = "frame too small to crop"
		return res
	case min(pw, ph) < floor:
		res.Reason = tooSmall
		return res
	}
	cx, cy := (ex0+ex1)/2, (ey0+ey1)/2
	px0 := clampInt(int(math.Round(cx-float64(pw)/2)), 0, uw-pw)
	py0 := clampInt(int(math.Round(cy-float64(ph)/2)), 0, uh-ph)

	oriented := uw >= uh // landscape frame for a landscape (or square) target
	if aspect < 1 {
		oriented = uh >= uw
	}
	if oriented && float64(pw)*float64(ph) >= NearlyUnchangedShare*W*H {
		res.Reason = "nearly unchanged"
		return res
	}

	res.Crop = &[4]float64{
		float64(px0) / W, float64(py0) / H,
		float64(px0+pw) / W, float64(py0+ph) / H,
	}
	res.Width, res.Height = pw, ph
	res.Reason = cropped
	return res
}

// maxMargin is the largest margin m around the box edges b0 < b1 in [0, size] (each side
// clamped to the frame) that keeps the expanded box at most limit wide; +Inf when the
// whole frame is within the limit. The box itself must be within it: the expanded
// width is b1-b0 + min(m, b0) + min(m, size-b1), growing by 2 per unit of margin until
// the nearer frame edge is reached and by 1 after.
func maxMargin(b0, b1, size, limit float64) float64 {
	if size <= limit {
		return math.Inf(1)
	}
	room := max(0, limit-(b1-b0))
	near := min(b0, size-b1)
	if room <= 2*near {
		return room / 2
	}
	return room - near
}

// shortSideFloor is the smallest acceptable short side of the result (see Params):
// minSide, lowered for a crop that changes the orientation of the upright uw x uh
// frame to the largest short side an Aspect rectangle can have in it, as long as that
// stays at or above ReorientedMinShortSidePx.
func shortSideFloor(uw, uh int, aspect float64, minSide int) int {
	reorients := aspect > 1 && uh > uw || aspect < 1 && uw > uh
	if !reorients || minSide <= ReorientedMinShortSidePx {
		return minSide
	}
	// The largest Aspect rectangle in the frame, on even pixels (as Plan snaps crops).
	W, H := float64(uw), float64(uh)
	w := min(W, H*aspect)
	best := evenFloor(int(math.Floor(min(w, w/aspect) + 1e-6)))
	if best >= ReorientedMinShortSidePx && best < minSide {
		return best
	}
	return minSide
}

// normRotation maps any multiple of 90 (e.g. -90, 450) to 0, 90, 180 or 270.
func normRotation(deg int) (int, bool) {
	if deg%90 != 0 {
		return 0, false
	}
	return (deg%360 + 360) % 360, true
}

// uprightBox validates b (finite, clamped to [0,1], positive extent) and maps it from
// the stored frame into the frame rotated rot degrees clockwise.
func uprightBox(b [4]float64, rot int) ([4]float64, bool) {
	for _, v := range b {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return b, false
		}
	}
	x0, y0 := clamp01(b[0]), clamp01(b[1])
	x1, y1 := clamp01(b[2]), clamp01(b[3])
	if !(x1 > x0) || !(y1 > y0) {
		return b, false
	}
	point := func(x, y float64) (float64, float64) {
		switch rot {
		case 90:
			return 1 - y, x
		case 180:
			return 1 - x, 1 - y
		case 270:
			return y, 1 - x
		}
		return x, y
	}
	ax, ay := point(x0, y0)
	bx, by := point(x1, y1)
	return [4]float64{min(ax, bx), min(ay, by), max(ax, bx), max(ay, by)}, true
}

// aspectLabel writes an aspect as "W:H" with small integers when it is one (4/3 →
// "4:3"), otherwise as a decimal ratio.
func aspectLabel(a float64) string {
	for d := 1; d <= 32; d++ {
		n := a * float64(d)
		if r := math.Round(n); r >= 1 && math.Abs(n-r) < 1e-9*n {
			return fmt.Sprintf("%d:%d", int(r), d)
		}
	}
	return fmt.Sprintf("%.3g:1", a)
}

func clamp01(v float64) float64 { return min(max(v, 0), 1) }

func clampInt(v, lo, hi int) int { return min(max(v, lo), hi) }

// evenCeil rounds up to an even number of pixels, ignoring float noise far below a
// pixel (2160/(4.0/3) is 1620.0000000000002).
func evenCeil(v float64) int { return 2 * int(math.Ceil(v/2-1e-6)) }

// evenFloor rounds down to an even number.
func evenFloor(v int) int { return v &^ 1 }
