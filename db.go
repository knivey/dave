package main

import (
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/jmoiron/sqlx"
	logxi "github.com/mgutz/logxi/v1"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

var theDB *sqlx.DB
var loggerDB = logxi.New("db")

type DatabaseConfig struct {
	Path       string `toml:"path"`
	MaxAgeDays int    `toml:"max_age_days"`
}

func (c *DatabaseConfig) SetDefaults() {
	if c.Path == "" {
		c.Path = "data/dave.db"
	}
	if c.MaxAgeDays == 0 {
		c.MaxAgeDays = 90
	}
}

func initDB(cfg DatabaseConfig) (*sqlx.DB, error) {
	dbPath := cfg.Path
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating database directory %s: %w", dir, err)
		}
	}

	sqldb, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", dbPath, err)
	}

	sqldb.SetMaxOpenConns(1)

	if err := goose.SetDialect("sqlite3"); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("setting goose dialect: %w", err)
	}

	goose.SetBaseFS(embedMigrations)

	if err := goose.Up(sqldb, "migrations"); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	db := sqlx.NewDb(sqldb, "sqlite")

	loggerDB.Info("Database initialized", "path", dbPath)
	return db, nil
}

func closeDB(db *sqlx.DB) {
	if db != nil {
		db.Close()
		loggerDB.Info("Database closed")
	}
}

type dbSession struct {
	ID           int64   `db:"id"`
	ContextKey   string  `db:"context_key"`
	Network      string  `db:"network"`
	Channel      string  `db:"channel"`
	Nick         string  `db:"nick"`
	ChatCommand  string  `db:"chat_command"`
	FirstMessage string  `db:"first_message"`
	ConvID       *string `db:"conv_id"`
	ResponseID   *string `db:"response_id"`
	Status       string  `db:"status"`
	CreatedAt    string  `db:"created_at"`
	LastActive   string  `db:"last_active"`
}

type dbMessage struct {
	ID               int64   `db:"id"`
	SessionID        int64   `db:"session_id"`
	Role             string  `db:"role"`
	Content          string  `db:"content"`
	ToolCalls        *string `db:"tool_calls"`
	ToolCallID       *string `db:"tool_call_id"`
	ReasoningContent *string `db:"reasoning_content"`
	IsAsyncResult    bool    `db:"is_async_result"`
	CreatedAt        string  `db:"created_at"`
}

func createDBSession(contextKey, network, channel, nick, chatCommand, convID string) (int64, error) {
	result, err := theDB.Exec(
		"INSERT INTO sessions (context_key, network, channel, nick, chat_command, conv_id, status) VALUES (?, ?, ?, ?, ?, ?, 'active')",
		contextKey, network, channel, nick, chatCommand, convID,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func updateDBSessionFirstMessage(sessionID int64, firstMessage string) error {
	_, err := theDB.Exec(
		"UPDATE sessions SET first_message = ? WHERE id = ? AND first_message = ''",
		firstMessage, sessionID,
	)
	return err
}

func updateDBSessionConvID(sessionID int64, convID string) error {
	_, err := theDB.Exec(
		"UPDATE sessions SET conv_id = ? WHERE id = ? AND (conv_id IS NULL OR conv_id = '')",
		convID, sessionID,
	)
	return err
}

func updateDBSessionResponseID(sessionID int64, responseID *string) error {
	_, err := theDB.Exec(
		"UPDATE sessions SET response_id = ? WHERE id = ?",
		responseID, sessionID,
	)
	return err
}

func insertDBMessage(sessionID int64, role, content string, toolCallsJSON *string, toolCallID *string, reasoningContent *string) error {
	_, err := theDB.Exec(
		"INSERT INTO messages (session_id, role, content, tool_calls, tool_call_id, reasoning_content) VALUES (?, ?, ?, ?, ?, ?)",
		sessionID, role, content, toolCallsJSON, toolCallID, reasoningContent,
	)
	if err != nil {
		return err
	}
	_, err = theDB.Exec(
		"UPDATE sessions SET last_active = CURRENT_TIMESTAMP WHERE id = ?",
		sessionID,
	)
	return err
}

func completeDBSession(sessionID int64) error {
	_, file, line, _ := runtime.Caller(1)
	loggerDB.Info("completing session", "id", sessionID, "caller", fmt.Sprintf("%s:%d", file, line))
	_, err := theDB.Exec(
		"UPDATE sessions SET status = 'completed' WHERE id = ?",
		sessionID,
	)
	return err
}

func completeDBOrphanedSessions() (int64, error) {
	result, err := theDB.Exec(`
		UPDATE sessions SET status = 'completed'
		WHERE status = 'active'
		AND id NOT IN (
			SELECT MAX(id) FROM sessions WHERE status = 'active' GROUP BY context_key
		)`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func reactivateDBStrandedSessions() (int64, error) {
	result, err := theDB.Exec(`
		UPDATE sessions SET status = 'active'
		WHERE id IN (
			SELECT MAX(id) FROM sessions GROUP BY context_key
		) AND status = 'completed'
		AND context_key IN (
			SELECT context_key FROM sessions GROUP BY context_key HAVING SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END) = 0
		)`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func loadActiveDBSessions() ([]dbSession, error) {
	var sessions []dbSession
	err := theDB.Select(&sessions, "SELECT * FROM sessions WHERE status = 'active' ORDER BY last_active DESC")
	return sessions, err
}

func loadDBSessionMessages(sessionID int64) ([]dbMessage, error) {
	var messages []dbMessage
	err := theDB.Select(&messages, "SELECT * FROM messages WHERE session_id = ? ORDER BY id ASC", sessionID)
	return messages, err
}

func cleanupDBSessions(maxAgeDays int) (int64, error) {
	result, err := theDB.Exec(
		"UPDATE sessions SET status = 'completed' WHERE status = 'active' AND last_active < datetime('now', ? || ' days')",
		fmt.Sprintf("-%d", maxAgeDays),
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func getUserDBSessions(network, channel, nick string, limit int) ([]dbSession, error) {
	var sessions []dbSession
	err := theDB.Select(&sessions,
		"SELECT * FROM sessions WHERE network = ? AND channel = ? AND nick = ? ORDER BY last_active DESC LIMIT ?",
		network, channel, nick, limit,
	)
	return sessions, err
}

func getDBSessionByID(id int64) (*dbSession, error) {
	var session dbSession
	err := theDB.Get(&session, "SELECT * FROM sessions WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func deleteDBSession(id int64) error {
	_, err := theDB.Exec("DELETE FROM sessions WHERE id = ?", id)
	return err
}

func deleteUserDBSessions(network, channel, nick string) (int64, error) {
	result, err := theDB.Exec(
		"DELETE FROM sessions WHERE network = ? AND channel = ? AND nick = ?",
		network, channel, nick,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func getUserDBStats(network, channel, nick string) (sessionCount int, messageCount int, err error) {
	err = theDB.Get(&sessionCount,
		"SELECT COUNT(*) FROM sessions WHERE network = ? AND channel = ? AND nick = ?",
		network, channel, nick,
	)
	if err != nil {
		return 0, 0, err
	}
	err = theDB.Get(&messageCount,
		`SELECT COUNT(*) FROM messages WHERE session_id IN (
			SELECT id FROM sessions WHERE network = ? AND channel = ? AND nick = ?
		)`,
		network, channel, nick,
	)
	return sessionCount, messageCount, err
}

type pendingJob struct {
	ID          int64   `db:"id"`
	SessionID   *int64  `db:"session_id"`
	JobID       string  `db:"job_id"`
	ToolName    string  `db:"tool_name"`
	MCPServer   string  `db:"mcp_server"`
	Status      string  `db:"status"`
	Result      *string `db:"result"`
	Network     *string `db:"network"`
	Channel     *string `db:"channel"`
	Nick        *string `db:"nick"`
	CreatedAt   string  `db:"created_at"`
	CompletedAt *string `db:"completed_at"`
}

func createPendingJob(sessionID int64, jobID, toolName, mcpServer string) error {
	_, err := theDB.Exec(
		"INSERT INTO pending_jobs (session_id, job_id, tool_name, mcp_server, status) VALUES (?, ?, ?, ?, 'pending')",
		sessionID, jobID, toolName, mcpServer,
	)
	return err
}

func completePendingJob(jobID, resultText string) error {
	_, err := theDB.Exec(
		"UPDATE pending_jobs SET status = 'completed', result = ?, completed_at = CURRENT_TIMESTAMP WHERE job_id = ?",
		resultText, jobID,
	)
	return err
}

func markPendingJobDelivered(jobID string) error {
	_, err := theDB.Exec(
		"UPDATE pending_jobs SET status = 'delivered' WHERE job_id = ?",
		jobID,
	)
	return err
}

func getCompletedPendingJobs(sessionID int64) ([]pendingJob, error) {
	var jobs []pendingJob
	err := theDB.Select(&jobs,
		"SELECT * FROM pending_jobs WHERE session_id = ? AND status = 'completed' ORDER BY completed_at ASC",
		sessionID,
	)
	return jobs, err
}

func getPendingJobsForUser(network, channel, nick string) ([]pendingJob, error) {
	var jobs []pendingJob
	err := theDB.Select(&jobs,
		`SELECT p.* FROM pending_jobs p
		JOIN sessions s ON p.session_id = s.id
		WHERE s.network = ? AND s.channel = ? AND s.nick = ?
		AND p.status IN ('pending', 'running', 'completed')
		ORDER BY p.created_at DESC`,
		network, channel, nick,
	)
	return jobs, err
}

func getPendingJobsForRecovery() ([]pendingJob, error) {
	var jobs []pendingJob
	err := theDB.Select(&jobs,
		"SELECT * FROM pending_jobs WHERE status IN ('pending', 'running') AND session_id IS NOT NULL",
	)
	return jobs, err
}

func createToolPendingJob(jobID, toolName, mcpServer, network, channel, nick string) error {
	_, err := theDB.Exec(
		"INSERT INTO pending_jobs (session_id, job_id, tool_name, mcp_server, status, network, channel, nick) VALUES (NULL, ?, ?, ?, 'pending', ?, ?, ?)",
		jobID, toolName, mcpServer, network, channel, nick,
	)
	return err
}

func completeToolPendingJob(jobID, resultText string) error {
	_, err := theDB.Exec(
		"UPDATE pending_jobs SET status = 'completed', result = ?, completed_at = CURRENT_TIMESTAMP WHERE job_id = ?",
		resultText, jobID,
	)
	return err
}

func getToolPendingJobsForRecovery() ([]pendingJob, error) {
	var jobs []pendingJob
	err := theDB.Select(&jobs,
		"SELECT * FROM pending_jobs WHERE status IN ('pending', 'running') AND session_id IS NULL",
	)
	return jobs, err
}
