-- Queries on usage_events, the source of truth for cost reporting.

-- name: InsertUsage :one
-- created_at defaults to now() when @created_at is NULL. Returns the stored (rounded)
-- cost so jobs.cost_usd stays exactly the sum of its events.
INSERT INTO usage_events (
    job_id, domain_key, step, model, input_tokens, output_tokens, thinking_tokens,
    video_seconds, cost_usd, latency_ms, created_at
) VALUES (
    sqlc.narg(job_id), @domain_key, @step, @model, @input_tokens, @output_tokens, @thinking_tokens,
    @video_seconds, @cost_usd, @latency_ms, COALESCE(sqlc.narg(created_at)::timestamptz, now())
)
RETURNING cost_usd;

-- name: CostSince :one
SELECT COALESCE(sum(cost_usd), 0)::float8 AS total
FROM usage_events
WHERE created_at >= @since;

-- name: UsageReport :many
-- One row per UTC day and domain over [@from_time, @to_time). @domain_key '' means all.
SELECT
    (date_trunc('day', created_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC')::timestamptz AS day,
    domain_key,
    count(DISTINCT job_id)::bigint                      AS jobs,
    count(*)::bigint                                    AS calls,
    COALESCE(sum(input_tokens), 0)::bigint              AS input_tokens,
    COALESCE(sum(output_tokens), 0)::bigint             AS output_tokens,
    COALESCE(sum(thinking_tokens), 0)::bigint           AS thinking_tokens,
    COALESCE(sum(video_seconds::float8), 0)::float8     AS video_seconds,
    COALESCE(sum(cost_usd), 0)::float8                  AS cost_usd
FROM usage_events
WHERE created_at >= @from_time
  AND created_at < @to_time
  AND (@domain_key::text = '' OR domain_key = @domain_key::text)
GROUP BY 1, 2
ORDER BY 1, 2;
