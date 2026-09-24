package studio

import (
	"image"
	"image/color"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDownscale(t *testing.T) {
	tests := []struct {
		w, h, long int
		ww, wh     int
	}{
		{3840, 2160, 1024, 1024, 576},
		{2160, 3840, 1024, 576, 1024},
		{1000, 500, 1024, 1000, 500}, // never upscaled
		{1025, 1, 1024, 1024, 1},     // at least 1 px
		{800, 600, 0, 800, 600},      // 0 = no limit
	}
	for _, tt := range tests {
		w, h := fitLongSide(tt.w, tt.h, tt.long)
		assert.Equal(t, [2]int{tt.ww, tt.wh}, [2]int{w, h}, "%dx%d → %d", tt.w, tt.h, tt.long)
	}
	assert.Equal(t, image.Rect(0, 0, 64, 36), Downscale(image.NewRGBA(image.Rect(0, 0, 384, 216)), 64).Rect)

	// A 2×1 checker of black and white columns averages to grey; a copy is exact.
	src := image.NewRGBA(image.Rect(0, 0, 400, 200))
	for y := range 200 {
		for x := range 400 {
			if x%2 == 0 {
				src.SetRGBA(x, y, color.RGBA{255, 255, 255, 255})
			} else {
				src.SetRGBA(x, y, color.RGBA{0, 0, 0, 255})
			}
		}
	}
	d := Downscale(src, 100)
	assert.Equal(t, color.RGBA{128, 128, 128, 255}, d.RGBAAt(10, 10))
	c := Downscale(src, 400)
	assert.Equal(t, src.Pix, c.Pix)

	// YCbCr and other image types are read through their colour model.
	yc := image.NewYCbCr(image.Rect(0, 0, 64, 32), image.YCbCrSubsampleRatio420)
	for i := range yc.Y {
		yc.Y[i] = 200
	}
	for i := range yc.Cb {
		yc.Cb[i], yc.Cr[i] = 128, 128
	}
	assert.Equal(t, color.RGBA{200, 200, 200, 255}, Downscale(yc, 16).RGBAAt(3, 3))
	g := image.NewGray(image.Rect(0, 0, 10, 10))
	for i := range g.Pix {
		g.Pix[i] = 77
	}
	assert.Equal(t, color.RGBA{77, 77, 77, 255}, Downscale(g, 5).RGBAAt(2, 2))
}

func TestScaleToWidth(t *testing.T) {
	big := image.NewRGBA(image.Rect(0, 0, 1920, 1080))
	s := ScaleToWidth(big, ThumbWidth)
	assert.Equal(t, image.Rect(0, 0, 640, 360), s.Bounds())
	small := image.NewRGBA(image.Rect(0, 0, 600, 400))
	assert.Same(t, small, ScaleToWidth(small, ThumbWidth).(*image.RGBA), "not wider: returned as is")
}
