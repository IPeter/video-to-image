package score

import (
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"math/bits"
	"os"

	"github.com/corona10/goimagehash"
)

// Exposure thresholds: the darkest and brightest 5% of the 0..255 luma range
// (0.05·255 = 12.75), so luma ≤ 12 counts as crushed and luma ≥ 243 as blown.
const (
	DarkLumaMax   = 12
	BrightLumaMin = 243
)

// LaplacianVariance is the sharpness measure (higher is sharper): the population
// variance of the 3x3 Laplacian [0 1 0; 1 -4 1; 0 1 0] over the interior pixels of g.
// Images smaller than 3x3 (and nil) score 0. It reads g.Pix directly, row by row via
// g.Stride, so sub-images are handled without per-pixel interface calls.
//
// The one-pass accumulators are int64, which is exact: a response is at most 1020 in
// magnitude, so the sum of squares only overflows beyond ~8.8e12 pixels.
func LaplacianVariance(g *image.Gray) float64 {
	if g == nil {
		return 0
	}
	w, h := g.Rect.Dx(), g.Rect.Dy()
	if w < 3 || h < 3 {
		return 0
	}
	pix, stride := g.Pix, g.Stride
	if stride < w || len(pix) < (h-1)*stride+w {
		return 0 // malformed image; never index out of range
	}
	var sum, sumSq int64
	for y := 1; y < h-1; y++ {
		// Five aligned views of the interior columns, resliced to one length so the
		// compiler drops the bounds checks in the inner loop.
		row := pix[y*stride : y*stride+w]
		mid := row[1 : w-1]
		left := row[:len(mid)]
		right := row[2 : 2+len(mid)]
		up := pix[(y-1)*stride+1:][:len(mid)]
		down := pix[(y+1)*stride+1:][:len(mid)]
		var rowSum, rowSq int64
		for x, c := range mid {
			r := int64(up[x]) + int64(down[x]) + int64(left[x]) + int64(right[x]) - 4*int64(c)
			rowSum += r
			rowSq += r * r
		}
		sum += rowSum
		sumSq += rowSq
	}
	n := float64((w - 2) * (h - 2))
	mean := float64(sum) / n
	v := float64(sumSq)/n - mean*mean
	if v < 0 { // rounding can push an all-equal response set a hair below zero
		return 0
	}
	return v
}

// ExposurePenalty is the share (0..1) of pixels with luma ≤ DarkLumaMax or
// ≥ BrightLumaMin, read from a 256-bin luma histogram. Empty images (and nil) → 0.
func ExposurePenalty(g *image.Gray) float64 {
	if g == nil {
		return 0
	}
	w, h := g.Rect.Dx(), g.Rect.Dy()
	if w <= 0 || h <= 0 {
		return 0
	}
	if g.Stride < w || len(g.Pix) < (h-1)*g.Stride+w {
		return 0
	}
	var hist [256]int
	for y := 0; y < h; y++ {
		for _, v := range g.Pix[y*g.Stride : y*g.Stride+w] {
			hist[v]++
		}
	}
	bad := 0
	for v := 0; v <= DarkLumaMax; v++ {
		bad += hist[v]
	}
	for v := BrightLumaMin; v < len(hist); v++ {
		bad += hist[v]
	}
	return float64(bad) / float64(w*h)
}

// Hamming returns the number of differing bits.
func Hamming(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// measurement is what one worker computes for one scoring copy.
type measurement struct {
	Sharpness       float64
	ExposurePenalty float64
	DHash           uint64
}

// measureFile decodes the JPEG at path and measures it.
func measureFile(path string) (measurement, error) {
	if path == "" {
		return measurement{}, errors.New("no scoring copy path")
	}
	f, err := os.Open(path)
	if err != nil {
		return measurement{}, err // *PathError already names the file
	}
	defer f.Close()
	img, err := jpeg.Decode(f)
	if err != nil {
		return measurement{}, fmt.Errorf("decode %s: %w", path, err)
	}
	m, err := measure(toGray(img))
	if err != nil {
		return measurement{}, fmt.Errorf("hash %s: %w", path, err)
	}
	return m, nil
}

// measure computes all per-frame metrics on the grayscale image. The dHash is taken
// from the luma plane: dHash is defined on grayscale, and resizing one channel is
// cheaper than resizing YCbCr.
func measure(g *image.Gray) (measurement, error) {
	h, err := goimagehash.DifferenceHash(g)
	if err != nil {
		return measurement{}, err
	}
	return measurement{
		Sharpness:       LaplacianVariance(g),
		ExposurePenalty: ExposurePenalty(g),
		DHash:           h.GetHash(),
	}, nil
}

// toGray returns the luma plane of img. *image.Gray is returned as is; *image.YCbCr
// (every colour JPEG) has its full-range Y plane copied, which is exactly JFIF luma;
// anything else is converted with draw.
func toGray(img image.Image) *image.Gray {
	switch m := img.(type) {
	case *image.Gray:
		return m
	case *image.YCbCr:
		b := m.Rect
		g := image.NewGray(b)
		w := b.Dx()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			src := m.YOffset(b.Min.X, y)
			dst := g.PixOffset(b.Min.X, y)
			copy(g.Pix[dst:dst+w], m.Y[src:src+w])
		}
		return g
	default:
		b := img.Bounds()
		g := image.NewGray(b)
		draw.Draw(g, b, img, b.Min, draw.Src)
		return g
	}
}
