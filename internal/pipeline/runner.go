// Package pipeline orchestrates the processing steps for one job (plan/03-pipeline.md).
// Step packages never touch the database; the Runner does, recording progress,
// duration and cost per step in job_steps and usage_events.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/metrics"
	"video-to-image/internal/pipeline/analyze"
	"video-to-image/internal/pipeline/crop"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/pipeline/extract"
	"video-to-image/internal/pipeline/locate"
	"video-to-image/internal/pipeline/preview"
	"video-to-image/internal/pipeline/review"
	"video-to-image/internal/pipeline/score"
	"video-to-image/internal/pipeline/selectr"
	"video-to-image/internal/pipeline/studiojob"
	"video-to-image/internal/pricing"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
	"video-to-image/internal/studio"
)

// Job error codes (subset of httpapi.ErrorCode) and their user-safe messages.
const (
	CodeModelUnavailable = "MODEL_UNAVAILABLE"
	CodeModelBadOutput   = "MODEL_BAD_OUTPUT"
	CodeProcessingError  = "PROCESSING_ERROR"
)

var userMessages = map[string]string{
	CodeModelUnavailable: "The analysis service is temporarily unavailable. Please try again later.",
	CodeModelBadOutput:   "The analysis returned an unexpected result. Please try again.",
	CodeProcessingError:  "Something went wrong while processing the video.",
}

// NoScenesExplanation is the result explanation when the gate passed but the model
// found no usable scene (the brief requires this over returning unrelated images).
const NoScenesExplanation = selectr.NoScenesExplanation

// Store is the subset of *store.Store the runner uses.
type Store interface {
	GetJob(ctx context.Context, id string) (*store.Job, error)
	StartProcessing(ctx context.Context, id string) error
	SaveAnalysis(ctx context.Context, jobID string, a store.AnalysisResult) error
	CompleteJob(ctx context.Context, id string, c store.Completion) error
	RejectJob(ctx context.Context, id string, gate store.Gate, summary string) error
	FailJob(ctx context.Context, id, code, message, internalLog string) error
	AppendInternalLog(ctx context.Context, id, text string) error
	ListSteps(ctx context.Context, jobID string) ([]store.Step, error)
	StartStep(ctx context.Context, jobID, name string) error
	FinishStep(ctx context.Context, jobID, name string, status store.StepStatus, meta any) error
	MergeStepMeta(ctx context.Context, jobID, name string, meta map[string]any) error
	SkipRemainingSteps(ctx context.Context, jobID string) error
	ListScenes(ctx context.Context, jobID string) ([]store.Scene, error)
	ReplaceCandidates(ctx context.Context, jobID string, cands []store.CandidateFrame) error
	InsertUsage(ctx context.Context, e store.UsageEvent) error
}

// StudioEnqueuer inserts the studio job of a job inside the given transaction
// (*queue.Client).
type StudioEnqueuer interface {
	EnqueueStudioTx(ctx context.Context, tx pgx.Tx, jobID string) error
}

// Runner runs the pipeline for one job. It is safe for concurrent use by several
// River workers (all per-job state lives on the stack and in the job's scratch dir).
type Runner struct {
	Store      Store
	Storage    storage.Storage
	FFmpeg     *media.FFmpeg
	Analyzer   analyzer.Analyzer
	Registry   *domain.Registry
	ScratchDir string
	Logger     *slog.Logger
	// ExtractConcurrency bounds the ffmpeg processes one job's extract step runs at once
	// (extract.Input.Concurrency; ≤ 0 → extract.DefaultConcurrency). The worker sets it
	// with the ExtractConcurrency function.
	ExtractConcurrency int
	// Studio, when set, schedules studio backgrounds (internal/pipeline/studiojob) for a
	// job that completes with at least one eligible photo of a domain with studio
	// enabled: studio_status pending and the studio job inserted in the completion's
	// transaction. nil: this process has no segmentation sidecar; studio_status stays
	// none.
	Studio StudioEnqueuer

	// persistBackoff overrides defaultPersistBackoff (tests).
	persistBackoff []time.Duration
}

// ExtractConcurrency is the Runner.ExtractConcurrency for a process: configured when it
// is positive, else as many ffmpeg processes per job as keep every pipeline worker
// within procs CPUs, max(1, procs / (ffmpegThreads × workers)) with workers ≥ 1. An
// uncapped ffmpeg (ffmpegThreads ≤ 0) already uses every core, so auto gives 1. Memory
// scales the same way: one 4K HEVC extract process holds about 450 MiB at 2 threads
// (700 MiB at 4), and running processes side by side under a CPU limit barely helps.
func ExtractConcurrency(configured, procs, ffmpegThreads, workers int) int {
	if configured > 0 {
		return configured
	}
	if ffmpegThreads <= 0 {
		return 1
	}
	return max(1, procs/(ffmpegThreads*max(1, workers)))
}

// Bounds for work that must not be cut short by the job context (it runs on
// context.WithoutCancel) but must not hang a worker slot either.
const (
	// persistTimeout bounds one attempt of a write that records money already spent
	// (the analyze and review outcomes, usage) or a terminal outcome (FailJob,
	// RejectJob, the failed-step mark).
	persistTimeout = 15 * time.Second
	// cleanupTimeout bounds best-effort cleanup: provider file release, storage
	// deletes, skipping the remaining steps.
	cleanupTimeout = 30 * time.Second
	// previewSlackSec is how much longer than its cap a built preview may be (encoder
	// rounding) before it is treated as a lying container header.
	previewSlackSec = 1.0
)

// defaultPersistBackoff is the wait before each retry of a persist write: 4 attempts.
var defaultPersistBackoff = []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond, 4 * time.Second}

// terminalError ends the job as failed; River must not retry it (the money may
// already be spent and the failure is deterministic).
type terminalError struct {
	code     string
	internal string // for jobs.internal_log, never shown to users
	err      error
}

func (e *terminalError) Error() string { return fmt.Sprintf("%s: %v", e.code, e.err) }
func (e *terminalError) Unwrap() error { return e.err }

func terminal(code string, err error, internal string) *terminalError {
	return &terminalError{code: code, err: err, internal: internal}
}

// errJobGone means the job was deleted by the user (expired) while running.
var errJobGone = errors.New("pipeline: job no longer active")

// run holds per-job state.
type run struct {
	*Runner
	job   *store.Job
	dom   *domain.Config
	log   *slog.Logger
	work  string
	steps map[string]store.Step
	// located are the locate pass's moments of this run (analyze step meta
	// locatedMomentsKey on a resumed run), for the review shortlist.
	located map[string][]float64
	// previewFPS is the preview's real frame rate (keyframe-only previews are slower
	// than requested); the model is told to sample at it.
	previewFPS float64
	// assessments are the model review's verdicts by candidate ID (nil: no review).
	assessments map[string]review.Assessment
	// rotations is the review's orientation answer by candidate ID: the clockwise
	// rotation that makes a shortlisted frame upright. It is kept apart from assessments
	// because it survives a failed judgement (selection then runs on scores alone), and
	// editFor falls back to it for frames without a verdict.
	rotations map[string]int
	// scored is the select step's input (editFor looks up the scene of oriented frames).
	scored []score.Scored
}

// Run processes jobID. It returns nil once the job reached a terminal status (set by
// the runner itself), and an error only for infrastructure failures (database,
// storage, cancellation) that River should retry.
//
// Paid work is recorded before anything else can fail: the analyze outcome (gate,
// summary, scenes, step done and, for a rejection, the rejected status) is written in
// one transaction on a detached context and retried in-process, a terminal failure is
// recorded the same way, and the frame review's outcome is merged into the select step
// as soon as it returns. Re-delivery resumes after a done analyze step (and reuses the
// stored review outcome, see reuseReview), and re-applies a failure an earlier attempt
// decided but could not record, so a retry does not pay for the model again. The model
// is called again only when an attempt died between the model's answer and the
// database (for example the database stayed down for every in-process retry).
func (r *Runner) Run(ctx context.Context, jobID string) error {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	job, err := r.Store.GetJob(ctx, jobID)
	if errors.Is(err, store.ErrNotFound) {
		logger.Warn("pipeline: job not found; dropping", "job_id", jobID)
		return nil
	}
	if err != nil {
		return fmt.Errorf("pipeline: load job: %w", err)
	}
	if job.Status.Terminal() {
		return nil
	}
	rn := &run{Runner: r, job: job, log: logger.With("job_id", job.ID, "domain", job.DomainKey)}

	dom, ok := r.Registry.Get(job.DomainKey)
	if !ok {
		return rn.fail(ctx, terminal(CodeProcessingError, fmt.Errorf("domain %q is not configured", job.DomainKey), ""))
	}
	rn.dom = dom

	if err := r.Store.StartProcessing(ctx, job.ID); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil // became terminal in the meantime (e.g. user deletion)
		}
		return fmt.Errorf("pipeline: start processing: %w", err)
	}

	err = rn.execute(ctx)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errJobGone):
		rn.log.Info("pipeline: job deleted while processing; stopping")
		rn.deleteFiles(ctx)
		return nil
	default:
		var te *terminalError
		if errors.As(err, &te) {
			return rn.fail(ctx, te)
		}
		return err
	}
}

func (rn *run) execute(ctx context.Context) error {
	rn.work = filepath.Join(rn.ScratchDir, "jobs", rn.job.ID)
	_ = os.RemoveAll(rn.work) // stale leftovers of a crashed attempt
	if err := os.MkdirAll(rn.work, 0o755); err != nil {
		return fmt.Errorf("pipeline: scratch dir: %w", err)
	}
	defer os.RemoveAll(rn.work)

	steps, err := rn.Store.ListSteps(ctx, rn.job.ID)
	if err != nil {
		return fmt.Errorf("pipeline: list steps: %w", err)
	}
	rn.steps = make(map[string]store.Step, len(steps))
	for _, s := range steps {
		rn.steps[s.Name] = s
	}
	if err := rn.earlierFailure(); err != nil {
		return err
	}
	resume := rn.steps[store.StepNameAnalyze].Status == store.StepDone
	if resume {
		gate := rn.job.Gate
		if gate == nil {
			return terminal(CodeProcessingError, errors.New("analyze marked done without a gate"), "")
		}
		// Same predicate as analyze.Output.Accepted. A rejection whose status write was
		// lost (an attempt that died between the analyze step and the reject) ends here.
		if !gate.Passed || gate.Confidence < rn.dom.Gate.MinConfidence {
			rn.log.Info("pipeline: resuming after analyze; the stored gate rejects the video")
			return rn.rejectStored(ctx)
		}
	}

	original, err := rn.downloadOriginal(ctx)
	if err != nil {
		return err
	}

	var scenes []analyze.Scene
	if resume {
		rn.log.Info("pipeline: resuming after analyze")
		rn.deletePreview(ctx) // left behind if the attempt died right after analyze
		stored, err := rn.Store.ListScenes(ctx, rn.job.ID)
		if err != nil {
			return fmt.Errorf("pipeline: list scenes: %w", err)
		}
		scenes = scenesFromStore(stored)
		rn.located = locatedFromMeta(rn.steps[store.StepNameAnalyze].Meta)
	} else {
		previewPath, err := rn.stepPreview(ctx, original)
		if err != nil {
			return err
		}
		out, err := rn.stepAnalyze(ctx, previewPath)
		if err != nil {
			return err
		}
		if !out.Accepted {
			// SaveAnalysis already moved the job to rejected and skipped the steps.
			rn.finishRejected(ctx)
			return nil
		}
		rn.deletePreview(ctx)
		scenes = out.Scenes
	}

	if len(scenes) == 0 {
		return rn.completeEmpty(ctx)
	}

	cands, err := rn.stepExtract(ctx, original, scenes)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		return rn.completeEmpty(ctx)
	}
	scored, err := rn.stepScore(ctx, cands)
	if err != nil {
		return err
	}
	sel, err := rn.stepSelect(ctx, scored)
	if err != nil {
		return err
	}
	return rn.stepExport(ctx, sel)
}

// ---- steps ----

func (rn *run) stepPreview(ctx context.Context, original string) (string, error) {
	previewPath := filepath.Join(rn.work, "preview.mp4")
	key := storage.PreviewKey(rn.job.DomainKey, rn.job.ID)
	maxSec := previewMaxDuration(rn.job.VideoInfo.DurationSec, rn.dom.Limits.MaxDurationSec)

	if rn.steps[store.StepNamePreview].Status == store.StepDone {
		if err := rn.download(ctx, key, previewPath); err == nil {
			if err := rn.checkPreview(ctx, previewPath, maxSec); err != nil {
				return "", err
			}
			if fps, ok := rn.steps[store.StepNamePreview].Meta["fps"].(float64); ok {
				rn.previewFPS = fps
			}
			return previewPath, nil
		} else if !errors.Is(err, storage.ErrNotFound) {
			return "", err
		}
		// Stored preview is gone: rebuild it.
	}

	var out *preview.Output
	err := rn.step(ctx, store.StepNamePreview, func() (any, error) {
		var err error
		out, err = preview.Build(ctx, rn.FFmpeg, preview.Input{
			SourcePath:     original,
			OutPath:        previewPath,
			Height:         rn.dom.Model.PreviewHeight,
			FPS:            rn.dom.Model.PreviewFPS,
			MaxDurationSec: maxSec,
		})
		if err != nil {
			return nil, mediaFailure(err)
		}
		// The model bills every preview frame: never send more than the validated length.
		if err := rn.checkPreview(ctx, previewPath, maxSec); err != nil {
			return nil, err
		}
		// Keep the preview until analyze succeeds so a retry does not rebuild it.
		if err := rn.upload(ctx, key, previewPath, "video/mp4"); err != nil {
			return nil, err
		}
		rn.previewFPS = out.FPS
		return map[string]any{
			"sizeBytes": out.SizeBytes, "height": rn.dom.Model.PreviewHeight,
			"fps": out.FPS, "keyframesOnly": out.KeyframesOnly, "tookMs": out.Took.Milliseconds(),
		}, nil
	})
	if err != nil {
		return "", err
	}
	return previewPath, nil
}

// previewMaxDuration is the longest preview the model may see: the validated duration
// of the original (capped by the domain limit), plus one second of encoder slack.
func previewMaxDuration(declaredSec float64, limitSec int) float64 {
	maxSec := float64(limitSec)
	if declaredSec > 0 && declaredSec < maxSec {
		maxSec = declaredSec
	}
	return maxSec + 1
}

// checkPreview fails the job terminally when the built preview is longer than maxSec
// (plus slack): the original's container header lied about its duration, and the model
// would bill the real length. It runs before any model call.
func (rn *run) checkPreview(ctx context.Context, path string, maxSec float64) error {
	info, err := rn.FFmpeg.Probe(ctx, path)
	if err != nil {
		return mediaFailure(err)
	}
	if info.DurationSec > maxSec+previewSlackSec {
		return terminal(CodeProcessingError,
			fmt.Errorf("preview is %.1f s long, more than the %.1f s allowed", info.DurationSec, maxSec),
			fmt.Sprintf("preview duration %.3f s exceeds cap %.3f s (+%.0f s slack); the original declared %.3f s "+
				"(domain limit %d s): its container duration is not trustworthy, so no model call was made",
				info.DurationSec, maxSec, previewSlackSec, rn.job.VideoInfo.DurationSec, rn.dom.Limits.MaxDurationSec))
	}
	return nil
}

// stepAnalyze makes the paid model call and records its outcome with ONE SaveAnalysis
// (gate, summary, scenes, analyze step done and, when not accepted, the rejected status),
// on a detached context with in-process retries, so an answer that was paid for is not
// lost to a database blip or a cancellation right after the call.
func (rn *run) stepAnalyze(ctx context.Context, previewPath string) (*analyze.Output, error) {
	const name = store.StepNameAnalyze
	if err := rn.ensureActive(ctx); err != nil {
		return nil, err
	}
	if err := rn.Store.StartStep(ctx, rn.job.ID, name); err != nil {
		return nil, fmt.Errorf("pipeline: start step %s: %w", name, err)
	}
	start := time.Now()
	log := rn.log.With("step", name)
	out, err := analyze.Run(ctx, rn.Analyzer, analyze.Input{
		Domain:      rn.dom,
		JobID:       rn.job.ID,
		PreviewPath: previewPath,
		DurationSec: rn.job.VideoInfo.DurationSec,
		CacheKey:    rn.job.VideoInfo.SHA256,
		SourceName:  rn.job.VideoInfo.OriginalName,
		PreviewFPS:  rn.previewFPS,
	})
	metrics.StepDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
	if out != nil {
		// Money is spent as soon as a call returns: record it before anything can fail.
		rn.recordUsage(ctx, analyzer.StepAnalyze, out.Calls, rn.job.VideoInfo.DurationSec)
		rn.releaseFile(ctx, out.FileHandle)
	}
	if err != nil {
		err = analyzeFailure(rn.dom.Model.Name, err, out)
		rn.finishFailedStep(ctx, name, err)
		log.Warn("pipeline: step error", "err", err, "took", time.Since(start))
		return nil, err
	}

	gate := store.Gate{
		Passed:          out.Gate.Passed,
		Confidence:      out.Gate.Confidence,
		Reason:          out.Gate.Reason,
		DetectedSubject: out.Gate.DetectedSubject,
	}
	meta := map[string]any{
		"accepted":       out.Accepted,
		"gateConfidence": out.Gate.Confidence,
		"scenes":         len(out.Scenes),
		"dropped":        out.Dropped,
		"calls":          len(out.Calls),
	}
	if out.Accepted && rn.dom.Locate.Enabled {
		// The contact-sheet pass adds the seconds it finds to the scene list before
		// anything is saved, so the stored scenes are the ones extract will use (also
		// after a retry). Its failure is not the job's: the analyze scenes stand alone.
		if err := rn.locate(ctx, previewPath, out, meta); err != nil {
			return nil, err
		}
	}
	res := store.AnalysisResult{
		Gate:      gate,
		Summary:   out.VideoSummary,
		ModelName: rn.dom.Model.Name,
		Scenes:    scenesToStore(rn.job.ID, out.Scenes),
		StepMeta:  meta,
		Reject:    !out.Accepted,
	}
	if err := rn.persist(ctx, "save analysis", func(ctx context.Context) error {
		return rn.Store.SaveAnalysis(ctx, rn.job.ID, res)
	}); err != nil {
		return nil, storeErr(err, "save analysis")
	}
	// Keep the in-memory job in step with the row (export writes the summary).
	rn.job.Gate = &gate
	rn.job.VideoSummary = out.VideoSummary
	if res.ModelName != "" {
		rn.job.ModelName = res.ModelName
	}
	if res.Reject {
		rn.job.Status = store.StatusRejected
		rn.job.VideoPath = ""
	}
	log.Info("pipeline: step done", "took", time.Since(start))
	return out, nil
}

// locate runs the contact-sheet pass on the preview and merges the located seconds
// into out.Scenes (analyze.MergeScenes), recording its usage under
// analyzer.StepLocate and its outcome in meta ("analyzeScenes", "located",
// "locateDropped", "locateSheets", and "locateError" when it failed). It never fails
// the step: any error, a cancellation included, leaves out.Scenes as the analyze call
// made them, so the paid analyze answer is saved either way (the caller writes it on a
// detached context). It always returns nil.
func (rn *run) locate(ctx context.Context, previewPath string, out *analyze.Output, meta map[string]any) error {
	log := rn.log.With("step", store.StepNameAnalyze)
	start := time.Now()
	loc, err := locate.Run(ctx, rn.Analyzer, locate.Input{
		Domain:      rn.dom,
		JobID:       rn.job.ID,
		PreviewPath: previewPath,
		DurationSec: rn.job.VideoInfo.DurationSec,
		WorkDir:     rn.work,
		CacheKey:    rn.job.VideoInfo.SHA256,
		FFmpeg:      rn.FFmpeg,
	})
	if loc != nil && len(loc.Calls) > 0 {
		rn.recordUsage(ctx, analyzer.StepLocate, loc.Calls, 0)
	}
	meta["analyzeScenes"] = len(out.Scenes)
	if err != nil {
		log.Warn("pipeline: locate failed; continuing with the analyze scenes", "err", err, "took", time.Since(start))
		meta["locateError"] = truncate(err.Error(), 300)
		return nil
	}
	extra := locate.Scenes(loc.Moments, rn.dom, rn.job.VideoInfo.DurationSec)
	merged, drops := analyze.MergeScenes(out.Scenes, extra, rn.dom)
	out.Scenes = merged
	out.Dropped = append(out.Dropped, drops...)
	meta["scenes"] = len(merged)
	meta["dropped"] = out.Dropped
	meta["located"] = len(loc.Moments)
	meta["locateDropped"] = loc.Dropped
	meta["locateSheets"] = loc.Sheets
	meta[locatedMomentsKey] = momentsToMeta(loc.Moments)
	rn.located = preferredSeconds(loc.Moments)
	log.Info("pipeline: locate done", "moments", len(loc.Moments), "dropped", loc.Dropped,
		"sheets", loc.Sheets, "scenes", len(merged), "took", time.Since(start))
	return nil
}

// locatedMomentsKey is the analyze step meta key of the locate pass's moments:
// [{"sec", "shotId", "whole", "rotateCw"}], read back by a resumed run.
const locatedMomentsKey = "locatedMoments"

func momentsToMeta(ms []locate.Moment) []map[string]any {
	out := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		out = append(out, map[string]any{"sec": m.Sec, "shotId": m.ShotID, "whole": m.Whole, "rotateCw": m.RotateCW})
	}
	return out
}

// preferredSeconds are the seconds per shot at which the locate pass saw the whole
// subject, in its order (review.Input.Preferred).
func preferredSeconds(ms []locate.Moment) map[string][]float64 {
	out := map[string][]float64{}
	for _, m := range ms {
		if m.Whole {
			out[m.ShotID] = append(out[m.ShotID], m.Sec)
		}
	}
	return out
}

// locatedFromMeta reads the moments a previous attempt stored (locatedMomentsKey).
func locatedFromMeta(meta map[string]any) map[string][]float64 {
	raw, ok := meta[locatedMomentsKey].([]any)
	if !ok {
		return nil
	}
	var ms []locate.Moment
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		sec, _ := m["sec"].(float64)
		shot, _ := m["shotId"].(string)
		whole, _ := m["whole"].(bool)
		if shot != "" {
			ms = append(ms, locate.Moment{Sec: sec, ShotID: shot, Whole: whole})
		}
	}
	return preferredSeconds(ms)
}

func (rn *run) stepExtract(ctx context.Context, original string, scenes []analyze.Scene) ([]extract.Candidate, error) {
	var cands []extract.Candidate
	err := rn.step(ctx, store.StepNameExtract, func() (any, error) {
		ranges := make([]extract.Range, len(scenes))
		for i, s := range scenes {
			ranges[i] = extract.Range{
				SceneIdx: s.Idx, ShotID: s.ShotID, StartSec: s.StartSec, EndSec: s.EndSec,
				Relevance: s.Relevance, Reason: s.Reason,
			}
		}
		out, err := extract.Run(ctx, rn.FFmpeg, extract.Input{
			SourcePath:    original,
			WorkDir:       rn.work,
			DurationSec:   rn.job.VideoInfo.DurationSec,
			Ranges:        ranges,
			SampleFPS:     rn.dom.Selection.SampleFPS,
			MaxCandidates: rn.dom.Limits.MaxCandidateFrames,
			PaddingSec:    rn.dom.Selection.ScenePaddingSec,
			Concurrency:   rn.ExtractConcurrency,
		})
		if err != nil {
			return nil, mediaFailure(err)
		}
		cands = out.Candidates
		plans := make([]map[string]any, len(out.Plans))
		for i, p := range out.Plans {
			plans[i] = map[string]any{
				"sceneIdx": p.SceneIdx, "shotId": p.ShotID, "startSec": p.PadStartSec,
				"endSec": p.PadEndSec, "fps": p.FPS, "expected": p.Expected,
			}
		}
		concurrency := rn.ExtractConcurrency
		if concurrency <= 0 {
			concurrency = extract.DefaultConcurrency
		}
		return map[string]any{"candidates": len(cands), "ranges": plans, "concurrency": concurrency}, nil
	})
	return cands, err
}

func (rn *run) stepScore(ctx context.Context, cands []extract.Candidate) ([]score.Scored, error) {
	var scored []score.Scored
	err := rn.step(ctx, store.StepNameScore, func() (any, error) {
		var err error
		scored, err = score.Run(ctx, cands, score.DefaultParams(rn.dom))
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, terminal(CodeProcessingError, err, err.Error())
		}
		rejected := map[string]int{}
		for _, s := range scored {
			if s.Rejected != "" {
				rejected[s.Rejected]++
			}
		}
		return map[string]any{"candidates": len(scored), "rejected": rejected}, nil
	})
	return scored, err
}

func (rn *run) stepSelect(ctx context.Context, scored []score.Scored) (*selectr.Output, error) {
	var sel *selectr.Output
	err := rn.step(ctx, store.StepNameSelect, func() (any, error) {
		params := selectr.ParamsFor(rn.dom)
		meta := map[string]any{}
		rn.scored = scored
		if rn.dom.Review.Enabled {
			assess, err := rn.reviewFrames(ctx, scored, meta)
			if err != nil {
				return nil, err
			}
			if assess != nil {
				rn.assessments = assess
				params.Assessments = selectrAssessments(assess)
			}
		}

		sel = selectr.Select(scored, params)
		if hint := cutOffHint(sel, rn.assessments); hint != "" {
			sel.Explanation += " " + hint
		}

		selected := make(map[string]bool, len(sel.Images))
		for _, im := range sel.Images {
			selected[im.Candidate.ID] = true
		}
		rows := make([]store.CandidateFrame, len(scored))
		for i, s := range scored {
			rows[i] = store.CandidateFrame{
				ID: s.ID, JobID: rn.job.ID, SceneIdx: s.SceneIdx, ShotID: s.ShotID,
				TimestampSec: s.TimestampSec, Sharpness: s.Sharpness, SharpnessZ: s.SharpnessZ,
				ExposurePenalty: s.ExposurePenalty, DHash: s.DHash, DuplicateOf: s.DuplicateOf,
				RejectedReason: s.Rejected, Score: sel.Scores[s.ID], Selected: selected[s.ID],
			}
		}
		if err := rn.Store.ReplaceCandidates(ctx, rn.job.ID, rows); err != nil {
			return nil, storeErr(err, "replace candidates")
		}
		meta["selected"] = len(sel.Images)
		meta["missingShots"] = shotIDs(sel.MissingShots)
		meta["explanation"] = sel.Explanation
		return meta, nil
	})
	return sel, err
}

func (rn *run) stepExport(ctx context.Context, sel *selectr.Output) error {
	var images []store.SelectedImage
	var zipKey string
	err := rn.step(ctx, store.StepNameExport, func() (any, error) {
		// Image IDs are new on every attempt: remove what a failed attempt uploaded so
		// it is not orphaned under the job prefix until retention.
		prefix := storage.JobPrefix(rn.job.DomainKey, rn.job.ID)
		for _, dir := range []string{"images/", "thumbs/"} {
			if err := rn.Storage.DeletePrefix(ctx, prefix+dir); err != nil {
				return nil, fmt.Errorf("pipeline: clear previous export %s: %w", dir, err)
			}
		}
		in := export.Input{
			DomainKey:   rn.job.DomainKey,
			JobID:       rn.job.ID,
			CreatedAt:   rn.job.CreatedAt,
			Summary:     rn.job.VideoSummary,
			Explanation: sel.Explanation,
		}
		for _, m := range sel.MissingShots {
			in.MissingShots = append(in.MissingShots, export.MissingShot{ID: m.ID, Label: m.Label})
		}
		for i, s := range sel.Images {
			c := s.Candidate
			edit, err := rn.editFor(c)
			if err != nil {
				return nil, err
			}
			in.Images = append(in.Images, export.Image{
				Edit:         edit,
				Index:        i + 1,
				ImageID:      ids.NewImage(),
				CandidateID:  c.ID,
				ShotID:       s.ShotID,
				ShotLabel:    rn.dom.ShotLabel(s.ShotID),
				TimestampSec: c.TimestampSec,
				FullPath:     c.FullPath,
				SmallPath:    c.SmallPath,
				Reason:       s.Reason,
				Scores: export.Scores{
					Relevance: c.Relevance, Sharpness: c.Sharpness, SharpnessZ: c.SharpnessZ,
					ExposurePenalty: c.ExposurePenalty, Score: s.Score,
				},
			})
		}
		out, err := export.Run(ctx, rn.Storage, in)
		if err != nil {
			// Storage hiccups are retried by River; extract→export is deterministic.
			return nil, fmt.Errorf("pipeline: export: %w", err)
		}
		zipKey = out.ZipKey
		for _, e := range out.Images {
			images = append(images, store.SelectedImage{
				ID: e.ImageID, JobID: rn.job.ID, CandidateID: e.CandidateID, Idx: e.Index,
				ShotID: e.ShotID, TimestampSec: e.TimestampSec, Width: e.Width, Height: e.Height,
				Reason: e.Reason, FileName: e.FileName, ImagePath: e.ImageKey, ThumbPath: e.ThumbKey,
				Bytes: e.Bytes, Rotation: e.Rotation, Crop: storeCrop(e), SubjectBox: rn.subjectBox(e),
			})
		}
		return map[string]any{"images": len(images), "zipKey": zipKey}, nil
	})
	if err != nil {
		return err
	}
	completion := store.Completion{
		Explanation:  sel.Explanation,
		MissingShots: shotIDs(sel.MissingShots),
		ZipPath:      zipKey,
		Images:       images,
	}
	rn.planStudio(&completion)
	err = rn.Store.CompleteJob(ctx, rn.job.ID, completion)
	if errors.Is(err, store.ErrConflict) {
		return errJobGone
	}
	if err != nil {
		return fmt.Errorf("pipeline: complete job: %w", err)
	}
	metrics.JobsFinished.WithLabelValues(rn.job.DomainKey, string(store.StatusCompleted)).Inc()
	rn.log.Info("pipeline: completed", "images", len(images), "missing", len(sel.MissingShots),
		"studio", completion.Studio == store.StudioPending)
	return nil
}

// subjectBox is the review's subject box of the exported frame, mapped through the
// applied rotation and crop into the exported image (the prompt of the studio
// segmentation); nil when the frame has no verdict or no usable box.
func (rn *run) subjectBox(e export.Exported) *store.NRect {
	a, ok := rn.assessments[e.CandidateID]
	if !ok || !a.Box.Valid() {
		return nil
	}
	return exportedBox(a.Box, e)
}

// exportedBox maps a box of the stored frame into the exported image e. The result is
// clamped to [0,1] and rounded to 6 decimals like the crop; nil when nothing is left.
func exportedBox(b review.Box, e export.Exported) *store.NRect {
	var crop *studio.Box
	if e.Cropped && e.Edit != nil && e.Edit.Crop != nil {
		c := e.Edit.Crop
		crop = &studio.Box{X0: c.X0, Y0: c.Y0, X1: c.X1, Y1: c.Y1}
	}
	m, ok := studio.MapBox(studio.Box{X0: b.X0, Y0: b.Y0, X1: b.X1, Y1: b.Y1}, e.Rotation, crop)
	if !ok {
		return nil
	}
	r6 := func(v float64) float64 { return math.Round(min(max(v, 0), 1)*1e6) / 1e6 }
	r := &store.NRect{X0: r6(m.X0), Y0: r6(m.Y0), X1: r6(m.X1), Y1: r6(m.Y1)}
	if math.IsNaN(m.X0+m.Y0+m.X1+m.Y1) || r.X0 >= r.X1 || r.Y0 >= r.Y1 {
		return nil
	}
	return r
}

// planStudio schedules studio backgrounds with the completion when this process can make
// them (rn.Studio), the domain has them and at least one photo is eligible
// (studiojob.Eligible): studio_status pending, the studio job inserted in the same
// transaction, and every photo that is not eligible recorded as skipped right away, so
// it is never shown as waiting for a studio version.
func (rn *run) planStudio(c *store.Completion) {
	if rn.Studio == nil || !rn.dom.Studio.Enabled {
		return
	}
	eligible := 0
	for _, im := range c.Images {
		if ok, _ := studiojob.Eligible(rn.dom, im); ok {
			eligible++
		}
	}
	if eligible == 0 {
		return
	}
	for i, im := range c.Images {
		if ok, reason := studiojob.Eligible(rn.dom, im); !ok {
			c.Images[i].Studio = &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: reason}
		}
	}
	jobID := rn.job.ID
	c.Studio = store.StudioPending
	c.Enqueue = func(ctx context.Context, tx pgx.Tx) error { return rn.Studio.EnqueueStudioTx(ctx, tx, jobID) }
}

// ---- outcomes ----

// rejectStored rejects the job from the gate stored by an earlier attempt's analyze
// step (the resume path): nothing is paid again.
func (rn *run) rejectStored(ctx context.Context) error {
	gate := *rn.job.Gate
	if err := rn.persist(ctx, "reject job", func(ctx context.Context) error {
		return rn.Store.RejectJob(ctx, rn.job.ID, gate, rn.job.VideoSummary)
	}); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return errJobGone
		}
		return fmt.Errorf("pipeline: reject job: %w", err)
	}
	rn.skipRemaining(ctx)
	rn.finishRejected(ctx)
	return nil
}

// finishRejected cleans up after the job was rejected by the gate.
func (rn *run) finishRejected(ctx context.Context) {
	rn.deleteFiles(ctx) // nothing to retain for a rejected video
	metrics.JobsFinished.WithLabelValues(rn.job.DomainKey, string(store.StatusRejected)).Inc()
	var confidence float64
	var subject string
	if g := rn.job.Gate; g != nil {
		confidence, subject = g.Confidence, g.DetectedSubject
	}
	rn.log.Info("pipeline: rejected by gate", "confidence", confidence, "subject", subject)
}

func (rn *run) completeEmpty(ctx context.Context) error {
	rn.skipRemaining(ctx)
	var missing []string
	for _, s := range rn.dom.Shots {
		if s.Required {
			missing = append(missing, s.ID)
		}
	}
	err := rn.Store.CompleteJob(ctx, rn.job.ID, store.Completion{Explanation: NoScenesExplanation, MissingShots: missing})
	if errors.Is(err, store.ErrConflict) {
		return errJobGone
	}
	if err != nil {
		return fmt.Errorf("pipeline: complete job: %w", err)
	}
	metrics.JobsFinished.WithLabelValues(rn.job.DomainKey, string(store.StatusCompleted)).Inc()
	rn.log.Info("pipeline: completed without scenes")
	return nil
}

// fail marks the job failed (terminal); it returns nil so River does not retry, or
// an error if even that could not be recorded.
//
// FailJob runs on a detached context with in-process retries: when it still cannot be
// recorded, River retries the job and earlierFailure re-applies the failure from the
// failed step without redoing (and re-paying for) the work.
func (rn *run) fail(ctx context.Context, te *terminalError) error {
	msg := userMessages[te.code]
	if msg == "" {
		msg = userMessages[CodeProcessingError]
	}
	internal := te.err.Error()
	if te.internal != "" && te.internal != internal {
		internal += "\n" + te.internal
	}
	rn.log.Error("pipeline: job failed", "code", te.code, "err", te.err)
	err := rn.persist(ctx, "fail job", func(ctx context.Context) error {
		return rn.Store.FailJob(ctx, rn.job.ID, te.code, msg, internal)
	})
	if err != nil && !errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("pipeline: fail job: %w", err)
	}
	rn.skipRemaining(ctx)
	rn.deleteFiles(ctx) // failed jobs keep no files
	if err == nil {
		metrics.JobsFinished.WithLabelValues(rn.job.DomainKey, string(store.StatusFailed)).Inc()
	}
	return nil
}

// earlierFailure returns the terminal error of a step an earlier attempt marked failed.
// A step is only marked failed for a terminal error, so the job is still active here
// only because that attempt could not record FailJob: re-apply the failure instead of
// redoing the work (after a paid analyze call, doing it again would pay again).
func (rn *run) earlierFailure() error {
	for _, name := range store.StepNames {
		st := rn.steps[name]
		if st.Status != store.StepFailed {
			continue
		}
		code, _ := st.Meta["error"].(string)
		if _, known := userMessages[code]; !known {
			code = CodeProcessingError
		}
		return terminal(code, fmt.Errorf("step %s failed in an earlier attempt that could not record the failure", name), "")
	}
	return nil
}

// ---- helpers ----

// step records start/finish, duration and meta for one step and checks between
// steps whether the user deleted the job.
func (rn *run) step(ctx context.Context, name string, fn func() (any, error)) error {
	if err := rn.ensureActive(ctx); err != nil {
		return err
	}
	if err := rn.Store.StartStep(ctx, rn.job.ID, name); err != nil {
		return fmt.Errorf("pipeline: start step %s: %w", name, err)
	}
	start := time.Now()
	log := rn.log.With("step", name)
	meta, err := fn()
	metrics.StepDuration.WithLabelValues(name).Observe(time.Since(start).Seconds())
	if err != nil {
		rn.finishFailedStep(ctx, name, err)
		log.Warn("pipeline: step error", "err", err, "took", time.Since(start))
		return err
	}
	if err := rn.Store.FinishStep(ctx, rn.job.ID, name, store.StepDone, meta); err != nil {
		return fmt.Errorf("pipeline: finish step %s: %w", name, err)
	}
	log.Info("pipeline: step done", "took", time.Since(start))
	return nil
}

func (rn *run) ensureActive(ctx context.Context) error {
	j, err := rn.Store.GetJob(ctx, rn.job.ID)
	if err != nil {
		return fmt.Errorf("pipeline: reload job: %w", err)
	}
	if j.Status != store.StatusProcessing {
		return errJobGone
	}
	return nil
}

// finishFailedStep marks the step failed when err is terminal (the job is about to
// fail). Other errors leave it running so River's retry redoes it. The mark is what
// earlierFailure re-applies if FailJob cannot be recorded, so it is persisted like one.
func (rn *run) finishFailedStep(ctx context.Context, name string, err error) {
	var te *terminalError
	if !errors.As(err, &te) {
		return
	}
	if ferr := rn.persist(ctx, "finish failed step", func(ctx context.Context) error {
		return rn.Store.FinishStep(ctx, rn.job.ID, name, store.StepFailed, map[string]any{"error": te.code})
	}); ferr != nil {
		rn.log.Warn("pipeline: finish failed step", "step", name, "err", ferr)
	}
}

func (rn *run) skipRemaining(ctx context.Context) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if err := rn.Store.SkipRemainingSteps(cctx, rn.job.ID); err != nil {
		rn.log.Warn("pipeline: skip remaining steps", "err", err)
	}
}

func (rn *run) deleteFiles(ctx context.Context) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	prefix := storage.JobPrefix(rn.job.DomainKey, rn.job.ID)
	if err := rn.Storage.DeletePrefix(cctx, prefix); err != nil {
		rn.log.Warn("pipeline: delete job files", "prefix", prefix, "err", err)
	}
}

// deletePreview removes the stored preview once the analyze outcome is recorded.
func (rn *run) deletePreview(ctx context.Context) {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if err := rn.Storage.Delete(cctx, storage.PreviewKey(rn.job.DomainKey, rn.job.ID)); err != nil {
		rn.log.Warn("pipeline: delete preview", "err", err)
	}
}

// releaseFile deletes the provider copy of the preview (handles are reused within one
// attempt only).
func (rn *run) releaseFile(ctx context.Context, handle string) {
	if handle == "" {
		return
	}
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	if err := rn.Analyzer.ReleaseFile(cctx, handle); err != nil {
		rn.log.Warn("pipeline: release model file", "handle", handle, "err", err)
	}
}

// cleanupCtx is ctx without its cancellation (cleanup must also run when the job was
// cancelled) but bounded by cleanupTimeout, so a hung provider or storage cannot pin
// the worker slot past the job timeout.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

// persist runs write on ctx without its cancellation, each attempt bounded by
// persistTimeout, retrying with backoff: it records money already spent or a terminal
// outcome, which a cancelled job context or a database blip must not throw away.
// ErrConflict and ErrNotFound are final and returned at once.
func (rn *run) persist(ctx context.Context, what string, write func(context.Context) error) error {
	backoff := rn.persistBackoff
	if backoff == nil {
		backoff = defaultPersistBackoff
	}
	for attempt := 0; ; attempt++ {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), persistTimeout)
		err := write(wctx)
		cancel()
		if err == nil || errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) || attempt >= len(backoff) {
			return err
		}
		rn.log.Warn("pipeline: write failed; retrying", "what", what, "attempt", attempt+1, "err", err)
		time.Sleep(backoff[attempt])
	}
}

func (rn *run) downloadOriginal(ctx context.Context) (string, error) {
	if rn.job.VideoPath == "" {
		return "", terminal(CodeProcessingError, errors.New("original video is missing"), "")
	}
	ext := strings.TrimPrefix(path.Ext(rn.job.VideoPath), ".")
	if ext == "" {
		ext = "mp4"
	}
	dst := filepath.Join(rn.work, "original."+ext)
	if err := rn.download(ctx, rn.job.VideoPath, dst); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return "", terminal(CodeProcessingError, fmt.Errorf("original %s not in storage", rn.job.VideoPath), "")
		}
		return "", err
	}
	return dst, nil
}

func (rn *run) download(ctx context.Context, key, dst string) error {
	rc, err := rn.Storage.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("pipeline: get %s: %w", key, err)
	}
	defer rc.Close()
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("pipeline: create %s: %w", dst, err)
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		return fmt.Errorf("pipeline: download %s: %w", key, err)
	}
	return f.Close()
}

func (rn *run) upload(ctx context.Context, key, src, contentType string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("pipeline: open %s: %w", src, err)
	}
	defer f.Close()
	if err := rn.Storage.Put(ctx, key, f, contentType); err != nil {
		return fmt.Errorf("pipeline: put %s: %w", key, err)
	}
	return nil
}

// recordUsage writes one usage_events row per model call. A failure to record is
// logged, not returned: the call already happened and retrying the job would pay again.
func (rn *run) recordUsage(ctx context.Context, step string, calls []analyzer.Usage, videoSec float64) {
	for _, u := range calls {
		model := u.Model
		if model == "" {
			model = rn.dom.Model.Name
		}
		cost, known := pricing.Cost(model, u.InputTokens, u.OutputTokens, u.ThinkingTokens)
		if !known && rn.Analyzer.Name() != "fake" {
			rn.log.Warn("pipeline: no price for model; cost recorded as 0", "model", model)
		}
		ev := store.UsageEvent{
			JobID: rn.job.ID, DomainKey: rn.job.DomainKey, Step: step, Model: model,
			InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, ThinkingTokens: u.ThinkingTokens,
			VideoSeconds: videoSec, CostUSD: cost, LatencyMs: int(u.Latency.Milliseconds()),
		}
		if err := rn.persist(ctx, "record usage", func(ctx context.Context) error {
			return rn.Store.InsertUsage(ctx, ev)
		}); err != nil {
			rn.log.Error("pipeline: record usage", "err", err, "cost_usd", cost)
		}
		metrics.ModelLatency.WithLabelValues(model, step).Observe(u.Latency.Seconds())
		metrics.ModelTokens.WithLabelValues(model, "input").Add(float64(u.InputTokens))
		metrics.ModelTokens.WithLabelValues(model, "output").Add(float64(u.OutputTokens))
		metrics.ModelTokens.WithLabelValues(model, "thinking").Add(float64(u.ThinkingTokens))
		metrics.ModelCostUSD.WithLabelValues(rn.job.DomainKey, model).Add(cost)
	}
}

// analyzeFailure classifies an analyze.Run error.
func analyzeFailure(model string, err error, out *analyze.Output) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, analyze.ErrBadOutput):
		metrics.ModelErrors.WithLabelValues(model, "bad_output").Inc()
		raw := ""
		var bo *analyze.BadOutputError
		if errors.As(err, &bo) {
			raw = bo.Raw
		} else if out != nil {
			raw = out.RawText
		}
		return terminal(CodeModelBadOutput, err, "raw model output:\n"+truncate(raw, 16<<10))
	case errors.Is(err, analyzer.ErrUnavailable):
		metrics.ModelErrors.WithLabelValues(model, "unavailable").Inc()
		return terminal(CodeModelUnavailable, err, "")
	default:
		metrics.ModelErrors.WithLabelValues(model, "other").Inc()
		return terminal(CodeProcessingError, err, "")
	}
}

// mediaFailure turns ffmpeg errors into terminal failures (deterministic, no retry)
// but lets cancellation through so River can retry after a worker restart.
func mediaFailure(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	internal := ""
	var ee *media.ExecError
	if errors.As(err, &ee) {
		internal = ee.Cmd + "\n" + ee.StderrTail
	}
	return terminal(CodeProcessingError, err, internal)
}

func storeErr(err error, what string) error {
	if errors.Is(err, store.ErrConflict) {
		return errJobGone
	}
	return fmt.Errorf("pipeline: %s: %w", what, err)
}

func scenesToStore(jobID string, scenes []analyze.Scene) []store.Scene {
	out := make([]store.Scene, len(scenes))
	for i, s := range scenes {
		out[i] = store.Scene{
			JobID: jobID, Idx: s.Idx, StartSec: s.StartSec, EndSec: s.EndSec, ShotID: s.ShotID,
			Relevance: s.Relevance, Reason: s.Reason, RawStart: s.RawStart, RawEnd: s.RawEnd,
		}
	}
	return out
}

func scenesFromStore(scenes []store.Scene) []analyze.Scene {
	out := make([]analyze.Scene, len(scenes))
	for i, s := range scenes {
		out[i] = analyze.Scene{
			Idx: s.Idx, StartSec: s.StartSec, EndSec: s.EndSec, ShotID: s.ShotID,
			Relevance: s.Relevance, Reason: s.Reason, RawStart: s.RawStart, RawEnd: s.RawEnd,
		}
	}
	return out
}

func shotIDs(shots []domain.Shot) []string {
	out := make([]string, len(shots))
	for i, s := range shots {
		out[i] = s.ID
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}

// Select step meta keys of the model review outcome. Verdicts and orientation answers
// are keyed by the candidates' (scene shot, timestamp): a retry (e.g. after an export
// hiccup) re-extracts the same frames with new IDs. The outcome is merged into the
// step's meta as soon as the review returns, before selection and export, so a River
// retry reuses it instead of paying again. Meta written before these keys existed
// (only review, reviewSent, reviewUsable, reviewError) still loads.
const (
	reviewMetaKey             = "review"                 // []storedAssessment: the verdicts
	reviewRotationsKey        = "reviewRotations"        // []storedRotation: the orientation answer
	reviewSentKey             = "reviewSent"             // frames sent
	reviewAnsweredKey         = "reviewAnswered"         // frames with a verdict
	reviewUsableKey           = "reviewUsable"           // usable verdicts
	reviewErrorKey            = "reviewError"            // why the verdicts are not used (text)
	reviewFailureKey          = "reviewFailure"          // failure kind, one of reviewFail*
	reviewOrientationErrorKey = "reviewOrientationError" // the orientation call failed: frames judged as stored
	reviewReusedKey           = "reviewReused"           // this attempt reused an earlier attempt's outcome
)

// reviewKeys are every key of the review outcome: a new outcome replaces all of them.
var reviewKeys = []string{
	reviewMetaKey, reviewRotationsKey, reviewSentKey, reviewAnsweredKey, reviewUsableKey,
	reviewErrorKey, reviewFailureKey, reviewOrientationErrorKey, reviewReusedKey,
}

// Kinds of review failure (reviewFailureKey). Only reviewFailUnavailable is retried by a
// later attempt: the others were paid for (or are deterministic) and would fail again.
const (
	reviewFailBadOutput   = "badOutput"   // the judgement answer could not be parsed
	reviewFailTooFew      = "tooFew"      // fewer than half of the frames sent got a verdict
	reviewFailUnavailable = "unavailable" // no judgement answer: provider down or the call timed out
	reviewFailOther       = "other"       // any other error (e.g. the frames could not be prepared)
)

// rotationReachSec is how far from an oriented frame of the same scene editFor applies
// its rotation to a frame the orientation call did not see (the phone's orientation does
// not change within a second).
const rotationReachSec = 1.0

// storedAssessment is one review verdict as kept in job_steps.meta.
type storedAssessment struct {
	// SceneIdx tells apart two scenes of one shot whose padded ranges overlap (they
	// sample the same timestamps). nil in meta stored before it was kept.
	SceneIdx     *int       `json:"sceneIdx,omitempty"`
	ShotID       string     `json:"shotId"`
	TimestampSec float64    `json:"timestampSec"`
	RotateCW     int        `json:"rotateCw"`
	View         string     `json:"view"`
	FullyVisible bool       `json:"fullyVisible"`
	Quality      int        `json:"quality"`
	Box          [4]float64 `json:"box"`
	Issues       string     `json:"issues,omitempty"`
	// Usable is informational: a reuse recomputes it with the current minimum quality.
	Usable bool `json:"usable"`
	// SameAs is the index (in this stored list) of the frame this one duplicates, -1 none.
	SameAs int `json:"sameAs"`
}

// UnmarshalJSON defaults SameAs to -1: meta stored before near-duplicate links existed
// has no sameAs key, and 0 would link every frame to the first one.
func (s *storedAssessment) UnmarshalJSON(b []byte) error {
	type plain storedAssessment // without this method
	p := plain{SameAs: -1}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = storedAssessment(p)
	return nil
}

// storedRotation is one orientation answer as kept in job_steps.meta.
type storedRotation struct {
	SceneIdx     *int    `json:"sceneIdx,omitempty"` // see storedAssessment.SceneIdx
	ShotID       string  `json:"shotId"`
	TimestampSec float64 `json:"timestampSec"`
	RotateCW     int     `json:"rotateCw"`
}

// reviewFrames runs (or, on a retry, reuses) the model review of the shortlisted
// frames. It returns nil assessments when the review failed, answered fewer than half
// of the frames sent, or could not run: the job then continues on scores alone (the
// review is a refinement, not a gate), with the orientation answer still applied at
// export when that call succeeded (rn.rotations).
func (rn *run) reviewFrames(ctx context.Context, scored []score.Scored, meta map[string]any) (map[string]review.Assessment, error) {
	if assess, done := rn.reuseReview(scored, meta); done {
		return assess, nil
	}
	pre := make(map[string]float64, len(scored))
	for _, s := range scored {
		pre[s.ID] = selectr.Score(s, rn.dom.Selection.Weights)
	}
	out, err := review.Run(ctx, rn.Analyzer, review.Input{
		Domain: rn.dom, JobID: rn.job.ID, CacheKey: rn.job.VideoInfo.SHA256,
		WorkDir: rn.work, Candidates: scored, Scores: pre, Preferred: rn.located,
	})
	if out == nil {
		out = &review.Output{}
	}
	rn.recordUsage(ctx, analyzer.StepReview, out.Calls, 0)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err == nil && out.Sent == 0 {
		meta[reviewSentKey] = 0 // nothing eligible to review, no call made
		return nil, nil
	}

	outcome := make(map[string]any, len(reviewKeys)) // nil values clear an earlier attempt's keys
	for _, k := range reviewKeys {
		outcome[k] = nil
	}
	outcome[reviewSentKey] = out.Sent
	if len(out.Rotations) > 0 {
		rn.rotations = out.Rotations
		outcome[reviewRotationsKey] = rotationsToMeta(out.Rotations, scored)
	}
	if out.OrientationErr != nil {
		outcome[reviewOrientationErrorKey] = out.OrientationErr.Error()
	}
	defer rn.saveReview(ctx, outcome, meta) // on every return below, before selection runs

	if err != nil {
		kind := reviewFailureKind(err)
		rn.log.Warn("pipeline: frame review failed; selecting on scores alone", "err", err, "kind", kind)
		outcome[reviewErrorKey] = err.Error()
		outcome[reviewFailureKey] = kind
		return nil, nil
	}
	answered := len(out.Assessments)
	outcome[reviewAnsweredKey] = answered
	if 2*answered < out.Sent {
		// An empty or mostly missing answer says nothing about the frames: treating every
		// frame without a verdict as rejected would end a paid job with no photos.
		msg := fmt.Sprintf("review answered %d of %d frames", answered, out.Sent)
		rn.log.Warn("pipeline: frame review incomplete; selecting on scores alone", "answered", answered, "sent", out.Sent)
		outcome[reviewErrorKey] = msg
		outcome[reviewFailureKey] = reviewFailTooFew
		return nil, nil
	}
	outcome[reviewUsableKey] = countUsable(out.Assessments)
	outcome[reviewMetaKey] = assessmentsToMeta(out.Assessments, scored)
	return out.Assessments, nil
}

// reuseReview applies the review outcome an earlier attempt stored in the select step's
// meta. done is false when the review must run (again): nothing stored, the provider was
// unavailable, the verdicts no longer map onto this attempt's candidates (the sampling
// changed), or the frames were judged without the orientation (it must be redone).
func (rn *run) reuseReview(scored []score.Scored, meta map[string]any) (map[string]review.Assessment, bool) {
	prev := rn.steps[store.StepNameSelect].Meta
	if len(prev) == 0 {
		return nil, false
	}
	keep := func() {
		for _, k := range reviewKeys {
			if v, ok := prev[k]; ok && v != nil {
				meta[k] = v
			}
		}
		meta[reviewReusedKey] = true
		rn.rotations = rotationsFromMeta(prev[reviewRotationsKey], scored)
	}

	if raw := prev[reviewMetaKey]; raw != nil && prev[reviewOrientationErrorKey] == nil {
		assess, n, ok := assessmentsFromMeta(raw, scored, reviewMinQuality(rn.dom.Review))
		sent, _ := prev[reviewSentKey].(float64)
		switch {
		case !ok:
			rn.log.Warn("pipeline: stored review verdicts do not match this attempt's frames; reviewing again", "stored", n)
			return nil, false
		case n == 0 || 2*float64(n) < sent:
			// Stored before an incomplete answer counted as a failure (or nothing was
			// sent): the outcome is now scores alone, without paying again.
			keep()
			delete(meta, reviewMetaKey)
			if sent > 0 {
				meta[reviewErrorKey] = fmt.Sprintf("review answered %d of %.0f frames", n, sent)
				meta[reviewFailureKey] = reviewFailTooFew
			}
			return nil, true
		}
		keep()
		meta[reviewUsableKey] = countUsable(assess)
		return assess, true
	}
	if kind, _ := prev[reviewFailureKey].(string); kind != "" && kind != reviewFailUnavailable {
		rn.log.Info("pipeline: the frame review failed in an earlier attempt; selecting on scores alone", "kind", kind)
		keep()
		return nil, true
	}
	return nil, false
}

// saveReview copies the review outcome into the step meta and merges it into the stored
// select step right away (the step's final meta only lands after selection), so a retry
// does not pay for the review again. A failed write is logged: the outcome still lands
// with the step's final meta.
func (rn *run) saveReview(ctx context.Context, outcome, meta map[string]any) {
	for k, v := range outcome {
		if v != nil {
			meta[k] = v
		}
	}
	if err := rn.persist(ctx, "save review", func(ctx context.Context) error {
		return rn.Store.MergeStepMeta(ctx, rn.job.ID, store.StepNameSelect, outcome)
	}); err != nil {
		rn.log.Warn("pipeline: save review outcome; a retry may pay for the review again", "err", err)
	}
}

// reviewFailureKind classifies a review.Run error (reviewFailureKey).
func reviewFailureKind(err error) string {
	switch {
	case errors.Is(err, analyze.ErrBadOutput):
		return reviewFailBadOutput
	case errors.Is(err, analyzer.ErrUnavailable), errors.Is(err, context.DeadlineExceeded):
		return reviewFailUnavailable
	default:
		return reviewFailOther
	}
}

// reviewMinQuality is the domain's minimum usable quality, with review's default.
func reviewMinQuality(r domain.Review) int {
	if r.MinQuality > 0 {
		return r.MinQuality
	}
	return review.DefaultMinQuality
}

func countUsable(assess map[string]review.Assessment) int {
	n := 0
	for _, a := range assess {
		if a.Usable {
			n++
		}
	}
	return n
}

func assessmentsToMeta(assess map[string]review.Assessment, scored []score.Scored) []storedAssessment {
	out := make([]storedAssessment, 0, len(assess))
	pos := map[string]int{}
	for _, s := range scored {
		a, ok := assess[s.ID]
		if !ok {
			continue
		}
		pos[s.ID] = len(out)
		out = append(out, storedAssessment{
			SceneIdx: &s.SceneIdx, ShotID: s.ShotID, TimestampSec: s.TimestampSec, RotateCW: a.RotateCW, View: a.View,
			FullyVisible: a.FullyVisible, Quality: a.Quality, Box: [4]float64{a.Box.X0, a.Box.Y0, a.Box.X1, a.Box.Y1},
			Issues: a.Issues, Usable: a.Usable, SameAs: -1,
		})
	}
	for _, s := range scored {
		if a, ok := assess[s.ID]; ok && a.DuplicateOf != "" {
			if j, ok := pos[a.DuplicateOf]; ok {
				out[pos[s.ID]].SameAs = j
			}
		}
	}
	return out
}

// assessmentsFromMeta maps stored verdicts back onto this attempt's candidates (see
// matchCandidate) and recomputes Usable from the verdict with minQuality, the current
// domain minimum. n is the number of stored verdicts. ok is false when the meta cannot
// be decoded or any stored verdict matches no candidate (not taken by an earlier one):
// the verdicts then belong to a different sampling and must not be reused, since every
// candidate without a verdict would count as rejected.
func assessmentsFromMeta(raw any, scored []score.Scored, minQuality int) (assess map[string]review.Assessment, n int, ok bool) {
	var stored []storedAssessment
	if !decodeStored(raw, &stored) {
		return nil, 0, false
	}
	assess = make(map[string]review.Assessment, len(stored))
	ids := make([]string, len(stored)) // stored index → this attempt's candidate ID
	taken := make(map[int]bool, len(stored))
	for k, st := range stored {
		i := matchCandidate(scored, st.SceneIdx, st.ShotID, st.TimestampSec, taken)
		if i < 0 {
			return nil, len(stored), false
		}
		id := scored[i].ID
		ids[k] = id
		a := review.Assessment{
			CandidateID: id, RotateCW: st.RotateCW, View: st.View, FullyVisible: st.FullyVisible,
			Quality: st.Quality, Box: review.Box{X0: st.Box[0], Y0: st.Box[1], X1: st.Box[2], Y1: st.Box[3]},
			Issues: st.Issues,
		}
		a.Usable = a.FullyVisible && a.Quality >= minQuality && a.View != review.ViewNone
		assess[id] = a
	}
	for k, st := range stored {
		if st.SameAs >= 0 && st.SameAs < len(ids) && st.SameAs != k {
			a := assess[ids[k]]
			a.DuplicateOf = ids[st.SameAs]
			assess[ids[k]] = a
		}
	}
	return assess, len(stored), true
}

func rotationsToMeta(rot map[string]int, scored []score.Scored) []storedRotation {
	out := make([]storedRotation, 0, len(rot))
	for _, s := range scored {
		if r, ok := rot[s.ID]; ok {
			out = append(out, storedRotation{SceneIdx: &s.SceneIdx, ShotID: s.ShotID, TimestampSec: s.TimestampSec, RotateCW: r})
		}
	}
	return out
}

// rotationsFromMeta maps stored orientation answers back onto this attempt's candidates.
// It is best effort (the answer only straightens exported frames): entries that match no
// candidate or hold no right angle are dropped, and undecodable meta gives nil.
func rotationsFromMeta(raw any, scored []score.Scored) map[string]int {
	var stored []storedRotation
	if raw == nil || !decodeStored(raw, &stored) {
		return nil
	}
	out := make(map[string]int, len(stored))
	taken := make(map[int]bool, len(stored))
	for _, st := range stored {
		if st.RotateCW%90 != 0 || st.RotateCW < 0 || st.RotateCW >= 360 {
			continue
		}
		if i := matchCandidate(scored, st.SceneIdx, st.ShotID, st.TimestampSec, taken); i >= 0 {
			out[scored[i].ID] = st.RotateCW
		}
	}
	return out
}

// matchCandidate returns the index in scored of the first candidate not yet taken with
// this scene shot and timestamp (within 1 ms: the sampling plan is deterministic) and,
// when sceneIdx is known, this scene, and marks it taken; -1 if there is none. Stored
// entries are in scored order, so entries without a scene still pair up in order.
func matchCandidate(scored []score.Scored, sceneIdx *int, shotID string, ts float64, taken map[int]bool) int {
	const tolerance = 1e-3
	for i, s := range scored {
		if taken[i] || s.ShotID != shotID || math.Abs(s.TimestampSec-ts) > tolerance ||
			(sceneIdx != nil && s.SceneIdx != *sceneIdx) {
			continue
		}
		taken[i] = true
		return i
	}
	return -1
}

// decodeStored decodes a value read back from job_steps.meta (generic JSON) into dst.
func decodeStored(raw, dst any) bool {
	b, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, dst) == nil
}

func selectrAssessments(assess map[string]review.Assessment) map[string]selectr.Assessment {
	out := make(map[string]selectr.Assessment, len(assess))
	for id, a := range assess {
		view := a.View
		if view == review.ViewNone {
			view = domain.OtherShotID // never usable anyway
		}
		out[id] = selectr.Assessment{View: view, Usable: a.Usable, Quality: a.Quality, DuplicateOf: a.DuplicateOf}
	}
	return out
}

// editFor plans how a selected frame is reframed: rotated upright per the review and,
// with crop enabled, cropped to the domain's aspect around the reviewed subject box. A
// frame without a verdict (the judgement failed) is only rotated, per rotationFor.
// nil means the frame is exported as extracted.
func (rn *run) editFor(c score.Scored) (*export.Edit, error) {
	a, ok := rn.assessments[c.ID]
	if !ok {
		rot, _ := rn.rotationFor(c)
		return export.NewEdit(rot, nil), nil
	}
	if !rn.dom.Crop.Enabled || !a.Box.Valid() {
		return export.NewEdit(a.RotateCW, nil), nil
	}
	w, h, err := jpegSize(c.FullPath)
	if err != nil {
		return nil, terminal(CodeProcessingError, err, "")
	}
	params, err := crop.ParamsFor(rn.dom.Crop)
	if err != nil {
		return nil, terminal(CodeProcessingError, err, "")
	}
	plan := crop.Plan(crop.Frame{
		Width: w, Height: h, RotateCW: a.RotateCW,
		Box: [4]float64{a.Box.X0, a.Box.Y0, a.Box.X1, a.Box.Y1},
	}, params)
	rn.log.Debug("pipeline: crop plan", "candidate", c.ID, "rotate", plan.RotateCW, "cropped", plan.Crop != nil, "reason", plan.Reason)
	return export.NewEdit(plan.RotateCW, plan.Crop), nil
}

// rotationFor is the orientation answer for c: its own, else that of the nearest
// oriented frame of the same scene within rotationReachSec. ok is false when neither
// exists (the frame is then exported as extracted).
func (rn *run) rotationFor(c score.Scored) (rot int, ok bool) {
	if r, found := rn.rotations[c.ID]; found {
		return r, true
	}
	best := math.Inf(1)
	for _, s := range rn.scored {
		r, found := rn.rotations[s.ID]
		if !found || s.SceneIdx != c.SceneIdx {
			continue
		}
		if d := math.Abs(s.TimestampSec - c.TimestampSec); d <= rotationReachSec && d < best {
			best, rot, ok = d, r, true
		}
	}
	return rot, ok
}

func jpegSize(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("pipeline: open frame: %w", err)
	}
	defer f.Close()
	cfg, err := jpeg.DecodeConfig(f)
	if err != nil {
		return 0, 0, fmt.Errorf("pipeline: frame size %s: %w", path, err)
	}
	return cfg.Width, cfg.Height, nil
}

func storeCrop(e export.Exported) *store.NRect {
	if !e.Cropped || e.Edit == nil || e.Edit.Crop == nil {
		return nil
	}
	c := e.Edit.Crop
	return &store.NRect{X0: c.X0, Y0: c.Y0, X1: c.X1, Y1: c.Y1}
}

// cutOffHint explains missing views the review did see but never with the whole subject
// inside the frame (a close walkaround filmed in portrait), so the user knows to film
// them from further away instead of assuming the model missed them. A view with at least
// one fully visible take is left out: it went missing for another reason (blur, a
// duplicate), and "film from further away" would be the wrong advice.
func cutOffHint(sel *selectr.Output, assess map[string]review.Assessment) string {
	if sel == nil || len(assess) == 0 || len(sel.MissingShots) == 0 {
		return ""
	}
	seen, visible := map[string]bool{}, map[string]bool{}
	for _, a := range assess {
		seen[a.View] = true
		if a.FullyVisible {
			visible[a.View] = true
		}
	}
	var labels []string
	for _, m := range sel.MissingShots {
		if seen[m.ID] && !visible[m.ID] {
			labels = append(labels, m.Label)
		}
	}
	if len(labels) == 0 {
		return ""
	}
	return "The frame edge cut off part of the subject in every take of: " + strings.Join(labels, ", ") +
		". Film these views from further away so the whole subject fits in the picture."
}
