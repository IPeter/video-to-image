package store

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"video-to-image/internal/store/sqlcdb"
)

// Studio backgrounds (internal/pipeline/studiojob) run as a separate job after the
// pipeline completed: jobs.studio_status goes none | pending → running → done | failed |
// skipped, and each eligible image gets its outcome in selected_images.studio. The job's
// own status and its original images are never changed by this work.

// StartStudio moves the job's studio work pending → running; already running is a no-op
// (River re-delivery). The job must be completed with studio work pending or running,
// else ErrConflict (the user deleted it, or the studio work already ended). Missing →
// ErrNotFound.
func (s *Store) StartStudio(ctx context.Context, jobID string) error {
	n, err := s.q.StartStudio(ctx, jobID)
	if err == nil && n == 0 {
		err = studioStateErr(ctx, s.q, jobID)
	}
	if err != nil {
		return fmt.Errorf("store: start studio %s: %w", jobID, err)
	}
	return nil
}

// SetImageStudio stores one image's studio outcome (replacing an earlier one) while the
// job is completed and its studio work running, else ErrConflict. A missing job or image
// → ErrNotFound. st.Status must be one of the ImageStudio* values.
func (s *Store) SetImageStudio(ctx context.Context, jobID, imageID string, st ImageStudio) error {
	b, err := encodeImageStudio(&st)
	if err == nil {
		var n int64
		n, err = s.q.SetImageStudio(ctx, sqlcdb.SetImageStudioParams{Studio: b, JobID: jobID, ID: imageID})
		if err == nil && n == 0 {
			if err = studioStateErr(ctx, s.q, jobID, StudioRunning); err == nil {
				err = fmt.Errorf("image %s: %w", imageID, ErrNotFound)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("store: set image studio %s/%s: %w", jobID, imageID, err)
	}
	return nil
}

// FinishStudio ends the job's running studio work with f.Status (done, failed or
// skipped) and f.ZipPath, and stores f.Unprocessed (when set) for every image still
// without an outcome, in one transaction. The job must be completed with studio work
// running, else ErrConflict (nothing written). Missing → ErrNotFound.
func (s *Store) FinishStudio(ctx context.Context, jobID string, f StudioFinish) error {
	if err := s.finishStudio(ctx, jobID, f); err != nil {
		return fmt.Errorf("store: finish studio %s: %w", jobID, err)
	}
	return nil
}

func (s *Store) finishStudio(ctx context.Context, jobID string, f StudioFinish) error {
	switch f.Status {
	case StudioDone, StudioFailed, StudioSkipped:
	default:
		return fmt.Errorf("status %q is not done, failed or skipped", f.Status)
	}
	fill, err := encodeImageStudio(f.Unprocessed)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *sqlcdb.Queries) error {
		n, err := q.FinishStudio(ctx, sqlcdb.FinishStudioParams{
			StudioStatus: string(f.Status), StudioZipPath: nullText(f.ZipPath), ID: jobID,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return studioStateErr(ctx, q, jobID)
		}
		if fill == nil {
			return nil
		}
		if _, err := q.FillImageStudios(ctx, sqlcdb.FillImageStudiosParams{Studio: fill, JobID: jobID}); err != nil {
			return fmt.Errorf("fill images: %w", err)
		}
		return nil
	})
}

// FailStudio marks the job's pending or running studio work failed, whatever the job's
// own status (River gave up on the studio job, or the reconciler found it orphaned), and
// gives every image still without an outcome ImageStudio{failed, reason}, in one
// transaction. No studio work pending or running → ErrConflict. Missing → ErrNotFound.
func (s *Store) FailStudio(ctx context.Context, jobID, reason string) error {
	fill, err := encodeImageStudio(&ImageStudio{Status: ImageStudioFailed, Reason: reason})
	if err == nil {
		err = s.inTx(ctx, func(_ pgx.Tx, q *sqlcdb.Queries) error {
			n, err := q.FailStudio(ctx, jobID)
			if err != nil {
				return err
			}
			if n == 0 {
				return studioStateErr(ctx, q, jobID)
			}
			if _, err := q.FillImageStudios(ctx, sqlcdb.FillImageStudiosParams{Studio: fill, JobID: jobID}); err != nil {
				return fmt.Errorf("fill images: %w", err)
			}
			return nil
		})
	}
	if err != nil {
		return fmt.Errorf("store: fail studio %s: %w", jobID, err)
	}
	return nil
}

// studioStateErr is called after a conditional studio UPDATE matched no row: ErrNotFound
// when the job does not exist, nil when the job is completed and its studio status is
// one of okIf (the caller then knows something else matched nothing), ErrConflict
// otherwise.
func studioStateErr(ctx context.Context, q *sqlcdb.Queries, id string, okIf ...StudioStatus) error {
	st, err := q.GetStudioState(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("read status: %w", err)
	}
	if JobStatus(st.Status) == StatusCompleted {
		for _, ok := range okIf {
			if StudioStatus(st.StudioStatus) == ok {
				return nil
			}
		}
	}
	return fmt.Errorf("status %s, studio %s: %w", st.Status, st.StudioStatus, ErrConflict)
}

// listOrphanedStudioJobsSQL is hand-written for the same reason as listOrphanedJobsSQL
// (river_job is not part of the sqlc schema); TestListOrphanedStudioJobs runs it against
// the River-migrated database.
const listOrphanedStudioJobsSQL = `
SELECT j.id, j.domain_key, j.status::text, j.studio_status
FROM jobs AS j
WHERE j.studio_status IN ('pending', 'running')
  AND j.updated_at < $1
  AND NOT EXISTS (
      SELECT 1
      FROM river_job AS r
      WHERE r.kind = 'studio'
        AND r.args @> jsonb_build_object('jobId', j.id)
        AND r.state IN ('available', 'pending', 'running', 'retryable', 'scheduled'))
ORDER BY j.updated_at, j.id
LIMIT $2`

// ListOrphanedStudioJobs returns up to limit jobs whose studio work is pending or
// running, that were last updated before idleSince, and that have NO River studio job
// (kind 'studio', args->>'jobId' = jobs.id) in an active state: River discarded,
// cancelled or deleted it without the studio worker recording the failure. Oldest first.
// The reconciler fails them (FailStudio). limit <= 0 returns an empty list.
func (s *Store) ListOrphanedStudioJobs(ctx context.Context, idleSince time.Time, limit int) ([]JobRef, error) {
	if limit <= 0 {
		return []JobRef{}, nil
	}
	rows, err := s.pool.Query(ctx, listOrphanedStudioJobsSQL, idleSince, min(limit, math.MaxInt32))
	if err != nil {
		return nil, fmt.Errorf("store: list orphaned studio jobs: %w", err)
	}
	defer rows.Close()
	out := []JobRef{}
	for rows.Next() {
		var r JobRef
		var status, studio string
		if err := rows.Scan(&r.ID, &r.DomainKey, &status, &studio); err != nil {
			return nil, fmt.Errorf("store: list orphaned studio jobs: scan: %w", err)
		}
		r.Status, r.StudioStatus = JobStatus(status), StudioStatus(studio)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list orphaned studio jobs: %w", err)
	}
	return out, nil
}
