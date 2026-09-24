package studio

import (
	"image"
	"math"
	"sort"
)

// geometry is what the compositor reads from the (cleaned) mask. It does not depend on
// the style; the wall/floor horizon is placed per style (see horizonY).
type geometry struct {
	box      image.Rectangle // alpha ≥ 0.5
	coverage float64         // fraction of the image with alpha ≥ 0.5
	carW     float64
	carH     float64 // top of the car to its lowest tyre contact
	contacts []image.Point
	tyres    []tyre
	tyreSpan [2]int  // x of the outermost contacts
	highest  float64 // y of the highest ground contact / tyre (for the horizon)
	touchL   bool
	touchR   bool
	// level: no tyre contact was found (straight front/rear view, tyres hidden behind the
	// bumper), so the ground line is the lowest point of the silhouette, level, and the
	// contact shadow spans the whole low part of the bottom silhouette.
	level bool

	bottom []int     // per column: lowest row with alpha ≥ 0.5, -1 if none
	ground []float64 // per column: the ground line under/beside the car
}

// tyre is a detected tyre bottom.
type tyre struct {
	x, y     int
	halfW    int  // half width of its flat bottom
	onGround bool // on the ground line through the near-side tyres
}

// horizonY is the middle of the wall→floor cove for a style: horizonAbove × carH above
// the highest ground contact, so the floor runs on behind the far wheels.
func (g *geometry) horizonY(horizonAbove float64, h int) float64 {
	return math.Max(g.highest-horizonAbove*g.carH, 0.02*float64(h))
}

func analyse(src *image.RGBA, a []uint8, w, h int) (*geometry, bool) {
	g := &geometry{bottom: make([]int, w)}
	x0, y0, x1, y1 := w, h, -1, -1
	area := 0
	for x := range w {
		g.bottom[x] = -1
	}
	for y := range h {
		row := a[y*w : (y+1)*w]
		for x, v := range row {
			if v >= 128 {
				area++
				g.bottom[x] = y
				x0, x1 = min(x0, x), max(x1, x)
				y0, y1 = min(y0, y), max(y1, y)
			}
		}
	}
	if x1 < 0 {
		return nil, false
	}
	g.box = image.Rect(x0, y0, x1+1, y1+1)
	g.coverage = float64(area) / float64(w*h)
	g.touchL, g.touchR = x0 == 0, x1 == w-1
	g.carW = float64(x1 - x0 + 1)

	type pt struct{ x, y float64 }
	// Lower hull of the (lightly smoothed) bottom silhouette: the chain below every
	// bottom point.
	prof := medianProfile(g.bottom, x0, x1, 2)
	var hull []pt
	cross := func(o, a, b pt) float64 { // with y' = -y (y up), lower hull keeps cross > 0
		return (a.x-o.x)*(-(b.y)+o.y) - (-(a.y)+o.y)*(b.x-o.x)
	}
	for x := x0; x <= x1; x++ {
		if prof[x] < 0 {
			continue
		}
		p := pt{float64(x), float64(prof[x])}
		for len(hull) >= 2 && cross(hull[len(hull)-2], hull[len(hull)-1], p) <= 0 {
			hull = hull[:len(hull)-1]
		}
		hull = append(hull, p)
	}
	lowest := 0.0
	for _, p := range hull {
		lowest = max(lowest, p.y)
	}
	g.carH = max(lowest-float64(y0)+1, 1)

	// hullY interpolates the hull at column x.
	hullY := func(fx float64) float64 {
		if fx <= hull[0].x {
			return hull[0].y
		}
		for i := 1; i < len(hull); i++ {
			if fx <= hull[i].x {
				p, q := hull[i-1], hull[i]
				return p.y + (fx-p.x)/(q.x-p.x)*(q.y-p.y)
			}
		}
		return hull[len(hull)-1].y
	}

	// Tyres: local lowest points of the silhouette with a flat-bottomed U at one of three
	// scales (the far tyre of a steep 3/4 view is half the size of the near one): nearly
	// flat within ±e1, clearly rising at ±e2 = 3·e1 (a bumper or sill stays flat) or the
	// car ending there, and not a tiny circle (an exhaust tip or tow hook), and dark,
	// neutral pixels just above the bottom (rubber, not a silver or red bumper). At a local
	// lowest point the tangent is level, so the test does not depend on the tilt of the
	// ground line (in a steep 3/4 view the far tyre sits 40 % of the car's height above
	// the near one). Tyres on (or within 4 % carH of) the lower hull touch the ground line;
	// the others are far-side wheels that still get their own contact spot.
	d2 := 0.09 * g.carH
	flatTol := 0.012 * g.carH
	at := func(fx float64) (float64, bool) {
		x := int(math.Round(fx))
		if x < x0 || x > x1 || prof[x] < 0 {
			return 0, false
		}
		return float64(prof[x]), true
	}
	win := max(1, int(d2/2))
	var ground []pt
	addTyre := func(t tyre) {
		for i, u := range g.tyres { // the same tyre found twice: keep the lower point
			if math.Abs(float64(u.x-t.x)) < d2 {
				if t.y > u.y {
					g.tyres[i] = t
				}
				return
			}
		}
		g.tyres = append(g.tyres, t)
	}
	halfWidth := func(v pt) int { // how far the silhouette stays near the bottom
		hw := 1
		for ; hw < int(3*d2); hw++ {
			pl, okl := at(v.x - float64(hw))
			pr, okr := at(v.x + float64(hw))
			if (!okl || v.y-pl > 2*flatTol) && (!okr || v.y-pr > 2*flatTol) {
				break
			}
		}
		return hw
	}
	isRubber := func(x, y int) bool {
		luma, chroma := tyreColour(src, a, w, x, y, max(2, int(0.02*g.carH)), max(2, int(0.05*g.carH)))
		return luma < 140 && chroma < 45
	}
	for x := x0; x <= x1; x++ {
		if prof[x] < 0 {
			continue
		}
		isMax := true
		for k := max(x-win, x0); k <= min(x+win, x1) && isMax; k++ {
			if prof[k] > prof[x] || (prof[k] == prof[x] && k < x && k >= x-win) {
				isMax = false // lower point nearby, or not the first of a plateau
			}
		}
		if !isMax {
			continue
		}
		v := pt{float64(x), float64(prof[x])}
		shapeOK := false
		for _, k := range []float64{1, 0.66, 0.44} {
			e2 := d2 * k
			e1 := e2 / 3
			pl1, okl1 := at(v.x - e1)
			pr1, okr1 := at(v.x + e1)
			pl2, okl2 := at(v.x - e2)
			pr2, okr2 := at(v.x + e2)
			var dep1, dep2 float64
			switch {
			case okl1 && okr1 && okl2 && okr2:
				dep1, dep2 = v.y-(pl1+pr1)/2, v.y-(pl2+pr2)/2
			case okl1 && okl2 && !okr2: // the car ends on the right: one-sided
				dep1, dep2 = v.y-pl1, (v.y-pl2)/2
			case okr1 && okr2 && !okl2:
				dep1, dep2 = v.y-pr1, (v.y-pr2)/2
			default:
				continue
			}
			// radius of the circle through the bottom and the ±e1 points: an exhaust tip
			// or tow hook is far smaller than any tyre
			rEst := math.Inf(1)
			if dep1 > 0.5 {
				rEst = (e1*e1 + dep1*dep1) / (2 * dep1)
			}
			if dep1 <= 0.4*e1 && dep2 >= 0.1*e2 && rEst >= 0.03*g.carH {
				shapeOK = true
				break
			}
		}
		if shapeOK && isRubber(x, prof[x]) {
			addTyre(tyre{x: x, y: prof[x], halfW: halfWidth(v), onGround: hullY(v.x)-v.y <= 0.04*g.carH})
		}
	}
	sort.Slice(g.tyres, func(i, j int) bool { return g.tyres[i].x < g.tyres[j].x })
	for _, t := range g.tyres {
		if t.onGround {
			ground = append(ground, pt{float64(t.x), float64(t.y)})
		}
	}
	// Steep 3/4 views: the ground line from the near tyre touches the far tyre at its
	// lower corner, which is neither a local lowest point nor a U. Follow the hull outward
	// from the outermost ground tyre: when its first long edge (≥ 10 % carW) ends at a
	// rubber-coloured vertex that is not the car's extreme column, that is a contact too.
	if len(ground) > 0 {
		extend := func(from pt, dir float64) (pt, bool) {
			for i := range hull {
				p := hull[i]
				j := i + int(dir)
				if j < 0 || j >= len(hull) || (p.x-from.x)*dir < -0.02*g.carW {
					continue
				}
				q := hull[j]
				if math.Abs(q.x-p.x) < 0.10*g.carW {
					if (q.x-from.x)*dir > 0.10*g.carW {
						return pt{}, false // left the tyre without a long edge
					}
					continue
				}
				if int(q.x) <= x0+2 || int(q.x) >= x1-2 {
					return pt{}, false
				}
				return q, isRubber(int(q.x), int(q.y))
			}
			return pt{}, false
		}
		if q, ok := extend(ground[len(ground)-1], 1); ok {
			ground = append(ground, q)
			g.tyres = append(g.tyres, tyre{x: int(q.x), y: int(q.y), halfW: int(0.02 * g.carH), onGround: true})
		}
		if q, ok := extend(ground[0], -1); ok {
			ground = append([]pt{q}, ground...)
			g.tyres = append([]tyre{{x: int(q.x), y: int(q.y), halfW: int(0.02 * g.carH), onGround: true}}, g.tyres...)
		}
	}
	if len(ground) == 0 { // fall back to the lowest hull point
		for _, p := range hull {
			if p.y == lowest {
				ground = append(ground, p)
				break
			}
		}
	}
	for _, p := range ground {
		g.contacts = append(g.contacts, image.Pt(int(p.x), int(p.y)))
	}
	c0, c1 := ground[0], ground[len(ground)-1]
	g.tyreSpan = [2]int{int(c0.x), int(c1.x)}
	if len(ground) == 1 {
		// Level fallback: let the shadow run under every column whose bottom is close to the
		// lowest point (the whole bumper / sill line), not only around the single contact.
		g.level = true
		tol := 0.08 * g.carH
		sx0, sx1 := int(c0.x), int(c0.x)
		for x, b := range g.bottom {
			if b >= 0 && float64(b) >= lowest-tol {
				sx0, sx1 = min(sx0, x), max(sx1, x)
			}
		}
		g.tyreSpan = [2]int{sx0, sx1}
	}
	highest := lowest
	for _, t := range g.tyres {
		highest = min(highest, float64(t.y))
	}
	for _, p := range ground {
		highest = min(highest, p.y)
	}
	g.highest = highest

	// Ground line: the hull between the outermost ground tyres, extended beyond them with
	// the slope between the neighbouring ground tyres (the ground runs on under the
	// overhangs); one ground tyre only → level.
	slopeL, slopeR := 0.0, 0.0
	if len(ground) >= 2 {
		a, b := ground[0].x, ground[1].x
		slopeL = (hullY(b) - hullY(a)) / (b - a)
		a, b = ground[len(ground)-2].x, ground[len(ground)-1].x
		slopeR = (hullY(b) - hullY(a)) / (b - a)
	}
	y0L, y0R := hullY(c0.x), hullY(c1.x)
	if len(ground) == 1 {
		y0L, y0R = c0.y, c0.y
	}
	g.ground = make([]float64, w)
	for x := range w {
		fx := float64(x)
		switch {
		case fx <= c0.x:
			g.ground[x] = y0L + slopeL*(fx-c0.x)
		case fx >= c1.x:
			g.ground[x] = y0R + slopeR*(fx-c1.x)
		default:
			g.ground[x] = hullY(fx)
		}
	}
	return g, true
}

// medianProfile is the bottom profile with a (2r+1)-column median over defined columns,
// which removes single-column spikes of a ragged mask edge.
func medianProfile(b []int, x0, x1, r int) []int {
	out := make([]int, len(b))
	copy(out, b)
	win := make([]int, 0, 2*r+1)
	for x := x0; x <= x1; x++ {
		if b[x] < 0 {
			continue
		}
		win = win[:0]
		for k := max(x-r, x0); k <= min(x+r, x1); k++ {
			if b[k] >= 0 {
				win = append(win, b[k])
			}
		}
		for i := 1; i < len(win); i++ { // insertion sort, ≤ 5 values
			for j := i; j > 0 && win[j] < win[j-1]; j-- {
				win[j], win[j-1] = win[j-1], win[j]
			}
		}
		out[x] = win[len(win)/2]
	}
	return out
}

// tyreColour is the median luma and chroma (max-min of RGB) of the car pixels in the
// box hw wide and up to ht above the bottom point (x, y): rubber is dark and neutral.
func tyreColour(src *image.RGBA, a []uint8, w, x, y, hw, ht int) (float64, float64) {
	var ls, cs []float64
	for yy := max(y-ht, 0); yy <= y; yy++ {
		for xx := max(x-hw, 0); xx <= min(x+hw, w-1); xx++ {
			if a[yy*w+xx] != 255 {
				continue
			}
			p := src.Pix[yy*src.Stride+xx*4:]
			r, g, b := float64(p[0]), float64(p[1]), float64(p[2])
			ls = append(ls, 0.299*r+0.587*g+0.114*b)
			cs = append(cs, max(r, g, b)-min(r, g, b))
		}
	}
	if len(ls) == 0 {
		return 255, 255
	}
	sort.Float64s(ls)
	sort.Float64s(cs)
	return ls[len(ls)/2], cs[len(cs)/2]
}
