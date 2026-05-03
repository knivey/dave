package main

import (
	"testing"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
)

func setupHistoryTest(t *testing.T) (*girc.Client, func()) {
	t.Helper()
	setupJMTestDB(t)
	setupTestJobManager(t)
	setupCancelTestMCP(t)

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

	return client, func() {}
}

func makeHistoryEvent(channel, nick string) girc.Event {
	return girc.Event{
		Source: &girc.Source{
			Name:  nick,
			Ident: "user",
			Host:  "host",
		},
		Params: []string{channel},
	}
}

func TestHistoryDelete_CancelsAsyncJobs(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	network := Network{Name: "testnet", Trigger: "!"}
	ctxKey := "testnet#testuser"
	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	jobMgr.jobs["job-delete-1"] = &asyncJob{
		JobID:     "job-delete-1",
		SessionID: sid,
		CtxKey:    ctxKey,
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
		cancel:    func() {},
	}
	jobMgr.jobs["job-delete-2"] = &asyncJob{
		JobID:     "job-delete-2",
		SessionID: sid,
		CtxKey:    ctxKey,
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
		cancel:    func() {},
	}

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	historyDelete(network, client, e, "1")

	assert.NotContains(t, jobMgr.jobs, "job-delete-1", "job-delete-1 should be removed after session delete")
	assert.NotContains(t, jobMgr.jobs, "job-delete-2", "job-delete-2 should be removed after session delete")
}

func TestHistoryDelete_NoAsyncJobs(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	network := Network{Name: "testnet", Trigger: "!"}
	ctxKey := "testnet#testuser"
	createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	historyDelete(network, client, e, "1")
}

func TestHistoryDelete_ClearsInMemoryContext(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	network := Network{Name: "testnet", Trigger: "!"}
	ctxKey := "testnet#testuser"
	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	chatContextsMutex.Lock()
	chatContextsMap[ctxKey] = ChatContext{SessionID: sid}
	chatContextsMutex.Unlock()

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	historyDelete(network, client, e, "1")

	chatContextsMutex.Lock()
	ctx := chatContextsMap[ctxKey]
	chatContextsMutex.Unlock()
	assert.Equal(t, int64(0), ctx.SessionID, "expected SessionID 0 after delete")
}

func TestHistoryDelete_OwnershipCheck(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	network := Network{Name: "testnet", Trigger: "!"}
	ctxKey := "testnet#testuser"
	sid := createTestSession(t, ctxKey, "testnet", "#test", "testuser", "testchat")

	jobMgr.jobs["job-owned"] = &asyncJob{
		JobID:     "job-owned",
		SessionID: sid,
		CtxKey:    ctxKey,
		Network:   "testnet",
		Channel:   "#test",
		Nick:      "testuser",
		cancel:    func() {},
	}

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "otheruser")

	historyDelete(network, client, e, "1")

	assert.Contains(t, jobMgr.jobs, "job-owned", "job-owned should NOT be removed when different user deletes")
}
