package pipeline_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/media"
	"video-to-image/internal/pipeline"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

// Regression tests for the review findings on paying twice, split outcomes, unbounded
// cleanup, orphaned exports, re-rank re-payment and lying container durations.

var errBlip = errors.New("db blip")

// flakyStore wraps the real store: each listed method fails its first n calls with
// errBlip, and whether each call's context had a deadline is recorded.
type flakyStore struct {
	pipeline.Store
	mu        sync.Mutex
	failures  map[string]int
	calls     map[string]int
	deadlines map[string][]time.Duration // time left at call; <0 = no deadline
}

func newFlakyStore(inner pipeline.Store, failures map[string]int) *flakyStore {
	return &flakyStore{Store: inner, failures: failures, calls: map[string]int{}, deadlines: map[string][]time.Duration{}}
}

func (f *flakyStore) hit(ctx context.Context, method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[method]++
	left := time.Duration(-1)
	if d, ok := ctx.Deadline(); ok {
		left = time.Until(d)
	}
	f.deadlines[method] = append(f.deadlines[method], left)
	if f.failures[method] > 0 {
		f.failures[method]--
		return fmt.Errorf("store: %s: %w", method, errBlip)
	}
	return nil
}

func (f *flakyStore) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

func (f *flakyStore) SaveAnalysis(ctx context.Context, id string, a store.AnalysisResult) error {
	if err := f.hit(ctx, "SaveAnalysis"); err != nil {
		return err
	}
	return f.Store.SaveAnalysis(ctx, id, a)
}

func (f *flakyStore) RejectJob(ctx context.Context, id string, g store.Gate, summary string) error {
	if err := f.hit(ctx, "RejectJob"); err != nil {
		return err
	}
	return f.Store.RejectJob(ctx, id, g, summary)
}

func (f *flakyStore) FailJob(ctx context.Context, id, code, msg, log string) error {
	if err := f.hit(ctx, "FailJob"); err != nil {
		return err
	}
	return f.Store.FailJob(ctx, id, code, msg, log)
}

func (f *flakyStore) FinishStep(ctx context.Context, id, name string, st store.StepStatus, meta any) error {
	if err := f.hit(ctx, "FinishStep:"+string(st)); err != nil {
		return err
	}
	return f.Store.FinishStep(ctx, id, name, st, meta)
}

func (f *flakyStore) SkipRemainingSteps(ctx context.Context, id string) error {
	if err := f.hit(ctx, "SkipRemainingSteps"); err != nil {
		return err
	}
	return f.Store.SkipRemainingSteps(ctx, id)
}

func (f *flakyStore) InsertUsage(ctx context.Context, e store.UsageEvent) error {
	if err := f.hit(ctx, "InsertUsage"); err != nil {
		return err
	}
	return f.Store.InsertUsage(ctx, e)
}

// assertBounded checks every recorded call of method had a deadline of at most limit.
func (f *flakyStore) assertBounded(t *testing.T, method string, limit time.Duration) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	require.NotEmpty(t, f.deadlines[method], "%s was called", method)
	for _, left := range f.deadlines[method] {
		assert.True(t, left > 0 && left <= limit, "%s: time left %v, want a deadline ≤ %v", method, left, limit)
	}
}

func TestRunSaveAnalysisBlipIsRetriedWithoutPayingTwice(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	fs := newFlakyStore(e.store, map[string]int{"SaveAnalysis": 1, "InsertUsage": 1})
	a := &countingAnalyzer{Analyzer: newFake(t)}

	require.NoError(t, e.runnerWith(fs, a).Run(ctx, job.ID))
	got, err := e.store.GetJob(ctx, job.ID)
	require.NoError(t, err)
	assert.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	assert.Equal(t, int32(1), a.calls.Load(), "the model is called once")
	assert.Equal(t, 2, fs.count("SaveAnalysis"), "the write is retried in-process")
	assert.Equal(t, 2, fs.count("InsertUsage"), "usage is retried too, so the cost ceiling sees it")
	assert.Equal(t, "Synthetic walkaround (fake analyzer).", got.VideoSummary)
	fs.assertBounded(t, "SaveAnalysis", 15*time.Second)
}

func TestRunCancelledAfterModelAnswerKeepsTheAnswer(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inner := newFake(t)
	a := &scripted{onCall: cancel, answers: []func(*analyzer.Request) (*analyzer.Response, error){
		// A rollout's StopAndCancel lands while the (paid) answer is on its way back.
		func(r *analyzer.Request) (*analyzer.Response, error) { return inner.Generate(context.Background(), r) },
	}}
	err := e.runner(a).Run(ctx, job.ID)
	require.Error(t, err, "the cancelled attempt is retried by River")
	assert.Equal(t, store.StepDone, e.steps(job.ID)["analyze"], "the answer is recorded despite the cancellation")
	got, err := e.store.GetJob(context.Background(), job.ID)
	require.NoError(t, err)
	require.Equal(t, store.StatusProcessing, got.Status)
	require.NotNil(t, got.Gate)

	second := &scripted{} // any model call would fail the job
	require.NoError(t, e.runner(second).Run(context.Background(), job.ID))
	assert.Zero(t, second.calls.Load(), "the retry does not pay again")
	got, _ = e.store.GetJob(context.Background(), job.ID)
	assert.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
}

func TestRunFailJobBlipDoesNotPayTwice(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	fs := newFlakyStore(e.store, map[string]int{"FailJob": 1})
	a := &scripted{answers: []func(*analyzer.Request) (*analyzer.Response, error){text("not json"), text("still {not json")}}

	require.NoError(t, e.runnerWith(fs, a).Run(ctx, job.ID), "the failure is recorded in-process")
	got, _ := e.store.GetJob(ctx, job.ID)
	assert.Equal(t, store.StatusFailed, got.Status)
	assert.Equal(t, "MODEL_BAD_OUTPUT", got.ErrorCode)
	assert.Equal(t, int32(2), a.calls.Load())
	assert.Equal(t, 2, fs.count("FailJob"))
	assert.Empty(t, e.keysUnder(storage.JobPrefix("cars", job.ID)))
}

func TestRunUnrecordedFailureIsReappliedOnRetry(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	fs := newFlakyStore(e.store, map[string]int{"FailJob": 100}) // the database stays down for FailJob
	a := &scripted{answers: []func(*analyzer.Request) (*analyzer.Response, error){text("not json"), text("still {not json")}}

	require.Error(t, e.runnerWith(fs, a).Run(ctx, job.ID), "River must retry when the failure could not be recorded")
	assert.Equal(t, 4, fs.count("FailJob"), "4 in-process attempts")
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusProcessing, got.Status)
	require.Equal(t, store.StepFailed, e.steps(job.ID)["analyze"])

	second := &scripted{}
	require.NoError(t, e.runner(second).Run(ctx, job.ID))
	assert.Zero(t, second.calls.Load(), "the terminal outcome is re-applied, not re-paid")
	got, _ = e.store.GetJob(ctx, job.ID)
	assert.Equal(t, store.StatusFailed, got.Status)
	assert.Equal(t, "MODEL_BAD_OUTPUT", got.ErrorCode)
	assert.Empty(t, got.VideoPath)
	assert.Empty(t, e.keysUnder(storage.JobPrefix("cars", job.ID)), "failed jobs keep no files")
	st := e.steps(job.ID)
	assert.Equal(t, store.StepFailed, st["analyze"])
	assert.Equal(t, store.StepSkipped, st["extract"])
	assert.Equal(t, store.StepSkipped, st["export"])
}

func TestRunResumeAfterAnalyzeRejectsStoredGate(t *testing.T) {
	for name, gate := range map[string]store.Gate{
		"low confidence": {Passed: true, Confidence: 0.4, Reason: "blurry, maybe a car", DetectedSubject: "car?"},
		"gate failed":    {Passed: false, Confidence: 0.9, Reason: "This is a kitchen.", DetectedSubject: "kitchen"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			job := e.createJob("car.mp4")
			ctx := context.Background()
			// The state an older, non-atomic attempt left when it died between recording
			// the analyze step and the reject.
			require.NoError(t, e.store.StartProcessing(ctx, job.ID))
			require.NoError(t, e.store.SetAnalysis(ctx, job.ID, gate, "A summary.", "m"))
			require.NoError(t, e.store.StartStep(ctx, job.ID, store.StepNameAnalyze))
			require.NoError(t, e.store.FinishStep(ctx, job.ID, store.StepNameAnalyze, store.StepDone, nil))

			fs := newFlakyStore(e.store, map[string]int{"RejectJob": 1})
			a := &scripted{}
			require.NoError(t, e.runnerWith(fs, a).Run(ctx, job.ID))
			got, err := e.store.GetJob(ctx, job.ID)
			require.NoError(t, err)
			assert.Equal(t, store.StatusRejected, got.Status, got.InternalLog)
			assert.Empty(t, got.ErrorCode)
			require.NotNil(t, got.Gate)
			assert.Equal(t, gate, *got.Gate)
			assert.Equal(t, "A summary.", got.VideoSummary)
			assert.Empty(t, got.VideoPath)
			assert.Zero(t, a.calls.Load())
			assert.Equal(t, 2, fs.count("RejectJob"), "retried in-process")
			assert.Empty(t, e.keysUnder(storage.JobPrefix("cars", job.ID)), "rejected jobs keep no files")
			st := e.steps(job.ID)
			assert.Equal(t, store.StepDone, st["analyze"])
			assert.Equal(t, store.StepSkipped, st["extract"])
			assert.Equal(t, store.StepSkipped, st["export"])
		})
	}
}

func TestRunRejectionIsRecordedInOneWrite(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("reject-this-kitchen.mp4")
	ctx := context.Background()
	fs := newFlakyStore(e.store, nil)
	require.NoError(t, e.runnerWith(fs, newFake(t)).Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	assert.Equal(t, store.StatusRejected, got.Status)
	assert.Equal(t, "Synthetic rejected video (fake analyzer).", got.VideoSummary)
	assert.Equal(t, 1, fs.count("SaveAnalysis"))
	assert.Zero(t, fs.count("RejectJob"), "the rejection is part of SaveAnalysis")
}

// releaseRecorder is the fake analyzer plus a provider file handle whose release
// context is recorded.
type releaseRecorder struct {
	scripted
	mu       sync.Mutex
	released []time.Duration // time left at release; <0 = no deadline
}

func (r *releaseRecorder) ReleaseFile(ctx context.Context, _ string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	left := time.Duration(-1)
	if d, ok := ctx.Deadline(); ok {
		left = time.Until(d)
	}
	r.released = append(r.released, left)
	return nil
}

// deadlineStorage records whether DeletePrefix calls carry a deadline.
type deadlineStorage struct {
	storage.Storage
	mu   sync.Mutex
	left []time.Duration
}

func (d *deadlineStorage) DeletePrefix(ctx context.Context, prefix string) error {
	d.mu.Lock()
	left := time.Duration(-1)
	if dl, ok := ctx.Deadline(); ok {
		left = time.Until(dl)
	}
	d.left = append(d.left, left)
	d.mu.Unlock()
	return d.Storage.DeletePrefix(ctx, prefix)
}

func TestRunDetachedCleanupIsBounded(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ds := &deadlineStorage{Storage: e.storage}
	e.storage = ds
	withHandle := func(body string) func(*analyzer.Request) (*analyzer.Response, error) {
		return func(r *analyzer.Request) (*analyzer.Response, error) {
			resp, err := text(body)(r)
			resp.FileHandles = []string{"files/preview-1"}
			return resp, err
		}
	}
	a := &releaseRecorder{scripted: scripted{answers: []func(*analyzer.Request) (*analyzer.Response, error){
		withHandle("not json"), withHandle("still {not json"),
	}}}
	fs := newFlakyStore(e.store, nil)
	// The job context has no deadline of its own (like River's with JobTimeout -1).
	require.NoError(t, e.runnerWith(fs, a).Run(context.Background(), job.ID))
	got, _ := e.store.GetJob(context.Background(), job.ID)
	require.Equal(t, store.StatusFailed, got.Status)

	limit := pipeline.CleanupTimeout
	require.Len(t, a.released, 1)
	assert.True(t, a.released[0] > 0 && a.released[0] <= limit, "ReleaseFile deadline: %v", a.released[0])
	require.NotEmpty(t, ds.left)
	for _, left := range ds.left {
		assert.True(t, left > 0 && left <= limit, "DeletePrefix deadline: %v", left)
	}
	fs.assertBounded(t, "FailJob", limit)
	fs.assertBounded(t, "SkipRemainingSteps", limit)
	fs.assertBounded(t, "FinishStep:failed", limit)
	fs.assertBounded(t, "InsertUsage", limit)
}

func TestRunRetriedExportLeavesNoOrphans(t *testing.T) {
	e := newEnv(t)
	job := e.createJob("car.mp4")
	ctx := context.Background()
	e.storage = &flakyStorage{Storage: e.storage}

	require.Error(t, e.runner(newFake(t)).Run(ctx, job.ID))
	prefix := storage.JobPrefix("cars", job.ID)
	require.NotEmpty(t, e.keysUnder(prefix+"images/"), "the failed attempt uploaded images")

	require.NoError(t, e.runner(newFake(t)).Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	imgs, err := e.store.ListSelectedImages(ctx, job.ID)
	require.NoError(t, err)
	require.NotEmpty(t, imgs)
	var wantImages, wantThumbs []string
	for _, im := range imgs {
		wantImages = append(wantImages, im.ImagePath)
		wantThumbs = append(wantThumbs, im.ThumbPath)
	}
	assert.ElementsMatch(t, wantImages, e.keysUnder(prefix+"images/"), "only the final attempt's images remain")
	assert.ElementsMatch(t, wantThumbs, e.keysUnder(prefix+"thumbs/"))
}

// rerankPicker answers analyze with the fake analyzer and re-rank by picking the LAST
// candidate listed for each shot (not the best-scoring one), counting re-rank calls.
type rerankPicker struct {
	analyzer.Analyzer
	reranks atomic.Int32
}

func (p *rerankPicker) Generate(ctx context.Context, r *analyzer.Request) (*analyzer.Response, error) {
	if r.Step != analyzer.StepRerank {
		return p.Analyzer.Generate(ctx, r)
	}
	p.reranks.Add(1)
	last := map[string]string{}
	var order []string
	shot := ""
	for _, part := range r.Parts {
		f := strings.Fields(part.Text)
		switch {
		case len(f) >= 2 && f[0] == "Shot":
			shot = f[1]
			order = append(order, shot)
		case len(f) == 2 && f[0] == "Candidate":
			last[shot] = strings.TrimSuffix(f[1], ":")
		}
	}
	type pick struct {
		ShotID      string `json:"shotId"`
		CandidateID string `json:"candidateId"`
		Reason      string `json:"reason"`
	}
	var picks []pick
	for _, s := range order {
		picks = append(picks, pick{ShotID: s, CandidateID: last[s], Reason: "test pick"})
	}
	b, err := json.Marshal(map[string]any{"picks": picks})
	if err != nil {
		return nil, err
	}
	return &analyzer.Response{Text: string(b), Usage: analyzer.Usage{Model: r.Model, InputTokens: 10, OutputTokens: 10}}, nil
}

type shotAt struct {
	shot string
	ts   float64
}

func TestRunRetryReusesRerankPicks(t *testing.T) {
	e := newEnv(t)
	dom, ok := e.reg.Get("cars")
	require.True(t, ok)
	dom.Selection.ModelRerank = true
	// The synthetic clip's neighbouring frames are near-identical: keep them all so each
	// shot has several eligible candidates to re-rank.
	dom.Selection.DedupeMaxHamming = -1
	job := e.createJob("car.mp4")
	ctx := context.Background()
	e.storage = &flakyStorage{Storage: e.storage}
	a := &rerankPicker{Analyzer: newFake(t)}

	require.Error(t, e.runner(a).Run(ctx, job.ID), "export hiccup")
	require.Equal(t, int32(1), a.reranks.Load())
	steps, err := e.store.ListSteps(ctx, job.ID)
	require.NoError(t, err)
	var picks map[string]any
	for _, s := range steps {
		if s.Name == store.StepNameSelect {
			picks, _ = s.Meta["rerankPreferred"].(map[string]any)
		}
	}
	require.NotEmpty(t, picks, "the picks are stored with the select step")
	cands, err := e.store.ListCandidates(ctx, job.ID)
	require.NoError(t, err)
	var first []shotAt
	for _, c := range cands {
		if c.Selected {
			first = append(first, shotAt{c.ShotID, c.TimestampSec})
		}
	}
	applied := 0
	for shot, ts := range picks {
		for _, s := range first {
			if s.shot == shot && math.Abs(s.ts-ts.(float64)) < 1e-3 {
				applied++
			}
		}
	}
	require.NotZero(t, applied, "the picks changed the selection")

	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	assert.Equal(t, int32(1), a.reranks.Load(), "the retry reuses the stored picks instead of paying again")
	got, _ := e.store.GetJob(ctx, job.ID)
	require.Equal(t, store.StatusCompleted, got.Status, got.InternalLog)
	imgs, err := e.store.ListSelectedImages(ctx, job.ID)
	require.NoError(t, err)
	var second []shotAt
	for _, im := range imgs {
		second = append(second, shotAt{im.ShotID, im.TimestampSec})
	}
	assert.ElementsMatch(t, first, second, "the retry selects the same frames")
}

// previewProbe records the duration of the preview each analyze call receives.
type previewProbe struct {
	analyzer.Analyzer
	ff        *media.FFmpeg
	calls     atomic.Int32
	durations []float64
}

func (p *previewProbe) Generate(ctx context.Context, r *analyzer.Request) (*analyzer.Response, error) {
	p.calls.Add(1)
	for _, part := range r.Parts {
		if part.FilePath != "" {
			info, err := p.ff.Probe(ctx, part.FilePath)
			if err != nil {
				return nil, err
			}
			p.durations = append(p.durations, info.DurationSec)
		}
	}
	return p.Analyzer.Generate(ctx, r)
}

// A container header claiming 5 s on a 12 s video must never lead to the model seeing
// (and billing) the real 12 s: either the preview is cut to the declared length, or the
// job fails before any model call.
func TestRunPreviewIsCappedToDeclaredDuration(t *testing.T) {
	e := newEnv(t)
	job := e.createJobInfo("car.mp4", func(v *media.VideoInfo) { v.DurationSec = 5 })
	ctx := context.Background()
	a := &previewProbe{Analyzer: newFake(t), ff: e.ff}

	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	if got.Status == store.StatusFailed {
		assert.Equal(t, "PROCESSING_ERROR", got.ErrorCode)
		assert.Contains(t, got.InternalLog, "preview duration")
		assert.Zero(t, a.calls.Load(), "no model call for an over-long preview")
		return
	}
	require.NotZero(t, a.calls.Load())
	for _, d := range a.durations {
		assert.LessOrEqual(t, d, 5.0+2, "the model saw a %.1f s preview", d)
	}
}

// The check also covers a stored preview reused by a retry (deterministic regardless of
// how BuildPreview caps the encode).
func TestRunStoredPreviewLongerThanDeclaredFailsBeforeModelCall(t *testing.T) {
	e := newEnv(t)
	job := e.createJobInfo("car.mp4", func(v *media.VideoInfo) { v.DurationSec = 5 })
	ctx := context.Background()
	// An earlier attempt built (and stored) a preview of the real 12 s.
	f, err := os.Open(sharedVideo(t))
	require.NoError(t, err)
	defer f.Close()
	require.NoError(t, e.storage.Put(ctx, storage.PreviewKey("cars", job.ID), f, "video/mp4"))
	require.NoError(t, e.store.StartProcessing(ctx, job.ID))
	require.NoError(t, e.store.StartStep(ctx, job.ID, store.StepNamePreview))
	require.NoError(t, e.store.FinishStep(ctx, job.ID, store.StepNamePreview, store.StepDone, nil))

	a := &countingAnalyzer{Analyzer: newFake(t)}
	require.NoError(t, e.runner(a).Run(ctx, job.ID))
	got, _ := e.store.GetJob(ctx, job.ID)
	assert.Equal(t, store.StatusFailed, got.Status)
	assert.Equal(t, "PROCESSING_ERROR", got.ErrorCode)
	assert.Contains(t, got.InternalLog, "preview duration")
	assert.NotContains(t, got.ErrorMessage, "preview", "the user message stays generic")
	assert.Zero(t, a.calls.Load())
	assert.Empty(t, e.keysUnder(storage.JobPrefix("cars", job.ID)))
}
