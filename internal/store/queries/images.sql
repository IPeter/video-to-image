-- Queries on selected_images.

-- name: DeleteSelectedImages :execrows
DELETE FROM selected_images WHERE job_id = @job_id;

-- name: InsertSelectedImages :copyfrom
-- crop is NULL (nil) for an uncropped image, subject_box without a review box, studio
-- when the studio step has not decided about the image yet.
INSERT INTO selected_images (
    id, job_id, candidate_id, idx, shot_id, timestamp_sec, width, height, reason, file_name,
    image_path, thumb_path, bytes, rotation, crop, subject_box, studio
) VALUES (
    @id, @job_id, @candidate_id, @idx, @shot_id, @timestamp_sec, @width, @height, @reason, @file_name,
    @image_path, @thumb_path, @bytes, @rotation, @crop, @subject_box, @studio
);

-- name: ListSelectedImages :many
SELECT * FROM selected_images WHERE job_id = @job_id ORDER BY idx;

-- name: GetSelectedImage :one
SELECT * FROM selected_images WHERE job_id = @job_id AND id = @id;

-- name: ExpireSelectedImages :execrows
-- The studio outcome keeps its status, reason and diagnostics; its variants (storage
-- keys) go with the files.
UPDATE selected_images
SET image_path = NULL,
    thumb_path = NULL,
    studio     = CASE WHEN studio IS NULL THEN NULL ELSE studio - 'styles' END
WHERE job_id = @job_id
  AND (image_path IS NOT NULL OR thumb_path IS NOT NULL OR studio -> 'styles' IS NOT NULL);

-- name: SetImageStudio :execrows
-- Only while the job is completed and its studio work is running.
UPDATE selected_images AS si
SET studio = @studio
FROM jobs AS j
WHERE si.job_id = @job_id AND si.id = @id
  AND j.id = si.job_id AND j.status = 'completed' AND j.studio_status = 'running';

-- name: FillImageStudios :execrows
-- Gives every image the studio step has not decided about yet (studio NULL) @studio.
UPDATE selected_images
SET studio = @studio
WHERE job_id = @job_id AND studio IS NULL;
