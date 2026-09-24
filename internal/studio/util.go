package studio

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
)

// parallel runs fn over [0, n) in chunks of at least minChunk on up to workers goroutines
// (0 = GOMAXPROCS). Chunks are handed out dynamically, so uneven rows balance out.
func parallel(n, minChunk, workers int, fn func(lo, hi int)) {
	if n <= 0 {
		return
	}
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	chunk := max(minChunk, n/(workers*4), 1)
	if workers == 1 || n <= chunk {
		fn(0, n)
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(workers, (n+chunk-1)/chunk) {
		wg.Go(func() {
			for {
				lo := int(next.Add(int64(chunk))) - chunk
				if lo >= n {
					return
				}
				fn(lo, min(lo+chunk, n))
			}
		})
	}
	wg.Wait()
}

// toRGBA converts a decoded image to a new *image.RGBA with origin (0,0). For
// *image.YCbCr it uses exactly the conversion of draw.Draw / color.YCbCrToRGB, in
// parallel, so "the decoded original" is the same whichever path produced it.
func toRGBA(m image.Image, workers int) *image.RGBA {
	b := m.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	switch m := m.(type) {
	case *image.RGBA:
		for y := range h {
			o := m.PixOffset(b.Min.X, b.Min.Y+y)
			copy(out.Pix[y*out.Stride:y*out.Stride+w*4], m.Pix[o:o+w*4])
		}
		return out
	case *image.YCbCr:
		parallel(h, 16, workers, func(y0, y1 int) {
			for y := y0; y < y1; y++ {
				o := out.Pix[y*out.Stride : y*out.Stride+w*4]
				sy := b.Min.Y + y
				for x := range w {
					sx := b.Min.X + x
					yi, ci := m.YOffset(sx, sy), m.COffset(sx, sy)
					r, g, bl := color.YCbCrToRGB(m.Y[yi], m.Cb[ci], m.Cr[ci])
					o[x*4], o[x*4+1], o[x*4+2], o[x*4+3] = r, g, bl, 0xff
				}
			}
		})
		return out
	}
	draw.Draw(out, out.Bounds(), m, b.Min, draw.Src)
	return out
}

// grayPlane returns the w×h bytes of g row by row (a copy; g may be a sub-image).
func grayPlane(g *image.Gray) []uint8 {
	b := g.Bounds()
	w, h := b.Dx(), b.Dy()
	a := make([]uint8, w*h)
	for y := range h {
		o := g.PixOffset(b.Min.X, b.Min.Y+y)
		copy(a[y*w:(y+1)*w], g.Pix[o:o+w])
	}
	return a
}

func clamp01(v float64) float64 { return min(max(v, 0), 1) }

func smoothstep(e0, e1, x float64) float64 {
	if e1 == e0 {
		if x < e0 {
			return 0
		}
		return 1
	}
	t := clamp01((x - e0) / (e1 - e0))
	return t * t * (3 - 2*t)
}

func to8(v float64) uint8 {
	return uint8(min(max(v+0.5, 0), 255))
}

// boxBlurH writes to dst the horizontal box average of src (w×h, row-major) over the
// window [x-r, x+r], normalised by the number of in-bounds pixels.
func boxBlurH(dst, src []float32, w, h, r int) {
	if r <= 0 {
		copy(dst, src)
		return
	}
	for y := range h {
		s := src[y*w : (y+1)*w]
		d := dst[y*w : (y+1)*w]
		var sum float64
		for i := 0; i <= min(r, w-1); i++ {
			sum += float64(s[i])
		}
		for x := range w {
			lo, hi := x-r, x+r
			cnt := min(hi, w-1) - max(lo, 0) + 1
			d[x] = float32(sum / float64(cnt))
			if hi+1 < w {
				sum += float64(s[hi+1])
			}
			if lo >= 0 {
				sum -= float64(s[lo])
			}
		}
	}
}

// boxBlurV is boxBlurH along columns; acc is a scratch slice of at least w.
func boxBlurV(dst, src []float32, w, h, r int, acc []float64) {
	if r <= 0 {
		copy(dst, src)
		return
	}
	acc = acc[:w]
	clear(acc)
	for y := 0; y <= min(r, h-1); y++ {
		row := src[y*w : (y+1)*w]
		for x, v := range row {
			acc[x] += float64(v)
		}
	}
	for y := range h {
		lo, hi := y-r, y+r
		inv := 1 / float64(min(hi, h-1)-max(lo, 0)+1)
		d := dst[y*w : (y+1)*w]
		for x := range d {
			d[x] = float32(acc[x] * inv)
		}
		if hi+1 < h {
			for x, v := range src[(hi+1)*w : (hi+2)*w] {
				acc[x] += float64(v)
			}
		}
		if lo >= 0 {
			for x, v := range src[lo*w : (lo+1)*w] {
				acc[x] -= float64(v)
			}
		}
	}
}

// boxBlur blurs p in place (tmp: scratch of len(p), acc: scratch of w).
func boxBlur(p, tmp []float32, w, h, rx, ry int, acc []float64) {
	boxBlurH(tmp, p, w, h, rx)
	boxBlurV(p, tmp, w, h, ry, acc)
}

// gaussBlur approximates a Gaussian with sigmas sx, sy (pixels) by three box blurs.
func gaussBlur(p []float32, w, h int, sx, sy float64) {
	tmp := make([]float32, len(p))
	acc := make([]float64, w)
	rx, ry := boxRadius(sx), boxRadius(sy)
	for range 3 {
		boxBlur(p, tmp, w, h, rx, ry, acc)
	}
}

// boxRadius is the radius of one of three box blurs that together approximate a Gaussian
// of sigma s (width sqrt(12 s²/3 + 1)).
func boxRadius(s float64) int {
	if s <= 0 {
		return 0
	}
	wIdeal := math.Sqrt(12*s*s/3 + 1)
	return max(0, int(math.Round((wIdeal-1)/2)))
}

// dither is triangular noise in (-1, 1) (1 LSB peak), deterministic per pixel.
func dither(x, y int) float64 {
	h := uint32(x)*0x9E3779B1 ^ uint32(y)*0x85EBCA77 ^ 0x68E31DA4
	h ^= h >> 15
	h *= 0x2C1B3C6D
	h ^= h >> 12
	h *= 0x297A2D39
	h ^= h >> 15
	return float64(h&0xffff)/65536 + float64(h>>16)/65536 - 1
}
