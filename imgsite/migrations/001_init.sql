-- +goose Up
-- Full imgsite schema ships complete in migration 001 so later milestones
-- (extraction, thumbnails, search) add no migrations.
--
-- sha256 is deliberately NOT unique: dedupe means N gallery rows can point
-- at one stored file (the store skips writes for known hashes).
CREATE TABLE images (
    id            TEXT PRIMARY KEY,      -- short public id (7-char base62)
    sha256        TEXT NOT NULL,
    filename      TEXT NOT NULL,         -- dave-chosen name, sanitized
    mime_type     TEXT NOT NULL,
    size_bytes    INTEGER NOT NULL,
    width         INTEGER,
    height        INTEGER,
    created_at    TEXT NOT NULL,         -- upload time, UTC "YYYY-MM-DD HH:MM:SS"
    thumb_status  TEXT NOT NULL DEFAULT 'pending',  -- pending|ready|failed
    hidden        INTEGER NOT NULL DEFAULT 0,       -- soft delete

    -- prompt metadata (source: upload meta, EXIF, or merged)
    original_prompt TEXT NOT NULL DEFAULT '',
    enhanced_prompt TEXT NOT NULL DEFAULT '',
    negative_prompt TEXT NOT NULL DEFAULT '',
    reasoning       TEXT NOT NULL DEFAULT '',
    job_id          TEXT,
    llm_generated   INTEGER NOT NULL DEFAULT 0,
    -- provenance extras sent by dave (display-only)
    network  TEXT, channel TEXT, nick TEXT, workflow_name TEXT,

    -- graph-derived params (populated by the extraction milestone)
    seed INTEGER, steps INTEGER, cfg REAL, denoise REAL,
    sampler TEXT, scheduler TEXT,
    model_unet TEXT, model_clip TEXT, model_vae TEXT,
    loras TEXT,                -- JSON array [{"name":…,"strength":…}]
    workflow_json TEXT NOT NULL DEFAULT '',  -- full embedded graph
    meta_source TEXT NOT NULL DEFAULT ''     -- "upload" | "exif" | "upload+exif"
);

CREATE INDEX idx_images_created ON images(created_at DESC, id DESC);
CREATE INDEX idx_images_sha ON images(sha256);

-- External-content FTS5: the index stores only the two prompt columns and
-- joins back to images on rowid. The prefix index (2/3/4 chars) powers
-- as-you-type search; porter+unicode61 gives stemming.
CREATE VIRTUAL TABLE images_fts USING fts5(
    original_prompt, enhanced_prompt,
    content='images', content_rowid='rowid',
    tokenize="porter unicode61", prefix='2 3 4'
);

-- Triggers maintaining the external-content index. Each is wrapped in
-- StatementBegin/StatementEnd because goose's statement splitter would
-- otherwise cut the trigger body at its internal semicolons. The 'delete'
-- command form (inserting into images_fts with the images_fts column set to
-- 'delete' and the OLD values) is the required dance for content= tables.
-- +goose StatementBegin
CREATE TRIGGER images_fts_ai AFTER INSERT ON images BEGIN
    INSERT INTO images_fts(rowid, original_prompt, enhanced_prompt)
    VALUES (new.rowid, new.original_prompt, new.enhanced_prompt);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER images_fts_ad AFTER DELETE ON images BEGIN
    INSERT INTO images_fts(images_fts, rowid, original_prompt, enhanced_prompt)
    VALUES ('delete', old.rowid, old.original_prompt, old.enhanced_prompt);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER images_fts_au AFTER UPDATE OF original_prompt, enhanced_prompt ON images BEGIN
    INSERT INTO images_fts(images_fts, rowid, original_prompt, enhanced_prompt)
    VALUES ('delete', old.rowid, old.original_prompt, old.enhanced_prompt);
    INSERT INTO images_fts(rowid, original_prompt, enhanced_prompt)
    VALUES (new.rowid, new.original_prompt, new.enhanced_prompt);
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS images_fts_au;
DROP TRIGGER IF EXISTS images_fts_ad;
DROP TRIGGER IF EXISTS images_fts_ai;
DROP TABLE IF EXISTS images_fts;
DROP INDEX IF EXISTS idx_images_sha;
DROP INDEX IF EXISTS idx_images_created;
DROP TABLE IF EXISTS images;
