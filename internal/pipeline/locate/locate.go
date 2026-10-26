package locate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/media"
	"video-to-image/internal/pipeline/analyze"
)

// HalfWindowSec is half the scene a located second becomes: [sec−½, sec+½] at 1 fps
// (half the sampling interval in general), so consecutive located seconds merge into
// one range and extract's padding widens it further.
const halfWindowFactor = 0.5

// Output budget of the answer: ~30 tokens per moment plus a base, capped.
const (
	baseTokens       = 256
	tokensPerMoment  = 32
	maxOutputCeiling = 8192
)

// releaseTimeout bounds the cleanup of files a provider uploaded for the call.
const releaseTimeout = 30 * time.Second

// Run samples the preview at the domain's locate fps, lays the frames out on
// time-stamped contact sheets, makes ONE model call over them (Step
// analyzer.StepLocate) and parses the moments. The caller decides whether the pass
// runs (Domain.Locate.Enabled); Run does not check it. The returned Output is never
// nil. Errors: analyzer and context errors unchanged (a per-call timeout wraps
// analyzer.ErrUnavailable), an error wrapping *analyze.BadOutputError for an
// unparseable answer, or a local error when the sheets could not be prepared. Callers
// continue with the analyze scenes alone on any error.
func Run(ctx context.Context, a analyzer.Analyzer, in Input) (*Output, error) {
	out := &Output{}
	switch {
	case a == nil:
		return out, errors.New("locate: nil analyzer")
	case in.Domain == nil:
		return out, errors.New("locate: nil domain")
	case in.FFmpeg == nil:
		return out, errors.New("locate: nil ffmpeg")
	case in.PreviewPath == "" || in.WorkDir == "":
		return out, errors.New("locate: no preview or work dir")
	case !(in.DurationSec > 0) || math.IsInf(in.DurationSec, 0):
		return out, errors.New("locate: no duration")
	}
	d := in.Domain
	rate := fps(d.Locate)
	dir := filepath.Join(in.WorkDir, "locate")

	tiles, err := sweep(ctx, in.FFmpeg, in.PreviewPath, in.DurationSec, rate, dir)
	if err != nil {
		return out, err
	}
	out.Frames = len(tiles)
	if len(tiles) == 0 {
		return out, errors.New("locate: the preview yielded no frames")
	}
	sheets, err := writeSheets(dir, tiles, rate)
	if err != nil {
		return out, err
	}
	out.Sheets = len(sheets)
	unreadable := 0
	for _, s := range sheets {
		unreadable += s.unreadable
	}
	if unreadable > 0 {
		slog.WarnContext(ctx, "locate: unreadable sweep frames left dark", "job", in.JobID, "frames", unreadable)
	}

	req := buildRequest(in, sheets, rate)
	resp, err := analyze.Generate(ctx, a, req, analyze.CallTimeout)
	if err != nil {
		return out, err
	}
	out.Calls = append(out.Calls, resp.Usage)
	out.RawText = resp.Text
	releaseFiles(ctx, a, in.JobID, resp.FileHandles)

	moments, dropped, err := parseMoments(resp.Text, d, in.DurationSec, rate)
	if err != nil {
		return out, fmt.Errorf("locate: %w", &analyze.BadOutputError{Raw: resp.Text, Err: err})
	}
	out.Moments, out.Dropped = moments, dropped
	slog.InfoContext(ctx, "locate: done", "job", in.JobID, "domain", d.Key, "frames", len(tiles),
		"sheets", len(sheets), "moments", len(moments), "dropped", dropped, "seconds", secondsByShot(moments))
	return out, nil
}

// sweep extracts one frame every 1/rate seconds of the preview into dir/sweep/ and
// returns them with their times. The preview is small (480p) and short, so this is a
// cheap decode; ffmpeg numbers frame i at i/rate seconds.
func sweep(ctx context.Context, ff *media.FFmpeg, preview string, durationSec, rate float64, dir string) ([]tile, error) {
	sweepDir := filepath.Join(dir, "sweep")
	if err := os.MkdirAll(sweepDir, 0o755); err != nil {
		return nil, fmt.Errorf("locate: create sweep dir: %w", err)
	}
	res, err := ff.ExtractFrames(ctx, media.ExtractArgs{
		Input:        preview,
		StartSec:     0,
		DurationSec:  durationSec,
		FPS:          rate,
		FullPattern:  filepath.Join(sweepDir, "f_%04d.jpg"),
		SmallPattern: filepath.Join(sweepDir, "s_%04d.jpg"),
		SmallWidth:   CellSide,
		FullQScale:   3,
		SmallQScale:  3,
	})
	if err != nil {
		return nil, fmt.Errorf("locate: sweep: %w", err)
	}
	// The full copies are the preview's own size (≤ 480p): use them for the tiles,
	// the "small" copies only exist because ExtractFrames writes both.
	tiles := make([]tile, 0, len(res.Full))
	for i, p := range res.Full {
		tiles = append(tiles, tile{path: p, sec: float64(i) / rate})
	}
	return tiles, nil
}

// buildRequest assembles the call: the intro (subject, how to read the sheets, the
// domain's locate instructions, the views, the rule for a good moment), one text +
// image part per sheet, and the closing instruction with the JSON shape.
func buildRequest(in Input, sheets []sheet, rate float64) *analyzer.Request {
	d := in.Domain
	cacheKey := ""
	if in.CacheKey != "" {
		cacheKey = in.CacheKey + ":" + d.Key + ":" + analyzer.StepLocate
	}
	parts := make([]analyzer.Part, 0, 2*len(sheets)+2)
	parts = append(parts, analyzer.Part{Text: intro(d, rate, len(sheets))})
	for i, s := range sheets {
		parts = append(parts,
			analyzer.Part{Text: fmt.Sprintf("Sheet %d of %d: %s to %s", i+1, len(sheets), formatSec(s.first, rate), formatSec(s.last, rate))},
			analyzer.Part{FilePath: s.path, MIMEType: "image/jpeg"},
		)
	}
	perShot := maxPerShot(d.Locate)
	parts = append(parts, analyzer.Part{Text: closing(perShot)})
	need := baseTokens + tokensPerMoment*perShot*len(d.Shots)
	return &analyzer.Request{
		Step:            analyzer.StepLocate,
		JobID:           in.JobID,
		DomainKey:       d.Key,
		CacheKey:        cacheKey,
		Model:           d.Model.Name,
		Temperature:     d.Model.Temperature,
		ThinkingBudget:  d.Model.ThinkingBudget,
		ThinkingLevel:   d.Model.ThinkingLevel,
		MaxOutputTokens: max(d.Model.EffectiveMaxOutputTokens(), int32(min(need, maxOutputCeiling))),
		SystemPrompt:    SystemPrompt,
		Parts:           parts,
		ResponseSchema:  responseSchema(d),
		DurationSec:     in.DurationSec,
		ShotIDs:         d.ShotIDs(),
	}
}

func intro(d *domain.Config, rate float64, sheets int) string {
	var b strings.Builder
	subject := strings.TrimRight(strings.Join(strings.Fields(d.Gate.Subject), " "), ". ")
	fmt.Fprintf(&b, "The video shows %s.\n", subject)
	every := "one frame per second"
	if rate != 1 {
		every = fmt.Sprintf("%g frames per second", rate)
	}
	fmt.Fprintf(&b, "It is shown below as %d contact sheet(s). Every numbered square on a sheet is ONE frame of the "+
		"video (%s, in time order); the number printed in its top-left corner is that frame's time in seconds. "+
		"The square shows the same frame four times: as recorded (top-left), turned 90° clockwise (top-right), "+
		"180° (bottom-left) and 270° (bottom-right). The phone was sometimes held sideways, so exactly one of the "+
		"four copies is upright (ground at the bottom, sky or ceiling at the top, wheels below the body): judge "+
		"every frame by its upright copy and report which copy that is. Dark squares are frames that could not be "+
		"shown.\n", sheets, every)
	if s := cleanBlock(d.Locate.Instructions); s != "" {
		b.WriteString("\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	b.WriteString("\nViews (the \"shotId\" field takes one of these ids):\n")
	for _, s := range d.Shots {
		fmt.Fprintf(&b, "- %s: %s — %s\n", strings.TrimSpace(s.ID), strings.Join(strings.Fields(s.Label), " "),
			strings.Join(strings.Fields(s.Description), " "))
	}
	b.WriteString("\nFor every view that appears in the video, list EVERY second whose frame shows that view " +
		"recognisably, and say with wholeSubject whether the whole subject of the view is inside the frame (for an " +
		"exterior view: nothing of the vehicle cut off by any frame edge). A later step checks the full-resolution " +
		"frame, so when in doubt list the second with wholeSubject false rather than leaving it out; only skip " +
		"frames where a large part of the subject is clearly outside, the camera is between positions, or the " +
		"frame is blurred. Skip a view that is not shown at all; do not guess.")
	return b.String()
}

func closing(perShot int) string {
	return fmt.Sprintf("Return JSON only, in the form "+
		`{"moments":[{"sec":12,"shotId":"...","wholeSubject":true,"upright":"top-left","confidence":0.9}]}`+
		", ordered by sec, every qualifying second up to %d per view (not only the best one, and the ones with "+
		"wholeSubject true first when you must leave some out); an empty list if no view is shown.", perShot)
}

// responseSchema constrains shotId to the domain's shots; sec is a whole number of
// seconds as printed on the tiles (tenths when the sampling is finer).
func responseSchema(d *domain.Config) *analyzer.Schema {
	lo, hi := 0.0, 1.0
	zero := 0.0
	order := []string{"sec", "shotId", "wholeSubject", "upright", "confidence"}
	return &analyzer.Schema{
		Type: "object",
		Properties: map[string]*analyzer.Schema{
			"moments": {
				Type:        "array",
				Description: "the seconds at which a view is shown well, ordered by sec; empty if none",
				Items: &analyzer.Schema{
					Type: "object",
					Properties: map[string]*analyzer.Schema{
						"sec":          {Type: "number", Description: "the time printed on the tile, in seconds", Minimum: &zero},
						"shotId":       {Type: "string", Description: "the view the tile shows", Enum: d.ShotIDs()},
						"wholeSubject": {Type: "boolean", Description: "true if nothing of the subject is cut off by the frame edge"},
						"upright":      {Type: "string", Description: "the copy of the frame that is upright", Enum: cellNames()},
						"confidence":   {Type: "number", Description: "how sure you are, 0 to 1", Minimum: &lo, Maximum: &hi},
					},
					PropertyOrdering: order,
					Required:         order,
				},
			},
		},
		PropertyOrdering: []string{"moments"},
		Required:         []string{"moments"},
	}
}

// wireMoment decodes one answer entry; pointers tell a missing field from false/0.
type wireMoment struct {
	Sec          *float64 `json:"sec"`
	ShotID       string   `json:"shotId"`
	WholeSubject *bool    `json:"wholeSubject"`
	Upright      string   `json:"upright"`
	Confidence   *float64 `json:"confidence"`
}

// cellNames lists the quad positions the "upright" field may take.
func cellNames() []string {
	out := make([]string, len(quadCells))
	for i, c := range quadCells {
		out[i] = c.name
	}
	return out
}

// rotationOf maps an "upright" answer to the clockwise rotation of that copy; -1 for
// anything else.
func rotationOf(upright string) int {
	u := strings.ToLower(strings.TrimSpace(upright))
	for _, c := range quadCells {
		if c.name == u {
			return c.rotCW
		}
	}
	return -1
}

// parseMoments decodes the answer (fence-tolerant). A missing or non-array "moments"
// is invalid output. Entries are checked one by one and a bad one is dropped
// (counted), never fatal: no or non-finite sec, sec outside [0, duration], a shotId
// that is not a domain shot (case-insensitive), or a repeat of a (sec, shot) already
// kept (the first wins). wholeSubject missing counts as false; upright maps to
// RotateCW (unknown → -1). sec is snapped to the sampling grid (1/rate). Confidence is
// clamped to [0, 1] (missing → 0.5). The result is ordered by Sec, then shot priority.
func parseMoments(text string, d *domain.Config, durationSec, rate float64) ([]Moment, int, error) {
	payload := analyze.ExtractJSON(text)
	if payload == "" {
		return nil, 0, errors.New("empty response")
	}
	var w struct {
		Moments *[]json.RawMessage `json:"moments"`
	}
	if err := json.Unmarshal([]byte(payload), &w); err != nil {
		return nil, 0, fmt.Errorf("decode json: %w", err)
	}
	if w.Moments == nil {
		return nil, 0, errors.New("missing moments")
	}
	shots := make(map[string]string, len(d.Shots))
	prio := make(map[string]int, len(d.Shots))
	for i, s := range d.Shots {
		shots[strings.ToLower(strings.TrimSpace(s.ID))] = s.ID
		prio[s.ID] = i
	}
	if !(rate > 0) {
		rate = DefaultFPS
	}
	type key struct {
		sec  float64
		shot string
	}
	seen := map[key]bool{}
	var out []Moment
	dropped := 0
	for _, raw := range *w.Moments {
		var e wireMoment
		if err := json.Unmarshal(raw, &e); err != nil || e.Sec == nil || math.IsNaN(*e.Sec) || math.IsInf(*e.Sec, 0) {
			dropped++
			continue
		}
		sec := math.Round(*e.Sec*rate) / rate
		shot, ok := shots[strings.ToLower(strings.TrimSpace(e.ShotID))]
		if !ok || sec < 0 || sec > durationSec {
			dropped++
			continue
		}
		k := key{sec, shot}
		if seen[k] {
			dropped++
			continue
		}
		seen[k] = true
		conf := 0.5
		if e.Confidence != nil && !math.IsNaN(*e.Confidence) {
			conf = math.Max(0, math.Min(1, *e.Confidence))
		}
		out = append(out, Moment{Sec: sec, ShotID: shot, Confidence: conf, Whole: e.WholeSubject != nil && *e.WholeSubject,
			RotateCW: rotationOf(e.Upright)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Sec != out[j].Sec {
			return out[i].Sec < out[j].Sec
		}
		return prio[out[i].ShotID] < prio[out[j].ShotID]
	})
	return out, dropped, nil
}

// Scenes turns moments into analyze scenes: each second becomes the range
// [sec − h, sec + h] with h = half the sampling interval, clamped to the video, with
// the moment's confidence as relevance (× PartialRelevance when the subject was not
// seen whole) and a neutral "<Label> view" reason (the
// selected image shows the scene reason as its caption). Consecutive seconds of one
// shot therefore touch and analyze.MergeScenes joins them into one range. Idx is
// assigned in order; MergeScenes reassigns it.
func Scenes(moments []Moment, d *domain.Config, durationSec float64) []analyze.Scene {
	if d == nil || len(moments) == 0 {
		return nil
	}
	h := halfWindowFactor / fps(d.Locate)
	labels := make(map[string]string, len(d.Shots))
	for _, s := range d.Shots {
		labels[s.ID] = strings.Join(strings.Fields(s.Label), " ")
	}
	out := make([]analyze.Scene, 0, len(moments))
	for _, m := range moments {
		start := math.Max(0, m.Sec-h)
		end := math.Min(durationSec, m.Sec+h)
		if !(end > start) {
			continue
		}
		raw := formatSec(m.Sec, fps(d.Locate))
		rel := m.Confidence
		if !m.Whole {
			rel *= PartialRelevance
		}
		out = append(out, analyze.Scene{
			Idx:       len(out),
			StartSec:  start,
			EndSec:    end,
			ShotID:    m.ShotID,
			Relevance: rel,
			Reason:    labels[m.ShotID] + " view",
			RawStart:  "sheet " + raw,
			RawEnd:    "sheet " + raw,
		})
	}
	return out
}

// secondsByShot renders "shot: 6 7 8" pairs for the log.
func secondsByShot(ms []Moment) []string {
	byShot := map[string][]string{}
	var order []string
	for _, m := range ms {
		if _, ok := byShot[m.ShotID]; !ok {
			order = append(order, m.ShotID)
		}
		sec := formatSec(m.Sec, 1)
		if !m.Whole {
			sec += "?"
		}
		if m.RotateCW > 0 {
			sec += fmt.Sprintf("(r%d)", m.RotateCW)
		}
		byShot[m.ShotID] = append(byShot[m.ShotID], sec)
	}
	out := make([]string, 0, len(order))
	for _, s := range order {
		out = append(out, s+": "+strings.Join(byShot[s], " "))
	}
	return out
}

// releaseFiles deletes the provider files uploaded for the call (inline images leave
// none), on a detached context so a cancelled job still cleans up.
func releaseFiles(ctx context.Context, a analyzer.Analyzer, jobID string, handles []string) {
	seen := map[string]bool{}
	var todo []string
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
			slog.WarnContext(ctx, "locate: release file", "job", jobID, "handle", h, "err", err)
		}
	}
}

// cleanBlock normalises a YAML block scalar the way the analyze prompt does.
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
		if l == "" {
			if len(out) == 0 || out[len(out)-1] == "" {
				continue
			}
			out = append(out, "")
			continue
		}
		out = append(out, l[indent:])
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}
