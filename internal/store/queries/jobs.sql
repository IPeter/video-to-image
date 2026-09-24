-- Queries on jobs. Every status transition is conditional on the current status in
-- SQL; the Go wrapper turns "0 rows" into ErrNotFound or ErrConflict with GetJobStatus.
-- Every UPDATE bumps updated_at.

-- name: InsertJob :exec
INSERT INTO jobs (
    id, domain_key, status, expires_at, client_ip_hash, video_path, video_info, model_name
) VALUES (
    @id, @domain_key, 'queued', @expires_at, @client_ip_hash, sqlc.narg(video_path),
    @video_info, @model_name
);

-- name: GetJob :one
SELECT * FROM jobs WHERE id = @id;

-- name: GetJobStatus :one
SELECT status FROM jobs WHERE id = @id;

-- name: StartProcessing :execrows
UPDATE jobs
SET status = 'processing', updated_at = now()
WHERE id = @id AND status = 'queued';

-- name: SetAnalysis :execrows
-- An empty model name keeps the one recorded at creation.
UPDATE jobs
SET gate          = @gate,
    video_summary = @video_summary,
    model_name    = COALESCE(NULLIF(@model_name::text, ''), model_name),
    updated_at    = now()
WHERE id = @id AND status = 'processing';

-- name: CompleteJob :execrows
-- @studio_status is 'none' or 'pending' (the studio job is enqueued in the same transaction).
UPDATE jobs
SET status             = 'completed',
    result_explanation = @result_explanation,
    missing_shots      = @missing_shots,
    zip_path           = sqlc.narg(zip_path),
    studio_status      = @studio_status,
    updated_at         = now()
WHERE id = @id AND status = 'processing';

-- name: RejectJob :execrows
UPDATE jobs
SET status        = 'rejected',
    gate          = @gate,
    video_summary = @video_summary,
    video_path    = NULL,
    updated_at    = now()
WHERE id = @id AND status = 'processing';

-- name: FailJob :execrows
-- @log is appended (it already carries its trailing newline; '' appends nothing) and the
-- result keeps at most the last @max_log_bytes bytes of UTF-8: the byte tail is cut (substr with a
-- start below 1 keeps everything), then the leading continuation bytes
-- (0x80-0xBF) of a character split by the cut are dropped so the text stays valid.
UPDATE jobs
SET status        = 'failed',
    error_code    = sqlc.narg(error_code),
    error_message = sqlc.narg(error_message),
    internal_log  = convert_from(
        ltrim(
            substr(convert_to(internal_log || @log::text, 'UTF8'),
                   octet_length(convert_to(internal_log || @log::text, 'UTF8')) - sqlc.arg(max_log_bytes)::integer + 1),
            decode('808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf', 'hex')),
        'UTF8'),
    video_path    = NULL,
    updated_at    = now()
WHERE id = @id AND status IN ('queued', 'processing');

-- name: ExpireJob :execrows
-- Studio work still pending or running ends as skipped: nothing is made for a deleted job.
UPDATE jobs
SET status          = 'expired',
    video_path      = NULL,
    zip_path        = NULL,
    studio_zip_path = NULL,
    studio_status   = CASE WHEN studio_status IN ('pending', 'running') THEN 'skipped' ELSE studio_status END,
    updated_at      = now()
WHERE id = @id AND status <> 'expired';

-- name: AppendInternalLog :execrows
-- Same capping rule as FailJob.
UPDATE jobs
SET internal_log = convert_from(
        ltrim(
            substr(convert_to(internal_log || @log::text, 'UTF8'),
                   octet_length(convert_to(internal_log || @log::text, 'UTF8')) - sqlc.arg(max_log_bytes)::integer + 1),
            decode('808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebf', 'hex')),
        'UTF8'),
    updated_at = now()
WHERE id = @id;

-- name: AddJobCost :execrows
UPDATE jobs
SET cost_usd = cost_usd + @delta::numeric, updated_at = now()
WHERE id = @id;

-- name: ListExpiredJobs :many
SELECT id, domain_key, status, studio_status
FROM jobs
WHERE expires_at < @now AND status <> 'expired'
ORDER BY expires_at, id
LIMIT @max_rows;

-- name: StartStudio :execrows
-- Studio backgrounds (internal/pipeline/studiojob): pending → running; already running
-- is a no-op (River re-delivery). Start and finish need a completed job, so a job the
-- user deleted (expired) in the meantime stops the studio work.
UPDATE jobs
SET studio_status = 'running', updated_at = now()
WHERE id = @id AND status = 'completed' AND studio_status IN ('pending', 'running');

-- name: FinishStudio :execrows
-- running → done | failed | skipped, with the studio.zip key (NULL when none was built).
UPDATE jobs
SET studio_status   = @studio_status,
    studio_zip_path = sqlc.narg(studio_zip_path),
    updated_at      = now()
WHERE id = @id AND status = 'completed' AND studio_status = 'running';

-- name: FailStudio :execrows
-- pending | running → failed, whatever the job's status (River gave up on the studio job).
UPDATE jobs
SET studio_status = 'failed', updated_at = now()
WHERE id = @id AND studio_status IN ('pending', 'running');

-- name: GetStudioState :one
SELECT status, studio_status FROM jobs WHERE id = @id;
