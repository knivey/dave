-- +goose Up
CREATE TABLE IF NOT EXISTS sessions (
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

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    role TEXT NOT NULL,
    content TEXT NOT NULL,
    tool_calls TEXT,
    tool_call_id TEXT,
    reasoning_content TEXT,
    is_async_result BOOLEAN DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS pending_jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    job_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    mcp_server TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    result TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME
);

CREATE INDEX IF NOT EXISTS idx_sessions_context_key ON sessions(context_key);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(network, channel, nick);
CREATE INDEX IF NOT EXISTS idx_sessions_status ON sessions(status);
CREATE INDEX IF NOT EXISTS idx_sessions_last_active ON sessions(last_active);
CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
CREATE INDEX IF NOT EXISTS idx_pending_jobs_session ON pending_jobs(session_id);
CREATE INDEX IF NOT EXISTS idx_pending_jobs_status ON pending_jobs(status);

-- +goose Down
DROP INDEX IF EXISTS idx_pending_jobs_status;
DROP INDEX IF EXISTS idx_pending_jobs_session;
DROP INDEX IF EXISTS idx_messages_session;
DROP INDEX IF EXISTS idx_sessions_last_active;
DROP INDEX IF EXISTS idx_sessions_status;
DROP INDEX IF EXISTS idx_sessions_user;
DROP INDEX IF EXISTS idx_sessions_context_key;
DROP TABLE IF EXISTS pending_jobs;
DROP TABLE IF EXISTS messages;
DROP TABLE IF EXISTS sessions;
