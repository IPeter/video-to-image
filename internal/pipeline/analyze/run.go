package analyze

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"video-to-image/internal/analyzer"
)

// CallTimeout bounds one model call (plan/03-pipeline.md step 2), including the file
// upload and the provider's own transient-error retries.
const CallTimeout = 120 * time.Second

// errCallTimeout is the cancellation cause of the per-call timeout in Generate.
var errCallTimeout = errors.New("analyze: model call timeout")

// Run performs the combined gate + scenes call and post-processing.
// Errors: wraps analyzer.ErrUnavailable, *BadOutputError (ErrBadOutput), or ctx errors.
//
// Analyzer errors are returned unchanged (a per-call timeout becomes ErrUnavailable,
// see Generate). If the answer is not valid JSON matching the schema, the call is
// retried once with RetryNudge appended, reusing the uploaded file. The returned Output
// is never nil: on error it still carries the usage of every successful call, the raw
// text of the last answer and the provider file handle so the runner can record cost,
// log the output and release the file.
func Run(ctx context.Context, a analyzer.Analyzer, in Input) (*Output, error) {
	out := &Output{FileHandle: in.FileHandle}
	switch {
	case a == nil:
		return out, errors.New("analyze: nil analyzer")
	case in.Domain == nil:
		return out, errors.New("analyze: nil domain")
	case in.PreviewPath == "" && in.FileHandle == "":
		return out, errors.New("analyze: no preview path or file handle")
	}

	req := buildRequest(in)
	resp, err := Generate(ctx, a, req, CallTimeout)
	if err != nil {
		return out, err
	}
	out.record(resp)

	parsed, perr := ParseResponse(resp.Text)
	if perr != nil {
		slog.WarnContext(ctx, "analyze: invalid model output; retrying once with a nudge",
			"job", in.JobID, "domain", in.Domain.Key, "analyzer", a.Name(), "err", perr)
		resp, err = Generate(ctx, a, retryRequest(req, firstHandle(resp.FileHandles)), CallTimeout)
		if err != nil {
			return out, err
		}
		out.record(resp)
		if parsed, perr = ParseResponse(resp.Text); perr != nil {
			return out, &BadOutputError{Raw: resp.Text, Err: perr}
		}
	}

	out.Gate = parsed.Gate
	out.Gate.Confidence = clamp(out.Gate.Confidence, 0, 1)
	out.VideoSummary = parsed.VideoSummary
	out.Accepted = out.Gate.Passed && out.Gate.Confidence >= in.Domain.Gate.MinConfidence
	if out.Accepted {
		out.Scenes, out.Dropped = PostProcess(parsed, in.DurationSec, in.Domain)
	}
	return out, nil
}

// record adds a successful call to the output. The latest non-empty file handle wins:
// if a provider had to re-upload, the new handle is the one to release.
func (o *Output) record(resp *analyzer.Response) {
	o.Calls = append(o.Calls, resp.Usage)
	o.RawText = resp.Text
	if h := firstHandle(resp.FileHandles); h != "" {
		o.FileHandle = h
	}
}

// buildRequest assembles the analyze call: the preview video first, then the prompt.
func buildRequest(in Input) *analyzer.Request {
	d := in.Domain
	system, user := BuildPrompt(d)
	cacheKey := ""
	if in.CacheKey != "" {
		cacheKey = in.CacheKey + ":" + d.Key + ":" + analyzer.StepAnalyze
	}
	return &analyzer.Request{
		Step:            analyzer.StepAnalyze,
		JobID:           in.JobID,
		DomainKey:       d.Key,
		CacheKey:        cacheKey,
		Model:           d.Model.Name,
		Temperature:     d.Model.Temperature,
		ThinkingBudget:  d.Model.ThinkingBudget,
		ThinkingLevel:   d.Model.ThinkingLevel,
		MaxOutputTokens: d.Model.EffectiveMaxOutputTokens(),
		SystemPrompt:    system,
		Parts: []analyzer.Part{
			{
				FilePath:   in.PreviewPath,
				MIMEType:   "video/mp4",
				VideoFPS:   float64(d.Model.PreviewFPS),
				FileHandle: in.FileHandle,
			},
			{Text: user},
		},
		ResponseSchema: ResponseSchema(d),
		DurationSec:    in.DurationSec,
		ShotIDs:        d.ShotIDs(),
		SourceName:     in.SourceName,
	}
}

// retryRequest is req plus the nudge part, with a distinct cache key and every media
// part pointed at the handle the first attempt uploaded (so nothing is re-uploaded).
// req is not modified.
func retryRequest(req *analyzer.Request, handle string) *analyzer.Request {
	r := *req
	if r.CacheKey != "" {
		r.CacheKey += ":retry"
	}
	r.Parts = make([]analyzer.Part, 0, len(req.Parts)+1)
	for _, p := range req.Parts {
		if p.FilePath != "" && handle != "" {
			p.FileHandle = handle
		}
		r.Parts = append(r.Parts, p)
	}
	r.Parts = append(r.Parts, analyzer.Part{Text: RetryNudge})
	r.ShotIDs = append([]string(nil), req.ShotIDs...)
	return &r
}

func firstHandle(hs []string) string {
	for _, h := range hs {
		if h != "" {
			return h
		}
	}
	return ""
}

// Generate performs one model call bounded by timeout (≤ 0 → only ctx bounds it).
// When the per-call timeout — not ctx — expires, the error wraps analyzer.ErrUnavailable
// (and not context.DeadlineExceeded), so the job fails with MODEL_UNAVAILABLE instead of
// being retried as an interrupted job that already spent money. A nil response without
// an error is reported as an error. Shared with the rerank step.
func Generate(ctx context.Context, a analyzer.Analyzer, req *analyzer.Request, timeout time.Duration) (*analyzer.Response, error) {
	callCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeoutCause(ctx, timeout, errCallTimeout)
		defer cancel()
	}
	resp, err := a.Generate(callCtx, req)
	if err != nil {
		if ctx.Err() == nil && errors.Is(context.Cause(callCtx), errCallTimeout) && !errors.Is(err, analyzer.ErrUnavailable) {
			return nil, fmt.Errorf("analyze: %s call timed out after %s (%v): %w", req.Step, timeout, err, analyzer.ErrUnavailable) //nolint:errorlint // must not match context.DeadlineExceeded
		}
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("analyze: %s call: analyzer %q returned no response", req.Step, a.Name())
	}
	return resp, nil
}

// ExtractJSON returns the JSON payload of a model answer: surrounding whitespace and a
// UTF-8 BOM are removed and a Markdown code fence (```json ... ```) is unwrapped, also
// when the model wrote a sentence before or after it. Text whose JSON starts before any
// fence is returned trimmed, so "```" inside a JSON string is left alone.
func ExtractJSON(text string) string {
	s := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "\ufeff"))
	open := strings.Index(s, "```")
	if open < 0 {
		return s
	}
	if j := strings.IndexAny(s, "{["); j >= 0 && j < open {
		return s
	}
	body := s[open+3:]
	// Skip the fence's info string ("json", "JSON", "jsonc", ...).
	body = strings.TrimLeftFunc(body, func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
	})
	if end := strings.LastIndex(body, "```"); end >= 0 {
		body = body[:end]
	}
	return strings.TrimSpace(body)
}

// wireGate/wireResponse decode the model JSON with pointers so a missing field can be
// told apart from false/0.
type wireGate struct {
	Passed          *bool    `json:"passed"`
	Confidence      *float64 `json:"confidence"`
	Reason          string   `json:"reason"`
	DetectedSubject string   `json:"detectedSubject"`
}

type wireResponse struct {
	Gate         *wireGate    `json:"gate"`
	VideoSummary string       `json:"videoSummary"`
	Scenes       []ModelScene `json:"scenes"`
}

// ParseResponse decodes and validates the model's answer (fence-tolerant, see
// ExtractJSON). gate, gate.passed and a finite gate.confidence are required; a missing
// scenes array is treated as empty. Text fields are trimmed and capped with Truncate
// (videoSummary MaxVideoSummaryRunes, gate.reason MaxGateReasonRunes,
// gate.detectedSubject MaxDetectedSubjectRunes). Scene contents are validated later by
// PostProcess, which drops bad scenes instead of failing the call.
func ParseResponse(text string) (*ModelResponse, error) {
	payload := ExtractJSON(text)
	if payload == "" {
		return nil, errors.New("empty response")
	}
	var w wireResponse
	if err := json.Unmarshal([]byte(payload), &w); err != nil {
		return nil, fmt.Errorf("decode json: %w", err)
	}
	switch {
	case w.Gate == nil:
		return nil, errors.New("missing gate")
	case w.Gate.Passed == nil:
		return nil, errors.New("missing gate.passed")
	case w.Gate.Confidence == nil:
		return nil, errors.New("missing gate.confidence")
	case math.IsNaN(*w.Gate.Confidence) || math.IsInf(*w.Gate.Confidence, 0):
		return nil, errors.New("gate.confidence is not a finite number")
	}
	return &ModelResponse{
		Gate: Gate{
			Passed:          *w.Gate.Passed,
			Confidence:      *w.Gate.Confidence,
			Reason:          Truncate(w.Gate.Reason, MaxGateReasonRunes),
			DetectedSubject: Truncate(w.Gate.DetectedSubject, MaxDetectedSubjectRunes),
		},
		VideoSummary: Truncate(w.VideoSummary, MaxVideoSummaryRunes),
		Scenes:       w.Scenes,
	}, nil
}
