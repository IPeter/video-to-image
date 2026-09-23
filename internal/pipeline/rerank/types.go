// Package rerank is the OPTIONAL model re-rank (selection.modelRerank, off by default
// per decision 5): send the top candidates per shot as images and let the model pick
// the best one for each shot, in a single call.
package rerank

import (
	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/score"
)

// PerShot is how many top candidates per shot are shown to the model.
const PerShot = 3

// MaxPickReasonRunes caps ModelPick.Reason (see analyze.Truncate).
const MaxPickReasonRunes = 300

type Input struct {
	Domain     *domain.Config
	JobID      string
	CacheKey   string
	Candidates []score.Scored     // all scored candidates; rejected ones are ignored
	Scores     map[string]float64 // weighted score by candidate ID (selectr.Score)
	PerShot    int                // 0 → PerShot
}

// ModelPick / ModelResponse are the JSON the model returns:
// {"picks":[{"shotId":"...","candidateId":"cf_...","reason":"..."}]}.
type ModelPick struct {
	ShotID      string `json:"shotId"`
	CandidateID string `json:"candidateId"`
	Reason      string `json:"reason"`
}

type ModelResponse struct {
	Picks []ModelPick `json:"picks"`
}

type Output struct {
	Preferred map[string]string // shotID → candidate ID the model picked
	Calls     []analyzer.Usage
	RawText   string
}
