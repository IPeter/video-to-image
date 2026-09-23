package httpapi

import (
	"time"

	"github.com/danielgtaylor/huma/v2"

	"video-to-image/internal/domain"
)

// DTOs for the HTTP contract in plan/05-api.md. The OpenAPI document (and through it
// web/lib/api.d.ts) is generated from these types, so they ARE the contract.

// JobStatus mirrors store.JobStatus on the wire.
type JobStatus string

const (
	JobQueued     JobStatus = "queued"
	JobProcessing JobStatus = "processing"
	JobCompleted  JobStatus = "completed"
	JobRejected   JobStatus = "rejected"
	JobFailed     JobStatus = "failed"
	JobExpired    JobStatus = "expired"
)

func (JobStatus) Schema(huma.Registry) *huma.Schema {
	return enumSchema("Job status.", JobQueued, JobProcessing, JobCompleted, JobRejected, JobFailed, JobExpired)
}

// StepName is a pipeline step.
type StepName string

func (StepName) Schema(huma.Registry) *huma.Schema {
	return enumSchema("Pipeline step.", "preview", "analyze", "extract", "score", "select", "export")
}

// StepStatus mirrors store.StepStatus.
type StepStatus string

func (StepStatus) Schema(huma.Registry) *huma.Schema {
	return enumSchema("Step status.", "pending", "running", "done", "failed", "skipped")
}

// GetDomainOutput → GET /v1/domains/{domainKey}
type GetDomainOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         domain.PublicDomain
}

// CreateJobResponse is the 202 body of POST /v1/domains/{domainKey}/jobs.
type CreateJobResponse struct {
	JobID  string    `json:"jobId"  example:"job_01J8ZK3M2Q7K2M9QXA4B5C6D7E"`
	Status JobStatus `json:"status"`
}

// VideoInfo is the public part of the probed video.
type VideoInfo struct {
	DurationSec float64 `json:"durationSec"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	FPS         float64 `json:"fps"`
}

type ProgressStep struct {
	Name   StepName   `json:"name"`
	Status StepStatus `json:"status"`
	Ms     *int       `json:"ms,omitempty" doc:"Duration in milliseconds, set once the step finished."`
}

type Progress struct {
	Step    *StepName      `json:"step" nullable:"true" doc:"The step currently running, null when none is."`
	Percent int            `json:"percent" minimum:"0" maximum:"100"`
	Steps   []ProgressStep `json:"steps"`
}

type ResultImage struct {
	ID             string  `json:"id"`
	Index          int     `json:"index" doc:"1-based display order (shot-list order)."`
	ShotID         string  `json:"shotId"`
	ShotLabel      string  `json:"shotLabel"`
	TimestampSec   float64 `json:"timestampSec"`
	TimestampLabel string  `json:"timestampLabel" example:"00:06"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	Reason         string  `json:"reason"`
	FileName       string  `json:"fileName" example:"cars_7K2M9QXA_1_front_three_quarter_00-06.jpg"`
	URL            string  `json:"url" doc:"Full-resolution JPG, relative to the API base URL."`
	DownloadURL    string  `json:"downloadUrl" doc:"Same as url with ?download=1 (Content-Disposition: attachment)."`
	ThumbnailURL   string  `json:"thumbnailUrl" doc:"640 px wide JPG, relative to the API base URL."`
}

type ShotRef struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type JobResult struct {
	_            struct{}      `nullable:"true"`
	Summary      string        `json:"summary"`
	Images       []ResultImage `json:"images"`
	MissingShots []ShotRef     `json:"missingShots" doc:"Required shots that were not found."`
	Explanation  string        `json:"explanation"`
	ZipURL       *string       `json:"zipUrl" doc:"All images + manifest.json; null when there are no images."`
}

type Rejection struct {
	_               struct{} `nullable:"true"`
	Reason          string   `json:"reason"`
	DetectedSubject string   `json:"detectedSubject"`
}

// JobErrorCode is the subset of error codes a failed job can carry.
type JobErrorCode string

func (JobErrorCode) Schema(huma.Registry) *huma.Schema {
	return enumSchema("Why a job failed.", CodeModelUnavailable, CodeModelBadOutput, CodeProcessingError)
}

type JobError struct {
	_       struct{}     `nullable:"true"`
	Code    JobErrorCode `json:"code"`
	Message string       `json:"message"`
}

// Job is GET /v1/domains/{domainKey}/jobs/{jobId}.
type Job struct {
	JobID     string     `json:"jobId"`
	DomainKey string     `json:"domainKey"`
	Status    JobStatus  `json:"status"`
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"`
	Video     VideoInfo  `json:"video"`
	Progress  Progress   `json:"progress"`
	Result    *JobResult `json:"result"`
	Rejection *Rejection `json:"rejection"`
	Error     *JobError  `json:"error"`
}

type GetJobOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         Job
}

// UsageRow is one line of GET /internal/usage.
type UsageRow struct {
	Day            string  `json:"day" example:"2026-09-23"`
	DomainKey      string  `json:"domainKey"`
	Jobs           int64   `json:"jobs"`
	Calls          int64   `json:"calls"`
	InputTokens    int64   `json:"inputTokens"`
	OutputTokens   int64   `json:"outputTokens"`
	ThinkingTokens int64   `json:"thinkingTokens"`
	VideoSeconds   float64 `json:"videoSeconds"`
	CostUSD        float64 `json:"costUsd"`
}

type UsageReport struct {
	From         time.Time  `json:"from" doc:"00:00 UTC of the first day (inclusive)."`
	To           time.Time  `json:"to" doc:"00:00 UTC of the last day (inclusive)."`
	Rows         []UsageRow `json:"rows"`
	TotalCostUSD float64    `json:"totalCostUsd"`
}

type GetUsageOutput struct {
	Body UsageReport
}

func enumSchema[T ~string](doc string, values ...T) *huma.Schema {
	enum := make([]any, len(values))
	for i, v := range values {
		enum[i] = string(v)
	}
	return &huma.Schema{Type: huma.TypeString, Description: doc, Enum: enum}
}
