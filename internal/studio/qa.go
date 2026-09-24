package studio

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"video-to-image/internal/analyzer"
)

// The QA check (spike gate G11): Gemini compares the original with the cut-out on flat
// magenta and lists car parts the cut-out lost. On the spike's fixtures it caught the
// lost roof antennas and a half-lost mirror and passed every good cut-out, but it missed
// one lost boot-lid antenna: it is a safety net, not a proof.
const (
	DefaultQAModel    = "gemini-3.8-flash"
	DefaultQALongSide = 1024 // px, both images sent to the model
	QAJPEGQuality     = 90
	qaMaxOutputTokens = 512 // the answer is two short lists and a sentence (~50-150 tokens)
	qaMaxItems        = 12
	qaMaxItemRunes    = 120
	qaMaxNotesRunes   = 300
	qaLargePrefix     = "large:"
	qaCacheKeySuffix  = ":studio-qa"
	qaKeyBackground   = "backgroundKept"
)

// magenta is the flat background of the QA cut-out.
var magenta = [3]float64{255, 0, 255}

// QAPreview renders the cut-out on flat magenta (#FF00FF), long side ≤ longSide px,
// the image the QA check compares with the original.
//
// alpha (same size as orig; nil or another size → nil) gets the same clean-up as
// Prepare, so the check sees exactly the cut-out that is published. longSide ≤ 0 →
// DefaultQALongSide. The composite is a·orig + (1−a)·magenta, averaged down with a box
// filter.
func QAPreview(orig image.Image, alpha *image.Gray, longSide int) image.Image {
	if orig == nil || alpha == nil {
		return nil
	}
	b := orig.Bounds()
	w, h := b.Dx(), b.Dy()
	if ab := alpha.Bounds(); ab.Dx() != w || ab.Dy() != h || w == 0 || h == 0 {
		return nil
	}
	if longSide <= 0 {
		longSide = DefaultQALongSide
	}
	a := grayPlane(alpha)
	if defaultOptions.cleanup.enabled {
		cleanupAlpha(a, w, h, defaultOptions.cleanup)
	}
	read := rowReader(orig)
	dw, dh := fitLongSide(w, h, longSide)
	return areaResize(w, h, dw, dh, func(y int, dst []uint8) {
		read(y, dst)
		for x, av := range a[y*w : (y+1)*w] {
			switch av {
			case 255:
			case 0:
				dst[x*3], dst[x*3+1], dst[x*3+2] = 255, 0, 255
			default:
				af := float64(av) / 255
				for c := range 3 {
					dst[x*3+c] = to8(af*float64(dst[x*3+c]) + (1-af)*magenta[c])
				}
			}
		}
	})
}

// QAImages encodes the two images of the QA check: the original and its cut-out on
// magenta (QAPreview, with the same clean-up as Render), both at most DefaultQALongSide
// px on the long side, at QAJPEGQuality.
func QAImages(orig image.Image, alpha *image.Gray) (origJPEG, cutoutJPEG []byte, err error) {
	if orig == nil || alpha == nil {
		return nil, nil, errors.New("studio: nil image or mask")
	}
	cut := QAPreview(orig, alpha, DefaultQALongSide)
	if cut == nil {
		return nil, nil, fmt.Errorf("%w: mask %v, image %v", ErrMaskSize, alpha.Bounds().Size(), orig.Bounds().Size())
	}
	if origJPEG, err = EncodeJPEG(Downscale(orig, DefaultQALongSide), QAJPEGQuality); err != nil {
		return nil, nil, err
	}
	if cutoutJPEG, err = EncodeJPEG(cut, QAJPEGQuality); err != nil {
		return nil, nil, err
	}
	return origJPEG, cutoutJPEG, nil
}

// QAParams are the inputs of one QA check.
type QAParams struct {
	JobID, DomainKey, ImageID string
	Model                     string // "" → gemini-3.8-flash
	View                      string // shot ID, e.g. front_three_quarter
	OriginalJPEG, CutoutJPEG  []byte // both ≤ 1024 px long side
	// OriginalPath and CutoutPath are local files holding OriginalJPEG and CutoutJPEG:
	// analyzer.Part carries files, not bytes. WriteQAFiles writes them. The bytes also
	// give the request its CacheKey (recording/replay).
	OriginalPath, CutoutPath string
	// SourceName is the upload's file name, a hint for the fake analyzer only (a name
	// containing "missingpart" makes its QA answer report a missing part); real
	// providers never see it.
	SourceName string
}

// WriteQAFiles writes p.OriginalJPEG and p.CutoutJPEG to dir (created if needed) as
// <ImageID>_qa_original.jpg and <ImageID>_qa_cutout.jpg and sets the paths.
func WriteQAFiles(dir string, p *QAParams) error {
	if len(p.OriginalJPEG) == 0 || len(p.CutoutJPEG) == 0 {
		return errors.New("studio: QA images are empty")
	}
	id := p.ImageID
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return fmt.Errorf("studio: invalid image ID %q for the QA files", id)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("studio: QA dir: %w", err)
	}
	op := filepath.Join(dir, id+"_qa_original.jpg")
	cp := filepath.Join(dir, id+"_qa_cutout.jpg")
	if err := os.WriteFile(op, p.OriginalJPEG, 0o600); err != nil {
		return fmt.Errorf("studio: write QA original: %w", err)
	}
	if err := os.WriteFile(cp, p.CutoutJPEG, 0o600); err != nil {
		return fmt.Errorf("studio: write QA cut-out: %w", err)
	}
	p.OriginalPath, p.CutoutPath = op, cp
	return nil
}

// qaPrompt is adapted from the spike's check (rembg/gemini_check.py), which rejected
// the cut-outs that lost an antenna or a mirror and passed the good ones.
const qaPrompt = `Image 1 is an original photo of a car for a sales listing (view: %s). Image 2 is the same photo after an automatic cut-out: every pixel judged "not car" was replaced by flat magenta (#FF00FF); car pixels are unchanged.
The car will be put on a studio backdrop, and the buyer must see the car exactly as it is, so the cut-out must not remove any part of THIS car, including dirt, damage, rust, dents and missing or broken trim.
Compare the two images carefully, part by part: mirrors, antennas, roof rails and bars, spoiler, wipers, wheels and tyres, bumpers and bumper corners, tow hook or tow bar, exhaust, open doors or tailgate and their gas struts, trim, side skirts and sills, damaged or missing trim, glass.
1. missingParts: parts or areas of this car that are visible in image 1 but missing, cut off or turned (partly) magenta or see-through in image 2. Magenta seen THROUGH the car's windows where image 1 shows the scene behind the glass is fine, do not list it. Do not list parts that the photo frame cuts off in both images.
2. backgroundKept: things that are NOT part of this car (other vehicles, people, ground, walls, poles, shadows on the ground) that still remain in image 2 outside the car's outline. Start an item with "LARGE:" when it covers a big area (about a tenth of the car or more, such as part of another vehicle, a person or a patch of ground).
3. notes: at most one short sentence, or empty.
Be strict about missing parts: a missing thin antenna or a missing mirror counts. Name each item in at most six words. Answer with JSON only.`

// qaSchema is the structured-output schema of the answer.
func qaSchema() *analyzer.Schema {
	list := func(desc string) *analyzer.Schema {
		return &analyzer.Schema{Type: "array", Description: desc, Items: &analyzer.Schema{Type: "string"}}
	}
	return &analyzer.Schema{
		Type: "object",
		Properties: map[string]*analyzer.Schema{
			"missingParts":  list("Car parts visible in image 1 but missing in image 2; empty if none."),
			qaKeyBackground: list(`Leftover background outside the car in image 2; prefix big pieces with "LARGE:".`),
			"notes":         {Type: "string", Description: "At most one short sentence."},
		},
		Required:         []string{"missingParts", qaKeyBackground, "notes"},
		PropertyOrdering: []string{"missingParts", qaKeyBackground, "notes"},
	}
}

// viewLabel turns a shot ID into words ("front_three_quarter" → "front three quarter").
func viewLabel(v string) string {
	v = strings.TrimSpace(strings.ReplaceAll(v, "_", " "))
	if v == "" {
		return "exterior"
	}
	return v
}

// QARequest builds the model call that asks whether any part of the car is missing
// from the cut-out (or background was kept). Step is analyzer.StepStudio.
//
// Temperature 0, no thinking (gemini-3.8-flash: thinkingBudget 0, it rejects the
// "minimal" level; a flash-lite model: thinkingLevel minimal, it rejects budget 0), a
// JSON schema {missingParts, backgroundKept, notes} and a small output cap. The images
// are sent as files (OriginalPath, CutoutPath). CacheKey is
// sha256(original ‖ cut-out) + ":" + DomainKey + ":studio-qa" when the bytes are set.
func QARequest(p QAParams) *analyzer.Request {
	model := strings.TrimSpace(p.Model)
	if model == "" {
		model = DefaultQAModel
	}
	req := &analyzer.Request{
		Step:            analyzer.StepStudio,
		JobID:           p.JobID,
		DomainKey:       p.DomainKey,
		Model:           model,
		Temperature:     0,
		MaxOutputTokens: qaMaxOutputTokens,
		Parts: []analyzer.Part{
			{Text: "Image 1 (original):"},
			{FilePath: p.OriginalPath, MIMEType: "image/jpeg"},
			{Text: "Image 2 (cut-out on magenta):"},
			{FilePath: p.CutoutPath, MIMEType: "image/jpeg"},
			{Text: fmt.Sprintf(qaPrompt, viewLabel(p.View))},
		},
		ResponseSchema: qaSchema(),
		SourceName:     p.SourceName,
	}
	if strings.Contains(strings.ToLower(model), "lite") {
		req.ThinkingLevel = "minimal"
	} else {
		zero := int32(0)
		req.ThinkingBudget = &zero
	}
	if len(p.OriginalJPEG) > 0 && len(p.CutoutJPEG) > 0 {
		h := sha256.New()
		h.Write(p.OriginalJPEG)
		h.Write(p.CutoutJPEG)
		req.CacheKey = hex.EncodeToString(h.Sum(nil)) + ":" + p.DomainKey + qaCacheKeySuffix
	}
	return req
}

// QAVerdict is the parsed QA answer. OK is false when MissingParts is not empty.
//
// Background kept alone is reported but not fatal: it does not misrepresent the car,
// and the mask gates already bound leftovers that can be measured (mask outside the
// box ≤ 3 %, no second component). The exception is a piece the model marks as large
// (a "LARGE:" item: part of another vehicle, a person, a patch of ground), which
// withholds the studio version too (LargeBackground).
type QAVerdict struct {
	OK              bool
	MissingParts    []string
	BackgroundKept  []string // without the "LARGE:" prefix
	LargeBackground bool     // some kept background is large
	Notes           string
}

// ErrQABadOutput means the QA answer could not be read; treat the cut-out as unchecked.
var ErrQABadOutput = errors.New("studio: unreadable QA answer")

// ParseQA decodes the QA answer (fence-tolerant JSON).
//
// Code fences and text around the JSON object are ignored. missingParts must be
// present and an array (an answer without it is not "nothing missing"); items are
// trimmed, empty ones and "none"-like placeholders dropped, and at most 12 items of 120
// characters are kept (notes: 300).
func ParseQA(text string) (QAVerdict, error) {
	obj, ok := jsonObject(text)
	if !ok {
		return QAVerdict{}, fmt.Errorf("%w: no JSON object in %q", ErrQABadOutput, clip(text, 120))
	}
	var raw struct {
		MissingParts   *[]string `json:"missingParts"`
		BackgroundKept []string  `json:"backgroundKept"`
		Notes          string    `json:"notes"`
	}
	if err := json.Unmarshal([]byte(obj), &raw); err != nil {
		return QAVerdict{}, fmt.Errorf("%w: %w", ErrQABadOutput, err)
	}
	if raw.MissingParts == nil {
		return QAVerdict{}, fmt.Errorf("%w: no missingParts list", ErrQABadOutput)
	}
	v := QAVerdict{
		MissingParts:   cleanItems(*raw.MissingParts),
		BackgroundKept: []string{},
		Notes:          clip(strings.TrimSpace(raw.Notes), qaMaxNotesRunes),
	}
	for _, it := range cleanItems(raw.BackgroundKept) {
		if rest, ok := cutPrefixFold(it, qaLargePrefix); ok {
			v.LargeBackground = true
			if it = strings.TrimSpace(rest); it == "" {
				it = "large leftover background"
			}
		}
		v.BackgroundKept = append(v.BackgroundKept, it)
	}
	v.OK = len(v.MissingParts) == 0 && !v.LargeBackground
	return v, nil
}

// Reason is the verdict for people: empty when OK.
func (v QAVerdict) Reason() string {
	switch {
	case len(v.MissingParts) > 0:
		return "The cut-out may have removed part of the car: " + strings.Join(v.MissingParts, ", ") + "."
	case v.LargeBackground:
		return "The cut-out kept a large piece of the old background, so it was not used."
	}
	return ""
}

// jsonObject returns the outermost {...} of text (after removing Markdown fences).
func jsonObject(text string) (string, bool) {
	s := strings.TrimSpace(text)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```")
		if i := strings.IndexByte(s, '\n'); i >= 0 && !strings.Contains(s[:i], "{") {
			s = s[i+1:] // the fence's language tag
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}')
	if i < 0 || j < i {
		return "", false
	}
	return s[i : j+1], true
}

// placeholders are "nothing" answers some models put into a list instead of leaving it
// empty.
var placeholders = map[string]bool{
	"none": true, "n/a": true, "na": true, "nothing": true, "no missing parts": true,
	"no parts missing": true, "nothing missing": true, "no leftover background": true, "-": true,
}

func cleanItems(in []string) []string {
	out := []string{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || placeholders[strings.ToLower(strings.TrimRight(s, "."))] {
			continue
		}
		if len(out) == qaMaxItems {
			break
		}
		out = append(out, clip(s, qaMaxItemRunes))
	}
	return out
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix) {
		return s[len(prefix):], true
	}
	return s, false
}

// clip shortens s to at most n runes.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}
