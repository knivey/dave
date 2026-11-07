-- +goose Up
-- Edited images never cross to the safe site via the safety verdict: the
-- vet is text-only and cannot see an edit's input image, so an NSFW input
-- with an innocent edit prompt would pass as 'safe'. The network arm
-- (origin in allowed_networks) still applies to edits.
ALTER TABLE images ADD COLUMN edited BOOLEAN NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE images DROP COLUMN edited;
