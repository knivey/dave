package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// helper: build a slice of N ChatMessage instances representing turns of
// (user, assistant) pairs after a system prompt. roles is the role for each
// message in order. starts at idx 0 with system, alternating user/assistant
// thereafter.
func makeChatMessages(n int) []ChatMessage {
	out := make([]ChatMessage, 0, n)
	out = append(out, ChatMessage{Role: RoleSystem, Content: "system prompt"})
	for i := 1; i < n; i++ {
		if i%2 == 1 {
			out = append(out, ChatMessage{Role: RoleUser, Content: "user msg"})
		} else {
			out = append(out, ChatMessage{Role: RoleAssistant, Content: "assistant msg"})
		}
	}
	return out
}

func TestPickCompactionCutTurn_BasicTwoThirds(t *testing.T) {
	// 1 system + 12 turn messages = 7 turns total (turn 0 = system)
	// non-system turns = 6 (each of 1 user + 1 assistant = 2 msgs ⇒ 3 turns
	// of 2 msgs = 6 msgs but buildTurns groups by RoleUser starts).
	// Compute via real buildTurns.
	msgs := makeChatMessages(13)
	turns := buildTurns(msgs)
	cut := pickCompactionCutTurn(msgs, turns)
	require.NotEqual(t, -1, cut, "should find a cut point")
	assert.Greater(t, cut, 0, "cut must skip system turn")
	assert.Less(t, cut, len(turns)-1, "cut must leave a tail")
}

func TestPickCompactionCutTurn_TooShort(t *testing.T) {
	cases := []struct {
		name string
		n    int
	}{
		{"only system", 1},
		{"system + 1 user", 2},
		{"system + 1 turn", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := makeChatMessages(tc.n)
			turns := buildTurns(msgs)
			cut := pickCompactionCutTurn(msgs, turns)
			assert.Equal(t, -1, cut, "expected refusal")
		})
	}
}

func TestPickCompactionCutTurn_NeverSplitsTurns(t *testing.T) {
	// Build a multi-message turn (user → assistant tool_call → tool result)
	// to ensure the cut point lands on a turn boundary.
	msgs := []ChatMessage{
		{Role: RoleSystem, Content: "system"},
		{Role: RoleUser, Content: "u1"},
		{Role: RoleAssistant, Content: "a1"},
		{Role: RoleUser, Content: "u2"},
		{Role: RoleAssistant, Content: "", ToolCalls: []ToolCall{{ID: "x", Function: FunctionCall{Name: "f"}}}},
		{Role: RoleTool, Content: "tool result", ToolCallID: "x"},
		{Role: RoleAssistant, Content: "a2"},
		{Role: RoleUser, Content: "u3"},
		{Role: RoleAssistant, Content: "a3"},
	}
	turns := buildTurns(msgs)
	cut := pickCompactionCutTurn(msgs, turns)
	require.NotEqual(t, -1, cut)
	// Verify the cut ends at the end of a turn (i.e. the next message
	// after the archived range is a user role, or end of slice).
	lastIdx := turns[cut].end - 1
	if lastIdx+1 < len(msgs) {
		assert.Equal(t, RoleUser, msgs[lastIdx+1].Role,
			"message after archive range must start a new turn (user role)")
	}
}

func TestPickCompactionCutTurn_TailMustStartWithUser(t *testing.T) {
	// Synthetic transcript where the natural 2/3 cut would place the tail
	// boundary at a non-user message. Force the picker to either advance
	// the cut to a valid boundary or return -1.
	//
	// We build buildTurns by hand using messageTurn since real messages
	// always have user-anchored turns. This test exercises the safety
	// net's branch logic directly.
	msgs := []ChatMessage{
		{Role: RoleSystem, Content: "sys"},      // 0
		{Role: RoleUser, Content: "u1"},         // 1
		{Role: RoleAssistant, Content: "a1"},    // 2
		{Role: RoleUser, Content: "u2"},         // 3
		{Role: RoleAssistant, Content: "a2"},    // 4
		{Role: RoleSystem, Content: "injected"}, // 5  ← non-user boundary
		{Role: RoleAssistant, Content: "a3"},    // 6
		{Role: RoleUser, Content: "u3"},         // 7
		{Role: RoleAssistant, Content: "a3b"},   // 8
	}
	// Manually construct turns with a non-user boundary at idx 5.
	turns := []messageTurn{
		{start: 0, end: 1}, // system
		{start: 1, end: 3}, // user u1, asst a1
		{start: 3, end: 5}, // user u2, asst a2  (boundary→idx 5 is RoleSystem)
		{start: 5, end: 7}, // system injected, asst a3 (boundary→idx 7 is RoleUser ✓)
		{start: 7, end: 9}, // user u3, asst a3b
	}
	cut := pickCompactionCutTurn(msgs, turns)
	require.NotEqual(t, -1, cut, "should advance past non-user boundary to a safe one")
	tailStart := turns[cut].end
	require.Less(t, tailStart, len(msgs))
	assert.Equal(t, RoleUser, msgs[tailStart].Role,
		"chosen cut's tail must begin with RoleUser")
}

func TestPickCompactionCutTurn_RefusesWhenNoUserBoundary(t *testing.T) {
	// All boundaries past the natural 2/3 cut are non-user. Picker must
	// return -1 (refusal) rather than create a system→assistant chain.
	msgs := []ChatMessage{
		{Role: RoleSystem, Content: "sys"},   // 0
		{Role: RoleUser, Content: "u1"},      // 1
		{Role: RoleAssistant, Content: "a1"}, // 2
		{Role: RoleUser, Content: "u2"},      // 3
		{Role: RoleAssistant, Content: "a2"}, // 4
		{Role: RoleSystem, Content: "inj"},   // 5
		{Role: RoleAssistant, Content: "a3"}, // 6
	}
	turns := []messageTurn{
		{start: 0, end: 1}, // system
		{start: 1, end: 3}, // user u1
		{start: 3, end: 5}, // user u2  (boundary idx 5 = system)
		{start: 5, end: 7}, // system+asst (boundary = end of slice; out of bounds means refuse)
	}
	cut := pickCompactionCutTurn(msgs, turns)
	assert.Equal(t, -1, cut, "no safe boundary → refuse")
}

func TestCompactSession_LiveHistoryStartsWithUserAfterSystem(t *testing.T) {
	// Invariant test: after a successful compaction, the live history's
	// first non-system message must be RoleUser. This protects against
	// providers that reject system→assistant chains.
	setupTestDB(t)

	stub := newSummarizerStubServer(t, "Summary text.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 6; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}
	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.NotEmpty(t, live)

	// Walk past leading system messages and assert first non-system is user.
	idx := 0
	for idx < len(live) && live[idx].Role == RoleSystem {
		idx++
	}
	require.Less(t, idx, len(live), "live history must contain non-system messages after compaction")
	assert.Equal(t, RoleUser, live[idx].Role,
		"first non-system message after compaction must be RoleUser")
}

func TestStripImagesForSummary(t *testing.T) {
	msgs := []ChatMessage{
		{Role: RoleUser, MultiContent: []MessagePart{
			{Type: PartTypeText, Text: "hello"},
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64,XXXX"}},
			{Type: PartTypeText, Text: "world"},
		}},
		{Role: RoleUser, MultiContent: []MessagePart{
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "https://example.com/a.png"}},
		}},
		{Role: RoleAssistant, Content: "no multi content here"},
	}
	out := stripImagesForSummary(msgs)
	require.Len(t, out, 3)

	// First message: text retained, image replaced with placeholder.
	require.Len(t, out[0].MultiContent, 3)
	assert.Equal(t, "hello", out[0].MultiContent[0].Text)
	assert.Equal(t, PartTypeText, out[0].MultiContent[1].Type)
	assert.Equal(t, "[image]", out[0].MultiContent[1].Text)
	assert.Equal(t, "world", out[0].MultiContent[2].Text)

	// Second message: image-only got replaced and we should still have a
	// non-empty content payload.
	require.NotEmpty(t, out[1].MultiContent)
	hasImagePlaceholder := false
	for _, p := range out[1].MultiContent {
		if p.Type == PartTypeText && strings.Contains(p.Text, "image") {
			hasImagePlaceholder = true
		}
	}
	assert.True(t, hasImagePlaceholder, "image-only message should have a placeholder text part")

	// Third message: untouched.
	assert.Equal(t, "no multi content here", out[2].Content)
	assert.Empty(t, out[2].MultiContent)

	// Ensure originals are not mutated (defensive — slice elements share
	// the same underlying ImageURL pointer but we reassigned MultiContent
	// to a new slice).
	require.Len(t, msgs[0].MultiContent, 3)
	assert.Equal(t, PartTypeImageURL, msgs[0].MultiContent[1].Type)
}

func TestLoadDBSessionMessages_FiltersArchived(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "model")
	for i := 0; i < 4; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "m"}))
	}
	// Mark message #2 (id 2) as archived.
	all, err := loadDBSessionMessagesAll(sid)
	require.NoError(t, err)
	require.Len(t, all, 4)

	require.NoError(t, theDB.Model(&Message{}).Where("id = ?", all[1].ID).
		Updates(map[string]interface{}{"archived": true, "compaction_id": int64(99)}).Error)

	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	assert.Len(t, live, 3, "archived row should be excluded from live history")

	allAgain, err := loadDBSessionMessagesAll(sid)
	require.NoError(t, err)
	assert.Len(t, allAgain, 4, "all-loader should still see archived row")
}

func TestArchiveMessagesRange(t *testing.T) {
	db := setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "model")
	for i := 0; i < 5; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "m"}))
	}
	all, _ := loadDBSessionMessagesAll(sid)
	require.Len(t, all, 5)

	// Archive ids[1..3] inclusive.
	first := all[1].ID
	last := all[3].ID
	require.NoError(t, archiveMessagesRange(db, sid, 42, first, last))

	live, _ := loadDBSessionMessages(sid)
	assert.Len(t, live, 2, "two non-archived rows expected")

	for _, m := range all[1:4] {
		var fresh Message
		require.NoError(t, db.Where("id = ?", m.ID).First(&fresh).Error)
		assert.True(t, fresh.Archived)
		require.NotNil(t, fresh.CompactionID)
		assert.Equal(t, int64(42), *fresh.CompactionID)
	}
}

func TestCompactSession_RefusesShort(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "model")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))

	_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, AIConfig{Service: "svc", Model: "model", Timeout: time.Second})
	assert.ErrorIs(t, err, ErrCompactionTooShort)
}

func TestCompactSession_EnforcesMinTurns(t *testing.T) {
	setupTestDB(t)

	stub := newSummarizerStubServer(t, "Summary.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}

	cases := []struct {
		name      string
		minTurns  int
		numTurns  int
		wantShort bool
	}{
		{"default_min_4_turns_fails", 6, 4, true},
		{"default_min_6_turns_passes", 6, 6, false},
		{"low_min_4_turns_passes", 3, 4, false},
		{"high_min_10_turns_fails", 10, 8, true},
	}

	prev := config.Compaction
	defer func() { config.Compaction = prev }()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config.Compaction = CompactionConfig{
				Enabled:     true,
				AutoEnabled: true,
				MinTurns:    tc.minTurns,
			}

			sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
			require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
			for i := 0; i < tc.numTurns; i++ {
				require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
				require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
			}

			_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
				SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
			}, cfg)

			if tc.wantShort {
				assert.ErrorIs(t, err, ErrCompactionTooShort,
					"compaction should refuse when turns=%d < min_turns=%d", tc.numTurns, tc.minTurns)
			} else {
				assert.NoError(t, err,
					"compaction should succeed when turns=%d >= min_turns=%d", tc.numTurns, tc.minTurns)
			}
		})
	}
}

func TestCompactSession_EndToEnd(t *testing.T) {
	setupTestDB(t)

	// Stub the summarizer transport: any HTTP call returns a minimal
	// chat-completion JSON. We rely on the openai SDK pointing at a stub
	// HTTP server. The simplest path: register a service whose BaseURL
	// is our local test server, and pass an AIConfig referencing it.
	stub := newSummarizerStubServer(t, "Concise summary of prior conversation.")
	defer stub.Close()

	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	// Seed: system + 6 turns (12 user/assistant messages) so we have
	// enough material.
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 6; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}
	// Set a response_id so we can verify it gets cleared.
	rid := "resp_test_123"
	require.NoError(t, sessionMgr.UpdateResponseID(sid, &rid))

	cfg := AIConfig{
		Service:   "stubsvc",
		Model:     "stubmodel",
		Timeout:   5 * time.Second,
		MaxTokens: 256,
	}
	res, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Greater(t, res.ArchivedCount, 0)

	// Live history should now contain only fresh-system + summary +
	// preserved tail messages, none of them archived.
	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.NotEmpty(t, live)
	assert.False(t, live[0].Archived)
	assert.Equal(t, RoleSystem, live[0].Role, "first live row should be the fresh system row")
	assert.Equal(t, RoleSystem, live[1].Role, "second live row should be the summary system row")
	assert.Contains(t, live[1].Content, "CONVERSATION SUMMARY")
	assert.Contains(t, live[1].Content, "Concise summary")

	// Originals are still in the all-loader. Archived count includes the
	// original system row, the contiguous archived range, and the
	// re-inserted preserved-tail rows (we copy them so the new fresh
	// system + summary rows can come first in id-asc order).
	all, _ := loadDBSessionMessagesAll(sid)
	archivedCount := 0
	for _, m := range all {
		if m.Archived {
			archivedCount++
		}
	}
	assert.GreaterOrEqual(t, archivedCount, res.ArchivedCount+1,
		"archived count includes at minimum the original system row")

	// Session response_id reset.
	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	assert.Nil(t, session.ResponseID, "response_id must be cleared post-compaction")

	// Compactions table populated.
	comps, err := getCompactionsForSession(sid)
	require.NoError(t, err)
	require.Len(t, comps, 1)
	assert.Equal(t, "manual", comps[0].Trigger)
	assert.Equal(t, res.CompactionID, comps[0].ID)
}

func TestCompactSession_Concurrency(t *testing.T) {
	setupTestDB(t)

	stub := newSummarizerStubServer(t, "Summary.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 6; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
				SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
			}, cfg)
			results[idx] = err
		}(i)
	}
	wg.Wait()

	successes := 0
	inProgress := 0
	for _, err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrCompactionInProgress) || errors.Is(err, ErrCompactionTooShort) {
			inProgress++
		}
	}
	assert.GreaterOrEqual(t, successes, 1, "at least one compaction should succeed")
	// The other may either be rejected by the lock OR succeed if it raced
	// after the first commit, in which case the second sees too few
	// unarchived turns and fails with ErrCompactionTooShort.
	assert.Equal(t, 2, successes+inProgress)
}

func TestShouldAutoCompact(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "model")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))

	// Insert a TurnUsage row.
	require.NoError(t, theDB.Create(&TurnUsage{SessionID: sid, PromptTokens: 90000, APIPath: "x"}).Error)

	cases := []struct {
		name     string
		ccfg     CompactionConfig
		expected bool
	}{
		{"disabled", CompactionConfig{Enabled: false, AutoEnabled: true, ContextWindow: 100000, AutoThreshold: 0.7}, false},
		{"auto disabled", CompactionConfig{Enabled: true, AutoEnabled: false, ContextWindow: 100000, AutoThreshold: 0.7}, false},
		{"no context window", CompactionConfig{Enabled: true, AutoEnabled: true, AutoThreshold: 0.7}, false},
		{"below threshold", CompactionConfig{Enabled: true, AutoEnabled: true, ContextWindow: 200000, AutoThreshold: 0.7}, false},
		{"above threshold", CompactionConfig{Enabled: true, AutoEnabled: true, ContextWindow: 100000, AutoThreshold: 0.7}, true},
	}
	prev := config.Compaction
	defer func() { config.Compaction = prev }()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config.Compaction = tc.ccfg
			got := sessionMgr.ShouldAutoCompact(sid, AIConfig{})
			assert.Equal(t, tc.expected, got)
		})
	}
}

// TestCompactSession_TagsTailCopiesWithSourceCompactionID verifies the basic
// invariant that re-inserted preserved-tail rows are tagged with
// SourceCompactionID = comp.ID. This tag is what the next compaction uses
// to identify duplicates so it can supersede them instead of double-counting.
func TestCompactSession_TagsTailCopiesWithSourceCompactionID(t *testing.T) {
	setupTestDB(t)

	stub := newSummarizerStubServer(t, "Summary.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 6; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	res, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	// After compaction, the live history should be: fresh-system, summary,
	// then tail-copies. The tail-copy rows should have
	// SourceCompactionID = res.CompactionID. The fresh-system and
	// summary rows themselves should NOT (they are unique structural
	// artifacts; superseding them on the next compaction would lose the
	// summary chain).
	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(live), 3,
		"live history should contain at minimum fresh-system, summary, and one tail row")

	assert.Nil(t, live[0].SourceCompactionID,
		"fresh-system row should not be tagged as a tail-copy")
	assert.Nil(t, live[1].SourceCompactionID,
		"summary row should not be tagged as a tail-copy")

	tailCopies := 0
	for _, m := range live[2:] {
		require.NotNil(t, m.SourceCompactionID,
			"every preserved-tail row must be tagged with SourceCompactionID")
		assert.Equal(t, res.CompactionID, *m.SourceCompactionID)
		tailCopies++
	}
	assert.Greater(t, tailCopies, 0)
}

// TestRepeatCompaction_DoesNotInflateArchivedCount is the core regression for
// the storage-amplification fix. Two compactions are performed back-to-back.
// The archived count exposed to user-facing surfaces (loadDBSessionMessagesAll
// and the count query used by historySessions) must NOT include the
// tail-copies inserted by compaction #1 that compaction #2 then re-archives:
// those rows duplicate content already covered by summary #1 and would
// mislead the user about how much actual conversation got compacted.
func TestRepeatCompaction_DoesNotInflateArchivedCount(t *testing.T) {
	setupTestDB(t)

	stub := newSummarizerStubServer(t, "Summary text.")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second, MaxHistory: 100},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 12; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}

	// Compaction #1.
	_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	// Count tail-copies inserted by compaction #1 — these are the rows
	// that compaction #2 must NOT count as fresh archived material.
	var tailCopiesAfter1 int64
	require.NoError(t, theDB.Model(&Message{}).
		Where("session_id = ? AND source_compaction_id IS NOT NULL", sid).
		Count(&tailCopiesAfter1).Error)
	require.Greater(t, tailCopiesAfter1, int64(0),
		"compaction #1 should have inserted at least one tail-copy")

	// Add fresh material so compaction #2 has something genuine to chew on.
	for i := 0; i < 8; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u2"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a2"}))
	}

	// Compaction #2.
	_, err = sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	// User-facing all-loader (archived viewer) excludes superseded rows.
	all, err := loadDBSessionMessagesAll(sid)
	require.NoError(t, err)
	for _, m := range all {
		assert.False(t, m.Superseded,
			"loadDBSessionMessagesAll must never return superseded rows")
	}

	// Count of superseded rows on disk equals the number of compaction-1
	// tail copies (every one was re-archived as superseded by compaction #2).
	var supersededOnDisk int64
	require.NoError(t, theDB.Model(&Message{}).
		Where("session_id = ? AND superseded = ?", sid, true).
		Count(&supersededOnDisk).Error)
	assert.Equal(t, tailCopiesAfter1, supersededOnDisk,
		"every tail-copy from compaction #1 should be marked superseded after compaction #2")

	// The count query used by historySessions must report only
	// non-superseded archived rows.
	var visibleArchived int64
	require.NoError(t, theDB.Model(&Message{}).
		Where("session_id = ? AND archived = ? AND superseded = ?", sid, true, false).
		Count(&visibleArchived).Error)

	// All-archived count (including superseded) is what the user WOULD see
	// without the fix — it's strictly larger when supersession kicks in.
	var allArchived int64
	require.NoError(t, theDB.Model(&Message{}).
		Where("session_id = ? AND archived = ?", sid, true).
		Count(&allArchived).Error)
	assert.Less(t, visibleArchived, allArchived,
		"visible archived count must be strictly less than total archived "+
			"count after a repeat compaction (otherwise the fix did nothing)")

	// Sanity: superseded rows still exist on disk (not GC'd in this change).
	allRows, err := loadDBSessionMessagesIncludingSuperseded(sid)
	require.NoError(t, err)
	assert.Greater(t, len(allRows), len(all),
		"raw loader should still see superseded rows on disk")
}
