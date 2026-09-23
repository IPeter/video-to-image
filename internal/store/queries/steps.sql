-- Queries on job_steps. Rows are created with the job (one pending row per step name);
-- StartStep/FinishStep upsert so a step name added after a job was created still works.

-- name: InsertSteps :exec
INSERT INTO job_steps (job_id, name)
SELECT @job_id, unnest(@names::text[]);

-- name: ListSteps :many
-- Ordered by the position of name in @names (the pipeline order); unknown names last.
SELECT *
FROM job_steps
WHERE job_id = @job_id
ORDER BY array_position(@names::text[], name) NULLS LAST, id;

-- name: StartStep :exec
INSERT INTO job_steps (job_id, name, status, started_at, finished_at, duration_ms)
VALUES (@job_id, @name, 'running', now(), NULL, NULL)
ON CONFLICT (job_id, name) DO UPDATE
SET status      = 'running',
    started_at  = now(),
    finished_at = NULL,
    duration_ms = NULL;

-- name: FinishStep :exec
-- duration_ms is measured from started_at; NULL when the step never started.
INSERT INTO job_steps (job_id, name, status, finished_at, meta)
VALUES (@job_id, @name, @status, now(), @meta)
ON CONFLICT (job_id, name) DO UPDATE
SET status      = excluded.status,
    finished_at = now(),
    duration_ms = CASE
        WHEN job_steps.started_at IS NULL THEN NULL
        ELSE greatest(0, round(extract(epoch FROM now() - job_steps.started_at) * 1000))::integer
    END,
    meta        = excluded.meta;

-- name: SkipRemainingSteps :execrows
UPDATE job_steps
SET status      = 'skipped',
    finished_at = now(),
    duration_ms = CASE
        WHEN started_at IS NULL THEN NULL
        ELSE greatest(0, round(extract(epoch FROM now() - started_at) * 1000))::integer
    END
WHERE job_id = @job_id AND status IN ('pending', 'running');

-- name: MergeStepMeta :execrows
-- Drops the keys in @remove from meta, then adds or replaces the keys of @set; status
-- and times are unchanged.
UPDATE job_steps
SET meta = (meta - @remove::text[]) || @set::jsonb
WHERE job_id = @job_id AND name = @name;
