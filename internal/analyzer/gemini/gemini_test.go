package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/genai"

	"video-to-image/internal/analyzer"
)

const sourceMarker = "SECRET-customer-upload-name-7f3a.mp4"

func analyzeRequest(t *testing.T) *analyzer.Request {
	t.Helper()
	return &analyzer.Request{
		Step: analyzer.StepAnalyze, JobID: "job_01HX", DomainKey: "cars", CacheKey: "sha:cars:analyze",
		Model: "gemini-3.5-flash-lite", Temperature: 0.2, ThinkingBudget: genai.Ptr[int32](0),
		MaxOutputTokens: 4096,
		SystemPrompt:    "You analyze short videos and return structured JSON only.",
		Parts: []analyzer.Part{
			{FilePath: writeFile(t, "preview.mp4", 2048), MIMEType: "video/mp4", VideoFPS: 2},
			{Text: "Step 1 — Gate."},
		},
		ResponseSchema: &analyzer.Schema{
			Type:             "object",
			Properties:       map[string]*analyzer.Schema{"ok": {Type: "boolean"}},
			PropertyOrdering: []string{"ok"},
			Required:         []string{"ok"},
		},
		DurationSec: 42, ShotIDs: []string{"front"},
		SourceName: sourceMarker,
	}
}

func TestGenerateUploadsPollsAndBuildsRequest(t *testing.T) {
	fake := &fakeAPI{
		onUpload: func(n int, _, mimeType, _ string) (*genai.File, error) {
			return &genai.File{Name: "files/vid1", MIMEType: mimeType, State: genai.FileStateProcessing}, nil
		},
		onGet: func(n int, name string) (*genai.File, error) {
			if n < 3 {
				return &genai.File{Name: name, State: genai.FileStateProcessing}, nil
			}
			return activeFile(name, "video/mp4"), nil
		},
	}
	c := testClient(fake)
	req := analyzeRequest(t)
	resp, err := c.Generate(context.Background(), req)
	require.NoError(t, err)

	uploads, gets, deletes, generates := fake.counts()
	assert.Equal(t, 1, uploads)
	assert.Equal(t, 3, gets, "polled until ACTIVE")
	assert.Zero(t, deletes, "the caller owns the handle after success")
	assert.Equal(t, 1, generates)
	assert.Equal(t, uploadRec{req.Parts[0].FilePath, "video/mp4", "v2i-analyze-job_01HX"}, fake.uploads[0])

	// Response mapping.
	assert.Equal(t, `{"ok":true}`, resp.Text)
	assert.Equal(t, []string{"files/vid1", ""}, resp.FileHandles)
	assert.Equal(t, "gemini-3.5-flash-lite-001", resp.Usage.Model)
	assert.Equal(t, 12000, resp.Usage.InputTokens)
	assert.Equal(t, 800, resp.Usage.OutputTokens)
	assert.Equal(t, 40, resp.Usage.ThinkingTokens)
	assert.Positive(t, resp.Usage.Latency)

	// Request shape.
	g := fake.generates[0]
	assert.Equal(t, "gemini-3.5-flash-lite", g.model)
	require.Len(t, g.contents, 1)
	assert.Equal(t, genai.RoleUser, g.contents[0].Role)
	parts := g.contents[0].Parts
	require.Len(t, parts, 2)
	require.NotNil(t, parts[0].FileData)
	assert.Equal(t, "https://fake.test/v1beta/files/vid1", parts[0].FileData.FileURI)
	assert.Equal(t, "video/mp4", parts[0].FileData.MIMEType)
	require.NotNil(t, parts[0].VideoMetadata)
	require.NotNil(t, parts[0].VideoMetadata.FPS)
	assert.Equal(t, 2.0, *parts[0].VideoMetadata.FPS)
	assert.Equal(t, "Step 1 — Gate.", parts[1].Text)

	cfg := g.cfg
	require.NotNil(t, cfg.SystemInstruction)
	assert.Equal(t, req.SystemPrompt, cfg.SystemInstruction.Parts[0].Text)
	require.NotNil(t, cfg.Temperature)
	assert.Equal(t, float32(0.2), *cfg.Temperature)
	assert.Equal(t, "application/json", cfg.ResponseMIMEType)
	require.NotNil(t, cfg.ResponseSchema)
	assert.Equal(t, genai.TypeObject, cfg.ResponseSchema.Type)
	require.NotNil(t, cfg.ThinkingConfig)
	require.NotNil(t, cfg.ThinkingConfig.ThinkingBudget)
	assert.Equal(t, int32(0), *cfg.ThinkingConfig.ThinkingBudget)
	assert.Equal(t, int32(4096), cfg.MaxOutputTokens, "the answer is capped")

	assertNoSourceName(t, fake)
}

// assertNoSourceName checks that nothing handed to the SDK carries Request.SourceName.
func assertNoSourceName(t *testing.T, fake *fakeAPI) {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, u := range fake.uploads {
		assert.NotContains(t, u.display+u.path+u.mime, "SECRET")
	}
	for _, g := range fake.generates {
		b, err := json.Marshal(struct {
			C []*genai.Content
			G *genai.GenerateContentConfig
		}{g.contents, g.cfg})
		require.NoError(t, err)
		assert.NotContains(t, string(b), "SECRET", "SourceName leaked into the model request")
	}
}

func TestGenerateMinimalConfig(t *testing.T) {
	fake := &fakeAPI{}
	c := testClient(fake)
	_, err := c.Generate(context.Background(), &analyzer.Request{
		Model: "m", Parts: []analyzer.Part{{Text: "hi"}},
	})
	require.NoError(t, err)
	cfg := fake.generates[0].cfg
	assert.Nil(t, cfg.SystemInstruction)
	assert.Empty(t, cfg.ResponseMIMEType)
	assert.Nil(t, cfg.ResponseSchema)
	assert.Nil(t, cfg.ThinkingConfig, "nil ThinkingBudget → provider default")
	assert.Zero(t, cfg.MaxOutputTokens, "0 → provider default")
	require.NotNil(t, cfg.Temperature)
	assert.Zero(t, *cfg.Temperature)
}

// An answer cut off at the output token limit is returned as is (the pipeline's JSON
// validation rejects it) and logged with the limit, so bad output can be diagnosed.
func TestGenerateTruncatedAtMaxTokens(t *testing.T) {
	var logs bytes.Buffer
	fake := &fakeAPI{onGenerate: func(context.Context, int, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		r := okResponse(`{"gate":{"passed":true,"confidence":0.9},"videoSummary":"aaaa`)
		r.Candidates[0].FinishReason = genai.FinishReasonMaxTokens
		r.UsageMetadata.CandidatesTokenCount = 4096
		return r, nil
	}}
	c := testClient(fake, func(o *Options) { o.Logger = slog.New(slog.NewJSONHandler(&logs, nil)) })
	resp, err := c.Generate(context.Background(), analyzeRequest(t))
	require.NoError(t, err, "a truncated answer is bad output for the pipeline, not a provider error")
	assert.Equal(t, `{"gate":{"passed":true,"confidence":0.9},"videoSummary":"aaaa`, resp.Text)
	assert.Equal(t, 4096, resp.Usage.OutputTokens)
	_, _, _, generates := fake.counts()
	assert.Equal(t, 1, generates, "not retried here; the pipeline nudges")

	var entry map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		var e map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &e))
		if e["msg"] == "gemini: answer cut off at the output token limit" {
			entry = e
		}
	}
	require.NotNil(t, entry, "MAX_TOKENS is logged explicitly; logs: %s", logs.String())
	assert.Equal(t, "WARN", entry["level"])
	assert.Equal(t, "MAX_TOKENS", entry["finish_reason"])
	assert.Equal(t, 4096.0, entry["max_output_tokens"])
	assert.Equal(t, 4096.0, entry["output_tokens"])
	assert.Equal(t, "job_01HX", entry["job_id"])
}

func TestGenerateReusesActiveHandle(t *testing.T) {
	fake := &fakeAPI{}
	c := testClient(fake)
	req := analyzeRequest(t)
	req.Parts[0].FileHandle = "files/earlier"
	resp, err := c.Generate(context.Background(), req)
	require.NoError(t, err)

	uploads, gets, deletes, _ := fake.counts()
	assert.Zero(t, uploads, "an ACTIVE handle must not be re-uploaded")
	assert.Equal(t, 1, gets)
	assert.Zero(t, deletes)
	assert.Equal(t, []string{"files/earlier"}, fake.gets)
	assert.Equal(t, "files/earlier", resp.FileHandles[0])
	assert.Equal(t, "https://fake.test/v1beta/files/earlier", fake.generates[0].contents[0].Parts[0].FileData.FileURI)
}

func TestGenerateHandleOnlyPart(t *testing.T) {
	fake := &fakeAPI{onGet: func(_ int, name string) (*genai.File, error) { return activeFile(name, "video/webm"), nil }}
	c := testClient(fake)
	resp, err := c.Generate(context.Background(), &analyzer.Request{
		Model: "m", Parts: []analyzer.Part{{FileHandle: "files/h"}, {Text: "go"}},
	})
	require.NoError(t, err)
	assert.Equal(t, "video/webm", fake.generates[0].contents[0].Parts[0].FileData.MIMEType, "MIME falls back to the file's")
	assert.Equal(t, []string{"files/h", ""}, resp.FileHandles)

	// A dead handle without a local file cannot be recovered.
	fake = &fakeAPI{onGet: func(int, string) (*genai.File, error) { return nil, apiErr(404, "NOT_FOUND") }}
	_, err = testClient(fake).Generate(context.Background(), &analyzer.Request{
		Model: "m", Parts: []analyzer.Part{{FileHandle: "files/gone", MIMEType: "video/mp4"}},
	})
	require.Error(t, err)
	assert.False(t, errors.Is(err, analyzer.ErrUnavailable))
	assert.Contains(t, err.Error(), "no FilePath")
}

func TestGenerateReuploadsStaleHandle(t *testing.T) {
	for _, code := range []int{403, 404} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			fake := &fakeAPI{onGet: func(n int, name string) (*genai.File, error) {
				return nil, apiErr(code, "gone")
			}}
			c := testClient(fake)
			req := analyzeRequest(t)
			req.Parts[0].FileHandle = "files/expired"
			resp, err := c.Generate(context.Background(), req)
			require.NoError(t, err)
			uploads, _, deletes, _ := fake.counts()
			assert.Equal(t, 1, uploads)
			assert.Zero(t, deletes)
			assert.Equal(t, "files/up1", resp.FileHandles[0])
		})
	}
}

func TestGenerateReplacesFailedHandle(t *testing.T) {
	fake := &fakeAPI{onGet: func(_ int, name string) (*genai.File, error) {
		return &genai.File{Name: name, State: genai.FileStateFailed, Error: &genai.FileStatus{Message: "bad video"}}, nil
	}}
	c := testClient(fake)
	req := analyzeRequest(t)
	req.Parts[0].FileHandle = "files/broken"
	resp, err := c.Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, []string{"files/broken"}, fake.deleted(), "the replaced FAILED file is deleted")
	assert.Equal(t, "files/up1", resp.FileHandles[0])
}

func TestGenerateWaitsForCallersProcessingHandle(t *testing.T) {
	fake := &fakeAPI{onGet: func(n int, name string) (*genai.File, error) {
		if n < 3 {
			return &genai.File{Name: name, State: genai.FileStateProcessing}, nil
		}
		return activeFile(name, "video/mp4"), nil
	}}
	req := analyzeRequest(t)
	req.Parts[0].FileHandle = "files/inflight"
	resp, err := testClient(fake).Generate(context.Background(), req)
	require.NoError(t, err)
	uploads, gets, _, _ := fake.counts()
	assert.Zero(t, uploads)
	assert.Equal(t, 3, gets)
	assert.Equal(t, "files/inflight", resp.FileHandles[0])
}

func TestGenerateProcessingFailedRetriesWithFreshUpload(t *testing.T) {
	fake := &fakeAPI{
		onUpload: func(n int, _, mimeType, _ string) (*genai.File, error) {
			return &genai.File{Name: fmt.Sprintf("files/up%d", n), MIMEType: mimeType, State: genai.FileStateProcessing}, nil
		},
		onGet: func(_ int, name string) (*genai.File, error) {
			if name == "files/up1" {
				return &genai.File{Name: name, State: genai.FileStateFailed}, nil
			}
			return activeFile(name, "video/mp4"), nil
		},
	}
	resp, err := testClient(fake).Generate(context.Background(), analyzeRequest(t))
	require.NoError(t, err)
	uploads, _, _, generates := fake.counts()
	assert.Equal(t, 2, uploads)
	assert.Equal(t, 1, generates)
	assert.Equal(t, []string{"files/up1"}, fake.deleted(), "the FAILED upload is deleted")
	assert.Equal(t, "files/up2", resp.FileHandles[0])
}

func TestGenerateFileNeverActive(t *testing.T) {
	fake := &fakeAPI{
		onUpload: func(_ int, _, mimeType, _ string) (*genai.File, error) {
			return &genai.File{Name: "files/slow", MIMEType: mimeType, State: genai.FileStateProcessing}, nil
		},
		onGet: func(_ int, name string) (*genai.File, error) {
			return &genai.File{Name: name, State: genai.FileStateProcessing}, nil
		},
	}
	c := testClient(fake, func(o *Options) { o.FileActiveTimeout = 20 * time.Millisecond })
	_, err := c.Generate(context.Background(), analyzeRequest(t))
	require.Error(t, err)
	assert.ErrorIs(t, err, analyzer.ErrUnavailable)
	assert.Contains(t, err.Error(), "not ACTIVE after")
	uploads, _, _, generates := fake.counts()
	assert.Equal(t, 1, uploads, "a processing timeout is not retried")
	assert.Zero(t, generates)
	assert.Equal(t, []string{"files/slow"}, fake.deleted(), "the orphaned upload is deleted")
}

func TestGenerateRetriesTransientErrors(t *testing.T) {
	tests := map[string]error{
		"503":               apiErr(503, "UNAVAILABLE"),
		"500":               apiErr(500, "INTERNAL"),
		"429":               apiErr(429, "RESOURCE_EXHAUSTED"),
		"408":               apiErr(408, "timeout"),
		"pointer APIError":  &genai.APIError{Code: 502},
		"url.Error":         fmt.Errorf("doRequest: error sending request: %w", &url.Error{Op: "Post", URL: "https://x", Err: errors.New("connection reset by peer")}),
		"net timeout":       &net.OpError{Op: "read", Err: timeoutErr{}},
		"unexpected EOF":    io.ErrUnexpectedEOF,
		"request deadline":  fmt.Errorf("per-request timeout: %w", context.DeadlineExceeded),
		"garbled response":  errors.New("deserializeUnaryResponse: error unmarshalling response"),
		"upload status 503": errors.New("Failed to create file. Ran into an error: Error 503, Message: busy, Status: UNAVAILABLE, Details: []"),
	}
	for name, transient := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAPI{onGenerate: func(_ context.Context, n int, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
				if n <= 2 {
					return nil, transient
				}
				return okResponse("third time lucky"), nil
			}}
			resp, err := testClient(fake).Generate(context.Background(), analyzeRequest(t))
			require.NoError(t, err)
			assert.Equal(t, "third time lucky", resp.Text)
			uploads, _, deletes, generates := fake.counts()
			assert.Equal(t, 3, generates)
			assert.Equal(t, 1, uploads, "retries reuse the uploaded file")
			assert.Zero(t, deletes)
		})
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestGenerateGivesUpAfterRetries(t *testing.T) {
	tests := []struct {
		maxRetries int
		wantCalls  int
	}{
		{0, 3}, // default: 2 retries
		{4, 5},
		{-1, 1},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.maxRetries), func(t *testing.T) {
			fake := &fakeAPI{onGenerate: func(context.Context, int, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
				return nil, apiErr(503, "UNAVAILABLE")
			}}
			c := testClient(fake, func(o *Options) { o.MaxRetries = tt.maxRetries })
			resp, err := c.Generate(context.Background(), analyzeRequest(t))
			assert.Nil(t, resp)
			require.Error(t, err)
			assert.ErrorIs(t, err, analyzer.ErrUnavailable)
			ae, ok := apiError(err)
			require.True(t, ok, "the SDK error stays inspectable")
			assert.Equal(t, 503, ae.Code)
			assert.Contains(t, err.Error(), fmt.Sprintf("giving up after %d attempts", tt.wantCalls))
			uploads, _, _, generates := fake.counts()
			assert.Equal(t, tt.wantCalls, generates)
			assert.Equal(t, 1, uploads)
			assert.Equal(t, []string{"files/up1"}, fake.deleted(), "uploads of a failed call are deleted")
		})
	}
}

func TestGenerateRejectedRequestsAreNotRetried(t *testing.T) {
	for _, code := range []int{400, 401, 403, 404, 413} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			fake := &fakeAPI{onGenerate: func(context.Context, int, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
				return nil, apiErr(code, "INVALID_ARGUMENT")
			}}
			_, err := testClient(fake).Generate(context.Background(), analyzeRequest(t))
			require.Error(t, err)
			assert.ErrorIs(t, err, analyzer.ErrUnavailable, "a rejected request fails the job as MODEL_UNAVAILABLE")
			assert.Contains(t, err.Error(), fmt.Sprintf("request rejected (HTTP %d INVALID_ARGUMENT)", code))
			_, _, _, generates := fake.counts()
			assert.Equal(t, 1, generates)
		})
	}
}

func TestGenerateUploadErrors(t *testing.T) {
	t.Run("transient create error is retried", func(t *testing.T) {
		fake := &fakeAPI{onUpload: func(n int, _, mimeType, _ string) (*genai.File, error) {
			if n == 1 {
				// Files.Upload formats the create error with %s, dropping the APIError type.
				return nil, errors.New("upload: Failed to create file. Ran into an error: Error 429, Message: quota, Status: RESOURCE_EXHAUSTED, Details: []")
			}
			return activeFile("files/ok", mimeType), nil
		}}
		resp, err := testClient(fake).Generate(context.Background(), analyzeRequest(t))
		require.NoError(t, err)
		assert.Equal(t, "files/ok", resp.FileHandles[0])
	})
	t.Run("rejected create error is not retried", func(t *testing.T) {
		fake := &fakeAPI{onUpload: func(int, string, string, string) (*genai.File, error) {
			return nil, errors.New("upload: Failed to create file. Ran into an error: Error 400, Message: bad mime, Status: INVALID_ARGUMENT, Details: []")
		}}
		_, err := testClient(fake).Generate(context.Background(), analyzeRequest(t))
		assert.ErrorIs(t, err, analyzer.ErrUnavailable)
		uploads, _, _, _ := fake.counts()
		assert.Equal(t, 1, uploads)
	})
	t.Run("local file error is not ErrUnavailable", func(t *testing.T) {
		fake := &fakeAPI{onUpload: func(int, string, string, string) (*genai.File, error) {
			return nil, localErrorf("open media file: %w", os.ErrPermission)
		}}
		_, err := testClient(fake).Generate(context.Background(), analyzeRequest(t))
		require.Error(t, err)
		assert.False(t, errors.Is(err, analyzer.ErrUnavailable))
		assert.ErrorIs(t, err, os.ErrPermission)
	})
	t.Run("upload without a name", func(t *testing.T) {
		fake := &fakeAPI{onUpload: func(n int, _, mimeType, _ string) (*genai.File, error) {
			if n == 1 {
				return &genai.File{}, nil
			}
			return activeFile("files/ok", mimeType), nil
		}}
		resp, err := testClient(fake).Generate(context.Background(), analyzeRequest(t))
		require.NoError(t, err)
		assert.Equal(t, "files/ok", resp.FileHandles[0])
	})
}

func TestGenerateContextCancellation(t *testing.T) {
	t.Run("already canceled", func(t *testing.T) {
		fake := &fakeAPI{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := testClient(fake).Generate(ctx, analyzeRequest(t))
		assert.Equal(t, context.Canceled, err)
		u, g, d, gen := fake.counts()
		assert.Zero(t, u+g+d+gen)
	})
	t.Run("canceled during generate", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fake := &fakeAPI{onGenerate: func(ctx context.Context, _ int, _ []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
			cancel()
			<-ctx.Done()
			return nil, fmt.Errorf("doRequest: error sending request: %w", ctx.Err())
		}}
		_, err := testClient(fake).Generate(ctx, analyzeRequest(t))
		assert.Equal(t, context.Canceled, err, "context errors are returned unwrapped")
		assert.Equal(t, []string{"files/up1"}, fake.deleted(), "cleanup runs even though ctx is canceled")
	})
	t.Run("canceled during backoff", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		fake := &fakeAPI{onGenerate: func(context.Context, int, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
			time.AfterFunc(10*time.Millisecond, cancel)
			return nil, apiErr(503, "UNAVAILABLE")
		}}
		c := testClient(fake, func(o *Options) { o.BackoffBase, o.MaxBackoff = time.Hour, time.Hour })
		start := time.Now()
		_, err := c.Generate(ctx, analyzeRequest(t))
		assert.Equal(t, context.Canceled, err)
		assert.Less(t, time.Since(start), 5*time.Second)
		_, _, _, generates := fake.counts()
		assert.Equal(t, 1, generates)
	})
	t.Run("deadline while polling", func(t *testing.T) {
		fake := &fakeAPI{
			onUpload: func(_ int, _, mimeType, _ string) (*genai.File, error) {
				return &genai.File{Name: "files/p", MIMEType: mimeType, State: genai.FileStateProcessing}, nil
			},
			onGet: func(_ int, name string) (*genai.File, error) {
				return &genai.File{Name: name, State: genai.FileStateProcessing}, nil
			},
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		c := testClient(fake, func(o *Options) { o.FileActiveTimeout = time.Hour })
		_, err := c.Generate(ctx, analyzeRequest(t))
		assert.Equal(t, context.DeadlineExceeded, err)
		assert.Equal(t, []string{"files/p"}, fake.deleted())
	})
}

func TestGenerateEmptyOrBlockedResponse(t *testing.T) {
	tests := map[string]*genai.GenerateContentResponse{
		"no candidates": {
			PromptFeedback: &genai.GenerateContentResponsePromptFeedback{BlockReason: genai.BlockedReasonSafety},
			UsageMetadata:  &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 900},
		},
		"nil content": {
			Candidates:    []*genai.Candidate{{FinishReason: genai.FinishReasonSafety}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 900},
		},
		"thought only": {
			Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{{Text: "thinking...", Thought: true}}}}},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount: 900, ThoughtsTokenCount: 30,
			},
		},
	}
	for name, r := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAPI{onGenerate: func(context.Context, int, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
				return r, nil
			}}
			resp, err := testClient(fake).Generate(context.Background(), analyzeRequest(t))
			require.NoError(t, err, "an empty answer is bad output for the pipeline, not a provider error")
			assert.Empty(t, resp.Text)
			assert.Equal(t, 900, resp.Usage.InputTokens)
			assert.Equal(t, "gemini-3.5-flash-lite", resp.Usage.Model, "no ModelVersion → requested model")
			_, _, _, generates := fake.counts()
			assert.Equal(t, 1, generates, "not retried here; the pipeline nudges")
		})
	}
}

func TestGenerateJoinsTextParts(t *testing.T) {
	fake := &fakeAPI{onGenerate: func(context.Context, int, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{Content: &genai.Content{Parts: []*genai.Part{
			{Text: "plan", Thought: true}, {Text: `{"a":`}, {Text: `1}`},
		}}}}}, nil
	}}
	resp, err := testClient(fake).Generate(context.Background(), &analyzer.Request{Model: "m", Parts: []analyzer.Part{{Text: "x"}}})
	require.NoError(t, err)
	assert.Equal(t, `{"a":1}`, resp.Text)
	assert.Zero(t, resp.Usage.InputTokens, "missing usage metadata → zero tokens")
}

func jpegFile(t *testing.T, name string, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x ^ y), 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}))
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, buf.Bytes(), 0o600))
	return p
}

func TestGenerateImagesInlineOrUpload(t *testing.T) {
	small := jpegFile(t, "small.jpg", 64, 48)
	smallBytes, err := os.ReadFile(small)
	require.NoError(t, err)
	req := &analyzer.Request{
		Step: analyzer.StepRerank, Model: "m",
		Parts: []analyzer.Part{
			{Text: "Candidate cf_1 for front:"},
			{FilePath: small}, // MIME from the extension
			{Text: "Candidate cf_2 for front:"},
			{FilePath: small, MIMEType: "image/jpeg"},
		},
	}

	t.Run("inline", func(t *testing.T) {
		fake := &fakeAPI{}
		resp, err := testClient(fake).Generate(context.Background(), req)
		require.NoError(t, err)
		uploads, _, _, _ := fake.counts()
		assert.Zero(t, uploads, "small images are sent inline")
		parts := fake.generates[0].contents[0].Parts
		require.Len(t, parts, 4)
		assert.Equal(t, "Candidate cf_1 for front:", parts[0].Text, "part order is kept")
		require.NotNil(t, parts[1].InlineData)
		assert.Equal(t, "image/jpeg", parts[1].InlineData.MIMEType)
		assert.Equal(t, smallBytes, parts[1].InlineData.Data)
		assert.Nil(t, parts[1].VideoMetadata)
		assert.Equal(t, "Candidate cf_2 for front:", parts[2].Text)
		require.NotNil(t, parts[3].InlineData)
		assert.Equal(t, []string{"", "", "", ""}, resp.FileHandles)
	})
	t.Run("too large → Files API", func(t *testing.T) {
		fake := &fakeAPI{}
		c := testClient(fake, func(o *Options) { o.InlineImageMaxBytes = int64(len(smallBytes) - 1) })
		resp, err := c.Generate(context.Background(), req)
		require.NoError(t, err)
		uploads, _, _, _ := fake.counts()
		assert.Equal(t, 2, uploads)
		assert.Equal(t, []string{"", "files/up1", "", "files/up2"}, resp.FileHandles)
		assert.NotNil(t, fake.generates[0].contents[0].Parts[1].FileData)
	})
	t.Run("per-request budget", func(t *testing.T) {
		fake := &fakeAPI{}
		c := testClient(fake)
		c.inlineBudget = int64(len(smallBytes)) // room for one image only
		resp, err := c.Generate(context.Background(), req)
		require.NoError(t, err)
		parts := fake.generates[0].contents[0].Parts
		assert.NotNil(t, parts[1].InlineData, "the first image fits the budget")
		assert.NotNil(t, parts[3].FileData, "the second one is uploaded")
		assert.Equal(t, []string{"", "", "", "files/up1"}, resp.FileHandles)
	})
	t.Run("inlining disabled", func(t *testing.T) {
		fake := &fakeAPI{}
		c := testClient(fake, func(o *Options) { o.InlineImageMaxBytes = -1 })
		_, err := c.Generate(context.Background(), req)
		require.NoError(t, err)
		uploads, _, _, _ := fake.counts()
		assert.Equal(t, 2, uploads)
	})
}

func TestGenerateVideoFPSClamp(t *testing.T) {
	fake := &fakeAPI{}
	req := analyzeRequest(t)
	req.Parts[0].VideoFPS = 60
	_, err := testClient(fake).Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, maxVideoFPS, *fake.generates[0].contents[0].Parts[0].VideoMetadata.FPS)

	fake = &fakeAPI{}
	req = analyzeRequest(t)
	req.Parts[0].VideoFPS = 0
	_, err = testClient(fake).Generate(context.Background(), req)
	require.NoError(t, err)
	assert.Nil(t, fake.generates[0].contents[0].Parts[0].VideoMetadata, "0 → provider default sampling")
}

func TestGenerateInvalidRequests(t *testing.T) {
	video := writeFile(t, "v.mp4", 10)
	tests := map[string]*analyzer.Request{
		"no model":        {Parts: []analyzer.Part{{Text: "x"}}},
		"no parts":        {Model: "m"},
		"empty part":      {Model: "m", Parts: []analyzer.Part{{}}},
		"text and media":  {Model: "m", Parts: []analyzer.Part{{Text: "x", FilePath: video}}},
		"missing file":    {Model: "m", Parts: []analyzer.Part{{FilePath: filepath.Join(t.TempDir(), "nope.mp4")}}},
		"directory":       {Model: "m", Parts: []analyzer.Part{{FilePath: t.TempDir(), MIMEType: "video/mp4"}}},
		"unknown MIME":    {Model: "m", Parts: []analyzer.Part{{FilePath: writeFile(t, "blob.unknownext", 3)}}},
		"empty file":      {Model: "m", Parts: []analyzer.Part{{FilePath: writeFile(t, "empty.jpg", 0)}}},
		"bad schema type": {Model: "m", Parts: []analyzer.Part{{Text: "x"}}, ResponseSchema: &analyzer.Schema{Type: "map"}},
	}
	for name, req := range tests {
		t.Run(name, func(t *testing.T) {
			fake := &fakeAPI{}
			_, err := testClient(fake).Generate(context.Background(), req)
			require.Error(t, err)
			assert.False(t, errors.Is(err, analyzer.ErrUnavailable), "request bugs are not model outages: %v", err)
			assert.True(t, strings.HasPrefix(err.Error(), "gemini: "), err.Error())
			u, g, d, gen := fake.counts()
			assert.Zero(t, u+g+d+gen, "nothing is sent for an invalid request")
		})
	}
	_, err := testClient(&fakeAPI{}).Generate(context.Background(), nil)
	assert.Error(t, err)
}

func TestGenerateConcurrent(t *testing.T) {
	// Every job's first attempt fails with 503; the retry must succeed independently of
	// the other goroutines sharing the client.
	var mu sync.Mutex
	seen := map[string]bool{}
	fake := &fakeAPI{onGenerate: func(_ context.Context, _ int, contents []*genai.Content, _ *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		job := contents[0].Parts[1].Text
		mu.Lock()
		first := !seen[job]
		seen[job] = true
		mu.Unlock()
		if first {
			return nil, apiErr(503, "UNAVAILABLE")
		}
		return okResponse("ok " + job), nil
	}}
	c := testClient(fake)
	video := writeFile(t, "preview.mp4", 128)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Go(func() {
			job := fmt.Sprint("job_", i)
			resp, err := c.Generate(context.Background(), &analyzer.Request{
				Step: analyzer.StepAnalyze, Model: "m", JobID: job,
				Parts: []analyzer.Part{{FilePath: video, MIMEType: "video/mp4"}, {Text: job}},
			})
			if assert.NoError(t, err) {
				assert.Equal(t, "ok "+job, resp.Text)
				assert.NotEmpty(t, resp.FileHandles[0])
			}
		})
	}
	wg.Wait()
	uploads, _, deletes, generates := fake.counts()
	assert.Equal(t, 24, uploads, "one upload per call, reused by its retry")
	assert.Equal(t, 48, generates)
	assert.Zero(t, deletes)
}

func TestUsageModel(t *testing.T) {
	tests := []struct{ requested, reported, want string }{
		{"gemini-3.5-flash-lite", "", "gemini-3.5-flash-lite"},
		{"gemini-3.5-flash-lite", "gemini-3.5-flash-lite-001", "gemini-3.5-flash-lite-001"},
		{"gemini-3.5-flash-lite", "some-new-alias", "gemini-3.5-flash-lite"}, // keep costs priced
		{"unknown-a", "unknown-b", "unknown-b"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, usageModel(tt.requested, tt.reported), "%+v", tt)
	}
}

func TestDisplayName(t *testing.T) {
	assert.Equal(t, "v2i-analyze-job_01HX", displayName(&analyzer.Request{Step: "analyze", JobID: "job_01HX"}))
	assert.Equal(t, "v2i-file", displayName(&analyzer.Request{}))
	assert.Equal(t, "v2i-rerank-jobevil", displayName(&analyzer.Request{Step: "rerank", JobID: "job/../evil"}))
	assert.LessOrEqual(t, len(displayName(&analyzer.Request{Step: "s", JobID: strings.Repeat("x", 500)})), 128)
}

func TestNew(t *testing.T) {
	_, err := New(context.Background(), Options{})
	require.Error(t, err)

	c, err := New(context.Background(), Options{APIKey: "test-key", BaseURL: "http://127.0.0.1:1/"})
	require.NoError(t, err, "construction must not need the network")
	assert.Equal(t, "gemini", c.Name())
	assert.Equal(t, DefaultMaxRetries, c.maxRetries)
	assert.Equal(t, DefaultBackoffBase, c.backoffBase)
	assert.Equal(t, DefaultFileActiveTimeout, c.activeTimeout)
	assert.Equal(t, DefaultPollInterval, c.pollInterval)
	assert.Equal(t, int64(DefaultInlineImageMaxBytes), c.inlineMax)
	var _ analyzer.Sweeper = c
}

// Gemini 3.x flash-lite rejects thinkingBudget 0 with HTTP 400 and needs a level
// (verified against the live API on 2026-09-23); levels go on the wire upper-case.
func TestGenerateThinkingLevel(t *testing.T) {
	fake := &fakeAPI{}
	c := testClient(fake)
	_, err := c.Generate(context.Background(), &analyzer.Request{
		Model: "gemini-3.5-flash-lite", ThinkingLevel: "minimal", Parts: []analyzer.Part{{Text: "hi"}},
	})
	require.NoError(t, err)
	cfg := fake.generates[0].cfg
	require.NotNil(t, cfg.ThinkingConfig)
	assert.Equal(t, genai.ThinkingLevelMinimal, cfg.ThinkingConfig.ThinkingLevel)
	assert.Nil(t, cfg.ThinkingConfig.ThinkingBudget, "never send both")
}
