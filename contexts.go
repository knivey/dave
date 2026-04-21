package main

import (
	"encoding/json"
	"time"

	gogpt "github.com/sashabaranov/go-openai"
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

func AddContext(config AIConfig, key string, message gogpt.ChatCompletionMessage, network, channel, nick string) []gogpt.ChatCompletionMessage {
	msgs := chatContexts.Add(key, config, message)
	SetContextLastActive(key)

	if theDB != nil {
		chatContextsMutex.Lock()
		ctx := chatContextsMap[key]
		sid := ctx.SessionID
		chatContextsMutex.Unlock()

		if sid == 0 {
			var err error
			sid, err = createDBSession(key, network, channel, nick, config.Name)
			if err != nil {
				loggerCS.Error("Failed to create session", "error", err)
			} else {
				chatContextsMutex.Lock()
				if c, ok := chatContextsMap[key]; ok {
					c.SessionID = sid
					chatContextsMap[key] = c
				}
				chatContextsMutex.Unlock()
			}
		}

		if sid != 0 {
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
