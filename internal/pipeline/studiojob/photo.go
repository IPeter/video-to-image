package studiojob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"os"
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
	TimingsMS map[string]int64     `json:"timingsMs,omitempty"`
	Error     string               `json:"error,omitempty"`
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
		v, ms, err := render(prep, style)
		if err != nil {
			return p.fail("render "+style, err), nil
		}
		p.variants = append(p.variants, v)
		p.diag.Styles[style] = styleDiag{ComposeMS: ms, Bytes: len(v.full)}
	}
	p.diag.TimingsMS["render"] = time.Since(start).Milliseconds()

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

// startQA runs the QA check of p in the background (see runQA).
func (rn *run) startQA(ctx context.Context, p *photo) *qaCall {
	c := &qaCall{done: make(chan qaResult, 1)}
	go func() { c.done <- rn.runQA(ctx, p) }()
	return c
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
			rn.recordUsage(ctx, req.Model, resp.Usage)
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
		q.OK, q.MissingParts, q.BackgroundKept, q.LargeBackground, q.Notes = v.OK, v.MissingParts, v.BackgroundKept, v.LargeBackground, v.Notes
		if !v.OK {
			p.withhold(v.Reason())
		}
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
