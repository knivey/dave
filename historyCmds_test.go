package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat")

	asyncJobMgr.jobs["job-delete-1"] = &jobEntry[asyncJobPayload]{
		jobID:   "job-delete-1",
		payload: asyncJobPayload{sessionID: sid},
		network: "testnet",
		channel: "#test",
		nick:    "testuser",
		cancel:  func() {},
	}
	asyncJobMgr.jobs["job-delete-2"] = &jobEntry[asyncJobPayload]{
		jobID:   "job-delete-2",
		payload: asyncJobPayload{sessionID: sid},
		network: "testnet",
		channel: "#test",
		nick:    "testuser",
		cancel:  func() {},
	}

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	historyDelete(network, client, e, "1")

	assert.NotContains(t, asyncJobMgr.jobs, "job-delete-1", "job-delete-1 should be removed after session delete")
	assert.NotContains(t, asyncJobMgr.jobs, "job-delete-2", "job-delete-2 should be removed after session delete")
}

func TestHistoryDelete_NoAsyncJobs(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	network := Network{Name: "testnet", Trigger: "!"}
	createTestSession(t, "testnet", "#test", "testuser", "testchat")

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	historyDelete(network, client, e, "1")
}

func TestHistoryDelete_DeletesSession(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	network := Network{Name: "testnet", Trigger: "!"}
	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat")

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	historyDelete(network, client, e, fmt.Sprintf("%d", sid))

	_, err := getDBSessionByID(sid)
	assert.Error(t, err, "expected session to be deleted from DB")
}

func TestHistoryDelete_OwnershipCheck(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	network := Network{Name: "testnet", Trigger: "!"}
	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat")

	asyncJobMgr.jobs["job-owned"] = &jobEntry[asyncJobPayload]{
		jobID:   "job-owned",
		payload: asyncJobPayload{sessionID: sid},
		network: "testnet",
		channel: "#test",
		nick:    "testuser",
		cancel:  func() {},
	}

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "otheruser")

	historyDelete(network, client, e, "1")

	assert.Contains(t, asyncJobMgr.jobs, "job-owned", "job-owned should NOT be removed when different user deletes")
}

// drainOutput collects up to maxMsgs messages from the channel until it
// closes or maxWait elapses, then returns the joined text. Used by the
// history-display tests to inspect IRC output without spinning a real bot.
func drainOutput(t *testing.T, ch <-chan string, maxMsgs int, maxWait time.Duration) []string {
	t.Helper()
	var out []string
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()
	for i := 0; i < maxMsgs; i++ {
		select {
		case msg, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, msg)
		case <-deadline.C:
			return out
		}
	}
	return out
}

// TestHistorySessions_StarterPreservedAfterCompaction verifies that the
// `Session.FirstMessage` (the user's original starter) still appears in the
// `^sessions$` command output even after the session has been compacted and
// most messages are archived.
func TestHistorySessions_StarterPreservedAfterCompaction(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	stub := newSummarizerStubServer(t, "Compacted summary text.")
	defer stub.Close()
	prevServices := config.Services
	prevSessionsDisplayLimit := config.SessionsDisplayLimit
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	config.SessionsDisplayLimit = 10
	defer func() {
		config.Services = prevServices
		config.SessionsDisplayLimit = prevSessionsDisplayLimit
	}()

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"
	sid := createTestSession(t, network.Name, channel, "testuser", "testchat")
	starter := "what is the meaning of life and everything else"
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: starter}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "42"}))
	for i := 0; i < 5; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("u%d", i)}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: fmt.Sprintf("a%d", i)}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: network, Channel: channel, UserNick: "testuser", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	client := bots["testnet"].Client
	e := makeHistoryEvent(channel, "testuser")
	output := make(chan string, 16)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historySessions(network, client, e, ctx, output)
		close(output)
	}()
	lines := drainOutput(t, output, 16, 2*time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, starter,
		"-sessions output must include original starter message preview after compaction")
	assert.Contains(t, joined, "archived",
		"-sessions output should annotate compacted sessions with an archived count")
}

// TestHistoryShow_StarterInHeadAfterCompaction verifies that the head pair
// shown by `^history <id>` still contains the original first user message
// after the session has been compacted (the original message is now
// archived, but should still appear in the displayed head).
func TestHistoryShow_StarterInHeadAfterCompaction(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	stub := newSummarizerStubServer(t, "Compacted summary text.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"
	sid := createTestSession(t, network.Name, channel, "testuser", "testchat")
	starter := "DISTINCTIVE_STARTER_TOKEN"
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: starter}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "first reply"}))
	for i := 0; i < 5; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("u%d", i)}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: fmt.Sprintf("a%d", i)}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: network, Channel: channel, UserNick: "testuser", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	client := bots["testnet"].Client
	e := makeHistoryEvent(channel, "testuser")
	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		historyShow(network, client, e, ctx, output, fmt.Sprintf("%d", sid))
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, starter,
		"-history output head pair must include original starter even when archived")
	assert.Contains(t, joined, "archived",
		"-history output should mark archived rows and/or summarize archived count")
}

// TestRepeatAutoCompaction verifies that a session which remains over the
// auto-compaction threshold AFTER a first compaction will be compacted
// again on a subsequent turn. We simulate this by calling CompactSession
// twice in succession on a synthetically large session and asserting both
// runs succeed and produce distinct compaction rows.
func TestRepeatAutoCompaction(t *testing.T) {
	_, cleanup := setupTestDB(t)
	defer cleanup()

	stub := newSummarizerStubServer(t, "Summary v1.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	sid := testCreateSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 12; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("u%d", i)}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: fmt.Sprintf("a%d", i)}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	res1, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "auto",
	}, cfg)
	require.NoError(t, err, "first compaction should succeed")
	require.NotNil(t, res1)

	// After the first compaction, simulate continued conversation so the
	// next auto-compaction has fresh material to operate on (the new
	// fresh-system + summary + re-inserted tail is already there; add a
	// few more turns).
	for i := 0; i < 6; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("u-post%d", i)}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: fmt.Sprintf("a-post%d", i)}))
	}

	res2, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "auto",
	}, cfg)
	require.NoError(t, err, "second compaction should succeed")
	require.NotNil(t, res2)
	assert.NotEqual(t, res1.CompactionID, res2.CompactionID,
		"each compaction event should produce a distinct row")

	// Both compactions should be recorded.
	comps, err := getCompactionsForSession(sid)
	require.NoError(t, err)
	assert.Len(t, comps, 2, "two compaction rows expected")

	// Live history must still respect the system→user invariant.
	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	idx := 0
	for idx < len(live) && live[idx].Role == RoleSystem {
		idx++
	}
	require.Less(t, idx, len(live))
	assert.Equal(t, RoleUser, live[idx].Role,
		"after two compactions, first non-system row in live history must be RoleUser")
}

// TestHistorySessions_ArchivedCountExcludesSupersededRows verifies the
// user-facing fix: after two compactions, the archived suffix shown by
// `^sessions$` reflects only the non-superseded archived rows. Superseded
// tail-copy ghosts (which duplicate content already covered by an earlier
// summary) must NOT inflate the count.
func TestHistorySessions_ArchivedCountExcludesSupersededRows(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	stub := newSummarizerStubServer(t, "Summary.")
	defer stub.Close()
	prevServices := config.Services
	prevSessionsDisplayLimit := config.SessionsDisplayLimit
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	config.SessionsDisplayLimit = 10
	defer func() {
		config.Services = prevServices
		config.SessionsDisplayLimit = prevSessionsDisplayLimit
	}()

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"
	sid := createTestSession(t, network.Name, channel, "testuser", "testchat")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 12; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("u%d", i)}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: fmt.Sprintf("a%d", i)}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: network, Channel: channel, UserNick: "testuser", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	for i := 0; i < 8; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("u-post%d", i)}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: fmt.Sprintf("a-post%d", i)}))
	}

	_, err = sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: network, Channel: channel, UserNick: "testuser", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	// Compute what the archived suffix should be: count of archived rows
	// NOT including superseded ones.
	var visibleArchived int64
	require.NoError(t, theDB.Model(&Message{}).
		Where("session_id = ? AND archived = ? AND superseded = ?", sid, true, false).
		Count(&visibleArchived).Error)
	require.Greater(t, visibleArchived, int64(0))

	var totalArchived int64
	require.NoError(t, theDB.Model(&Message{}).
		Where("session_id = ? AND archived = ?", sid, true).
		Count(&totalArchived).Error)
	require.Greater(t, totalArchived, visibleArchived,
		"after two compactions there must be superseded rows on disk")

	client := bots["testnet"].Client
	e := makeHistoryEvent(channel, "testuser")
	output := make(chan string, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		historySessions(network, client, e, ctx, output)
		close(output)
	}()
	lines := drainOutput(t, output, 32, 2*time.Second)
	joined := strings.Join(lines, "\n")

	wantSuffix := fmt.Sprintf("(%d archived)", visibleArchived)
	assert.Contains(t, joined, wantSuffix,
		"-sessions archived suffix must reflect non-superseded archived rows only")

	// And we MUST NOT show the inflated total count.
	wrongSuffix := fmt.Sprintf("(%d archived)", totalArchived)
	assert.NotContains(t, joined, wrongSuffix,
		"-sessions archived suffix must not include superseded rows")
}

// TestHistoryShow_DoesNotShowSupersededRows verifies the history viewer
// hides tail-copy ghosts after a repeat compaction. The viewer walks
// loadDBSessionMessagesAll, which is now filtered. The user should never
// see duplicate archived content from earlier compactions.
func TestHistoryShow_DoesNotShowSupersededRows(t *testing.T) {
	_, cleanup := setupHistoryTest(t)
	defer cleanup()

	stub := newSummarizerStubServer(t, "Summary.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"
	sid := createTestSession(t, network.Name, channel, "testuser", "testchat")

	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 12; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("u%d", i)}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: fmt.Sprintf("a%d", i)}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: network, Channel: channel, UserNick: "testuser", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	for i := 0; i < 8; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: fmt.Sprintf("u-post%d", i)}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: fmt.Sprintf("a-post%d", i)}))
	}

	_, err = sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: network, Channel: channel, UserNick: "testuser", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	// Confirm there ARE superseded rows on disk before we test the viewer.
	var supersededOnDisk int64
	require.NoError(t, theDB.Model(&Message{}).
		Where("session_id = ? AND superseded = ?", sid, true).
		Count(&supersededOnDisk).Error)
	require.Greater(t, supersededOnDisk, int64(0),
		"test setup precondition: must have at least one superseded row")

	// loadDBSessionMessagesAll (used by historyShow) must omit them.
	visible, err := loadDBSessionMessagesAll(sid)
	require.NoError(t, err)
	for _, m := range visible {
		assert.False(t, m.Superseded,
			"loadDBSessionMessagesAll must not return superseded rows")
	}
}
