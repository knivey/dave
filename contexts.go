package main

import (
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
	chatContexts.Clear(key)
	DeleteContextLastActive(key)
	MarkContextsDirty()
}

func AddContext(config AIConfig, key string, message gogpt.ChatCompletionMessage) []gogpt.ChatCompletionMessage {
	msgs := chatContexts.Add(key, config, message)
	SetContextLastActive(key)
	MarkContextsDirty()
	return msgs
}

func GetContext(key string) ChatContext {
	return chatContexts.Get(key)
}

func ContextExists(key string) bool {
	return chatContexts.Exists(key)
}
