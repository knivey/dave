-- +goose Up
-- Dislikes (owner request, Oct 2026): the like toggle becomes a
-- two-way vote. The table keeps its name and its (image_id, token)
-- PK — one row per anonymous browser per image remains the entire
-- dedupe story — and gains a vote column that says WHICH stance the
-- token holds: 1 = like, -1 = dislike. Mutual exclusivity is a
-- free consequence of the PK: a token cannot hold both stances
-- because it cannot hold two rows.
--
-- Existing rows are likes (vote = 1): dislikes did not exist when
-- they landed, so the NOT NULL DEFAULT 1 backfill is not a guess but
-- a restatement of what every pre-migration row already meant. The
-- CHECK is a tripwire for direct SQL fiddling only — Go callers pass
-- the package's voteLike/voteDislike constants.
ALTER TABLE likes ADD COLUMN vote INTEGER NOT NULL DEFAULT 1 CHECK (vote IN (-1, 1));

-- Count lookups now carry a vote predicate (like count, dislike
-- count, and the net-score ORDER BY of the liked sort), so the index
-- grows a vote column. The old image_id-only index is dropped: the
-- composite's leading column covers every query the prefix did, and
-- keeping both would only double the per-toggle index maintenance.
DROP INDEX idx_likes_image;
CREATE INDEX idx_likes_image_vote ON likes(image_id, vote);

-- +goose Down
DROP INDEX IF EXISTS idx_likes_image_vote;
CREATE INDEX idx_likes_image ON likes(image_id);
ALTER TABLE likes DROP COLUMN vote;
