-- +goose Up
-- Reaction keys become the emoji itself (owner request, Oct 2026:
-- any emoji reactable via the full-catalog picker — the reactions
-- table's emoji column now stores the raw emoji string, e.g. '🔥',
-- percent-encoded in toggle URLs). Every pre-migration row keyed on
-- a configured ASCII short name; this migration translates the
-- shipped default names to their emoji and drops rows holding names
-- that were never in a shipped default (custom local presets from
-- the feature's first days — unconvertible without the operator's
-- name→glyph mapping, days old at most, a casual anonymous feature).
UPDATE reactions SET emoji = '🔥' WHERE emoji = 'fire';
UPDATE reactions SET emoji = '😂' WHERE emoji = 'laugh';
UPDATE reactions SET emoji = '💀' WHERE emoji = 'skull';
UPDATE reactions SET emoji = '💩' WHERE emoji = 'poop';
UPDATE reactions SET emoji = '👀' WHERE emoji = 'eyes';
UPDATE reactions SET emoji = '🤡' WHERE emoji = 'clown';
UPDATE reactions SET emoji = '😮' WHERE emoji = 'wow';
UPDATE reactions SET emoji = '🤔' WHERE emoji = 'thinking';
UPDATE reactions SET emoji = '🥺' WHERE emoji = 'pleading';
UPDATE reactions SET emoji = '🙏' WHERE emoji = 'pray';
UPDATE reactions SET emoji = '✨' WHERE emoji = 'sparkles';
UPDATE reactions SET emoji = '🎨' WHERE emoji = 'art';
-- Old validation pinned keys to [a-z0-9_], so a lowercase-ASCII
-- prefix detects exactly the leftovers (every emoji key starts
-- above U+007F).
DELETE FROM reactions WHERE emoji GLOB '[a-z0-9_]*';

-- +goose Down
-- Not meaningfully invertible (name→glyph is many-to-none after any
-- post-migration reaction); a downgrade wants a from-backup restore.
SELECT 1;
