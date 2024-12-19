package main

import (
	"sync"

	gogpt "github.com/sashabaranov/go-openai"
)

type ChatContext struct {
	Messages []gogpt.ChatCompletionMessage
	Config   AIConfig
}

var chatContextsMutex sync.Mutex
var chatContexts map[string]ChatContext

func init() {
	chatContexts = make(map[string]ChatContext)
}

func ClearContext(key string) {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()
}

// TODO check if we need to limit size and how should that be handled? maybe the api will just give an error
func AddContext(config AIConfig, key string, message gogpt.ChatCompletionMessage) []gogpt.ChatCompletionMessage {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()
	context := chatContexts[key]
	context.Config = config
	context.Messages = append(context.Messages, message)
	chatContexts[key] = context
	return chatContexts[key].Messages
}

func GetContext(key string) ChatContext {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()
	return chatContexts[key]
}

func ContextExists(key string) bool {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()
	_, ok := chatContexts[key]
	return ok
}
