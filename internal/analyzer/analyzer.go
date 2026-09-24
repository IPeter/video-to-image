// Package analyzer is the model-provider boundary. An Analyzer sends media + a prompt
// to a model and returns the raw text answer plus token usage. Prompt composition,
// JSON parsing, validation and the "return valid JSON only" nudge live in the pipeline
// steps (internal/pipeline/analyze, internal/pipeline/review), so every provider —
// gemini, fake, recorded — is interchangeable.
package analyzer

import (
	"context"
	"errors"
	"time"
)

// Step names used in Request.Step and usage_events.step.
const (
	StepAnalyze = "analyze"
	StepRerank  = "rerank" // legacy, replaced by StepReview
	StepReview  = "review"
	StepStudio  = "studio" // the studio-background QA check (internal/studio)
)

// ErrUnavailable means the provider failed after its own transient-error retries
// (HTTP 429 / 5xx / timeouts). The job fails with MODEL_UNAVAILABLE.
var ErrUnavailable = errors.New("analyzer: model unavailable")

// ErrNoRecording is returned by the replay analyzer when no recording matches the request.
var ErrNoRecording = errors.New("analyzer: no recording for request")

// Request is one model call.
type Request struct {
	Step      string // StepAnalyze | StepReview | StepStudio
	JobID     string // for logs
	DomainKey string // for logs
	// CacheKey identifies the request for recording/replay. It must be stable across
	// runs of the same input, e.g. sha256(original) + ":" + domainKey + ":" + Step.
	CacheKey string

	Model          string
	Temperature    float32
	ThinkingBudget *int32 // nil → provider default
	ThinkingLevel  string // "minimal" | "low" | "medium" | "high"; "" → provider default. Not with ThinkingBudget.
	// MaxOutputTokens caps the answer (0 → provider default). Always set it for paid
	// providers: a runaway response is otherwise billed in full.
	MaxOutputTokens int32
	SystemPrompt    string
	// Parts are sent in order as one user turn. Media parts first, then text, is the
	// recommended order for video prompts.
	Parts []Part
	// ResponseSchema, when set, asks for JSON matching the schema (structured output).
	ResponseSchema *Schema

	// Hints for fakes and usage accounting; providers may ignore them.
	DurationSec float64  // duration of the video part, seconds
	ShotIDs     []string // valid shot IDs for this domain, in priority order
	// SourceName is the original upload's file name. Real providers MUST ignore it (it
	// is never sent to a model); the fake analyzer uses it so tests can request a
	// rejection deterministically (a name containing "reject" fails the gate).
	SourceName string
}

// Part is either text or a media file.
type Part struct {
	Text string

	FilePath string  // local path of an image or video to send
	MIMEType string  // "video/mp4", "image/jpeg", ...
	VideoFPS float64 // requested model sampling fps for video (0 → provider default)
	// FileHandle reuses a file uploaded by an earlier attempt (provider-specific, e.g.
	// Gemini "files/abc123"). Providers fall back to uploading FilePath when the handle
	// is empty, unknown or no longer usable.
	FileHandle string
}

// Response is one successful model call.
type Response struct {
	Text  string // raw model text (JSON when ResponseSchema was set)
	Usage Usage
	// FileHandles has one entry per request Part (empty for text parts and for
	// providers without uploads). Callers reuse them within one attempt (the analyze
	// nudge retry) and release them with ReleaseFile afterwards; they are not persisted
	// across worker restarts (a re-delivered job uploads the preview again).
	FileHandles []string
}

// Usage is recorded as one usage_events row per call.
type Usage struct {
	Model          string // model name as reported by the provider (or as requested)
	InputTokens    int
	OutputTokens   int
	ThinkingTokens int
	Latency        time.Duration
}

// Analyzer is implemented by gemini.Client, fake.Analyzer and recording.Recorder/Replayer.
type Analyzer interface {
	// Generate performs one model call. Transient provider failures are retried
	// internally; after that the error wraps ErrUnavailable. Context errors are
	// returned as-is.
	Generate(ctx context.Context, req *Request) (*Response, error)
	// ReleaseFile deletes an uploaded file handle. Unknown/empty handles are a no-op.
	ReleaseFile(ctx context.Context, handle string) error
	// Name identifies the implementation in logs ("gemini", "fake", ...).
	Name() string
}

// Sweeper is optionally implemented by analyzers that upload files; the daily
// maintenance job calls it to delete leaked uploads older than olderThan.
type Sweeper interface {
	SweepFiles(ctx context.Context, olderThan time.Duration) (deleted int, err error)
}

// Schema is a provider-neutral subset of OpenAPI schema used for structured output.
// gemini converts it to genai.Schema.
type Schema struct {
	Type             string // "object", "array", "string", "number", "integer", "boolean"
	Description      string
	Properties       map[string]*Schema
	PropertyOrdering []string // key order the model should emit (Gemini honours it)
	Required         []string
	Items            *Schema
	Enum             []string
	Nullable         bool
	Minimum          *float64
	Maximum          *float64
}
