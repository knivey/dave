package main

import (
	"time"

	"gorm.io/gorm"
)

// Read-only mirrors of dave's tables. sesssite never writes and never
// AutoMigrates outside tests. Column sets are the subset the site reads;
// column names mirror dave's structs in db.go exactly — dave owns the
// schema, these models must never drift from it. Explicit TableName()
// methods are mandatory: GORM's default pluralization would name
// Session's table "sessions" by luck but nothing else is guaranteed
// (turn_usage, compactions).
type Session struct {
	ID           int64 `gorm:"primaryKey"`
	Slug         string
	Network      string
	Channel      string
	ChatCommand  string `gorm:"column:chat_command"`
	FirstMessage string `gorm:"column:first_message"`
	Service      string
	Model        string
	Status       string
	CreatedAt    time.Time
	LastActive   time.Time `gorm:"column:last_active"`
	// DeletedAt gives every query GORM's soft-delete filter for free:
	// soft-deleted sessions stay invisible on the site (spec, "Error
	// handling & edges").
	DeletedAt gorm.DeletedAt
	UserID    *int64
	// Clone lineage (dave's ^clone): the source session id and the
	// source owner's nick AT CLONE TIME — displayed as a cloned-from
	// badge on the session page and user-page rows.
	ClonedFromID   *int64 `gorm:"column:cloned_from_id"`
	ClonedFromNick string `gorm:"column:cloned_from_nick"`
}

func (Session) TableName() string { return "sessions" }

type User struct {
	ID          int64 `gorm:"primaryKey"`
	Slug        string
	Network     string
	CurrentNick string
	// IRCAccount maps to the legacy `account` column (dave's User struct
	// tags it the same way).
	IRCAccount string `gorm:"column:account"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (User) TableName() string { return "users" }

type Message struct {
	ID               int64 `gorm:"primaryKey"`
	SessionID        int64 `gorm:"column:session_id"`
	Role             string
	Content          string
	ToolCalls        *string `gorm:"column:tool_calls"`
	ToolCallID       *string `gorm:"column:tool_call_id"`
	ReasoningContent *string `gorm:"column:reasoning_content"`
	ReasoningSummary *string `gorm:"column:reasoning_summary"`
	MultiContent     *string `gorm:"column:multi_content"`
	Archived         bool    `gorm:"column:archived"`
	CompactionID     *int64  `gorm:"column:compaction_id"`
	// SourceCompactionID set ⇒ compaction tail-copy: content duplicates
	// an archived original; excluded from the flat transcript.
	SourceCompactionID *int64 `gorm:"column:source_compaction_id"`
	Superseded         bool   `gorm:"column:superseded"`
	// UserID attributes the human author of user-role rows (dave's
	// per-message attribution, migration #11) — on cloned sessions the
	// ORIGINAL author's id, copied verbatim through clone chains.
	UserID    *int64 `gorm:"column:user_id"`
	CreatedAt time.Time
}

func (Message) TableName() string { return "messages" }

type TurnUsage struct {
	ID               int64 `gorm:"primaryKey"`
	SessionID        int64 `gorm:"column:session_id"`
	Model            string
	Service          string
	ReasoningEffort  string `gorm:"column:reasoning_effort"`
	PromptTokens     int    `gorm:"column:prompt_tokens"`
	CompletionTokens int    `gorm:"column:completion_tokens"`
	CachedTokens     int    `gorm:"column:cached_tokens"`
	ReasoningTokens  int    `gorm:"column:reasoning_tokens"`
	FinishReason     string `gorm:"column:finish_reason"`
	APIPath          string `gorm:"column:api_path"`
	DurationMs       int    `gorm:"column:duration_ms"`
	CreatedAt        time.Time
}

func (TurnUsage) TableName() string { return "turn_usage" }

type Compaction struct {
	ID               int64 `gorm:"primaryKey"`
	SessionID        int64 `gorm:"column:session_id"`
	SummaryMessageID int64 `gorm:"column:summary_message_id"`
	FirstArchivedID  int64 `gorm:"column:first_archived_id"`
	LastArchivedID   int64 `gorm:"column:last_archived_id"`
	ArchivedCount    int64 `gorm:"column:archived_count"`
	Service          string
	Model            string
	Trigger          string
	PromptTokens     int `gorm:"column:prompt_tokens"`
	CompletionTokens int `gorm:"column:completion_tokens"`
	DurationMs       int `gorm:"column:duration_ms"`
	CreatedAt        time.Time
}

func (Compaction) TableName() string { return "compactions" }
