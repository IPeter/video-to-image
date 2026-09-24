// Package store is the Postgres repository (pgx v5 + sqlc). sqlc output lives in
// internal/store/sqlcdb and is wrapped here so callers see plain Go types. The schema
// is migrations/*.sql (goose).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"video-to-image/internal/media"
)

var (
	// ErrNotFound is returned for a missing job, image, etc.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict is returned when a status transition is not allowed from the job's
	// current status (e.g. completing a job the user already deleted → expired).
	ErrConflict = errors.New("store: invalid state transition")
)

type JobStatus string

const (
	StatusQueued     JobStatus = "queued"
	StatusProcessing JobStatus = "processing"
	StatusCompleted  JobStatus = "completed"
	StatusRejected   JobStatus = "rejected"
	StatusFailed     JobStatus = "failed"
	StatusExpired    JobStatus = "expired"
)

// Terminal reports whether no more pipeline work happens for the status.
func (s JobStatus) Terminal() bool {
	switch s {
	case StatusCompleted, StatusRejected, StatusFailed, StatusExpired:
		return true
	}
	return false
}

type StepStatus string

const (
	StepPending StepStatus = "pending"
	StepRunning StepStatus = "running"
	StepDone    StepStatus = "done"
	StepFailed  StepStatus = "failed"
	StepSkipped StepStatus = "skipped"
)

// Pipeline step names, in execution order. CreateJob inserts one pending row per step.
const (
	StepNamePreview = "preview"
	StepNameAnalyze = "analyze"
	StepNameExtract = "extract"
	StepNameScore   = "score"
	StepNameSelect  = "select"
	StepNameExport  = "export"
)

var StepNames = []string{StepNamePreview, StepNameAnalyze, StepNameExtract, StepNameScore, StepNameSelect, StepNameExport}

// Gate is jobs.gate.
type Gate struct {
	Passed          bool    `json:"passed"`
	Confidence      float64 `json:"confidence"`
	Reason          string  `json:"reason"`
	DetectedSubject string  `json:"detectedSubject"`
}

// Job is a jobs row. Nullable text columns map to "" when NULL.
type Job struct {
	ID                string
	DomainKey         string
	Status            JobStatus
	CreatedAt         time.Time
	UpdatedAt         time.Time
	ExpiresAt         time.Time
	ClientIPHash      string
	VideoPath         string // storage key of the original; "" once deleted
	VideoInfo         media.VideoInfo
	ModelName         string
	Gate              *Gate // nil until analyze ran
	VideoSummary      string
	ResultExplanation string
	MissingShots      []string // shot IDs
	ZipPath           string
	ErrorCode         string
	ErrorMessage      string
	InternalLog       string
	CostUSD           float64
	StudioStatus      StudioStatus // studio backgrounds, see internal/studio
	StudioZipPath     string       // storage key of studio.zip; "" until built
}

// NewJob is the input of CreateJob. Status starts as queued.
type NewJob struct {
	ID           string
	DomainKey    string
	ExpiresAt    time.Time
	ClientIPHash string
	VideoPath    string
	VideoInfo    media.VideoInfo
	ModelName    string
}

// JobRef identifies a job for maintenance work.
type JobRef struct {
	ID        string
	DomainKey string
	Status    JobStatus
	// StudioStatus is set by ListExpiredJobs and ListOrphanedStudioJobs ("" elsewhere).
	StudioStatus StudioStatus
}

// Step is a job_steps row.
type Step struct {
	JobID      string
	Name       string
	Status     StepStatus
	StartedAt  *time.Time
	FinishedAt *time.Time
	DurationMs *int
	Meta       map[string]any // never nil when read (empty map)
}

// Scene is a scenes row.
type Scene struct {
	ID        int64 // set when read
	JobID     string
	Idx       int
	StartSec  float64
	EndSec    float64
	ShotID    string
	Relevance float64
	Reason    string
	RawStart  string
	RawEnd    string
}

// CandidateFrame is a candidate_frames row. On insert, SceneIdx is resolved to scene_id
// through (job_id, idx); SceneIdx < 0 stores NULL.
type CandidateFrame struct {
	ID              string
	JobID           string
	SceneIdx        int
	SceneID         *int64 // set when read
	ShotID          string
	TimestampSec    float64
	Sharpness       float64
	SharpnessZ      float64
	ExposurePenalty float64
	DHash           uint64
	DuplicateOf     string // "" → NULL
	RejectedReason  string
	Score           float64
	Selected        bool
}

// SelectedImage is a selected_images row.
type SelectedImage struct {
	ID           string
	JobID        string
	CandidateID  string // "" → NULL
	Idx          int    // 1-based
	ShotID       string
	TimestampSec float64
	Width        int
	Height       int
	Reason       string
	FileName     string
	ImagePath    string // "" once expired
	ThumbPath    string
	Bytes        int64
	Rotation     int    // degrees clockwise applied to the source frame
	Crop         *NRect // normalised crop of the upright frame; nil = uncropped
	// SubjectBox is the reviewed subject box in the EXPORTED image (after rotation and
	// crop); nil without a review verdict. The studio step prompts segmentation with it.
	SubjectBox *NRect
	Studio     *ImageStudio // nil = not processed by the studio step
}

// StudioStatus is a job's studio-background progress (jobs.studio_status).
type StudioStatus string

const (
	StudioNone    StudioStatus = "none"    // not requested (feature off, or no eligible image)
	StudioPending StudioStatus = "pending" // enqueued after the pipeline completed
	StudioRunning StudioStatus = "running"
	StudioDone    StudioStatus = "done"    // every eligible image has a final ImageStudio
	StudioFailed  StudioStatus = "failed"  // gave up (sidecar down, budget); originals unaffected
	StudioSkipped StudioStatus = "skipped" // daily cost ceiling, or the job was deleted first
)

// Active reports whether studio work is still to come for the job (pending or running).
func (s StudioStatus) Active() bool { return s == StudioPending || s == StudioRunning }

// ImageStudio.Status values. Every one is final: the studio step never revisits an image
// that has one.
const (
	ImageStudioReady    = "ready"    // variants exist (ImageStudio.Styles)
	ImageStudioWithheld = "withheld" // a gate or the QA check refused the cut-out
	ImageStudioSkipped  = "skipped"  // not eligible, or not attempted (daily cost ceiling)
	ImageStudioFailed   = "failed"   // an error; only the original is offered
)

// ImageStudio is one image's studio outcome (selected_images.studio, jsonb).
type ImageStudio struct {
	// Status: "ready" (variants exist), "withheld" (a gate or the QA check refused the
	// cut-out; Reason says why), "skipped" (not eligible: view, not fully visible, no
	// box), "failed" (an error; Reason is internal and short).
	Status string                  `json:"status"`
	Reason string                  `json:"reason,omitempty"`
	Styles map[Style]StudioVariant `json:"styles,omitempty"`
	// Diagnostics: mask stats, gate values, QA verdict, timings (opaque JSON for ops).
	Diagnostics json.RawMessage `json:"diagnostics,omitempty"`
}

// Style is a studio backdrop name (studio.Style values: grey, light, dark).
type Style string

// StudioVariant is one stored studio rendering of an image.
type StudioVariant struct {
	Key      string `json:"key"`      // storage key of the full-size JPEG
	ThumbKey string `json:"thumbKey"` // storage key of the 640 px thumbnail
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Bytes    int64  `json:"bytes"`
}

// NRect is a normalised rectangle, stored as jsonb {x0,y0,x1,y1}.
type NRect struct {
	X0 float64 `json:"x0"`
	Y0 float64 `json:"y0"`
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
}

// UsageByStep is one job's model usage summed per step and model.
type UsageByStep struct {
	Step           string
	Model          string
	Calls          int64
	InputTokens    int64
	OutputTokens   int64
	ThinkingTokens int64
	CostUSD        float64
}

// AnalysisResult is the input of SaveAnalysis: everything the analyze step produced,
// written in ONE transaction so a paid model answer is never half-recorded.
type AnalysisResult struct {
	Gate      Gate
	Summary   string
	ModelName string  // "" keeps the model recorded at creation
	Scenes    []Scene // replaces the job's scenes (empty when rejected)
	StepMeta  any     // job_steps.meta of the analyze step
	// Reject also moves the job processing → rejected in the same transaction (gate
	// failed or confidence below the domain minimum): video_path NULL, every pending or
	// running step skipped.
	Reject bool
}

// Completion is the input of CompleteJob.
type Completion struct {
	Explanation  string
	MissingShots []string // shot IDs
	ZipPath      string   // "" when there are no images
	Images       []SelectedImage
	// Studio is the job's studio_status after completion: "" or StudioNone (no studio
	// backgrounds), or StudioPending with Enqueue scheduling the studio job.
	Studio StudioStatus
	// Enqueue, when set, runs inside the completion's transaction after the job row and
	// images are written (River InsertTx of the studio job): an error rolls back the
	// whole completion, so a job is never pending studio work without a studio job.
	Enqueue func(ctx context.Context, tx pgx.Tx) error
}

// StudioFinish is the input of FinishStudio.
type StudioFinish struct {
	Status  StudioStatus // StudioDone, StudioFailed or StudioSkipped
	ZipPath string       // storage key of studio.zip; "" when none was built
	// Unprocessed, when set, is stored for every image still without a studio outcome
	// (e.g. skipped: the daily cost ceiling was reached before they were done).
	Unprocessed *ImageStudio
}

// UsageEvent is a usage_events row.
type UsageEvent struct {
	JobID          string
	DomainKey      string
	Step           string
	Model          string
	InputTokens    int
	OutputTokens   int
	ThinkingTokens int
	VideoSeconds   float64
	CostUSD        float64
	LatencyMs      int
	CreatedAt      time.Time // set when read
}

// UsageRow is one line of the /internal/usage report: totals per UTC day per domain.
type UsageRow struct {
	Day            time.Time // 00:00 UTC of the day
	DomainKey      string
	Jobs           int64 // distinct jobs with usage that day
	Calls          int64
	InputTokens    int64
	OutputTokens   int64
	ThinkingTokens int64
	VideoSeconds   float64
	CostUSD        float64
}
