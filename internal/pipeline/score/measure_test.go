package score

import (
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaplacianVarianceKnownValues(t *testing.T) {
	// 4x3 with one bright pixel at (1,1): interior responses are -400 at (1,1) and
	// +100 at (2,1); mean -150, population variance ((-250)² + 250²)/2 = 62500.
	g := image.NewGray(image.Rect(0, 0, 4, 3))
	g.SetGray(1, 1, color.Gray{Y: 100})
	assert.InDelta(t, 62500.0, LaplacianVariance(g), 1e-9)

	// A single interior response has zero variance.
	spike := image.NewGray(image.Rect(0, 0, 3, 3))
	spike.SetGray(1, 1, color.Gray{Y: 255})
	assert.Zero(t, LaplacianVariance(spike))

	assert.Zero(t, LaplacianVariance(uniform(fixW, fixH, 128)), "uniform image")
}

func TestLaplacianVarianceTinyAndNil(t *testing.T) {
	for _, r := range []image.Rectangle{
		image.Rect(0, 0, 0, 0), image.Rect(0, 0, 2, 5), image.Rect(0, 0, 5, 2), image.Rect(0, 0, 1, 1),
	} {
		assert.Zero(t, LaplacianVariance(noise(r.Dx(), r.Dy(), 1)), "size %v", r.Size())
	}
	assert.Zero(t, LaplacianVariance(nil))
	assert.Zero(t, LaplacianVariance(&image.Gray{Rect: image.Rect(0, 0, 10, 10)}), "missing Pix")
}

func TestLaplacianVarianceMatchesReference(t *testing.T) {
	for i, sz := range []image.Point{{3, 3}, {4, 7}, {50, 31}, {64, 64}, {fixW, fixH}} {
		g := noise(sz.X, sz.Y, uint64(i+1))
		assert.InDelta(t, laplacianVarianceRef(g), LaplacianVariance(g), 1e-6*laplacianVarianceRef(g)+1e-9, "size %v", sz)
	}
}

func TestLaplacianVarianceSubImage(t *testing.T) {
	big := noise(200, 120, 7)
	// The pixels around the sub-image are noise too, so reading outside it (a stride
	// or offset bug) would change the result.
	sub := big.SubImage(image.Rect(37, 21, 151, 99)).(*image.Gray)
	require.NotEqual(t, 0, sub.Rect.Min.X)

	cp := image.NewGray(image.Rect(0, 0, sub.Rect.Dx(), sub.Rect.Dy()))
	draw.Draw(cp, cp.Bounds(), sub, sub.Rect.Min, draw.Src)

	assert.InDelta(t, LaplacianVariance(cp), LaplacianVariance(sub), 1e-9)
	assert.InDelta(t, laplacianVarianceRef(sub), LaplacianVariance(sub), 1e-6)
	assert.InDelta(t, ExposurePenalty(cp), ExposurePenalty(sub), 1e-12)
}

func TestSharpScoresHigherThanBlurred(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]*image.Gray{
		"checkerboard": checkerboard(fixW, fixH, 8, 50, 200),
		"noise":        noise(fixW, fixH, 42),
		"ramp":         rampUp(fixW, fixH),
	}
	for name, sharp := range cases {
		t.Run(name, func(t *testing.T) {
			ms, err := measureFile(writeJPEG(t, dir, name+"_sharp.jpg", toRGBA(sharp)))
			require.NoError(t, err)
			mb, err := measureFile(writeJPEG(t, dir, name+"_blur.jpg", toRGBA(boxBlur(sharp, 3))))
			require.NoError(t, err)
			t.Logf("%s: sharp=%.1f blurred=%.1f", name, ms.Sharpness, mb.Sharpness)
			assert.Greater(t, ms.Sharpness, 10*mb.Sharpness, "sharp must score much higher")
		})
	}
}

func TestUniformGreyHasNoSharpnessOrExposurePenalty(t *testing.T) {
	m, err := measureFile(writeJPEG(t, t.TempDir(), "grey.jpg", toRGBA(uniform(fixW, fixH, 128))))
	require.NoError(t, err)
	assert.Less(t, m.Sharpness, 0.5)
	assert.Zero(t, m.ExposurePenalty)
}

func TestExposurePenalty(t *testing.T) {
	half := image.NewGray(image.Rect(0, 0, 10, 10))
	for i := range half.Pix {
		half.Pix[i] = 128
		if i%2 == 0 {
			half.Pix[i] = 0
		}
	}
	cases := []struct {
		name string
		g    *image.Gray
		want float64
	}{
		{"all black", uniform(8, 8, 0), 1},
		{"all white", uniform(8, 8, 255), 1},
		{"mid grey", uniform(8, 8, 128), 0},
		{"dark edge 12 counts", uniform(4, 4, DarkLumaMax), 1},
		{"13 does not", uniform(4, 4, DarkLumaMax+1), 0},
		{"242 does not", uniform(4, 4, BrightLumaMin-1), 0},
		{"bright edge 243 counts", uniform(4, 4, BrightLumaMin), 1},
		{"half black", half, 0.5},
		{"empty", image.NewGray(image.Rect(0, 0, 0, 0)), 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.InDelta(t, tc.want, ExposurePenalty(tc.g), 1e-12)
		})
	}
}

func TestExposureOfMostlyBlackAndWhiteJPEGs(t *testing.T) {
	dir := t.TempDir()
	for name, img := range map[string]*image.Gray{
		"black": mostly(fixW, fixH, 0.75, 0),
		"white": mostly(fixW, fixH, 0.75, 255),
	} {
		m, err := measureFile(writeJPEG(t, dir, name+".jpg", toRGBA(img)))
		require.NoError(t, err)
		assert.InDelta(t, 0.75, m.ExposurePenalty, 0.03, name)
	}
}

func TestHamming(t *testing.T) {
	cases := []struct {
		a, b uint64
		want int
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0, ^uint64(0), 64},
		{0xF0F0, 0x0F0F, 16},
		{0xDEADBEEF, 0xDEADBEEF, 0},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, Hamming(tc.a, tc.b), "%x^%x", tc.a, tc.b)
		assert.Equal(t, tc.want, Hamming(tc.b, tc.a), "symmetric")
	}
}

func TestToGray(t *testing.T) {
	src := noise(97, 61, 3) // odd sizes exercise 4:2:0 chroma rounding
	dir := t.TempDir()

	// Colour JPEG → *image.YCbCr → Y-plane copy, equal to a draw conversion within
	// the colour-model rounding.
	f, err := os.Open(writeJPEG(t, dir, "c.jpg", toRGBA(src)))
	require.NoError(t, err)
	defer f.Close()
	img, err := jpeg.Decode(f)
	require.NoError(t, err)
	ycc, ok := img.(*image.YCbCr)
	require.True(t, ok, "colour JPEG decodes to YCbCr, got %T", img)
	fast := toGray(ycc)
	slow := image.NewGray(ycc.Bounds())
	draw.Draw(slow, slow.Bounds(), ycc, ycc.Bounds().Min, draw.Src)
	require.Equal(t, slow.Rect, fast.Rect)
	for i := range fast.Pix {
		d := int(fast.Pix[i]) - int(slow.Pix[i])
		require.LessOrEqual(t, max(d, -d), 2, "pixel %d", i)
	}

	// *image.Gray is used as is.
	assert.Same(t, src, toGray(src))

	// Other types go through draw.
	g := toGray(toRGBA(src))
	assert.Equal(t, src.Pix, g.Pix)
}

func TestMeasureGrayscaleJPEG(t *testing.T) {
	// jpeg.Encode of *image.Gray writes a 1-component JPEG that decodes to *image.Gray.
	dir := t.TempDir()
	mg, err := measureFile(writeJPEG(t, dir, "g.jpg", rampUp(fixW, fixH)))
	require.NoError(t, err)
	mc, err := measureFile(writeJPEG(t, dir, "c.jpg", toRGBA(rampUp(fixW, fixH))))
	require.NoError(t, err)
	assert.LessOrEqual(t, Hamming(mg.DHash, mc.DHash), 2)
	assert.InEpsilon(t, mg.Sharpness, mc.Sharpness, 0.25)
}

func TestMeasureFileErrors(t *testing.T) {
	dir := t.TempDir()
	notJPEG := filepath.Join(dir, "x.jpg")
	require.NoError(t, os.WriteFile(notJPEG, []byte("definitely not a jpeg"), 0o600))

	_, err := measureFile(filepath.Join(dir, "missing.jpg"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing.jpg")

	_, err = measureFile(notJPEG)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode "+notJPEG)

	_, err = measureFile("")
	require.Error(t, err)
}

func BenchmarkLaplacianVariance(b *testing.B) {
	g := noise(fixW, fixH, 1)
	b.SetBytes(int64(len(g.Pix)))
	b.ReportAllocs()
	for b.Loop() {
		LaplacianVariance(g)
	}
}

func BenchmarkMeasureFile(b *testing.B) {
	path := writeJPEG(b, b.TempDir(), "bench.jpg", toRGBA(noise(fixW, fixH, 1)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := measureFile(path); err != nil {
			b.Fatal(err)
		}
	}
}
