-- +goose Up
-- Anonymous image likes (design: docs/image-site.md "Likes").
--
-- Identity is a cookie token (see likes.go): one row = one like from
-- one anonymous browser. The composite PK (image_id, token) IS the
-- dedupe story — INSERT ... ON CONFLICT DO NOTHING decides the toggle
-- direction — so no unique-violation handling exists anywhere else.
--
-- created_at rides along in the same UTC text format as images so a
-- future "when did the likes land" query is possible; nothing reads
-- it today.
--
-- No FK enforcement: the connection does not enable foreign_keys
-- (uploads never relied on it), and no hard delete exists for images
-- anyway — hidden rows keep their likes exactly like they keep their
-- files and metadata.
CREATE TABLE likes (
    image_id   TEXT NOT NULL,
    token      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (image_id, token)
);

-- Count lookups (details page state, gallery hydration, the liked-sort
-- ORDER BY) all filter by image_id.
CREATE INDEX idx_likes_image ON likes(image_id);

-- +goose Down
DROP TABLE IF EXISTS likes;
