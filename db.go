package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	logxi "github.com/mgutz/logxi/v1"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var theDB *gorm.DB

type DatabaseConfig struct {
	Driver     string `toml:"driver"`
	Path       string `toml:"path"`
	DSN        string `toml:"dsn"`
	MaxAgeDays int    `toml:"max_age_days"`
}

func (c *DatabaseConfig) SetDefaults() {
	if c.Driver == "" {
		c.Driver = "sqlite"
	}
	if c.Path == "" {
		c.Path = "data/dave.db"
	}
	if c.MaxAgeDays == 0 {
		c.MaxAgeDays = 90
	}
}

type Session struct {
	ID           int64   `gorm:"primaryKey;autoIncrement"`
	Network      string  `gorm:"not null;index:idx_sessions_user"`
	Channel      string  `gorm:"not null;index:idx_sessions_user"`
	Nick         string  `gorm:"not null;index:idx_sessions_user"`
	ChatCommand  string  `gorm:"column:chat_command;not null"`
	FirstMessage string  `gorm:"column:first_message;not null;default:''"`
	ConvID       *string `gorm:"column:conv_id;index:idx_sessions_conv_id"`
	ResponseID   *string `gorm:"column:response_id;index:idx_sessions_response_id"`
	Service      string  `gorm:"not null;default:''"`
	Model        string  `gorm:"not null;default:''"`
	Status       string  `gorm:"not null;default:'active';index:idx_sessions_status"`
	CreatedAt    time.Time
	LastActive   time.Time      `gorm:"column:last_active;index:idx_sessions_last_active"`
	DeletedAt    gorm.DeletedAt `gorm:"index"`
	SettingsID   *int64         `gorm:"index:idx_sessions_settings"`
}

type SessionSetting struct {
	ID               int64  `gorm:"primaryKey;autoIncrement"`
	System           string `gorm:"type:text"`
	Model            string
	DetectImages     bool
	MaxImages        int
	MaxContextImages int
	ReasoningEffort  string
	CreatedAt        time.Time
}

type Message struct {
	ID               int64   `gorm:"primaryKey;autoIncrement"`
	SessionID        int64   `gorm:"not null;index:idx_messages_session"`
	Role             string  `gorm:"not null"`
	Content          string  `gorm:"not null;type:text"`
	ToolCalls        *string `gorm:"type:text"`
	ToolCallID       *string
	ReasoningContent *string `gorm:"type:text"`
	MultiContent     *string `gorm:"type:text"`
	IsAsyncResult    bool    `gorm:"default:false"`
	SettingsID       *int64  `gorm:"index:idx_messages_settings"`
	CreatedAt        time.Time
}

type PendingJob struct {
	ID          int64   `gorm:"primaryKey;autoIncrement"`
	SessionID   *int64  `gorm:"index:idx_pending_jobs_session"`
	JobID       string  `gorm:"not null;index:idx_pending_jobs_tool_job"`
	ToolName    string  `gorm:"not null"`
	MCPServer   string  `gorm:"not null"`
	Status      string  `gorm:"not null;default:'pending';index:idx_pending_jobs_status"`
	Result      *string `gorm:"type:text"`
	Network     *string
	Channel     *string
	Nick        *string
	CreatedAt   time.Time
	CompletedAt *time.Time
}

type TurnUsage struct {
	ID               int64  `gorm:"primaryKey;autoIncrement"`
	SessionID        int64  `gorm:"not null;index:idx_turn_usage_session_id"`
	PromptTokens     int    `gorm:"not null;default:0"`
	CompletionTokens int    `gorm:"not null;default:0"`
	CachedTokens     int    `gorm:"not null;default:0"`
	ReasoningTokens  int    `gorm:"not null;default:0"`
	FinishReason     string `gorm:"not null;default:''"`
	APIPath          string `gorm:"not null;default:''"`
	DurationMs       int    `gorm:"not null;default:0"`
	CreatedAt        time.Time
}

func (PendingJob) TableName() string { return "pending_jobs" }

func (TurnUsage) TableName() string { return "turn_usage" }

func (SessionSetting) TableName() string { return "session_settings" }

func initDB(cfg DatabaseConfig, log logxi.Logger) (*gorm.DB, error) {
	var dialector gorm.Dialector
	switch cfg.Driver {
	case "postgres":
		dialector = postgres.Open(cfg.DSN)
	default:
		dir := filepath.Dir(cfg.Path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				return nil, fmt.Errorf("creating database directory %s: %w", dir, err)
			}
		}
		dialector = sqlite.Open(cfg.Path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: newGormLogger(log),
	})
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if cfg.Driver == "sqlite" || cfg.Driver == "" {
		sqlDB, err := db.DB()
		if err != nil {
			return nil, fmt.Errorf("getting underlying db: %w", err)
		}
		sqlDB.SetMaxOpenConns(1)
	}

	if err := db.AutoMigrate(&Session{}, &Message{}, &PendingJob{}, &TurnUsage{}, &SessionSetting{}); err != nil {
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	dbPath := cfg.Path
	if cfg.Driver == "postgres" {
		dbPath = ""
	}
	if err := runMigrations(db, dbPath); err != nil {
		return nil, fmt.Errorf("running schema migrations: %w", err)
	}

	log.Info("Database initialized", "driver", cfg.Driver, "path", cfg.Path)
	return db, nil
}

func closeDB(db *gorm.DB) {
	if db != nil {
		sqlDB, err := db.DB()
		if err == nil {
			sqlDB.Close()
		}
		if logger != nil {
			logger.Info("Database closed")
		}
	}
}

func updateDBSessionFirstMessage(sessionID int64, firstMessage string) error {
	return theDB.Model(&Session{}).Where("id = ? AND first_message = ''", sessionID).
		Update("first_message", firstMessage).Error
}

func updateDBSessionConvID(sessionID int64, convID string) error {
	return theDB.Model(&Session{}).Where("id = ? AND (conv_id IS NULL OR conv_id = '')", sessionID).
		Update("conv_id", convID).Error
}

func updateDBSessionResponseID(sessionID int64, responseID *string) error {
	return theDB.Model(&Session{}).Where("id = ?", sessionID).
		Update("response_id", responseID).Error
}

func insertDBMessage(sessionID int64, role, content string, toolCallsJSON *string, toolCallID *string, reasoningContent *string, multiContentJSON *string) error {
	msg := Message{
		SessionID:        sessionID,
		Role:             role,
		Content:          content,
		ToolCalls:        toolCallsJSON,
		ToolCallID:       toolCallID,
		ReasoningContent: reasoningContent,
		MultiContent:     multiContentJSON,
	}
	if err := theDB.Create(&msg).Error; err != nil {
		return err
	}
	return theDB.Model(&Session{}).Where("id = ?", sessionID).
		Update("last_active", time.Now()).Error
}

func insertDBTurnUsage(sessionID int64, usage *Usage, finishReason, apiPath string, durationMs int) error {
	if usage == nil || sessionID == 0 {
		return nil
	}
	var cachedTokens, reasoningTokens int
	if usage.PromptTokensDetails != nil {
		cachedTokens = int(usage.PromptTokensDetails.CachedTokens)
	}
	if usage.CompletionTokensDetails != nil {
		reasoningTokens = int(usage.CompletionTokensDetails.ReasoningTokens)
	}
	turnUsage := TurnUsage{
		SessionID:        sessionID,
		PromptTokens:     int(usage.PromptTokens),
		CompletionTokens: int(usage.CompletionTokens),
		CachedTokens:     cachedTokens,
		ReasoningTokens:  reasoningTokens,
		FinishReason:     finishReason,
		APIPath:          apiPath,
		DurationMs:       durationMs,
	}
	return theDB.Create(&turnUsage).Error
}

func completeDBOrphanedSessions() (int64, error) {
	result := theDB.Model(&Session{}).
		Where("status = ? AND id NOT IN (?)", "active",
			theDB.Model(&Session{}).Select("MAX(id)").Where("status = ?", "active").Group("network, channel, nick"),
		).
		Update("status", "completed")
	return result.RowsAffected, result.Error
}

func reactivateDBStrandedSessions() (int64, error) {
	result := theDB.Exec(`
		UPDATE sessions SET status = 'active'
		WHERE id IN (
			SELECT MAX(id) FROM sessions WHERE deleted_at IS NULL GROUP BY network, channel, nick
		) AND status = 'completed'
		AND (network, channel, nick) IN (
			SELECT network, channel, nick FROM sessions WHERE deleted_at IS NULL GROUP BY network, channel, nick HAVING SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END) = 0
		)`)
	return result.RowsAffected, result.Error
}

func loadDBSessionMessages(sessionID int64) ([]Message, error) {
	var messages []Message
	err := theDB.Where("session_id = ?", sessionID).Order("id ASC").Find(&messages).Error
	return messages, err
}

func cleanupDBSessions(maxAgeDays int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -maxAgeDays)
	result := theDB.Model(&Session{}).
		Where("status = ? AND last_active < ?", "active", cutoff).
		Update("status", "completed")
	return result.RowsAffected, result.Error
}

func getUserDBSessions(network, channel, nick string, limit int) ([]Session, error) {
	var sessions []Session
	err := theDB.Where("network = ? AND channel = ? AND nick = ?", network, channel, nick).
		Order("last_active DESC").Limit(limit).Find(&sessions).Error
	return sessions, err
}

func getDBSessionByID(id int64) (*Session, error) {
	var session Session
	err := theDB.Where("id = ?", id).First(&session).Error
	if err != nil {
		return nil, err
	}
	return &session, nil
}

func deleteDBSession(id int64) error {
	return theDB.Where("id = ?", id).Delete(&Session{}).Error
}

func deleteUserDBSessions(network, channel, nick string) (int64, error) {
	result := theDB.Where("network = ? AND channel = ? AND nick = ?", network, channel, nick).
		Delete(&Session{})
	return result.RowsAffected, result.Error
}

// purgeDeletedDBSessions permanently removes soft-deleted sessions older than the
// given duration. Not called automatically — intentionally left as an admin utility
// for manual or scheduled invocation (e.g. TUI command, cron). Soft-deleted data is
// retained indefinitely until this is called.
func purgeDeletedDBSessions(olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	result := theDB.Unscoped().Where("deleted_at IS NOT NULL AND deleted_at < ?", cutoff).
		Delete(&Session{})
	return result.RowsAffected, result.Error
}

func getUserDBStats(network, channel, nick string) (int, int, error) {
	var sessionCount int64
	err := theDB.Model(&Session{}).
		Where("network = ? AND channel = ? AND nick = ?", network, channel, nick).
		Count(&sessionCount).Error
	if err != nil {
		return 0, 0, err
	}
	var sessionIDs []int64
	err = theDB.Model(&Session{}).
		Where("network = ? AND channel = ? AND nick = ?", network, channel, nick).
		Pluck("id", &sessionIDs).Error
	if err != nil {
		return int(sessionCount), 0, err
	}
	if len(sessionIDs) == 0 {
		return int(sessionCount), 0, nil
	}
	var messageCount int64
	err = theDB.Model(&Message{}).
		Where("session_id IN ?", sessionIDs).
		Count(&messageCount).Error
	return int(sessionCount), int(messageCount), err
}

func createPendingJob(sessionID int64, jobID, toolName, mcpServer string) error {
	job := PendingJob{
		SessionID: int64Ptr(sessionID),
		JobID:     jobID,
		ToolName:  toolName,
		MCPServer: mcpServer,
		Status:    "pending",
	}
	return theDB.Create(&job).Error
}

func completePendingJob(jobID, resultText string) error {
	now := time.Now()
	return theDB.Model(&PendingJob{}).Where("job_id = ?", jobID).
		Updates(map[string]interface{}{"status": "completed", "result": resultText, "completed_at": &now}).Error
}

func markPendingJobDelivered(jobID string) error {
	return theDB.Model(&PendingJob{}).Where("job_id = ?", jobID).
		Update("status", "delivered").Error
}

func deliverInlinePendingJob(jobID, resultText string) error {
	now := time.Now()
	return theDB.Model(&PendingJob{}).Where("job_id = ? AND status = ?", jobID, "pending").
		Updates(map[string]interface{}{"status": "delivered", "result": resultText, "completed_at": &now}).Error
}

func getCompletedPendingJobs(sessionID int64) ([]PendingJob, error) {
	var jobs []PendingJob
	err := theDB.Where("session_id = ? AND status = ?", sessionID, "completed").
		Order("completed_at ASC").Find(&jobs).Error
	return jobs, err
}

func getPendingJobsForUser(network, channel, nick string) ([]PendingJob, error) {
	var jobs []PendingJob
	err := theDB.Joins("JOIN sessions ON sessions.id = pending_jobs.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.network = ? AND sessions.channel = ? AND sessions.nick = ?", network, channel, nick).
		Where("pending_jobs.status IN ?", []string{"pending", "running", "completed"}).
		Order("pending_jobs.created_at DESC").
		Find(&jobs).Error
	return jobs, err
}

func getPendingJobsForRecovery() ([]PendingJob, error) {
	var jobs []PendingJob
	err := theDB.Where("status IN ? AND session_id IS NOT NULL", []string{"pending", "running"}).
		Find(&jobs).Error
	return jobs, err
}

func createToolPendingJob(jobID, toolName, mcpServer, network, channel, nick string) error {
	job := PendingJob{
		JobID:     jobID,
		ToolName:  toolName,
		MCPServer: mcpServer,
		Status:    "pending",
		Network:   &network,
		Channel:   &channel,
		Nick:      &nick,
	}
	return theDB.Create(&job).Error
}

func completeToolPendingJob(jobID, resultText string) error {
	now := time.Now()
	return theDB.Model(&PendingJob{}).Where("job_id = ?", jobID).
		Updates(map[string]interface{}{"status": "completed", "result": resultText, "completed_at": &now}).Error
}

func getToolPendingJobsForRecovery() ([]PendingJob, error) {
	var jobs []PendingJob
	err := theDB.Where("status IN ? AND session_id IS NULL", []string{"pending", "running"}).
		Find(&jobs).Error
	return jobs, err
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func int64Ptr(i int64) *int64 {
	return &i
}
