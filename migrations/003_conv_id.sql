-- +goose Up
ALTER TABLE sessions ADD COLUMN conv_id TEXT;
CREATE INDEX IF NOT EXISTS idx_sessions_conv_id ON sessions(conv_id);

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_conv_id;
ALTER TABLE sessions DROP COLUMN conv_id;
