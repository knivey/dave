-- +goose Up
-- Direct asset URL for each job image (imgsite's /orig/ link). The url
-- column holds the PAGE link; view_image needs the direct bytes. Legacy
-- rows keep '' and rebuild without the field (omitempty).
ALTER TABLE job_images ADD COLUMN image_url TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE job_images DROP COLUMN image_url;
