package studio

import "math"

// shadowMap is a low-resolution multiplier (1 = no shadow) for the backdrop, bilinearly
// upsampled at compose time. Shadows are soft, so ~1000 px on the long side is plenty.
// It only ever multiplies the backdrop, never car pixels.
type shadowMap struct {
	s      int // full-res pixels per low-res pixel
	lw, lh int
	mul    []float32
}

func (m *shadowMap) at(x, y int) float64 {
	if m == nil {
		return 1
	}
	fx := (float64(x)+0.5)/float64(m.s) - 0.5
	fy := (float64(y)+0.5)/float64(m.s) - 0.5
	x0, y0 := int(math.Floor(fx)), int(math.Floor(fy))
	tx, ty := fx-float64(x0), fy-float64(y0)
	cx := func(v int) int { return min(max(v, 0), m.lw-1) }
	cy := func(v int) int { return min(max(v, 0), m.lh-1) }
	r0, r1 := cy(y0)*m.lw, cy(y0+1)*m.lw
	a, b := float64(m.mul[r0+cx(x0)]), float64(m.mul[r0+cx(x0+1)])
	c, d := float64(m.mul[r1+cx(x0)]), float64(m.mul[r1+cx(x0+1)])
	return (1-ty)*((1-tx)*a+tx*b) + ty*((1-tx)*c+tx*d)
}

// buildShadow draws the contact shadow from the bottom silhouette:
//
//   - core: the visible floor under the body between the tyres (shadowUnder), a thin band
//     along the ground line (shadowLine, fading out beyond the outermost tyres) and full
//     density where the tyres meet the ground; blurred tightly (σx ≈ 1 % carW,
//     σy ≈ 0.35 % carW);
//   - ambient: a band from a little above to a little below the ground line, spanning the
//     car plus a margin, blurred wide and squashed vertically (σx = shadowSpread·carW,
//     σy = shadowSquash·σx). In the level fallback (no tyre contact found: a straight
//     front or rear view) it reaches 5 % carH below the ground line, onto the floor in
//     front of the bumper.
//
// mul = (1 - shadowCore·core) · (1 - shadowAmbient·ambient).
func buildShadow(g *geometry, w, h int, st look) *shadowMap {
	if st.shadowCore <= 0 && st.shadowAmbient <= 0 {
		return nil
	}
	s := max(1, int(math.Round(float64(max(w, h))/1024)))
	lw, lh := (w+s-1)/s, (h+s-1)/s
	core := make([]float32, lw*lh)
	amb := make([]float32, lw*lh)
	fill := func(p []float32, X int, yTop, yBot float64, v float32) {
		if v <= 0 || yBot < yTop {
			return
		}
		Y0 := max(int(math.Floor(yTop/float64(s))), 0)
		Y1 := min(int(math.Floor(yBot/float64(s))), lh-1)
		for Y := Y0; Y <= Y1; Y++ {
			if i := Y*lw + X; p[i] < v {
				p[i] = v
			}
		}
	}
	tx0, tx1 := float64(g.tyreSpan[0]), float64(g.tyreSpan[1])
	// A car cut off by the frame has its other tyre outside it: keep the under-body
	// shadow running to that edge.
	if g.touchL {
		tx0 = 0
	}
	if g.touchR {
		tx1 = float64(w - 1)
	}
	overhang := 0.10 * g.carW // shadow along the ground line fades over this beyond the tyres
	pad := 0.04 * g.carW      // ambient shadow reaches this far beyond the bumpers
	bx0, bx1 := float64(g.box.Min.X), float64(g.box.Max.X-1)
	lineUp, lineDown := 0.004*g.carH, 0.010*g.carH
	ambUp, ambDown := 0.10*g.carH, 0.012*g.carH
	if g.level {
		// Straight front/rear view: the floor under the car is hidden behind the bumper, the
		// visible shadow is the soft one falling forward onto the floor in front of it.
		ambDown = 0.05 * g.carH
	}
	tyreTol := 0.035 * g.carH
	for X := range lw {
		x := min(X*s+s/2, w-1)
		fx := float64(x)
		G := g.ground[x]
		out := 0.0
		if fx < tx0 {
			out = tx0 - fx
		} else if fx > tx1 {
			out = fx - tx1
		}
		// On a steep ground line the extrapolation leaves the car quickly: fade faster.
		slope := math.Abs(g.ground[min(x+1, w-1)]-g.ground[max(x-1, 0)]) / 2
		spanFade := 1 - smoothstep(0, overhang/(1+2*slope), out)
		inCar := fx >= bx0 && fx <= bx1
		b := g.bottom[x]
		if inCar && b >= 0 && float64(b) < G {
			fill(core, X, float64(b), G, float32(st.shadowUnder*spanFade))
		}
		if inCar {
			tyre := 0.0
			if b >= 0 {
				tyre = 1 - smoothstep(0, tyreTol, G-float64(b))
			}
			dens := max(tyre*spanFade, st.shadowLine*spanFade)
			fill(core, X, G-lineUp, G+lineDown, float32(dens))
		}
		if fx >= bx0-pad && fx <= bx1+pad {
			fade := 1.0
			if fx < bx0 {
				fade = 1 - smoothstep(0, pad, bx0-fx)
			} else if fx > bx1 {
				fade = 1 - smoothstep(0, pad, fx-bx1)
			}
			// Under an overhang the extrapolated ground line can run far below the car
			// (steep 3/4 views): the soft shadow then follows the bumper bottom, weaker.
			xc := min(max(x, g.box.Min.X), g.box.Max.X-1)
			ge, wgap := g.ground[xc], 1.0
			if bc := g.bottom[xc]; bc >= 0 {
				gap := (ge - float64(bc)) / g.carH
				wgap = 1 - 0.6*smoothstep(0.03, 0.2, gap)
				ge = min(ge, float64(bc)+0.05*g.carH)
			}
			ge += G - g.ground[xc] // keep the slope beyond the bumpers
			fill(amb, X, ge-ambUp, ge+ambDown, float32(fade*wgap))
		}
	}
	// Far-side tyres (above the ground line) get their own contact spot.
	for _, t := range g.tyres {
		if t.onGround {
			continue
		}
		hw := 1.2 * float64(t.halfW)
		for X := max(int((float64(t.x)-hw)/float64(s)), 0); X <= min(int((float64(t.x)+hw)/float64(s)), lw-1); X++ {
			x := min(X*s+s/2, w-1)
			b := g.bottom[x]
			if b < 0 {
				continue
			}
			u := (float64(x) - float64(t.x)) / hw
			fill(core, X, float64(b)-lineUp, float64(b)+1.5*lineDown, float32(max(0, 1-u*u)))
		}
	}
	ls := float64(s)
	gaussBlur(core, lw, lh, max(0.010*g.carW/ls, 1), max(0.0035*g.carW/ls, 0.7))
	sx := st.shadowSpread * g.carW / ls
	gaussBlur(amb, lw, lh, max(sx, 1), max(sx*st.shadowSquash, 0.7))
	mul := make([]float32, lw*lh)
	for i := range mul {
		mul[i] = float32((1 - st.shadowCore*float64(core[i])) * (1 - st.shadowAmbient*float64(amb[i])))
	}
	return &shadowMap{s: s, lw: lw, lh: lh, mul: mul}
}
