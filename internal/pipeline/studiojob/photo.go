package studiojob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/metrics"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
	"video-to-image/internal/studio"
)

// qaAttempts: an unavailable model or an unreadable QA answer is asked once more.
const qaAttempts = 2

// storeTimeout bounds storing one photo's variants (six small objects).
const storeTimeout = time.Minute

// photo is one image between segmentation and its stored outcome.
type photo struct {
	im store.SelectedImage
	// outcome is set when the photo was decided before the QA check (withheld by a gate,
	// or failed); nil means ready unless the QA check withholds it.
	outcome  *store.ImageStudio
	needsQA  bool
	variants []variant // in rn.styles order
	qa       studio.QAParams
	diag     diagnostics
	// gen is the generative-studio call to make (nil: none); canvas its padded cut-out
	// (exterior) and orig the decoded photo (interior: the compose needs it).
	gen      *studio.GenParams
	canvas   *studio.Canvas
	orig     image.Image
	interior bool
}

// variant is one rendered style, encoded.
type variant struct {
	style         string
	full, thumb   []byte
	width, height int
}

// diagnostics are stored with the outcome (store.ImageStudio.Diagnostics) for ops: what
// was measured, what decided, how long it took.
type diagnostics struct {
	Box      *studio.Box          `json:"box,omitempty"` // in the exported image
	BoxGate  *gateDiag            `json:"boxGate,omitempty"`
	MaskGate *gateDiag            `json:"maskGate,omitempty"`
	Stats    *studio.MaskStats    `json:"stats,omitempty"`
	Models   map[string]string    `json:"models,omitempty"`
	Render   *studio.RenderReport `json:"render,omitempty"` // the style-independent part
	// Styles are the per-style results: composite time and JPEG size.
	Styles    map[string]styleDiag `json:"styles,omitempty"`
	QA        *qaDiag              `json:"qa,omitempty"`
	Gen       *genDiag             `json:"gen,omitempty"`
	TimingsMS map[string]int64     `json:"timingsMs,omitempty"`
	Error     string               `json:"error,omitempty"`
}

// genDiag is the generative studio call's outcome.
type genDiag struct {
	Model     string  `json:"model"`
	Interior  bool    `json:"interior,omitempty"`
	Pad       float64 `json:"pad,omitempty"`
	Calls     int     `json:"calls"`
	Alignment float64 `json:"alignment,omitempty"` // mean car-pixel difference, 0-255 (exterior)
	Windows   int     `json:"windows,omitempty"`   // glass boxes found (interior)
	Changed   float64 `json:"changed,omitempty"`   // share of the glass area changed (interior)
	OK        bool    `json:"ok"`
	Error     string  `json:"error,omitempty"`
}

type gateDiag struct {
	OK     bool               `json:"ok"`
	Code   string             `json:"code,omitempty"`
	Hard   bool               `json:"hard,omitempty"`
	Values map[string]float64 `json:"values,omitempty"`
}

type styleDiag struct {
	ComposeMS int64 `json:"composeMs"`
	Bytes     int   `json:"bytes"`
}

type qaDiag struct {
	Model           string   `json:"model"`
	Required        bool     `json:"required,omitempty"` // a hard case: QA ran although the domain turns it off
	OK              bool     `json:"ok"`
	MissingParts    []string `json:"missingParts,omitempty"`
	MinorParts      []string `json:"minorParts,omitempty"` // antennas: reported, never withheld for
	BackgroundKept  []string `json:"backgroundKept,omitempty"`
	LargeBackground bool     `json:"largeBackground,omitempty"`
	Notes           string   `json:"notes,omitempty"`
	Calls           int      `json:"calls"`
	Error           string   `json:"error,omitempty"`
}

func gateDiagOf(g studio.GateResult) *gateDiag {
	d := &gateDiag{OK: g.OK, Code: g.Code, Hard: g.Hard}
	if len(g.Values) > 0 {
		d.Values = make(map[string]float64, len(g.Values))
		for k, v := range g.Values {
			d.Values[k] = finite(v)
		}
	}
	return d
}

func (p *photo) withhold(reason string) *photo {
	if reason == "" {
		reason = ReasonFailed
	}
	p.outcome = &store.ImageStudio{Status: store.ImageStudioWithheld, Reason: reason}
	return p
}

func (p *photo) fail(what string, err error) *photo {
	p.outcome = &store.ImageStudio{Status: store.ImageStudioFailed, Reason: ReasonFailed}
	p.diag.Error = what + ": " + err.Error()
	return p
}

// prepare runs everything up to the QA check for one photo: the box gate, the
// segmentation (bounded by the photo timeout), the mask gates, the render of every style
// (default first) with its q92 JPEG and 640 px thumbnail, and the QA images. A photo the
// gates refuse or that fails is returned decided (photo.outcome). An error means the run
// must stop: the job is gone (errJobGone), the sidecar is unreachable (wrapping
// studio.ErrSegmenterUnavailable), storage failed or ctx ended.
func (rn *run) prepare(ctx context.Context, im store.SelectedImage) (*photo, error) {
	p := &photo{im: im, diag: diagnostics{TimingsMS: map[string]int64{}}}
	if rn.dom.Studio.IsInteriorView(im.ShotID) {
		return rn.prepareInterior(ctx, p)
	}
	if im.SubjectBox == nil {
		return p.withhold(ReasonNoBox), nil
	}
	box := studio.Box{X0: im.SubjectBox.X0, Y0: im.SubjectBox.Y0, X1: im.SubjectBox.X1, Y1: im.SubjectBox.Y1}
	p.diag.Box = &box
	bg := studio.CheckBox(box, rn.gates)
	p.diag.BoxGate = gateDiagOf(bg)
	if !bg.OK {
		return p.withhold(bg.Reason), nil
	}

	raw, err := rn.readImage(ctx, im)
	if errors.Is(err, storage.ErrNotFound) {
		// Deleted with the job, or never stored: tell the two apart.
		if aerr := rn.ensureActive(ctx); aerr != nil {
			return nil, aerr
		}
		return p.fail("read the exported photo", err), nil
	}
	if err != nil {
		return nil, err
	}
	orig, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return p.fail("decode the exported photo", err), nil
	}

	start := time.Now()
	mctx, cancel := context.WithTimeout(ctx, rn.photoTimeout())
	res, err := rn.Segmenter.Mask(mctx, raw, box)
	cancel()
	p.diag.TimingsMS["mask"] = time.Since(start).Milliseconds()
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, ctx.Err()
	case err != nil && Unavailable(err):
		if !errors.Is(err, studio.ErrSegmenterUnavailable) {
			err = fmt.Errorf("%w: %w", studio.ErrSegmenterUnavailable, err)
		}
		return nil, fmt.Errorf("studiojob: photo %s: %w", im.ID, err)
	case err != nil:
		rn.log.Warn("studio: segmentation failed", "image_id", im.ID, "err", err)
		return p.fail("segmentation", err), nil
	case res == nil || res.Alpha == nil:
		return p.fail("segmentation", errors.New("no mask")), nil
	case res.Alpha.Bounds().Size() != orig.Bounds().Size():
		return p.fail("segmentation", fmt.Errorf("%w: mask %v, photo %v", studio.ErrMaskSize, res.Alpha.Bounds().Size(), orig.Bounds().Size())), nil
	}
	for k, v := range res.TimingsMS {
		p.diag.TimingsMS["sidecar."+k] = v
	}
	stats := res.Stats
	p.diag.Stats, p.diag.Models = &stats, res.Models
	mg := studio.CheckMask(res.Stats, rn.gates)
	p.diag.MaskGate = gateDiagOf(mg)
	if !mg.OK {
		return p.withhold(mg.Reason), nil
	}

	start = time.Now()
	prep, err := studio.Prepare(orig, res.Alpha)
	switch {
	case errors.Is(err, studio.ErrEmptyMask):
		return p.withhold(ReasonCoverage), nil
	case err != nil:
		return p.fail("prepare the composite", err), nil
	}
	rep := prep.Report()
	p.diag.Render = &rep
	if g := rn.gates; g.MaxCoverage > 0 && (rep.Coverage < g.MinCoverage || rep.Coverage > g.MaxCoverage) {
		return p.withhold(ReasonCoverage), nil // spike gate G10, on the cleaned cut-out
	}
	p.diag.Styles = make(map[string]styleDiag, len(rn.styles))
	for _, style := range rn.styles {
		if !studio.Style(style).Drawn() {
			continue // the generative style is made by the model below
		}
		v, ms, err := render(prep, style)
		if err != nil {
			return p.fail("render "+style, err), nil
		}
		p.variants = append(p.variants, v)
		p.diag.Styles[style] = styleDiag{ComposeMS: ms, Bytes: len(v.full)}
	}
	p.diag.TimingsMS["render"] = time.Since(start).Milliseconds()

	if g := rn.dom.Studio.Generative; g.Enabled {
		if err := rn.prepareGen(p, orig, res.Alpha, false); err != nil {
			return nil, err
		}
	}

	hard := bg.Hard || mg.Hard
	p.needsQA = rn.dom.Studio.QA || hard
	if !p.needsQA {
		return p, nil
	}
	origQA, cutQA, err := studio.QAImages(orig, res.Alpha)
	if err != nil {
		return p.fail("encode the QA images", err), nil
	}
	p.qa = studio.QAParams{
		JobID: rn.job.ID, DomainKey: rn.job.DomainKey, ImageID: im.ID,
		Model: rn.dom.Studio.EffectiveQAModel(), View: im.ShotID,
		OriginalJPEG: origQA, CutoutJPEG: cutQA, SourceName: rn.job.VideoInfo.OriginalName,
	}
	if err := studio.WriteQAFiles(rn.work, &p.qa); err != nil {
		return nil, fmt.Errorf("studiojob: %w", err) // scratch disk trouble: retry later
	}
	p.diag.QA = &qaDiag{Model: p.qa.Model, Required: hard && !rn.dom.Studio.QA}
	return p, nil
}

// prepareInterior sets up the generative window treatment of an interior photo: no
// box, no mask, no drawn styles and no QA check; the model call decides alone.
func (rn *run) prepareInterior(ctx context.Context, p *photo) (*photo, error) {
	p.interior = true
	raw, err := rn.readImage(ctx, p.im)
	if errors.Is(err, storage.ErrNotFound) {
		if aerr := rn.ensureActive(ctx); aerr != nil {
			return nil, aerr
		}
		return p.fail("read the exported photo", err), nil
	}
	if err != nil {
		return nil, err
	}
	orig, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		return p.fail("decode the exported photo", err), nil
	}
	if err := rn.prepareGen(p, orig, nil, true); err != nil {
		return nil, err
	}
	return p, nil
}

// prepareGen builds the generative call of a photo: the padded magenta cut-out
// (exterior) or the photo itself (interior), written to the scratch dir. Only a
// scratch-disk problem is an error; anything else leaves p.gen nil with the reason in
// the diagnostics (the drawn styles still stand).
func (rn *run) prepareGen(p *photo, orig image.Image, alpha *image.Gray, interior bool) error {
	g := rn.dom.Studio.Generative
	pad := g.Pad
	if pad <= 0 {
		pad = studio.DefaultGenPad
	}
	params := studio.GenParams{
		JobID: rn.job.ID, DomainKey: rn.job.DomainKey, ImageID: p.im.ID,
		Model: g.Model, View: p.im.ShotID, Look: g.Look, Interior: interior,
		SourceName: rn.job.VideoInfo.OriginalName,
	}
	p.diag.Gen = &genDiag{Model: studio.GenModel(g.Model), Interior: interior}
	var send image.Image
	if interior {
		send = orig
		p.orig = orig
	} else {
		c, err := studio.BuildCanvas(orig, alpha, pad)
		if err != nil {
			p.diag.Gen.Error = "canvas: " + err.Error()
			return nil
		}
		p.canvas = c
		p.diag.Gen.Pad = pad
		send = c.Cutout
	}
	b := send.Bounds()
	params.Width, params.Height = b.Dx(), b.Dy()
	jpg, err := studio.SendJPEG(send)
	if err != nil {
		p.diag.Gen.Error = "encode: " + err.Error()
		return nil
	}
	params.CanvasJPEG = jpg
	if err := studio.WriteGenFile(rn.work, &params); err != nil {
		return fmt.Errorf("studiojob: %w", err) // scratch disk trouble: retry later
	}
	p.gen = &params
	return nil
}

// render composites one style and encodes it (q92) with its 640 px thumbnail. The
// rendered image is dropped once encoded (a 4K RGBA frame is ~33 MB).
func render(prep *studio.Prepared, style string) (variant, int64, error) {
	out, rep, err := prep.Render(studio.Style(style))
	if err != nil {
		return variant{}, 0, err
	}
	full, err := studio.EncodeJPEG(out, studio.JPEGQuality)
	if err != nil {
		return variant{}, 0, err
	}
	thumb, err := studio.EncodeJPEG(studio.ScaleToWidth(out, studio.ThumbWidth), studio.ThumbQuality)
	if err != nil {
		return variant{}, 0, fmt.Errorf("thumbnail: %w", err)
	}
	b := out.Bounds()
	return variant{style: style, full: full, thumb: thumb, width: b.Dx(), height: b.Dy()}, rep.ComposeMS, nil
}

func (rn *run) readImage(ctx context.Context, im store.SelectedImage) ([]byte, error) {
	if im.ImagePath == "" {
		return nil, storage.ErrNotFound // expired
	}
	rc, err := rn.Storage.Get(ctx, im.ImagePath)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, err
		}
		return nil, fmt.Errorf("studiojob: get %s: %w", im.ImagePath, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("studiojob: read %s: %w", im.ImagePath, err)
	}
	return b, nil
}

// qaCall is a QA check running in its own goroutine.
type qaCall struct{ done chan qaResult }

func (c *qaCall) wait() qaResult { return <-c.done }

// qaResult is a photo after its QA check (verdict nil and err nil: no check was needed).
type qaResult struct {
	p       *photo
	verdict *studio.QAVerdict
	err     error
}

// startChecks runs the photo's model calls in the background: the generative studio
// (runGen) and then the QA check (runQA), whichever the photo needs.
func (rn *run) startChecks(ctx context.Context, p *photo) *qaCall {
	c := &qaCall{done: make(chan qaResult, 1)}
	go func() {
		if p.gen != nil {
			if err := rn.runGen(ctx, p); err != nil {
				c.done <- qaResult{p: p, err: err}
				return
			}
		}
		if !p.needsQA {
			c.done <- qaResult{p: p}
			return
		}
		c.done <- rn.runQA(ctx, p)
	}()
	return c
}

// genAttempts: an unavailable model, an empty answer or a misaligned answer is asked
// once more (the second time with a wider margin, which is what fixes misalignment).
const genAttempts = 2

// runGen makes the generative studio variant of p: the model call (usage recorded as
// soon as it returns), the alignment check (exterior) or the change bounds (interior),
// the composite and its encoding. Failures are not the photo's failure: they are noted
// in the diagnostics, an exterior keeps its drawn styles and an interior ends withheld
// (complete). Only a context error is returned.
func (rn *run) runGen(ctx context.Context, p *photo) error {
	start := time.Now()
	d := p.diag.Gen
	defer func() {
		p.diag.TimingsMS["gen"] = time.Since(start).Milliseconds()
		if p.gen != nil {
			os.Remove(p.gen.CanvasPath)
		}
		p.canvas, p.orig = nil, nil // free the 4K frames
	}()
	if p.interior {
		if err := rn.findWindows(ctx, p); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.Error = truncate(err.Error(), 300)
			rn.log.Warn("studio: interior windows not found; no studio version", "image_id", p.im.ID, "err", err)
			return nil
		}
	}
	var lastErr error
	for attempt := 1; attempt <= genAttempts; attempt++ {
		req := studio.GenRequest(*p.gen)
		gctx, cancel := context.WithTimeout(ctx, rn.genTimeout())
		d.Calls++
		resp, err := rn.Analyzer.Generate(gctx, req)
		cancel()
		if resp != nil {
			rn.recordUsage(ctx, req.Step, req.Model, resp.Usage)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			if errors.Is(err, analyzer.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
				metrics.ModelErrors.WithLabelValues(req.Model, "unavailable").Inc()
				continue
			}
			metrics.ModelErrors.WithLabelValues(req.Model, "other").Inc()
			break
		}
		if len(resp.Images) == 0 {
			metrics.ModelErrors.WithLabelValues(req.Model, "bad_output").Inc()
			lastErr = fmt.Errorf("no image in the answer (%s)", truncate(resp.Text, 120))
			continue
		}
		gen, err := studio.DecodeImage(resp.Images[0])
		if err != nil {
			metrics.ModelErrors.WithLabelValues(req.Model, "bad_output").Inc()
			lastErr = fmt.Errorf("decode the answer: %w", err)
			continue
		}
		v, err := rn.composeGen(p, gen, attempt)
		if err != nil {
			lastErr = err
			if p.interior || attempt == genAttempts {
				break
			}
			// Misaligned: widen the margin and ask again.
			if rerr := rn.widenCanvas(p); rerr != nil {
				lastErr = errors.Join(lastErr, rerr)
				break
			}
			continue
		}
		p.variants = append([]variant{v}, p.variants...)
		if p.diag.Styles == nil {
			p.diag.Styles = map[string]styleDiag{}
		}
		p.diag.Styles[string(studio.StyleStudio)] = styleDiag{ComposeMS: time.Since(start).Milliseconds(), Bytes: len(v.full)}
		d.OK = true
		return nil
	}
	if lastErr != nil {
		d.Error = truncate(lastErr.Error(), 300)
		rn.log.Warn("studio: generative background not made", "image_id", p.im.ID, "interior", p.interior, "err", lastErr)
	}
	return nil
}

// findWindows asks the cheap text model where the glass is in an interior photo and
// keeps the boxes on p.gen. No glass → errNoGlass (the image call is not made).
func (rn *run) findWindows(ctx context.Context, p *photo) error {
	req := studio.WindowBoxesRequest(*p.gen)
	wctx, cancel := context.WithTimeout(ctx, rn.qaTimeout())
	p.diag.Gen.Calls++
	resp, err := rn.Analyzer.Generate(wctx, req)
	cancel()
	if resp != nil {
		rn.recordUsage(ctx, req.Step, req.Model, resp.Usage)
	}
	if err != nil {
		return fmt.Errorf("window boxes: %w", err)
	}
	boxes, err := studio.ParseWindowBoxes(resp.Text)
	if err != nil {
		return fmt.Errorf("window boxes: %w", err)
	}
	if len(boxes) == 0 {
		return errNoGlass
	}
	p.gen.Windows = boxes
	p.diag.Gen.Windows = len(boxes)
	return nil
}

// composeGen turns the model's answer into the studio variant: exterior answers must
// be aligned (studio.MaxAlignment), interior answers must have changed a plausible
// share of the picture.
func (rn *run) composeGen(p *photo, gen image.Image, attempt int) (variant, error) {
	d := p.diag.Gen
	var out *image.RGBA
	if p.interior {
		img, changed, err := studio.InteriorCompose(p.orig, gen, p.gen.Windows)
		if err != nil {
			return variant{}, err
		}
		d.Changed = changed
		switch {
		case changed < studio.MinInteriorChange:
			return variant{}, fmt.Errorf("%w: the model changed %.1f%% of the glass area", errNoGlass, changed*100)
		}
		out = img
	} else {
		align := studio.Alignment(p.canvas, gen)
		d.Alignment = align
		if align < 0 || align > studio.MaxAlignment {
			return variant{}, fmt.Errorf("attempt %d: the model moved the car (alignment %.1f, limit %.0f)", attempt, align, studio.MaxAlignment)
		}
		composed, err := studio.ComposeGenerated(p.canvas, gen)
		if err != nil {
			return variant{}, err
		}
		keep := rn.dom.Studio.Generative.KeepPad
		if keep <= 0 {
			keep = studio.DefaultGenKeepPad
		}
		out = studio.CropCanvas(p.canvas, composed, keep)
	}
	full, err := studio.EncodeJPEG(out, studio.JPEGQuality)
	if err != nil {
		return variant{}, err
	}
	thumb, err := studio.EncodeJPEG(studio.ScaleToWidth(out, studio.ThumbWidth), studio.ThumbQuality)
	if err != nil {
		return variant{}, fmt.Errorf("thumbnail: %w", err)
	}
	b := out.Bounds()
	return variant{style: string(studio.StyleStudio), full: full, thumb: thumb, width: b.Dx(), height: b.Dy()}, nil
}

// errNoGlass: an interior answer that changed nothing (no window in view).
var errNoGlass = errors.New("no glass")

// widenCanvas rebuilds an exterior photo's canvas with a wider margin for the retry.
func (rn *run) widenCanvas(p *photo) error {
	if p.canvas == nil {
		return errors.New("no canvas")
	}
	c := p.canvas
	orig := c.Full.SubImage(c.Inner)
	alpha := c.Alpha.SubImage(c.Inner).(*image.Gray)
	// SubImage keeps the parent's bounds; BuildCanvas wants matching sizes only.
	pad := p.diag.Gen.Pad + 0.15
	nc, err := studio.BuildCanvas(orig, alpha, pad)
	if err != nil {
		return err
	}
	jpg, err := studio.SendJPEG(nc.Cutout)
	if err != nil {
		return err
	}
	os.Remove(p.gen.CanvasPath)
	p.gen.CanvasJPEG = jpg
	nb := nc.Cutout.Bounds()
	p.gen.Width, p.gen.Height = nb.Dx(), nb.Dy()
	if err := studio.WriteGenFile(rn.work, p.gen); err != nil {
		return err
	}
	p.canvas = nc
	p.diag.Gen.Pad = pad
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// runQA asks the model whether any part of the car is missing from the cut-out, retrying
// once on an unavailable model or an unreadable answer. Every call's usage is recorded
// as soon as it returns.
func (rn *run) runQA(ctx context.Context, p *photo) qaResult {
	start := time.Now()
	defer func() {
		p.diag.TimingsMS["qa"] = time.Since(start).Milliseconds()
		os.Remove(p.qa.OriginalPath)
		os.Remove(p.qa.CutoutPath)
	}()
	var lastErr error
	for range qaAttempts {
		req := studio.QARequest(p.qa)
		qctx, cancel := context.WithTimeout(ctx, rn.qaTimeout())
		p.diag.QA.Calls++
		resp, err := rn.Analyzer.Generate(qctx, req)
		cancel()
		if resp != nil {
			rn.recordUsage(ctx, req.Step, req.Model, resp.Usage)
		}
		if err != nil {
			if ctx.Err() != nil {
				return qaResult{p: p, err: ctx.Err()}
			}
			lastErr = err
			if errors.Is(err, analyzer.ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
				metrics.ModelErrors.WithLabelValues(req.Model, "unavailable").Inc()
				continue
			}
			metrics.ModelErrors.WithLabelValues(req.Model, "other").Inc()
			break
		}
		v, err := studio.ParseQA(resp.Text)
		if err != nil {
			metrics.ModelErrors.WithLabelValues(req.Model, "bad_output").Inc()
			lastErr = err
			continue
		}
		return qaResult{p: p, verdict: &v}
	}
	return qaResult{p: p, err: lastErr}
}

// complete decides the photo's outcome from its QA result, stores the variants of a
// ready photo and records the outcome, on a context detached from ctx (bounded by
// storeTimeout): the QA call is paid for, and a shutdown must not throw its answer
// away. An interrupted QA call leaves the photo without an outcome (a retry redoes it)
// and returns the context error.
func (rn *run) complete(ctx context.Context, res qaResult) error {
	p := res.p
	switch {
	case p.outcome != nil:
	case res.err != nil:
		if ctx.Err() != nil && (errors.Is(res.err, context.Canceled) || errors.Is(res.err, context.DeadlineExceeded)) {
			return ctx.Err()
		}
		rn.log.Warn("studio: QA check failed; studio version not offered", "image_id", p.im.ID, "err", res.err)
		p.diag.QA.Error = res.err.Error()
		p.outcome = &store.ImageStudio{Status: store.ImageStudioFailed, Reason: ReasonQAFailed}
	case res.verdict != nil:
		v := res.verdict
		q := p.diag.QA
		q.OK, q.MissingParts, q.MinorParts, q.BackgroundKept, q.LargeBackground, q.Notes = v.OK, v.MissingParts, v.MinorParts, v.BackgroundKept, v.LargeBackground, v.Notes
		if !v.OK {
			p.withhold(v.Reason())
		}
	}
	if p.outcome == nil && len(p.variants) == 0 {
		// An interior whose window treatment was not made (or an exterior with nothing
		// rendered): nothing to offer.
		reason := ReasonGenFailed
		if d := p.diag.Gen; d != nil && strings.Contains(d.Error, errNoGlass.Error()) {
			reason = ReasonGenNoGlass
		}
		p.withhold(reason)
	}
	if p.outcome == nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
		st, err := rn.storeVariants(sctx, p)
		cancel()
		if err != nil {
			return err
		}
		p.outcome = st
	}
	p.outcome.Diagnostics = encodeDiag(p.diag)
	return rn.save(ctx, p.im, *p.outcome)
}

// storeVariants uploads every rendered style and its thumbnail under the studio keys
// (overwriting what an interrupted attempt may have left) and returns the ready outcome.
func (rn *run) storeVariants(ctx context.Context, p *photo) (*store.ImageStudio, error) {
	st := &store.ImageStudio{Status: store.ImageStudioReady, Styles: make(map[store.Style]store.StudioVariant, len(p.variants))}
	d, id := rn.job.DomainKey, rn.job.ID
	for _, v := range p.variants {
		key, thumbKey := ImageKey(d, id, v.style, p.im.ID), ThumbKey(d, id, v.style, p.im.ID)
		if err := rn.Storage.Put(ctx, key, bytes.NewReader(v.full), "image/jpeg"); err != nil {
			return nil, fmt.Errorf("studiojob: store %s: %w", key, err)
		}
		if err := rn.Storage.Put(ctx, thumbKey, bytes.NewReader(v.thumb), "image/jpeg"); err != nil {
			return nil, fmt.Errorf("studiojob: store %s: %w", thumbKey, err)
		}
		st.Styles[store.Style(v.style)] = store.StudioVariant{
			Key: key, ThumbKey: thumbKey, Width: v.width, Height: v.height, Bytes: int64(len(v.full)),
		}
	}
	return st, nil
}

// encodeDiag marshals the diagnostics; values JSON cannot hold (NaN from a broken mask
// statistic) turn the diagnostics into an error note instead of failing the write.
func encodeDiag(d diagnostics) json.RawMessage {
	b, err := json.Marshal(d)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"error": "diagnostics: " + err.Error()})
	}
	return b
}

// finite maps NaN and ±Inf (which encoding/json rejects) to 0.
func finite(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}
