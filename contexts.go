package main

import (
	gogpt "github.com/sashabaranov/go-openai"
)

func ClearContext(key string) {
	chatContexts.Clear(key)
}

func AddContext(config AIConfig, key string, message gogpt.ChatCompletionMessage) []gogpt.ChatCompletionMessage {
	return chatContexts.Add(key, config, message)
}

func GetContext(key string) ChatContext {
	return chatContexts.Get(key)
}

func ContextExists(key string) bool {
	return chatContexts.Exists(key)
}
