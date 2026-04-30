-- +goose Up
ALTER TABLE pending_jobs RENAME TO pending_jobs_old;

CREATE TABLE pending_jobs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id INTEGER REFERENCES sessions(id) ON DELETE CASCADE,
    job_id TEXT NOT NULL,
    tool_name TEXT NOT NULL,
    mcp_server TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    result TEXT,
    network TEXT,
    channel TEXT,
    nick TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    completed_at DATETIME
);

INSERT INTO pending_jobs (id, session_id, job_id, tool_name, mcp_server, status, result, created_at, completed_at)
    SELECT id, session_id, job_id, tool_name, mcp_server, status, result, created_at, completed_at
    FROM pending_jobs_old;

DROP TABLE pending_jobs_old;

CREATE INDEX IF NOT EXISTS idx_pending_jobs_session ON pending_jobs(session_id);
CREATE INDEX IF NOT EXISTS idx_pending_jobs_status ON pending_jobs(status);
CREATE INDEX IF NOT EXISTS idx_pending_jobs_tool_job ON pending_jobs(job_id);

-- +goose Down
ALTER TABLE pending_jobs RENAME TO pending_jobs_new;

CREATE TABLE pending_jobs (
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

INSERT INTO pending_jobs (id, session_id, job_id, tool_name, mcp_server, status, result, created_at, completed_at)
    SELECT id, session_id, job_id, tool_name, mcp_server, status, result, created_at, completed_at
    FROM pending_jobs_new
    WHERE session_id IS NOT NULL;

DROP TABLE pending_jobs_new;

CREATE INDEX IF NOT EXISTS idx_pending_jobs_session ON pending_jobs(session_id);
CREATE INDEX IF NOT EXISTS idx_pending_jobs_status ON pending_jobs(status);
