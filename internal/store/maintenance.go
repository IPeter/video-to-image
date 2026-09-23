package store

import (
	"context"
	"fmt"
	"math"
	"time"
)

// RowEstimateTables are the tables TableRowEstimates reports, as map keys.
var RowEstimateTables = []string{"jobs", "job_steps", "scenes", "candidate_frames", "selected_images", "usage_events"}

// JobStatuses lists every JobStatus, in state-machine order.
var JobStatuses = []JobStatus{StatusQueued, StatusProcessing, StatusCompleted, StatusRejected, StatusFailed, StatusExpired}

// TableRowEstimates returns approximate row counts (pg_class.reltuples) for jobs,
// job_steps, scenes, candidate_frames, selected_images and usage_events.
// Every table is a key; a table never analyzed (reltuples -1) reports 0.
func (s *Store) TableRowEstimates(ctx context.Context) (map[string]int64, error) {
	rows, err := s.q.TableRowEstimates(ctx, RowEstimateTables)
	if err != nil {
		return nil, fmt.Errorf("store: table row estimates: %w", err)
	}
	out := make(map[string]int64, len(RowEstimateTables))
	for _, t := range RowEstimateTables {
		out[t] = 0
	}
	for _, r := range rows {
		if r.Estimate > 0 {
			out[r.TableName] = int64(math.Round(r.Estimate))
		}
	}
	return out, nil
}

// CountJobsByStatus returns the number of jobs per status.
// Every JobStatus is a key (0 when no job has it), so gauges reset to zero.
func (s *Store) CountJobsByStatus(ctx context.Context) (map[JobStatus]int64, error) {
	rows, err := s.q.CountJobsByStatus(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: count jobs by status: %w", err)
	}
	out := make(map[JobStatus]int64, len(JobStatuses))
	for _, st := range JobStatuses {
		out[st] = 0
	}
	for _, r := range rows {
		out[JobStatus(r.Status)] = r.Jobs
	}
	return out, nil
}

// listOrphanedJobsSQL is hand-written rather than sqlc-generated: river_job is created by
// rivermigrate (store.Migrate), not by migrations/, so sqlc cannot type-check a query
// against it. A schema stub for sqlc would drift silently from River's real schema;
// instead TestListOrphanedJobs runs this query against the River-migrated database.
// It reads only River's long-stable kind, args and state columns. The containment test
// (args @> ...) can use River's GIN index on args.
const listOrphanedJobsSQL = `
SELECT j.id, j.domain_key, j.status::text
FROM jobs AS j
WHERE j.status IN ('queued', 'processing')
  AND j.updated_at < $1
  AND NOT EXISTS (
      SELECT 1
      FROM river_job AS r
      WHERE r.kind = 'pipeline'
        AND r.args @> jsonb_build_object('jobId', j.id)
        AND r.state IN ('available', 'pending', 'running', 'retryable', 'scheduled'))
ORDER BY j.updated_at, j.id
LIMIT $2`

// ListOrphanedJobs returns up to limit jobs that are queued or processing, were last
// updated before idleSince, and have NO River pipeline job (kind 'pipeline',
// args->>'jobId' = jobs.id) in an active state (available, pending, running, retryable,
// scheduled) — i.e. River discarded, cancelled or deleted their work. Oldest first.
// The maintenance reconciler fails them so users never wait on a spinner forever.
// limit <= 0 returns an empty list.
func (s *Store) ListOrphanedJobs(ctx context.Context, idleSince time.Time, limit int) ([]JobRef, error) {
	if limit <= 0 {
		return []JobRef{}, nil
	}
	rows, err := s.pool.Query(ctx, listOrphanedJobsSQL, idleSince, min(limit, math.MaxInt32))
	if err != nil {
		return nil, fmt.Errorf("store: list orphaned jobs: %w", err)
	}
	defer rows.Close()
	out := []JobRef{}
	for rows.Next() {
		var r JobRef
		var status string
		if err := rows.Scan(&r.ID, &r.DomainKey, &status); err != nil {
			return nil, fmt.Errorf("store: list orphaned jobs: scan: %w", err)
		}
		r.Status = JobStatus(status)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list orphaned jobs: %w", err)
	}
	return out, nil
}
