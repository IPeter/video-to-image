-- +goose Up
-- Rotation and crop applied to an exported image (model review + crop step).
ALTER TABLE selected_images
    ADD COLUMN rotation integer NOT NULL DEFAULT 0,   -- degrees clockwise: 0, 90, 180, 270
    ADD COLUMN crop     jsonb;                        -- {x0,y0,x1,y1} normalised in the upright frame; NULL = uncropped

-- +goose Down
ALTER TABLE selected_images
    DROP COLUMN crop,
    DROP COLUMN rotation;
