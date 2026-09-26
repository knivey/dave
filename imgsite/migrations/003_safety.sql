-- +goose Up
-- Safety classification for the safe-site host split (design:
-- docs/superpowers/specs/2026-09-26-safe-site-design.md).
--
-- Values are exactly 'unknown' | 'safe' | 'unsafe'; NULL never occurs
-- (NOT NULL + DEFAULT). 'unknown' is default-deny: the safe site shows
-- only allowed-network origins ∪ safety='safe', so unclassified rows
-- stay invisible there until a verdict lands via upload meta, an
-- EXIF-note re-extract backfill, or the admin -safety CLI.
--
-- No index at current gallery scale (hundreds → low thousands of
-- rows; the safe-site predicate rides the existing scans); revisit
-- past ~50k rows.
ALTER TABLE images ADD COLUMN safety TEXT NOT NULL DEFAULT 'unknown';

-- +goose Down
ALTER TABLE images DROP COLUMN safety;
