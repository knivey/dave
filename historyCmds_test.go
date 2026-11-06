package main

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// TestHistoryRegexesTolerateTrailingWhitespace guards against tab-completion
// trailing spaces silently dropping commands: users habitually leave a
// trailing space after any argument (e.g. "-delete 12 "), which the
// end-anchored regexes must ignore. Mirrors handleTrigger's dispatch
// boundary exactly (Match + extractSubmatchArgs).
func TestHistoryRegexesTolerateTrailingWhitespace(t *testing.T) {
	tests := []struct {
		name    string
		re      *regexp.Regexp
		input   string
		want    bool
		wantArg string
	}{
		{name: "delete plain", re: delete_re, input: "delete 12", want: true, wantArg: "12"},
		{name: "delete trailing space", re: delete_re, input: "delete 12 ", want: true, wantArg: "12"},
		{name: "delete trailing tab", re: delete_re, input: "delete 12\t", want: true, wantArg: "12"},
		{name: "delete no argument", re: delete_re, input: "delete", want: false},
		{name: "delete multi word", re: delete_re, input: "delete 12 x", want: false},

		{name: "resume plain", re: resume_re, input: "resume 7", want: true, wantArg: "7"},
		{name: "resume trailing space", re: resume_re, input: "resume 7 ", want: true, wantArg: "7"},
		{name: "resume trailing tab", re: resume_re, input: "resume 7\t", want: true, wantArg: "7"},
		{name: "resume no argument", re: resume_re, input: "resume", want: false},
		{name: "resume multi word", re: resume_re, input: "resume 7 x", want: false},

		{name: "sessions plain", re: sessions_re, input: "sessions", want: true, wantArg: ""},
		{name: "sessions with nick", re: sessions_re, input: "sessions alice", want: true, wantArg: "alice"},
		{name: "sessions nick trailing space", re: sessions_re, input: "sessions alice ", want: true, wantArg: "alice"},
		{name: "sessions bare trailing space", re: sessions_re, input: "sessions ", want: true, wantArg: ""},
		{name: "sessions multi word", re: sessions_re, input: "sessions alice bob", want: false},

		{name: "session bare", re: session_re, input: "session", want: true, wantArg: ""},
		{name: "session trailing space", re: session_re, input: "session ", want: true, wantArg: ""},
		{name: "session with arg rejected", re: session_re, input: "session 12", want: false},
		{name: "sessions does not match session_re", re: session_re, input: "sessions", want: false},
		{name: "session does not match sessions_re", re: sessions_re, input: "session", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matched := tt.re.Match([]byte(tt.input))
			assert.Equal(t, tt.want, matched, "%s match on %q", tt.name, tt.input)
			if !tt.want {
				return
			}
			args := extractSubmatchArgs(tt.re, tt.input)
			if tt.wantArg == "" {
				assert.Empty(t, args, "expected no capture for %q", tt.input)
				return
			}
			require.Len(t, args, 1, "expected exactly one capture group for %q", tt.input)
			assert.Equal(t, tt.wantArg, args[0], "captured argument for %q", tt.input)
		})
	}
}

func TestHistoryDelete_CancelsAsyncJobs(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

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
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	historyDelete(network, client, e, "1")
}

func TestHistoryDelete_DeletesSession(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	historyDelete(network, client, e, fmt.Sprintf("%d", sid))

	_, err := getDBSessionByID(sid)
	assert.Error(t, err, "expected session to be deleted from DB")
}

func TestHistoryDelete_OwnershipCheck(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	sid := createTestSession(t, "testnet", "#test", "testuser", "testchat", "", "")

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
	setupBotTest(t)

	stub := newSummarizerStubServer(t, "Compacted summary text.")
	defer stub.Close()
	prevServices := config.Services
	prevSessionsDisplayLimit := config.SessionsDisplayLimit
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
	}
	config.SessionsDisplayLimit = 10
	defer func() {
		config.Services = prevServices
		config.SessionsDisplayLimit = prevSessionsDisplayLimit
	}()

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"
	sid := createTestSession(t, network.Name, channel, "testuser", "testchat", "", "")
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
	setupBotTest(t)

	stub := newSummarizerStubServer(t, "Compacted summary text.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
	}
	defer func() { config.Services = prevServices }()

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"
	sid := createTestSession(t, network.Name, channel, "testuser", "testchat", "", "")
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
	setupTestDB(t)

	stub := newSummarizerStubServer(t, "Summary v1.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
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
	setupBotTest(t)

	stub := newSummarizerStubServer(t, "Summary.")
	defer stub.Close()
	prevServices := config.Services
	prevSessionsDisplayLimit := config.SessionsDisplayLimit
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
	}
	config.SessionsDisplayLimit = 10
	defer func() {
		config.Services = prevServices
		config.SessionsDisplayLimit = prevSessionsDisplayLimit
	}()

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"
	sid := createTestSession(t, network.Name, channel, "testuser", "testchat", "", "")
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
	setupBotTest(t)

	stub := newSummarizerStubServer(t, "Summary.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
	}
	defer func() { config.Services = prevServices }()

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"
	sid := createTestSession(t, network.Name, channel, "testuser", "testchat", "", "")

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

// runHistoryCommand drives an output-channel history command to completion
// and returns the lines it emitted (the goroutine/close/drain dance every
// handler test repeats). The handler receives REAL ctx/output channels —
// sendOrDone deadlocks on nil channels.
func runHistoryCommand(t *testing.T, fn func(ctx context.Context, output chan<- string), maxLines int) []string {
	t.Helper()
	output := make(chan string, maxLines)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() {
		fn(ctx, output)
		close(output)
	}()
	lines := drainOutput(t, output, maxLines, 2*time.Second)
	return lines
}

// TestHistorySessionCurrent_NoActiveSession pins the empty case: a user
// with no active session in the channel gets the current_none notice, not
// an error.
func TestHistorySessionCurrent_NoActiveSession(t *testing.T) {
	setupBotTest(t)

	network := Network{Name: "testnet", Trigger: "!"}
	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	lines := runHistoryCommand(t, func(ctx context.Context, output chan<- string) {
		historySessionCurrent(network, client, e, ctx, output)
	}, 4)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "No active session", "empty case must get the current_none notice")
}

// TestHistorySessionCurrent_Full pins the full detail view: header with
// date/ago/model/service, starter preview, live/archived counts (archived
// suffix mirroring the list view), provider last-turn usage with the
// reasoning fragment, context pressure from provider prompt tokens vs the
// resolved window, and the cloned-from tag rendered like the list view.
func TestHistorySessionCurrent_Full(t *testing.T) {
	setupBotTest(t)

	prevChats := config.Commands.Chats
	prevCompaction := config.Compaction
	config.Commands.Chats = map[string]AIConfig{
		"testchat": {Service: "svc1", Model: "gpt-4o", ContextWindow: 16000},
	}
	config.Compaction = CompactionConfig{Enabled: true, AutoEnabled: true, AutoThreshold: 0.8}
	defer func() {
		config.Commands.Chats = prevChats
		config.Compaction = prevCompaction
	}()

	network := Network{Name: "testnet", Trigger: "!"}
	channel := "#test"
	// Source session for the clone tag, owned by another user.
	srcSID := createTestSession(t, network.Name, channel, "dave", "oldchat", "svc1", "m1")
	sid := createTestSession(t, network.Name, channel, "testuser", "testchat", "svc1", "gpt-4o")
	starter := "explain the queue system in detail"
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: starter}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "ok"}))

	// Archive one row so the archived suffix shows (same shape the list
	// view counts: archived=true, superseded=false).
	var rows []Message
	require.NoError(t, theDB.Where("session_id = ?", sid).Order("id ASC").Find(&rows).Error)
	require.NoError(t, theDB.Model(&Message{}).Where("id = ?", rows[0].ID).Update("archived", true).Error)

	// Cloned-from tag material.
	require.NoError(t, theDB.Model(&Session{}).Where("id = ?", sid).
		Updates(map[string]any{"cloned_from_id": srcSID, "cloned_from_nick": "dave"}).Error)

	// Provider usage row: 6500/16000 = 40.625 → 41%.
	require.NoError(t, theDB.Create(&TurnUsage{
		SessionID: sid, PromptTokens: 6500, CachedTokens: 3000,
		CompletionTokens: 800, ReasoningTokens: 400, APIPath: "x",
	}).Error)

	client := bots["testnet"].Client
	e := makeHistoryEvent(channel, "testuser")

	lines := runHistoryCommand(t, func(ctx context.Context, output chan<- string) {
		historySessionCurrent(network, client, e, ctx, output)
	}, 12)
	joined := strings.Join(lines, "\n")

	var sess Session
	require.NoError(t, theDB.Where("id = ?", sid).First(&sess).Error)

	assert.Contains(t, joined, fmt.Sprintf("#%d", sid), "header names the session id")
	assert.Contains(t, joined, "!testchat", "header shows trigger+command like the list view")
	assert.Contains(t, joined, "gpt-4o", "header shows the live config's model")
	assert.Contains(t, joined, "svc1", "header shows the service")
	assert.Contains(t, joined, sess.CreatedAt.Format("Jan 02"), "header shows the start date")
	assert.Contains(t, joined, starter, "starter preview line shows the first message")
	assert.Contains(t, joined, "2 msgs live (1 archived)", "live count excludes the archived row; suffix mirrors the list view")
	assert.Contains(t, joined, "prompt 6500 (cached 3000) · completion 800 · reasoning 400",
		"provider last-turn line with the reasoning fragment when reasoning exists")
	assert.Contains(t, joined, "41% of 16k", "context pressure from provider prompt tokens vs the resolved window")
	assert.Contains(t, joined, "auto-compacts at 80%", "threshold fragment when auto-compaction is enabled")
	assert.Contains(t, joined, fmt.Sprintf("cloned from #%d by dave", srcSID), "cloned-from tag renders like the list view")
	assert.NotContains(t, joined, "compacted", "no compaction events yet — fragment omitted")
	assert.NotContains(t, joined, "responses chain", "no ResponseID — chain fragment omitted")
}

// TestHistorySessionCurrent_Omissions pins the conditional lines: with no
// provider usage rows and no resolvable context window, the usage and
// context lines drop entirely while the header/starter/counts remain.
func TestHistorySessionCurrent_Omissions(t *testing.T) {
	setupBotTest(t)

	prevChats := config.Commands.Chats
	prevCompaction := config.Compaction
	prevServices := config.Services
	config.Commands.Chats = map[string]AIConfig{
		"testchat": {Service: "svc1", Model: "gpt-4o"}, // no ContextWindow
	}
	config.Compaction = CompactionConfig{} // no window fallback either
	config.Services = map[string]Service{} // self-contained: no ambient svc1 window
	defer func() {
		config.Commands.Chats = prevChats
		config.Compaction = prevCompaction
		config.Services = prevServices
	}()

	network := Network{Name: "testnet", Trigger: "!"}
	sid := createTestSession(t, network.Name, "#test", "testuser", "testchat", "svc1", "gpt-4o")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	lines := runHistoryCommand(t, func(ctx context.Context, output chan<- string) {
		historySessionCurrent(network, client, e, ctx, output)
	}, 8)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, fmt.Sprintf("#%d", sid), "header still shows")
	assert.Contains(t, joined, "2 msgs live", "message count still shows")
	assert.Contains(t, joined, "hi", "starter preview still shows")
	assert.NotContains(t, joined, "prompt", "no usage rows — provider line omitted")
	assert.NotContains(t, joined, "Context:", "no resolvable window — context line omitted")
}

// TestHistorySessionCurrent_CommandGone pins the dead-config path: the
// session's chat command no longer exists → the shared command_gone error.
func TestHistorySessionCurrent_CommandGone(t *testing.T) {
	setupBotTest(t)

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{}
	defer func() { config.Commands.Chats = prevChats }()

	network := Network{Name: "testnet", Trigger: "!"}
	sid := createTestSession(t, network.Name, "#test", "testuser", "testchat", "svc1", "gpt-4o")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")

	lines := runHistoryCommand(t, func(ctx context.Context, output chan<- string) {
		historySessionCurrent(network, client, e, ctx, output)
	}, 4)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "no longer exists", "command_gone error surfaces")
}

// TestHistorySessionCurrent_Fragments pins the conditional context
// fragments in their PRESENT state (the Full test pins them absent): the
// compaction count, the responses-chain flag, and the <=0 threshold
// falling back to the ApplyDefaults 0.7 — the displayed threshold is the
// enforced one.
func TestHistorySessionCurrent_Fragments(t *testing.T) {
	setupBotTest(t)

	prevChats := config.Commands.Chats
	prevCompaction := config.Compaction
	config.Commands.Chats = map[string]AIConfig{
		"testchat": {Service: "svc1", Model: "gpt-4o", ContextWindow: 8000},
	}
	// AutoThreshold 0 must display as the 0.7 default.
	config.Compaction = CompactionConfig{Enabled: true, AutoEnabled: true, AutoThreshold: 0}
	defer func() {
		config.Commands.Chats = prevChats
		config.Compaction = prevCompaction
	}()

	network := Network{Name: "testnet", Trigger: "!"}
	sid := createTestSession(t, network.Name, "#test", "testuser", "testchat", "svc1", "gpt-4o")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))

	// Two compaction events → "compacted 2×".
	for i := 0; i < 2; i++ {
		require.NoError(t, theDB.Create(&Compaction{SessionID: sid, Trigger: "manual"}).Error)
	}
	// Active Responses chain.
	rid := "resp_123"
	require.NoError(t, theDB.Model(&Session{}).Where("id = ?", sid).Update("response_id", rid).Error)
	// Usage: 4000/8000 = 50%.
	require.NoError(t, theDB.Create(&TurnUsage{
		SessionID: sid, PromptTokens: 4000, CompletionTokens: 100, APIPath: "x",
	}).Error)

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")
	lines := runHistoryCommand(t, func(ctx context.Context, output chan<- string) {
		historySessionCurrent(network, client, e, ctx, output)
	}, 10)
	joined := strings.Join(lines, "\n")

	assert.Contains(t, joined, "50% of 8k", "context percentage present")
	assert.Contains(t, joined, "auto-compacts at 70%", "unset threshold displays the ApplyDefaults 0.7 — the enforced value")
	assert.Contains(t, joined, "compacted 2×", "compaction count fragment present")
	assert.Contains(t, joined, "responses chain", "responses-chain fragment present")
}

// TestHistorySessionCurrent_WrapsLongLines pins sibling parity: every
// emitted line goes through wrapLine, so an oversize notice line (e.g. an
// admin-customized template) is split instead of risking server-side
// truncation.
func TestHistorySessionCurrent_WrapsLongLines(t *testing.T) {
	setupBotTest(t)

	prevChats := config.Commands.Chats
	prevCompaction := config.Compaction
	prevStarter := config.Notices.Sessions.CurrentStarter
	config.Commands.Chats = map[string]AIConfig{
		"testchat": {Service: "svc1", Model: "gpt-4o"},
	}
	config.Compaction = CompactionConfig{}
	// Force an oversize starter line through the handler. Words with
	// spaces: wrapLine splits on word boundaries, so an unbroken
	// 450-byte word would pass through verbatim by design (pre-existing
	// wrapLine behavior) — realistic oversize content has spaces.
	config.Notices.Sessions.CurrentStarter = strings.Repeat("x ", 250) + "{preview}"
	defer func() {
		config.Commands.Chats = prevChats
		config.Compaction = prevCompaction
		config.Notices.Sessions.CurrentStarter = prevStarter
	}()

	network := Network{Name: "testnet", Trigger: "!"}
	sid := createTestSession(t, network.Name, "#test", "testuser", "testchat", "svc1", "gpt-4o")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))

	client := bots["testnet"].Client
	e := makeHistoryEvent("#test", "testuser")
	lines := runHistoryCommand(t, func(ctx context.Context, output chan<- string) {
		historySessionCurrent(network, client, e, ctx, output)
	}, 10)

	require.NotEmpty(t, lines)
	sawSplitStarter := false
	for _, l := range lines {
		assert.LessOrEqual(t, len(l), 400, "every emitted line must respect the wrapLine boundary")
		if strings.Contains(l, "hi") {
			sawSplitStarter = true
		}
	}
	assert.True(t, sawSplitStarter, "the oversize starter line was split, not dropped")
	assert.Contains(t, strings.Join(lines, "\n"), strings.Repeat("x ", 50),
		"the padded starter content survived the wrap")
}

func TestFormatWindowTokens(t *testing.T) {
	assert.Equal(t, "800", formatWindowTokens(800))
	assert.Equal(t, "1k", formatWindowTokens(1000))
	assert.Equal(t, "16k", formatWindowTokens(16000))
	assert.Equal(t, "131k", formatWindowTokens(131072))
	assert.Equal(t, "200k", formatWindowTokens(200000))
}

// TestHistorySessionCurrent_NoticesDefaults pins that the new notice keys
// survive a stale notices.toml (missing keys fall back to defaults) — the
// same contract every other notice key follows.
func TestHistorySessionCurrent_NoticesDefaults(t *testing.T) {
	var n NoticesConfig
	setNoticesDefaults(&n)
	for _, key := range []string{
		n.Sessions.CurrentNone, n.Sessions.CurrentHeader, n.Sessions.CurrentStarter,
		n.Sessions.CurrentMessages, n.Sessions.CurrentUsage, n.Sessions.CurrentContext,
	} {
		assert.NotEmpty(t, key)
	}
}
