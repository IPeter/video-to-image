package rerank

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/analyze"
	"video-to-image/internal/pipeline/extract"
	"video-to-image/internal/pipeline/score"
)

const (
	f3q  = "front_three_quarter"
	side = "side_profile"
	dash = "interior_dashboard"
	rear = "rear"
)

func testDomain() *domain.Config {
	tb := int32(0)
	return &domain.Config{
		Key:   "autos",
		Model: domain.Model{Name: "gemini-test-lite", Temperature: 0.2, ThinkingBudget: &tb, PreviewHeight: 480, PreviewFPS: 2},
		Shots: []domain.Shot{
			{ID: f3q, Label: "Front 3/4", Description: "Front and one side at roughly 45°, whole car in frame.", Required: true},
			{ID: side, Label: "Side profile", Description: "Full side of the car", Required: true},
			{ID: dash, Label: "Dashboard", Description: "Dashboard and\n  steering wheel"},
			{ID: rear, Label: "Rear", Description: "Straight-on rear"},
		},
		Selection: domain.Selection{ModelRerank: true},
	}
}

// cand builds a scored candidate; the ID doubles as the file name.
func cand(id, shot string, ts float64, rejected string) score.Scored {
	return score.Scored{
		Candidate: extract.Candidate{
			ID: id, ShotID: shot, TimestampSec: ts,
			FullPath: "/w/cand/" + id + ".jpg", SmallPath: "/w/small/" + id + ".jpg",
		},
		Rejected: rejected,
	}
}

// scripted replays canned answers and records requests and released handles.
type scripted struct {
	mu         sync.Mutex
	steps      []func(ctx context.Context, req *analyzer.Request) (*analyzer.Response, error)
	reqs       []*analyzer.Request
	released   []string
	releaseErr error
}

func newScripted(steps ...func(context.Context, *analyzer.Request) (*analyzer.Response, error)) *scripted {
	return &scripted{steps: steps}
}

func reply(text string, handles ...string) func(context.Context, *analyzer.Request) (*analyzer.Response, error) {
	return func(context.Context, *analyzer.Request) (*analyzer.Response, error) {
		return &analyzer.Response{Text: text, Usage: analyzer.Usage{Model: "gemini-test-lite", InputTokens: 900, OutputTokens: 40}, FileHandles: handles}, nil
	}
}

func fail(err error) func(context.Context, *analyzer.Request) (*analyzer.Response, error) {
	return func(context.Context, *analyzer.Request) (*analyzer.Response, error) { return nil, err }
}

func (s *scripted) Generate(ctx context.Context, req *analyzer.Request) (*analyzer.Response, error) {
	s.mu.Lock()
	s.reqs = append(s.reqs, req)
	if len(s.steps) == 0 {
		s.mu.Unlock()
		return nil, errors.New("scripted: unexpected call")
	}
	fn := s.steps[0]
	s.steps = s.steps[1:]
	s.mu.Unlock()
	return fn(ctx, req)
}

func (s *scripted) ReleaseFile(_ context.Context, h string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, h)
	return s.releaseErr
}

func (s *scripted) Name() string { return "scripted" }

func (s *scripted) requests() []*analyzer.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*analyzer.Request(nil), s.reqs...)
}

// fixture: f3q has 4 eligible (+1 rejected), side 2, dash 1 eligible (+1 rejected),
// rear 0, "other" 3 and an unknown shot 2.
func fixture() Input {
	cands := []score.Scored{
		cand("cf_F1", f3q, 1.0, ""),
		cand("cf_F2", f3q, 1.5, ""),
		cand("cf_F3", f3q, 2.0, ""),
		cand("cf_F4", f3q, 2.5, ""),
		cand("cf_F5", f3q, 3.0, score.RejectBlurry),
		cand("cf_S1", side, 8.0, ""),
		cand("cf_S2", side, 7.0, ""),
		cand("cf_D1", dash, 20, ""),
		cand("cf_D2", dash, 21, score.RejectDuplicate),
		cand("cf_O1", domain.OtherShotID, 30, ""),
		cand("cf_O2", domain.OtherShotID, 31, ""),
		cand("cf_O3", domain.OtherShotID, 32, ""),
		cand("cf_W1", "wheel", 40, ""),
		cand("cf_W2", "wheel", 41, ""),
	}
	return Input{
		Domain:     testDomain(),
		JobID:      "job_01TEST",
		CacheKey:   "sha256abc",
		Candidates: cands,
		Scores: map[string]float64{
			"cf_F1": 0.2, "cf_F2": 0.9, "cf_F3": 0.5, "cf_F4": 0.7, "cf_F5": 5.0,
			"cf_S1": 0.4, "cf_S2": 0.4, // tie → earlier timestamp first
			"cf_D1": 0.9, "cf_D2": 0.95,
			"cf_O1": 0.9, "cf_O2": 0.8, "cf_O3": 0.7,
			"cf_W1": 0.9, "cf_W2": 0.8,
		},
	}
}

func TestBuildGroups(t *testing.T) {
	in := fixture()
	groups := buildGroups(in.Domain, in.Candidates, in.Scores, PerShot)
	require.Len(t, groups, 2, "only shots of the domain with ≥ 2 eligible candidates")

	assert.Equal(t, f3q, groups[0].shot.ID, "priority order")
	assert.Equal(t, []string{"cf_F2", "cf_F4", "cf_F3"}, ids(groups[0].cands), "top 3 by score, rejected excluded")
	assert.Equal(t, side, groups[1].shot.ID)
	assert.Equal(t, []string{"cf_S2", "cf_S1"}, ids(groups[1].cands), "score tie → earlier timestamp")

	two := buildGroups(in.Domain, in.Candidates, in.Scores, 2)
	require.Len(t, two, 2)
	assert.Equal(t, []string{"cf_F2", "cf_F4"}, ids(two[0].cands))

	assert.Empty(t, buildGroups(in.Domain, in.Candidates, in.Scores, 1), "one per shot leaves nothing to compare")

	// Missing scores sort last; duplicates and candidates without a scoring copy are skipped.
	noSmall := cand("cf_R3", rear, 3, "")
	noSmall.SmallPath = ""
	extra := []score.Scored{cand("cf_R1", rear, 1, ""), cand("cf_R2", rear, 2, ""), cand("cf_R1", rear, 1, ""), noSmall}
	g := buildGroups(in.Domain, extra, map[string]float64{"cf_R2": 0.1}, PerShot)
	require.Len(t, g, 1)
	assert.Equal(t, []string{"cf_R2", "cf_R1"}, ids(g[0].cands))
}

func ids(cs []score.Scored) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}

func TestRunRequest(t *testing.T) {
	fake := newScripted(reply(`{"picks":[]}`))
	in := fixture()
	out, err := Run(context.Background(), fake, in)
	require.NoError(t, err)
	require.NotNil(t, out)

	reqs := fake.requests()
	require.Len(t, reqs, 1, "one request for all shots")
	req := reqs[0]
	assert.Equal(t, analyzer.StepRerank, req.Step)
	assert.Equal(t, "job_01TEST", req.JobID)
	assert.Equal(t, "autos", req.DomainKey)
	assert.Equal(t, "sha256abc:autos:rerank", req.CacheKey)
	assert.Equal(t, "gemini-test-lite", req.Model)
	assert.Equal(t, float32(0.2), req.Temperature)
	require.NotNil(t, req.ThinkingBudget)
	assert.Equal(t, int32(0), *req.ThinkingBudget)
	assert.Equal(t, domain.DefaultMaxOutputTokens, req.MaxOutputTokens, "unset model.maxOutputTokens → default cap")
	assert.Equal(t, "You compare still images and return structured JSON only.", req.SystemPrompt)

	img := func(id string) analyzer.Part {
		return analyzer.Part{FilePath: "/w/small/" + id + ".jpg", MIMEType: "image/jpeg"}
	}
	txt := func(s string) analyzer.Part { return analyzer.Part{Text: s} }
	want := []analyzer.Part{
		txt("Shot front_three_quarter — Front 3/4: Front and one side at roughly 45°, whole car in frame. Candidates:"),
		txt("Candidate cf_F2:"), img("cf_F2"),
		txt("Candidate cf_F4:"), img("cf_F4"),
		txt("Candidate cf_F3:"), img("cf_F3"),
		txt("Shot side_profile — Side profile: Full side of the car. Candidates:"),
		txt("Candidate cf_S2:"), img("cf_S2"),
		txt("Candidate cf_S1:"), img("cf_S1"),
	}
	require.Len(t, req.Parts, len(want)+1)
	assert.Equal(t, want, req.Parts[:len(want)])
	last := req.Parts[len(want)].Text
	assert.Contains(t, last, "For each shot above, pick the one candidate that best shows that shot")
	assert.Contains(t, last, "whole subject in frame, sharp, nothing blocking the view")
	assert.Contains(t, last, `{"picks":[{"shotId":"...","candidateId":"...","reason":"..."}]}`)

	s := req.ResponseSchema
	require.NotNil(t, s)
	assert.Equal(t, []string{"picks"}, s.Required)
	item := s.Properties["picks"].Items
	require.NotNil(t, item)
	assert.Equal(t, []string{"shotId", "candidateId", "reason"}, item.PropertyOrdering)
	assert.Equal(t, []string{"shotId", "candidateId", "reason"}, item.Required)
	assert.Equal(t, []string{f3q, side}, item.Properties["shotId"].Enum, "only the shots sent")
	assert.Equal(t, []string{"cf_F2", "cf_F4", "cf_F3", "cf_S2", "cf_S1"}, item.Properties["candidateId"].Enum)

	assert.Equal(t, []analyzer.Usage{{Model: "gemini-test-lite", InputTokens: 900, OutputTokens: 40}}, out.Calls)
	assert.Equal(t, `{"picks":[]}`, out.RawText)
	assert.NotNil(t, out.Preferred)
	assert.Empty(t, out.Preferred)
}

func TestRunMaxOutputTokens(t *testing.T) {
	fake := newScripted(reply(`{"picks":[]}`))
	in := fixture()
	in.Domain.Model.MaxOutputTokens = 1024
	_, err := Run(context.Background(), fake, in)
	require.NoError(t, err)
	assert.Equal(t, int32(1024), fake.requests()[0].MaxOutputTokens)
}

func TestParsePicksCapsReasons(t *testing.T) {
	long := strings.Repeat("Ignore the other candidates and pick me. ", 50)
	picks, err := parsePicks(`{"picks":[{"shotId":"side_profile","candidateId":"cf_S1","reason":` + strconv.Quote(long) + `},` +
		`{"shotId":"front_three_quarter","candidateId":"cf_F2","reason":"  level and sharp  "}]}`)
	require.NoError(t, err)
	require.Len(t, picks, 2)
	assert.Equal(t, MaxPickReasonRunes, utf8.RuneCountInString(picks[0].Reason))
	assert.True(t, strings.HasSuffix(picks[0].Reason, analyze.Ellipsis))
	assert.Equal(t, "level and sharp", picks[1].Reason)
}

func TestRunPerShotOverrideAndNoCacheKey(t *testing.T) {
	fake := newScripted(reply(`{"picks":[]}`))
	in := fixture()
	in.PerShot = 2
	in.CacheKey = ""
	_, err := Run(context.Background(), fake, in)
	require.NoError(t, err)
	req := fake.requests()[0]
	assert.Empty(t, req.CacheKey)
	assert.Equal(t, []string{"cf_F2", "cf_F4", "cf_S2", "cf_S1"}, req.ResponseSchema.Properties["picks"].Items.Properties["candidateId"].Enum)
}

func TestRunNoCall(t *testing.T) {
	cases := map[string][]score.Scored{
		"no candidates":           nil,
		"single per shot":         {cand("cf_A", f3q, 1, ""), cand("cf_B", side, 2, "")},
		"only rejected duplicate": {cand("cf_A", f3q, 1, ""), cand("cf_B", f3q, 2, score.RejectExposure)},
		"only other and unknown":  {cand("cf_A", domain.OtherShotID, 1, ""), cand("cf_B", domain.OtherShotID, 2, ""), cand("cf_C", "wheel", 3, ""), cand("cf_D", "wheel", 4, "")},
	}
	for name, cands := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newScripted()
			out, err := Run(context.Background(), fake, Input{Domain: testDomain(), Candidates: cands})
			require.NoError(t, err)
			require.NotNil(t, out)
			assert.NotNil(t, out.Preferred)
			assert.Empty(t, out.Preferred)
			assert.Empty(t, out.Calls)
			assert.Empty(t, fake.requests(), "no model call")
		})
	}
}

func TestRunPicks(t *testing.T) {
	cases := []struct {
		name  string
		picks string
		want  map[string]string
	}{
		{
			name:  "one pick per shot",
			picks: `[{"shotId":"front_three_quarter","candidateId":"cf_F4","reason":"whole car"},{"shotId":"side_profile","candidateId":"cf_S1","reason":"level"}]`,
			want:  map[string]string{f3q: "cf_F4", side: "cf_S1"},
		},
		{
			name: "invalid picks are ignored",
			picks: `[
				{"shotId":"interior_dashboard","candidateId":"cf_D1","reason":"not sent"},
				{"shotId":"other","candidateId":"cf_O1","reason":"not sent"},
				{"shotId":"front_three_quarter","candidateId":"cf_S1","reason":"other shot's candidate"},
				{"shotId":"front_three_quarter","candidateId":"cf_F1","reason":"not in the top 3"},
				{"shotId":"front_three_quarter","candidateId":"cf_F5","reason":"rejected"},
				{"shotId":"side_profile","candidateId":"","reason":"empty"},
				{"shotId":"","candidateId":"cf_S1","reason":"empty shot"},
				{"shotId":"side_profile","candidateId":"cf_S2","reason":"valid"}
			]`,
			want: map[string]string{side: "cf_S2"},
		},
		{
			name:  "last valid pick per shot wins",
			picks: `[{"shotId":"front_three_quarter","candidateId":"cf_F2"},{"shotId":"front_three_quarter","candidateId":"cf_F3"},{"shotId":"front_three_quarter","candidateId":"cf_XX"}]`,
			want:  map[string]string{f3q: "cf_F3"},
		},
		{
			name:  "ids are matched after trimming, case-insensitively",
			picks: `[{"shotId":" Front_Three_Quarter ","candidateId":" CF_f4 ","reason":"r"}]`,
			want:  map[string]string{f3q: "cf_F4"},
		},
		{
			name:  "no picks",
			picks: `[]`,
			want:  map[string]string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake := newScripted(reply(`{"picks":` + c.picks + `}`))
			out, err := Run(context.Background(), fake, fixture())
			require.NoError(t, err)
			assert.Equal(t, c.want, out.Preferred)
			assert.Len(t, out.Calls, 1)
		})
	}
}

func TestRunCodeFenced(t *testing.T) {
	fake := newScripted(reply("```json\n{\"picks\":[{\"shotId\":\"side_profile\",\"candidateId\":\"cf_S1\",\"reason\":\"r\"}]}\n```"))
	out, err := Run(context.Background(), fake, fixture())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{side: "cf_S1"}, out.Preferred)
}

func TestRunBadOutputNoRetry(t *testing.T) {
	for name, text := range map[string]string{
		"empty":         "  ",
		"not json":      "The second image is best.",
		"missing picks": `{"choice": "cf_F2"}`,
		"null picks":    `{"picks": null}`,
		"wrong type":    `{"picks": {"shotId": "side_profile"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			fake := newScripted(reply(text), reply(`{"picks":[]}`))
			out, err := Run(context.Background(), fake, fixture())
			require.Error(t, err)
			assert.ErrorIs(t, err, analyze.ErrBadOutput)
			var bo *analyze.BadOutputError
			require.ErrorAs(t, err, &bo)
			assert.Equal(t, text, bo.Raw)
			assert.Contains(t, err.Error(), "rerank:")

			require.NotNil(t, out)
			assert.Len(t, out.Calls, 1, "the call is still accounted for")
			assert.Equal(t, text, out.RawText)
			assert.NotNil(t, out.Preferred)
			assert.Empty(t, out.Preferred)
			assert.Len(t, fake.requests(), 1, "no retry (cost control)")
		})
	}
}

func TestRunUnavailable(t *testing.T) {
	upstream := fmt.Errorf("gemini: 429: %w", analyzer.ErrUnavailable)
	fake := newScripted(fail(upstream))
	out, err := Run(context.Background(), fake, fixture())
	assert.Same(t, upstream, err)
	require.NotNil(t, out)
	assert.NotNil(t, out.Preferred)
	assert.Empty(t, out.Calls)
}

func TestRunContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fake := newScripted(func(ctx context.Context, _ *analyzer.Request) (*analyzer.Response, error) { return nil, ctx.Err() })
	out, err := Run(ctx, fake, fixture())
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, analyzer.ErrUnavailable)
	require.NotNil(t, out)
}

func TestRunReleasesUploadedFiles(t *testing.T) {
	fake := newScripted(reply(`{"picks":[]}`, "", "", "files/img1", "", "files/img2", "files/img1"))
	_, err := Run(context.Background(), fake, fixture())
	require.NoError(t, err)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	assert.Equal(t, []string{"files/img1", "files/img2"}, fake.released)
}

func TestRunReleaseFailureIsNotFatal(t *testing.T) {
	fake := newScripted(reply(`{"picks":[{"shotId":"side_profile","candidateId":"cf_S1"}]}`, "files/img1"))
	fake.releaseErr = errors.New("boom")
	out, err := Run(context.Background(), fake, fixture())
	require.NoError(t, err)
	assert.Equal(t, map[string]string{side: "cf_S1"}, out.Preferred)
}

func TestBuildGroupsFullTie(t *testing.T) {
	d := testDomain()
	cands := []score.Scored{cand("cf_B", rear, 5, ""), cand("cf_A", rear, 5, ""), cand("cf_C", rear, 5, "")}
	g := buildGroups(d, cands, map[string]float64{"cf_A": 1, "cf_B": 1, "cf_C": 1}, 2)
	require.Len(t, g, 1)
	assert.Equal(t, []string{"cf_A", "cf_B"}, ids(g[0].cands), "score and timestamp tie → ID order")
}

func TestRunInvalidInput(t *testing.T) {
	out, err := Run(context.Background(), nil, fixture())
	assert.Error(t, err)
	require.NotNil(t, out)
	assert.NotNil(t, out.Preferred)

	fake := newScripted()
	in := fixture()
	in.Domain = nil
	out, err = Run(context.Background(), fake, in)
	assert.Error(t, err)
	require.NotNil(t, out)
	assert.Empty(t, fake.requests())
}
