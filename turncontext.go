package main

type turnContext struct {
	sessionID int64
	messages  []ChatMessage
}

var loggerTC = newLogger("turnContext")

func newTurnContext(sessionID int64, initial []ChatMessage) *turnContext {
	return &turnContext{
		sessionID: sessionID,
		messages:  initial,
	}
}

func (tc *turnContext) Add(msg ChatMessage) {
	tc.messages = append(tc.messages, msg)
	if err := sessionMgr.AddMessage(tc.sessionID, msg); err != nil {
		loggerTC.Error("Failed to add message", "session", tc.sessionID, "error", err)
	}
}

func (tc *turnContext) Messages() []ChatMessage {
	return tc.messages
}

func (tc *turnContext) LastN(n int) []ChatMessage {
	if n <= 0 {
		return nil
	}
	if n >= len(tc.messages) {
		return tc.messages
	}
	return tc.messages[len(tc.messages)-n:]
}
