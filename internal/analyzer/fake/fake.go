// Package fake is a deterministic, free analyzer.Analyzer for tests, e2e runs and local
// UI work (V2I_ANALYZER=fake). It never touches the network: for the analyze step it
// returns either a canned JSON file verbatim or synthetic scenes spread evenly over the
// video, one per shot ID; for the review step it approves every frame it was shown
// (upright, the proposed view, fully visible, quality 4, a centred subject box); for the
// studio QA check it finds nothing missing and no background kept; for the legacy
// re-rank step it returns no picks.
//
// A request whose SourceName contains "reject" (case-insensitive) fails the domain gate,
// so tests and demos can exercise the rejection path deterministically; one containing
// "missingpart" makes the studio QA report a missing part (the studio version is then
// withheld).
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"regexp"
	"strings"
	"time"

	"video-to-image/internal/analyzer"
)

// Name is what Analyzer.Name reports.
const Name = "fake"

// MaxScenes is the most synthetic scenes the fake returns for one video.
const MaxScenes = 8

// DefaultDurationSec is assumed when a request carries no usable video duration.
const DefaultDurationSec = 10.0

// Options configures New.
type Options struct {
	// ResponseFile, when set, is returned verbatim for every analyze call instead of the
	// synthetic answer. It is read once, by New.
	ResponseFile string
	// Delay is the simulated model latency (context-aware) and the reported Usage.Latency.
	Delay  time.Duration
	Logger *slog.Logger
}

// Analyzer is the fake analyzer. It is safe for concurrent use.
type Analyzer struct {
	canned []byte // contents of Options.ResponseFile; nil → synthesise
	delay  time.Duration
	log    *slog.Logger
}

var _ analyzer.Analyzer = (*Analyzer)(nil)

// New returns a fake analyzer. It fails when ResponseFile is set but cannot be read.
// ModelPrefix is prepended to the requested model in Usage.Model, so usage_events rows
// from the fake stay attributable and internal/pricing prices them at $0 (the fake is
// free and must not count towards DAILY_COST_CEILING_USD).
const ModelPrefix = "fake:"

func New(opts Options) (*Analyzer, error) {
	a := &Analyzer{delay: max(0, opts.Delay), log: opts.Logger}
	if a.log == nil {
		a.log = slog.Default()
	}
	if opts.ResponseFile != "" {
		b, err := os.ReadFile(opts.ResponseFile)
		if err != nil {
			return nil, fmt.Errorf("fake: read response file: %w", err)
		}
		a.canned = b
	}
	return a, nil
}

// Name returns "fake".
func (a *Analyzer) Name() string { return Name }

// ReleaseFile is a no-op: the fake uploads nothing.
func (a *Analyzer) ReleaseFile(context.Context, string) error { return nil }

// Generate returns the canned or synthetic answer for req.Step after sleeping Delay.
// Unknown steps are an error. Context errors are returned as-is.
func (a *Analyzer) Generate(ctx context.Context, req *analyzer.Request) (*analyzer.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("fake: nil request")
	}

	duration := req.DurationSec
	if !(duration > 0) || math.IsInf(duration, 0) { // also catches NaN
		duration = DefaultDurationSec
	}

	var text string
	usage := analyzer.Usage{
		Model:        ModelPrefix + req.Model,
		InputTokens:  int(duration*100) + 500,
		OutputTokens: 300,
		Latency:      a.delay,
	}
	switch req.Step {
	case analyzer.StepAnalyze:
		if a.canned != nil {
			text = string(a.canned)
		} else {
			b, err := json.Marshal(synthesizeAnalyze(req, duration))
			if err != nil {
				return nil, fmt.Errorf("fake: encode analyze response: %w", err)
			}
			text = string(b)
		}
	case analyzer.StepReview:
		if ids, ok := orientIDs(req.ResponseSchema); ok {
			// The review's orientation call: every frame is upright as stored.
			type entry struct {
				ID      string `json:"id"`
				Upright string `json:"upright"`
			}
			out := struct {
				Images []entry `json:"images"`
			}{Images: []entry{}}
			for _, id := range ids {
				out.Images = append(out.Images, entry{ID: id, Upright: "top-left"})
			}
			b, err := json.Marshal(out)
			if err != nil {
				return nil, fmt.Errorf("fake: encode orientation response: %w", err)
			}
			text = string(b)
			usage.InputTokens = ReviewTokensPerImage * len(ids)
			usage.OutputTokens = 10 * len(ids)
			break
		}
		resp := synthesizeReview(req)
		b, err := json.Marshal(resp)
		if err != nil {
			return nil, fmt.Errorf("fake: encode review response: %w", err)
		}
		text = string(b)
		usage.InputTokens = ReviewBaseTokens + ReviewTokensPerImage*len(resp.Images)
		usage.OutputTokens = ReviewOutputTokensPerImage * len(resp.Images)
	case analyzer.StepRerank:
		text = `{"picks":[]}`
	case analyzer.StepStudio:
		b, err := json.Marshal(synthesizeStudioQA(req))
		if err != nil {
			return nil, fmt.Errorf("fake: encode studio QA response: %w", err)
		}
		text = string(b)
		usage.InputTokens = StudioQAInputTokens
		usage.OutputTokens = StudioQAOutputTokens
	default:
		return nil, fmt.Errorf("fake: unknown step %q", req.Step)
	}

	if a.delay > 0 {
		t := time.NewTimer(a.delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}

	a.log.DebugContext(ctx, "fake analyzer call",
		"job_id", req.JobID, "domain", req.DomainKey, "step", req.Step, "bytes", len(text))

	return &analyzer.Response{
		Text:        text,
		Usage:       usage,
		FileHandles: make([]string, len(req.Parts)),
	}, nil
}

// The types below mirror the JSON shape of internal/pipeline/analyze.ModelResponse
// (plan/03-pipeline.md "Response schema"). They are duplicated rather than imported so
// the fake does not depend on the pipeline package.

type gate struct {
	Passed          bool    `json:"passed"`
	Confidence      float64 `json:"confidence"`
	Reason          string  `json:"reason"`
	DetectedSubject string  `json:"detectedSubject"`
}

type scene struct {
	Start     string  `json:"start"`
	End       string  `json:"end"`
	ShotID    string  `json:"shotId"`
	Reason    string  `json:"reason"`
	Relevance float64 `json:"relevance"`
}

type analyzeResponse struct {
	Gate         gate    `json:"gate"`
	VideoSummary string  `json:"videoSummary"`
	Scenes       []scene `json:"scenes"`
}

// synthesizeAnalyze builds the gate + scenes answer. Scenes are spread over the video:
// with n = min(len(shots), MaxScenes) segments of length seg, scene i starts 20% into
// segment i and lasts max(0.6 s, 60% of seg), clamped to the video duration.
func synthesizeAnalyze(req *analyzer.Request, duration float64) analyzeResponse {
	if strings.Contains(strings.ToLower(req.SourceName), "reject") {
		return analyzeResponse{
			Gate: gate{
				Passed:          false,
				Confidence:      0.9,
				Reason:          `Fake analyzer: rejected because the file name contains "reject".`,
				DetectedSubject: "not a car",
			},
			VideoSummary: "Synthetic rejected video (fake analyzer).",
			Scenes:       []scene{},
		}
	}

	shots := req.ShotIDs
	if len(shots) == 0 {
		shots = []string{"other"}
	}
	n := min(len(shots), MaxScenes)
	seg := duration / float64(n)
	scenes := make([]scene, 0, n)
	for i := 0; i < n; i++ {
		start := float64(i)*seg + 0.2*seg
		end := math.Min(start+math.Max(0.6, 0.6*seg), duration)
		scenes = append(scenes, scene{
			Start:     FormatTimestamp(start),
			End:       FormatTimestamp(end),
			ShotID:    shots[i],
			Reason:    fmt.Sprintf("Fake analyzer: synthetic %s moment.", shots[i]),
			Relevance: math.Round((0.95-0.05*float64(i))*100) / 100,
		})
	}
	return analyzeResponse{
		Gate: gate{
			Passed:          true,
			Confidence:      0.95,
			Reason:          "Fake analyzer: accepted.",
			DetectedSubject: "car",
		},
		VideoSummary: "Synthetic walkaround (fake analyzer).",
		Scenes:       scenes,
	}
}

// FormatTimestamp renders seconds as "MM:SS.ss" (zero-padded, two decimals), e.g.
// 4.25 → "00:04.25" and 125.5 → "02:05.50". Negative input is treated as 0. Minutes are
// not wrapped into hours, so 6000 s → "100:00.00".
func FormatTimestamp(sec float64) string {
	if !(sec > 0) {
		sec = 0
	}
	cs := int64(math.Round(sec * 100)) // round once, so 59.999 → "01:00.00", not "00:60.00"
	m := cs / 6000
	rem := cs % 6000
	return fmt.Sprintf("%02d:%02d.%02d", m, rem/100, rem%100)
}

// Synthetic review usage: a plausible order of magnitude per image sent, so the result
// page shows non-zero tokens in fake runs (the fake is still priced at $0).
const (
	ReviewBaseTokens           = 600
	ReviewTokensPerImage       = 258
	ReviewOutputTokensPerImage = 50
)

// ReviewBox is the box_2d ([ymin, xmin, ymax, xmax], 0-1000) the fake reports for every
// reviewed frame: a centred subject with room around it.
var ReviewBox = [4]int{150, 100, 850, 900}

// The types below mirror the JSON shape internal/pipeline/review parses.

type reviewImage struct {
	ID                  string `json:"id"`
	Rotation            int    `json:"rotation"`
	View                string `json:"view"`
	SubjectFullyVisible bool   `json:"subjectFullyVisible"`
	Quality             int    `json:"quality"`
	Box2D               [4]int `json:"box_2d"`
	Issues              string `json:"issues"`
}

type reviewResponse struct {
	Images []reviewImage `json:"images"`
}

// proposedViewRe finds "id <id>, proposed view: <view>" in a review text part.
var proposedViewRe = regexp.MustCompile(`\bid (\S+), proposed view: (\S+)`)

// synthesizeReview approves every frame of the request. The frame IDs come from the
// response schema (images.items.properties.id.Enum, in the order sent); the proposed
// view of each comes from its "Image <n> — id <id>, proposed view: <view>" text part
// ("other" when absent).
func synthesizeReview(req *analyzer.Request) reviewResponse {
	views := map[string]string{}
	for _, p := range req.Parts {
		if m := proposedViewRe.FindStringSubmatch(p.Text); m != nil {
			if _, ok := views[m[1]]; !ok {
				views[m[1]] = m[2]
			}
		}
	}
	resp := reviewResponse{Images: []reviewImage{}}
	for _, id := range reviewIDs(req.ResponseSchema) {
		view, ok := views[id]
		if !ok {
			view = "other"
		}
		resp.Images = append(resp.Images, reviewImage{
			ID: id, View: view, SubjectFullyVisible: true, Quality: 4, Box2D: ReviewBox,
		})
	}
	return resp
}

// reviewIDs returns images.items.properties.id.Enum of the review schema, or nil.
func reviewIDs(s *analyzer.Schema) []string {
	if s == nil {
		return nil
	}
	images := s.Properties["images"]
	if images == nil || images.Items == nil {
		return nil
	}
	id := images.Items.Properties["id"]
	if id == nil {
		return nil
	}
	return id.Enum
}

// Synthetic studio QA usage: two ~1024 px images plus the prompt, and a short answer
// (the fake is still priced at $0).
const (
	StudioQAInputTokens  = 2*ReviewTokensPerImage + 450
	StudioQAOutputTokens = 20
)

// StudioMissingPart is what the fake's studio QA reports as missing when the source
// name asks for it (see synthesizeStudioQA).
const StudioMissingPart = "roof antenna"

// studioQAResponse mirrors the JSON shape internal/studio.ParseQA reads.
type studioQAResponse struct {
	MissingParts   []string `json:"missingParts"`
	BackgroundKept []string `json:"backgroundKept"`
	Notes          string   `json:"notes"`
}

// synthesizeStudioQA answers the studio QA check: nothing missing, nothing kept. A
// request whose SourceName contains "missingpart" (case-insensitive) reports
// StudioMissingPart instead, so tests can exercise the withheld path.
func synthesizeStudioQA(req *analyzer.Request) studioQAResponse {
	resp := studioQAResponse{MissingParts: []string{}, BackgroundKept: []string{}}
	if strings.Contains(strings.ToLower(req.SourceName), "missingpart") {
		resp.MissingParts = []string{StudioMissingPart}
		resp.Notes = `Fake analyzer: a part is reported missing because the file name contains "missingpart".`
	}
	return resp
}

// orientIDs returns the ids of an orientation request (its answer schema has an
// "upright" field), or ok false for any other request.
func orientIDs(s *analyzer.Schema) ([]string, bool) {
	if s == nil || s.Properties["images"] == nil || s.Properties["images"].Items == nil {
		return nil, false
	}
	item := s.Properties["images"].Items
	if _, ok := item.Properties["upright"]; !ok || item.Properties["id"] == nil {
		return nil, false
	}
	return item.Properties["id"].Enum, true
}
