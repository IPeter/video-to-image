-- Queries on scenes (model output after post-processing, kept for evaluation).

-- name: DeleteScenes :execrows
DELETE FROM scenes WHERE job_id = @job_id;

-- name: InsertScenes :copyfrom
INSERT INTO scenes (job_id, idx, start_sec, end_sec, shot_id, relevance, reason, raw_start, raw_end)
VALUES (@job_id, @idx, @start_sec, @end_sec, @shot_id, @relevance, @reason, @raw_start, @raw_end);

-- name: ListScenes :many
SELECT * FROM scenes WHERE job_id = @job_id ORDER BY idx;

-- name: SceneIDsByIdx :many
SELECT idx, id FROM scenes WHERE job_id = @job_id;
