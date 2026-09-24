package studio

import (
	"image"
	"image/color"
	"math"
)

// Downscale returns img scaled so its long side is at most longSide px (aspect ratio
// kept, each side rounded, at least 1 px), as RGBA. A smaller image is copied at its
// own size (never upscaled). Downscaling averages the covered source pixels (box
// filter), which does not alias however large the ratio.
func Downscale(img image.Image, longSide int) *image.RGBA {
	b := img.Bounds()
	dw, dh := fitLongSide(b.Dx(), b.Dy(), longSide)
	return areaResize(b.Dx(), b.Dy(), dw, dh, rowReader(img))
}

// ScaleToWidth returns img scaled to width px wide (aspect ratio kept), the size of the
// job's thumbnails; an image that is not wider is returned as is.
func ScaleToWidth(img image.Image, width int) image.Image {
	b := img.Bounds()
	if width <= 0 || b.Dx() <= width {
		return img
	}
	h := max(1, int(math.Round(float64(b.Dy())*float64(width)/float64(b.Dx()))))
	return areaResize(b.Dx(), b.Dy(), width, h, rowReader(img))
}

// fitLongSide is the size of a w×h image scaled down to a long side of at most n.
func fitLongSide(w, h, n int) (int, int) {
	if n <= 0 || max(w, h) <= n {
		return w, h
	}
	s := float64(n) / float64(max(w, h))
	return max(1, int(math.Round(float64(w)*s))), max(1, int(math.Round(float64(h)*s)))
}

// rowReader returns a function writing row y (0-based) of m as RGB bytes into dst
// (3 per pixel). *image.RGBA and *image.YCbCr are read directly; other types go
// through the colour model.
func rowReader(m image.Image) func(y int, dst []uint8) {
	b := m.Bounds()
	w := b.Dx()
	switch m := m.(type) {
	case *image.RGBA:
		return func(y int, dst []uint8) {
			s := m.Pix[m.PixOffset(b.Min.X, b.Min.Y+y):]
			for x := range w {
				dst[x*3], dst[x*3+1], dst[x*3+2] = s[x*4], s[x*4+1], s[x*4+2]
			}
		}
	case *image.YCbCr:
		return func(y int, dst []uint8) {
			sy := b.Min.Y + y
			for x := range w {
				sx := b.Min.X + x
				yi, ci := m.YOffset(sx, sy), m.COffset(sx, sy)
				dst[x*3], dst[x*3+1], dst[x*3+2] = color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci])
			}
		}
	}
	return func(y int, dst []uint8) {
		for x := range w {
			c := color.RGBAModel.Convert(m.At(b.Min.X+x, b.Min.Y+y)).(color.RGBA)
			dst[x*3], dst[x*3+1], dst[x*3+2] = c.R, c.G, c.B
		}
	}
}

// tap is one source pixel's share of a destination pixel.
type tap struct {
	idx int
	w   float32
}

// areaTaps returns, for each of dst output pixels along one axis of src input pixels,
// the input pixels it covers and their weights (summing to 1). For dst > src it
// degenerates to nearest-neighbour-like coverage, which only happens for a copy.
func areaTaps(src, dst int) [][]tap {
	scale := float64(src) / float64(dst)
	out := make([][]tap, dst)
	for i := range out {
		lo, hi := float64(i)*scale, float64(i+1)*scale
		for j := int(lo); j < src && float64(j) < hi; j++ {
			a, b := max(lo, float64(j)), min(hi, float64(j+1))
			if b > a {
				out[i] = append(out[i], tap{idx: j, w: float32((b - a) / scale)})
			}
		}
	}
	return out
}

// areaResize builds a dw×dh opaque RGBA image from a sw×sh source read row by row
// (row writes RGB), with a separable box filter. Source rows are read in order, each
// at most twice, and only two filtered rows are held at a time.
func areaResize(sw, sh, dw, dh int, row func(y int, dst []uint8)) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, dw, dh))
	if sw == 0 || sh == 0 || dw == 0 || dh == 0 {
		return out
	}
	xt, yt := areaTaps(sw, dw), areaTaps(sh, dh)
	src := make([]uint8, sw*3)
	// Horizontally filtered rows, cached by source row (consecutive output rows share
	// at most one source row).
	cacheY := [2]int{-1, -1}
	cache := [2][]float32{make([]float32, dw*3), make([]float32, dw*3)}
	next := 0
	filtered := func(y int) []float32 {
		for k := range 2 {
			if cacheY[k] == y {
				return cache[k]
			}
		}
		k := next
		next ^= 1
		row(y, src)
		d := cache[k]
		for x, taps := range xt {
			var r, g, b float32
			for _, t := range taps {
				p := src[t.idx*3:]
				r += t.w * float32(p[0])
				g += t.w * float32(p[1])
				b += t.w * float32(p[2])
			}
			d[x*3], d[x*3+1], d[x*3+2] = r, g, b
		}
		cacheY[k] = y
		return d
	}
	acc := make([]float32, dw*3)
	for y, taps := range yt {
		clear(acc)
		for _, t := range taps {
			for i, v := range filtered(t.idx) {
				acc[i] += t.w * v
			}
		}
		o := out.Pix[y*out.Stride:]
		for x := range dw {
			o[x*4] = to8(float64(acc[x*3]))
			o[x*4+1] = to8(float64(acc[x*3+1]))
			o[x*4+2] = to8(float64(acc[x*3+2]))
			o[x*4+3] = 0xff
		}
	}
	return out
}
