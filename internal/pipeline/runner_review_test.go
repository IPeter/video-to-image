package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/pipeline"
	"video-to-image/internal/store"
)

// Tests for how the select step uses, stores and reuses the model review: incomplete
// answers, failures that must not be paid twice, and the orientation answer that
// outlives a failed judgement.

func (e *env) stepMeta(jobID, name string) map[string]any {
	e.t.Helper()
	steps, err := e.store.ListSteps(context.Background(), jobID)
	require.NoError(e.t, err)
	for _, s := range steps {
		if s.Name == name {
			return s.Meta
		}
	}
	e.t.Fatalf("step %s not found", name)
	return nil
}

func (e *env) selectedImages(jobID string) []store.SelectedImage {
	e.t.Helper()
	imgs, err := e.store.ListSelectedImages(context.Background(), jobID)
	require.NoError(e.t, err)
	return imgs
}

// usableAnswer is a judgement answer rating the frames ids fully visible, quality 4, in
// their proposed view.
func usableAnswer(r *analyzer.Request, ids []string, frames map[string]sentFrame) *analyzer.Response {
	type img struct {
		ID           string `json:"id"`
		View         string `json:"view"`
		FullyVisible bool   `json:"subjectFullyVisible"`
		Quality      int    `json:"quality"`
		Box          [4]int `json:"box_2d"`
		SameAs       string `json:"sameAs"`
		Issues       string `json:"issues"`
	}
	out := struct {
		Images []img `json:"images"`
	}{Images: []img{}}
	for _, id := range ids {
		out.Images = append(out.Images, img{ID: id, View: frames[id].view, FullyVisible: true, Quality: 4,
			Box: [4]int{150, 100, 850, 900}, SameAs: "none"})
	}
	b, _ := json.Marshal(out)
	return &analyzer.Response{Text: string(b), Usage: analyzer.Usage{Model: r.Model, InputTokens: 258 * len(ids), OutputTokens: 40 * len(ids)}}
}

func textAnswer(r *analyzer.Request, body string) *analyzer.Response {
	return &analyzer.Response{Text: body, Usage: analyzer.Usage{Model: r.Model, InputTokens: 1000, OutputTokens: 10}}
}

// An answer that covers fewer than half of the frames sent says nothing about the
// frames: the job selects on scores alone instead of treating every unanswered frame as
// rejected (which ended paid jobs with no photos). From half on, the answered frames are
// the only eligible ones, as before.
func TestRunReviewAnswerCoverage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answer   func(ids []string) []string // the ids answered
		unknown  bool                        // answer with an id that was never sent
		fallback bool
	}{
		{name: "empty", answer: func([]string) []string { return nil }, fallback: true},
		{name: "unknown ids", answer: func([]string) []string { return nil }, unknown: true, fallback: true},
		{name: "fewer than half", answer: func(ids []string) []string { return ids[:(len(ids)-1)/2] }, fallback: true},
		{name: "half", answer: func(ids []string) []string { return ids[:(len(ids)+1)/2] }},
		{name: "all", answer: func(ids []string) []string { return ids }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			dom, _ := e.reg.Get("cars")
			dom.Selection.DedupeMaxHamming = 0 // the synthetic clip's frames hash alike: shortlist more of them
			job := e.createJob("car.mp4")
			ctx := context.Background()
			var sent int
			answered := map[string]bool{} // candidate IDs with a verdict
			a := &reviewer{Analyzer: newFake(t), judge: func(r *analyzer.Request, ids []string, frames map[string]sentFrame) (*analyzer.Response, error) {
				sent = len(ids)
				keys := tc.answer(ids)
				for _, k := range keys {
					answered[frames[k].candidate] = true
				}
				resp := usableAnswer(r, keys, frames)
				if tc.unknown {
					resp.Text = `{"images":[{"id":"cf_x","view":"side_profile","subjectFullyVisible":true,"quality":5,` +
						`"box_2d":[0,0,1000,1000],"sameAs":"none","issues":""}]}`
				}
				return resp, nil
			}}
			require.NoError(t, e.runner(a).Run(ctx, job.ID))
			got, err := e.store.GetJob(ctx, job.ID)
			require.NoError(t, err)
			require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
			require.GreaterOrEqual(t, sent, 4, "enough frames for a partial answer")

			meta := e.stepMeta(job.ID, store.StepNameSelect)
			imgs := e.selectedImages(job.ID)
			require.NotEmpty(t, imgs, "a paid job never ends empty because of the review's answer")
			assert.NotEqual(t, pipeline.NoScenesExplanation, got.ResultExplanation)
			if tc.fallback {
				assert.Equal(t, fmt.Sprintf("review answered %d of %d frames", len(answered), sent), meta["reviewError"])
				assert.Equal(t, "tooFew", meta["reviewFailure"])
				assert.NotContains(t, meta, "review", "no verdicts to reuse")
				return
			}
			assert.NotContains(t, meta, "reviewError")
			stored, _ := meta["review"].([]any)
			assert.Len(t, stored, len(answered))
			for _, im := range imgs {
				assert.True(t, answered[im.CandidateID], "only answered frames are eligible: %s", im.CandidateID)
			}
		})
	}
}

// A judgement answer that could not be parsed was paid for and would most likely fail
// again: a River retry (here after an export hiccup) selects on scores alone without
// calling the model, and still straightens the frames with the stored orientation.
func TestRunRetryDoesNotRepayFailedReview(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	e.storage = &flakyStorage{Storage: e.storage}
	a := &reviewer{Analyzer: newFake(t), rotate: 90, judge: func(r *analyzer.Request, _ []string, _ map[string]sentFrame) (*analyzer.Response, error) {
		return textAnswer(r, `{"images":[{"id":"img1","view":`), nil // cut off
	}}

	require.Error(t, e.runner(a).Run(ctx, job.ID), "export hiccup")
	require.Equal(t, int32(1), a.reviews.Load())
	orients := a.orients.Load()
	require.NotZero(t, orients)
	meta := e.stepMeta(job.ID, store.StepNameSelect)
	assert.Equal(t, "badOutput", meta["reviewFailure"])
	assert.Contains(t, meta["reviewError"], "invalid output")
	assert.NotEmpty(t, meta["reviewRotations"], "the orientation answer is stored")

	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	assert.Equal(t, int32(1), a.reviews.Load(), "the failed judgement is not paid again")
	assert.Equal(t, orients, a.orients.Load(), "nor the orientation")
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	meta = e.stepMeta(job.ID, store.StepNameSelect)
	assert.Equal(t, true, meta["reviewReused"])
	assert.Equal(t, "badOutput", meta["reviewFailure"])

	imgs := e.selectedImages(job.ID)
	require.NotEmpty(t, imgs)
	for _, im := range imgs {
		assert.Equal(t, 90, im.Rotation, "upright from the stored orientation answer: %s", im.ShotID)
		assert.Nil(t, im.Crop, "no verdict, no box: rotated only")
		assert.Less(t, im.Width, im.Height, "the 1280x720 frame turned upright")
	}
	usage, err := e.store.JobUsage(ctx, job.ID)
	require.NoError(t, err)
	var reviewCalls int64
	for _, u := range usage {
		if u.Step == analyzer.StepReview {
			reviewCalls += u.Calls
		}
	}
	assert.Equal(t, int64(orients)+1, reviewCalls, "one review is billed across both attempts")
}

// No judgement answer was paid for when the provider was unavailable, so a retry may
// review again; its outcome replaces the stored failure.
func TestRunRetryRerunsUnavailableReview(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	e.storage = &flakyStorage{Storage: e.storage}
	a := &reviewer{Analyzer: newFake(t)}
	a.judge = func(r *analyzer.Request, ids []string, frames map[string]sentFrame) (*analyzer.Response, error) {
		if a.reviews.Load() == 1 {
			return nil, fmt.Errorf("gemini 503: %w", analyzer.ErrUnavailable)
		}
		return usableAnswer(r, ids, frames), nil
	}

	require.Error(t, e.runner(a).Run(ctx, job.ID), "export hiccup")
	meta := e.stepMeta(job.ID, store.StepNameSelect)
	assert.Equal(t, "unavailable", meta["reviewFailure"])

	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	assert.Equal(t, int32(2), a.reviews.Load(), "reviewed again")
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	meta = e.stepMeta(job.ID, store.StepNameSelect)
	assert.NotEmpty(t, meta["review"])
	for _, k := range []string{"reviewFailure", "reviewError", "reviewReused"} {
		assert.NotContains(t, meta, k, "the earlier failure is cleared")
	}
}

// A failed judgement still leaves the orientation answer: the frames are selected on
// scores alone and exported upright.
func TestRunFailedJudgementKeepsOrientation(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	a := &reviewer{Analyzer: newFake(t), rotate: 270, judge: func(*analyzer.Request, []string, map[string]sentFrame) (*analyzer.Response, error) {
		return nil, fmt.Errorf("gemini 503: %w", analyzer.ErrUnavailable)
	}}
	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	imgs := e.selectedImages(job.ID)
	require.NotEmpty(t, imgs)
	for _, im := range imgs {
		assert.Equal(t, 270, im.Rotation, im.ShotID)
		assert.Nil(t, im.Crop)
	}
}

// Verdicts given without the orientation (the orientation call failed) are not reused:
// the retry orients and reviews again.
func TestRunRetryRedoesReviewJudgedWithoutOrientation(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	e.storage = &flakyStorage{Storage: e.storage}
	orientOK := false
	a := &reviewer{Analyzer: newFake(t), rotate: 90}
	a.orient = func(r *analyzer.Request) (*analyzer.Response, error) {
		if !orientOK {
			return textAnswer(r, "not json"), nil
		}
		ids := r.ResponseSchema.Properties["images"].Items.Properties["id"].Enum
		body := `{"images":[`
		for i, id := range ids {
			if i > 0 {
				body += ","
			}
			body += fmt.Sprintf(`{"id":%q,"upright":"top-right"}`, id)
		}
		return textAnswer(r, body+"]}"), nil
	}

	require.Error(t, e.runner(a).Run(ctx, job.ID), "export hiccup")
	require.Equal(t, int32(1), a.reviews.Load())
	meta := e.stepMeta(job.ID, store.StepNameSelect)
	assert.NotEmpty(t, meta["reviewOrientationError"])
	assert.NotEmpty(t, meta["review"])

	orientOK = true
	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	assert.Equal(t, int32(2), a.reviews.Load(), "the frames are judged again, upright")
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	meta = e.stepMeta(job.ID, store.StepNameSelect)
	assert.NotContains(t, meta, "reviewOrientationError")
	for _, im := range e.selectedImages(job.ID) {
		assert.Equal(t, 90, im.Rotation, im.ShotID)
	}
}

// The verdicts are stored as soon as the review returns: a database error later in the
// select step does not throw them away.
func TestRunReviewIsStoredBeforeSelection(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	a := &reviewer{Analyzer: newFake(t)}
	fs := newFlakyStore(e.store, map[string]int{"ReplaceCandidates": 1})

	require.Error(t, e.runnerWith(fs, a).Run(ctx, job.ID))
	require.Equal(t, int32(1), a.reviews.Load())
	assert.NotEmpty(t, e.stepMeta(job.ID, store.StepNameSelect)["review"])

	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	assert.Equal(t, int32(1), a.reviews.Load(), "the retry reuses the verdicts")
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	assert.Equal(t, true, e.stepMeta(job.ID, store.StepNameSelect)["reviewReused"])
}

// Storing the outcome early is an optimisation: when it cannot be written the job goes
// on, and the outcome lands with the step's final meta.
func TestRunReviewStoreFailureIsNotFatal(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	fs := newFlakyStore(e.store, map[string]int{"MergeStepMeta": 100})
	r := e.runnerWith(fs, &reviewer{Analyzer: newFake(t)})
	r.ExtractConcurrency = 1
	require.NoError(t, r.Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	assert.Equal(t, 4, fs.count("MergeStepMeta"), "retried in-process")
	assert.NotEmpty(t, e.stepMeta(job.ID, store.StepNameSelect)["review"])
	assert.Equal(t, float64(1), e.stepMeta(job.ID, store.StepNameExtract)["concurrency"], "Runner.ExtractConcurrency reaches extract")
}
