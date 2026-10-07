package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"

	"gorm.io/gorm"
)

var sessionMgr *SessionManager
var loggerSM = newLogger("sessionManager")

// CRITICAL DESIGN NOTE: Per-user session creation lock.
//
// Every -command MUST create its own session. When two commands from the same
// user arrive within milliseconds, their chat() goroutines run concurrently in
// the queue. Without this lock, the second goroutine's GetActiveSession() finds
// the session just created by the first goroutine and reuses it, merging both
// conversations into one session. This regression happened once already during
// the removal of in-memory contexts and MUST NOT happen again. DO NOT REMOVE.
var sessionCreationMu sync.Map // key: "network\x00channel\x00<userID>" → *sync.Mutex

func sessionCreationKey(network, channel string, userID int64) string {
	return fmt.Sprintf("%s\x00%s\x00%d", network, channel, userID)
}

func getSessionCreationLock(network, channel string, userID int64) *sync.Mutex {
	key := sessionCreationKey(network, channel, userID)
	v, _ := sessionCreationMu.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

type SessionManager struct {
	db *gorm.DB
}

func NewSessionManager(db *gorm.DB) *SessionManager {
	return &SessionManager{db: db}
}

func (sm *SessionManager) GetActiveSession(network, channel string, userID int64) (*Session, error) {
	var session Session
	err := sm.db.Where("network = ? AND channel = ? AND user_id = ? AND status = ?",
		network, channel, userID, StatusActive).First(&session).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &session, nil
}

func (sm *SessionManager) ContextExists(network, channel string, userID int64) bool {
	session, err := sm.GetActiveSession(network, channel, userID)
	return err == nil && session != nil
}

func (sm *SessionManager) CreateSession(network, channel string, userID int64, chatCommand, service, model string) (int64, error) {
	if err := sm.db.Model(&Session{}).
		Where("network = ? AND channel = ? AND user_id = ? AND status = ?",
			network, channel, userID, StatusActive).
		Update("status", StatusCompleted).Error; err != nil {
		loggerSM.Warn("failed to complete previous active sessions", "network", network, "channel", channel, "user_id", userID, "error", err)
	}

	convID := generateConvID()
	session := Session{
		Network:     network,
		Channel:     channel,
		UserID:      &userID,
		ChatCommand: chatCommand,
		ConvID:      &convID,
		Service:     service,
		Model:       model,
		Status:      StatusActive,
	}
	if err := sm.db.Create(&session).Error; err != nil {
		return 0, err
	}
	return session.ID, nil
}

func (sm *SessionManager) AddMessage(sessionID int64, msg ChatMessage) error {
	var toolCallsJSON *string
	if len(msg.ToolCalls) > 0 {
		if tcData, err := json.Marshal(msg.ToolCalls); err == nil {
			s := string(tcData)
			toolCallsJSON = &s
		}
	}
	var toolCallID *string
	if msg.ToolCallID != "" {
		toolCallID = &msg.ToolCallID
	}
	var reasoningContent *string
	if msg.ReasoningContent != "" {
		reasoningContent = &msg.ReasoningContent
	}
	var multiContentJSON *string
	if len(msg.MultiContent) > 0 {
		if mcData, err := json.Marshal(msg.MultiContent); err == nil {
			s := string(mcData)
			multiContentJSON = &s
		}
	}

	if err := insertDBMessage(sessionID, msg.Role, msg.Content, toolCallsJSON, toolCallID, reasoningContent, multiContentJSON); err != nil {
		return err
	}

	if msg.Role == "user" {
		if err := updateDBSessionFirstMessage(sessionID, textContentFromMessage(msg)); err != nil {
			loggerSM.Error("Failed to update first message", "session", sessionID, "error", err)
		}
	}

	return nil
}

func (sm *SessionManager) GetMessages(sessionID int64, maxHistory int) ([]ChatMessage, error) {
	dbMsgs, err := loadDBSessionMessages(sessionID)
	if err != nil {
		return nil, err
	}

	var messages []ChatMessage
	for _, dm := range dbMsgs {
		messages = append(messages, messageFromDB(dm))
	}

	return TruncateHistory(messages, maxHistory), nil
}

func (sm *SessionManager) CompleteSession(sessionID int64) error {
	_, file, line, _ := runtime.Caller(1)
	loggerSM.Info("completing session", "id", sessionID, "caller", fmt.Sprintf("%s:%d", file, line))
	return sm.db.Model(&Session{}).Where("id = ?", sessionID).
		Update("status", StatusCompleted).Error
}

func (sm *SessionManager) ActivateSession(sessionID int64) error {
	return sm.db.Model(&Session{}).Where("id = ?", sessionID).
		Update("status", StatusActive).Error
}

func (sm *SessionManager) SwitchActive(network, channel string, userID int64, newSessionID int64) (int64, error) {
	var oldID int64

	var currentActive []Session
	if err := sm.db.Where("network = ? AND channel = ? AND user_id = ? AND status = ?",
		network, channel, userID, StatusActive).Find(&currentActive).Error; err != nil {
		return 0, fmt.Errorf("querying active sessions: %w", err)
	}

	if len(currentActive) > 0 {
		oldID = currentActive[0].ID
	}

	for _, s := range currentActive {
		if s.ID != newSessionID {
			if err := sm.CompleteSession(s.ID); err != nil {
				return oldID, err
			}
		}
	}

	if err := sm.ActivateSession(newSessionID); err != nil {
		return oldID, err
	}

	return oldID, nil
}

func (sm *SessionManager) IsSessionActive(sessionID int64) bool {
	var count int64
	sm.db.Model(&Session{}).Where("id = ? AND status = ?", sessionID, StatusActive).Count(&count)
	return count > 0
}

func (sm *SessionManager) UpdateResponseID(sessionID int64, responseID *string, model string) error {
	return updateDBSessionResponseID(sessionID, responseID, model)
}

func (sm *SessionManager) GetSession(id int64) (*Session, error) {
	return getDBSessionByID(id)
}

// CreateSessionSettings records the creation-time config snapshot for the
// session (what it started with) and updates the session's settings_id
// foreign key. Provenance only — stored settings are never applied to live
// turns; the live config always wins (see
// docs/superpowers/specs/2026-10-06-live-config-and-usage-attribution-design.md).
// Returns the settings row ID.
func (sm *SessionManager) CreateSessionSettings(sessionID int64, cfg AIConfig) (int64, error) {
	setting := SessionSetting{
		System:           cfg.System,
		Model:            cfg.Model,
		DetectImages:     cfg.DetectImages,
		MaxImages:        cfg.MaxImages,
		MaxContextImages: cfg.MaxContextImages,
		ReasoningEffort:  cfg.ReasoningEffort,
		ReasoningSummary: cfg.ReasoningSummary,
	}
	if err := sm.db.Create(&setting).Error; err != nil {
		return 0, fmt.Errorf("creating session settings: %w", err)
	}
	if err := sm.db.Model(&Session{}).Where("id = ?", sessionID).
		Update("settings_id", setting.ID).Error; err != nil {
		return 0, fmt.Errorf("updating session settings_id: %w", err)
	}
	return setting.ID, nil
}

func (sm *SessionManager) DeleteSession(id int64) error {
	return deleteDBSession(id)
}

func (sm *SessionManager) CleanupOrphaned() (int64, error) {
	return completeDBOrphanedSessions()
}

func (sm *SessionManager) ReactivateStranded() (int64, error) {
	return reactivateDBStrandedSessions()
}

func (sm *SessionManager) CleanupByAge(maxAgeDays int) (int64, error) {
	return cleanupDBSessions(maxAgeDays)
}

func messageFromDB(dm Message) ChatMessage {
	msg := ChatMessage{
		Role:    dm.Role,
		Content: dm.Content,
	}
	if dm.ToolCallID != nil {
		msg.ToolCallID = *dm.ToolCallID
	}
	if dm.ReasoningContent != nil {
		msg.ReasoningContent = *dm.ReasoningContent
	}
	if dm.ToolCalls != nil {
		var toolCalls []ToolCall
		if err := json.Unmarshal([]byte(*dm.ToolCalls), &toolCalls); err == nil {
			msg.ToolCalls = toolCalls
		}
	}
	if dm.MultiContent != nil {
		var parts []MessagePart
		if err := json.Unmarshal([]byte(*dm.MultiContent), &parts); err == nil {
			msg.MultiContent = parts
		}
	}
	return msg
}

func textContentFromMessage(msg ChatMessage) string {
	if msg.Content != "" {
		return msg.Content
	}
	for _, part := range msg.MultiContent {
		if part.Type == PartTypeText && part.Text != "" {
			return part.Text
		}
	}
	return ""
}
