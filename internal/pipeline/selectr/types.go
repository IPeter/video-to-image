// Package selectr is pipeline step 5: pick the final set — cover the shot list, keep
// frames sharp and visually distinct, and explain shortfalls instead of padding.
// (Named selectr because select is a Go keyword.)
package selectr

import (
	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/score"
)

// MinGapSec: two selected frames must be at least this far apart in time.
const MinGapSec = 1.0

type Params struct {
	ImageCount       int
	Shots            []domain.Shot // priority order
	Weights          domain.Weights
	DedupeMaxHamming int
	MinGapSec        float64
	// Preferred maps shotID → candidate ID chosen by the optional model re-rank. In the
	// coverage pass a preferred, eligible candidate wins over the best-scoring one.
	Preferred map[string]string
	// MaxPerShot caps the images of one shot ID ("other" included) in both passes.
	// <= 0 → no cap.
	MaxPerShot int
	// Assessments are the model review's verdicts by candidate ID. When non-nil the
	// review ran: ONLY assessed candidates with Usable == true are eligible, they are
	// grouped by Assessment.View instead of the scene's shot, and ordered by Quality
	// (desc) before score. nil → no review (selection as without it).
	Assessments map[string]Assessment
}

// Assessment is the part of a model review verdict selection needs.
type Assessment struct {
	View    string // the shot the frame really shows (a domain shot ID or domain.OtherShotID)
	Usable  bool   // subject fully visible, quality >= the domain minimum, a real view
	Quality int    // 1-5
	// Reason replaces the scene reason on the selected image when non-empty. When empty
	// and View is not the scene's shot, the image gets a neutral text for View instead
	// (the scene reason describes the old view).
	Reason string
	// DuplicateOf is a candidate the review says shows the same view as this one:
	// the two are never both selected, and neither are two frames linked through a
	// chain of such links.
	DuplicateOf string
}

// Selected is one chosen frame.
type Selected struct {
	Candidate score.Scored
	ShotID    string  // the shot this image fills (the effective shot, see Select)
	Score     float64 // weighted score
	Reason    string  // the review's or the scene's model reason, see Select
}

type Output struct {
	Images       []Selected         // in shot-list order (shots by priority, then "other"), ties by timestamp
	Scores       map[string]float64 // weighted score of every candidate by candidate ID
	MissingShots []domain.Shot      // required shots with no selected image, in priority order
	Explanation  string
}
