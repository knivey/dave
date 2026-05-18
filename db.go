package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	logxi "github.com/mgutz/logxi/v1"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

var theDB *gorm.DB

const (
	StatusActive    = "active"
	StatusCompleted = "completed"
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusDelivered = "delivered"
)

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
	UserID       *int64         `gorm:"index:idx_sessions_user"`
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
	ID                 int64   `gorm:"primaryKey;autoIncrement"`
	SessionID          int64   `gorm:"not null;index:idx_messages_session"`
	Role               string  `gorm:"not null"`
	Content            string  `gorm:"not null;type:text"`
	ToolCalls          *string `gorm:"type:text"`
	ToolCallID         *string
	ReasoningContent   *string `gorm:"type:text"`
	EncryptedReasoning *string `gorm:"type:text"`
	MultiContent       *string `gorm:"type:text"`
	IsAsyncResult      bool    `gorm:"default:false"`
	SettingsID         *int64  `gorm:"index:idx_messages_settings"`
	// Archived: when true, this message has been compacted into a summary and is
	// no longer included in the active history sent to the LLM. Originals are
	// preserved for the history viewer and future reconstruction. CompactionID
	// references the Compaction row that performed the archival.
	Archived     bool   `gorm:"not null;default:false;index:idx_messages_archived"`
	CompactionID *int64 `gorm:"index:idx_messages_compaction"`
	// SourceCompactionID, when non-nil, indicates this row was inserted as a
	// tail-copy by the referenced compaction event (i.e. it duplicates content
	// that the prior compaction had archived from a higher row id). On the
	// NEXT compaction, rows with SourceCompactionID set are marked
	// Superseded=true rather than counted as fresh archived material; this
	// keeps user-visible archived counts and the history viewer from being
	// inflated by repeated re-archival of the same content. See compaction.go
	// for the transaction logic.
	SourceCompactionID *int64 `gorm:"index:idx_messages_source_compaction"`
	// Superseded: hidden-from-users flag for tail-copy rows that have already
	// been re-archived by a later compaction. Such rows still exist on disk
	// (no GC in this change; relies on MaxAgeDays session cleanup) but are
	// excluded from loadDBSessionMessagesAll and from the historySessions
	// archived count. They are NOT visible in any UI surface.
	Superseded bool `gorm:"not null;default:false;index:idx_messages_superseded"`
	CreatedAt  time.Time
}

// Compaction records a single session-compaction event. Archived messages
// reference this row via Message.CompactionID. See docs / AGENTS.md and
// session compacting design notes.
type Compaction struct {
	ID               int64 `gorm:"primaryKey;autoIncrement"`
	SessionID        int64 `gorm:"not null;index:idx_compactions_session"`
	SummaryMessageID int64 `gorm:"not null"`
	FirstArchivedID  int64 `gorm:"not null"`
	LastArchivedID   int64 `gorm:"not null"`
	ArchivedCount    int   `gorm:"not null"`
	Service          string
	Model            string
	PromptTokens     int    `gorm:"default:0"`
	CompletionTokens int    `gorm:"default:0"`
	DurationMs       int    `gorm:"default:0"`
	Trigger          string `gorm:"not null;default:'manual'"`
	CreatedAt        time.Time
}

func (Compaction) TableName() string { return "compactions" }

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
	UserID      *int64
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

// User represents a tracked IRC identity.
//
// NormalizedNick uniqueness is enforced by a *partial* unique index:
//
//	UNIQUE (network, normalized_nick) WHERE released = false AND flagged = false
//
// created in migration #7 (convert_sentinels_to_released_column). The
// struct tag therefore does NOT request a full unique index — GORM cannot
// express partial indexes in struct tags. Released and flagged rows
// preserve their real normalized_nick (no sentinels); the partial index
// simply ignores them so another active user can claim the same nick.
type User struct {
	ID             int64  `gorm:"primaryKey;autoIncrement"`
	Network        string `gorm:"not null;index:idx_users_account"`
	CurrentNick    string `gorm:"not null"`
	NormalizedNick string `gorm:"not null"`
	IRCAccount     string `gorm:"column:account;index:idx_users_account"`
	Released       bool   `gorm:"not null;default:false;index:idx_users_released"`
	Flagged        bool   `gorm:"not null;default:false;index:idx_users_flagged"`
	FlaggedReason  string `gorm:"not null;default:''"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func (User) TableName() string { return "users" }

type UserKnownHost struct {
	ID        int64  `gorm:"primaryKey;autoIncrement"`
	UserID    int64  `gorm:"not null;index:idx_user_known_hosts_user;uniqueIndex:idx_user_known_hosts_identity"`
	Ident     string `gorm:"not null;uniqueIndex:idx_user_known_hosts_identity"`
	Host      string `gorm:"not null;uniqueIndex:idx_user_known_hosts_identity;index:idx_user_known_hosts_lookup"`
	FirstSeen time.Time
	LastSeen  time.Time
}

func (UserKnownHost) TableName() string { return "user_known_hosts" }

type NickChange struct {
	ID            int64  `gorm:"primaryKey;autoIncrement"`
	UserID        int64  `gorm:"not null;index:idx_nick_changes_user"`
	OldNick       string `gorm:"not null"`
	NewNick       string `gorm:"not null"`
	NormalizedOld string `gorm:"not null;index:idx_nick_changes_norm"`
	NormalizedNew string `gorm:"not null;index:idx_nick_changes_norm"`
	CreatedAt     time.Time
}

func (NickChange) TableName() string { return "nick_changes" }

type Ban struct {
	ID            int64  `gorm:"primaryKey;autoIncrement"`
	UserID        int64  `gorm:"not null;index:idx_bans_user"`
	Network       string `gorm:"not null;index:idx_bans_lookup"`
	Channel       string `gorm:"index:idx_bans_lookup"`
	ServiceScope  string `gorm:"column:service_scope"`
	Reason        string
	Duration      time.Duration
	ExpiresAt     time.Time `gorm:"index:idx_bans_active"`
	Active        bool      `gorm:"not null;default:true;index:idx_bans_active"`
	DeactivatedAt *time.Time
	BannerUserID  *int64 `gorm:"index:idx_bans_banner"`
	BannerNick    string
	CreatedAt     time.Time
}

func (Ban) TableName() string { return "bans" }

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

	if err := db.AutoMigrate(
		&Session{}, &Message{}, &PendingJob{}, &TurnUsage{}, &SessionSetting{},
		&User{}, &UserKnownHost{}, &NickChange{}, &Ban{}, &Compaction{},
	); err != nil {
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

func insertDBMessage(sessionID int64, role, content string, toolCallsJSON *string, toolCallID *string, reasoningContent *string, encryptedReasoning *string, multiContentJSON *string) error {
	msg := Message{
		SessionID:          sessionID,
		Role:               role,
		Content:            content,
		ToolCalls:          toolCallsJSON,
		ToolCallID:         toolCallID,
		ReasoningContent:   reasoningContent,
		EncryptedReasoning: encryptedReasoning,
		MultiContent:       multiContentJSON,
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
		Where("status = ? AND id NOT IN (?)", StatusActive,
			theDB.Model(&Session{}).Select("MAX(id)").Where("status = ?", StatusActive).Group("network, channel, user_id"),
		).
		Update("status", StatusCompleted)
	return result.RowsAffected, result.Error
}

func reactivateDBStrandedSessions() (int64, error) {
	result := theDB.Exec(`
		UPDATE sessions SET status = 'active'
		WHERE id IN (
			SELECT MAX(id) FROM sessions WHERE deleted_at IS NULL GROUP BY network, channel, user_id
		) AND status = 'completed'
		AND (network, channel, user_id) IN (
			SELECT network, channel, user_id FROM sessions WHERE deleted_at IS NULL GROUP BY network, channel, user_id HAVING SUM(CASE WHEN status = 'active' THEN 1 ELSE 0 END) = 0
		)`)
	return result.RowsAffected, result.Error
}

// loadDBSessionMessages returns the active (non-archived) messages for a
// session, ordered by id. Compacted/archived messages are excluded so they
// do not appear in the live history sent to the LLM. Use
// loadDBSessionMessagesAll to inspect the full record including archived rows.
func loadDBSessionMessages(sessionID int64) ([]Message, error) {
	var messages []Message
	err := theDB.Where("session_id = ? AND archived = ?", sessionID, false).
		Order("id ASC").Find(&messages).Error
	return messages, err
}

// loadDBSessionMessagesAll returns every message for a session including
// archived rows, excluding only superseded tail-copy ghosts (rows that have
// been re-archived by a later compaction and represent content already
// covered by an existing summary). Used by the history viewer for read-only
// display of compacted ranges. Superseded rows live on disk until session
// cleanup but never surface to users.
func loadDBSessionMessagesAll(sessionID int64) ([]Message, error) {
	var messages []Message
	err := theDB.Where("session_id = ? AND superseded = ?", sessionID, false).
		Order("id ASC").Find(&messages).Error
	return messages, err
}

// loadDBSessionMessagesIncludingSuperseded returns absolutely every row for a
// session — including superseded tail-copy ghosts. Intended for tests and
// debug tooling only. Production code paths should use
// loadDBSessionMessagesAll.
func loadDBSessionMessagesIncludingSuperseded(sessionID int64) ([]Message, error) {
	var messages []Message
	err := theDB.Where("session_id = ?", sessionID).Order("id ASC").Find(&messages).Error
	return messages, err
}

// archiveMessagesRange marks every message in the (inclusive) id range as
// archived and links it to the given compaction. Operates on the supplied
// transaction so the caller can run it inside an outer atomic block.
func archiveMessagesRange(tx *gorm.DB, sessionID, compactionID, firstID, lastID int64) error {
	return tx.Model(&Message{}).
		Where("session_id = ? AND id >= ? AND id <= ?", sessionID, firstID, lastID).
		Updates(map[string]interface{}{"archived": true, "compaction_id": compactionID}).Error
}

// archiveMessageByID archives a single message (used for the original system
// prompt row, which lives outside the contiguous compacted range).
func archiveMessageByID(tx *gorm.DB, messageID, compactionID int64) error {
	return tx.Model(&Message{}).Where("id = ?", messageID).
		Updates(map[string]interface{}{"archived": true, "compaction_id": compactionID}).Error
}

// markMessagesSupersededByIDs marks a set of rows as superseded by a later
// compaction. Used during compaction #N to neutralize tail-copies inserted by
// compaction #N-1 (or earlier): the underlying content is already represented
// by the earlier compaction's summary, so re-archiving these rows would
// inflate user-visible archived counts and clutter the history viewer.
//
// Rows are also marked archived=true with compaction_id set to the new
// compaction event so the bookkeeping is self-consistent — the row IS
// archived (not live), it's just hidden from any user-facing surface.
func markMessagesSupersededByIDs(tx *gorm.DB, ids []int64, compactionID int64) error {
	if len(ids) == 0 {
		return nil
	}
	return tx.Model(&Message{}).Where("id IN ?", ids).
		Updates(map[string]interface{}{
			"superseded":    true,
			"archived":      true,
			"compaction_id": compactionID,
		}).Error
}

// getCompactionsForSession returns all compaction events for a session
// ordered by creation time.
func getCompactionsForSession(sessionID int64) ([]Compaction, error) {
	var rows []Compaction
	err := theDB.Where("session_id = ?", sessionID).Order("id ASC").Find(&rows).Error
	return rows, err
}

// getLastTurnUsageForSession returns the most recent recorded TurnUsage for
// a session, used to gauge whether the next turn is at risk of exceeding
// the model's context window.
func getLastTurnUsageForSession(sessionID int64) (*TurnUsage, error) {
	var u TurnUsage
	err := theDB.Where("session_id = ?", sessionID).Order("id DESC").First(&u).Error
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func cleanupDBSessions(maxAgeDays int) (int64, error) {
	cutoff := time.Now().AddDate(0, 0, -maxAgeDays)
	result := theDB.Model(&Session{}).
		Where("status = ? AND last_active < ?", StatusActive, cutoff).
		Update("status", StatusCompleted)
	return result.RowsAffected, result.Error
}

func getUserDBSessions(network, channel string, userID int64, limit int) ([]Session, error) {
	var sessions []Session
	err := theDB.Where("network = ? AND channel = ? AND user_id = ?", network, channel, userID).
		Order("last_active DESC").Limit(limit).Find(&sessions).Error
	return sessions, err
}

func getUserDBSessionsByNetwork(network string, userID int64, limit int) ([]Session, error) {
	var sessions []Session
	err := theDB.Where("network = ? AND user_id = ?", network, userID).
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

func deleteUserDBSessions(network, channel string, userID int64) (int64, error) {
	result := theDB.Where("network = ? AND channel = ? AND user_id = ?", network, channel, userID).
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

func getUserDBStats(userID int64, network, channel string) (sessionCount int, messageCount int, err error) {
	q := theDB.Model(&Session{}).Where("user_id = ?", userID)
	if network != "" {
		q = q.Where("network = ?", network)
	}
	if channel != "" {
		q = q.Where("channel = ?", channel)
	}
	var sc int64
	err = q.Count(&sc).Error
	if err != nil {
		return 0, 0, err
	}
	var sessionIDs []int64
	q2 := theDB.Model(&Session{}).Where("user_id = ?", userID)
	if network != "" {
		q2 = q2.Where("network = ?", network)
	}
	if channel != "" {
		q2 = q2.Where("channel = ?", channel)
	}
	err = q2.Pluck("id", &sessionIDs).Error
	if err != nil {
		return int(sc), 0, err
	}
	if len(sessionIDs) == 0 {
		return int(sc), 0, nil
	}
	var mc int64
	err = theDB.Model(&Message{}).Where("session_id IN ?", sessionIDs).Count(&mc).Error
	return int(sc), int(mc), err
}

func createPendingJob(sessionID int64, jobID, toolName, mcpServer string) error {
	job := PendingJob{
		SessionID: int64Ptr(sessionID),
		JobID:     jobID,
		ToolName:  toolName,
		MCPServer: mcpServer,
		Status:    StatusPending,
	}
	return theDB.Create(&job).Error
}

func completePendingJob(jobID, resultText string) error {
	now := time.Now()
	return theDB.Model(&PendingJob{}).Where("job_id = ?", jobID).
		Updates(map[string]interface{}{"status": StatusCompleted, "result": resultText, "completed_at": &now}).Error
}

func markPendingJobDelivered(jobID string) error {
	return theDB.Model(&PendingJob{}).Where("job_id = ?", jobID).
		Update("status", StatusDelivered).Error
}

func deliverInlinePendingJob(jobID, resultText string) error {
	now := time.Now()
	return theDB.Model(&PendingJob{}).Where("job_id = ? AND status = ?", jobID, StatusPending).
		Updates(map[string]interface{}{"status": StatusDelivered, "result": resultText, "completed_at": &now}).Error
}

func getCompletedPendingJobs(sessionID int64) ([]PendingJob, error) {
	var jobs []PendingJob
	err := theDB.Where("session_id = ? AND status = ?", sessionID, StatusCompleted).
		Order("completed_at ASC").Find(&jobs).Error
	return jobs, err
}

func getPendingJobsForUser(network, channel string, userID int64) ([]PendingJob, error) {
	var jobs []PendingJob
	err := theDB.Joins("JOIN sessions ON sessions.id = pending_jobs.session_id AND sessions.deleted_at IS NULL").
		Where("sessions.network = ? AND sessions.channel = ? AND sessions.user_id = ?", network, channel, userID).
		Where("pending_jobs.status IN ?", []string{StatusPending, StatusRunning, StatusCompleted}).
		Order("pending_jobs.created_at DESC").
		Find(&jobs).Error
	return jobs, err
}

func getPendingJobsForRecovery(requireSessionID bool) ([]PendingJob, error) {
	var jobs []PendingJob
	q := theDB.Where("status IN ?", []string{StatusPending, StatusRunning})
	if requireSessionID {
		q = q.Where("session_id IS NOT NULL")
	} else {
		q = q.Where("session_id IS NULL")
	}
	err := q.Find(&jobs).Error
	return jobs, err
}

func createToolPendingJob(jobID, toolName, mcpServer, network, channel, nick string, userID int64) error {
	job := PendingJob{
		JobID:     jobID,
		ToolName:  toolName,
		MCPServer: mcpServer,
		Status:    StatusPending,
		Network:   &network,
		Channel:   &channel,
		Nick:      &nick,
		UserID:    &userID,
	}
	return theDB.Create(&job).Error
}

type SessionWithUser struct {
	Session
	OwnerNick string
}

func getChannelDBSessions(network, channel string, limit int) ([]SessionWithUser, error) {
	var results []SessionWithUser
	err := theDB.Model(&Session{}).
		Select("sessions.*, users.current_nick as owner_nick").
		Joins("JOIN users ON users.id = sessions.user_id").
		Where("sessions.network = ? AND sessions.channel = ? AND sessions.deleted_at IS NULL", network, channel).
		Order("sessions.last_active DESC").
		Limit(limit).
		Find(&results).Error
	return results, err
}

func sessionHasIncompleteToolCalls(sessionID int64) (bool, error) {
	messages, err := loadDBSessionMessages(sessionID)
	if err != nil {
		return false, err
	}

	toolResponses := make(map[string]struct{})
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID != nil {
			toolResponses[*m.ToolCallID] = struct{}{}
		}
	}

	for _, m := range messages {
		if m.Role == "assistant" && m.ToolCalls != nil {
			var calls []struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal([]byte(*m.ToolCalls), &calls); err != nil {
				continue
			}
			for _, tc := range calls {
				if _, ok := toolResponses[tc.ID]; !ok {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func cloneDBSession(sourceSessionID int64, targetNetwork, targetChannel string, targetUserID int64) (int64, error) {
	tx := theDB.Begin()
	if tx.Error != nil {
		return 0, tx.Error
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	var source Session
	if err := tx.Where("id = ?", sourceSessionID).First(&source).Error; err != nil {
		return 0, fmt.Errorf("load source session: %w", err)
	}

	var newSettingsID *int64
	if source.SettingsID != nil {
		var srcSettings SessionSetting
		if err := tx.Where("id = ?", *source.SettingsID).First(&srcSettings).Error; err != nil {
			return 0, fmt.Errorf("load source settings: %w", err)
		}
		srcSettings.ID = 0
		srcSettings.CreatedAt = time.Time{}
		if err := tx.Create(&srcSettings).Error; err != nil {
			return 0, fmt.Errorf("copy settings: %w", err)
		}
		newSettingsID = &srcSettings.ID
	}

	if err := tx.Model(&Session{}).
		Where("network = ? AND channel = ? AND user_id = ? AND status = ?",
			targetNetwork, targetChannel, targetUserID, StatusActive).
		Update("status", StatusCompleted).Error; err != nil {
		return 0, fmt.Errorf("complete existing active session: %w", err)
	}

	convID := generateConvID()
	newSession := Session{
		Network:     targetNetwork,
		Channel:     targetChannel,
		UserID:      &targetUserID,
		ChatCommand: source.ChatCommand,
		ConvID:      &convID,
		ResponseID:  nil,
		Service:     source.Service,
		Model:       source.Model,
		Status:      StatusActive,
		SettingsID:  newSettingsID,
	}
	if err := tx.Create(&newSession).Error; err != nil {
		return 0, fmt.Errorf("create new session: %w", err)
	}

	var sourceMessages []Message
	if err := tx.Where("session_id = ? AND archived = ? AND superseded = ?", sourceSessionID, false, false).
		Order("id ASC").Find(&sourceMessages).Error; err != nil {
		return 0, fmt.Errorf("load source messages: %w", err)
	}

	skippedSystem := false
	var firstUserContent string
	for _, m := range sourceMessages {
		if !skippedSystem && m.Role == "system" {
			skippedSystem = true
			continue
		}
		if firstUserContent == "" && m.Role == "user" {
			firstUserContent = m.Content
		}
		newMsg := Message{
			SessionID:          newSession.ID,
			Role:               m.Role,
			Content:            m.Content,
			ToolCalls:          m.ToolCalls,
			ToolCallID:         m.ToolCallID,
			ReasoningContent:   m.ReasoningContent,
			EncryptedReasoning: m.EncryptedReasoning,
			MultiContent:       m.MultiContent,
			IsAsyncResult:      m.IsAsyncResult,
			Archived:           false,
			CompactionID:       nil,
			SourceCompactionID: nil,
			Superseded:         false,
		}
		if err := tx.Create(&newMsg).Error; err != nil {
			return 0, fmt.Errorf("copy message: %w", err)
		}
	}

	if firstUserContent != "" {
		preview := strings.ReplaceAll(firstUserContent, "\n", " ")
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		if err := tx.Model(&Session{}).Where("id = ? AND first_message = ''", newSession.ID).
			Update("first_message", preview).Error; err != nil {
			return 0, fmt.Errorf("set first_message: %w", err)
		}
	}

	if err := tx.Commit().Error; err != nil {
		return 0, err
	}
	committed = true
	return newSession.ID, nil
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
