-- +goose Up
-- Studio backgrounds (internal/studio): a separate job that runs after the pipeline.
ALTER TABLE jobs
    ADD COLUMN studio_status   text NOT NULL DEFAULT 'none'
        CHECK (studio_status IN ('none', 'pending', 'running', 'done', 'failed', 'skipped')),
    ADD COLUMN studio_zip_path text;                  -- storage key of studio.zip; NULL until built

-- The orphan reconciler looks for studio work stuck in pending/running.
CREATE INDEX jobs_studio_active_idx ON jobs (updated_at) WHERE studio_status IN ('pending', 'running');

ALTER TABLE selected_images
    ADD COLUMN subject_box jsonb,                     -- {x0,y0,x1,y1} in the EXPORTED image; NULL = no review box
    ADD COLUMN studio      jsonb;                     -- store.ImageStudio; NULL = not processed

-- +goose Down
ALTER TABLE selected_images
    DROP COLUMN studio,
    DROP COLUMN subject_box;

DROP INDEX jobs_studio_active_idx;

ALTER TABLE jobs
    DROP COLUMN studio_zip_path,
    DROP COLUMN studio_status;
