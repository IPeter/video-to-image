package analyze

import (
	"context"
	"errors"
	"sync"
	"testing"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
)

// testDomain is a small, fully specified domain so golden output does not depend on the
// shipped configs. The whitespace in the text fields is deliberate (see BuildPrompt).
func testDomain() *domain.Config {
	tb := int32(0)
	return &domain.Config{
		Key:            "autos",
		Name:           "Test Autos",
		AllowedOrigins: []string{"http://localhost:3000"},
		Limits: domain.Limits{
			MaxUploadBytes: 1 << 20, MaxDurationSec: 180, MaxCandidateFrames: 300, RatePerIPPerHour: 10,
		},
		Model: domain.Model{Name: "gemini-test-lite", Temperature: 0.2, ThinkingBudget: &tb, PreviewHeight: 480, PreviewFPS: 2},
		Gate: domain.Gate{
			Subject:       "  a single car,\n  shown for a sales listing.  ",
			Instructions:  "Pass if one car is the main subject.   \nFail if it is mainly people or scenery.\n\n\n\nSeveral different cars also fail.\n",
			MinConfidence: 0.6,
		},
		Shots: []domain.Shot{
			{ID: "front_three_quarter", Label: "Front 3/4", Description: "Front and one side at roughly 45°,\n whole car in frame", Required: true},
			{ID: "side_profile", Label: "Side profile", Description: "Full side of the car, perpendicular", Required: true},
			{ID: "interior_dashboard", Label: "Dashboard", Description: "Dashboard, steering wheel and center console"},
		},
		Selection: domain.Selection{
			ImageCount:       4,
			Instructions:     "\n  Prefer moments where the whole car is in frame.\n  Avoid motion blur.\n",
			MaxScenes:        5,
			SampleFPS:        4,
			Weights:          domain.Weights{Relevance: 0.5, Sharpness: 0.4, Exposure: 0.1},
			DedupeMaxHamming: 6,
		},
		RetentionHours: 24,
		Copy:           domain.Copy{UploadTitle: "t", UploadHint: "h", RejectedTitle: "r"},
	}
}

// step is one scripted answer: either fn, or resp/err.
type step struct {
	resp *analyzer.Response
	err  error
	fn   func(ctx context.Context, req *analyzer.Request) (*analyzer.Response, error)
}

// scripted is a fake analyzer that replays steps in order and records every request.
type scripted struct {
	mu    sync.Mutex
	steps []step
	reqs  []*analyzer.Request
}

var errUnscripted = errors.New("scripted: unexpected call")

func newScripted(steps ...step) *scripted { return &scripted{steps: steps} }

func (s *scripted) Generate(ctx context.Context, req *analyzer.Request) (*analyzer.Response, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	if len(s.steps) == 0 {
		s.mu.Unlock()
		return nil, errUnscripted
	}
	st := s.steps[0]
	s.steps = s.steps[1:]
	s.mu.Unlock()
	if st.fn != nil {
		return st.fn(ctx, req)
	}
	return st.resp, st.err
}

func (s *scripted) ReleaseFile(context.Context, string) error { return nil }
func (s *scripted) Name() string                              { return "scripted" }

func (s *scripted) requests(t *testing.T) []*analyzer.Request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*analyzer.Request(nil), s.reqs...)
}

// answer is a successful response with a file handle on the video part.
func answer(text string, handle string, in, out int) step {
	return step{resp: &analyzer.Response{
		Text:        text,
		Usage:       analyzer.Usage{Model: "gemini-test-lite", InputTokens: in, OutputTokens: out},
		FileHandles: []string{handle, ""},
	}}
}
