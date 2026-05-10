package main

func ClearContext(network, channel string, userID int64) {
	if theDB == nil {
		return
	}
	session, err := sessionMgr.GetActiveSession(network, channel, userID)
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

func ContextExists(network, channel string, userID int64) bool {
	if theDB == nil {
		return false
	}
	return sessionMgr.ContextExists(network, channel, userID)
}

func SetContextResponseID(network, channel string, userID int64, responseID string) {
	if theDB == nil {
		return
	}
	sessionMgr.SetResponseIDForActive(network, channel, userID, responseID)
}
