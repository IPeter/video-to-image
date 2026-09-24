package studio

import "image"

// fgOptions configures the foreground colour estimation at soft mask edges.
type fgOptions struct {
	enabled bool
	// Box sizes of the two blur-fusion passes (Forte & Pitié 2021 use 90 and 6). The box
	// is 2·(radius/2)+1 pixels wide.
	radius1, radius2 int
	// The soft box is split into tiles of stripRows × tileCols; tiles without soft
	// pixels are skipped, the rest are computed with a halo of radius1/2 + radius2/2 on
	// every side. Memory per worker is 6 float32 planes of the tile plus halo.
	stripRows, tileCols int
}

var defaultFG = fgOptions{enabled: true, radius1: 90, radius2: 6, stripRows: 192, tileCols: 256}

// estimateForeground runs the two-pass blur-fusion foreground estimator ("Approximate
// Fast Foreground Colour Estimation", Forte & Pitié, ICIP 2021 — the same estimator as
// pymatting's estimate_foreground_ml family and BiRefNet's refine_foreground) on src
// (w×h, origin 0) and, once every tile is done, writes the estimated foreground colour
// F into src at every pixel with 0 < alpha < 255. Pixels with alpha 0 or 255 are never
// written. It returns the number of soft pixels.
//
//	pass(I, F, B, a, r):  Fb = blur_r(F·a) / blur_r(a)
//	                      Bb = blur_r(B·(1-a)) / blur_r(1-a)
//	                      F' = clip(Fb + a·(I - a·Fb - (1-a)·Bb))
//	F1, B1 = pass(I, I, I, a, 90);  F = pass(I, F1, B1, a, 6)
//
// The work is split into tiles (with a halo of r1/2 + r2/2) inside the bounding box of
// the soft pixels; only tiles containing soft pixels are computed, in parallel. Every
// tile reads the unmodified src, so the result does not depend on the tiling.
func estimateForeground(src *image.RGBA, a []uint8, w, h int, o fgOptions, workers int) int {
	x0, y0, x1, y1 := w, h, -1, -1
	soft := 0
	for y := range h {
		row := a[y*w : (y+1)*w]
		for x, v := range row {
			if v != 0 && v != 255 {
				soft++
				x0, x1 = min(x0, x), max(x1, x)
				y0, y1 = min(y0, y), max(y1, y)
			}
		}
	}
	if soft == 0 {
		return 0
	}
	r1, r2 := max(o.radius1/2, 1), max(o.radius2/2, 1)
	halo := r1 + r2 + 1
	// ROI: soft box plus a halo on every side, so every soft pixel sees exactly the
	// neighbourhood it would see in a full-image computation.
	rx0, ry0 := max(x0-halo, 0), max(y0-halo, 0)
	rx1, ry1 := min(x1+1+halo, w), min(y1+1+halo, h)
	stripRows := o.stripRows
	if stripRows <= 0 {
		stripRows = defaultFG.stripRows
	}
	tileCols := o.tileCols
	if tileCols <= 0 {
		tileCols = defaultFG.tileCols
	}
	// Tiles of stripRows × tileCols over the soft box; only tiles that contain soft
	// pixels are computed, each with a halo on every side (the car outline covers a
	// small part of the box).
	nty := (y1 - y0 + stripRows) / stripRows
	ntx := (x1 - x0 + tileCols) / tileCols
	has := make([]bool, nty*ntx)
	for y := y0; y <= y1; y++ {
		row := a[y*w : (y+1)*w]
		ty := (y - y0) / stripRows
		for x := x0; x <= x1; x++ {
			if v := row[x]; v != 0 && v != 255 {
				has[ty*ntx+(x-x0)/tileCols] = true
			}
		}
	}
	var tiles []int
	for i, ok := range has {
		if ok {
			tiles = append(tiles, i)
		}
	}
	// Buffers fit the largest tile plus halo, never more than the ROI.
	maxW := min(tileCols+2*halo, rx1-rx0)
	maxH := min(stripRows+2*halo, ry1-ry0)

	type bufs struct {
		a, a1, a2, p, q, tmp []float32
		acc                  []float64
	}
	pool := make(chan *bufs, 64)
	get := func() *bufs {
		select {
		case b := <-pool:
			return b
		default:
			n := maxW * maxH
			return &bufs{
				a: make([]float32, n), a1: make([]float32, n), a2: make([]float32, n),
				p: make([]float32, n), q: make([]float32, n), tmp: make([]float32, n),
				acc: make([]float64, maxW),
			}
		}
	}
	// One result per tile: the soft pixels (index into a) and their F (3 bytes each).
	type tileOut struct {
		idx []int32
		rgb []uint8
	}
	outs := make([]tileOut, len(tiles))

	parallel(len(tiles), 1, workers, func(lo, hi int) {
		b := get()
		defer func() {
			select {
			case pool <- b:
			default:
			}
		}()
		for ti := lo; ti < hi; ti++ {
			tile := tiles[ti]
			ty, tx := tile/ntx, tile%ntx
			sy0 := y0 + ty*stripRows
			sy1 := min(sy0+stripRows, y1+1)
			sx0 := x0 + tx*tileCols
			sx1 := min(sx0+tileCols, x1+1)
			ey0, ey1 := max(sy0-halo, ry0), min(sy1+halo, ry1)
			ex0, ex1 := max(sx0-halo, rx0), min(sx1+halo, rx1)
			eh, rw := ey1-ey0, ex1-ex0
			n := rw * eh
			af, a1, a2, p, q, tmp := b.a[:n], b.a1[:n], b.a2[:n], b.p[:n], b.q[:n], b.tmp[:n]
			for y := range eh {
				row := a[(ey0+y)*w+ex0 : (ey0+y)*w+ex1]
				for x, v := range row {
					af[y*rw+x] = float32(v) / 255
				}
			}
			// The soft pixels this tile answers for (its core, without the halo).
			var local []int32 // index into the tile planes
			out := tileOut{}
			for y := sy0; y < sy1; y++ {
				for x := sx0; x < sx1; x++ {
					if v := a[y*w+x]; v != 0 && v != 255 {
						local = append(local, int32((y-ey0)*rw+x-ex0))
						out.idx = append(out.idx, int32(y*w+x))
					}
				}
			}
			out.rgb = make([]uint8, 3*len(local))
			copy(a1, af)
			boxBlur(a1, tmp, rw, eh, r1, r1, b.acc)
			copy(a2, af)
			boxBlur(a2, tmp, rw, eh, r2, r2, b.acc)
			const eps = 1e-5
			for c := range 3 {
				img := func(i int) float32 {
					y, x := i/rw, i%rw
					return float32(src.Pix[(ey0+y)*src.Stride+(ex0+x)*4+c]) / 255
				}
				// pass 1: F = B = I
				for y := range eh {
					srow := src.Pix[(ey0+y)*src.Stride+ex0*4:]
					for x := range rw {
						i := y*rw + x
						v, al := float32(srow[x*4+c])/255, af[i]
						p[i] = v * al
						q[i] = v * (1 - al)
					}
				}
				boxBlur(p, tmp, rw, eh, r1, r1, b.acc)
				boxBlur(q, tmp, rw, eh, r1, r1, b.acc)
				for y := range eh {
					srow := src.Pix[(ey0+y)*src.Stride+ex0*4:]
					for x := range rw {
						i := y*rw + x
						v, al := float32(srow[x*4+c])/255, af[i]
						fb := p[i] / (a1[i] + eps)
						bb := q[i] / (1 - a1[i] + eps)
						f1 := min(max(fb+al*(v-al*fb-(1-al)*bb), 0), 1)
						p[i] = f1 * al       // input of pass 2's blur(F·a)
						q[i] = bb * (1 - al) // input of pass 2's blur(B·(1-a))
					}
				}
				// pass 2: F = F1, B = Bb1
				boxBlur(p, tmp, rw, eh, r2, r2, b.acc)
				boxBlur(q, tmp, rw, eh, r2, r2, b.acc)
				for k, li := range local {
					i := int(li)
					v, al := img(i), af[i]
					fb := p[i] / (a2[i] + eps)
					bb := q[i] / (1 - a2[i] + eps)
					f := min(max(fb+al*(v-al*fb-(1-al)*bb), 0), 1)
					out.rgb[k*3+c] = to8(float64(f) * 255)
				}
			}
			outs[ti] = out
		}
	})
	for _, t := range outs {
		for k, i := range t.idx {
			o := int(i)/w*src.Stride + int(i)%w*4
			copy(src.Pix[o:o+3], t.rgb[k*3:k*3+3])
		}
	}
	return soft
}
