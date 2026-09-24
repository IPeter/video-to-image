package studio

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var allStyles = []Style{StyleGrey, StyleLight, StyleDark}

// noisyJPEG returns a decoded JPEG (so *image.YCbCr, like a real photo) of random
// content.
func noisyJPEG(t testing.TB, w, h int) image.Image {
	t.Helper()
	r := rand.New(rand.NewPCG(1, 2))
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range m.Pix {
		if i%4 == 3 {
			m.Pix[i] = 255
			continue
		}
		m.Pix[i] = uint8(r.IntN(256))
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, m, &jpeg.Options{Quality: 90}))
	d, err := jpeg.Decode(&buf)
	require.NoError(t, err)
	_, ok := d.(*image.YCbCr)
	require.True(t, ok, "decoded %T, want *image.YCbCr", d)
	return d
}

// carMask draws a car-like blob: body, two wheels, a 1 px antenna, a soft 3 px edge,
// a detached antenna fragment close to the roof, a far speck and a tiny hole.
func carMask(w, h int) *image.Gray {
	g := image.NewGray(image.Rect(0, 0, w, h))
	a := g.Pix
	set := func(x, y int, v uint8) {
		if x >= 0 && y >= 0 && x < w && y < h && a[y*w+x] < v {
			a[y*w+x] = v
		}
	}
	for y := h / 3; y < h*3/4; y++ { // body
		for x := w / 8; x < w*7/8; x++ {
			set(x, y, 255)
		}
	}
	for _, cx := range []int{w / 4, w * 3 / 4} { // wheels
		cy, r := h*3/4, h/8
		for y := cy - r; y <= cy+r; y++ {
			for x := cx - r; x <= cx+r; x++ {
				if (x-cx)*(x-cx)+(y-cy)*(y-cy) <= r*r {
					set(x, y, 255)
				}
			}
		}
	}
	src := append([]uint8(nil), a...) // soft edge: 3 px ramp around everything opaque
	for y := range h {
		for x := range w {
			if src[y*w+x] != 255 {
				continue
			}
			for d := 1; d <= 3; d++ {
				v := uint8(255 - d*60)
				set(x-d, y, v)
				set(x+d, y, v)
				set(x, y-d, v)
				set(x, y+d, v)
			}
		}
	}
	// 1 px antenna on the roof (attached), and a detached fragment above it
	for y := h/3 - h/6; y < h/3; y++ {
		set(w/2, y, 255)
	}
	for y := h/3 - h/6 - 12; y < h/3-h/6-4; y++ {
		set(w/2, y, 200)
	}
	for y := 2; y < 5; y++ { // far speck in the top-left corner
		for x := 2; x < 5; x++ {
			set(x, y, 255)
		}
	}
	for y := h / 2; y < h/2+3; y++ { // tiny hole in the body
		for x := w / 2; x < w/2+3; x++ {
			a[y*w+x] = 0
		}
	}
	return g
}

// reference is "the decoded original" via the standard library.
func reference(src image.Image) *image.RGBA {
	b := src.Bounds()
	ref := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(ref, ref.Rect, src, b.Min, draw.Src)
	return ref
}

func mustPrepare(t testing.TB, src image.Image, alpha *image.Gray, opt options) *Prepared {
	t.Helper()
	p, err := prepare(src, alpha, opt)
	require.NoError(t, err)
	return p
}

func mustRender(t testing.TB, p *Prepared, st Style) *image.RGBA {
	t.Helper()
	out, _, err := p.Render(st)
	require.NoError(t, err)
	return out.(*image.RGBA)
}

// TestCarPixelsByteIdentical: every pixel with alpha = 255 (after the clean-up) is the
// decoded original, byte for byte, in every style and with every option.
func TestCarPixelsByteIdentical(t *testing.T) {
	const w, h = 400, 240
	src := noisyJPEG(t, w, h)
	ref := reference(src)
	for _, st := range allStyles {
		for _, fg := range []bool{true, false} {
			opt := defaultOptions
			opt.fg.enabled = fg
			opt.fg.stripRows = 37 // many strips with halos, to exercise the seams
			p := mustPrepare(t, src, carMask(w, h), opt)
			out, rep, err := p.Render(st)
			require.NoError(t, err)
			o := out.(*image.RGBA)
			require.Equal(t, image.Rect(0, 0, w, h), o.Rect)
			opaque := 0
			for i, v := range p.alpha {
				if v != 255 {
					continue
				}
				opaque++
				if !bytes.Equal(o.Pix[i*4:i*4+4], ref.Pix[i*4:i*4+4]) {
					t.Fatalf("%s fg=%v: pixel %d,%d = %v, original %v", st, fg, i%w, i/w, o.Pix[i*4:i*4+4], ref.Pix[i*4:i*4+4])
				}
			}
			require.NotZero(t, opaque)
			assert.Equal(t, opaque, rep.OpaquePixels)
		}
	}
}

// TestRenderLeavesInputAlone: the caller's mask is not modified (Render is called once
// per style with the same mask) and the contract Render equals Prepare + Render.
func TestRenderLeavesInputAlone(t *testing.T) {
	const w, h = 320, 200
	src := noisyJPEG(t, w, h)
	alpha := carMask(w, h)
	before := append([]uint8(nil), alpha.Pix...)
	out1, rep1, err := Render(src, alpha, StyleGrey)
	require.NoError(t, err)
	assert.Equal(t, before, alpha.Pix, "Render modified the mask")
	p, err := Prepare(src, alpha)
	require.NoError(t, err)
	out2, rep2, err := p.Render(StyleGrey)
	require.NoError(t, err)
	assert.Equal(t, out1.(*image.RGBA).Pix, out2.(*image.RGBA).Pix)
	rep1.PrepareMS, rep1.ComposeMS, rep2.PrepareMS, rep2.ComposeMS = 0, 0, 0, 0
	assert.Equal(t, rep1, rep2)
	assert.Equal(t, w, rep1.Width)
	assert.Equal(t, h, rep1.Height)
	assert.InDelta(t, 0.45, rep1.Coverage, 0.1)
	assert.Equal(t, 1, rep1.Cleanup.IslandsRemoved, "the far speck")
	cleaned := p.Alpha()
	assert.Zero(t, cleaned.GrayAt(3, 3).Y, "Alpha returns the cleaned mask")
	cleaned.Pix[0] = 99
	assert.NotEqual(t, uint8(99), p.alpha[0], "Alpha returns a copy")
}

// TestConcurrentRender: one Prepared renders its styles concurrently with the same
// result as one after the other (run with -race).
func TestConcurrentRender(t *testing.T) {
	const w, h = 320, 200
	p := mustPrepare(t, noisyJPEG(t, w, h), carMask(w, h), defaultOptions)
	want := map[Style][]uint8{}
	for _, st := range allStyles {
		want[st] = mustRender(t, p, st).Pix
	}
	got := make([][]uint8, len(allStyles))
	var wg sync.WaitGroup
	for i, st := range allStyles {
		wg.Go(func() {
			out, _, err := p.Render(st)
			if err == nil {
				got[i] = out.(*image.RGBA).Pix
			}
		})
	}
	wg.Wait()
	for i, st := range allStyles {
		assert.Equal(t, want[st], got[i], string(st))
	}
}

// TestBackgroundReplaced: alpha = 0 pixels carry no trace of the original.
func TestBackgroundReplaced(t *testing.T) {
	const w, h = 400, 240
	src := noisyJPEG(t, w, h)
	flat := image.NewRGBA(src.Bounds())
	draw.Draw(flat, flat.Rect, image.NewUniform(color.RGBA{200, 10, 10, 255}), image.Point{}, draw.Src)
	for _, st := range allStyles {
		p1 := mustPrepare(t, src, carMask(w, h), defaultOptions)
		p2 := mustPrepare(t, flat, carMask(w, h), defaultOptions)
		o1, o2 := mustRender(t, p1, st), mustRender(t, p2, st)
		for i, v := range p1.alpha {
			if v == 0 && !bytes.Equal(o1.Pix[i*4:i*4+4], o2.Pix[i*4:i*4+4]) {
				t.Fatalf("%s: background pixel %d,%d depends on the photo", st, i%w, i/w)
			}
		}
	}
}

// TestStylesDiffer: the three backdrops are different, and each is what its name says
// (light is bright, dark is dark, grey in between) at a background corner.
func TestStylesDiffer(t *testing.T) {
	const w, h = 400, 240
	p := mustPrepare(t, noisyJPEG(t, w, h), carMask(w, h), defaultOptions)
	luma := map[Style]float64{}
	for _, st := range allStyles {
		o := mustRender(t, p, st)
		c := o.RGBAAt(w-5, 5) // background: top-right corner
		luma[st] = 0.299*float64(c.R) + 0.587*float64(c.G) + 0.114*float64(c.B)
	}
	assert.Greater(t, luma[StyleLight], 200.0)
	assert.Less(t, luma[StyleDark], 60.0)
	assert.Greater(t, luma[StyleGrey], luma[StyleDark])
	assert.Less(t, luma[StyleGrey], luma[StyleLight])

	_, _, err := p.Render("purple")
	require.Error(t, err)
	_, _, err = Render(noisyJPEG(t, 32, 32), carMask(32, 32), "")
	require.Error(t, err)
}

// TestCleanupNeverErodes: the clean-up only raises alpha on the car, removes the far
// speck, keeps the 1 px antenna and its detached fragment, and fills the tiny hole.
func TestCleanupNeverErodes(t *testing.T) {
	const w, h = 400, 240
	in := carMask(w, h).Pix
	a := append([]uint8(nil), in...)
	o := defaultCleanup
	st := cleanupAlpha(a, w, h, o)
	for i := range a {
		x, y := i%w, i/w
		if x < 10 && y < 10 { // the speck
			require.Zero(t, a[i], "speck pixel %d,%d kept", x, y)
			continue
		}
		if in[i] > o.lowSnap && a[i] < in[i] {
			t.Fatalf("alpha lowered at %d,%d: %d → %d", x, y, in[i], a[i])
		}
	}
	for y := h/3 - h/6; y < h/3; y++ { // attached 1 px antenna
		require.NotZero(t, a[y*w+w/2], "antenna cut at y=%d", y)
	}
	for y := h/3 - h/6 - 12; y < h/3-h/6-4; y++ { // fragment 4 px above it
		require.NotZero(t, a[y*w+w/2], "detached antenna fragment removed at y=%d", y)
	}
	assert.Equal(t, uint8(255), a[(h/2+1)*w+w/2+1], "tiny hole not filled")
	assert.Equal(t, 1, st.IslandsRemoved)
	assert.Equal(t, 9, st.IslandPixels)
	assert.GreaterOrEqual(t, st.HolesFilled, 1)
}

// TestCleanupSnaps: near-opaque edge values become 255 (the original pixel), haze
// becomes 0, everything in between is kept.
func TestCleanupSnaps(t *testing.T) {
	const w, h = 64, 64
	a := make([]uint8, w*h)
	for y := 10; y < 50; y++ {
		for x := 10; x < 50; x++ {
			a[y*w+x] = 252 // BiRefNet's "certain" is 250-254, not 255
		}
	}
	a[5*w+30], a[9*w+30], a[9*w+31] = 3, 128, 249
	st := cleanupAlpha(a, w, h, defaultCleanup)
	assert.Equal(t, uint8(255), a[30*w+30])
	assert.Equal(t, 40*40, st.HighSnapped)
	assert.Zero(t, a[5*w+30])
	assert.Equal(t, 1, st.LowSnapped)
	assert.Equal(t, uint8(128), a[9*w+30])
	assert.Equal(t, uint8(249), a[9*w+31])
}

// TestLargeHoleKept: a big enclosed hole (a see-through window the mask left open) is
// not filled — that is the mask's decision.
func TestLargeHoleKept(t *testing.T) {
	const w, h = 400, 240
	a := carMask(w, h).Pix
	for y := h/3 + 10; y < h/3+60; y++ {
		for x := w/4 + 10; x < w/4+90; x++ {
			a[y*w+x] = 0
		}
	}
	cleanupAlpha(a, w, h, defaultCleanup)
	assert.Zero(t, a[(h/3+30)*w+w/4+50], "large hole filled")
}

// TestForegroundOnlyAtSoftEdge: with FG estimation the soft band changes, nothing else.
func TestForegroundOnlyAtSoftEdge(t *testing.T) {
	const w, h = 400, 240
	src := noisyJPEG(t, w, h)
	pOn := mustPrepare(t, src, carMask(w, h), defaultOptions)
	opt := defaultOptions
	opt.fg.enabled = false
	pOff := mustPrepare(t, src, carMask(w, h), opt)
	on, off := mustRender(t, pOn, StyleGrey), mustRender(t, pOff, StyleGrey)
	diff := 0
	for i, v := range pOn.alpha {
		if !bytes.Equal(on.Pix[i*4:i*4+4], off.Pix[i*4:i*4+4]) {
			if v == 0 || v == 255 {
				t.Fatalf("pixel %d,%d (alpha %d) changed by FG estimation", i%w, i/w, v)
			}
			diff++
		}
	}
	assert.NotZero(t, diff, "FG estimation changed nothing")
	assert.Equal(t, pOn.report.SoftPixels, pOff.report.SoftPixels)
	assert.NotZero(t, pOn.report.SoftPixels)
}

// TestTilesMatchSingleTile: the tile split of the FG estimator (and the worker count)
// does not change the result.
func TestTilesMatchSingleTile(t *testing.T) {
	const w, h = 400, 240
	src := noisyJPEG(t, w, h)
	opt := defaultOptions
	opt.fg.stripRows, opt.fg.tileCols = 23, 31
	o1 := mustRender(t, mustPrepare(t, src, carMask(w, h), opt), StyleDark)
	opt.fg.stripRows, opt.fg.tileCols, opt.workers = 100000, 100000, 1
	o2 := mustRender(t, mustPrepare(t, src, carMask(w, h), opt), StyleDark)
	assert.True(t, bytes.Equal(o1.Pix, o2.Pix), "tile seams change the result")
}

func TestPrepareErrors(t *testing.T) {
	src := noisyJPEG(t, 64, 64)
	_, err := Prepare(src, image.NewGray(image.Rect(0, 0, 64, 64)))
	assert.ErrorIs(t, err, ErrEmptyMask)
	_, err = Prepare(src, image.NewGray(image.Rect(0, 0, 32, 64)))
	assert.ErrorIs(t, err, ErrMaskSize)
	_, _, err = Render(src, image.NewGray(image.Rect(0, 0, 64, 63)), StyleGrey)
	assert.ErrorIs(t, err, ErrMaskSize)
	_, err = Prepare(nil, image.NewGray(image.Rect(0, 0, 1, 1)))
	assert.Error(t, err)
	_, err = Prepare(src, nil)
	assert.Error(t, err)

	// Haze only (≤ 4) is no car either: the clean-up removes it.
	haze := image.NewGray(image.Rect(0, 0, 64, 64))
	for i := range haze.Pix {
		haze.Pix[i] = 3
	}
	_, err = Prepare(src, haze)
	assert.ErrorIs(t, err, ErrEmptyMask)
}

// TestSubImageInputs: an orig or mask that is a sub-image (non-zero origin) is read
// from its own bounds.
func TestSubImageInputs(t *testing.T) {
	const w, h = 200, 120
	full := reference(noisyJPEG(t, w+20, h+10))
	sub := full.SubImage(image.Rect(20, 10, w+20, h+10)).(*image.RGBA)
	maskFull := image.NewGray(image.Rect(0, 0, w+5, h+7))
	m := carMask(w, h)
	for y := range h {
		copy(maskFull.Pix[(y+7)*maskFull.Stride+5:], m.Pix[y*w:(y+1)*w])
	}
	subMask := maskFull.SubImage(image.Rect(5, 7, w+5, h+7)).(*image.Gray)
	p := mustPrepare(t, sub, subMask, defaultOptions)
	o := mustRender(t, p, StyleLight)
	for i, v := range p.alpha {
		if v == 255 {
			x, y := i%w, i/w
			require.Equal(t, sub.RGBAAt(20+x, 10+y), o.RGBAAt(x, y), "pixel %d,%d", x, y)
		}
	}
}

// wheelScene is a silver car body on a grey floor with two dark rubber tyres (so the
// tyre detection finds contacts) and its mask; flat has no tyres: a flat-bottomed body
// like a straight front view with the tyres hidden behind the bumper.
func wheelScene(w, h int, tyres bool) (*image.RGBA, *image.Gray) {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	mask := image.NewGray(img.Rect)
	fill := func(x0, y0, x1, y1 int, c color.RGBA, disc bool) {
		cx, cy, r := (x0+x1)/2, (y0+y1)/2, (x1-x0)/2
		for y := max(y0, 0); y < min(y1, h); y++ {
			for x := max(x0, 0); x < min(x1, w); x++ {
				if disc && (x-cx)*(x-cx)+(y-cy)*(y-cy) > r*r {
					continue
				}
				img.SetRGBA(x, y, c)
				mask.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	draw.Draw(img, img.Rect, image.NewUniform(color.RGBA{120, 130, 110, 255}), image.Point{}, draw.Src)
	silver := color.RGBA{190, 192, 196, 255}
	fill(w/8, h/3, w*7/8, h*2/3, silver, false)
	if tyres {
		r := h / 8
		for _, cx := range []int{w / 4, w * 3 / 4} {
			fill(cx-r, h*2/3-r, cx+r, h*2/3+r, color.RGBA{28, 28, 30, 255}, true)
		}
	}
	return img, mask
}

// TestContactShadow: the shadow darkens the floor right under the tyres and never the
// car; with tyres there is no level fallback.
func TestContactShadow(t *testing.T) {
	const w, h = 480, 320
	img, mask := wheelScene(w, h, true)
	p := mustPrepare(t, img, mask, defaultOptions)
	require.False(t, p.report.LevelFallback, "tyres were not found")
	assert.GreaterOrEqual(t, p.report.Contacts, 2)
	st := looks[StyleLight]
	sh := buildShadow(p.geom, w, h, st)
	tyreBottom := h*2/3 + h/8
	for _, cx := range []int{w / 4, w * 3 / 4} {
		assert.Less(t, sh.at(cx, tyreBottom+1), 0.8, "no dark contact under the tyre at x=%d", cx)
	}
	assert.InDelta(t, 1, sh.at(w/2, h/10), 1e-3, "shadow on the wall far above the car")
	o := mustRender(t, p, StyleLight)
	ref := reference(img)
	for i, v := range p.alpha {
		if v == 255 {
			require.Equal(t, ref.Pix[i*4:i*4+4], o.Pix[i*4:i*4+4], "shadow over a car pixel")
		}
	}
}

// TestLevelFallbackShadowSpan: without tyres (a straight front/rear view) the report
// says so, and the shadow spans the whole bottom of the bumper and reaches the floor in
// front of it — not a short blob around one point.
func TestLevelFallbackShadowSpan(t *testing.T) {
	const w, h = 480, 320
	img, mask := wheelScene(w, h, false)
	p := mustPrepare(t, img, mask, defaultOptions)
	require.True(t, p.report.LevelFallback)
	assert.Equal(t, 1, p.report.Contacts)
	g := p.geom
	x0, x1 := w/8, w*7/8-1
	assert.LessOrEqual(t, g.tyreSpan[0], x0+2)
	assert.GreaterOrEqual(t, g.tyreSpan[1], x1-2)
	sh := buildShadow(g, w, h, looks[StyleGrey])
	bottom := h*2/3 - 1
	for _, f := range []float64{0.05, 0.25, 0.5, 0.75, 0.95} {
		x := x0 + int(f*float64(x1-x0))
		assert.Less(t, sh.at(x, bottom+2), 0.8, "no shadow under the bumper at x=%d", x)
	}
	carH := g.carH
	assert.Less(t, sh.at(w/2, bottom+int(0.03*carH)), 0.97, "the soft shadow does not reach the floor in front")

	// Render reports it too.
	_, rep, err := Render(img, mask, StyleGrey)
	require.NoError(t, err)
	assert.True(t, rep.LevelFallback)
}

// fixture is a public photo with its spike mask (testdata, ≤ 800 px; see
// testdata/SOURCES.txt).
type fixture struct {
	name string
	box  Box // the review box, normalised
}

var fixtures = []fixture{
	{"pub03_chevrolet_sonic_white_rear34", Box{0.045, 0.005, 0.955, 0.925}},
	{"pub04_mazda_silver_rear34_vandalised", Box{0.095, 0.06, 0.905, 0.95}},
	{"pub05_microvan_green_side_rusty", Box{0.135, 0.215, 0.905, 0.82}},
}

func loadFixture(t testing.TB, name string) (image.Image, *image.Gray) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name+".jpg"))
	require.NoError(t, err)
	defer f.Close()
	img, err := jpeg.Decode(f)
	require.NoError(t, err)
	mf, err := os.Open(filepath.Join("testdata", name+".png"))
	require.NoError(t, err)
	defer mf.Close()
	m, err := png.Decode(mf)
	require.NoError(t, err)
	g, ok := m.(*image.Gray)
	require.True(t, ok, "mask %s is %T", name, m)
	return img, g
}

// TestFixtureByteIdentical: on real photos with the spike's masks every car pixel —
// alpha 255 in the input and everything the clean-up made 255 (the 250-254 "certain"
// values of BiRefNet) — is the decoded original in every style.
func TestFixtureByteIdentical(t *testing.T) {
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			img, mask := loadFixture(t, fx.name)
			ref := reference(img)
			p, err := Prepare(img, mask)
			require.NoError(t, err)
			rep := p.Report()
			require.Zero(t, rep.Cleanup.IslandPixels, "no speck on these fixtures")
			w := img.Bounds().Dx()
			for _, st := range allStyles {
				out, _, err := p.Render(st)
				require.NoError(t, err)
				o := out.(*image.RGBA)
				car, input255 := 0, 0
				for i, v := range p.alpha {
					if v != 255 {
						require.NotEqual(t, uint8(255), mask.Pix[i], "alpha 255 lowered at %d,%d", i%w, i/w)
						continue
					}
					car++
					if mask.Pix[i] == 255 {
						input255++
					}
					if !bytes.Equal(o.Pix[i*4:i*4+4], ref.Pix[i*4:i*4+4]) {
						t.Fatalf("%s: car pixel %d,%d changed: %v, original %v", st, i%w, i/w, o.Pix[i*4:i*4+4], ref.Pix[i*4:i*4+4])
					}
				}
				require.Greater(t, car, 10000)
				t.Logf("%s: %d car pixels byte-identical (%d were 255 in the input), coverage %.3f, contacts %d, level %v",
					st, car, input255, rep.Coverage, rep.Contacts, rep.LevelFallback)
			}
		})
	}
}

func TestEncodeJPEG(t *testing.T) {
	img := reference(noisyJPEG(t, 64, 48))
	b, err := EncodeJPEG(img, 92)
	require.NoError(t, err)
	d, err := jpeg.Decode(bytes.NewReader(b))
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 64, 48), d.Bounds())
	for _, q := range []int{0, -1, 101} {
		_, err := EncodeJPEG(img, q)
		assert.Error(t, err, "quality %d", q)
	}
	_, err = EncodeJPEG(nil, 90)
	assert.Error(t, err)
}

// bigScene is a 4K photo (YCbCr like a decoded JPEG) with a realistic mask: the pub05
// spike mask scaled up bilinearly (a soft edge a few pixels wide, like BiRefNet's
// 1024² output resized to 4K).
func bigScene(b *testing.B) (image.Image, *image.Gray) {
	const w, h = 3840, 2160
	_, small := loadFixture(b, "pub05_microvan_green_side_rusty")
	sw, sh := small.Bounds().Dx(), small.Bounds().Dy()
	mask := image.NewGray(image.Rect(0, 0, w, h))
	for y := range h {
		fy := (float64(y)+0.5)*float64(sh)/float64(h) - 0.5
		y0 := min(max(int(fy), 0), sh-1)
		y1 := min(y0+1, sh-1)
		ty := min(max(fy-float64(y0), 0), 1)
		for x := range w {
			fx := (float64(x)+0.5)*float64(sw)/float64(w) - 0.5
			x0 := min(max(int(fx), 0), sw-1)
			x1 := min(x0+1, sw-1)
			tx := min(max(fx-float64(x0), 0), 1)
			v := (1-ty)*((1-tx)*float64(small.Pix[y0*sw+x0])+tx*float64(small.Pix[y0*sw+x1])) +
				ty*((1-tx)*float64(small.Pix[y1*sw+x0])+tx*float64(small.Pix[y1*sw+x1]))
			mask.Pix[y*w+x] = to8(v)
		}
	}
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	r := rand.New(rand.NewPCG(3, 4))
	for i := range img.Y {
		img.Y[i] = uint8(64 + r.IntN(128))
	}
	for i := range img.Cb {
		img.Cb[i], img.Cr[i] = uint8(118+r.IntN(20)), uint8(118+r.IntN(20))
	}
	return img, mask
}

// BenchmarkRender4K is the contract Render (prepare + one style) on a 4K photo. Target:
// < 1 s at GOMAXPROCS=4 (go test -bench Render4K -cpu 4).
func BenchmarkRender4K(b *testing.B) {
	img, mask := bigScene(b)
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := Render(img, mask, StyleGrey); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkPrepared4KThreeStyles prepares once and renders all three styles.
func BenchmarkPrepared4KThreeStyles(b *testing.B) {
	img, mask := bigScene(b)
	b.ResetTimer()
	for b.Loop() {
		p, err := Prepare(img, mask)
		if err != nil {
			b.Fatal(err)
		}
		for _, st := range allStyles {
			if _, _, err := p.Render(st); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkQAPreview4K is the magenta cut-out of a 4K photo at 1024 px.
func BenchmarkQAPreview4K(b *testing.B) {
	img, mask := bigScene(b)
	b.ResetTimer()
	for b.Loop() {
		if QAPreview(img, mask, DefaultQALongSide) == nil {
			b.Fatal("no preview")
		}
	}
}

// BenchmarkEncodeJPEG4K is the q92 encode of one 4K composite.
func BenchmarkEncodeJPEG4K(b *testing.B) {
	img, mask := bigScene(b)
	out, _, err := Render(img, mask, StyleGrey)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := EncodeJPEG(out, 92); err != nil {
			b.Fatal(err)
		}
	}
}
