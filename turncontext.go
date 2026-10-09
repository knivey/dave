package main

type turnContext struct {
	sessionID int64
	messages  []ChatMessage
	// ephemeral marks a stateless (generator) turn: Add accumulates in
	// memory only and never touches sessionMgr. Spec §Ephemeral Turn
	// Machinery — this is the single persistence seam runTurn flows through.
	ephemeral bool
}

var loggerTC = newLogger("turnContext")

func newTurnContext(sessionID int64, initial []ChatMessage) *turnContext {
	return &turnContext{
		sessionID: sessionID,
		messages:  initial,
	}
}

// newEphemeralTurnContext builds a non-persisted turn seeded with initial
// messages (system + user for generators).
func newEphemeralTurnContext(initial []ChatMessage) *turnContext {
	return &turnContext{messages: initial, ephemeral: true}
}

func (tc *turnContext) Add(msg ChatMessage) {
	tc.messages = append(tc.messages, msg)
	if tc.ephemeral || sessionMgr == nil {
		return
	}
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
