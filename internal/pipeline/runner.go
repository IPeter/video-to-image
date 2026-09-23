// Package pipeline orchestrates the processing steps for one job (plan/03-pipeline.md).
// Step packages never touch the database; the Runner does, recording progress,
// duration and cost per step in job_steps and usage_events.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/media"
	"video-to-image/internal/metrics"
	"video-to-image/internal/pipeline/analyze"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/pipeline/extract"
	"video-to-image/internal/pipeline/preview"
	"video-to-image/internal/pipeline/rerank"
	"video-to-image/internal/pipeline/score"
	"video-to-image/internal/pipeline/selectr"
	"video-to-image/internal/pricing"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
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
	SkipRemainingSteps(ctx context.Context, jobID string) error
	ListScenes(ctx context.Context, jobID string) ([]store.Scene, error)
	ReplaceCandidates(ctx context.Context, jobID string, cands []store.CandidateFrame) error
	InsertUsage(ctx context.Context, e store.UsageEvent) error
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

	// persistBackoff overrides defaultPersistBackoff (tests).
	persistBackoff []time.Duration
}

// Bounds for work that must not be cut short by the job context (it runs on
// context.WithoutCancel) but must not hang a worker slot either.
const (
	// persistTimeout bounds one attempt of a write that records money already spent
	// (the analyze outcome, usage) or a terminal outcome (FailJob, RejectJob, the
	// failed-step mark).
	persistTimeout = 15 * time.Second
	// cleanupTimeout bounds best-effort cleanup: provider file release, storage
	// deletes, skipping the remaining steps.
	cleanupTimeout = 30 * time.Second
	// previewSlackSec is how much longer than its cap a built preview may be (encoder
	// rounding) before it is treated as a lying container header.
	previewSlackSec = 1.0
	// rerankPicksMetaKey is the select step's meta key holding the re-rank picks as
	// shotId → candidate timestamp, reused on a retry instead of paying for a new call.
	rerankPicksMetaKey = "rerankPreferred"
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
}

// Run processes jobID. It returns nil once the job reached a terminal status (set by
// the runner itself), and an error only for infrastructure failures (database,
// storage, cancellation) that River should retry.
//
// Paid work is recorded before anything else can fail: the analyze outcome (gate,
// summary, scenes, step done and, for a rejection, the rejected status) is written in
// one transaction on a detached context and retried in-process, and a terminal failure
// is recorded the same way. Re-delivery resumes after a done analyze step (and reuses
// stored re-rank picks), and re-applies a failure an earlier attempt decided but could
// not record, so a retry does not pay for the model again. The model is called again
// only when an attempt died between the model's answer and the database (for example
// the database stayed down for every in-process retry).
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
		return map[string]any{"sizeBytes": out.SizeBytes, "height": rn.dom.Model.PreviewHeight, "fps": rn.dom.Model.PreviewFPS}, nil
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
	res := store.AnalysisResult{
		Gate:      gate,
		Summary:   out.VideoSummary,
		ModelName: rn.dom.Model.Name,
		Scenes:    scenesToStore(rn.job.ID, out.Scenes),
		StepMeta: map[string]any{
			"accepted":       out.Accepted,
			"gateConfidence": out.Gate.Confidence,
			"scenes":         len(out.Scenes),
			"dropped":        out.Dropped,
			"calls":          len(out.Calls),
		},
		Reject: !out.Accepted,
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
		return map[string]any{"candidates": len(cands), "ranges": plans}, nil
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
		if rn.dom.Selection.ModelRerank {
			if picks, ok := storedRerankPicks(rn.steps[store.StepNameSelect].Meta); ok {
				// A retry (e.g. after an export hiccup): candidate IDs are new, but the
				// frames are the same, so map the stored picks back by shot and timestamp.
				params.Preferred = picksToCandidates(picks, scored)
				meta["rerankReused"] = true
			} else {
				pre := make(map[string]float64, len(scored))
				for _, s := range scored {
					pre[s.ID] = selectr.Score(s, rn.dom.Selection.Weights)
				}
				rr, err := rerank.Run(ctx, rn.Analyzer, rerank.Input{
					Domain: rn.dom, JobID: rn.job.ID, CacheKey: rn.job.VideoInfo.SHA256,
					Candidates: scored, Scores: pre,
				})
				if rr != nil {
					rn.recordUsage(ctx, analyzer.StepRerank, rr.Calls, 0)
					params.Preferred = rr.Preferred
				}
				if err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					// The re-rank is an optional refinement: continue on sharpness alone.
					rn.log.Warn("pipeline: rerank failed; continuing without it", "err", err)
					meta["rerankError"] = err.Error()
				}
			}
			meta["rerankPicks"] = len(params.Preferred)
			// Also stored when empty, so a retry reproduces this selection without a call.
			meta[rerankPicksMetaKey] = candidatesToPicks(params.Preferred, scored)
		}

		sel = selectr.Select(scored, params)

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
			in.Images = append(in.Images, export.Image{
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
				Bytes: e.Bytes,
			})
		}
		return map[string]any{"images": len(images), "zipKey": zipKey}, nil
	})
	if err != nil {
		return err
	}
	err = rn.Store.CompleteJob(ctx, rn.job.ID, store.Completion{
		Explanation:  sel.Explanation,
		MissingShots: shotIDs(sel.MissingShots),
		ZipPath:      zipKey,
		Images:       images,
	})
	if errors.Is(err, store.ErrConflict) {
		return errJobGone
	}
	if err != nil {
		return fmt.Errorf("pipeline: complete job: %w", err)
	}
	metrics.JobsFinished.WithLabelValues(rn.job.DomainKey, string(store.StatusCompleted)).Inc()
	rn.log.Info("pipeline: completed", "images", len(images), "missing", len(sel.MissingShots))
	return nil
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

// storedRerankPicks reads the re-rank picks (shotId → candidate timestamp) an earlier
// attempt stored in the select step's meta. ok is false when there are none.
func storedRerankPicks(meta map[string]any) (map[string]float64, bool) {
	raw, ok := meta[rerankPicksMetaKey].(map[string]any)
	if !ok {
		return nil, false
	}
	picks := make(map[string]float64, len(raw))
	for shot, v := range raw {
		if ts, ok := v.(float64); ok {
			picks[shot] = ts
		}
	}
	return picks, true
}

// candidatesToPicks turns re-rank preferences (shotId → candidate ID) into shotId →
// timestamp, which survives re-extraction (candidate IDs do not).
func candidatesToPicks(preferred map[string]string, scored []score.Scored) map[string]float64 {
	byID := make(map[string]float64, len(scored))
	for _, s := range scored {
		byID[s.ID] = s.TimestampSec
	}
	picks := make(map[string]float64, len(preferred))
	for shot, id := range preferred {
		if ts, ok := byID[id]; ok {
			picks[shot] = ts
		}
	}
	return picks
}

// picksToCandidates maps stored picks back onto this attempt's candidates: for each
// shot, the candidate of that shot closest to the stored timestamp (within 1 ms; the
// sampling plan is deterministic). Picks without a match are dropped.
func picksToCandidates(picks map[string]float64, scored []score.Scored) map[string]string {
	const tolerance = 1e-3
	preferred := make(map[string]string, len(picks))
	for shot, ts := range picks {
		best, bestDiff := "", math.Inf(1)
		for _, s := range scored {
			if d := math.Abs(s.TimestampSec - ts); s.ShotID == shot && d <= tolerance && d < bestDiff {
				best, bestDiff = s.ID, d
			}
		}
		if best != "" {
			preferred[shot] = best
		}
	}
	return preferred
}
