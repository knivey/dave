package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	gogpt "github.com/sashabaranov/go-openai"
	"golang.org/x/time/rate"
)

type ChatContext struct {
	Messages  []gogpt.ChatCompletionMessage
	Config    AIConfig
	SessionID int64
	ConvID    string
}

type ChatContextStore interface {
	Add(key string, config AIConfig, message gogpt.ChatCompletionMessage) []gogpt.ChatCompletionMessage
	Get(key string) ChatContext
	Clear(key string)
	Exists(key string) bool
}

var chatContextsMutex sync.Mutex
var chatContextsMap map[string]ChatContext

func init() {
	chatContextsMap = make(map[string]ChatContext)
}

var _ ChatContextStore = (*globalContextStore)(nil)

type globalContextStore struct{}

func (s *globalContextStore) Add(key string, config AIConfig, message gogpt.ChatCompletionMessage) []gogpt.ChatCompletionMessage {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()
	context := chatContextsMap[key]
	context.Config = config
	if context.ConvID == "" {
		context.ConvID = generateConvID()
	}
	context.Messages = append(context.Messages, message)
	if len(context.Messages) > config.MaxHistory+1 {
		newMsgs := []gogpt.ChatCompletionMessage{context.Messages[0]}
		context.Messages = append(newMsgs, context.Messages[len(context.Messages)-config.MaxHistory:]...)
	}
	chatContextsMap[key] = context
	return chatContextsMap[key].Messages
}

func (s *globalContextStore) Get(key string) ChatContext {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()
	return chatContextsMap[key]
}

func (s *globalContextStore) Clear(key string) {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()
	chatContextsMap[key] = ChatContext{}
}

func (s *globalContextStore) Exists(key string) bool {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()
	ctx, ok := chatContextsMap[key]
	return ok && len(ctx.Messages) > 0
}

var chatContexts ChatContextStore = &globalContextStore{}

func TruncateHistory(msgs []gogpt.ChatCompletionMessage, maxHistory int) []gogpt.ChatCompletionMessage {
	if len(msgs) > maxHistory+1 {
		newMsgs := []gogpt.ChatCompletionMessage{msgs[0]}
		return append(newMsgs, msgs[len(msgs)-maxHistory:]...)
	}
	return msgs
}

func generateConvID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

type RateLimiter interface {
	Allow(networkName, key string) bool
}

var rateLimiter RateLimiter = &globalRateLimiter{}

type globalRateLimiter struct{}

func (r *globalRateLimiter) Allow(networkName, key string) bool {
	rateMutex.Lock()
	defer rateMutex.Unlock()

	rateKey := networkName + key
	if entry, ok := rateLimits[rateKey]; ok {
		entry.lastUsed = time.Now()
		return entry.limiter.Allow()
	}
	rateLimits[rateKey] = &rateEntry{
		limiter:  rate.NewLimiter(rate.Every(time.Second), 2),
		lastUsed: time.Now(),
	}
	return rateLimits[rateKey].limiter.Allow()
}
