-- +goose Up
ALTER TABLE sessions ADD COLUMN response_id TEXT;
CREATE INDEX IF NOT EXISTS idx_sessions_response_id ON sessions(response_id);

-- +goose Down
DROP INDEX IF EXISTS idx_sessions_response_id;
ALTER TABLE sessions DROP COLUMN response_id;
