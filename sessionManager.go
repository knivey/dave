package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"

	logxi "github.com/mgutz/logxi/v1"
	"gorm.io/gorm"
)

var sessionMgr *SessionManager
var loggerSM = logxi.New("sessionManager")

type SessionManager struct {
	db *gorm.DB
}

func NewSessionManager(db *gorm.DB) *SessionManager {
	return &SessionManager{db: db}
}

func (sm *SessionManager) GetActiveSession(network, channel, nick string) (*Session, error) {
	var session Session
	err := sm.db.Where("network = ? AND channel = ? AND nick = ? AND status = ?",
		network, channel, nick, "active").First(&session).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &session, nil
}

func (sm *SessionManager) ContextExists(network, channel, nick string) bool {
	session, err := sm.GetActiveSession(network, channel, nick)
	return err == nil && session != nil
}

func (sm *SessionManager) CreateSession(network, channel, nick, chatCommand, service, model string) (int64, error) {
	if err := sm.db.Model(&Session{}).
		Where("network = ? AND channel = ? AND nick = ? AND status = ?",
			network, channel, nick, "active").
		Update("status", "completed").Error; err != nil {
		loggerSM.Warn("failed to complete previous active sessions", "network", network, "channel", channel, "nick", nick, "error", err)
	}

	convID := generateConvID()
	session := Session{
		Network:     network,
		Channel:     channel,
		Nick:        nick,
		ChatCommand: chatCommand,
		ConvID:      &convID,
		Service:     service,
		Model:       model,
		Status:      "active",
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

	if err := insertDBMessage(sessionID, msg.Role, msg.Content, toolCallsJSON, toolCallID, reasoningContent); err != nil {
		return err
	}

	if msg.Role == "user" {
		if err := updateDBSessionFirstMessage(sessionID, msg.Content); err != nil {
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
		messages = append(messages, msg)
	}

	return TruncateHistory(messages, maxHistory), nil
}

func (sm *SessionManager) CompleteSession(sessionID int64) error {
	_, file, line, _ := runtime.Caller(1)
	loggerSM.Info("completing session", "id", sessionID, "caller", fmt.Sprintf("%s:%d", file, line))
	return sm.db.Model(&Session{}).Where("id = ?", sessionID).
		Update("status", "completed").Error
}

func (sm *SessionManager) ActivateSession(sessionID int64) error {
	return sm.db.Model(&Session{}).Where("id = ?", sessionID).
		Update("status", "active").Error
}

func (sm *SessionManager) SwitchActive(network, channel, nick string, newSessionID int64) (int64, error) {
	var oldID int64

	var currentActive []Session
	sm.db.Where("network = ? AND channel = ? AND nick = ? AND status = ?",
		network, channel, nick, "active").Find(&currentActive)

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
	sm.db.Model(&Session{}).Where("id = ? AND status = ?", sessionID, "active").Count(&count)
	return count > 0
}

func (sm *SessionManager) UpdateResponseID(sessionID int64, responseID *string) error {
	return updateDBSessionResponseID(sessionID, responseID)
}

func (sm *SessionManager) GetSession(id int64) (*Session, error) {
	return getDBSessionByID(id)
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

func (sm *SessionManager) SetResponseIDForActive(network, channel, nick, responseID string) {
	session, err := sm.GetActiveSession(network, channel, nick)
	if err != nil || session == nil {
		return
	}
	var rid *string
	if responseID != "" {
		rid = &responseID
	}
	if err := sm.UpdateResponseID(session.ID, rid); err != nil {
		loggerSM.Error("Failed to update response_id", "session", session.ID, "error", err)
	}
}
