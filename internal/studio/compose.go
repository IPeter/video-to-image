package studio

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"math"
	"time"
)

// Encoding of the studio variants, the same as the export's edited images.
const (
	JPEGQuality  = 92  // full-size variant
	ThumbWidth   = 640 // thumbnail width (ScaleToWidth)
	ThumbQuality = 85
)

// Errors of Prepare and Render.
var (
	ErrEmptyMask = errors.New("studio: the mask contains no car")
	ErrMaskSize  = errors.New("studio: the mask does not match the image size")
)

// options are the compositor settings that are not part of the look (tests switch them).
type options struct {
	cleanup cleanupOptions
	fg      fgOptions
	dither  bool // ±1 LSB triangular noise on the backdrop against banding
	workers int  // 0 = GOMAXPROCS
}

var defaultOptions = options{cleanup: defaultCleanup, fg: defaultFG, dither: true}

// RenderReport describes a composite.
type RenderReport struct {
	Coverage      float64 `json:"coverage"`      // share of the image that is car (alpha ≥ 128)
	LevelFallback bool    `json:"levelFallback"` // no tyre contact found; the shadow uses the lowest car row
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	// OpaquePixels have alpha 255 after the clean-up: they are the original pixels.
	OpaquePixels int `json:"opaquePixels"`
	// SoftPixels have 0 < alpha < 255: the estimated car colour blended with the backdrop.
	SoftPixels int          `json:"softPixels"`
	Contacts   int          `json:"contacts"` // ground contacts found (tyres; 1 in the level fallback)
	Cleanup    CleanupStats `json:"cleanup"`
	PrepareMS  int64        `json:"prepareMs"` // conversion, clean-up, edge colours, geometry (once per photo)
	ComposeMS  int64        `json:"composeMs"` // shadow, backdrop and composite (per style)
}

// Prepared is a photo with its cleaned cut-out, ready to be rendered in any style.
// Everything that does not depend on the style (colour conversion, alpha clean-up,
// edge colours, mask geometry) is done once, so rendering three styles costs about one
// preparation plus three cheap composites. It is safe for concurrent Render calls.
type Prepared struct {
	// base is the decoded original; only its soft-edge pixels (0 < alpha < 255) hold
	// the estimated car colour instead.
	base   *image.RGBA
	alpha  []uint8 // cleaned, w×h
	w, h   int
	geom   *geometry
	opt    options
	report RenderReport // the style-independent part
}

// Prepare converts orig, cleans a copy of alpha (same size as orig; 255 = car) and
// measures the car. alpha itself is not modified.
//
// The clean-up never lowers alpha on the car: it snaps near-opaque edge pixels
// (alpha ≥ 250) to 255 and fills tiny enclosed holes (both then show the original
// pixels), and it drops near-transparent haze (alpha ≤ 4) and specks smaller than
// 0.05 % of the image that lie more than 2 % of the long side away from the car.
func Prepare(orig image.Image, alpha *image.Gray) (*Prepared, error) {
	return prepare(orig, alpha, defaultOptions)
}

func prepare(orig image.Image, alpha *image.Gray, opt options) (*Prepared, error) {
	if orig == nil || alpha == nil {
		return nil, errors.New("studio: nil image or mask")
	}
	t0 := time.Now()
	b := orig.Bounds()
	w, h := b.Dx(), b.Dy()
	if ab := alpha.Bounds(); ab.Dx() != w || ab.Dy() != h {
		return nil, fmt.Errorf("%w: mask %dx%d, image %dx%d", ErrMaskSize, ab.Dx(), ab.Dy(), w, h)
	}
	if w == 0 || h == 0 {
		return nil, ErrEmptyMask
	}
	p := &Prepared{alpha: grayPlane(alpha), w: w, h: h, opt: opt}
	p.base = toRGBA(orig, opt.workers)
	if opt.cleanup.enabled {
		p.report.Cleanup = cleanupAlpha(p.alpha, w, h, opt.cleanup)
	}
	g, ok := analyse(p.base, p.alpha, w, h)
	if !ok {
		return nil, ErrEmptyMask
	}
	p.geom = g
	if opt.fg.enabled {
		p.report.SoftPixels = estimateForeground(p.base, p.alpha, w, h, opt.fg, opt.workers)
	} else {
		for _, v := range p.alpha {
			if v != 0 && v != 255 {
				p.report.SoftPixels++
			}
		}
	}
	for _, v := range p.alpha {
		if v == 255 {
			p.report.OpaquePixels++
		}
	}
	p.report.Coverage = g.coverage
	p.report.LevelFallback = g.level
	p.report.Width, p.report.Height = w, h
	p.report.Contacts = len(g.contacts)
	p.report.PrepareMS = time.Since(t0).Milliseconds()
	return p, nil
}

// Alpha returns a copy of the cleaned mask: the cut-out every Render uses.
func (p *Prepared) Alpha() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, p.w, p.h))
	copy(g.Pix, p.alpha)
	return g
}

// Report is the style-independent part of the render report.
func (p *Prepared) Report() RenderReport { return p.report }

// Render composites the car on the style's backdrop with a contact shadow:
//
//	out = alpha·car + (1−alpha)·backdrop·shadow
//
// where car is the original pixel (and, at the soft edge only, the estimated car
// colour, so the old background does not bleed into the outline). Pixels with alpha =
// 255 are copied from the decoded original unchanged, byte for byte. The result has
// the photo's size and framing.
func (p *Prepared) Render(style Style) (image.Image, RenderReport, error) {
	st, ok := looks[style]
	if !ok {
		return nil, RenderReport{}, fmt.Errorf("studio: unknown style %q", style)
	}
	t0 := time.Now()
	rep := p.report
	w, h, g := p.w, p.h, p.geom
	sh := buildShadow(g, w, h, st)
	bd := newBackdrop(st, g, w, h)
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	parallel(h, 8, p.opt.workers, func(y0, y1 int) {
		for y := y0; y < y1; y++ {
			ar := p.alpha[y*w : (y+1)*w]
			o := out.Pix[y*out.Stride : y*out.Stride+w*4]
			s := p.base.Pix[y*p.base.Stride : y*p.base.Stride+w*4]
			row := bd.row(y)
			for x, av := range ar {
				q := o[x*4 : x*4+4 : x*4+4]
				if av == 255 {
					copy(q, s[x*4:x*4+4])
					continue
				}
				r, gg, b := bd.at(&row, x)
				m := sh.at(x, y)
				r, gg, b = r*m, gg*m, b*m
				if p.opt.dither {
					d := dither(x, y)
					r, gg, b = r+d, gg+d, b+d
				}
				if av == 0 {
					q[0], q[1], q[2], q[3] = to8(r), to8(gg), to8(b), 0xff
					continue
				}
				af := float64(av) / 255
				q[0] = to8(af*float64(s[x*4]) + (1-af)*r)
				q[1] = to8(af*float64(s[x*4+1]) + (1-af)*gg)
				q[2] = to8(af*float64(s[x*4+2]) + (1-af)*b)
				q[3] = 0xff
			}
		}
	})
	rep.ComposeMS = time.Since(t0).Milliseconds()
	return out, rep, nil
}

// Render composites orig on the style's backdrop with a contact shadow, using alpha
// (same size as orig; not modified). Pixels with alpha = 255 are copied unchanged; see
// Prepare for the clean-up and Prepared.Render for the formula. To render several
// styles of one photo, Prepare once and call Prepared.Render per style.
func Render(orig image.Image, alpha *image.Gray, style Style) (image.Image, RenderReport, error) {
	if !style.Valid() {
		return nil, RenderReport{}, fmt.Errorf("studio: unknown style %q", style)
	}
	p, err := Prepare(orig, alpha)
	if err != nil {
		return nil, RenderReport{}, err
	}
	return p.Render(style)
}

// EncodeJPEG encodes img at quality (1-100).
func EncodeJPEG(img image.Image, quality int) ([]byte, error) {
	if img == nil {
		return nil, errors.New("studio: nil image")
	}
	if quality < 1 || quality > 100 {
		return nil, fmt.Errorf("studio: JPEG quality %d is not in 1-100", quality)
	}
	var buf bytes.Buffer
	b := img.Bounds()
	buf.Grow(b.Dx() * b.Dy() / 4)
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return nil, fmt.Errorf("studio: encode JPEG: %w", err)
	}
	return buf.Bytes(), nil
}

// backdrop evaluates the cyclorama colour; everything that depends only on x or only on
// y is precomputed.
type backdrop struct {
	st      look
	w, h    int
	spotX   []float64 // spotlight gaussian along x
	poolX   []float64
	vigX    []float64 // ((x-cx)/(w/2))²
	cy      float64
	horizon float64
	cove    float64
	groundY float64
	carH    float64
}

type bdRow struct {
	base       [3]float64
	spot, pool float64
	vigY       float64
}

func newBackdrop(st look, g *geometry, w, h int) *backdrop {
	bd := &backdrop{
		st: st, w: w, h: h,
		horizon: g.horizonY(st.horizonAbove, h),
		cove:    max(st.coveWidth*g.carH, 0.02*float64(h)),
		carH:    g.carH,
	}
	carCx := float64(g.box.Min.X+g.box.Max.X) / 2
	cx := 0.5*float64(w)/2 + 0.5*carCx
	bd.cy = 0.5*float64(h)/2 + 0.5*float64(g.box.Min.Y+g.box.Max.Y)/2
	lowest := 0.0
	for _, p := range g.contacts {
		lowest = max(lowest, float64(p.Y))
	}
	bd.groundY = lowest
	bd.spotX = make([]float64, w)
	bd.poolX = make([]float64, w)
	bd.vigX = make([]float64, w)
	sw := max(st.spotWidth*float64(w), 1)
	pw := max(0.75*g.carW, 1)
	for x := range w {
		dx := float64(x) - cx
		bd.spotX[x] = math.Exp(-0.5 * dx * dx / (sw * sw))
		bd.poolX[x] = math.Exp(-0.5 * dx * dx / (pw * pw))
		v := dx / (float64(w) / 2)
		bd.vigX[x] = v * v
	}
	return bd
}

func lerp3(a, b rgb, t float64) [3]float64 {
	return [3]float64{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t, a[2] + (b[2]-a[2])*t}
}

func (bd *backdrop) floorWeight(y float64) float64 {
	return smoothstep(bd.horizon-bd.cove, bd.horizon+bd.cove, y)
}

func (bd *backdrop) row(y int) bdRow {
	st := bd.st
	fy := float64(y)
	tw := clamp01(fy / max(bd.horizon, 1))
	wall := lerp3(st.wallTop, st.wallBottom, tw)
	tf := clamp01((fy - bd.horizon) / max(float64(bd.h)-bd.horizon, 1))
	floor := lerp3(st.floorNear, st.floorFront, tf)
	m := bd.floorWeight(fy)
	var r bdRow
	for c := range 3 {
		r.base[c] = wall[c]*(1-m) + floor[c]*m
	}
	sh := max(st.spotHeight*float64(bd.h), 1)
	dy := fy - st.spotY*float64(bd.h)
	r.spot = st.spot * math.Exp(-0.5*dy*dy/(sh*sh)) * (1 - 0.6*m)
	ph := max(0.22*bd.carH, 1)
	dp := fy - bd.groundY
	r.pool = st.floorPool * math.Exp(-0.5*dp*dp/(ph*ph)) * m
	v := (fy - bd.cy) / (float64(bd.h) / 2)
	r.vigY = v * v
	return r
}

func (bd *backdrop) at(r *bdRow, x int) (float64, float64, float64) {
	add := r.spot*bd.spotX[x] + r.pool*bd.poolX[x]
	vig := 1 - bd.st.vignette*smoothstep(0.15, 2.0, bd.vigX[x]+r.vigY)
	return (r.base[0] + add) * vig, (r.base[1] + add) * vig, (r.base[2] + add) * vig
}
