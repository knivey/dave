-- +goose Up
-- Persist the resolved safety verdict (safe|unsafe|unknown) so restart
-- recovery never re-vets: recovery reads this column and reuses the value.
-- The empty default marks a job that never underwent classification
-- (skip_networks, or a crash before the verdict resolved) — deliberately
-- distinct from 'unknown' (vetted but unresolved), which recovery must
-- NOT re-vet either. Written once via dbUpdateJobSafety when the verdict
-- resolves; like the provenance columns, no terminal UPDATE statement
-- touches it.
ALTER TABLE jobs ADD COLUMN safety TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE jobs DROP COLUMN safety;
