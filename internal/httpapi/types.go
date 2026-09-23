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

// Chunked upload (large videos behind a proxy that caps request bodies, e.g.
// Cloudflare's 100 MB): create a session, PUT the chunks, then complete it — which runs
// the same validation as POST /jobs and creates the job.

// CreateUploadRequest starts a chunked upload.
type CreateUploadRequest struct {
	FileName    string `json:"fileName" maxLength:"255" doc:"The file's name (shown on the job; a name containing \"reject\" makes the fake analyzer fail the gate)."`
	SizeBytes   int64  `json:"sizeBytes" minimum:"1" doc:"Exact size of the whole file."`
	ContentType string `json:"contentType,omitempty" doc:"The browser's MIME type; informational (the content is sniffed)."`
}

// UploadSession tells the client how to send the file.
type UploadSession struct {
	UploadID       string    `json:"uploadId" example:"upl_01J8ZK3M2Q7K2M9QXA4B5C6D7E"`
	ChunkSizeBytes int64     `json:"chunkSizeBytes" doc:"Every chunk but the last has exactly this size."`
	ChunkCount     int       `json:"chunkCount" doc:"Chunks are numbered 0..chunkCount-1."`
	ExpiresAt      time.Time `json:"expiresAt" doc:"The session and its chunks are dropped after this unless another request arrives: 5 minutes after create until the first chunk is stored, then the session TTL after the last request. It may also be cancelled earlier to make room once it has been idle (see createUpload)."`
}

type CreateUploadInput struct {
	DomainKey string `path:"domainKey" doc:"Domain key, e.g. cars." example:"cars"`
	Body      CreateUploadRequest
}

type CreateUploadOutput struct {
	Body UploadSession
}

type UploadPathInput struct {
	DomainKey string `path:"domainKey" doc:"Domain key, e.g. cars." example:"cars"`
	UploadID  string `path:"uploadId" doc:"Upload session ID (upl_ + ULID)."`
}

// CompleteUploadOutput is the 202 of completing an upload (same body as POST /jobs).
type CompleteUploadOutput struct {
	Location string `header:"Location"`
	Body     CreateJobResponse
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
	Rotation       int     `json:"rotation" doc:"Degrees clockwise the source frame was rotated to make it upright (0, 90, 180, 270)."`
	Cropped        bool    `json:"cropped" doc:"The frame was cropped around the subject."`
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

// UsageStep is the model usage of one pipeline step (analyze, review) with one model.
type UsageStep struct {
	Step           string  `json:"step" example:"analyze"`
	Model          string  `json:"model" example:"gemini-3.5-flash-lite" doc:"The model that answered; the built-in test analyzer reports \"fake:<model>\"."`
	Calls          int64   `json:"calls"`
	InputTokens    int64   `json:"inputTokens"`
	OutputTokens   int64   `json:"outputTokens"`
	ThinkingTokens int64   `json:"thinkingTokens"`
	CostUSD        float64 `json:"costUsd"`
	Priced         bool    `json:"priced" doc:"The model has a price in the price table, or is the free test analyzer (\"fake:\" prefix). False: real calls to a model without a price, recorded at costUsd 0 although they were billed."`
}

// JobUsage is the job's total model usage and cost (from usage_events).
type JobUsage struct {
	Calls          int64       `json:"calls"`
	InputTokens    int64       `json:"inputTokens"`
	OutputTokens   int64       `json:"outputTokens"`
	ThinkingTokens int64       `json:"thinkingTokens"`
	TotalTokens    int64       `json:"totalTokens"`
	CostUSD        float64     `json:"costUsd" doc:"USD, computed from the price table when each call was made. Only the full cost when costComplete."`
	TestMode       bool        `json:"testMode" doc:"Every call was answered by the built-in test analyzer (model \"fake:...\"), so nothing was charged. False when there were no calls."`
	CostComplete   bool        `json:"costComplete" doc:"Every step is priced, so costUsd is the whole cost (true when there were no calls). False: some calls went to a model missing from the price table, so their cost is unknown (not 0); also false when the usage could not be read."`
	Steps          []UsageStep `json:"steps"`
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
	Usage     JobUsage   `json:"usage"`
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
