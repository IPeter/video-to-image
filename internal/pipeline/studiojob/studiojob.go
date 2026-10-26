// Package studiojob makes the studio backgrounds of a completed job (River job "studio",
// internal/queue.StudioWorker). For every eligible exterior photo it cuts the car out
// with the segmentation sidecar (studio.Segmenter), checks the cut-out (the gates, then a
// model QA check), renders every configured backdrop around the unchanged car pixels,
// stores the variants and records the photo's outcome right away; then it builds
// studio.zip. It runs after the job completed, so users see the normal photos first and
// the studio versions appear afterwards. The job's own status and its original photos
// are never changed.
package studiojob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"video-to-image/internal/analyzer"
	"video-to-image/internal/domain"
	"video-to-image/internal/metrics"
	"video-to-image/internal/pricing"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
	"video-to-image/internal/studio"
)

// Defaults for zero Runner fields.
const (
	DefaultPhotoTimeout = 45 * time.Second // V2I_STUDIO_TIMEOUT
	DefaultQATimeout    = 90 * time.Second
	DefaultGenTimeout   = 120 * time.Second // one image-model call (~10-20 s seen)
)

// Bounds for writes that must survive a cancelled job context (see persist).
const (
	persistTimeout = 15 * time.Second
	cleanupTimeout = 30 * time.Second
)

// defaultPersistBackoff is the wait before each retry of a persist write: 4 attempts.
var defaultPersistBackoff = []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond, 4 * time.Second}

// errJobGone means the job was deleted (expired) or its studio work ended elsewhere.
var errJobGone = errors.New("studiojob: job no longer waiting for studio work")

// Store is the subset of *store.Store the studio job uses.
type Store interface {
	GetJob(ctx context.Context, id string) (*store.Job, error)
	ListSelectedImages(ctx context.Context, jobID string) ([]store.SelectedImage, error)
	StartStudio(ctx context.Context, jobID string) error
	SetImageStudio(ctx context.Context, jobID, imageID string, st store.ImageStudio) error
	FinishStudio(ctx context.Context, jobID string, f store.StudioFinish) error
	InsertUsage(ctx context.Context, e store.UsageEvent) error
	CostSince(ctx context.Context, since time.Time) (float64, error)
}

var _ Store = (*store.Store)(nil)

// Runner makes the studio backgrounds of one job at a time; it is safe for concurrent
// use (all per-job state lives on the stack).
type Runner struct {
	Store     Store
	Storage   storage.Storage
	Segmenter studio.Segmenter
	Analyzer  analyzer.Analyzer // the QA check
	Registry  *domain.Registry
	Logger    *slog.Logger
	// ScratchDir holds the QA images while their check runs (analyzer parts are files),
	// in <ScratchDir>/jobs/<jobId>.studio; "" → the system temp dir.
	ScratchDir string
	// PhotoTimeout bounds the segmentation of one photo (≤ 0 → DefaultPhotoTimeout).
	PhotoTimeout time.Duration
	// QATimeout bounds one QA model call (≤ 0 → DefaultQATimeout).
	QATimeout time.Duration
	// GenTimeout bounds one generative-studio image call (≤ 0 → DefaultGenTimeout).
	GenTimeout time.Duration
	// DailyCostCeilingUSD > 0: no QA call is made once today's model spend (since 00:00
	// UTC) reached it; the photos not done yet are skipped and the job's studio work
	// ends as skipped.
	DailyCostCeilingUSD float64
	Now                 func() time.Time // default time.Now

	persistBackoff []time.Duration // tests
}

// run holds the state of one job's studio work.
type run struct {
	*Runner
	job    *store.Job
	dom    *domain.Config
	log    *slog.Logger
	work   string   // scratch dir of this job's QA images
	styles []string // render and display order, the default first
	gates  studio.Gates
}

// Run makes the studio backgrounds of jobID. It returns nil once the job's studio work
// has a final status (done, failed or skipped), or when there is nothing to do: the job
// is gone, not completed, or its studio work is not pending or running. It is idempotent
// and resumable: photos that already have an outcome are skipped. It returns an error
// for River to retry when the sidecar is unreachable (wrapping
// studio.ErrSegmenterUnavailable) or the database or storage failed; a problem with one
// photo (including a segmentation timeout) only marks that photo failed.
func (r *Runner) Run(ctx context.Context, jobID string) error {
	log := r.logger().With("job_id", jobID, "step", analyzer.StepStudio)
	job, err := r.Store.GetJob(ctx, jobID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		log.Info("studio: job not found; nothing to do")
		return nil
	case err != nil:
		return fmt.Errorf("studiojob: load job: %w", err)
	}
	log = log.With("domain", job.DomainKey)
	if job.Status != store.StatusCompleted || !job.StudioStatus.Active() {
		log.Info("studio: nothing to do", "status", job.Status, "studio_status", job.StudioStatus)
		return nil
	}
	if err := r.Store.StartStudio(ctx, jobID); err != nil {
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			log.Info("studio: job no longer waiting for studio work; stopping", "err", err)
			return nil
		}
		return fmt.Errorf("studiojob: start: %w", err)
	}
	rn := &run{Runner: r, job: job, log: log}
	dom, ok := r.Registry.Get(job.DomainKey)
	if !ok {
		log.Error("studio: domain is not configured; studio work failed")
		err = rn.finish(ctx, store.StudioFailed, "", &store.ImageStudio{Status: store.ImageStudioFailed, Reason: ReasonFailed})
	} else {
		rn.dom = dom
		start := time.Now()
		err = rn.execute(ctx)
		metrics.StepDuration.WithLabelValues(analyzer.StepStudio).Observe(time.Since(start).Seconds())
	}
	if errors.Is(err, errJobGone) {
		log.Info("studio: job deleted meanwhile; stopping")
		rn.deleteFiles(ctx)
		return nil
	}
	return err
}

// execute works through the photos without an outcome, one segmentation at a time (the
// sidecar serves one request at a time). The QA call of a photo runs while the next
// photo is segmented, bounded to one photo ahead: photo i is recorded before photo i+1's
// QA call starts. Every outcome is stored as soon as it is known.
func (rn *run) execute(ctx context.Context) error {
	images, err := rn.Store.ListSelectedImages(ctx, rn.job.ID)
	if err != nil {
		return fmt.Errorf("studiojob: list images: %w", err)
	}
	rn.styles = rn.dom.Studio.EffectiveStyles()
	rn.gates = studio.GatesFor(rn.dom.Studio.Gates)
	scratch := rn.ScratchDir
	if scratch == "" {
		scratch = os.TempDir()
	}
	rn.work = filepath.Join(scratch, "jobs", rn.job.ID+".studio")
	_ = os.RemoveAll(rn.work) // leftovers of a crashed attempt
	if err := os.MkdirAll(rn.work, 0o755); err != nil {
		return fmt.Errorf("studiojob: scratch dir: %w", err)
	}
	defer os.RemoveAll(rn.work)

	var todo []store.SelectedImage
	for _, im := range images {
		if im.Studio != nil {
			continue // decided at completion or by an earlier attempt
		}
		if ok, reason := Eligible(rn.dom, im); !ok { // the domain changed since completion
			if err := rn.save(ctx, im, store.ImageStudio{Status: store.ImageStudioSkipped, Reason: reason}); err != nil {
				return err
			}
			continue
		}
		todo = append(todo, im)
	}
	rn.log.Info("studio: started", "photos", len(todo), "decided", len(images)-len(todo), "styles", rn.styles)

	var inflight *qaCall // the QA call of the previous photo
	// settle records the in-flight photo (its QA call was paid for) before returning err.
	settle := func(err error) error {
		if inflight == nil {
			return err
		}
		res := inflight.wait()
		inflight = nil
		return errors.Join(err, rn.complete(ctx, res))
	}
	for _, im := range todo {
		if err := rn.ensureActive(ctx); err != nil {
			return settle(err)
		}
		p, err := rn.prepare(ctx, im)
		if err != nil {
			return settle(err)
		}
		if err := settle(nil); err != nil {
			return err
		}
		if (!p.needsQA && p.gen == nil) || p.outcome != nil {
			if err := rn.complete(ctx, qaResult{p: p}); err != nil {
				return err
			}
			continue
		}
		over, err := rn.overCeiling(ctx)
		if err != nil {
			return err
		}
		if over {
			rn.log.Warn("studio: daily cost ceiling reached; remaining photos skipped", "ceiling_usd", rn.DailyCostCeilingUSD)
			return rn.finish(ctx, store.StudioSkipped, "", &store.ImageStudio{Status: store.ImageStudioSkipped, Reason: ReasonCeiling})
		}
		inflight = rn.startChecks(ctx, p)
	}
	if err := settle(nil); err != nil {
		return err
	}
	return rn.finishDone(ctx)
}

// finishDone builds studio.zip from the ready photos (none: no ZIP) and ends the studio
// work as done. Photos somehow still without an outcome are marked failed.
func (rn *run) finishDone(ctx context.Context) error {
	images, err := rn.Store.ListSelectedImages(ctx, rn.job.ID)
	if err != nil {
		return fmt.Errorf("studiojob: list images: %w", err)
	}
	zipKey := ""
	if countStatus(images, store.ImageStudioReady) > 0 {
		zipKey = ZipKey(rn.job.DomainKey, rn.job.ID)
		if err := rn.putZip(ctx, zipKey, images); err != nil {
			return err
		}
	}
	if err := rn.finish(ctx, store.StudioDone, zipKey, &store.ImageStudio{Status: store.ImageStudioFailed, Reason: ReasonFailed}); err != nil {
		return err
	}
	rn.log.Info("studio: done", "ready", countStatus(images, store.ImageStudioReady),
		"withheld", countStatus(images, store.ImageStudioWithheld), "failed", countStatus(images, store.ImageStudioFailed),
		"skipped", countStatus(images, store.ImageStudioSkipped), "zip", zipKey != "")
	return nil
}

// finish ends the running studio work (see store.FinishStudio).
func (rn *run) finish(ctx context.Context, status store.StudioStatus, zipKey string, unprocessed *store.ImageStudio) error {
	err := rn.persist(ctx, "finish studio", func(ctx context.Context) error {
		return rn.Store.FinishStudio(ctx, rn.job.ID, store.StudioFinish{Status: status, ZipPath: zipKey, Unprocessed: unprocessed})
	})
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		return errJobGone
	case err != nil:
		return fmt.Errorf("studiojob: finish: %w", err)
	}
	return nil
}

// save records one photo's outcome (a detached, retried write: a QA call may have been
// paid for it).
func (rn *run) save(ctx context.Context, im store.SelectedImage, st store.ImageStudio) error {
	err := rn.persist(ctx, "save photo outcome", func(ctx context.Context) error {
		return rn.Store.SetImageStudio(ctx, rn.job.ID, im.ID, st)
	})
	switch {
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
		return errJobGone
	case err != nil:
		return fmt.Errorf("studiojob: save outcome of %s: %w", im.ID, err)
	}
	rn.log.Info("studio: photo done", "image_id", im.ID, "shot", im.ShotID, "status", st.Status, "reason", st.Reason)
	return nil
}

// ensureActive checks between photos that the job is still completed with its studio
// work running (the user may delete the job at any time).
func (rn *run) ensureActive(ctx context.Context) error {
	j, err := rn.Store.GetJob(ctx, rn.job.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errJobGone
	case err != nil:
		return fmt.Errorf("studiojob: reload job: %w", err)
	case j.Status != store.StatusCompleted || j.StudioStatus != store.StudioRunning:
		return errJobGone
	}
	return nil
}

// overCeiling reports whether today's model spend reached the daily ceiling.
func (rn *run) overCeiling(ctx context.Context) (bool, error) {
	if rn.DailyCostCeilingUSD <= 0 {
		return false, nil
	}
	now := time.Now
	if rn.Now != nil {
		now = rn.Now
	}
	y, m, d := now().UTC().Date()
	cost, err := rn.Store.CostSince(ctx, time.Date(y, m, d, 0, 0, 0, 0, time.UTC))
	if err != nil {
		return false, fmt.Errorf("studiojob: check cost ceiling: %w", err)
	}
	return cost >= rn.DailyCostCeilingUSD, nil
}

// deleteFiles removes the studio uploads of a job that was deleted meanwhile (retention
// and user deletion sweep the job prefix, but uploads can land after their sweep). When
// the job is still completed its studio work ended elsewhere and the files it references
// are kept.
func (rn *run) deleteFiles(ctx context.Context) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if j, err := rn.Store.GetJob(cctx, rn.job.ID); err == nil && j.Status == store.StatusCompleted {
		return
	}
	d, id := rn.job.DomainKey, rn.job.ID
	if err := rn.Storage.DeletePrefix(cctx, Prefix(d, id)); err != nil {
		rn.log.Warn("studio: delete studio files", "err", err)
	}
	if err := rn.Storage.Delete(cctx, ZipKey(d, id)); err != nil {
		rn.log.Warn("studio: delete studio.zip", "err", err)
	}
}

// recordUsage writes one usage_events row for a model call (step "studio" for the QA
// check, "studio-gen" for the generative background), counted in the job's cost and the
// daily ceiling. A failure to record is logged, not returned.
func (rn *run) recordUsage(ctx context.Context, step, model string, u analyzer.Usage) {
	if step == "" {
		step = analyzer.StepStudio
	}
	if u.Model != "" {
		model = u.Model
	}
	cost, known := pricing.Cost(model, u.InputTokens, u.OutputTokens, u.ThinkingTokens)
	if !known && rn.Analyzer.Name() != "fake" {
		rn.log.Warn("studio: no price for model; cost recorded as 0", "model", model)
	}
	ev := store.UsageEvent{
		JobID: rn.job.ID, DomainKey: rn.job.DomainKey, Step: step, Model: model,
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, ThinkingTokens: u.ThinkingTokens,
		CostUSD: cost, LatencyMs: int(u.Latency.Milliseconds()),
	}
	if err := rn.persist(ctx, "record usage", func(ctx context.Context) error {
		return rn.Store.InsertUsage(ctx, ev)
	}); err != nil {
		rn.log.Error("studio: record usage", "err", err, "cost_usd", cost)
	}
	metrics.ModelLatency.WithLabelValues(model, step).Observe(u.Latency.Seconds())
	metrics.ModelTokens.WithLabelValues(model, "input").Add(float64(u.InputTokens))
	metrics.ModelTokens.WithLabelValues(model, "output").Add(float64(u.OutputTokens))
	metrics.ModelTokens.WithLabelValues(model, "thinking").Add(float64(u.ThinkingTokens))
	metrics.ModelCostUSD.WithLabelValues(rn.job.DomainKey, model).Add(cost)
}

// persist runs write on ctx without its cancellation, each attempt bounded by
// persistTimeout, retrying with backoff: it records money already spent or an outcome,
// which a cancelled job context or a database blip must not throw away. ErrConflict and
// ErrNotFound are final.
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
		rn.log.Warn("studio: write failed; retrying", "what", what, "attempt", attempt+1, "err", err)
		time.Sleep(backoff[attempt])
	}
}

func (r *Runner) logger() *slog.Logger {
	if r.Logger == nil {
		return slog.Default()
	}
	return r.Logger
}

func (r *Runner) photoTimeout() time.Duration {
	if r.PhotoTimeout > 0 {
		return r.PhotoTimeout
	}
	return DefaultPhotoTimeout
}

func (r *Runner) qaTimeout() time.Duration {
	if r.QATimeout > 0 {
		return r.QATimeout
	}
	return DefaultQATimeout
}

func (r *Runner) genTimeout() time.Duration {
	if r.GenTimeout > 0 {
		return r.GenTimeout
	}
	return DefaultGenTimeout
}

// Unavailable reports whether a Segmenter error means the sidecar itself could not be
// used (studio.ErrSegmenterUnavailable: unreachable, or busy / still loading until the
// timeout; or, from another Segmenter, a refused, reset or dropped connection) rather
// than a problem with the photo. A timeout (studio.ErrSegmenterTimeout), a rejected
// input and a bad answer are problems with the photo.
func Unavailable(err error) bool {
	var op *net.OpError
	var dns *net.DNSError
	switch {
	case err == nil:
		return false
	case errors.Is(err, studio.ErrSegmenterUnavailable):
		return true
	case errors.Is(err, studio.ErrSegmenterTimeout), errors.Is(err, studio.ErrSegmenterRejected),
		errors.Is(err, studio.ErrSegmenterBadResponse):
		return false
	case errors.As(err, &op), errors.As(err, &dns),
		errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, syscall.ECONNRESET),
		errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	return false
}

func countStatus(images []store.SelectedImage, status string) int {
	n := 0
	for _, im := range images {
		if im.Studio != nil && im.Studio.Status == status {
			n++
		}
	}
	return n
}
