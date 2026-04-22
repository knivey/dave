-- +goose Up
ALTER TABLE sessions ADD COLUMN first_message TEXT NOT NULL DEFAULT '';

-- +goose Down
-- SQLite doesn't support DROP COLUMN before 3.35.0, so we recreate the table
ALTER TABLE sessions RENAME TO sessions_old;

CREATE TABLE sessions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    context_key TEXT NOT NULL,
    network TEXT NOT NULL,
    channel TEXT NOT NULL,
    nick TEXT NOT NULL,
    chat_command TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_active DATETIME DEFAULT CURRENT_TIMESTAMP
);

INSERT INTO sessions (id, context_key, network, channel, nick, chat_command, status, created_at, last_active)
    SELECT id, context_key, network, channel, nick, chat_command, status, created_at, last_active FROM sessions_old;

DROP TABLE sessions_old;
