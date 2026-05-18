-- +goose Up
CREATE TABLE notes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    network TEXT NOT NULL,
    channel TEXT NOT NULL,
    user_id INTEGER NOT NULL,
    nick TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_notes_scope_key ON notes(network, channel, key);
CREATE INDEX IF NOT EXISTS idx_notes_scope_user ON notes(network, channel, user_id);
CREATE INDEX IF NOT EXISTS idx_notes_scope_nick ON notes(network, channel, nick);
CREATE INDEX IF NOT EXISTS idx_notes_scope_time ON notes(network, channel, created_at);

CREATE VIRTUAL TABLE notes_fts USING fts5(
    value,
    content='notes',
    content_rowid='id'
);

-- +goose StatementBegin
CREATE TRIGGER notes_ai AFTER INSERT ON notes BEGIN
    INSERT INTO notes_fts(rowid, value) VALUES (new.id, new.value);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER notes_ad AFTER DELETE ON notes BEGIN
    INSERT INTO notes_fts(notes_fts, rowid, value) VALUES('delete', old.id, old.value);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER notes_au AFTER UPDATE ON notes BEGIN
    INSERT INTO notes_fts(notes_fts, rowid, value) VALUES('delete', old.id, old.value);
    INSERT INTO notes_fts(rowid, value) VALUES (new.id, new.value);
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS notes_au;
DROP TRIGGER IF EXISTS notes_ad;
DROP TRIGGER IF EXISTS notes_ai;
DROP TABLE IF EXISTS notes_fts;
DROP TABLE IF EXISTS notes;
