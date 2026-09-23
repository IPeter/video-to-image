package export

import (
	"fmt"
	"image"
	"image/color"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forward maps source pixel (x, y) of a w×h image to its position after turning the
// image deg degrees clockwise, by repeating the 90° step (x, y) → (h-1-y, x).
func forward(x, y, w, h, deg int) (int, int) {
	for range deg / 90 {
		x, y, w, h = h-1-y, x, h, w
	}
	return x, y
}

func randomYCbCr(rng *rand.Rand, r image.Rectangle, ratio image.YCbCrSubsampleRatio) *image.YCbCr {
	m := image.NewYCbCr(r, ratio)
	for _, p := range [][]byte{m.Y, m.Cb, m.Cr} {
		for i := range p {
			p[i] = byte(rng.IntN(256))
		}
	}
	return m
}

// TestRotateYCbCr turns random images of every subsampling, even and odd sizes, and
// padded or offset layouts, and compares every pixel with the source.
func TestRotateYCbCr(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	ratios := map[image.YCbCrSubsampleRatio]string{
		image.YCbCrSubsampleRatio444: "444", image.YCbCrSubsampleRatio422: "422",
		image.YCbCrSubsampleRatio420: "420", image.YCbCrSubsampleRatio440: "440",
		image.YCbCrSubsampleRatio411: "411", image.YCbCrSubsampleRatio410: "410",
	}
	type layout struct {
		name string
		make func(ratio image.YCbCrSubsampleRatio) *image.YCbCr
	}
	layouts := []layout{
		{"even", func(r image.YCbCrSubsampleRatio) *image.YCbCr { return randomYCbCr(rng, image.Rect(0, 0, 34, 20), r) }},
		{"odd", func(r image.YCbCrSubsampleRatio) *image.YCbCr { return randomYCbCr(rng, image.Rect(0, 0, 33, 19), r) }},
		{"padded like the JPEG decoder", func(r image.YCbCrSubsampleRatio) *image.YCbCr {
			return randomYCbCr(rng, image.Rect(0, 0, 48, 32), r).SubImage(image.Rect(0, 0, 38, 22)).(*image.YCbCr)
		}},
		{"even offset", func(r image.YCbCrSubsampleRatio) *image.YCbCr {
			return randomYCbCr(rng, image.Rect(0, 0, 48, 32), r).SubImage(image.Rect(4, 6, 40, 26)).(*image.YCbCr)
		}},
		{"odd offset", func(r image.YCbCrSubsampleRatio) *image.YCbCr {
			return randomYCbCr(rng, image.Rect(0, 0, 48, 32), r).SubImage(image.Rect(3, 5, 39, 25)).(*image.YCbCr)
		}},
	}
	for ratio, rname := range ratios {
		for _, l := range layouts {
			for _, deg := range []int{90, 180, 270} {
				t.Run(fmt.Sprintf("%s/%s/%d", rname, l.name, deg), func(t *testing.T) {
					src := l.make(ratio)
					b := src.Bounds()
					w, h := b.Dx(), b.Dy()
					got := rotate(src, deg)
					dw, dh := rotatedSize(w, h, deg)
					require.Equal(t, image.Rect(0, 0, dw, dh), got.Bounds())

					exact := ratio != image.YCbCrSubsampleRatio411 && ratio != image.YCbCrSubsampleRatio410 &&
						(ratio == image.YCbCrSubsampleRatio444 || (w%2 == 0 && h%2 == 0 && b.Min.X%2 == 0 && b.Min.Y%2 == 0))
					if exact {
						g, ok := got.(*image.YCbCr)
						require.True(t, ok, "rotated plane by plane, got %T", got)
						want, _ := rotatedRatio(ratio, deg)
						assert.Equal(t, want, g.SubsampleRatio)
					} else {
						require.IsType(t, &image.RGBA{}, got, "falls back to RGBA")
					}
					for y := range h {
						for x := range w {
							dx, dy := forward(x, y, w, h, deg)
							s := src.YCbCrAt(b.Min.X+x, b.Min.Y+y)
							if g, ok := got.(*image.YCbCr); ok {
								if gc := g.YCbCrAt(dx, dy); gc != s {
									t.Fatalf("(%d,%d)→(%d,%d): got %v, want %v", x, y, dx, dy, gc, s)
								}
								continue
							}
							wc := color.RGBAModel.Convert(s).(color.RGBA)
							gc := got.(*image.RGBA).RGBAAt(dx, dy)
							if absDiff(gc.R, wc.R) > 1 || absDiff(gc.G, wc.G) > 1 || absDiff(gc.B, wc.B) > 1 {
								t.Fatalf("(%d,%d)→(%d,%d): got %v, want %v", x, y, dx, dy, gc, wc)
							}
						}
					}
				})
			}
		}
	}
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func TestRotateGrayRGBAAndOthers(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 6))
	gray := image.NewGray(image.Rect(0, 0, 40, 30))
	rgba := image.NewRGBA(image.Rect(0, 0, 40, 30))
	cmyk := image.NewCMYK(image.Rect(0, 0, 40, 30))
	for i := range gray.Pix {
		gray.Pix[i] = byte(rng.IntN(256))
	}
	for i := range rgba.Pix {
		rgba.Pix[i] = byte(rng.IntN(256))
	}
	for i := range cmyk.Pix {
		cmyk.Pix[i] = byte(rng.IntN(256))
	}
	for _, src := range []image.Image{
		gray, rgba, cmyk,
		gray.SubImage(image.Rect(3, 5, 37, 26)),
		rgba.SubImage(image.Rect(3, 5, 37, 26)),
	} {
		for _, deg := range []int{0, 90, 180, 270} {
			t.Run(fmt.Sprintf("%T %v/%d", src, src.Bounds(), deg), func(t *testing.T) {
				b := src.Bounds()
				w, h := b.Dx(), b.Dy()
				got := rotate(src, deg)
				if deg == 0 {
					assert.Same(t, src, got, "no rotation, no copy")
					return
				}
				dw, dh := rotatedSize(w, h, deg)
				require.Equal(t, image.Rect(0, 0, dw, dh), got.Bounds())
				for y := range h {
					for x := range w {
						dx, dy := forward(x, y, w, h, deg)
						want := color.RGBAModel.Convert(src.At(b.Min.X+x, b.Min.Y+y))
						require.Equal(t, want, color.RGBAModel.Convert(got.At(dx, dy)), "(%d,%d)", x, y)
					}
				}
			})
		}
	}
}

func TestBoxSpans(t *testing.T) {
	sum := func(s span) float32 {
		var v float32
		for _, w := range s.w {
			v += w
		}
		return v
	}
	for _, tc := range []struct{ n, dn int }{{4, 2}, {3, 2}, {3840, 640}, {2160, 640}, {1621, 640}, {7, 7}, {641, 640}, {2, 5}} {
		spans := boxSpans(tc.n, tc.dn)
		require.Len(t, spans, tc.dn)
		for i, s := range spans {
			assert.InDelta(t, 1, sum(s), 1e-5, "%v span %d", tc, i)
			assert.GreaterOrEqual(t, s.start, 0)
			assert.LessOrEqual(t, s.start+len(s.w), tc.n, "%v span %d", tc, i)
		}
	}
	// 3 → 2: the middle pixel is shared half and half.
	s := boxSpans(3, 2)
	assert.Equal(t, 0, s[0].start)
	assert.InDeltaSlice(t, []float32{2.0 / 3, 1.0 / 3}, s[0].w, 1e-6)
	assert.Equal(t, 1, s[1].start)
	assert.InDeltaSlice(t, []float32{1.0 / 3, 2.0 / 3}, s[1].w, 1e-6)
}

func TestResizeArea(t *testing.T) {
	// A 1 px checkerboard of black and white averages to mid grey.
	cb := image.NewGray(image.Rect(0, 0, 64, 48))
	for y := range 48 {
		for x := range 64 {
			if (x+y)%2 == 0 {
				cb.Pix[y*cb.Stride+x] = 255
			}
		}
	}
	out := resizeArea(cb, 16, 12)
	require.Equal(t, image.Rect(0, 0, 16, 12), out.Bounds())
	for y := range 12 {
		for x := range 16 {
			c := out.RGBAAt(x, y)
			assert.InDelta(t, 128, int(c.R), 1)
			assert.Equal(t, c.R, c.G)
			assert.Equal(t, uint8(255), c.A)
		}
	}

	// Exact fractional averaging: 3 → 2 columns.
	row := image.NewRGBA(image.Rect(0, 0, 3, 1))
	for x, v := range []uint8{0, 90, 180} {
		row.SetRGBA(x, 0, color.RGBA{v, v, 255 - v, 255})
	}
	out = resizeArea(row, 2, 1)
	assert.Equal(t, color.RGBA{30, 30, 225, 255}, out.RGBAAt(0, 0)) // (0·2 + 90)/3
	assert.Equal(t, color.RGBA{150, 150, 105, 255}, out.RGBAAt(1, 0))

	// A YCbCr source (read without a full conversion) matches its RGBA conversion.
	rng := rand.New(rand.NewPCG(7, 8))
	y := randomYCbCr(rng, image.Rect(0, 0, 97, 61), image.YCbCrSubsampleRatio420).SubImage(image.Rect(3, 1, 97, 61))
	a, b := resizeArea(y, 20, 13), resizeArea(toRGBA(y), 20, 13)
	for i := range a.Pix {
		require.LessOrEqual(t, absDiff(a.Pix[i], b.Pix[i]), 1, "byte %d", i)
	}
}

func TestScaleToWidth(t *testing.T) {
	m := image.NewRGBA(image.Rect(0, 0, 1280, 720))
	assert.Equal(t, image.Rect(0, 0, 640, 360), scaleToWidth(m, 640).Bounds())
	tall := image.NewRGBA(image.Rect(0, 0, 720, 1280))
	assert.Equal(t, image.Rect(0, 0, 640, 1138), scaleToWidth(tall, 640).Bounds())
	thin := image.NewRGBA(image.Rect(0, 0, 3000, 1))
	assert.Equal(t, image.Rect(0, 0, 640, 1), scaleToWidth(thin, 640).Bounds(), "at least 1 px high")
	small := image.NewRGBA(image.Rect(0, 0, 500, 900))
	assert.Same(t, small, scaleToWidth(small, 640), "never upscaled")
}
