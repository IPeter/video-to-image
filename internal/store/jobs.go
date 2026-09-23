package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"video-to-image/internal/store/sqlcdb"
)

// MaxInternalLogBytes caps jobs.internal_log: appends keep the last 64 KiB of UTF-8
// (cut on a character boundary).
const MaxInternalLogBytes = 64 << 10

// CreateJob inserts the job (status queued) and one pending job_steps row per
// StepNames entry, then calls enqueue with the same transaction (River InsertTx) and
// commits. If enqueue fails, nothing is committed: a job never exists without work
// scheduled, or vice versa. A nil enqueue schedules nothing (tools and tests).
func (s *Store) CreateJob(ctx context.Context, j NewJob, enqueue func(ctx context.Context, tx pgx.Tx) error) error {
	switch {
	case j.ID == "":
		return errors.New("store: create job: empty id")
	case j.DomainKey == "":
		return fmt.Errorf("store: create job %s: empty domain key", j.ID)
	case j.ExpiresAt.IsZero():
		return fmt.Errorf("store: create job %s: zero expiry", j.ID)
	}
	info, err := jsonb(j.VideoInfo)
	if err != nil {
		return fmt.Errorf("store: create job %s: video info: %w", j.ID, err)
	}
	err = s.inTx(ctx, func(tx pgx.Tx, q *sqlcdb.Queries) error {
		if err := q.InsertJob(ctx, sqlcdb.InsertJobParams{
			ID:           j.ID,
			DomainKey:    j.DomainKey,
			ExpiresAt:    j.ExpiresAt,
			ClientIpHash: j.ClientIPHash,
			VideoPath:    nullText(j.VideoPath),
			VideoInfo:    info,
			ModelName:    j.ModelName,
		}); err != nil {
			return fmt.Errorf("insert job: %w", err)
		}
		if err := q.InsertSteps(ctx, sqlcdb.InsertStepsParams{JobID: j.ID, Names: StepNames}); err != nil {
			return fmt.Errorf("insert steps: %w", err)
		}
		if enqueue != nil {
			if err := enqueue(ctx, tx); err != nil {
				return fmt.Errorf("enqueue: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: create job %s: %w", j.ID, err)
	}
	return nil
}

// GetJob returns the job or ErrNotFound.
func (s *Store) GetJob(ctx context.Context, id string) (*Job, error) {
	row, err := s.q.GetJob(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("store: get job %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("store: get job %s: %w", id, err)
	}
	j, err := jobFromRow(row)
	if err != nil {
		return nil, fmt.Errorf("store: get job %s: %w", id, err)
	}
	return j, nil
}

func jobFromRow(r sqlcdb.Job) (*Job, error) {
	j := &Job{
		ID:                r.ID,
		DomainKey:         r.DomainKey,
		Status:            JobStatus(r.Status),
		CreatedAt:         r.CreatedAt.UTC(),
		UpdatedAt:         r.UpdatedAt.UTC(),
		ExpiresAt:         r.ExpiresAt.UTC(),
		ClientIPHash:      r.ClientIpHash,
		VideoPath:         textOrEmpty(r.VideoPath),
		ModelName:         r.ModelName,
		VideoSummary:      r.VideoSummary,
		ResultExplanation: r.ResultExplanation,
		ZipPath:           textOrEmpty(r.ZipPath),
		ErrorCode:         textOrEmpty(r.ErrorCode),
		ErrorMessage:      textOrEmpty(r.ErrorMessage),
		InternalLog:       r.InternalLog,
	}
	if err := json.Unmarshal(r.VideoInfo, &j.VideoInfo); err != nil {
		return nil, fmt.Errorf("video_info: %w", err)
	}
	if len(r.Gate) > 0 && string(r.Gate) != "null" {
		j.Gate = &Gate{}
		if err := json.Unmarshal(r.Gate, j.Gate); err != nil {
			return nil, fmt.Errorf("gate: %w", err)
		}
	}
	var err error
	if j.MissingShots, err = decodeStrings(r.MissingShots); err != nil {
		return nil, fmt.Errorf("missing_shots: %w", err)
	}
	if j.CostUSD, err = floatFromNumeric(r.CostUsd); err != nil {
		return nil, fmt.Errorf("cost_usd: %w", err)
	}
	return j, nil
}

// StartProcessing moves queued → processing. Already processing is a no-op (River
// re-delivery). Terminal → ErrConflict. Missing → ErrNotFound.
func (s *Store) StartProcessing(ctx context.Context, id string) error {
	n, err := s.q.StartProcessing(ctx, id)
	if err == nil && n == 0 {
		err = jobStateErr(ctx, s.q, id, StatusProcessing)
	}
	if err != nil {
		return fmt.Errorf("store: start processing %s: %w", id, err)
	}
	return nil
}

// SetAnalysis stores the gate verdict, the video summary and the model name used.
// Only allowed while processing (else ErrConflict). An empty modelName keeps the model
// name recorded at creation.
func (s *Store) SetAnalysis(ctx context.Context, id string, gate Gate, summary, modelName string) error {
	g, err := gateJSON(gate)
	if err == nil {
		var n int64
		n, err = s.q.SetAnalysis(ctx, sqlcdb.SetAnalysisParams{
			ID:           id,
			Gate:         g,
			VideoSummary: pgText(summary),
			ModelName:    modelName,
		})
		if err == nil && n == 0 {
			err = jobStateErr(ctx, s.q, id)
		}
	}
	if err != nil {
		return fmt.Errorf("store: set analysis %s: %w", id, err)
	}
	return nil
}

// SaveAnalysis records the analyze outcome atomically: gate, summary and model name on
// the job, the scenes (replaced), the analyze step finished as done with StepMeta, and —
// when a.Reject — the processing → rejected transition (video_path NULL, remaining
// steps skipped). The job must be processing, else ErrConflict and nothing is written.
//
// Replacing the scenes also deletes the job's candidate frames (see ReplaceScenes).
// Missing job → ErrNotFound. StepMeta must marshal to a JSON object (nil → {}).
func (s *Store) SaveAnalysis(ctx context.Context, jobID string, a AnalysisResult) error {
	if err := s.saveAnalysis(ctx, jobID, a); err != nil {
		return fmt.Errorf("store: save analysis %s: %w", jobID, err)
	}
	return nil
}

func (s *Store) saveAnalysis(ctx context.Context, id string, a AnalysisResult) error {
	g, err := gateJSON(a.Gate)
	if err != nil {
		return err
	}
	rows, err := sceneRows(id, a.Scenes)
	if err != nil {
		return err
	}
	meta, err := jsonbObject(a.StepMeta)
	if err != nil {
		return fmt.Errorf("step meta: %w", err)
	}
	summary := pgText(a.Summary)
	return s.inTx(ctx, func(_ pgx.Tx, q *sqlcdb.Queries) error {
		// The status update comes first: it locks the job row and fails fast.
		n, err := q.SetAnalysis(ctx, sqlcdb.SetAnalysisParams{
			ID: id, Gate: g, VideoSummary: summary, ModelName: a.ModelName,
		})
		if err != nil {
			return fmt.Errorf("set analysis: %w", err)
		}
		if n == 0 {
			return jobStateErr(ctx, q, id)
		}
		if err := replaceSceneRows(ctx, q, id, rows); err != nil {
			return err
		}
		if err := q.FinishStep(ctx, sqlcdb.FinishStepParams{
			JobID: id, Name: StepNameAnalyze, Status: sqlcdb.StepStatus(StepDone), Meta: meta,
		}); err != nil {
			return fmt.Errorf("finish analyze step: %w", err)
		}
		if !a.Reject {
			return nil
		}
		n, err = q.RejectJob(ctx, sqlcdb.RejectJobParams{ID: id, Gate: g, VideoSummary: summary})
		if err != nil {
			return fmt.Errorf("reject: %w", err)
		}
		if n == 0 { // unreachable while the row is locked, but never commit a half outcome
			return jobStateErr(ctx, q, id)
		}
		if _, err := q.SkipRemainingSteps(ctx, id); err != nil {
			return fmt.Errorf("skip remaining steps: %w", err)
		}
		return nil
	})
}

// CompleteJob atomically replaces selected_images with c.Images, marks their
// candidates selected, and moves processing → completed with explanation, missing
// shots and zip path. Not processing → ErrConflict (nothing written).
//
// Every other candidate of the job is marked not selected. Images are stored under id
// (an image whose JobID names another job is rejected); Idx must be ≥ 1.
func (s *Store) CompleteJob(ctx context.Context, id string, c Completion) error {
	if err := s.completeJob(ctx, id, c); err != nil {
		return fmt.Errorf("store: complete job %s: %w", id, err)
	}
	return nil
}

func (s *Store) completeJob(ctx context.Context, id string, c Completion) error {
	missing, err := encodeStrings(c.MissingShots)
	if err != nil {
		return fmt.Errorf("missing shots: %w", err)
	}
	images := make([]sqlcdb.InsertSelectedImagesParams, 0, len(c.Images))
	selected := make([]string, 0, len(c.Images))
	for _, im := range c.Images {
		p, err := imageParams(id, im)
		if err != nil {
			return err
		}
		images = append(images, p)
		if im.CandidateID != "" {
			selected = append(selected, im.CandidateID)
		}
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *sqlcdb.Queries) error {
		// The status update comes first: it locks the job row and fails fast.
		n, err := q.CompleteJob(ctx, sqlcdb.CompleteJobParams{
			ID:                id,
			ResultExplanation: pgText(c.Explanation),
			MissingShots:      missing,
			ZipPath:           nullText(c.ZipPath),
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return jobStateErr(ctx, q, id)
		}
		if _, err := q.DeleteSelectedImages(ctx, id); err != nil {
			return fmt.Errorf("delete images: %w", err)
		}
		if _, err := q.SetSelectedCandidates(ctx, sqlcdb.SetSelectedCandidatesParams{JobID: id, Ids: selected}); err != nil {
			return fmt.Errorf("mark candidates: %w", err)
		}
		if len(images) > 0 {
			if _, err := q.InsertSelectedImages(ctx, images); err != nil {
				return fmt.Errorf("insert images: %w", err)
			}
		}
		return nil
	})
}

// RejectJob moves processing → rejected (gate failed), stores the gate and summary and
// sets video_path NULL (the caller deletes the files). Not processing → ErrConflict.
func (s *Store) RejectJob(ctx context.Context, id string, gate Gate, summary string) error {
	g, err := gateJSON(gate)
	if err == nil {
		var n int64
		n, err = s.q.RejectJob(ctx, sqlcdb.RejectJobParams{ID: id, Gate: g, VideoSummary: pgText(summary)})
		if err == nil && n == 0 {
			err = jobStateErr(ctx, s.q, id)
		}
	}
	if err != nil {
		return fmt.Errorf("store: reject job %s: %w", id, err)
	}
	return nil
}

// FailJob moves queued|processing → failed with a user-safe code/message, appends
// internalLog to internal_log, and sets video_path NULL. Terminal → ErrConflict.
// internalLog is appended like AppendInternalLog ("" appends nothing).
func (s *Store) FailJob(ctx context.Context, id, code, message, internalLog string) error {
	n, err := s.q.FailJob(ctx, sqlcdb.FailJobParams{
		ID:           id,
		ErrorCode:    nullText(code),
		ErrorMessage: nullText(pgText(message)),
		Log:          logLine(internalLog),
		MaxLogBytes:  MaxInternalLogBytes,
	})
	if err == nil && n == 0 {
		err = jobStateErr(ctx, s.q, id)
	}
	if err != nil {
		return fmt.Errorf("store: fail job %s: %w", id, err)
	}
	return nil
}

// ExpireJob moves any non-expired status → expired and NULLs video_path, zip_path and
// every selected_images image_path/thumb_path of the job. Already expired is a no-op.
// Missing → ErrNotFound. Metadata and scoring rows are kept (decision 11).
func (s *Store) ExpireJob(ctx context.Context, id string) error {
	err := s.inTx(ctx, func(_ pgx.Tx, q *sqlcdb.Queries) error {
		n, err := q.ExpireJob(ctx, id)
		if err != nil {
			return err
		}
		if n == 0 {
			if err := jobStateErr(ctx, q, id, StatusExpired); err != nil {
				return err
			}
		}
		// Also runs when already expired: it then matches no row, or repairs a job whose
		// image paths were somehow left behind.
		if _, err := q.ExpireSelectedImages(ctx, id); err != nil {
			return fmt.Errorf("images: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: expire job %s: %w", id, err)
	}
	return nil
}

// AppendInternalLog appends text (plus a newline) to internal_log, keeping at most the
// last 64 KiB. No newline is added when text already ends with one; invalid UTF-8 and
// NUL bytes are replaced with U+FFFD. Missing → ErrNotFound.
func (s *Store) AppendInternalLog(ctx context.Context, id, text string) error {
	n, err := s.q.AppendInternalLog(ctx, sqlcdb.AppendInternalLogParams{
		ID:          id,
		Log:         logLine(text),
		MaxLogBytes: MaxInternalLogBytes,
	})
	if err != nil {
		return fmt.Errorf("store: append internal log %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("store: append internal log %s: %w", id, ErrNotFound)
	}
	return nil
}

// logLine prepares text for appending to internal_log: sanitised, newline-terminated
// and already cut to the cap ("" stays "").
func logLine(text string) string {
	if text == "" {
		return ""
	}
	text = pgText(text)
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return tailUTF8(text, MaxInternalLogBytes)
}

// ListExpiredJobs returns up to limit jobs with expires_at < now and status <> expired,
// oldest expiry first. limit <= 0 returns an empty list.
func (s *Store) ListExpiredJobs(ctx context.Context, now time.Time, limit int) ([]JobRef, error) {
	if limit <= 0 {
		return []JobRef{}, nil
	}
	rows, err := s.q.ListExpiredJobs(ctx, sqlcdb.ListExpiredJobsParams{Now: now, MaxRows: int32(min(limit, math.MaxInt32))})
	if err != nil {
		return nil, fmt.Errorf("store: list expired jobs: %w", err)
	}
	out := make([]JobRef, 0, len(rows))
	for _, r := range rows {
		out = append(out, JobRef{ID: r.ID, DomainKey: r.DomainKey, Status: JobStatus(r.Status)})
	}
	return out, nil
}

func gateJSON(g Gate) ([]byte, error) {
	g.Reason = pgText(g.Reason)
	g.DetectedSubject = pgText(g.DetectedSubject)
	b, err := jsonb(g)
	if err != nil {
		return nil, fmt.Errorf("gate: %w", err)
	}
	return b, nil
}
