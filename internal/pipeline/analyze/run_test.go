package analyze

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
)

const goodJSON = `{
  "gate": {"passed": true, "confidence": 0.94, "reason": " A silver sedan on a lot. ", "detectedSubject": "car"},
  "videoSummary": "Walkaround of a silver sedan, exterior then interior.",
  "scenes": [
    {"start": "00:20", "end": "00:24", "shotId": "interior_dashboard", "reason": "Dashboard in view.", "relevance": 0.8},
    {"start": "00:04", "end": "00:09", "shotId": "front_three_quarter", "reason": "Hero angle.", "relevance": 0.9},
    {"start": "00:12", "end": "00:15", "shotId": "wheel", "reason": "Wheel close-up.", "relevance": 0.4},
    {"start": "02:00", "end": "02:05", "shotId": "side_profile", "reason": "Past the end.", "relevance": 0.7}
  ]
}`

func testInput() Input {
	return Input{
		Domain:      testDomain(),
		JobID:       "job_01TEST",
		PreviewPath: "/scratch/job_01TEST/preview.mp4",
		DurationSec: 60,
		CacheKey:    "sha256abc",
		SourceName:  "walkaround.mp4",
	}
}

func TestRunHappyPath(t *testing.T) {
	fake := newScripted(answer(goodJSON, "files/abc", 1200, 300))
	out, err := Run(context.Background(), fake, testInput())
	require.NoError(t, err)
	require.NotNil(t, out)

	assert.True(t, out.Accepted)
	assert.Equal(t, Gate{Passed: true, Confidence: 0.94, Reason: "A silver sedan on a lot.", DetectedSubject: "car"}, out.Gate)
	assert.Equal(t, "Walkaround of a silver sedan, exterior then interior.", out.VideoSummary)
	assert.Equal(t, goodJSON, out.RawText)
	assert.Equal(t, "files/abc", out.FileHandle)
	assert.Equal(t, []analyzer.Usage{{Model: "gemini-test-lite", InputTokens: 1200, OutputTokens: 300}}, out.Calls)

	require.Len(t, out.Scenes, 3)
	assert.Equal(t, Scene{Idx: 0, StartSec: 4, EndSec: 9, ShotID: f3q, Relevance: 0.9, Reason: "Hero angle.", RawStart: "00:04", RawEnd: "00:09"}, out.Scenes[0])
	assert.Equal(t, Scene{Idx: 1, StartSec: 12, EndSec: 15, ShotID: other, Relevance: 0.4, Reason: "Wheel close-up.", RawStart: "00:12", RawEnd: "00:15"}, out.Scenes[1])
	assert.Equal(t, Scene{Idx: 2, StartSec: 20, EndSec: 24, ShotID: dash, Relevance: 0.8, Reason: "Dashboard in view.", RawStart: "00:20", RawEnd: "00:24"}, out.Scenes[2])
	assert.Equal(t, []Drop{
		drop("00:12", "00:15", "wheel", DropUnknownShot),
		drop("02:00", "02:05", side, DropOutOfRange),
	}, out.Dropped)
	assert.Len(t, fake.requests(t), 1)
}

func TestRunRequestShape(t *testing.T) {
	fake := newScripted(answer(goodJSON, "files/abc", 1, 1))
	in := testInput()
	in.FileHandle = "files/earlier"
	_, err := Run(context.Background(), fake, in)
	require.NoError(t, err)

	reqs := fake.requests(t)
	require.Len(t, reqs, 1)
	req := reqs[0]
	d := in.Domain
	system, user := BuildPrompt(d)

	assert.Equal(t, analyzer.StepAnalyze, req.Step)
	assert.Equal(t, "job_01TEST", req.JobID)
	assert.Equal(t, "autos", req.DomainKey)
	assert.Equal(t, "sha256abc:autos:analyze", req.CacheKey)
	assert.Equal(t, "gemini-test-lite", req.Model)
	assert.Equal(t, float32(0.2), req.Temperature)
	require.NotNil(t, req.ThinkingBudget)
	assert.Equal(t, int32(0), *req.ThinkingBudget)
	assert.Equal(t, domain.DefaultMaxOutputTokens, req.MaxOutputTokens, "unset model.maxOutputTokens → default cap")
	assert.Equal(t, system, req.SystemPrompt)
	assert.Equal(t, 60.0, req.DurationSec)
	assert.Equal(t, []string{f3q, side, dash}, req.ShotIDs)
	assert.Equal(t, "walkaround.mp4", req.SourceName)

	require.Len(t, req.Parts, 2, "video first, then the prompt")
	assert.Equal(t, analyzer.Part{
		FilePath:   "/scratch/job_01TEST/preview.mp4",
		MIMEType:   "video/mp4",
		VideoFPS:   2,
		FileHandle: "files/earlier",
	}, req.Parts[0])
	assert.Equal(t, analyzer.Part{Text: user}, req.Parts[1])

	require.NotNil(t, req.ResponseSchema)
	enum := req.ResponseSchema.Properties["scenes"].Items.Properties["shotId"].Enum
	for _, id := range d.ShotIDs() {
		assert.Contains(t, enum, id)
	}
	assert.Contains(t, enum, domain.OtherShotID)
	assert.Len(t, enum, len(d.Shots)+1)
	assert.Equal(t, "gate", req.ResponseSchema.PropertyOrdering[0])
}

func TestRunMaxOutputTokens(t *testing.T) {
	fake := newScripted(answer("not json", "files/abc", 1, 1), answer(goodJSON, "files/abc", 1, 1))
	in := testInput()
	in.Domain.Model.MaxOutputTokens = 2048
	_, err := Run(context.Background(), fake, in)
	require.NoError(t, err)
	reqs := fake.requests(t)
	require.Len(t, reqs, 2)
	assert.Equal(t, int32(2048), reqs[0].MaxOutputTokens)
	assert.Equal(t, int32(2048), reqs[1].MaxOutputTokens, "the nudge retry is capped too")
}

func TestRunNoCacheKey(t *testing.T) {
	fake := newScripted(answer("not json", "files/abc", 1, 1), answer(goodJSON, "files/abc", 1, 1))
	in := testInput()
	in.CacheKey = ""
	_, err := Run(context.Background(), fake, in)
	require.NoError(t, err)
	reqs := fake.requests(t)
	require.Len(t, reqs, 2)
	assert.Empty(t, reqs[0].CacheKey)
	assert.Empty(t, reqs[1].CacheKey, "no key means no recording key, also on retry")
}

func TestRunGate(t *testing.T) {
	sceneJSON := `[{"start": "00:04", "end": "00:09", "shotId": "side_profile", "reason": "r", "relevance": 0.9}]`
	cases := []struct {
		name       string
		passed     bool
		confidence string
		accepted   bool
		wantConf   float64
	}{
		{"gate failed", false, "0.95", false, 0.95},
		{"low confidence", true, "0.5", false, 0.5},
		{"confidence at threshold", true, "0.6", true, 0.6},
		{"confidence above 1 is clamped", true, "1.7", true, 1},
		{"negative confidence is clamped", true, "-0.3", false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := fmt.Sprintf(`{"gate": {"passed": %t, "confidence": %s, "reason": "why", "detectedSubject": "kitchen interior"},
				"videoSummary": "A kitchen.", "scenes": %s}`, c.passed, c.confidence, sceneJSON)
			fake := newScripted(answer(text, "files/abc", 10, 5))
			out, err := Run(context.Background(), fake, testInput())
			require.NoError(t, err)
			assert.Equal(t, c.accepted, out.Accepted)
			assert.Equal(t, c.passed, out.Gate.Passed)
			assert.Equal(t, c.wantConf, out.Gate.Confidence)
			assert.Equal(t, "why", out.Gate.Reason)
			assert.Equal(t, "kitchen interior", out.Gate.DetectedSubject)
			assert.Equal(t, "A kitchen.", out.VideoSummary)
			assert.Len(t, out.Calls, 1)
			assert.Equal(t, "files/abc", out.FileHandle)
			if c.accepted {
				assert.Len(t, out.Scenes, 1)
			} else {
				assert.Nil(t, out.Scenes, "scenes are ignored when the gate rejects")
				assert.Nil(t, out.Dropped)
			}
		})
	}
}

func TestRunAcceptedWithoutScenes(t *testing.T) {
	fake := newScripted(answer(`{"gate": {"passed": true, "confidence": 0.9, "reason": "car", "detectedSubject": "car"}, "videoSummary": "s", "scenes": []}`, "", 1, 1))
	out, err := Run(context.Background(), fake, testInput())
	require.NoError(t, err)
	assert.True(t, out.Accepted)
	assert.Empty(t, out.Scenes)
	assert.Empty(t, out.FileHandle, "provider without uploads")
}

func TestRunCodeFencedJSON(t *testing.T) {
	fenced := "Here is the analysis:\n```json\n" + goodJSON + "\n```\n"
	fake := newScripted(answer(fenced, "files/abc", 1, 1))
	out, err := Run(context.Background(), fake, testInput())
	require.NoError(t, err)
	assert.True(t, out.Accepted)
	assert.Len(t, out.Scenes, 3)
	assert.Len(t, fake.requests(t), 1, "a fence is not a reason to retry")
	assert.Equal(t, fenced, out.RawText)
}

func TestRunRetryAfterBadJSON(t *testing.T) {
	fake := newScripted(
		answer(`{"gate": {"passed": tru`, "files/abc", 100, 7),
		answer(goodJSON, "files/abc", 110, 300),
	)
	out, err := Run(context.Background(), fake, testInput())
	require.NoError(t, err)
	assert.True(t, out.Accepted)
	assert.Len(t, out.Scenes, 3)
	assert.Equal(t, []analyzer.Usage{
		{Model: "gemini-test-lite", InputTokens: 100, OutputTokens: 7},
		{Model: "gemini-test-lite", InputTokens: 110, OutputTokens: 300},
	}, out.Calls)
	assert.Equal(t, goodJSON, out.RawText)
	assert.Equal(t, "files/abc", out.FileHandle)

	reqs := fake.requests(t)
	require.Len(t, reqs, 2)
	first, retry := reqs[0], reqs[1]

	assert.Equal(t, "sha256abc:autos:analyze:retry", retry.CacheKey)
	require.Len(t, retry.Parts, 3)
	assert.Equal(t, "files/abc", retry.Parts[0].FileHandle, "the retry reuses the uploaded file")
	assert.Equal(t, first.Parts[0].FilePath, retry.Parts[0].FilePath)
	assert.Equal(t, first.Parts[0].VideoFPS, retry.Parts[0].VideoFPS)
	assert.Equal(t, first.Parts[1], retry.Parts[1])
	assert.Equal(t, analyzer.Part{Text: "Your previous answer was not valid JSON matching the required schema. Return valid JSON only."}, retry.Parts[2])
	assert.Equal(t, first.SystemPrompt, retry.SystemPrompt)
	assert.Same(t, first.ResponseSchema, retry.ResponseSchema)

	// The first request is left untouched.
	assert.Equal(t, "sha256abc:autos:analyze", first.CacheKey)
	require.Len(t, first.Parts, 2)
	assert.Empty(t, first.Parts[0].FileHandle)
}

func TestRunRetryOnInvalidSchema(t *testing.T) {
	// Valid JSON but no gate verdict: must not be read as passed=false.
	cases := map[string]string{
		"missing gate":       `{"videoSummary": "s", "scenes": []}`,
		"missing passed":     `{"gate": {"confidence": 0.9, "reason": "r", "detectedSubject": "car"}, "videoSummary": "s", "scenes": []}`,
		"missing confidence": `{"gate": {"passed": true, "reason": "r", "detectedSubject": "car"}, "videoSummary": "s", "scenes": []}`,
		"wrong type":         `{"gate": {"passed": "yes", "confidence": 0.9}, "scenes": []}`,
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newScripted(answer(bad, "files/abc", 1, 1), answer(goodJSON, "files/abc", 1, 1))
			out, err := Run(context.Background(), fake, testInput())
			require.NoError(t, err)
			assert.True(t, out.Accepted)
			assert.Len(t, out.Calls, 2)
		})
	}
}

func TestRunBadOutputTwice(t *testing.T) {
	fake := newScripted(
		answer("I cannot help with that.", "files/abc", 100, 5),
		answer(`{"gate": null}`, "files/abc", 101, 4),
	)
	out, err := Run(context.Background(), fake, testInput())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrBadOutput)
	var bo *BadOutputError
	require.ErrorAs(t, err, &bo)
	assert.Equal(t, `{"gate": null}`, bo.Raw)
	assert.Error(t, bo.Err)
	assert.Contains(t, err.Error(), "missing gate")

	require.NotNil(t, out)
	assert.Len(t, out.Calls, 2, "both calls cost money and are reported")
	assert.Equal(t, `{"gate": null}`, out.RawText)
	assert.Equal(t, "files/abc", out.FileHandle, "the runner must still release the file")
	assert.False(t, out.Accepted)
	assert.Len(t, fake.requests(t), 2, "exactly one retry")
}

func TestRunUnavailable(t *testing.T) {
	t.Run("first call", func(t *testing.T) {
		upstream := fmt.Errorf("gemini: 503 after 3 attempts: %w", analyzer.ErrUnavailable)
		fake := newScripted(step{err: upstream})
		in := testInput()
		in.FileHandle = "files/earlier"
		out, err := Run(context.Background(), fake, in)
		assert.Same(t, upstream, err, "returned unchanged")
		require.NotNil(t, out)
		assert.Empty(t, out.Calls)
		assert.Equal(t, "files/earlier", out.FileHandle)
	})
	t.Run("retry call", func(t *testing.T) {
		fake := newScripted(answer("garbage", "files/abc", 50, 2), step{err: analyzer.ErrUnavailable})
		out, err := Run(context.Background(), fake, testInput())
		assert.ErrorIs(t, err, analyzer.ErrUnavailable)
		assert.NotErrorIs(t, err, ErrBadOutput)
		require.NotNil(t, out)
		assert.Len(t, out.Calls, 1, "the first call is still accounted for")
		assert.Equal(t, "garbage", out.RawText)
		assert.Equal(t, "files/abc", out.FileHandle)
	})
}

func TestRunContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fake := newScripted(step{fn: func(ctx context.Context, _ *analyzer.Request) (*analyzer.Response, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	out, err := Run(ctx, fake, testInput())
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotErrorIs(t, err, analyzer.ErrUnavailable)
	require.NotNil(t, out)
}

func TestRunInvalidInput(t *testing.T) {
	cases := map[string]func(*Input){
		"nil domain": func(in *Input) { in.Domain = nil },
		"no media":   func(in *Input) { in.PreviewPath = ""; in.FileHandle = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newScripted()
			in := testInput()
			mutate(&in)
			out, err := Run(context.Background(), fake, in)
			assert.Error(t, err)
			assert.NotNil(t, out)
			assert.Empty(t, fake.requests(t))
		})
	}
	out, err := Run(context.Background(), nil, testInput())
	assert.Error(t, err)
	assert.NotNil(t, out)

	// A handle alone is enough (resume after a worker restart).
	fake := newScripted(answer(goodJSON, "", 1, 1))
	in := testInput()
	in.PreviewPath = ""
	in.FileHandle = "files/earlier"
	out, err = Run(context.Background(), fake, in)
	require.NoError(t, err)
	assert.Equal(t, "files/earlier", out.FileHandle, "kept when the provider reports no new handle")
}

func TestRunNilResponse(t *testing.T) {
	fake := newScripted(step{})
	out, err := Run(context.Background(), fake, testInput())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrBadOutput)
	assert.NotNil(t, out)
}

func TestGenerateTimeout(t *testing.T) {
	block := step{fn: func(ctx context.Context, _ *analyzer.Request) (*analyzer.Response, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}

	t.Run("per-call timeout becomes unavailable", func(t *testing.T) {
		fake := newScripted(block)
		_, err := Generate(context.Background(), fake, &analyzer.Request{Step: analyzer.StepAnalyze}, 20*time.Millisecond)
		assert.ErrorIs(t, err, analyzer.ErrUnavailable)
		assert.NotErrorIs(t, err, context.DeadlineExceeded, "must not look like a job interruption")
		assert.Contains(t, err.Error(), "timed out")
	})
	t.Run("caller deadline is passed through", func(t *testing.T) {
		fake := newScripted(block)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := Generate(ctx, fake, &analyzer.Request{Step: analyzer.StepAnalyze}, time.Minute)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, analyzer.ErrUnavailable)
	})
	t.Run("provider unavailable error is kept", func(t *testing.T) {
		fake := newScripted(step{err: analyzer.ErrUnavailable})
		_, err := Generate(context.Background(), fake, &analyzer.Request{}, time.Minute)
		assert.Same(t, analyzer.ErrUnavailable, err)
	})
	t.Run("no timeout", func(t *testing.T) {
		fake := newScripted(step{fn: func(ctx context.Context, _ *analyzer.Request) (*analyzer.Response, error) {
			_, has := ctx.Deadline()
			assert.False(t, has)
			return &analyzer.Response{Text: "{}"}, nil
		}})
		resp, err := Generate(context.Background(), fake, &analyzer.Request{}, 0)
		require.NoError(t, err)
		assert.Equal(t, "{}", resp.Text)
	})
}

func TestExtractJSON(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", `{"a":1}`, `{"a":1}`},
		{"whitespace", "\n\t {\"a\":1} \n", `{"a":1}`},
		{"bom", "\ufeff{\"a\":1}", `{"a":1}`},
		{"json fence", "```json\n{\"a\":1}\n```", `{"a":1}`},
		{"upper fence", "```JSON\n{\"a\":1}\n```", `{"a":1}`},
		{"bare fence", "```\n{\"a\":1}\n```", `{"a":1}`},
		{"one-line fence", "```json {\"a\":1} ```", `{"a":1}`},
		{"prose around fence", "Sure! Here it is:\n```json\n{\"a\":1}\n```\nHope that helps.", `{"a":1}`},
		{"unclosed fence", "```json\n{\"a\":1}\n", `{"a":1}`},
		{"backticks inside json", "{\"reason\":\"use ```code```\"}", "{\"reason\":\"use ```code```\"}"},
		{"empty", "  ", ""},
		{"prose only", "no json here", "no json here"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, ExtractJSON(c.in))
		})
	}
}

func TestParseResponse(t *testing.T) {
	ok := map[string]string{
		"full":           goodJSON,
		"scenes missing": `{"gate": {"passed": false, "confidence": 0.9, "reason": "r", "detectedSubject": "dog"}, "videoSummary": "s"}`,
		"scenes null":    `{"gate": {"passed": false, "confidence": 0, "reason": "r", "detectedSubject": "dog"}, "videoSummary": "s", "scenes": null}`,
		"extra fields":   `{"gate": {"passed": true, "confidence": 1, "extra": 1}, "note": "x"}`,
		"fenced":         "```json\n" + goodJSON + "\n```",
	}
	for name, text := range ok {
		t.Run("ok/"+name, func(t *testing.T) {
			r, err := ParseResponse(text)
			require.NoError(t, err)
			require.NotNil(t, r)
		})
	}

	r, err := ParseResponse(goodJSON)
	require.NoError(t, err)
	assert.Equal(t, "A silver sedan on a lot.", r.Gate.Reason, "trimmed")
	assert.Len(t, r.Scenes, 4)

	r, err = ParseResponse(`{"gate": {"passed": false, "confidence": 0}}`)
	require.NoError(t, err)
	assert.False(t, r.Gate.Passed, "explicit false is a valid verdict")

	bad := map[string]string{
		"empty":              "",
		"prose":              "The video shows a car.",
		"truncated":          `{"gate": {"passed": true, "confidence": 0.9`,
		"trailing garbage":   `{"gate": {"passed": true, "confidence": 0.9}} extra`,
		"array":              `[{"gate": {"passed": true, "confidence": 0.9}}]`,
		"null":               `null`,
		"missing gate":       `{"scenes": []}`,
		"null gate":          `{"gate": null}`,
		"missing passed":     `{"gate": {"confidence": 0.9}}`,
		"null passed":        `{"gate": {"passed": null, "confidence": 0.9}}`,
		"missing confidence": `{"gate": {"passed": true}}`,
		"string confidence":  `{"gate": {"passed": true, "confidence": "high"}}`,
		"overflow":           `{"gate": {"passed": true, "confidence": 1e999}}`,
		"bad scene type":     `{"gate": {"passed": true, "confidence": 0.9}, "scenes": [{"relevance": "high"}]}`,
	}
	for name, text := range bad {
		t.Run("bad/"+name, func(t *testing.T) {
			_, err := ParseResponse(text)
			assert.Error(t, err)
		})
	}
}

func TestBadOutputErrorIs(t *testing.T) {
	inner := errors.New("decode json: boom")
	err := fmt.Errorf("pipeline: %w", &BadOutputError{Raw: "x", Err: inner})
	assert.ErrorIs(t, err, ErrBadOutput)
	assert.ErrorIs(t, err, inner)
}
