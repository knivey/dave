package main

import (
	"encoding/json"
	"sync"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	gogpt "github.com/sashabaranov/go-openai"
)

var loggerCS = logxi.New("contextStore")

var contextStoreMutex sync.Mutex
var contextLastActive map[string]int64

func init() {
	loggerCS.SetLevel(logxi.LevelAll)
	contextLastActive = make(map[string]int64)
}

func LoadContextStore() {
	if theDB == nil {
		return
	}

	if affected, err := completeDBOrphanedSessions(); err != nil {
		loggerCS.Error("Failed to cleanup orphaned sessions", "error", err)
	} else if affected > 0 {
		loggerCS.Info("Completed orphaned sessions", "count", affected)
	}

	if affected, err := reactivateDBStrandedSessions(); err != nil {
		loggerCS.Error("Failed to reactivate stranded sessions", "error", err)
	} else if affected > 0 {
		loggerCS.Info("Reactivated stranded sessions", "count", affected)
	}

	sessions, err := loadActiveDBSessions()
	if err != nil {
		loggerCS.Error("Failed to load active sessions", "error", err)
		return
	}

	loggerCS.Info("Found active sessions", "count", len(sessions))

	chatContextsMutex.Lock()
	for _, s := range sessions {
		currentCfg, ok := config.Commands.Chats[s.ChatCommand]
		if !ok {
			loggerCS.Warn("completing session for unknown chat command", "key", s.ContextKey, "command", s.ChatCommand)
			completeDBSession(s.ID)
			delete(contextLastActive, s.ContextKey)
			continue
		}

		dbMsgs, err := loadDBSessionMessages(s.ID)
		if err != nil {
			loggerCS.Error("Failed to load messages for session", "id", s.ID, "error", err)
			continue
		}

		var messages []gogpt.ChatCompletionMessage
		for _, dm := range dbMsgs {
			msg := gogpt.ChatCompletionMessage{
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
				var toolCalls []gogpt.ToolCall
				if err := json.Unmarshal([]byte(*dm.ToolCalls), &toolCalls); err == nil {
					msg.ToolCalls = toolCalls
				}
			}
			messages = append(messages, msg)
		}

		if len(messages) > 0 {
			messages = TruncateHistory(messages, currentCfg.MaxHistory)
			chatContextsMap[s.ContextKey] = ChatContext{
				Messages:  messages,
				Config:    currentCfg,
				SessionID: s.ID,
				ConvID: func() string {
					if s.ConvID != nil {
						return *s.ConvID
					}
					return ""
				}(),
				ResponseID: func() string {
					if s.ResponseID != nil {
						return *s.ResponseID
					}
					return ""
				}(),
			}
			contextLastActive[s.ContextKey] = time.Now().Unix()
			loggerCS.Info("Loaded session", "id", s.ID, "key", s.ContextKey, "command", s.ChatCommand, "messages", len(messages))
		} else {
			loggerCS.Warn("skipping session with no messages", "id", s.ID, "key", s.ContextKey)
		}
	}
	chatContextsMutex.Unlock()
}

func SaveContextStore() {
}

func CleanupContexts() {
	if theDB == nil {
		return
	}

	affected, err := cleanupDBSessions(config.Database.MaxAgeDays)
	if err != nil {
		loggerCS.Error("Failed to cleanup sessions", "error", err)
		return
	}
	if affected > 0 {
		loggerCS.Info("Cleaned up old sessions", "count", affected)
	}
}

func MarkContextsDirty() {
}

func StartSaveTimer() {
}

func StopPendingSave() {
}
