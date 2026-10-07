-- +goose Up
-- Emoji reactions (owner request, Oct 2026): Discord/Slack-style —
-- a token can hold MANY different reactions on one image, one row
-- per (image, token, emoji). The composite PK (image_id, token,
-- emoji) is the entire dedupe story, exactly like the likes table:
-- INSERT ... ON CONFLICT DO NOTHING decides the toggle direction
-- (1 row affected = reacted, 0 rows = DELETE un-reacts).
--
-- emoji is the CONFIGURED short name (ASCII, e.g. "fire"), never the
-- glyph: the glyph is rendered from the live [reactions] config, so
-- curating or re-skinning the set never needs a data migration. Rows
-- for emojis an admin later removes stay dormant (counts unread by
-- the render surfaces) and revive if the name returns.
--
-- created_at rides along in the same UTC text format as likes for a
-- future "when did these land" query; nothing reads it today.
--
-- No FK enforcement: the connection does not enable foreign_keys
-- (uploads never relied on it), and no hard delete exists for images
-- anyway — hidden rows keep their reactions like they keep their
-- files and votes.
CREATE TABLE reactions (
    image_id   TEXT NOT NULL,
    token      TEXT NOT NULL,
    emoji      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (image_id, token, emoji)
);

-- Count lookups (details-page state, card hydration, the reaction
-- button row) all filter by image_id, usually with an emoji
-- predicate; the composite serves both shapes.
CREATE INDEX idx_reactions_image_emoji ON reactions(image_id, emoji);

-- +goose Down
DROP TABLE IF EXISTS reactions;
