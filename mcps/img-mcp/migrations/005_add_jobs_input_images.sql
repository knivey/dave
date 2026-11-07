-- +goose Up
-- Edit jobs persist their resolved ComfyUI input-image upload coordinates
-- (JSON of [{ref, comfy filename/subfolder/type}]) plus the edit strength so
-- restart recovery re-submits without re-uploading. Insert/recovery-only
-- columns: no terminal-status UPDATE touches them.
ALTER TABLE jobs ADD COLUMN input_images TEXT DEFAULT '';
ALTER TABLE jobs ADD COLUMN strength REAL NULL;

-- +goose Down
ALTER TABLE jobs DROP COLUMN strength;
ALTER TABLE jobs DROP COLUMN input_images;
