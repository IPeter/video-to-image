package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"video-to-image/internal/analyzer/fake"
	"video-to-image/internal/domain"
	"video-to-image/internal/ids"
	"video-to-image/internal/pipeline/export"
	"video-to-image/internal/pricing"
	"video-to-image/internal/storage"
	"video-to-image/internal/store"
)

// stepWeights are the progress-bar shares of the pipeline steps (sum 100). The model
// call dominates wall time, so analyze carries the most weight.
var stepWeights = map[string]float64{
	store.StepNamePreview: 10,
	store.StepNameAnalyze: 40,
	store.StepNameExtract: 20,
	store.StepNameScore:   15,
	store.StepNameSelect:  5,
	store.StepNameExport:  10,
}

// Safe default messages for failed jobs whose row carries no message.
var jobErrorMessages = map[JobErrorCode]string{
	JobErrorCode(CodeModelUnavailable): "The analysis service is temporarily unavailable. Please try again later.",
	JobErrorCode(CodeModelBadOutput):   "The analysis returned an unexpected result. Please try again.",
	JobErrorCode(CodeProcessingError):  "Something went wrong while processing the video.",
}

const (
	genericRejectionReason = "The video does not show what this site accepts."
	// genericSubject stands in for a gate subject that does not shorten well.
	genericSubject = "the subject this site accepts"
	// maxSubjectRunes caps the subject phrase quoted in a low-confidence rejection.
	maxSubjectRunes = 80
)

func errJobNotFound() *APIError {
	return NewAPIError(http.StatusNotFound, CodeJobNotFound, "job not found")
}

func errExpired() *APIError {
	return NewAPIError(http.StatusGone, CodeExpired, "the job's files have expired")
}

// jobPath is the job's API path; image, thumbnail and ZIP URLs hang off it.
func jobPath(domainKey, jobID string) string { return domainsPrefix + domainKey + "/jobs/" + jobID }

// loadJob resolves the domain and job of a job-scoped route. A malformed ID, an
// unknown job and a job of ANOTHER domain all read JOB_NOT_FOUND, so one domain's
// frontend can never probe for another's jobs.
func (s *Server) loadJob(ctx context.Context, domainKey, jobID string) (*domain.Config, *store.Job, error) {
	dc, err := s.domainFor(ctx, domainKey)
	if err != nil {
		return nil, nil, err
	}
	if !ids.Valid(ids.PrefixJob, jobID) {
		return nil, nil, errJobNotFound()
	}
	noteJob(ctx, jobID)
	job, err := s.d.Store.GetJob(ctx, jobID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, errJobNotFound()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get job: %w", err)
	}
	if job.DomainKey != dc.Key {
		return nil, nil, errJobNotFound()
	}
	return dc, job, nil
}

// getJob serves the polling payload.
func (s *Server) getJob(ctx context.Context, in *JobPathInput) (*GetJobOutput, error) {
	dc, job, err := s.loadJob(ctx, in.DomainKey, in.JobID)
	if err != nil {
		return nil, s.apiError(ctx, "get job", err)
	}
	if job.Status == store.StatusExpired {
		return nil, errExpired()
	}
	dto, err := s.jobDTO(ctx, dc, job)
	if err != nil {
		return nil, s.apiError(ctx, "get job", err)
	}
	return &GetJobOutput{CacheControl: "no-store", Body: dto}, nil
}

// jobDTO maps a job row (plus its steps, its model usage and, when completed, its
// images with their studio versions) to the wire.
func (s *Server) jobDTO(ctx context.Context, dc *domain.Config, job *store.Job) (Job, error) {
	steps, err := s.d.Store.ListSteps(ctx, job.ID)
	if err != nil {
		return Job{}, fmt.Errorf("list steps: %w", err)
	}
	out := Job{
		JobID:     job.ID,
		DomainKey: job.DomainKey,
		Status:    JobStatus(job.Status),
		CreatedAt: job.CreatedAt.UTC(),
		ExpiresAt: job.ExpiresAt.UTC(),
		Video: VideoInfo{
			DurationSec: job.VideoInfo.DurationSec,
			Width:       job.VideoInfo.Width,
			Height:      job.VideoInfo.Height,
			FPS:         job.VideoInfo.FPS,
		},
		Progress: buildProgress(job.Status, steps),
		Usage:    s.jobUsage(ctx, dc, job.ID),
	}
	switch job.Status {
	case store.StatusCompleted:
		images, err := s.d.Store.ListSelectedImages(ctx, job.ID)
		if err != nil {
			return Job{}, fmt.Errorf("list images: %w", err)
		}
		out.Result = buildResult(dc, job, images)
	case store.StatusRejected:
		out.Rejection = buildRejection(dc, job.Gate)
	case store.StatusFailed:
		out.Error = buildJobError(job.ErrorCode, job.ErrorMessage)
	}
	out.Studio = buildJobStudio(dc, job, out.Result)
	return out, nil
}

// jobUsage reads the job's model usage. It is informational, so a store error is
// logged and reads as zero usage (with the cost marked incomplete) instead of
// failing the poll.
func (s *Server) jobUsage(ctx context.Context, dc *domain.Config, jobID string) JobUsage {
	rows, err := s.d.Store.JobUsage(ctx, jobID)
	if err != nil {
		s.d.Logger.WarnContext(ctx, "httpapi: get job: usage unavailable, reporting zero",
			"domain", dc.Key, "job_id", jobID, "err", err)
		u := buildUsage(nil)
		u.CostComplete = false
		return u
	}
	return buildUsage(rows)
}

// buildUsage totals the per-step usage rows. TotalTokens counts input, output and
// thinking tokens; costs are rounded to micro-dollars (the usage_events scale), so
// float sums never show as 0.30000000000000004. Steps is never nil. A step is priced
// when the price table knows its model or it is the test analyzer's; TestMode means
// every call was the test analyzer's, CostComplete that every step is priced (a real
// model missing from the table is recorded at cost 0, which is not "free").
func buildUsage(rows []store.UsageByStep) JobUsage {
	u := JobUsage{Steps: make([]UsageStep, 0, len(rows)), TestMode: len(rows) > 0, CostComplete: true}
	var cost float64
	for _, r := range rows {
		test := isTestModel(r.Model)
		_, known := pricing.Lookup(r.Model)
		priced := known || test
		u.TestMode = u.TestMode && test
		u.CostComplete = u.CostComplete && priced
		u.Steps = append(u.Steps, UsageStep{
			Step:           r.Step,
			Model:          r.Model,
			Calls:          r.Calls,
			InputTokens:    r.InputTokens,
			OutputTokens:   r.OutputTokens,
			ThinkingTokens: r.ThinkingTokens,
			CostUSD:        roundUSD(r.CostUSD),
			Priced:         priced,
		})
		u.Calls += r.Calls
		u.InputTokens += r.InputTokens
		u.OutputTokens += r.OutputTokens
		u.ThinkingTokens += r.ThinkingTokens
		cost += r.CostUSD
	}
	u.TotalTokens = u.InputTokens + u.OutputTokens + u.ThinkingTokens
	u.CostUSD = roundUSD(cost)
	return u
}

// isTestModel reports usage recorded by the built-in test analyzer (fake.ModelPrefix).
func isTestModel(model string) bool { return strings.HasPrefix(model, fake.ModelPrefix) }

// roundUSD rounds a dollar amount to 6 decimals.
func roundUSD(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// buildProgress lists the steps in pipeline order. Done and skipped steps count fully
// towards percent, the running step half; a terminal job is always 100.
func buildProgress(status store.JobStatus, steps []store.Step) Progress {
	byName := make(map[string]store.Step, len(steps))
	for _, st := range steps {
		byName[st.Name] = st
	}
	p := Progress{Steps: make([]ProgressStep, 0, len(store.StepNames))}
	var percent float64
	for _, name := range store.StepNames {
		st, ok := byName[name]
		if !ok {
			st = store.Step{Name: name, Status: store.StepPending}
		}
		ps := ProgressStep{Name: StepName(name), Status: StepStatus(st.Status)}
		if st.DurationMs != nil {
			ms := *st.DurationMs
			ps.Ms = &ms
		}
		p.Steps = append(p.Steps, ps)
		switch st.Status {
		case store.StepDone, store.StepSkipped:
			percent += stepWeights[name]
		case store.StepRunning:
			percent += stepWeights[name] / 2
			if p.Step == nil {
				n := StepName(name)
				p.Step = &n
			}
		}
	}
	if status.Terminal() {
		percent = 100
	}
	p.Percent = int(math.Min(100, math.Max(0, math.Floor(percent))))
	return p
}

func buildResult(dc *domain.Config, job *store.Job, images []store.SelectedImage) *JobResult {
	images = slices.Clone(images)
	slices.SortStableFunc(images, func(a, b store.SelectedImage) int { return a.Idx - b.Idx })
	base := jobPath(dc.Key, job.ID)

	res := &JobResult{
		Summary:      job.VideoSummary,
		Images:       make([]ResultImage, 0, len(images)),
		MissingShots: make([]ShotRef, 0, len(job.MissingShots)),
		Explanation:  job.ResultExplanation,
	}
	for _, img := range images {
		u := base + "/images/" + img.ID
		res.Images = append(res.Images, ResultImage{
			ID:             img.ID,
			Index:          img.Idx,
			ShotID:         img.ShotID,
			ShotLabel:      dc.ShotLabel(img.ShotID),
			TimestampSec:   img.TimestampSec,
			TimestampLabel: export.TimestampLabel(img.TimestampSec),
			Width:          img.Width,
			Height:         img.Height,
			Reason:         img.Reason,
			FileName:       img.FileName,
			URL:            u,
			DownloadURL:    u + "?download=1",
			ThumbnailURL:   u + "/thumb",
			Rotation:       img.Rotation,
			Cropped:        img.Crop != nil,
			Studio:         buildImageStudio(dc, job, &img, u),
		})
	}
	for _, id := range job.MissingShots {
		res.MissingShots = append(res.MissingShots, ShotRef{ID: id, Label: dc.ShotLabel(id)})
	}
	if job.ZipPath != "" && len(res.Images) > 0 {
		z := base + "/download.zip"
		res.ZipURL = &z
	}
	return res
}

// buildRejection explains a rejected job. A gate that PASSED but was rejected anyway
// fell below the domain's minConfidence: the model's own reason and detected subject
// then argue for the video ("a silver sedan on a lot") and would contradict the
// rejection, so a neutral reason is shown instead (the raw gate stays in jobs.gate).
func buildRejection(dc *domain.Config, g *store.Gate) *Rejection {
	if g != nil && g.Passed {
		return &Rejection{Reason: lowConfidenceReason(dc)}
	}
	if g == nil || g.Reason == "" {
		rej := &Rejection{Reason: genericRejectionReason}
		if g != nil {
			rej.DetectedSubject = g.DetectedSubject
		}
		return rej
	}
	return &Rejection{Reason: g.Reason, DetectedSubject: g.DetectedSubject}
}

// lowConfidenceReason is the rejection reason of a low-confidence pass.
func lowConfidenceReason(dc *domain.Config) string {
	return "We couldn't tell with enough certainty that this video shows " + shortSubject(dc.Gate.Subject) +
		". Try a steadier, well-lit video that keeps the subject in frame."
}

// shortSubject turns a gate subject (a prompt phrase such as "a single passenger
// vehicle (car, SUV, van or pickup) being shown or walked around, as for a sales
// listing") into a short phrase for user copy: parentheticals dropped, cut at the
// first clause break, whitespace collapsed. Anything unusable → genericSubject.
func shortSubject(subject string) string {
	var b strings.Builder
	depth := 0
	for _, r := range subject {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	phrase := b.String()
	if i := strings.IndexAny(phrase, ",;:.!?\n"); i >= 0 {
		phrase = phrase[:i]
	}
	phrase = strings.Join(strings.Fields(phrase), " ")
	if phrase == "" || depth != 0 || utf8.RuneCountInString(phrase) > maxSubjectRunes {
		return genericSubject
	}
	return phrase
}

// buildJobError only ever exposes the three job error codes, with a safe message.
func buildJobError(code, message string) *JobError {
	c := JobErrorCode(code)
	if _, ok := jobErrorMessages[c]; !ok {
		c = JobErrorCode(CodeProcessingError)
	}
	if message == "" {
		message = jobErrorMessages[c]
	}
	return &JobError{Code: c, Message: message}
}

// deleteJob removes the job's files now and marks it expired. Deleting an expired job
// again is a no-op (204).
//
// The order closes the race with a worker still finishing the job:
//
//  1. Delete the files. When this fails the job stays visible and the user can retry,
//     instead of an expired job whose files nobody would ever clean up.
//  2. Expire the job. From here on the worker's CompleteJob fails with ErrConflict and
//     the worker deletes what it uploaded itself.
//  3. Delete the files again: a worker that uploaded after the listing in step 1 AND
//     committed before step 2 left objects behind, and retention never revisits an
//     expired job. Best effort: a failure is logged, the 3-day bucket lifecycle rule
//     is the backstop.
func (s *Server) deleteJob(ctx context.Context, in *JobPathInput) (*struct{}, error) {
	dc, job, err := s.loadJob(ctx, in.DomainKey, in.JobID)
	if err != nil {
		return nil, s.apiError(ctx, "delete job", err)
	}
	if job.Status == store.StatusExpired {
		return nil, nil
	}
	prefix := storage.JobPrefix(dc.Key, job.ID)
	if err := s.d.Storage.DeletePrefix(ctx, prefix); err != nil {
		return nil, s.apiError(ctx, "delete job", fmt.Errorf("delete files of %s: %w", job.ID, err))
	}
	if err := s.d.Store.ExpireJob(ctx, job.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, s.apiError(ctx, "delete job", fmt.Errorf("expire %s: %w", job.ID, err))
	}
	sweepCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := s.d.Storage.DeletePrefix(sweepCtx, prefix); err != nil {
		s.d.Logger.WarnContext(ctx, "httpapi: delete job: second file sweep failed", "domain", dc.Key, "job_id", job.ID, "err", err)
	}
	s.d.Logger.InfoContext(ctx, "httpapi: job deleted by user", "domain", dc.Key, "job_id", job.ID)
	return nil, nil
}
