-- Queries on candidate_frames. Rows are kept indefinitely (decision 11).

-- name: DeleteCandidates :execrows
DELETE FROM candidate_frames WHERE job_id = @job_id;

-- name: InsertCandidates :copyfrom
-- duplicate_of may point at a later row of the same batch: the FK is DEFERRABLE
-- INITIALLY DEFERRED, so it is checked at commit.
INSERT INTO candidate_frames (
    id, job_id, scene_id, shot_id, timestamp_sec, sharpness, sharpness_z, exposure_penalty,
    dhash, duplicate_of, rejected_reason, score, selected
) VALUES (
    @id, @job_id, @scene_id, @shot_id, @timestamp_sec, @sharpness, @sharpness_z, @exposure_penalty,
    @dhash, @duplicate_of, @rejected_reason, @score, @selected
);

-- name: ListCandidates :many
-- scene_idx is the idx of the linked scene (NULL when scene_id is NULL).
SELECT sqlc.embed(candidate_frames), scenes.idx AS scene_idx
FROM candidate_frames
LEFT JOIN scenes ON scenes.id = candidate_frames.scene_id
WHERE candidate_frames.job_id = @job_id
ORDER BY candidate_frames.timestamp_sec, candidate_frames.id;

-- name: SetSelectedCandidates :execrows
-- selected := (id is in @ids) for every candidate of the job; rows already right are not touched.
UPDATE candidate_frames
SET selected = (id = ANY(@ids::text[]))
WHERE job_id = @job_id AND selected IS DISTINCT FROM (id = ANY(@ids::text[]));
