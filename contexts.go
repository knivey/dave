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
	chatContexts[key] = ChatContext{}
}

func AddContext(config AIConfig, key string, message gogpt.ChatCompletionMessage) []gogpt.ChatCompletionMessage {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()
	context := chatContexts[key]
	context.Config = config
	context.Messages = append(context.Messages, message)
	if len(context.Messages) > config.MaxHistory+1 { // add one so we dont count system prompt
		//keep the system prompt
		newMsgs := []gogpt.ChatCompletionMessage{context.Messages[0]}
		context.Messages = append(newMsgs, context.Messages[len(context.Messages)-config.MaxHistory:]...)
	}
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
	ctx, ok := chatContexts[key]
	return ok && len(ctx.Messages) > 0
}
