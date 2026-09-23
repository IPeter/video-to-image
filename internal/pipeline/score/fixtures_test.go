package score

import (
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Synthetic fixtures, generated at test time. Sizes match the 640 px scoring copies.
const (
	fixW = 640
	fixH = 360
)

func clampU8(v int) uint8 {
	return uint8(max(0, min(255, v)))
}

func fill(w, h int, f func(x, y int) int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			g.Pix[y*g.Stride+x] = clampU8(f(x, y))
		}
	}
	return g
}

func uniform(w, h int, v uint8) *image.Gray {
	return fill(w, h, func(int, int) int { return int(v) })
}

// checkerboard alternates lo/hi in cell×cell squares.
func checkerboard(w, h, cell int, lo, hi uint8) *image.Gray {
	return fill(w, h, func(x, y int) int {
		if (x/cell+y/cell)%2 == 0 {
			return int(lo)
		}
		return int(hi)
	})
}

// noise is uniform noise in [40, 215] (well inside the exposure thresholds).
func noise(w, h int, seed uint64) *image.Gray {
	r := rand.New(rand.NewPCG(seed, 0))
	return fill(w, h, func(int, int) int { return 40 + r.IntN(176) })
}

// texture is a fine 2 px checker of ±12, strong for the Laplacian but averaged away by
// the 9x8 dHash downscale.
func texture(x, y int) int {
	if (x/2+y/2)%2 == 0 {
		return 12
	}
	return -12
}

// rampUp brightens left to right: every dHash bit is 1.
func rampUp(w, h int) *image.Gray {
	return fill(w, h, func(x, y int) int { return 30 + 190*x/(w-1) + texture(x, y) })
}

// rampDown darkens left to right: every dHash bit is 0.
func rampDown(w, h int) *image.Gray {
	return fill(w, h, func(x, y int) int { return 220 - 190*x/(w-1) + texture(x, y) })
}

// bands are 9 vertical bands alternating dark/bright: dHash bits alternate.
func bands(w, h int) *image.Gray {
	return fill(w, h, func(x, y int) int {
		if (x*9/w)%2 == 0 {
			return 60 + texture(x, y)
		}
		return 190 + texture(x, y)
	})
}

// mostly paints the left share of the width in v and leaves the rest textured.
func mostly(w, h int, share float64, v uint8) *image.Gray {
	cut := int(share * float64(w))
	return fill(w, h, func(x, y int) int {
		if x < cut {
			return int(v)
		}
		return 128 + 3*texture(x, y)
	})
}

// addNoise returns a copy of g with ±amp uniform noise.
func addNoise(g *image.Gray, amp int, seed uint64) *image.Gray {
	r := rand.New(rand.NewPCG(seed, 1))
	b := g.Bounds()
	return fill(b.Dx(), b.Dy(), func(x, y int) int {
		return int(g.GrayAt(b.Min.X+x, b.Min.Y+y).Y) + r.IntN(2*amp+1) - amp
	})
}

// boxBlur is a separable (2r+1)² box blur with clamped edges.
func boxBlur(g *image.Gray, r int) *image.Gray {
	b := g.Bounds()
	w, h := b.Dx(), b.Dy()
	at := func(src *image.Gray, x, y int) int {
		x = max(0, min(w-1, x))
		y = max(0, min(h-1, y))
		return int(src.Pix[y*src.Stride+x])
	}
	src := fill(w, h, func(x, y int) int { return int(g.GrayAt(b.Min.X+x, b.Min.Y+y).Y) })
	horiz := fill(w, h, func(x, y int) int {
		s := 0
		for d := -r; d <= r; d++ {
			s += at(src, x+d, y)
		}
		return s / (2*r + 1)
	})
	return fill(w, h, func(x, y int) int {
		s := 0
		for d := -r; d <= r; d++ {
			s += at(horiz, x, y+d)
		}
		return s / (2*r + 1)
	})
}

// toRGBA turns a grey image into an RGBA one, so jpeg.Encode writes a colour (YCbCr)
// JPEG like ffmpeg does, exercising the Y-plane fast path on decode.
func toRGBA(g *image.Gray) *image.RGBA {
	m := image.NewRGBA(g.Bounds())
	draw.Draw(m, m.Bounds(), g, g.Bounds().Min, draw.Src)
	return m
}

// writeJPEG encodes img at quality 90 into dir/name and returns the path.
func writeJPEG(t testing.TB, dir, name string, img image.Image) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, jpeg.Encode(f, img, &jpeg.Options{Quality: 90}))
	require.NoError(t, f.Close())
	return path
}

// laplacianVarianceRef is the naive reference for the Laplacian, via the image.Image interface.
func laplacianVarianceRef(g *image.Gray) float64 {
	b := g.Bounds()
	if b.Dx() < 3 || b.Dy() < 3 {
		return 0
	}
	v := func(x, y int) float64 { return float64(g.At(x, y).(color.Gray).Y) }
	var rs []float64
	for y := b.Min.Y + 1; y < b.Max.Y-1; y++ {
		for x := b.Min.X + 1; x < b.Max.X-1; x++ {
			rs = append(rs, v(x, y-1)+v(x, y+1)+v(x-1, y)+v(x+1, y)-4*v(x, y))
		}
	}
	var mean float64
	for _, r := range rs {
		mean += r
	}
	mean /= float64(len(rs))
	var ss float64
	for _, r := range rs {
		ss += (r - mean) * (r - mean)
	}
	return ss / float64(len(rs))
}
