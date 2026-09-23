package gemini

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/genai"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/pricing"
)

// maxVideoFPS is the highest sampling rate the API accepts (VideoMetadata.FPS is (0, 24]).
const maxVideoFPS = 24.0

type partKind int

const (
	partText   partKind = iota + 1
	partInline          // image bytes sent inline
	partFile            // Files API reference
)

// partState is one request part plus, for file parts, its upload state across attempts.
type partState struct {
	kind partKind
	text string
	data []byte // partInline
	mime string
	fps  float64
	path string // local file to (re-)upload; may be empty when only a handle was given

	name string // Files API name ("files/abc"): the caller's handle or our upload
	uri  string // set once the file is ACTIVE
	ours bool   // uploaded by this Generate call
}

// call is the state of one Generate call, shared by its attempts.
type call struct {
	c        *Client
	req      *analyzer.Request
	log      *slog.Logger
	parts    []partState
	cfg      *genai.GenerateContentConfig
	uploaded []string // files this call uploaded and still owns
}

// Generate performs one model call; see the package documentation for media handling,
// retries and error semantics.
func (c *Client) Generate(ctx context.Context, req *analyzer.Request) (*analyzer.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, errors.New("gemini: nil request")
	}
	step := req.Step
	if step == "" {
		step = "generate"
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, fmt.Errorf("gemini: %s: request has no model", step)
	}
	parts, err := c.planParts(req.Parts)
	if err != nil {
		return nil, fmt.Errorf("gemini: %s: %w", step, err)
	}
	cfg, err := buildConfig(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: %s: %w", step, err)
	}
	k := &call{
		c:     c,
		req:   req,
		log:   c.log.With("job_id", req.JobID, "domain", req.DomainKey, "step", req.Step, "model", req.Model),
		parts: parts,
		cfg:   cfg,
	}
	fail := func(err error) (*analyzer.Response, error) {
		k.releaseUploads(ctx)
		return nil, err
	}

	attempts := c.maxRetries + 1
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 {
			d := c.backoff(attempt-1, retryHint(lastErr))
			k.log.WarnContext(ctx, "gemini: transient error, retrying",
				"attempt", attempt, "attempts", attempts, "backoff", d, "err", lastErr)
			if err := sleepCtx(ctx, d); err != nil {
				return fail(err)
			}
		}
		resp, err := k.attempt(ctx)
		if err == nil {
			if attempt > 1 {
				k.log.InfoContext(ctx, "gemini: call succeeded after retry", "attempt", attempt)
			}
			return resp, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fail(ctxErr)
		}
		switch classify(err) {
		case kindLocal:
			return fail(fmt.Errorf("gemini: %s: %w", step, err))
		case kindFatal:
			what := "unrecoverable error"
			if _, ok := httpStatus(err); ok {
				what = "request rejected (" + describe(err) + ")"
			}
			k.log.ErrorContext(ctx, "gemini: "+what, "err", err)
			return fail(fmt.Errorf("gemini: %s: %s: %w: %w", step, what, analyzer.ErrUnavailable, err))
		}
		lastErr = err
	}
	k.log.ErrorContext(ctx, "gemini: giving up", "attempts", attempts, "err", lastErr)
	return fail(fmt.Errorf("gemini: %s: giving up after %d attempts: %w: %w", step, attempts, analyzer.ErrUnavailable, lastErr))
}

// attempt resolves all file parts to ACTIVE files and calls generateContent once.
func (k *call) attempt(ctx context.Context) (*analyzer.Response, error) {
	for i := range k.parts {
		if k.parts[i].kind == partFile {
			if err := k.resolveFile(ctx, i); err != nil {
				return nil, err
			}
		}
	}
	contents := k.contents()
	start := time.Now()
	resp, err := k.c.api.generate(ctx, k.req.Model, contents, k.cfg)
	latency := time.Since(start)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, transientErrorf("empty generateContent response")
	}
	return k.response(ctx, resp, latency), nil
}

// planParts validates the request parts and decides how each is sent.
func (c *Client) planParts(in []analyzer.Part) ([]partState, error) {
	if len(in) == 0 {
		return nil, localErrorf("request has no parts")
	}
	out := make([]partState, len(in))
	var inlineTotal int64
	for i, p := range in {
		hasMedia := p.FilePath != "" || p.FileHandle != ""
		switch {
		case p.Text != "" && hasMedia:
			return nil, localErrorf("part %d has both text and media", i)
		case p.Text != "":
			out[i] = partState{kind: partText, text: p.Text}
			continue
		case !hasMedia:
			return nil, localErrorf("part %d is empty", i)
		}

		mt := p.MIMEType
		if mt == "" && p.FilePath != "" {
			mt = mime.TypeByExtension(strings.ToLower(filepath.Ext(p.FilePath)))
		}
		if mt != "" {
			if parsed, _, err := mime.ParseMediaType(mt); err == nil {
				mt = parsed
			}
		}
		if mt == "" && p.FileHandle == "" {
			return nil, localErrorf("part %d: unknown MIME type for %s", i, filepath.Base(p.FilePath))
		}
		ps := partState{kind: partFile, mime: mt, path: p.FilePath, name: p.FileHandle}
		if p.VideoFPS > 0 {
			ps.fps = min(p.VideoFPS, maxVideoFPS)
		}
		if p.FilePath != "" && p.FileHandle == "" {
			st, err := os.Stat(p.FilePath)
			if err != nil {
				return nil, localErrorf("part %d: %w", i, err)
			}
			if st.IsDir() {
				return nil, localErrorf("part %d: %s is a directory", i, p.FilePath)
			}
			if st.Size() == 0 {
				return nil, localErrorf("part %d: media file %s is empty", i, filepath.Base(p.FilePath))
			}
			if strings.HasPrefix(mt, "image/") && st.Size() <= c.inlineMax &&
				inlineTotal+st.Size() <= c.inlineBudget {
				data, err := os.ReadFile(p.FilePath)
				if err != nil {
					return nil, localErrorf("part %d: %w", i, err)
				}
				inlineTotal += int64(len(data))
				ps = partState{kind: partInline, data: data, mime: mt}
			}
		}
		out[i] = ps
	}
	return out, nil
}

// buildConfig maps the request options onto GenerateContentConfig.
func buildConfig(req *analyzer.Request) (*genai.GenerateContentConfig, error) {
	cfg := &genai.GenerateContentConfig{Temperature: genai.Ptr(req.Temperature)}
	if req.SystemPrompt != "" {
		cfg.SystemInstruction = &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: req.SystemPrompt}}}
	}
	if req.ResponseSchema != nil {
		s, err := convertSchema(req.ResponseSchema, "$", 0)
		if err != nil {
			return nil, localErrorf("response schema: %w", err)
		}
		cfg.ResponseMIMEType = "application/json"
		cfg.ResponseSchema = s
	}
	switch {
	case req.ThinkingLevel != "":
		// Gemini 3.x uses levels; e.g. flash-lite rejects thinkingBudget 0 with a 400.
		cfg.ThinkingConfig = &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevel(strings.ToUpper(req.ThinkingLevel))}
	case req.ThinkingBudget != nil:
		cfg.ThinkingConfig = &genai.ThinkingConfig{ThinkingBudget: genai.Ptr(*req.ThinkingBudget)}
	}
	if req.MaxOutputTokens > 0 {
		// Bounds what a runaway or prompt-injected answer can cost. A cut-off answer
		// ends with FinishReason MAX_TOKENS (logged in response) and is incomplete JSON,
		// so the pipeline's validation treats it as bad output.
		cfg.MaxOutputTokens = req.MaxOutputTokens
	}
	return cfg, nil
}

// contents builds the single user turn, keeping the request's part order.
func (k *call) contents() []*genai.Content {
	parts := make([]*genai.Part, 0, len(k.parts))
	for _, p := range k.parts {
		switch p.kind {
		case partText:
			parts = append(parts, &genai.Part{Text: p.text})
		case partInline:
			parts = append(parts, &genai.Part{InlineData: &genai.Blob{Data: p.data, MIMEType: p.mime}})
		case partFile:
			gp := &genai.Part{FileData: &genai.FileData{FileURI: p.uri, MIMEType: p.mime}}
			if p.fps > 0 {
				gp.VideoMetadata = &genai.VideoMetadata{FPS: genai.Ptr(p.fps)}
			}
			parts = append(parts, gp)
		}
	}
	return []*genai.Content{{Role: genai.RoleUser, Parts: parts}}
}

// response maps the SDK response. A blocked, empty or truncated answer is not an error:
// it comes back as is (Text "" when there is nothing) so the pipeline's JSON validation
// takes the nudge / bad-output path. An answer cut off at the output token limit
// (FinishReason MAX_TOKENS) is logged as such, so a MODEL_BAD_OUTPUT job can be told
// apart from a model that really wrote invalid JSON.
func (k *call) response(ctx context.Context, resp *genai.GenerateContentResponse, latency time.Duration) *analyzer.Response {
	usage := analyzer.Usage{Model: usageModel(k.req.Model, resp.ModelVersion), Latency: latency}
	if um := resp.UsageMetadata; um != nil {
		usage.InputTokens = int(um.PromptTokenCount)
		usage.OutputTokens = int(um.CandidatesTokenCount)
		usage.ThinkingTokens = int(um.ThoughtsTokenCount)
	}

	text, finish := responseText(resp)
	switch {
	case finish == genai.FinishReasonMaxTokens:
		k.log.WarnContext(ctx, "gemini: answer cut off at the output token limit",
			"finish_reason", finish, "max_output_tokens", k.cfg.MaxOutputTokens,
			"output_tokens", usage.OutputTokens, "thinking_tokens", usage.ThinkingTokens, "bytes", len(text))
	case text == "":
		attrs := []any{"finish_reason", finish, "candidates", len(resp.Candidates)}
		if pf := resp.PromptFeedback; pf != nil && pf.BlockReason != "" {
			attrs = append(attrs, "block_reason", pf.BlockReason)
		}
		k.log.WarnContext(ctx, "gemini: empty model response", attrs...)
	case finish != "" && finish != genai.FinishReasonStop:
		k.log.WarnContext(ctx, "gemini: model stopped early", "finish_reason", finish, "bytes", len(text))
	}

	handles := make([]string, len(k.parts))
	for i, p := range k.parts {
		if p.kind == partFile {
			handles[i] = p.name
		}
	}
	k.uploaded = nil // the caller owns the handles now
	k.log.DebugContext(ctx, "gemini: call done",
		"latency", latency, "input_tokens", usage.InputTokens, "output_tokens", usage.OutputTokens,
		"thinking_tokens", usage.ThinkingTokens, "model_version", resp.ModelVersion)
	return &analyzer.Response{Text: text, Usage: usage, FileHandles: handles}
}

// responseText concatenates the non-thought text parts of the first candidate.
func responseText(resp *genai.GenerateContentResponse) (string, genai.FinishReason) {
	if len(resp.Candidates) == 0 || resp.Candidates[0] == nil {
		return "", ""
	}
	cand := resp.Candidates[0]
	if cand.Content == nil {
		return "", cand.FinishReason
	}
	var b strings.Builder
	for _, p := range cand.Content.Parts {
		if p != nil && !p.Thought {
			b.WriteString(p.Text)
		}
	}
	return b.String(), cand.FinishReason
}

// usageModel prefers the model version the API reports, unless the price table only
// knows the requested name (so a new versioned alias never silently costs $0).
func usageModel(requested, reported string) string {
	if reported == "" {
		return requested
	}
	if _, ok := pricing.Lookup(reported); ok {
		return reported
	}
	if _, ok := pricing.Lookup(requested); ok {
		return requested
	}
	return reported
}
