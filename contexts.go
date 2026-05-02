package main

import (
	"encoding/json"
	"time"
)

func GetContextLastActive(key string) int64 {
	return contextLastActive[key]
}

func SetContextLastActive(key string) {
	contextLastActive[key] = time.Now().Unix()
}

func DeleteContextLastActive(key string) {
	delete(contextLastActive, key)
}

func ClearContext(key string) {
	if theDB != nil {
		chatContextsMutex.Lock()
		ctx := chatContextsMap[key]
		chatContextsMutex.Unlock()
		if ctx.SessionID != 0 {
			if err := completeDBSession(ctx.SessionID); err != nil {
				loggerCS.Error("Failed to complete session", "id", ctx.SessionID, "error", err)
			}
		}
	}
	chatContexts.Clear(key)
	DeleteContextLastActive(key)
}

func AddContext(config AIConfig, key string, message ChatMessage, network, channel, nick string) []ChatMessage {
	msgs := chatContexts.Add(key, config, message)
	SetContextLastActive(key)

	if theDB != nil {
		chatContextsMutex.Lock()
		ctx := chatContextsMap[key]
		sid := ctx.SessionID
		chatContextsMutex.Unlock()

		if sid == 0 {
			var err error
			sid, err = createDBSession(key, network, channel, nick, config.Name, ctx.ConvID, config.Service, config.Model)
			if err != nil {
				loggerCS.Error("Failed to create session", "error", err)
			} else {
				chatContextsMutex.Lock()
				if c, ok := chatContextsMap[key]; ok {
					c.SessionID = sid
					chatContextsMap[key] = c
				}
				chatContextsMutex.Unlock()
				apiLogger.RestoreSession(sid, key)
			}
		} else if ctx.ConvID != "" {
			if err := updateDBSessionConvID(sid, ctx.ConvID); err != nil {
				loggerCS.Error("Failed to update conv_id", "session", sid, "error", err)
			}
		}

		if sid != 0 {
			if message.Role == "user" {
				if err := updateDBSessionFirstMessage(sid, message.Content); err != nil {
					loggerCS.Error("Failed to update first message", "session", sid, "error", err)
				}
			}

			var toolCallsJSON *string
			if len(message.ToolCalls) > 0 {
				if tcData, err := json.Marshal(message.ToolCalls); err == nil {
					s := string(tcData)
					toolCallsJSON = &s
				}
			}
			var toolCallID *string
			if message.ToolCallID != "" {
				toolCallID = &message.ToolCallID
			}
			var reasoningContent *string
			if message.ReasoningContent != "" {
				reasoningContent = &message.ReasoningContent
			}
			if err := insertDBMessage(sid, message.Role, message.Content, toolCallsJSON, toolCallID, reasoningContent); err != nil {
				loggerCS.Error("Failed to insert message", "session", sid, "error", err)
			}
		}
	}

	return msgs
}

func GetContext(key string) ChatContext {
	return chatContexts.Get(key)
}

func ContextExists(key string) bool {
	return chatContexts.Exists(key)
}

func SetContextResponseID(key, responseID string) {
	chatContextsMutex.Lock()
	ctx, ok := chatContextsMap[key]
	if !ok {
		chatContextsMutex.Unlock()
		return
	}
	ctx.ResponseID = responseID
	chatContextsMap[key] = ctx
	sid := ctx.SessionID
	chatContextsMutex.Unlock()
	if theDB != nil && sid != 0 {
		var rid *string
		if responseID != "" {
			rid = &responseID
		}
		if err := updateDBSessionResponseID(sid, rid); err != nil {
			loggerCS.Error("Failed to update response_id", "session", sid, "error", err)
		}
	}
}
