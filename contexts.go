package main

func ClearContext(network, channel, nick string) {
	if theDB == nil {
		return
	}
	session, err := sessionMgr.GetActiveSession(network, channel, nick)
	if err != nil {
		loggerSM.Error("Failed to get active session for clear", "error", err)
		return
	}
	if session != nil {
		if err := sessionMgr.CompleteSession(session.ID); err != nil {
			loggerSM.Error("Failed to complete session", "id", session.ID, "error", err)
		}
	}
}

func ContextExists(network, channel, nick string) bool {
	if theDB == nil {
		return false
	}
	return sessionMgr.ContextExists(network, channel, nick)
}

func SetContextResponseID(network, channel, nick, responseID string) {
	if theDB == nil {
		return
	}
	sessionMgr.SetResponseIDForActive(network, channel, nick, responseID)
}
