package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var ensureTestUserMu sync.Mutex

func ensureTestUser(t *testing.T, network, nick string) int64 {
	t.Helper()
	ensureTestUserMu.Lock()
	defer ensureTestUserMu.Unlock()
	var user User
	err := theDB.Where("network = ? AND normalized_nick = ?", network, normalizeIRC(nick, "rfc1459")).First(&user).Error
	if err == nil {
		return user.ID
	}
	user = User{
		Network:        network,
		CurrentNick:    nick,
		NormalizedNick: normalizeIRC(nick, "rfc1459"),
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}
	require.NoError(t, theDB.Create(&user).Error, "create test user")
	return user.ID
}

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := initDB(DatabaseConfig{Path: dbPath, MaxAgeDays: 90}, newLogger("test"))
	require.NoError(t, err, "failed to init test db")
	oldDB := theDB
	oldSM := sessionMgr
	theDB = db
	sessionMgr = NewSessionManager(db)
	t.Cleanup(func() {
		sessionMgr = oldSM
		theDB = oldDB
		sqlDB, _ := db.DB()
		if sqlDB != nil {
			sqlDB.Close()
		}
	})
	return db
}

func createTestSession(t *testing.T, network, channel, nick, chatCmd, service, model string) int64 {
	t.Helper()
	userID := ensureTestUser(t, network, nick)
	sid, err := sessionMgr.CreateSession(network, channel, userID, chatCmd, service, model)
	require.NoError(t, err, "CreateSession")
	return sid
}

func setupTestJobManager(t *testing.T) {
	t.Helper()
	queueMgr = NewQueueManager(NoticesConfig{Queue: QueueNotices{Msg: "queued", Started: "started"}}, 5)
	queueMgr.UpdateServiceLimits(map[string]Service{"testsvc": {Parallel: 1}})
	queueMgr.Start()
	asyncJobMgr = newGenericJobMgr[asyncJobPayload]()
	asyncJobMgr.ctx, asyncJobMgr.cancel = context.WithCancel(context.Background())
	t.Cleanup(func() {
		if queueMgr != nil {
			queueMgr.Stop()
		}
		if asyncJobMgr.cancel != nil {
			asyncJobMgr.cancel()
		}
	})
}

func setupNoticesDefaults(t *testing.T) {
	t.Helper()
	var n NoticesConfig
	setNoticesDefaults(&n)
	configMu.Lock()
	config.Notices = n
	configMu.Unlock()
}

func insertTestMessage(t *testing.T, sessionID int64, role, content string) {
	t.Helper()
	err := insertDBMessage(sessionID, role, content, nil, nil, nil, nil)
	require.NoError(t, err, "insertDBMessage")
}

func makeTestAIConfig() AIConfig {
	return AIConfig{
		Name:       "testchat",
		Service:    "testsvc",
		Model:      "test-model",
		MaxHistory: 20,
		Timeout:    30 * time.Second,
	}
}

func setupBotTest(t *testing.T) *girc.Client {
	t.Helper()
	setupTestDB(t)
	setupTestJobManager(t)
	setupCancelTestMCP(t)
	setupNoticesDefaults(t)

	client := girc.New(girc.Config{
		Server: "localhost",
		Port:   6667,
		Nick:   "testbot",
	})

	origBots := bots
	bots = map[string]*Bot{
		"testnet": {Client: client},
	}
	t.Cleanup(func() { bots = origBots })

	return client
}
