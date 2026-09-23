package store

import (
	"context"
	"fmt"

	"video-to-image/internal/store/sqlcdb"
)

// ListSteps returns the job's steps in StepNames order. Steps whose name is not in
// StepNames come last. An unknown job has no steps.
func (s *Store) ListSteps(ctx context.Context, jobID string) ([]Step, error) {
	rows, err := s.q.ListSteps(ctx, sqlcdb.ListStepsParams{JobID: jobID, Names: StepNames})
	if err != nil {
		return nil, fmt.Errorf("store: list steps %s: %w", jobID, err)
	}
	out := make([]Step, 0, len(rows))
	for _, r := range rows {
		meta, err := decodeMeta(r.Meta)
		if err != nil {
			return nil, fmt.Errorf("store: list steps %s: %s meta: %w", jobID, r.Name, err)
		}
		st := Step{
			JobID:      r.JobID,
			Name:       r.Name,
			Status:     StepStatus(r.Status),
			StartedAt:  utcPtr(r.StartedAt),
			FinishedAt: utcPtr(r.FinishedAt),
			Meta:       meta,
		}
		if r.DurationMs != nil {
			d := int(*r.DurationMs)
			st.DurationMs = &d
		}
		out = append(out, st)
	}
	return out, nil
}

// StartStep sets status running, started_at now(), finished_at NULL, duration_ms NULL.
// The row is created when missing (a step name added after the job was created).
// Missing job → ErrNotFound.
func (s *Store) StartStep(ctx context.Context, jobID, name string) error {
	if name == "" {
		return fmt.Errorf("store: start step %s: empty name", jobID)
	}
	if err := s.q.StartStep(ctx, sqlcdb.StartStepParams{JobID: jobID, Name: name}); err != nil {
		return wrapJobErr("start step "+name+" of", jobID, err)
	}
	return nil
}

// FinishStep sets status (done, failed or skipped), finished_at now(), duration_ms from
// started_at (NULL if never started) and meta (JSON-marshalled; nil → {}).
// meta must marshal to a JSON object. Missing job → ErrNotFound.
func (s *Store) FinishStep(ctx context.Context, jobID, name string, status StepStatus, meta any) error {
	switch {
	case name == "":
		return fmt.Errorf("store: finish step %s: empty name", jobID)
	case status != StepDone && status != StepFailed && status != StepSkipped:
		return fmt.Errorf("store: finish step %s of %s: invalid status %q", name, jobID, status)
	}
	m, err := jsonbObject(meta)
	if err != nil {
		return fmt.Errorf("store: finish step %s of %s: %w", name, jobID, err)
	}
	if err := s.q.FinishStep(ctx, sqlcdb.FinishStepParams{
		JobID:  jobID,
		Name:   name,
		Status: sqlcdb.StepStatus(status),
		Meta:   m,
	}); err != nil {
		return wrapJobErr("finish step "+name+" of", jobID, err)
	}
	return nil
}

// SkipRemainingSteps marks every pending or running step of the job as skipped.
// finished_at is set and duration_ms computed for steps that had started.
func (s *Store) SkipRemainingSteps(ctx context.Context, jobID string) error {
	if _, err := s.q.SkipRemainingSteps(ctx, jobID); err != nil {
		return fmt.Errorf("store: skip remaining steps %s: %w", jobID, err)
	}
	return nil
}
