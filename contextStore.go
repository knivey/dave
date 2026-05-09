package main

import (
	logxi "github.com/mgutz/logxi/v1"
)

var loggerCS = logxi.New("contextStore")

func init() {
	loggerCS.SetLevel(logxi.LevelAll)
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
