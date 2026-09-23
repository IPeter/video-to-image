-- Queries on selected_images.

-- name: DeleteSelectedImages :execrows
DELETE FROM selected_images WHERE job_id = @job_id;

-- name: InsertSelectedImages :copyfrom
-- crop is NULL (nil) for an uncropped image.
INSERT INTO selected_images (
    id, job_id, candidate_id, idx, shot_id, timestamp_sec, width, height, reason, file_name,
    image_path, thumb_path, bytes, rotation, crop
) VALUES (
    @id, @job_id, @candidate_id, @idx, @shot_id, @timestamp_sec, @width, @height, @reason, @file_name,
    @image_path, @thumb_path, @bytes, @rotation, @crop
);

-- name: ListSelectedImages :many
SELECT * FROM selected_images WHERE job_id = @job_id ORDER BY idx;

-- name: GetSelectedImage :one
SELECT * FROM selected_images WHERE job_id = @job_id AND id = @id;

-- name: ExpireSelectedImages :execrows
UPDATE selected_images
SET image_path = NULL, thumb_path = NULL
WHERE job_id = @job_id AND (image_path IS NOT NULL OR thumb_path IS NOT NULL);
