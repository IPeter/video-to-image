// Package analyze is pipeline step 2: ONE model call that returns both the domain gate
// verdict and the scene candidates (decision 3). It composes the prompt from the domain
// config, calls the Analyzer with a response schema, parses and validates the JSON
// (one retry with a "Return valid JSON only" nudge), and post-processes timestamps.
package analyze

import (
	"errors"
	"fmt"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
)

// Input for Run.
type Input struct {
	Domain      *domain.Config
	JobID       string
	PreviewPath string  // local path of preview.mp4
	DurationSec float64 // duration of the ORIGINAL video; scene times are clamped to [0, DurationSec]
	CacheKey    string  // stable key for recording/replay (sha256 of the original upload)
	FileHandle  string  // provider file handle from an earlier attempt ("" → upload)
	SourceName  string  // original upload file name; only passed through as analyzer.Request.SourceName
	// PreviewFPS is the preview's real frame rate (preview.Output.FPS); it is what the
	// model is told to sample at. 0 → Domain.Model.PreviewFPS.
	PreviewFPS float64
}

// Gate is the model's verdict on whether the video belongs to the domain. After
// ParseResponse, Reason is at most MaxGateReasonRunes and DetectedSubject at most
// MaxDetectedSubjectRunes runes.
type Gate struct {
	Passed          bool    `json:"passed"`
	Confidence      float64 `json:"confidence"`
	Reason          string  `json:"reason"`
	DetectedSubject string  `json:"detectedSubject"`
}

// ModelScene / ModelResponse are the raw JSON shape the model returns
// (plan/03-pipeline.md "Response schema").
type ModelScene struct {
	Start     string  `json:"start"`
	End       string  `json:"end"`
	ShotID    string  `json:"shotId"`
	Reason    string  `json:"reason"`
	Relevance float64 `json:"relevance"`
}

type ModelResponse struct {
	Gate         Gate         `json:"gate"`
	VideoSummary string       `json:"videoSummary"`
	Scenes       []ModelScene `json:"scenes"`
}

// Scene is a post-processed, validated time range.
type Scene struct {
	Idx       int     // 0-based, ordered by StartSec
	StartSec  float64 // clamped to [0, duration]
	EndSec    float64
	ShotID    string  // a shot ID of the domain, or domain.OtherShotID
	Relevance float64 // clamped to [0, 1]
	Reason    string  // at most MaxSceneReasonRunes runes
	RawStart  string  // what the model actually said
	RawEnd    string
}

// Drop records a model scene that was discarded or altered during post-processing, for
// job_steps.meta. Reason is a short machine-ish string: "unparseable_start",
// "unparseable_end", "end_before_start", "too_short", "out_of_range",
// "unknown_shot_mapped_to_other", "merged", "over_max_scenes".
type Drop struct {
	RawStart string `json:"rawStart"`
	RawEnd   string `json:"rawEnd"`
	ShotID   string `json:"shotId"`
	Reason   string `json:"reason"`
}

// Output of Run. On error, Run still returns a non-nil Output carrying Calls, RawText
// and FileHandle so the runner can record usage, log the raw text and release the file.
type Output struct {
	Gate         Gate
	Accepted     bool    // Gate.Passed && Gate.Confidence >= Domain.Gate.MinConfidence
	VideoSummary string  // at most MaxVideoSummaryRunes runes
	Scenes       []Scene // always empty when !Accepted
	Dropped      []Drop
	Calls        []analyzer.Usage // one per successful model call (2 when the nudge retry ran)
	FileHandle   string           // provider handle of the uploaded preview
	RawText      string           // raw text of the last model response
}

// MinSceneSec: ranges shorter than this after clamping are dropped.
const MinSceneSec = 0.3

// ErrBadOutput: the model returned invalid JSON (or JSON failing validation) twice.
// Maps to MODEL_BAD_OUTPUT.
var ErrBadOutput = errors.New("analyze: model returned invalid output")

// BadOutputError carries the raw text for jobs.internal_log. errors.Is(err, ErrBadOutput) is true.
type BadOutputError struct {
	Raw string
	Err error
}

func (e *BadOutputError) Error() string   { return fmt.Sprintf("%v: %v", ErrBadOutput, e.Err) }
func (e *BadOutputError) Unwrap() []error { return []error{ErrBadOutput, e.Err} }
