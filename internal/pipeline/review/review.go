package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/analyze"
	"video-to-image/internal/pipeline/score"
)

// SystemPrompt is the system instruction of the review call.
const SystemPrompt = "You review still frames for a listing and return structured JSON only."

// MaxIssuesRunes caps Assessment.Issues (model text is untrusted, see analyze.Truncate).
const MaxIssuesRunes = 200

// MinGapSec: two frames of one shot are only both shortlisted when they are at least
// this far apart in time (and visually distinct), so the model sees different frames.
const MinGapSec = 1.0

// releaseTimeout bounds the cleanup of files a provider uploaded for the call.
const releaseTimeout = 30 * time.Second

// frame is one shortlisted candidate.
type frame struct {
	cand score.Scored
	shot string // proposed view: the scene's shot ID, or domain.OtherShotID
	path string // downscaled copy sent to the model, rotated by rot (set by writeImages)
	key  string // the id the model sees, "img<n>" (set by assignKeys)
	rot  int    // clockwise rotation that makes the frame upright (from orientFrames)
	// preferred: shortlisted for a second the locate pass named (Input.Preferred).
	preferred bool
	// oriented: rot comes from the orientation call. Otherwise the frame is sent as
	// stored and the judgement call reports its rotation.
	oriented bool
}

// group is the shortlist of one shot, best first.
type group struct {
	shot   string
	frames []frame
}

// Run builds the shortlist (top CandidatesPerShot non-rejected, mutually distinct
// candidates per scene shot, at most MaxImages in total), asks which way is up for
// every frame (the orientation call, see orient.go), downscales the frames to
// MaxImageSide, turned upright, makes ONE judgement call on them (both calls are Step
// analyzer.StepReview) and parses the verdicts. No shortlist → no call, empty Output.
// On ErrUnavailable / bad JSON the returned Output carries Calls, RawText, the
// Rotations already known and the error; callers continue without the verdicts.
//
// A failed orientation call is retried once and is then not fatal: OrientationErr is
// set and the frames are judged as stored, with a prompt that says so and asks for the
// rotation itself.
//
// The caller decides whether the review runs (Domain.Review.Enabled); Run does not
// check it. The returned Output is never nil, and neither are Assessments and
// Rotations. The judgement call is not retried on bad output (cost control). Errors:
// analyzer and context errors unchanged (a per-call timeout wraps
// analyzer.ErrUnavailable), an error wrapping *analyze.BadOutputError
// (errors.Is(err, analyze.ErrBadOutput)) for an unparseable answer, or a local error
// when the frames could not be prepared.
func Run(ctx context.Context, a analyzer.Analyzer, in Input) (*Output, error) {
	out := &Output{Assessments: map[string]Assessment{}, Rotations: map[string]int{}}
	switch {
	case a == nil:
		return out, errors.New("review: nil analyzer")
	case in.Domain == nil:
		return out, errors.New("review: nil domain")
	}
	d := in.Domain

	groups := buildGroups(d, in.Candidates, in.Scores, candidatesPerShot(d.Review), in.Preferred)
	groups, dropped := capGroups(groups, MaxImages)
	if len(dropped) > 0 {
		slog.InfoContext(ctx, "review: shortlist capped", "job", in.JobID, "domain", d.Key,
			"max_images", MaxImages, "dropped", dropped)
	}
	frames := flatten(groups)
	if len(frames) == 0 {
		return out, nil
	}
	if n := countPreferred(frames); n > 0 {
		slog.InfoContext(ctx, "review: shortlist", "job", in.JobID, "domain", d.Key, "frames", len(frames), "at_located_seconds", n)
	}
	if in.WorkDir == "" {
		return out, errors.New("review: no work dir")
	}

	dir := filepath.Join(in.WorkDir, "review")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return out, fmt.Errorf("review: create image dir: %w", err)
	}
	// 1. Which way is up, per frame (see orient.go). The rotations go into out before
	// anything else can fail, so the caller can still straighten the frames without a
	// verdict. A failed orientation call is not fatal: the frames are judged as stored.
	rot, orientCalls, err := orientFrames(ctx, a, in, frames, dir)
	out.Calls = append(out.Calls, orientCalls...)
	out.Rotations = rot
	if err != nil {
		out.OrientationErr = err
		if ctx.Err() != nil {
			return out, err
		}
		slog.WarnContext(ctx, "review: orientation failed; judging frames as stored", "job", in.JobID, "err", err)
	}
	for i := range frames {
		frames[i].rot, frames[i].oriented = rot[frames[i].cand.ID]
	}

	// 2. The judgement, on upright copies (as stored where the orientation is unknown).
	frames, err = writeImages(ctx, dir, frames, maxImageSide(d.Review), in.JobID)
	if err != nil {
		return out, err
	}
	out.Sent = len(frames)
	assignKeys(frames)
	slog.DebugContext(ctx, "review: sending frames", "job", in.JobID, "domain", d.Key, "frames", keyMap(frames))

	req := buildRequest(in, frames)
	resp, err := analyze.Generate(ctx, a, req, analyze.CallTimeout)
	if err != nil {
		return out, err
	}
	out.Calls = append(out.Calls, resp.Usage)
	out.RawText = resp.Text
	releaseFiles(ctx, a, in.JobID, resp.FileHandles)

	assessments, ignored, err := parseAssessments(resp.Text, frames, d, minQuality(d.Review))
	if err != nil {
		return out, fmt.Errorf("review: %w", &analyze.BadOutputError{Raw: resp.Text, Err: limitHint(err, req, resp)})
	}
	out.Assessments = assessments

	usable, byBox := 0, 0
	for _, as := range assessments {
		if as.Usable {
			usable++
		}
		if as.VisibleByBox {
			byBox++
		}
	}
	attrs := []any{"job", in.JobID, "domain", d.Key, "sent", len(frames), "answered", len(assessments),
		"usable", usable, "visible_by_box", byBox, "ignored", ignored, "as_stored", len(frames) - countOriented(frames)}
	if ignored > 0 || len(assessments) < len(frames) {
		slog.WarnContext(ctx, "review: incomplete answer", attrs...)
	} else {
		slog.InfoContext(ctx, "review: done", attrs...)
	}
	return out, nil
}

func candidatesPerShot(r domain.Review) int {
	if r.CandidatesPerShot > 0 {
		return r.CandidatesPerShot
	}
	return DefaultCandidatesPerShot
}

func minQuality(r domain.Review) int {
	if r.MinQuality > 0 {
		return r.MinQuality
	}
	return DefaultMinQuality
}

func maxImageSide(r domain.Review) int {
	if r.MaxImageSide > 0 {
		return r.MaxImageSide
	}
	return DefaultMaxImageSide
}

// buildGroups returns one group per shot with eligible candidates, in domain priority
// order and domain.OtherShotID last. Eligible candidates are not rejected, have a
// scoring copy and a usable ID; a shot ID outside the domain counts as "other". Each
// group is ordered by scores (highest first, missing/NaN last; ties by timestamp, then
// ID); first the best candidate within PreferredWindowSec of each of the shot's
// preferred seconds is taken (mutually distinct by hash and MinGapSec), then the group
// is greedily filled to perShot candidates that are distinct from every pick (see
// distinct and shortlistGap).
func buildGroups(d *domain.Config, cands []score.Scored, scores map[string]float64, perShot int, preferred map[string][]float64) []group {
	byShot := make(map[string][]score.Scored)
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		if c.Rejected != "" || !validID(c.ID) || c.SmallPath == "" || seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		shot := c.ShotID
		if _, ok := d.ShotByID(shot); !ok {
			shot = domain.OtherShotID
		}
		byShot[shot] = append(byShot[shot], c)
	}

	scoreOf := func(id string) float64 {
		if s, ok := scores[id]; ok && !math.IsNaN(s) {
			return s
		}
		return math.Inf(-1)
	}

	order := d.ShotIDs()
	if _, ok := d.ShotByID(domain.OtherShotID); !ok {
		order = append(order, domain.OtherShotID)
	}
	var groups []group
	for _, shot := range order {
		cs := byShot[shot]
		if len(cs) == 0 {
			continue
		}
		delete(byShot, shot) // a shot listed twice in the domain is grouped once
		sort.SliceStable(cs, func(i, j int) bool {
			si, sj := scoreOf(cs[i].ID), scoreOf(cs[j].ID)
			switch {
			case si != sj:
				return si > sj
			case cs[i].TimestampSec != cs[j].TimestampSec:
				return cs[i].TimestampSec < cs[j].TimestampSec
			default:
				return cs[i].ID < cs[j].ID
			}
		})
		gap := shortlistGap(cs, perShot)
		g := group{shot: shot}
		fits := func(c score.Scored, gap float64) bool {
			for _, f := range g.frames {
				if f.cand.ID == c.ID || !distinct(f.cand, c, d.Selection.DedupeMaxHamming, gap) {
					return false
				}
			}
			return true
		}
		for _, sec := range preferred[shot] {
			if len(g.frames) >= perShot {
				break
			}
			for _, c := range cs { // best score first
				if math.Abs(c.TimestampSec-sec) <= PreferredWindowSec && fits(c, MinGapSec) {
					g.frames = append(g.frames, frame{cand: c, shot: shot, preferred: true})
					break
				}
			}
		}
		for _, c := range cs {
			if len(g.frames) >= perShot {
				break
			}
			if fits(c, gap) {
				g.frames = append(g.frames, frame{cand: c, shot: shot})
			}
		}
		groups = append(groups, g)
	}
	return groups
}

// distinct reports whether two frames differ enough to both be worth a look: their
// dHashes are further apart than maxHamming and they are at least gap seconds apart.
func distinct(a, b score.Scored, maxHamming int, gap float64) bool {
	return score.Hamming(a.DHash, b.DHash) > maxHamming && math.Abs(a.TimestampSec-b.TimestampSec) >= gap
}

// shortlistGap is the time two shortlisted frames of one shot must keep apart: at
// least MinGapSec, and at least the shot's time span divided by the shortlist size, so
// the picks spread over everything the shot covers instead of clustering where the
// frames happen to be sharpest. On a real walkaround the camera is still settling at
// the start of a scene (the car half in frame) and the usable moment comes later; the
// sharpest frames were all from the first seconds and every one was cut off.
func shortlistGap(cs []score.Scored, perShot int) float64 {
	if len(cs) == 0 || perShot <= 0 {
		return MinGapSec
	}
	// The span is summed per scene, so two short scenes far apart do not count the
	// silence between them.
	type span struct{ lo, hi float64 }
	spans := map[int]span{}
	for _, c := range cs {
		s, ok := spans[c.SceneIdx]
		if !ok {
			s = span{c.TimestampSec, c.TimestampSec}
		}
		spans[c.SceneIdx] = span{math.Min(s.lo, c.TimestampSec), math.Max(s.hi, c.TimestampSec)}
	}
	total := 0.0
	for _, s := range spans {
		total += s.hi - s.lo
	}
	return math.Max(MinGapSec, total/float64(perShot))
}

// capGroups bounds the shortlist to maxImages frames. It first trims the
// lowest-priority groups (last first, their worst frames first) down to one frame
// each, then drops whole groups from the end. It returns the kept groups and the IDs
// of the dropped frames.
func capGroups(groups []group, maxImages int) ([]group, []string) {
	total := 0
	for _, g := range groups {
		total += len(g.frames)
	}
	var dropped []string
	for i := len(groups) - 1; i >= 0 && total > maxImages; i-- {
		for len(groups[i].frames) > 1 && total > maxImages {
			last := groups[i].frames[len(groups[i].frames)-1]
			groups[i].frames = groups[i].frames[:len(groups[i].frames)-1]
			dropped = append(dropped, last.cand.ID)
			total--
		}
	}
	for total > maxImages && len(groups) > 0 {
		last := groups[len(groups)-1]
		groups = groups[:len(groups)-1]
		for _, f := range last.frames {
			dropped = append(dropped, f.cand.ID)
		}
		total -= len(last.frames)
	}
	return groups, dropped
}

func flatten(groups []group) []frame {
	var out []frame
	for _, g := range groups {
		out = append(out, g.frames...)
	}
	return out
}

// assignKeys numbers the frames sent: the model sees "img1".."imgN" instead of the
// candidate IDs. Candidate IDs are random per extraction, so a recorded answer (see
// internal/analyzer/recording) only replays onto a new run of the same video through
// IDs that depend on the shortlist alone; short IDs also cost fewer tokens.
func assignKeys(frames []frame) {
	for i := range frames {
		frames[i].key = "img" + strconv.Itoa(i+1)
	}
}

// keyMap renders "img1=cf_…" pairs for the debug log.
func keyMap(frames []frame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.key + "=" + f.cand.ID
	}
	return out
}

// validID rejects IDs that cannot safely name a file in the review directory.
func validID(id string) bool {
	return id != "" && id != "." && id != ".." && !strings.ContainsAny(id, `/\`) && !strings.ContainsRune(id, 0)
}

// viewIDs are the values the "view" field may take: the domain's shots in priority
// order, then domain.OtherShotID and ViewNone (without duplicates).
func viewIDs(d *domain.Config) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range append(d.ShotIDs(), domain.OtherShotID, ViewNone) {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Output budgets. The judgement answer takes about 90 tokens per frame on real runs, so
// a full shortlist of MaxImages frames comes close to the 4096-token default cap, and
// an answer cut off there is lost whole (paid for, no verdicts). Each call therefore
// gets the domain's cap raised to what its frame count needs, up to maxOutputCeiling.
// The budget only bounds a runaway answer: a normal one is billed for what it uses.
const (
	reviewBaseTokens     = 512
	reviewTokensPerFrame = 160
	orientBaseTokens     = 256
	orientTokensPerFrame = 48
	maxOutputCeiling     = 32768
)

// outputBudget is the domain's output cap, raised to need (at most maxOutputCeiling).
func outputBudget(domainCap int32, need int) int32 {
	return max(domainCap, int32(min(need, maxOutputCeiling)))
}

// limitHint marks a bad answer that used the whole output budget: it was most likely
// cut off at the limit (Gemini's MAX_TOKENS) rather than malformed. The analyzer does
// not report the finish reason, so this reads the usage.
func limitHint(err error, req *analyzer.Request, resp *analyzer.Response) error {
	if req.MaxOutputTokens > 0 && resp.Usage.OutputTokens+resp.Usage.ThinkingTokens >= int(req.MaxOutputTokens) {
		return fmt.Errorf("answer probably cut off at the %d-token output limit: %w", req.MaxOutputTokens, err)
	}
	return err
}

// countPreferred is the number of shortlisted frames taken for a preferred second.
func countPreferred(frames []frame) int {
	n := 0
	for _, f := range frames {
		if f.preferred {
			n++
		}
	}
	return n
}

// countOriented is the number of frames whose rotation came from the orientation call.
func countOriented(frames []frame) int {
	n := 0
	for _, f := range frames {
		if f.oriented {
			n++
		}
	}
	return n
}

// buildRequest assembles the call: the intro (instructions, views, fields), then per
// frame "Image <n> — id <key>, proposed view: <shot>" + the image, then the closing
// instruction. The id enum lists the keys of the frames sent (see assignKeys). When some
// frames are sent as stored (their orientation is unknown), the answer also carries a
// "rotation" per frame, and when only some are, those frames are marked
// "Image <n> (as recorded)".
func buildRequest(in Input, frames []frame) *analyzer.Request {
	d := in.Domain
	cacheKey := ""
	if in.CacheKey != "" {
		cacheKey = in.CacheKey + ":" + d.Key + ":" + analyzer.StepReview
	}
	oriented := countOriented(frames)
	mixed := oriented > 0 && oriented < len(frames)

	parts := make([]analyzer.Part, 0, 2*len(frames)+2)
	parts = append(parts, analyzer.Part{Text: intro(d, oriented, len(frames))})
	ids := make([]string, len(frames))
	for i, f := range frames {
		ids[i] = f.key
		mark := ""
		if mixed && !f.oriented {
			mark = " (as recorded)"
		}
		parts = append(parts,
			analyzer.Part{Text: fmt.Sprintf("Image %d%s — id %s, proposed view: %s", i+1, mark, f.key, f.shot)},
			analyzer.Part{FilePath: f.path, MIMEType: "image/jpeg"},
		)
	}
	withRotation := oriented < len(frames)
	parts = append(parts, analyzer.Part{Text: closing(len(frames), withRotation)})

	return &analyzer.Request{
		Step:            analyzer.StepReview,
		JobID:           in.JobID,
		DomainKey:       d.Key,
		CacheKey:        cacheKey,
		Model:           d.Model.Name,
		Temperature:     d.Model.Temperature,
		ThinkingBudget:  d.Model.ThinkingBudget,
		ThinkingLevel:   d.Model.ThinkingLevel,
		MaxOutputTokens: outputBudget(d.Model.EffectiveMaxOutputTokens(), reviewBaseTokens+reviewTokensPerFrame*len(frames)),
		SystemPrompt:    SystemPrompt,
		Parts:           parts,
		ResponseSchema:  responseSchema(d, ids, withRotation),
		ShotIDs:         d.ShotIDs(),
	}
}

// Orientation sentences of the intro: every frame was turned upright, none was (the
// orientation call failed), or only some were.
const (
	uprightNote  = "The images have already been turned upright."
	asStoredNote = "The images are shown as recorded and may be sideways or upside down: report the " +
		"rotation that makes each one upright, and judge the view and quality as if it were upright " +
		"(do not lower the quality for the orientation)."
	mixedNote = "Images marked \"as recorded\" are shown as recorded and may be sideways or upside down: " +
		"report the rotation that makes each one upright, and judge the view and quality as if it were " +
		"upright (do not lower the quality for the orientation). The other images have already been " +
		"turned upright (rotation 0)."
)

// intro is the first text part: the domain's review instructions, the list of views
// and the definition of every answer field. It depends only on d and on how many of
// the total frames were turned upright before sending (oriented); a "rotation" field is
// asked for unless all were.
func intro(d *domain.Config, oriented, total int) string {
	var b strings.Builder
	if s := cleanBlock(d.Review.Instructions); s != "" {
		b.WriteString(s)
		b.WriteString("\n\n")
	}
	b.WriteString("Views (the \"view\" field takes one of these ids):\n")
	for _, s := range d.Shots {
		fmt.Fprintf(&b, "- %s: %s — %s\n", strings.TrimSpace(s.ID), cleanInline(s.Label), cleanInline(s.Description))
	}
	fmt.Fprintf(&b, "- %s: a relevant view that fits no shot\n", domain.OtherShotID)
	fmt.Fprintf(&b, "- %s: not a usable view at all\n", ViewNone)
	b.WriteString("\n")
	switch {
	case oriented >= total:
		b.WriteString(uprightNote)
	case oriented == 0:
		b.WriteString(asStoredNote)
	default:
		b.WriteString(mixedNote)
	}
	b.WriteString(" Every image below is preceded by its id and the view it was proposed for. " +
		"For each image report:\n" +
		"- id: the image's id, exactly as given.\n")
	boxNote := ""
	if oriented < total {
		b.WriteString("- rotation: the clockwise rotation in degrees (0, 90, 180 or 270) that makes the image " +
			"upright; 0 if it already is.\n")
		// The box must stay in the frame as sent: parseAssessments maps it with the
		// rotation applied before sending, not with the reported one.
		boxNote = " (as shown, also when it is sideways)"
	}
	b.WriteString("- view: the view the image really shows, one of the view ids above; the proposed view is only a hint. " +
		"Use \"" + ViewNone + "\" if the image shows no usable view.\n" +
		"- subjectFullyVisible: true only if the whole subject of that view is inside the image; false if any " +
		"image edge cuts it off.\n" +
		"- quality: 1 to 5, the image as a listing photo (5 = publish as is, 3 = acceptable, 1 = unusable).\n" +
		"- box_2d: the subject's bounding box as [ymin, xmin, ymax, xmax], integers from 0 to 1000 relative to " +
		"the image as given" + boxNote + ".\n" +
		"- sameAs: the id of an EARLIER image in this list that shows essentially the same view of the same " +
		"part from nearly the same position (a near-duplicate a dealer would not publish twice), or \"none\".\n" +
		"- issues: at most " + strconv.Itoa(maxIssuesWords) + " words on what is wrong (blur, cut off, " +
		"obstructed, glare, tilted, dark), or \"\" if nothing is.")
	return b.String()
}

// maxIssuesWords is the length the prompt allows for "issues": the answer's size grows
// with the shortlist (see the output budgets), and a few words are all selection shows.
const maxIssuesWords = 8

// closing is the last text part.
func closing(n int, withRotation bool) string {
	rotation := ""
	if withRotation {
		rotation = `"rotation":0,`
	}
	return fmt.Sprintf("Return JSON only, in the form "+
		`{"images":[{"id":"...",`+rotation+`"view":"...","subjectFullyVisible":true,"quality":1,`+
		`"box_2d":[ymin,xmin,ymax,xmax],"sameAs":"none","issues":"..."}]}`+
		", with exactly one entry per image (%d in total), in the order the images were given.", n)
}

// responseSchema constrains id to the frames sent and view to the domain's views, so
// the model cannot invent either; quality, box_2d and rotation (only asked for when
// some frames were sent as stored) are bounded integers.
func responseSchema(d *domain.Config, ids []string, withRotation bool) *analyzer.Schema {
	order := []string{"id", "view", "subjectFullyVisible", "quality", "box_2d", "sameAs", "issues"}
	props := map[string]*analyzer.Schema{
		"id":   {Type: "string", Description: "the image's id, exactly as given", Enum: ids},
		"view": {Type: "string", Description: "the view the image really shows", Enum: viewIDs(d)},
		"subjectFullyVisible": {
			Type:        "boolean",
			Description: "true only if the whole subject is inside the image, not cut off by any edge",
		},
		"quality": {
			Type:        "integer",
			Description: "1 to 5, the image as a listing photo (5 = publish as is, 3 = acceptable, 1 = unusable)",
			Minimum:     ptr(1), Maximum: ptr(5),
		},
		"box_2d": {
			Type:        "array",
			Description: "the subject's bounding box: exactly 4 integers [ymin, xmin, ymax, xmax] from 0 to 1000 in the image",
			Items:       &analyzer.Schema{Type: "integer", Minimum: ptr(0), Maximum: ptr(1000)},
		},
		"sameAs": {
			Type:        "string",
			Description: `the id of an earlier image showing essentially the same view, or "none"`,
			Enum:        append(append([]string(nil), ids...), "none"),
		},
		"issues": {
			Type:        "string",
			Description: fmt.Sprintf(`at most %d words on what is wrong, or "" if nothing is`, maxIssuesWords),
		},
	}
	if withRotation {
		order = append([]string{"id", "rotation"}, order[1:]...)
		props["rotation"] = &analyzer.Schema{
			Type:        "integer",
			Description: "clockwise degrees (0, 90, 180 or 270) that make the image upright",
			Minimum:     ptr(0), Maximum: ptr(270),
		}
	}
	return &analyzer.Schema{
		Type: "object",
		Properties: map[string]*analyzer.Schema{
			"images": {
				Type:        "array",
				Description: "one entry per image, in the order the images were given",
				Items: &analyzer.Schema{
					Type:             "object",
					Properties:       props,
					PropertyOrdering: order,
					Required:         order,
				},
			},
		},
		PropertyOrdering: []string{"images"},
		Required:         []string{"images"},
	}
}

func ptr(v float64) *float64 { return &v }

// cleanInline reflows s to single spaces.
func cleanInline(s string) string { return strings.Join(strings.Fields(s), " ") }

// cleanBlock normalises a YAML block scalar like the analyze prompt does: CRLF → LF,
// trailing whitespace per line removed, leading/trailing blank lines dropped, the
// common indentation of non-blank lines stripped and runs of blank lines collapsed to a
// single blank line.
func cleanBlock(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	indent := -1
	for i, l := range lines {
		l = strings.TrimRight(l, " \t\r")
		lines[i] = l
		if l == "" {
			continue
		}
		if n := len(l) - len(strings.TrimLeft(l, " \t")); indent < 0 || n < indent {
			indent = n
		}
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		switch {
		case l != "":
			out = append(out, l[indent:])
		case len(out) > 0 && out[len(out)-1] != "":
			out = append(out, "")
		}
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// releaseFiles deletes any files the provider uploaded for this call. Nothing reuses
// them (an orientation retry sends its small grids again) and Output has no field to
// hand them to the runner.
func releaseFiles(ctx context.Context, a analyzer.Analyzer, jobID string, handles []string) {
	var todo []string
	seen := map[string]bool{}
	for _, h := range handles {
		if h != "" && !seen[h] {
			seen[h] = true
			todo = append(todo, h)
		}
	}
	if len(todo) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	for _, h := range todo {
		if err := a.ReleaseFile(rctx, h); err != nil {
			slog.WarnContext(ctx, "review: release model file", "job", jobID, "handle", h, "err", err)
		}
	}
}
