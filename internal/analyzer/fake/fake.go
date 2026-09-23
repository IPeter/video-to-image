// Package fake is a deterministic, free analyzer.Analyzer for tests, e2e runs and local
// UI work (V2I_ANALYZER=fake). It never touches the network: for the analyze step it
// returns either a canned JSON file verbatim or synthetic scenes spread evenly over the
// video, one per shot ID; for the re-rank step it returns no picks.
//
// A request whose SourceName contains "reject" (case-insensitive) fails the domain gate,
// so tests and demos can exercise the rejection path deterministically.
package fake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
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
	case analyzer.StepRerank:
		text = `{"picks":[]}`
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
		Text: text,
		Usage: analyzer.Usage{
			Model:        ModelPrefix + req.Model,
			InputTokens:  int(duration*100) + 500,
			OutputTokens: 300,
			Latency:      a.delay,
		},
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
