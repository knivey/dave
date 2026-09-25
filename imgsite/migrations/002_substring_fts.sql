-- +goose Up
-- Trigram-indexed side table accelerating tier-3 substring search.
--
-- The main images_fts table (migration 001) must stay unicode61+porter:
-- its token semantics and bm25 are what make the two-tier priority work.
-- Tier 3's LIKE '%…%' scan, however, cannot use any index — so this
-- second external-content FTS5 table indexes the same two prompt columns
-- with the trigram tokenizer, whose 3-code-point-window inverted index
-- answers "does this text contain this ≥3-char substring" without
-- scanning the row text. search.go keeps the LIKE conjuncts as the
-- semantic definition and adds a `rowid IN (… MATCH …)` prefilter from
-- this table; the LIKE still decides (the trigram match set is a
-- superset), so results are byte-identical.
--
-- tokenize='trigram' default options: case_sensitive=0 (case-insensitive
-- — matches LIKE's ASCII folding and more, and a wider prefilter is
-- always safe), remove_diacritics=0 (the option's default, deliberately
-- conservative: no diacritic folding, mirroring LIKE's byte-exact
-- behavior for non-ASCII text). Tokens
-- shorter than 3 code points cannot be indexed by trigram and fall back
-- to the plain scan (rare: short queries).
--
-- Index cost note: trigram stores every 3-char window, roughly 3–5× the
-- indexed text size — two prompt columns at gallery scale is trivially
-- small next to the image files themselves.
CREATE VIRTUAL TABLE images_substring_fts USING fts5(
    original_prompt, enhanced_prompt,
    content='images', content_rowid='rowid',
    tokenize='trigram'
);

-- Same trigger trio as 001 (external-content 'delete' dance). Wrapped in
-- StatementBegin/End for goose's statement splitter.
-- +goose StatementBegin
CREATE TRIGGER images_substring_fts_ai AFTER INSERT ON images BEGIN
    INSERT INTO images_substring_fts(rowid, original_prompt, enhanced_prompt)
    VALUES (new.rowid, new.original_prompt, new.enhanced_prompt);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER images_substring_fts_ad AFTER DELETE ON images BEGIN
    INSERT INTO images_substring_fts(images_substring_fts, rowid, original_prompt, enhanced_prompt)
    VALUES ('delete', old.rowid, old.original_prompt, old.enhanced_prompt);
END;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE TRIGGER images_substring_fts_au AFTER UPDATE OF original_prompt, enhanced_prompt ON images BEGIN
    INSERT INTO images_substring_fts(images_substring_fts, rowid, original_prompt, enhanced_prompt)
    VALUES ('delete', old.rowid, old.original_prompt, old.enhanced_prompt);
    INSERT INTO images_substring_fts(rowid, original_prompt, enhanced_prompt)
    VALUES (new.rowid, new.original_prompt, new.enhanced_prompt);
END;
-- +goose StatementEnd

-- One-time population from rows that predate this table. Runs at
-- startup; at gallery scale (hundreds→low thousands of rows) this is a
-- seconds-long rebuild.
INSERT INTO images_substring_fts(images_substring_fts) VALUES('rebuild');

-- +goose Down
DROP TRIGGER IF EXISTS images_substring_fts_au;
DROP TRIGGER IF EXISTS images_substring_fts_ad;
DROP TRIGGER IF EXISTS images_substring_fts_ai;
DROP TABLE IF EXISTS images_substring_fts;
