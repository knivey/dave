package main

var loggerCS = newLogger("contextStore")

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

func LoadContextStore() {
	if theDB == nil {
		return
	}

	if affected, err := sessionMgr.CleanupOrphaned(); err != nil {
		loggerCS.Error("Failed to cleanup orphaned sessions", "error", err)
	} else if affected > 0 {
		loggerCS.Info("Completed orphaned sessions", "count", affected)
	}

	if affected, err := sessionMgr.ReactivateStranded(); err != nil {
		loggerCS.Error("Failed to reactivate stranded sessions", "error", err)
	} else if affected > 0 {
		loggerCS.Info("Reactivated stranded sessions", "count", affected)
	}
}

func CleanupContexts() {
	if theDB == nil {
		return
	}

	affected, err := sessionMgr.CleanupByAge(config.Database.MaxAgeDays)
	if err != nil {
		loggerCS.Error("Failed to cleanup sessions", "error", err)
		return
	}
	if affected > 0 {
		loggerCS.Info("Cleaned up old sessions", "count", affected)
	}
}
