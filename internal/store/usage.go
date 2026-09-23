package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"video-to-image/internal/store/sqlcdb"
)

// InsertUsage inserts the event and adds its cost to jobs.cost_usd in one transaction.
// An empty JobID stores NULL and updates no job.
// The job is credited with the stored (6-decimal) cost, so jobs.cost_usd always equals
// the sum of its events. A non-zero CreatedAt is stored as given (backfills, replays);
// zero means now(). Missing job → ErrNotFound.
func (s *Store) InsertUsage(ctx context.Context, e UsageEvent) error {
	if err := s.insertUsage(ctx, e); err != nil {
		job := e.JobID
		if job == "" {
			job = "(no job)"
		}
		return wrapJobErr("insert usage "+e.Step+" for", job, err)
	}
	return nil
}

func (s *Store) insertUsage(ctx context.Context, e UsageEvent) error {
	cost, err := numericFromFloat(e.CostUSD)
	if err != nil {
		return fmt.Errorf("cost: %w", err)
	}
	p := sqlcdb.InsertUsageParams{
		JobID:        nullText(e.JobID),
		DomainKey:    e.DomainKey,
		Step:         e.Step,
		Model:        e.Model,
		VideoSeconds: f32(e.VideoSeconds),
		CostUsd:      cost,
	}
	for _, f := range []struct {
		name string
		v    int
		dst  *int32
	}{
		{"input tokens", e.InputTokens, &p.InputTokens},
		{"output tokens", e.OutputTokens, &p.OutputTokens},
		{"thinking tokens", e.ThinkingTokens, &p.ThinkingTokens},
		{"latency ms", e.LatencyMs, &p.LatencyMs},
	} {
		if *f.dst, err = int32Of(f.name, int64(f.v)); err != nil {
			return err
		}
	}
	if !e.CreatedAt.IsZero() {
		p.CreatedAt = &e.CreatedAt
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *sqlcdb.Queries) error {
		stored, err := q.InsertUsage(ctx, p)
		if err != nil {
			return err
		}
		if e.JobID == "" {
			return nil
		}
		n, err := q.AddJobCost(ctx, sqlcdb.AddJobCostParams{ID: e.JobID, Delta: stored})
		if err != nil {
			return fmt.Errorf("add job cost: %w", err)
		}
		if n == 0 { // cannot happen while the FK holds; kept as a guard
			return ErrNotFound
		}
		return nil
	})
}

// CostSince sums usage_events.cost_usd with created_at >= since.
func (s *Store) CostSince(ctx context.Context, since time.Time) (float64, error) {
	total, err := s.q.CostSince(ctx, since)
	if err != nil {
		return 0, fmt.Errorf("store: cost since %s: %w", since.Format(time.RFC3339), err)
	}
	return total, nil
}

// UsageReport groups usage_events in [from, to) by UTC day and domain, ordered by day
// then domain. domainKey "" means all domains.
// Jobs counts distinct non-NULL job ids.
func (s *Store) UsageReport(ctx context.Context, from, to time.Time, domainKey string) ([]UsageRow, error) {
	rows, err := s.q.UsageReport(ctx, sqlcdb.UsageReportParams{FromTime: from, ToTime: to, DomainKey: domainKey})
	if err != nil {
		return nil, fmt.Errorf("store: usage report: %w", err)
	}
	out := make([]UsageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, UsageRow{
			Day:            r.Day.UTC(),
			DomainKey:      r.DomainKey,
			Jobs:           r.Jobs,
			Calls:          r.Calls,
			InputTokens:    r.InputTokens,
			OutputTokens:   r.OutputTokens,
			ThinkingTokens: r.ThinkingTokens,
			VideoSeconds:   r.VideoSeconds,
			CostUSD:        r.CostUsd,
		})
	}
	return out, nil
}
