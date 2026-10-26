package studio

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	"video-to-image/internal/analyzer"
)

// Generative studio background: instead of a drawn backdrop, an image model paints a
// photo studio around the car. The car pixels still come from the original: the car is
// cut out with the segmentation mask and put on a magenta canvas with a margin, the
// model fills only the magenta, and the result is alpha-composited under the original
// car. A margin is essential: a car that fills the frame makes the model recompose the
// scene (smaller car, recentred), and the paste-back no longer lines up; with a quarter
// of the frame added on every side the model keeps the car where it is (spike
// 2026-09-24: alignment error 6–8 of 255 with the margin, 75–80 without). Alignment is
// measured and a misaligned answer is rejected by the caller.
//
// Interiors get the same studio through the glass: the model is asked to change only
// what is seen outside the windows, and the result keeps every pixel it did not change
// from the original (InteriorCompose).

// StyleStudio is the generative style; it is rendered by the model, not by Render.
const StyleStudio Style = "studio"

// Generative defaults.
const (
	DefaultGenModel   = "gemini-3.1-flash-image"
	DefaultGenPad     = 0.25 // margin sent to the model, fraction of the photo's size on every side
	DefaultGenKeepPad = 0.10 // margin kept in the final photo
	DefaultGenLook    = "grey"
	// GenSendLongSide is the long side of the canvas sent to the model; its answer
	// comes back at about 1–1.4 K px anyway.
	GenSendLongSide = 1536
	GenSendQuality  = 90
	// MaxAlignment is the largest mean car-pixel difference (0–255, inside the eroded
	// mask, at 768 px) between the model's answer and the original for which the
	// answer counts as aligned. Aligned answers measure 6–8; a recomposed scene 25+.
	MaxAlignment = 15.0
	// MinInteriorChange is the least share of the glass area the model must have
	// changed for an interior answer to count (below it nothing was put behind the
	// windows). The cabin is safe either way: the blend is confined to the window boxes.
	MinInteriorChange = 0.05
)

// genLooks are the studio descriptions by look name.
var genLooks = map[string]string{
	"grey":  "a seamless light-grey cyclorama curving from the wall into a glossy floor, a large round dark turntable platform on the floor exactly under the car",
	"white": "a seamless pure-white cyclorama curving from the wall into a glossy white floor, a large round light turntable platform on the floor exactly under the car",
	"dark":  "a dark charcoal showroom studio with a black glossy floor, a large round turntable platform under the car, rim lights along the walls",
}

// GenLooks lists the known look names.
func GenLooks() []string { return []string{"grey", "white", "dark"} }

// ValidGenLook reports whether look is known ("" counts as the default).
func ValidGenLook(look string) bool {
	_, ok := genLooks[look]
	return look == "" || ok
}

// GenModel returns model or DefaultGenModel.
func GenModel(model string) string {
	if m := strings.TrimSpace(model); m != "" {
		return m
	}
	return DefaultGenModel
}

func genLook(look string) string {
	if s, ok := genLooks[look]; ok {
		return s
	}
	return genLooks[DefaultGenLook]
}

// WindowBoxModel is the text model that locates the glass of an interior photo.
const WindowBoxModel = "gemini-3.5-flash-lite"

// WindowFeather is the soft edge (fraction of the photo's short side) of a window box
// in the interior blend.
const WindowFeather = 0.02

// WindowBoxesRequest asks a cheap text model where the glass is in an interior photo:
// every window, windscreen and open door opening through which the outside is visible.
// The interior treatment then takes generated pixels only inside those boxes, so the
// model can never repaint the cabin (it did, on a dashboard, when the blend was left to
// the pixel difference alone). Step analyzer.StepStudioGen.
func WindowBoxesRequest(p GenParams) *analyzer.Request {
	zero, thousand := 0.0, 1000.0
	item := &analyzer.Schema{Type: "array", Description: "[ymin, xmin, ymax, xmax], integers 0-1000",
		Items: &analyzer.Schema{Type: "integer", Minimum: &zero, Maximum: &thousand}}
	schema := &analyzer.Schema{
		Type: "object",
		Properties: map[string]*analyzer.Schema{
			"boxes": {Type: "array", Description: "one box per glass area or door opening through which the outside is visible; empty if none", Items: item},
		},
		Required:         []string{"boxes"},
		PropertyOrdering: []string{"boxes"},
	}
	req := &analyzer.Request{
		Step:            analyzer.StepStudioGen,
		JobID:           p.JobID,
		DomainKey:       p.DomainKey,
		Model:           WindowBoxModel,
		Temperature:     0,
		ThinkingLevel:   "minimal",
		MaxOutputTokens: 1024,
		Parts: []analyzer.Part{
			{FilePath: p.CanvasPath, MIMEType: "image/jpeg"},
			{Text: "This is a photo of a car's interior. Return the bounding boxes of every area through which the OUTSIDE of the car " +
				"is visible: the windscreen, each side window, the rear window, sunroof, and any open door or tailgate opening. " +
				"Cover the glass area only, not the pillars, dashboard or seats. box_2d as [ymin, xmin, ymax, xmax], integers 0-1000. " +
				"An empty list if no outside is visible. JSON only."},
		},
		ResponseSchema: schema,
		SourceName:     p.SourceName,
	}
	if len(p.CanvasJPEG) > 0 {
		h := sha256.Sum256(p.CanvasJPEG)
		req.CacheKey = hex.EncodeToString(h[:]) + ":" + p.DomainKey + ":studio-windows"
	}
	return req
}

// ParseWindowBoxes decodes the answer of WindowBoxesRequest into normalised boxes
// (invalid entries dropped; at most 12).
func ParseWindowBoxes(text string) ([]Box, error) {
	obj, ok := jsonObject(text)
	if !ok {
		return nil, fmt.Errorf("%w: no JSON object in %q", ErrQABadOutput, clip(text, 120))
	}
	var raw struct {
		Boxes *[][]float64 `json:"boxes"`
	}
	if err := json.Unmarshal([]byte(obj), &raw); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrQABadOutput, err)
	}
	if raw.Boxes == nil {
		return nil, fmt.Errorf("%w: no boxes list", ErrQABadOutput)
	}
	var out []Box
	for _, b := range *raw.Boxes {
		if len(b) != 4 {
			continue
		}
		y0, x0, y1, x1 := clamp01(b[0]/1000), clamp01(b[1]/1000), clamp01(b[2]/1000), clamp01(b[3]/1000)
		if x1-x0 < 0.02 || y1-y0 < 0.02 {
			continue
		}
		out = append(out, Box{X0: x0, Y0: y0, X1: x1, Y1: y1})
		if len(out) == 12 {
			break
		}
	}
	return out, nil
}

// GenParams are the inputs of one generative call.
type GenParams struct {
	JobID, DomainKey, ImageID string
	Model                     string // "" → DefaultGenModel
	View                      string // shot ID, for the prompt
	Look                      string // grey, white, dark ("" → grey)
	Interior                  bool   // the window treatment instead of the cut-out
	// CanvasJPEG is what the model sees: the padded magenta cut-out (exterior) or the
	// photo itself (interior), at most GenSendLongSide px. CanvasPath is that file
	// (analyzer parts are files). WriteGenFile sets it. Width/Height are the canvas's
	// full-resolution size, for the aspect ratio.
	CanvasJPEG    []byte
	CanvasPath    string
	Width, Height int
	SourceName    string // upload file name, a hint for the fake analyzer only
	// Windows are the glass boxes of an interior photo (WindowBoxesRequest), in the
	// photo's normalised coordinates; the blend is confined to them.
	Windows []Box
}

// Canvas is the padded working image of an exterior photo.
type Canvas struct {
	Cutout *image.RGBA // car on magenta, padded
	Full   *image.RGBA // the photo on magenta, padded (the composite's foreground)
	Alpha  *image.Gray // the mask, padded
	Pad    image.Point // the margin in pixels (left/top; right/bottom equal)
	Inner  image.Rectangle
}

// magentaRGBA is the fill the model is asked to replace.
var magentaRGBA = color.RGBA{255, 0, 255, 255}

// BuildCanvas puts orig on a canvas with pad (fraction of its size, ≥ 0) of magenta on
// every side and returns the cut-out (car pixels where alpha > 127, magenta elsewhere),
// the full photo on the same canvas and the padded mask. alpha must match orig's size.
func BuildCanvas(orig image.Image, alpha *image.Gray, pad float64) (*Canvas, error) {
	if orig == nil || alpha == nil {
		return nil, errors.New("studio: nil image or mask")
	}
	b := orig.Bounds()
	w, h := b.Dx(), b.Dy()
	if ab := alpha.Bounds(); ab.Dx() != w || ab.Dy() != h || w == 0 || h == 0 {
		return nil, fmt.Errorf("%w: mask %v, image %v", ErrMaskSize, alpha.Bounds().Size(), b.Size())
	}
	if !(pad >= 0) || pad > 2 {
		pad = DefaultGenPad
	}
	px, py := int(math.Round(float64(w)*pad)), int(math.Round(float64(h)*pad))
	cw, ch := w+2*px, h+2*py
	inner := image.Rect(px, py, px+w, py+h)

	full := image.NewRGBA(image.Rect(0, 0, cw, ch))
	draw.Draw(full, full.Bounds(), image.NewUniform(magentaRGBA), image.Point{}, draw.Src)
	draw.Draw(full, inner, orig, b.Min, draw.Src)

	a := image.NewGray(image.Rect(0, 0, cw, ch))
	draw.Draw(a, inner, alpha, alpha.Bounds().Min, draw.Src)

	cut := image.NewRGBA(image.Rect(0, 0, cw, ch))
	copy(cut.Pix, full.Pix)
	for y := 0; y < ch; y++ {
		ai := a.PixOffset(0, y)
		ci := cut.PixOffset(0, y)
		for x := 0; x < cw; x++ {
			if a.Pix[ai+x] <= 127 {
				cut.Pix[ci+x*4], cut.Pix[ci+x*4+1], cut.Pix[ci+x*4+2], cut.Pix[ci+x*4+3] = 255, 0, 255, 255
			}
		}
	}
	return &Canvas{Cutout: cut, Full: full, Alpha: a, Pad: image.Pt(px, py), Inner: inner}, nil
}

// SendJPEG encodes img for the model: at most GenSendLongSide px, quality GenSendQuality.
func SendJPEG(img image.Image) ([]byte, error) {
	return EncodeJPEG(Downscale(img, GenSendLongSide), GenSendQuality)
}

// WriteGenFile writes p.CanvasJPEG to dir/<ImageID>_gen_canvas.jpg and sets CanvasPath.
func WriteGenFile(dir string, p *GenParams) error {
	if len(p.CanvasJPEG) == 0 {
		return errors.New("studio: generative canvas is empty")
	}
	id := p.ImageID
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return fmt.Errorf("studio: invalid image ID %q for the canvas file", id)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("studio: canvas dir: %w", err)
	}
	path := filepath.Join(dir, id+"_gen_canvas.jpg")
	if err := os.WriteFile(path, p.CanvasJPEG, 0o600); err != nil {
		return fmt.Errorf("studio: write canvas: %w", err)
	}
	p.CanvasPath = path
	return nil
}

// AspectRatio names the model's aspect ratio nearest to w:h (the values the API takes).
func AspectRatio(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	r := float64(w) / float64(h)
	opts := []struct {
		name string
		v    float64
	}{{"1:1", 1}, {"3:2", 1.5}, {"2:3", 2.0 / 3}, {"4:3", 4.0 / 3}, {"3:4", 0.75}, {"16:9", 16.0 / 9},
		{"9:16", 9.0 / 16}, {"4:5", 0.8}, {"5:4", 1.25}, {"21:9", 21.0 / 9}}
	best := opts[0]
	for _, o := range opts[1:] {
		if math.Abs(o.v-r) < math.Abs(best.v-r) {
			best = o
		}
	}
	return best.name
}

func genPromptExterior(look, view string) string {
	return "The image shows a used car cut out of its photo (view: " + viewLabel(view) + "): every pixel that is not the car is flat magenta (#FF00FF). " +
		"Fill ONLY the magenta area with a professional car photography studio seen from the same camera position as this photo: " + genLook(look) + ", " +
		"matching the car's footprint and perspective, soft even softbox lighting, a soft contact shadow under the car and a subtle floor reflection of the car. " +
		"The output must be pixel-aligned with the input: the car stays exactly where it is, at exactly the same size and angle, and every car pixel is unchanged. " +
		"Do not repaint, clean, move, crop or resize the car. Photorealistic."
}

func genPromptInterior(look string) string {
	return "This is a photo of a used car's interior for a sales listing. Replace ONLY what is visible OUTSIDE the car through the windscreen, the side windows and any open door " +
		"with a professional car photography studio: " + genLook(look) + ", large softbox lights. Everything inside the car must stay exactly as it is, pixel for pixel: " +
		"dashboard, steering wheel, seats, trim, dirt, wear, objects lying around. Same camera perspective and framing."
}

// GenRequest builds the image-model call. Step is analyzer.StepStudioGen; the answer
// is an image (Response.Images). CacheKey is sha256(canvas) + ":" + DomainKey +
// ":studio-gen" when the bytes are set.
func GenRequest(p GenParams) *analyzer.Request {
	model := GenModel(p.Model)
	prompt := genPromptExterior(p.Look, p.View)
	if p.Interior {
		prompt = genPromptInterior(p.Look)
	}
	req := &analyzer.Request{
		Step:               analyzer.StepStudioGen,
		JobID:              p.JobID,
		DomainKey:          p.DomainKey,
		Model:              model,
		Temperature:        0,
		ResponseModalities: []string{"IMAGE", "TEXT"},
		ImageAspectRatio:   AspectRatio(p.Width, p.Height),
		Parts: []analyzer.Part{
			{FilePath: p.CanvasPath, MIMEType: "image/jpeg"},
			{Text: prompt},
		},
		SourceName: p.SourceName,
	}
	if len(p.CanvasJPEG) > 0 {
		h := sha256.Sum256(p.CanvasJPEG)
		req.CacheKey = hex.EncodeToString(h[:]) + ":" + p.DomainKey + ":studio-gen"
	}
	return req
}

// DecodeImage decodes a generated PNG or JPEG.
func DecodeImage(im analyzer.Image) (image.Image, error) {
	if len(im.Data) == 0 {
		return nil, errors.New("studio: empty generated image")
	}
	switch {
	case strings.Contains(im.MIMEType, "png"):
		return png.Decode(bytes.NewReader(im.Data))
	case strings.Contains(im.MIMEType, "jpeg") || strings.Contains(im.MIMEType, "jpg"):
		return jpeg.Decode(bytes.NewReader(im.Data))
	}
	img, _, err := image.Decode(bytes.NewReader(im.Data))
	return img, err
}

// ComposeGenerated returns the generated background under the original car: gen is
// resized to the canvas (bilinear), and every pixel is alpha·full + (1−alpha)·gen with
// the padded mask as alpha. The mask gets Prepare's clean-up so the published cut-out
// edge is the one the QA check saw.
func ComposeGenerated(c *Canvas, gen image.Image) (*image.RGBA, error) {
	if c == nil || gen == nil {
		return nil, errors.New("studio: nil canvas or generated image")
	}
	b := c.Full.Bounds()
	w, h := b.Dx(), b.Dy()
	g := resizeBilinear(gen, w, h)
	a := grayPlane(c.Alpha)
	if defaultOptions.cleanup.enabled {
		cleanupAlpha(a, w, h, defaultOptions.cleanup)
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		av := float64(a[i]) / 255
		for ch := 0; ch < 3; ch++ {
			out.Pix[i*4+ch] = to8(av*float64(c.Full.Pix[i*4+ch]) + (1-av)*float64(g.Pix[i*4+ch]))
		}
		out.Pix[i*4+3] = 255
	}
	return out, nil
}

// AlignmentWidth is the width at which Alignment compares the images: coarse enough
// that JPEG and resampling noise (the model answers at ~1 K px) hardly counts, fine
// enough that a car moved by a few percent of the frame does.
const AlignmentWidth = 384

// Alignment measures how far the model moved the car: the mean absolute luminance
// difference (0–255) between the canvas and the generated image inside the mask eroded
// by 2 % of the short side, both box-filtered down to AlignmentWidth. Luminance, because
// the answer is a JPEG-shaped image whose chroma is subsampled and colour edges bleed;
// a moved car changes the luminance everywhere. -1 when the eroded mask is empty.
// Aligned answers measure well under 10, a recomposed scene 25 and more.
func Alignment(c *Canvas, gen image.Image) float64 {
	if c == nil || gen == nil {
		return -1
	}
	sw := AlignmentWidth
	o := Downscale(c.Full, sw)
	// The answer goes through the same box filter from its own size.
	g := Downscale(gen, sw)
	if g.Bounds().Size() != o.Bounds().Size() {
		g = resizeBilinear(g, o.Bounds().Dx(), o.Bounds().Dy())
	}
	sh := o.Bounds().Dy()
	a := Downscale(c.Alpha, sw)
	r := max(2, int(math.Round(0.02*float64(min(sw, sh)))))
	var sum float64
	n := 0
	for y := 0; y < sh; y++ {
		for x := 0; x < sw; x++ {
			if !erodedIn(a, x, y, r) {
				continue
			}
			i := o.PixOffset(x, y)
			sum += math.Abs(luma(o.Pix[i], o.Pix[i+1], o.Pix[i+2]) - luma(g.Pix[i], g.Pix[i+1], g.Pix[i+2]))
			n++
		}
	}
	if n == 0 {
		return -1
	}
	return sum / float64(n)
}

// luma is the Rec. 601 luminance of an sRGB pixel.
func luma(r, g, b uint8) float64 { return 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b) }

// erodedIn reports whether the r-neighbourhood (square) of (x, y) is all inside the
// mask (gray > 127, read from the red channel of the downscaled mask image).
func erodedIn(a *image.RGBA, x, y, r int) bool {
	b := a.Bounds()
	if x-r < 0 || y-r < 0 || x+r >= b.Dx() || y+r >= b.Dy() {
		return false
	}
	for yy := y - r; yy <= y+r; yy++ {
		for xx := x - r; xx <= x+r; xx++ {
			if a.Pix[a.PixOffset(xx, yy)] <= 127 {
				return false
			}
		}
	}
	return true
}

// CropCanvas cuts the composed canvas back to keep (fraction of the photo's size) of
// margin on every side, and scales the result down so its long side is at most the
// photo's long side (the generated background was upscaled; the car keeps its detail
// at that size).
func CropCanvas(c *Canvas, composed *image.RGBA, keep float64) *image.RGBA {
	if !(keep >= 0) {
		keep = DefaultGenKeepPad
	}
	w, h := c.Inner.Dx(), c.Inner.Dy()
	kx, ky := int(math.Round(float64(w)*keep)), int(math.Round(float64(h)*keep))
	kx, ky = min(kx, c.Pad.X), min(ky, c.Pad.Y)
	r := image.Rect(c.Inner.Min.X-kx, c.Inner.Min.Y-ky, c.Inner.Max.X+kx, c.Inner.Max.Y+ky)
	sub := composed.SubImage(r).(*image.RGBA)
	long := max(w, h)
	if max(r.Dx(), r.Dy()) > long {
		return Downscale(sub, long)
	}
	out := image.NewRGBA(image.Rect(0, 0, r.Dx(), r.Dy()))
	draw.Draw(out, out.Bounds(), sub, r.Min, draw.Src)
	return out
}

// InteriorCompose keeps the original everywhere outside the window boxes; inside them
// it takes the generated pixels where the model changed the picture, with a soft blend
// and a feathered box edge. It reports the share of the boxed area (at 768 px) that
// changed. No boxes: nothing changes (changed 0).
func InteriorCompose(orig image.Image, gen image.Image, windows []Box) (*image.RGBA, float64, error) {
	if orig == nil || gen == nil {
		return nil, 0, errors.New("studio: nil image")
	}
	b := orig.Bounds()
	w, h := b.Dx(), b.Dy()
	o := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(o, o.Bounds(), orig, b.Min, draw.Src)
	g := resizeBilinear(gen, w, h)

	// Difference map at 768 px, blurred, then back up to full size as the blend weight.
	sw := 768
	os_ := Downscale(o, sw)
	gs := resizeBilinear(gen, os_.Bounds().Dx(), os_.Bounds().Dy())
	dw, dh := os_.Bounds().Dx(), os_.Bounds().Dy()
	inWin := windowWeight(windows, dw, dh)
	d := make([]float32, dw*dh)
	changed, boxed := 0, 0
	for i := 0; i < dw*dh; i++ {
		v := (math.Abs(float64(os_.Pix[i*4])-float64(gs.Pix[i*4])) +
			math.Abs(float64(os_.Pix[i*4+1])-float64(gs.Pix[i*4+1])) +
			math.Abs(float64(os_.Pix[i*4+2])-float64(gs.Pix[i*4+2]))) / 3
		if inWin[i] > 0.5 {
			boxed++
			if v > 25 {
				changed++
			}
		}
		d[i] = float32(v)
	}
	tmp := make([]float32, dw*dh)
	boxBlurH(tmp, d, dw, dh, 6)
	boxBlurV(d, tmp, dw, dh, 6, make([]float64, dw))
	weight := image.NewGray(image.Rect(0, 0, dw, dh))
	for i, v := range d {
		// 0 below a difference of ~21, 1 above ~50 (v×3 mapped through (x−0.25)/0.35),
		// and only inside the (feathered) window boxes.
		t := clamp01((float64(v)*3/255-0.25)/0.35) * float64(inWin[i])
		weight.Pix[i] = to8(t * 255)
	}
	wf := resizeBilinearGray(weight, w, h)
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		t := float64(wf.Pix[i]) / 255
		for ch := 0; ch < 3; ch++ {
			out.Pix[i*4+ch] = to8((1-t)*float64(o.Pix[i*4+ch]) + t*float64(g.Pix[i*4+ch]))
		}
		out.Pix[i*4+3] = 255
	}
	if boxed == 0 {
		return out, 0, nil
	}
	return out, float64(changed) / float64(boxed), nil
}

// windowWeight rasterises the window boxes at w×h as a 0..1 weight, feathered by
// WindowFeather of the short side at every box edge.
func windowWeight(windows []Box, w, h int) []float32 {
	out := make([]float32, w*h)
	if len(windows) == 0 {
		return out
	}
	f := math.Max(1, WindowFeather*float64(min(w, h)))
	for y := 0; y < h; y++ {
		fy := (float64(y) + 0.5) / float64(h)
		for x := 0; x < w; x++ {
			fx := (float64(x) + 0.5) / float64(w)
			var best float64
			for _, b := range windows {
				// Distance inside the box (in pixels) along each axis, feathered.
				dx := math.Min(fx-b.X0, b.X1-fx) * float64(w)
				dy := math.Min(fy-b.Y0, b.Y1-fy) * float64(h)
				if dx <= -f || dy <= -f {
					continue
				}
				t := clamp01((dx+f)/(2*f)) * clamp01((dy+f)/(2*f))
				best = math.Max(best, t)
			}
			out[y*w+x] = float32(best)
		}
	}
	return out
}

// resizeBilinear scales img to w×h with bilinear interpolation (used to bring the
// model's ~1 K px answer up to the canvas; smooth content, so no ringing).
func resizeBilinear(img image.Image, w, h int) *image.RGBA {
	b := img.Bounds()
	sw, sh := b.Dx(), b.Dy()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	if sw == 0 || sh == 0 || w == 0 || h == 0 {
		return out
	}
	src := image.NewRGBA(image.Rect(0, 0, sw, sh))
	draw.Draw(src, src.Bounds(), img, b.Min, draw.Src)
	for y := 0; y < h; y++ {
		fy := (float64(y)+0.5)*float64(sh)/float64(h) - 0.5
		y0 := int(math.Floor(fy))
		ty := fy - float64(y0)
		y1 := min(max(y0+1, 0), sh-1)
		y0 = min(max(y0, 0), sh-1)
		for x := 0; x < w; x++ {
			fx := (float64(x)+0.5)*float64(sw)/float64(w) - 0.5
			x0 := int(math.Floor(fx))
			tx := fx - float64(x0)
			x1 := min(max(x0+1, 0), sw-1)
			x0 = min(max(x0, 0), sw-1)
			i00, i10 := src.PixOffset(x0, y0), src.PixOffset(x1, y0)
			i01, i11 := src.PixOffset(x0, y1), src.PixOffset(x1, y1)
			o := out.PixOffset(x, y)
			for ch := 0; ch < 3; ch++ {
				top := float64(src.Pix[i00+ch])*(1-tx) + float64(src.Pix[i10+ch])*tx
				bot := float64(src.Pix[i01+ch])*(1-tx) + float64(src.Pix[i11+ch])*tx
				out.Pix[o+ch] = to8(top*(1-ty) + bot*ty)
			}
			out.Pix[o+3] = 255
		}
	}
	return out
}

// resizeBilinearGray scales a gray image to w×h bilinearly.
func resizeBilinearGray(g *image.Gray, w, h int) *image.Gray {
	rgba := image.NewRGBA(g.Bounds())
	draw.Draw(rgba, rgba.Bounds(), g, g.Bounds().Min, draw.Src)
	r := resizeBilinear(rgba, w, h)
	out := image.NewGray(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		out.Pix[i] = r.Pix[i*4]
	}
	return out
}
