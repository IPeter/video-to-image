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
}

// Selected is one chosen frame.
type Selected struct {
	Candidate score.Scored
	ShotID    string  // the shot this image fills (the candidate's shot)
	Score     float64 // weighted score
	Reason    string  // the scene's model reason
}

type Output struct {
	Images       []Selected         // in shot-list order (shots by priority, then "other"), ties by timestamp
	Scores       map[string]float64 // weighted score of every candidate by candidate ID
	MissingShots []domain.Shot      // required shots with no selected image, in priority order
	Explanation  string
}
