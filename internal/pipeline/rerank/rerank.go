package rerank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/pipeline/analyze"
	"video-to-image/internal/pipeline/score"
)

// SystemPrompt is the system instruction of the re-rank call.
const SystemPrompt = "You compare still images and return structured JSON only."

// releaseTimeout bounds the cleanup of files a provider uploaded for the call.
const releaseTimeout = 30 * time.Second

// group is one shot sent to the model with its top candidates, best first.
type group struct {
	shot  domain.Shot
	cands []score.Scored
}

// Run asks the model to pick the best candidate per shot among the top PerShot
// candidates of each shot group. Shots with a single eligible candidate are not sent.
// If no shot has ≥ 2 candidates, no call is made. Invalid picks are ignored. On
// ErrUnavailable / bad JSON the caller should proceed without preferences.
//
// Eligible candidates are not rejected, have a scoring copy and belong to a shot of the
// domain (domain.OtherShotID is never re-ranked). Each group is ordered by in.Scores
// (highest first, missing scores last; ties by timestamp, then ID) and cut to PerShot.
// All groups go into ONE request; there is no retry on bad output (cost control). The
// returned Output is never nil and Preferred is never nil. Errors: analyzer errors
// unchanged (a per-call timeout wraps analyzer.ErrUnavailable), or an error wrapping
// *analyze.BadOutputError (errors.Is(err, analyze.ErrBadOutput)).
func Run(ctx context.Context, a analyzer.Analyzer, in Input) (*Output, error) {
	out := &Output{Preferred: map[string]string{}}
	switch {
	case a == nil:
		return out, errors.New("rerank: nil analyzer")
	case in.Domain == nil:
		return out, errors.New("rerank: nil domain")
	}
	perShot := in.PerShot
	if perShot <= 0 {
		perShot = PerShot
	}
	groups := buildGroups(in.Domain, in.Candidates, in.Scores, perShot)
	if len(groups) == 0 {
		return out, nil
	}

	resp, err := analyze.Generate(ctx, a, buildRequest(in, groups), analyze.CallTimeout)
	if err != nil {
		return out, err
	}
	out.Calls = append(out.Calls, resp.Usage)
	out.RawText = resp.Text
	releaseFiles(ctx, a, in, resp.FileHandles)

	picks, err := parsePicks(resp.Text)
	if err != nil {
		return out, fmt.Errorf("rerank: %w", &analyze.BadOutputError{Raw: resp.Text, Err: err})
	}
	ignored := applyPicks(out.Preferred, groups, picks)
	if ignored > 0 {
		slog.WarnContext(ctx, "rerank: ignored invalid picks", "job", in.JobID, "domain", in.Domain.Key,
			"ignored", ignored, "picks", len(picks), "shots", len(groups))
	}
	return out, nil
}

// buildGroups returns the shots (in domain priority order) that have at least two
// eligible candidates, each with its top perShot candidates, best first.
func buildGroups(d *domain.Config, cands []score.Scored, scores map[string]float64, perShot int) []group {
	byShot := make(map[string][]score.Scored)
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		if c.Rejected != "" || c.ID == "" || c.SmallPath == "" || seen[c.ID] || c.ShotID == domain.OtherShotID {
			continue
		}
		if _, ok := d.ShotByID(c.ShotID); !ok {
			continue
		}
		seen[c.ID] = true
		byShot[c.ShotID] = append(byShot[c.ShotID], c)
	}

	scoreOf := func(id string) float64 {
		if s, ok := scores[id]; ok && !math.IsNaN(s) {
			return s
		}
		return math.Inf(-1)
	}

	var groups []group
	for _, s := range d.Shots {
		g := byShot[s.ID]
		sort.SliceStable(g, func(i, j int) bool {
			si, sj := scoreOf(g[i].ID), scoreOf(g[j].ID)
			switch {
			case si != sj:
				return si > sj
			case g[i].TimestampSec != g[j].TimestampSec:
				return g[i].TimestampSec < g[j].TimestampSec
			default:
				return g[i].ID < g[j].ID
			}
		})
		if len(g) > perShot {
			g = g[:perShot]
		}
		if len(g) < 2 {
			continue
		}
		groups = append(groups, group{shot: s, cands: g})
	}
	return groups
}

// buildRequest assembles one call: per shot a header, then "Candidate <id>:" + image
// for each candidate, then the instruction.
func buildRequest(in Input, groups []group) *analyzer.Request {
	d := in.Domain
	cacheKey := ""
	if in.CacheKey != "" {
		cacheKey = in.CacheKey + ":" + d.Key + ":" + analyzer.StepRerank
	}

	var parts []analyzer.Part
	shotIDs := make([]string, 0, len(groups))
	candIDs := make([]string, 0, len(groups)*PerShot)
	for _, g := range groups {
		shotIDs = append(shotIDs, g.shot.ID)
		desc := strings.TrimRight(strings.Join(strings.Fields(g.shot.Description), " "), ". ")
		parts = append(parts, analyzer.Part{Text: fmt.Sprintf("Shot %s — %s: %s. Candidates:", g.shot.ID, g.shot.Label, desc)})
		for _, c := range g.cands {
			candIDs = append(candIDs, c.ID)
			parts = append(parts,
				analyzer.Part{Text: fmt.Sprintf("Candidate %s:", c.ID)},
				analyzer.Part{FilePath: c.SmallPath, MIMEType: "image/jpeg"},
			)
		}
	}
	parts = append(parts, analyzer.Part{Text: instruction})

	return &analyzer.Request{
		Step:            analyzer.StepRerank,
		JobID:           in.JobID,
		DomainKey:       d.Key,
		CacheKey:        cacheKey,
		Model:           d.Model.Name,
		Temperature:     d.Model.Temperature,
		ThinkingBudget:  d.Model.ThinkingBudget,
		ThinkingLevel:   d.Model.ThinkingLevel,
		MaxOutputTokens: d.Model.EffectiveMaxOutputTokens(),
		SystemPrompt:    SystemPrompt,
		Parts:           parts,
		ResponseSchema:  responseSchema(shotIDs, candIDs),
		ShotIDs:         d.ShotIDs(),
	}
}

// instruction closes the re-rank prompt.
const instruction = "For each shot above, pick the one candidate that best shows that shot: the whole subject in " +
	"frame, sharp, nothing blocking the view, and lighting that shows it honestly. Only choose a candidate listed " +
	"under that shot. Return JSON only, in the form " +
	`{"picks":[{"shotId":"...","candidateId":"...","reason":"..."}]}` +
	", with one pick per shot and a one-sentence reason."

// responseSchema constrains shotId to the shots sent and candidateId to the candidates
// sent, so the model cannot invent IDs.
func responseSchema(shotIDs, candIDs []string) *analyzer.Schema {
	pickOrder := []string{"shotId", "candidateId", "reason"}
	return &analyzer.Schema{
		Type: "object",
		Properties: map[string]*analyzer.Schema{
			"picks": {
				Type:        "array",
				Description: "one pick per shot",
				Items: &analyzer.Schema{
					Type: "object",
					Properties: map[string]*analyzer.Schema{
						"shotId":      {Type: "string", Description: "the shot being decided", Enum: shotIDs},
						"candidateId": {Type: "string", Description: "the chosen candidate, listed under that shot", Enum: candIDs},
						"reason":      {Type: "string", Description: "one sentence on why this candidate is best"},
					},
					PropertyOrdering: pickOrder,
					Required:         pickOrder,
				},
			},
		},
		PropertyOrdering: []string{"picks"},
		Required:         []string{"picks"},
	}
}

// parsePicks decodes the answer (fence-tolerant). A missing or null "picks" is invalid.
// Each reason is trimmed and capped at MaxPickReasonRunes (model text is untrusted).
func parsePicks(text string) ([]ModelPick, error) {
	payload := analyze.ExtractJSON(text)
	if payload == "" {
		return nil, errors.New("empty response")
	}
	var w struct {
		Picks *[]ModelPick `json:"picks"`
	}
	if err := json.Unmarshal([]byte(payload), &w); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	if w.Picks == nil {
		return nil, errors.New("missing picks")
	}
	picks := *w.Picks
	for i := range picks {
		picks[i].Reason = analyze.Truncate(picks[i].Reason, MaxPickReasonRunes)
	}
	return picks, nil
}

// applyPicks records valid picks into preferred (last valid pick per shot wins) and
// returns how many were ignored. Shot IDs match case-insensitively after trimming;
// candidate IDs match exactly, or case-insensitively (ULIDs are case-insensitive).
func applyPicks(preferred map[string]string, groups []group, picks []ModelPick) (ignored int) {
	byShot := make(map[string]*group, len(groups))
	for i := range groups {
		byShot[strings.ToLower(groups[i].shot.ID)] = &groups[i]
	}
	for _, p := range picks {
		g, ok := byShot[strings.ToLower(strings.TrimSpace(p.ShotID))]
		if !ok {
			ignored++
			continue
		}
		id, ok := findCandidate(g.cands, strings.TrimSpace(p.CandidateID))
		if !ok {
			ignored++
			continue
		}
		preferred[g.shot.ID] = id
	}
	return ignored
}

func findCandidate(cands []score.Scored, id string) (string, bool) {
	if id == "" {
		return "", false
	}
	for _, c := range cands {
		if c.ID == id {
			return c.ID, true
		}
	}
	for _, c := range cands {
		if strings.EqualFold(c.ID, id) {
			return c.ID, true
		}
	}
	return "", false
}

// releaseFiles deletes any files the provider uploaded for this call. Nothing reuses
// them (there is no retry) and Output has no field to hand them to the runner.
func releaseFiles(ctx context.Context, a analyzer.Analyzer, in Input, handles []string) {
	var todo []string
	seen := map[string]bool{}
	for _, h := range handles {
		if h != "" && !seen[h] {
			seen[h] = true
			todo = append(todo, h)
		}
	}
	if len(todo) == 0 {
		return
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	for _, h := range todo {
		if err := a.ReleaseFile(rctx, h); err != nil {
			slog.WarnContext(ctx, "rerank: release model file", "job", in.JobID, "handle", h, "err", err)
		}
	}
}
