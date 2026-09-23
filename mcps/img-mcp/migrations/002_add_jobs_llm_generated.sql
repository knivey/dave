-- +goose Up
-- Persist whether the job's prompt came from an LLM tool call, so
-- restart-recovered jobs still carry it into the workflow's prompt note node.
ALTER TABLE jobs ADD COLUMN llm_generated BOOLEAN NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE jobs DROP COLUMN llm_generated;
