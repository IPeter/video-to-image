// Package locate is the contact-sheet pass between analyze and extract: the whole
// preview is shown to the model as sheets of time-stamped frames and it points out the
// seconds at which each view is shown with the whole subject in frame. Those seconds
// become scenes next to the analyze call's.
//
// Why: the analyze call watches a 2 fps preview and answers in whole seconds; on a real
// walkaround it put the side profile at 4–7 s while the car was only fully in frame
// from 6.5 s to 10.5 s, so every frame extracted for that view was cut off and the view
// was reported missing. Frames are extracted only where a scene says, so a moment the
// analyze call did not name is invisible downstream. On a sheet the model sees the
// actual frames with their times, which it reads far more reliably than it estimates
// positions in a video stream.
package locate

import (
	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/media"
)

// Defaults for domain.Locate zero values.
const (
	DefaultFPS        = 1.0
	DefaultMaxPerShot = 4
)

// Sheet layout. Every sampled second is one quad: the frame four times in a 2×2 grid,
// as recorded and turned 90°, 180° and 270° clockwise, each copy fitted into a
// CellSide square, so exactly one copy is upright whichever way the phone was held.
// The model recognised a car lying on its side in a single tile so poorly that the
// sideways side-profile seconds of the reference walkaround were missed in three runs;
// the review's orientation grids of the same shape scored 4 of 4. A sheet holds
// Cols × Rows quads (6 seconds, about 1480 × 990 px), so a one-minute video costs 10
// images.
const (
	Cols     = 3
	Rows     = 2
	CellSide = 240
	InnerGap = 4 // between the four copies of a quad
	Gap      = 8 // around the quads
	// QuadSide is the side of one second's square.
	QuadSide = 2*CellSide + InnerGap
	// LabelScale multiplies the 7×13 px bitmap font of the time label.
	LabelScale = 3
	// JPEGQuality of the sheets sent to the model.
	JPEGQuality = 85
)

// quadCells lists the four copies of a quad: position and the clockwise rotation shown.
var quadCells = []struct {
	name   string
	rotCW  int
	dx, dy int
}{
	{"top-left", 0, 0, 0},
	{"top-right", 90, 1, 0},
	{"bottom-left", 180, 0, 1},
	{"bottom-right", 270, 1, 1},
}

// SystemPrompt is the system instruction of the locate call.
const SystemPrompt = "You find moments in a video shown as contact sheets of time-stamped frames and return structured JSON only."

// Input for Run.
type Input struct {
	Domain *domain.Config
	JobID  string
	// PreviewPath is the analyze step's preview.mp4 (already upright where the
	// container said so; the sideways parts of a turned phone stay sideways).
	PreviewPath string
	DurationSec float64 // of the original (the preview is at most this long)
	WorkDir     string  // scratch dir; Run writes WorkDir/locate/
	CacheKey    string  // stable key for recording/replay (sha256 of the original)
	FFmpeg      *media.FFmpeg
}

// Moment is one second the model pointed out.
type Moment struct {
	Sec        float64 // time in the video, snapped to the sheet's sampling grid
	ShotID     string  // a shot ID of the domain
	Confidence float64 // 0–1
	// Whole: the model saw the whole subject inside the frame. A moment listed with
	// Whole false is kept (the review judges the full-resolution frame) at a lower
	// relevance, so the pass errs on recall: the copies are small and a car that
	// nearly fills one looks cut off when it is not.
	Whole bool
	// RotateCW is the clockwise rotation of the copy the model called upright (0, 90,
	// 180, 270); -1 when it named none. Informational (the review's orientation call
	// decides per frame).
	RotateCW int
}

// PartialRelevance scales the confidence of a moment the model listed with the
// subject not (or not surely) whole.
const PartialRelevance = 0.6

// Output of Run. On error it still carries Calls and RawText so the caller can record
// usage and log the raw text.
type Output struct {
	Moments []Moment // kept, ordered by Sec then shot priority
	// Dropped counts answered moments that were not kept: the second lies outside the
	// video, the view is unknown, or a repeat.
	Dropped int
	Frames  int // sweep frames on the sheets
	Sheets  int
	Calls   []analyzer.Usage
	RawText string
}

func fps(l domain.Locate) float64 {
	if l.FPS > 0 {
		return l.FPS
	}
	return DefaultFPS
}

func maxPerShot(l domain.Locate) int {
	if l.MaxPerShot > 0 {
		return l.MaxPerShot
	}
	return DefaultMaxPerShot
}
