// Package review is the model's check of shortlisted still frames. It replaces the
// plan's optional re-rank. Two calls see the top candidates of every shot: the first
// finds, per frame, the rotation that makes it upright (orient.go); the second, on the
// upright copies, returns the view each frame really shows, whether the subject is
// fully in frame, a 1-5 listing quality, the subject's bounding box and near-duplicate
// links. Selection keeps only usable frames; export rotates and crops them.
package review

import (
	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/score"
)

// Defaults for domain.Review zero values.
const (
	DefaultCandidatesPerShot = 3
	DefaultMinQuality        = 3
	DefaultMaxImageSide      = 512
	// MaxImages bounds one request (tokens and latency) whatever the shot count.
	MaxImages = 40
)

// ViewNone is the view the model reports when a frame shows no relevant subject.
const ViewNone = "none"

type Input struct {
	Domain     *domain.Config
	JobID      string
	CacheKey   string             // stable key for recording/replay (sha256 of the original)
	WorkDir    string             // scratch dir; Run writes the downscaled images to WorkDir/review/
	Candidates []score.Scored     // all scored candidates; rejected ones are never sent
	Scores     map[string]float64 // weighted score by candidate ID (selectr.Score), for the shortlist
}

// Assessment is the model's verdict on one frame.
type Assessment struct {
	CandidateID  string
	RotateCW     int    // 0, 90, 180 or 270: degrees clockwise that make the frame upright
	View         string // domain shot ID, domain.OtherShotID or ViewNone
	FullyVisible bool   // the whole subject is inside the frame
	Quality      int    // 1-5
	// Box is the subject's bounding box in the frame AS STORED (before RotateCW; the
	// model measures it on the copy sent, which may be rotated, and parseAssessments maps
	// it back), normalised to [0,1]: {X0, Y0, X1, Y1}. Zero value = no usable box.
	Box    Box
	Issues string // short free text from the model (capped)
	// DuplicateOf is the candidate ID of an EARLIER shortlisted frame the model says
	// shows essentially the same view from nearly the same position ("" = none).
	// Pixel hashes miss these (the framing differs); selection never picks both.
	DuplicateOf string
	// Usable: FullyVisible && Quality >= min quality && View != ViewNone.
	Usable bool
}

// Box is a normalised rectangle (0 ≤ X0 < X1 ≤ 1, 0 ≤ Y0 < Y1 ≤ 1).
type Box struct{ X0, Y0, X1, Y1 float64 }

// Valid reports whether b is a non-degenerate rectangle inside [0,1]².
func (b Box) Valid() bool {
	return b.X0 >= 0 && b.Y0 >= 0 && b.X1 <= 1 && b.Y1 <= 1 && b.X1-b.X0 > 0.01 && b.Y1-b.Y0 > 0.01
}

type Output struct {
	Assessments map[string]Assessment // by candidate ID; only frames that were sent and answered
	// Rotations is the clockwise rotation (0/90/180/270) that makes a shortlisted frame
	// upright, by candidate ID, for every frame the orientation call answered (never
	// nil). It is filled before the judgement call, so it survives a failed review. A
	// frame sent as stored gets its rotation from the judgement (Assessment.RotateCW)
	// only.
	Rotations map[string]int
	// OrientationErr is set when the orientation call failed (after its one retry) or no
	// grid could be written; the frames were then judged as stored.
	OrientationErr error
	Sent           int // frames sent to the model
	Calls          []analyzer.Usage
	RawText        string
}
