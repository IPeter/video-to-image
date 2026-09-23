// Package store is the Postgres repository (pgx v5 + sqlc). sqlc output lives in
// internal/store/sqlcdb and is wrapped here so callers see plain Go types. The schema
// is migrations/*.sql (goose).
package store

import (
	"errors"
	"time"

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
