-- +goose Up
-- Persist the job's IRC provenance (dave injects it via
-- _dave_inject_network/_dave_inject_channel/_dave_inject_nick) so
-- restart-recovered jobs — which rebuild the Job from this table — still
-- carry it into the imgsite upload meta. Insert/recovery-only columns: no
-- terminal UPDATE statement touches them.
ALTER TABLE jobs ADD COLUMN network TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN channel TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN nick TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE jobs DROP COLUMN nick;
ALTER TABLE jobs DROP COLUMN channel;
ALTER TABLE jobs DROP COLUMN network;
