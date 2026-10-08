package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
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
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
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
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
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
	require.NoError(t, sessionMgr.UpdateResponseID(sid, &rid, "m"))

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

	// Session response_id reset — response_model goes with it (the pair is
	// always written/cleared together).
	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	assert.Nil(t, session.ResponseID, "response_id must be cleared post-compaction")
	assert.Nil(t, session.ResponseModel, "response_model must be cleared post-compaction")

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
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
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
		} else if errors.Is(err, ErrCompactionInProgress) || errors.Is(err, ErrCompactionTooShort) || errors.Is(err, ErrCompactionNothingNew) {
			inProgress++
		}
	}
	assert.GreaterOrEqual(t, successes, 1, "at least one compaction should succeed")
	// The other is rejected by the lock, or — if it slips past and starts
	// after the first commit — sees only the first compaction's tail-copies
	// as archivable material. With this seed (zero-value MinTurns and 3
	// live turns) the slip-through deterministically refuses with
	// ErrCompactionNothingNew; ErrCompactionTooShort stays in the accepted
	// set to keep the test robust against seed/MinTurns drift.
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

// TestShouldAutoCompactServiceWindowCascade covers the service-first
// context-window cascade: the session's service context_window overrides
// the [compaction] fallback in BOTH directions (each direction alone would
// also pass if the fallback were still the only value consulted, so the
// pair is what proves the service value is actually used), the fallback
// still applies when the service sets nothing, both-unset keeps auto off,
// and the enabled flags trump every window value.
func TestShouldAutoCompactServiceWindowCascade(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "bigsvc", "model")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
	// 90000 prompt tokens sits above 0.7*100000 and below 0.7*200000, so
	// which window wins fully determines the verdict.
	require.NoError(t, theDB.Create(&TurnUsage{SessionID: sid, PromptTokens: 90000, APIPath: "x"}).Error)

	cases := []struct {
		name        string
		svcWindow   int
		compWindow  int
		enabled     bool
		autoEnabled bool
		expected    bool
	}{
		{"service window overrides larger fallback", 100000, 200000, true, true, true},
		{"service window overrides smaller fallback", 200000, 100000, true, true, false},
		{"service unset falls back to compaction window", 0, 100000, true, true, true},
		{"both unset disables auto", 0, 0, true, true, false},
		{"enabled false trumps service window", 100000, 0, false, true, false},
		{"auto_enabled false trumps service window", 100000, 0, true, false, false},
	}

	prevCompaction := config.Compaction
	defer func() { config.Compaction = prevCompaction }()
	prevServices := config.Services
	defer func() { config.Services = prevServices }()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config.Compaction = CompactionConfig{
				Enabled:       tc.enabled,
				AutoEnabled:   tc.autoEnabled,
				ContextWindow: tc.compWindow,
				AutoThreshold: 0.7,
			}
			config.Services = map[string]Service{
				"bigsvc": {ContextWindow: tc.svcWindow},
			}
			got := sessionMgr.ShouldAutoCompact(sid, AIConfig{Service: "bigsvc"})
			assert.Equal(t, tc.expected, got)
		})
	}
}

// TestSumSessionReasoningTokens pins the replay-eligibility split: `all`
// sums every turn_usage reasoning row, `prior` excludes the LATEST
// row's contribution (the latest row's reasoning belongs to its own
// completion; only previous turns' reasoning can be replayed into the
// next request's prompt). No rows → 0,0 without error.
func TestSumSessionReasoningTokens(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "model")

	t.Run("no rows yields zeros", func(t *testing.T) {
		all, prior, err := sumSessionReasoningTokens(sid)
		require.NoError(t, err)
		assert.Equal(t, int64(0), all)
		assert.Equal(t, int64(0), prior)
	})

	t.Run("single row: prior is zero", func(t *testing.T) {
		single := createTestSession(t, "net", "#c", "u2", "cmd", "svc", "model")
		require.NoError(t, theDB.Create(&TurnUsage{SessionID: single, ReasoningTokens: 250}).Error)
		all, prior, err := sumSessionReasoningTokens(single)
		require.NoError(t, err)
		assert.Equal(t, int64(250), all, "all includes the only row")
		assert.Equal(t, int64(0), prior, "the latest row's reasoning is its own completion, not replayable")
	})

	t.Run("multiple rows: prior excludes the latest row", func(t *testing.T) {
		multi := createTestSession(t, "net", "#c", "u3", "cmd", "svc", "model")
		// Insert order determines id order, hence which row is latest.
		require.NoError(t, theDB.Create(&TurnUsage{SessionID: multi, ReasoningTokens: 100}).Error)
		require.NoError(t, theDB.Create(&TurnUsage{SessionID: multi, ReasoningTokens: 200}).Error)
		require.NoError(t, theDB.Create(&TurnUsage{SessionID: multi, ReasoningTokens: 40}).Error)
		all, prior, err := sumSessionReasoningTokens(multi)
		require.NoError(t, err)
		assert.Equal(t, int64(340), all)
		assert.Equal(t, int64(300), prior, "prior = all minus the latest (40)")
	})

	t.Run("zero-reasoning rows sum to zero", func(t *testing.T) {
		zero := createTestSession(t, "net", "#c", "u4", "cmd", "svc", "model")
		require.NoError(t, theDB.Create(&TurnUsage{SessionID: zero, ReasoningTokens: 0, PromptTokens: 100}).Error)
		require.NoError(t, theDB.Create(&TurnUsage{SessionID: zero, ReasoningTokens: 0, PromptTokens: 200}).Error)
		all, prior, err := sumSessionReasoningTokens(zero)
		require.NoError(t, err)
		assert.Equal(t, int64(0), all)
		assert.Equal(t, int64(0), prior)
	})

	// The first session must be unaffected by the rows created for the
	// other subtests (session_id scoping).
	all, _, err := sumSessionReasoningTokens(sid)
	require.NoError(t, err)
	assert.Equal(t, int64(0), all)
}

// TestCallSummarizerMissingServiceErrorsEarly pins the explicit
// missing-service error: when cfg.Service is absent from config.Services
// the call must fail BEFORE constructing the SDK client — no network
// attempt, no opaque upstream 401 from an empty BaseURL — and the message
// must name the missing service. The "other" service points at a
// fail-if-hit stub server: the early return must fire BEFORE any HTTP
// attempt, so a request reaching ANY server is itself a failure. The
// message assertion additionally pins which error path was taken.
func TestCallSummarizerMissingServiceErrorsEarly(t *testing.T) {
	hitStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("callSummarizer must not make an HTTP request when the service is missing from config")
	}))
	defer hitStub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"other": {BaseURL: hitStub.URL},
	}
	defer func() { config.Services = prevServices }()

	_, _, _, err := callSummarizer(context.Background(),
		AIConfig{Service: "nosuchsvc", Model: "m", Timeout: time.Second},
		"sys", nil, 1, 0)
	require.Error(t, err)
	assert.ErrorContains(t, err, `summarizer service "nosuchsvc" not found in config`)
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
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
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
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
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

// TestCompactSession_FoldsPriorSummaryIntoNextSummarization is the core
// regression for the dropped-prior-summary bug: on a repeat compaction the
// previous compaction's summary row sits BEFORE firstIdx, and the old
// partition loop dumped it into tailRegularIDs — archiving it without ever
// feeding it to the summarizer, silently destroying the accumulated rolling
// summary. The fix routes it through priorSummaryIDs: its content must
// appear in the second summarizer request, and the row itself must archive
// (visible, superseded=false) while the live history carries exactly ONE
// summary row — the fresh one.
func TestCompactSession_FoldsPriorSummaryIntoNextSummarization(t *testing.T) {
	setupTestDB(t)

	stub, getBodies := newRecordingSummarizerStubServer(t, "SUMMARY_ONE_CONTENT", "SUMMARY_TWO_CONTENT")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
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

	// Add fresh turns so compaction #2 has genuine new material.
	for i := 0; i < 3; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "later"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "later-a"}))
	}

	// Count the live tail-copies from compaction #1 — these are the rows
	// compaction #2 must supersede (they cover 4 preserved turns = 8 rows:
	// the 2/3 rule on 12 turns archives 8 turns and keeps 4).
	var taggedLiveBefore2 int64
	require.NoError(t, theDB.Model(&Message{}).
		Where("session_id = ? AND source_compaction_id IS NOT NULL AND archived = ?", sid, false).
		Count(&taggedLiveBefore2).Error)
	require.Equal(t, int64(8), taggedLiveBefore2,
		"seed sanity: compaction #1 must leave exactly 8 tagged tail-copies live")

	// Compaction #2.
	res2, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	bodies := getBodies()
	require.Len(t, bodies, 2, "expected exactly one summarizer call per compaction")

	// (i) The second summarizer request must carry the prior summary's
	// content — the rolling-summary chain accumulates instead of resetting.
	assert.Contains(t, bodies[1], "SUMMARY_ONE_CONTENT",
		"compaction #2 must feed summary #1 to the summarizer")

	// (ii) Live history contains exactly ONE summary row: the new one —
	// and no stray copy of the old summary's text anywhere in live.
	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	summaryRows := 0
	for _, m := range live {
		if strings.Contains(m.Content, "SUMMARY_ONE_CONTENT") {
			t.Fatalf("old summary content must not remain live in any row (id=%d role=%s)",
				m.ID, m.Role)
		}
		if strings.Contains(m.Content, "[CONVERSATION SUMMARY") {
			summaryRows++
			assert.Contains(t, m.Content, "SUMMARY_TWO_CONTENT",
				"the surviving summary must be the new one")
			assert.NotContains(t, m.Content, "SUMMARY_ONE_CONTENT",
				"the old summary must not remain live")
		}
	}
	assert.Equal(t, 1, summaryRows,
		"live history must contain exactly one summary row")

	// (iii) The prior summary row archived as REAL material: visible in
	// the all-loader, archived, NOT superseded.
	all, err := loadDBSessionMessagesAll(sid)
	require.NoError(t, err)
	priorFound := false
	for _, m := range all {
		if strings.Contains(m.Content, "SUMMARY_ONE_CONTENT") {
			priorFound = true
			assert.True(t, m.Archived,
				"prior summary row must be archived")
			assert.False(t, m.Superseded,
				"prior summary row is real archived material, not a tail-copy ghost")
		}
	}
	assert.True(t, priorFound,
		"prior summary row must still exist for the history viewer")

	// (iv) Existing invariant: live history starts with system rows, then
	// the first non-system message is RoleUser.
	idx := 0
	for idx < len(live) && live[idx].Role == RoleSystem {
		idx++
	}
	require.Less(t, idx, len(live), "live history must contain non-system messages after compaction")
	assert.Equal(t, RoleUser, live[idx].Role,
		"first non-system message after compaction must be RoleUser")

	// (v) Observability fields on the result: SupersededCount equals the
	// tagged rows re-archived, PriorSummaryCount is the one folded summary
	// row, and LiveMessages/LiveTokensEst mirror exactly what the
	// transaction made live (fresh system + summary + preserved tail).
	assert.Equal(t, int(taggedLiveBefore2), res2.SupersededCount,
		"SupersededCount must equal the tagged tail-copies superseded")
	assert.Equal(t, 1, res2.PriorSummaryCount,
		"exactly the prior compaction's summary row is prior-summary material")
	assert.Equal(t, len(live), res2.LiveMessages,
		"LiveMessages must equal the actual live row count from loadDBSessionMessages")
	// Phase E: LiveTokensEst is tokenizer-derived when a model is
	// configured (cfg.Model = "m" → o200k_base approximation), counted
	// over exactly the rows the transaction made live — the chars/4 sum
	// over Content is only the no-model fallback now.
	wantEst := countMessageTokens("m", messagesToChat(live)).Tokens
	assert.Greater(t, res2.LiveTokensEst, 0)
	assert.Equal(t, wantEst, res2.LiveTokensEst,
		"LiveTokensEst must equal countMessageTokens over the actual live rows (model configured)")
}

// TestCompactSession_FirstCompactionUnchanged pins the never-compacted
// path: with no prior summary row in the session, the summarizer request
// must contain no summary text and the resulting live history keeps the
// [freshSys, summary, tagged tail-copies] shape. (TestCompactSession_
// EndToEnd additionally proves the general first-compaction behavior.)
func TestCompactSession_FirstCompactionUnchanged(t *testing.T) {
	setupTestDB(t)

	stub, getBodies := newRecordingSummarizerStubServer(t, "FIRST_SUMMARY_TEXT")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
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

	bodies := getBodies()
	require.Len(t, bodies, 1)
	assert.NotContains(t, bodies[0], "[CONVERSATION SUMMARY",
		"a never-compacted session must not feed summary text to the summarizer")

	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(live), 3,
		"live history should contain fresh-system, summary, and tail rows")
	assert.Equal(t, RoleSystem, live[0].Role, "first live row should be the fresh system row")
	assert.Equal(t, RoleSystem, live[1].Role, "second live row should be the summary system row")
	assert.Contains(t, live[1].Content, "FIRST_SUMMARY_TEXT")
	for _, m := range live[2:] {
		require.NotNil(t, m.SourceCompactionID,
			"every preserved-tail row must be tagged with SourceCompactionID")
		assert.Equal(t, res.CompactionID, *m.SourceCompactionID)
	}
	idx := 0
	for idx < len(live) && live[idx].Role == RoleSystem {
		idx++
	}
	require.Less(t, idx, len(live))
	assert.Equal(t, RoleUser, live[idx].Role)
}

// TestCompactSession_AbortsWhenSessionGainsMessagesMidFlight is the core
// regression for the live-ordering scramble: a message inserted by
// AddMessage while the summarizer call is in flight used to land between
// the snapshot rows and the transaction's inserted system/summary/tail
// rows, corrupting the live ORDER BY id ASC stream. The optimistic guard
// inside the transaction must abort with ErrCompactionSessionChanged and
// leave the session completely untouched.
func TestCompactSession_AbortsWhenSessionGainsMessagesMidFlight(t *testing.T) {
	setupTestDB(t)

	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseNow := func() { releaseOnce.Do(func() { close(release) }) }

	stub, arrived := newBlockingSummarizerStubServer(t, release)
	defer stub.Close()
	// LIFO defer ordering: registered AFTER stub.Close() so it runs BEFORE
	// it on failure paths — stub.Close() blocks indefinitely while the
	// handler is still parked on the release channel, so releasing first
	// is what lets a failed require unwind cleanly instead of hanging.
	defer releaseNow()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 30 * time.Second},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 6; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 30 * time.Second, MaxTokens: 256}
	errCh := make(chan error, 1)
	go func() {
		_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
			SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
		}, cfg)
		errCh <- err
	}()

	// Wait until the snapshot is taken and the summarizer request is
	// blocked server-side, then inject traffic exactly like a chatty user.
	// Bounded: if CompactSession ever fails before issuing the HTTP
	// request, this receive must not park forever (no handler exists yet,
	// so the deferred stub.Close() below would not block anyway).
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("summarizer request never arrived")
	}
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "mid-flight user"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "mid-flight assistant"}))
	releaseNow()

	select {
	case err := <-errCh:
		require.ErrorIs(t, err, ErrCompactionSessionChanged,
			"compaction must abort when messages arrive mid-flight")
	case <-time.After(30 * time.Second):
		t.Fatal("CompactSession did not return after release")
	}

	// No compaction row was committed.
	comps, err := getCompactionsForSession(sid)
	require.NoError(t, err)
	assert.Empty(t, comps, "no compaction row should exist after the abort")

	// All rows remain live and in the original shape: nothing archived, no
	// tail-copy tags, no summary/fresh-system rows inserted, system row
	// still first, the two mid-flight rows last.
	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Len(t, live, 15, "13 original + 2 mid-flight rows, all live")
	assert.Equal(t, RoleSystem, live[0].Role, "original system row must still lead the live history")
	for _, m := range live {
		assert.False(t, m.Archived, "no row should be archived after the abort")
		assert.Nil(t, m.SourceCompactionID, "no tail-copy rows should exist after the abort")
		assert.NotContains(t, m.Content, "[CONVERSATION SUMMARY",
			"no summary row should have been inserted")
	}
	assert.Equal(t, "mid-flight user", live[len(live)-2].Content)
	assert.Equal(t, "mid-flight assistant", live[len(live)-1].Content)
}

// TestCompactSession_NoLeadingSystemRowRefuses covers the defensive state
// check: a session whose live history does not start with a system row
// (e.g. a legacy session already scrambled before the session-changed
// guard existed) must be refused with ErrCompactionNoSystemRow before
// anything is archived or summarized.
func TestCompactSession_NoLeadingSystemRowRefuses(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "model")
	for i := 0; i < 6; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
	}

	_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, AIConfig{Service: "svc", Model: "model", Timeout: time.Second})
	assert.ErrorIs(t, err, ErrCompactionNoSystemRow)

	// Nothing was archived or inserted.
	comps, compErr := getCompactionsForSession(sid)
	require.NoError(t, compErr)
	assert.Empty(t, comps)

	all, err := loadDBSessionMessagesAll(sid)
	require.NoError(t, err)
	require.Len(t, all, 6)
	for _, m := range all {
		assert.False(t, m.Archived, "no row should be archived after the refusal")
		assert.False(t, m.Superseded)
	}
}

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"under one token drops remainder", "abc", 0},
		{"exactly one token", "abcd", 1},
		{"longer text", strings.Repeat("x", 400), 100},
		{"counts runes not bytes", strings.Repeat("é", 8), 2}, // 8 runes = 16 bytes
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, estimateTokens(tc.in))
		})
	}
}

// TestSummarySizeRatio covers the ratio helper AND the WARN tripwire
// decision boundary (ratio >= summarySizeWarnRatio) — asserting the
// decision function, not log output.
func TestSummarySizeRatio(t *testing.T) {
	cases := []struct {
		name                 string
		prompt, completion   int
		want                 float64
		wantWarnTripsTrigger bool
	}{
		{"zero prompt guard", 0, 100, 0, false},
		{"negative prompt guard", -5, 100, 0, false},
		{"zero completion", 100, 0, 0, false},
		{"well under threshold", 1000, 100, 0.1, false},
		{"just under threshold", 100, 49, 0.49, false},
		{"at threshold trips", 100, 50, 0.5, true},
		{"over threshold trips", 100, 80, 0.8, true},
		{"echo-sized trips", 100, 100, 1.0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarySizeRatio(tc.prompt, tc.completion)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantWarnTripsTrigger, got >= summarySizeWarnRatio,
				"warn tripwire decision must match ratio >= summarySizeWarnRatio")
		})
	}
}

// TestCompactionNoticeVars pins every placeholder compactionNoticeVars
// fills, including the new aliases: the compaction's own numbers AND the
// last-turn TurnUsage numbers (kept for custom-template compatibility).
func TestCompactionNoticeVars(t *testing.T) {
	setupTestDB(t)

	res := &CompactionResult{
		ArchivedCount:     9,
		PromptTokens:      500,
		CompletionTokens:  60,
		DurationMs:        1234,
		SupersededCount:   7,
		PriorSummaryCount: 1,
		LiveMessages:      6,
		LiveTokensEst:     42,
		ContextBefore:     5000,
	}

	t.Run("with last turn usage", func(t *testing.T) {
		sid := createTestSession(t, "net", "#c", "u1", "cmd", "svc", "model")
		require.NoError(t, theDB.Create(&TurnUsage{
			SessionID:        sid,
			PromptTokens:     111,
			CompletionTokens: 22,
			CachedTokens:     33,
			ReasoningTokens:  44,
			APIPath:          "chat",
		}).Error)

		vars := compactionNoticeVars(res, sid)
		// Compaction's own summarizer-call numbers + result mirrors.
		assert.Equal(t, "9", vars["count"])
		assert.Equal(t, "500", vars["tokens_in"])
		assert.Equal(t, "60", vars["tokens_out"])
		assert.Equal(t, "60", vars["summary_tokens"], "summary_tokens aliases tokens_out")
		assert.Equal(t, "7", vars["superseded"])
		assert.Equal(t, "1", vars["prior_summaries"])
		assert.Equal(t, "6", vars["live_messages"])
		assert.Equal(t, "42", vars["live_tokens_est"])
		assert.Equal(t, "5000", vars["context_before"])
		assert.Equal(t, "42", vars["context_after"], "context_after aliases live_tokens_est")
		assert.Equal(t, "1234", vars["duration"])
		// Last chat turn's TurnUsage numbers stay filled (template compat).
		assert.Equal(t, "111", vars["prompt"])
		assert.Equal(t, "22", vars["completion"])
		assert.Equal(t, "133", vars["total"])
		assert.Equal(t, "33", vars["cached"])
		assert.Equal(t, "44", vars["reasoning"])
	})

	t.Run("without turn usage falls back to zero", func(t *testing.T) {
		sid := createTestSession(t, "net", "#c", "u2", "cmd", "svc", "model")
		vars := compactionNoticeVars(res, sid)
		assert.Equal(t, "0", vars["prompt"])
		assert.Equal(t, "0", vars["completion"])
		assert.Equal(t, "0", vars["total"])
		assert.Equal(t, "0", vars["cached"])
		assert.Equal(t, "0", vars["reasoning"])
		// Compaction's own numbers are unaffected.
		assert.Equal(t, "9", vars["count"])
		assert.Equal(t, "60", vars["summary_tokens"])
		assert.Equal(t, "42", vars["live_tokens_est"])
		assert.Equal(t, "5000", vars["context_before"])
		assert.Equal(t, "42", vars["context_after"])
	})
}

// TestCompactSession_RefusesWhenNothingNew covers the degenerate repeat
// compaction: when the 2/3 cut lands entirely on tail-copies from the
// prior compaction, there is no genuinely-new material and the event must
// be refused BEFORE any summarizer call is spent.
//
// Cut math for this seed: 12 turns → compaction #1 archives turns 1–8
// (16 msgs ≥ target 16) and preserves 4 turns = 8 tagged rows. Adding 2
// more turns gives live = [freshSys, summary, 8 tagged, u,a,u,a] = 12
// non-system rows; target = (12*2)/3 = 8 → the cut after the 4 tagged
// turns reaches exactly 8 → archived range is 100% tagged rows → count 0.
func TestCompactSession_RefusesWhenNothingNew(t *testing.T) {
	setupTestDB(t)

	stub, getBodies := newRecordingSummarizerStubServer(t, "SUMMARY_ONE_CONTENT")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
	}
	defer func() { config.Services = prevServices }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 12; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}

	// Compaction #1: succeeds, spends exactly one summarizer call.
	_, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)
	require.Len(t, getBodies(), 1)

	// Add exactly 2 more turns (4 fresh messages).
	for i := 0; i < 2; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "later"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "later-a"}))
	}

	// Compaction #2 must refuse without calling the summarizer.
	_, err = sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	assert.ErrorIs(t, err, ErrCompactionNothingNew)
	assert.Len(t, getBodies(), 1,
		"no second summarizer call may happen when there is nothing new to compact")

	// No second compaction event was recorded.
	comps, err := getCompactionsForSession(sid)
	require.NoError(t, err)
	assert.Len(t, comps, 1, "compactions table must still hold exactly the first event")

	// The refusal mutated nothing: live history is still freshSys +
	// summary + 8 tail-copies + the 4 fresh rows = 14 rows, all in the
	// same order compaction #1 left them in.
	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Len(t, live, 14)
	assert.Equal(t, RoleSystem, live[0].Role)
	assert.Equal(t, RoleSystem, live[1].Role)
	assert.Contains(t, live[1].Content, "SUMMARY_ONE_CONTENT")
}

// TestTokensPerMessage covers every estimate path, the 1.0 floor, the
// empty-input guard, and the tool-definition de-bias of the usage path.
// The usage path divides the API's own prompt_tokens (minus dave's
// tool-token estimate when one exists) by the number of live rows — real
// provider numbers stay PRIMARY over any tokenizer estimate. Without
// usage rows the fallback basis is the real tokenizer when a model is
// configured (basis "tokenizer:<enc>[,approx]") and chars/4 when not —
// both must remain reachable (the second is the never-fail floor).
// toolTokens only ever touches the usage path: the fallback paths never
// counted the tool prefix in the first place. (The maxHistory divisor
// cap died with live-path truncation, Oct 2026 — the divisor is now
// always the full live row count, matching what the next request sends.)
func TestTokensPerMessage(t *testing.T) {
	msgs := make([]Message, 10)
	for i := range msgs {
		msgs[i] = Message{Content: "filler"}
	}

	t.Run("usage path", func(t *testing.T) {
		perMsg, basis := tokensPerMessage(&TurnUsage{PromptTokens: 1000}, msgs, "", 0)
		assert.Equal(t, 100.0, perMsg)
		assert.Equal(t, "usage", basis)
	})

	t.Run("usage path wins over tokenizer", func(t *testing.T) {
		// Real API numbers for the provider beat any tokenizer estimate
		// — a configured model must not steal the basis from usage.
		perMsg, basis := tokensPerMessage(&TurnUsage{PromptTokens: 1000}, msgs, "gpt-4o", 0)
		assert.Equal(t, 100.0, perMsg)
		assert.Equal(t, "usage", basis)
	})

	t.Run("usage path subtracts tool tokens", func(t *testing.T) {
		// prompt 1000 with a 600-token tool prefix over 10 messages:
		// the messages themselves cost (1000-600)/10 = 40/msg — NOT the
		// amortized 100/msg that divides the constant tool prefix into
		// every message.
		perMsg, basis := tokensPerMessage(&TurnUsage{PromptTokens: 1000}, msgs, "", 600)
		assert.Equal(t, 40.0, perMsg)
		assert.Equal(t, "usage-net-tools", basis)
	})

	t.Run("de-bias floor at exactly effective", func(t *testing.T) {
		// net = 1000-990 = 10 = effective: the floor admits the
		// de-biased dividend exactly at the boundary → perMsg 1.0 via
		// the DE-BIASED path (legacy would report 100).
		perMsg, basis := tokensPerMessage(&TurnUsage{PromptTokens: 1000}, msgs, "", 990)
		assert.Equal(t, 1.0, perMsg)
		assert.Equal(t, "usage-net-tools", basis)
	})

	t.Run("tools ate nearly the whole prompt falls back to legacy division", func(t *testing.T) {
		// net = 1000-995 = 5 < effective 10: the subtraction cannot
		// yield a meaningful per-message average, so the legacy
		// prompt/effective division stands (perMsg 100, not 0.5).
		perMsg, basis := tokensPerMessage(&TurnUsage{PromptTokens: 1000}, msgs, "", 995)
		assert.Equal(t, 100.0, perMsg)
		assert.Equal(t, "usage-net-tools", basis,
			"basis stays self-describing: the log carries tool_tokens alongside for the reconstruction")
	})

	t.Run("tools >= prompt falls back to legacy division", func(t *testing.T) {
		// net = -200: the tool estimate ate the whole prompt outright
		// (provider under-counts tools or the estimate drifted) — fall
		// back to prompt/effective; never a nonsense sub-1.0 perMsg.
		perMsg, basis := tokensPerMessage(&TurnUsage{PromptTokens: 1000}, msgs, "", 1200)
		assert.Equal(t, 100.0, perMsg)
		assert.Equal(t, "usage-net-tools", basis)
	})

	t.Run("zero prompt tokens falls back", func(t *testing.T) {
		perMsg, basis := tokensPerMessage(&TurnUsage{PromptTokens: 0}, msgs, "", 0)
		// 10 rows of "filler" (7 runes → estimateTokens = 1 each).
		assert.Equal(t, 1.0, perMsg)
		assert.Equal(t, "chars/4", basis)
	})

	t.Run("tokenizer fallback basis exact", func(t *testing.T) {
		perMsg, basis := tokensPerMessage(nil, msgs, "gpt-4o", 0)
		assert.Greater(t, perMsg, 0.0)
		assert.Equal(t, "tokenizer:o200k_base", basis)
	})

	t.Run("tokenizer fallback basis approx for unknown model", func(t *testing.T) {
		perMsg, basis := tokensPerMessage(nil, msgs, "grok-4", 0)
		assert.Greater(t, perMsg, 0.0)
		assert.Equal(t, "tokenizer:o200k_base,approx", basis)
	})

	t.Run("tool tokens do not touch the fallback paths", func(t *testing.T) {
		// No usage rows → the tokenizer path never counted the tool
		// prefix; a non-zero toolTokens must not perturb it (there is
		// no tool-inflated dividend to de-bias).
		perMsg, basis := tokensPerMessage(nil, msgs, "gpt-4o", 5000)
		assert.Equal(t, "tokenizer:o200k_base", basis)
		want := float64(countMessageTokens("gpt-4o", messagesToChat(msgs)).Tokens) / float64(len(msgs))
		assert.InDelta(t, want, perMsg, 0.0001)

		perMsg, basis = tokensPerMessage(nil, msgs, "", 5000)
		assert.Equal(t, 1.0, perMsg)
		assert.Equal(t, "chars/4", basis)
	})

	t.Run("tokenizer fallback averages the full live history", func(t *testing.T) {
		// Live-path truncation is gone (Oct 2026): the next request sends
		// all 10 rows, so the estimate averages over all of them.
		perMsg, basis := tokensPerMessage(nil, msgs, "gpt-4o", 0)
		assert.Greater(t, perMsg, 0.0)
		assert.Equal(t, "tokenizer:o200k_base", basis)
		want := float64(countMessageTokens("gpt-4o", messagesToChat(msgs)).Tokens) / float64(len(msgs))
		assert.InDelta(t, want, perMsg, 0.0001)
	})

	t.Run("chars fallback averages estimateTokens", func(t *testing.T) {
		fallbackMsgs := []Message{
			{Content: strings.Repeat("x", 400)}, // 100 est
			{Content: strings.Repeat("y", 400)}, // 100 est
		}
		perMsg, basis := tokensPerMessage(nil, fallbackMsgs, "", 0)
		assert.Equal(t, 100.0, perMsg)
		assert.Equal(t, "chars/4", basis)
	})

	t.Run("floor at one token per message", func(t *testing.T) {
		perMsg, _ := tokensPerMessage(&TurnUsage{PromptTokens: 1}, msgs, "", 0)
		assert.Equal(t, 1.0, perMsg, "budget math must never see a sub-1.0 per-message cost")
	})

	t.Run("empty live history guards division", func(t *testing.T) {
		perMsg, basis := tokensPerMessage(&TurnUsage{PromptTokens: 500}, nil, "gpt-4o", 0)
		assert.Equal(t, 1.0, perMsg)
		assert.Equal(t, "chars/4", basis)
	})
}

// TestEffectiveContextWindow covers the command-first window cascade
// shared by ShouldAutoCompact and CompactSession's token-aware sizing:
// command context_window > service > [compaction] global > none.
func TestEffectiveContextWindow(t *testing.T) {
	prevCompaction := config.Compaction
	defer func() { config.Compaction = prevCompaction }()
	prevServices := config.Services
	defer func() { config.Services = prevServices }()

	cfg := AIConfig{Service: "svc"}

	config.Services = map[string]Service{"svc": {ContextWindow: 128000}}
	config.Compaction = CompactionConfig{ContextWindow: 64000}
	w, src := effectiveContextWindow(cfg)
	assert.Equal(t, 128000, w)
	assert.Equal(t, "service", src)

	// The command's own window wins over the service's — the window
	// varies per model more than per service.
	cmdCfg := cfg
	cmdCfg.ContextWindow = 32000
	w, src = effectiveContextWindow(cmdCfg)
	assert.Equal(t, 32000, w)
	assert.Equal(t, "command", src)

	config.Services = map[string]Service{"svc": {}}
	w, src = effectiveContextWindow(cfg)
	assert.Equal(t, 64000, w)
	assert.Equal(t, "compaction", src)

	config.Compaction = CompactionConfig{}
	w, src = effectiveContextWindow(cfg)
	assert.Equal(t, 0, w)
	assert.Equal(t, "none", src)

	// Unknown service behaves like an unset one.
	w, src = effectiveContextWindow(AIConfig{Service: "missing"})
	assert.Equal(t, 0, w)
	assert.Equal(t, "none", src)
}

// TestGetMessagesReturnsFullHistory pins the post-truncation-removal
// contract (Oct 2026): the request path carries the FULL live history —
// the maxhistory knob was deleted outright (display limits are
// sessions_display_limit's job, not a message-count request cap).
// Tool call/result pairs ride intact regardless of window position.
func TestGetMessagesReturnsFullHistory(t *testing.T) {
	setupTestDB(t)
	sid := createTestSession(t, "testnet", "#test", "shrew", "testchat", "svc", "m")

	insertTestMessage(t, sid, "system", "sys")
	for i := 0; i < 30; i++ {
		insertTestMessage(t, sid, "user", fmt.Sprintf("msg %d", i))
	}
	// A tool pair at the "old window edge" — must never be split.
	callJSON := `[{"id":"tc-edge","type":"function","function":{"name":"job_status","arguments":"{}"}}]`
	require.NoError(t, theDB.Exec(`INSERT INTO messages (session_id, role, content, tool_calls, created_at)
		VALUES (?, 'assistant', '', ?, datetime('now'))`, sid, callJSON).Error)
	require.NoError(t, theDB.Exec(`INSERT INTO messages (session_id, role, content, tool_call_id, created_at)
		VALUES (?, 'tool', 'result', 'tc-edge', datetime('now'))`, sid).Error)

	msgs, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 33, "full live history expected — no message-count clipping")

	// The pair is adjacent and paired.
	pairIdx := -1
	for i, m := range msgs {
		if m.ToolCallID == "tc-edge" {
			pairIdx = i
		}
	}
	require.GreaterOrEqual(t, pairIdx, 1)
	assert.Len(t, msgs[pairIdx-1].ToolCalls, 1)
	assert.Equal(t, "tc-edge", msgs[pairIdx-1].ToolCalls[0].ID)
}

// TestCompactionConfigApplyDefaultsTargetFraction pins the defaulting of
// the token-aware sizing fraction: 0 (TOML unset) and negatives map to
// 0.4, legitimate fractions pass through, and the >= 1.0 disable
// sentinel passes through UNTOUCHED (defaulting it would make the
// feature impossible to turn off).
func TestCompactionConfigApplyDefaultsTargetFraction(t *testing.T) {
	cases := []struct {
		name string
		in   float64
		want float64
	}{
		{"unset defaults to 0.4", 0, 0.4},
		{"negative defaults to 0.4", -0.5, 0.4},
		{"default value passes through", 0.4, 0.4},
		{"custom fraction passes through", 0.75, 0.75},
		{"disable sentinel 1.0 passes through", 1.0, 1.0},
		{"disable sentinel >1.0 passes through", 2.5, 2.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := CompactionConfig{TargetFraction: tc.in}
			c.ApplyDefaults()
			assert.Equal(t, tc.want, c.TargetFraction)
		})
	}
}

// TestAdvanceCutForTokenBudget exercises the cut-advancement loop in
// isolation: budgets force advancing, advancement stops as soon as the
// projection fits, unreachable budgets still leave one tail turn,
// non-User boundaries are skipped, and disabled budgets are no-ops.
func TestAdvanceCutForTokenBudget(t *testing.T) {
	// system + 6 user/assistant turns = 13 messages, 7 turns.
	msgs := makeChatMessages(13)
	turns := buildTurns(msgs)
	// turns: t0=[0,1) t1=[1,3) t2=[3,5) t3=[5,7) t4=[7,9) t5=[9,11) t6=[11,13)
	// Projection at cut c (perMsg=10, summary=100):
	//   100 + 10*(1 + (13 - turns[c].end))
	//   cut3 → 100+10*7=170, cut4 → 100+10*5=150, cut5 → 100+10*3=130.

	t.Run("disabled budget returns cut unchanged", func(t *testing.T) {
		assert.Equal(t, 3, advanceCutForTokenBudget(msgs, turns, 3, 0, 10, 100))
		assert.Equal(t, 3, advanceCutForTokenBudget(msgs, turns, 3, -5, 10, 100))
	})

	t.Run("projection already fits returns cut unchanged", func(t *testing.T) {
		assert.Equal(t, 3, advanceCutForTokenBudget(msgs, turns, 3, 170, 10, 100))
	})

	t.Run("budget forces advancing and stops as soon as it fits", func(t *testing.T) {
		// cut3 projects 170 > 150 → advance; cut4 projects 150 <= 150 → stop.
		assert.Equal(t, 4, advanceCutForTokenBudget(msgs, turns, 3, 150, 10, 100))
	})

	t.Run("unreachable budget keeps one tail turn", func(t *testing.T) {
		// Even the smallest legal tail (turn 6 alone) projects 130, but
		// cut may never reach len(turns)-1 = 6.
		final := advanceCutForTokenBudget(msgs, turns, 3, 50, 10, 100)
		assert.Equal(t, 5, final)
		assert.Equal(t, 1, len(turns)-1-final, "at least one tail turn must survive")
	})

	t.Run("non-user boundaries are skipped", func(t *testing.T) {
		// Hand-built turns with async-injected RoleSystem rows at two
		// consecutive turn heads: advancing from cut 1 must skip the
		// boundaries at idx 5 and idx 7 (tails would start with
		// RoleSystem) and land on cut 4 (tail starts at idx 9 = u3).
		skipMsgs := []ChatMessage{
			{Role: RoleSystem, Content: "sys"},       // 0
			{Role: RoleUser, Content: "u1"},          // 1
			{Role: RoleAssistant, Content: "a1"},     // 2
			{Role: RoleUser, Content: "u2"},          // 3
			{Role: RoleAssistant, Content: "a2"},     // 4
			{Role: RoleSystem, Content: "injected"},  // 5  ← boundary role: system
			{Role: RoleAssistant, Content: "a3"},     // 6
			{Role: RoleSystem, Content: "injected2"}, // 7  ← boundary role: system
			{Role: RoleAssistant, Content: "a4"},     // 8
			{Role: RoleUser, Content: "u3"},          // 9
			{Role: RoleAssistant, Content: "a5"},     // 10
		}
		skipTurns := []messageTurn{
			{start: 0, end: 1},
			{start: 1, end: 3}, // end→3 user ✓
			{start: 3, end: 5}, // end→5 system ✗
			{start: 5, end: 7}, // end→7 system ✗
			{start: 7, end: 9}, // end→9 user ✓
			{start: 9, end: 11},
		}
		// Projection at cut1 (perMsg=100, summary=0): 100*(1+8)=900 > 500.
		// cut4 projects 100*(1+2)=300 <= 500 → stop.
		final := advanceCutForTokenBudget(skipMsgs, skipTurns, 1, 500, 100, 0)
		assert.Equal(t, 4, final,
			"must skip the two non-user boundaries and land on the first valid one")
		boundary := skipTurns[final].end
		require.Less(t, boundary, len(skipMsgs))
		assert.Equal(t, RoleUser, skipMsgs[boundary].Role,
			"advanced cut's tail must still begin with RoleUser")
	})

	t.Run("never retreats below the given cut", func(t *testing.T) {
		// A generous budget with a cut already past the 2/3 point must
		// not move the cut backward.
		assert.Equal(t, 4, advanceCutForTokenBudget(msgs, turns, 4, 100000, 10, 100))
	})
}

// TestCompactSession_TokenAwareTailShrinks is the end-to-end Phase D
// check: when the 2/3-rule preserved tail busts the configured budget,
// the cut advances (archived range grows, tail shrinks) while every
// structural invariant holds.
//
// Seed: system + 12 turns = 25 rows. TurnUsage PromptTokens=10000 →
// perMsg = 10000/25 = 400 ("usage" basis). Window = 8000 (compaction
// fallback; the stub service sets none), TargetFraction 0.4 →
// budget = 3200. Assumed summary = 512 (default).
//
// 2/3 cut math (unchanged rule): totalNonSystem = 24, target = 16 →
// base cut = 8 (turns 1–8, 16 msgs), tail = 8 msgs projecting
// 512 + 400*9 = 4112 > 3200.
//
// Advancement: cut 9 → tail 6 → 512+400*7 = 3312 > 3200; cut 10 →
// tail 4 → 512+400*5 = 2512 <= 3200 → stop. Final cut = 10: archived
// range = turns 1–10 = 20 msgs (idx 1–20), tail = turns 11–12 = 4 msgs.
func TestCompactSession_TokenAwareTailShrinks(t *testing.T) {
	setupTestDB(t)

	stub, getBodies := newRecordingSummarizerStubServer(t, "TOKEN_AWARE_SUMMARY")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
	}
	defer func() { config.Services = prevServices }()

	prevCompaction := config.Compaction
	config.Compaction = CompactionConfig{
		Enabled:        true,
		MinTurns:       3,
		ContextWindow:  8000,
		TargetFraction: 0.4,
	}
	defer func() { config.Compaction = prevCompaction }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 12; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}
	require.NoError(t, theDB.Create(&TurnUsage{SessionID: sid, PromptTokens: 10000, APIPath: "chat"}).Error)

	original, err := loadDBSessionMessagesAll(sid)
	require.NoError(t, err)
	require.Len(t, original, 25)

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	res, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "auto",
	}, cfg)
	require.NoError(t, err)

	// The archived range is LARGER than the 2/3 cut alone (2/3 would
	// archive 16 msgs; token-aware sizing archives 20).
	assert.Equal(t, 16, (24*2)/3, "seed sanity: 2/3 of 24 non-system msgs is 16")
	assert.Equal(t, 20, res.ArchivedCount,
		"token-aware sizing must archive more than the 2/3 cut (16) when the tail busts the budget")
	assert.Equal(t, original[1].ID, res.FirstArchivedID)
	assert.Equal(t, original[20].ID, res.LastArchivedID,
		"archived range must extend through turn 10 (row idx 20), not the 2/3 boundary (idx 16)")

	// Tail is smaller: fresh system + summary + 4 tail copies = 6 live rows.
	assert.Equal(t, 6, res.LiveMessages)
	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Len(t, live, 6)
	assert.Equal(t, RoleSystem, live[0].Role)
	assert.Equal(t, RoleSystem, live[1].Role)
	assert.Contains(t, live[1].Content, "TOKEN_AWARE_SUMMARY")
	tailCopies := 0
	for _, m := range live[2:] {
		require.NotNil(t, m.SourceCompactionID)
		assert.Equal(t, res.CompactionID, *m.SourceCompactionID)
		tailCopies++
	}
	assert.Equal(t, 4, tailCopies, "preserved tail must be the 4 messages of turns 11–12")

	// Structural invariant survives the advancement: first non-system
	// live message is RoleUser.
	idx := 0
	for idx < len(live) && live[idx].Role == RoleSystem {
		idx++
	}
	require.Less(t, idx, len(live))
	assert.Equal(t, RoleUser, live[idx].Role)

	// ContextBefore carries the real last-turn prompt tokens ("usage").
	assert.Equal(t, 10000, res.ContextBefore)

	require.Len(t, getBodies(), 1)
}

// TestCompactSession_ToolTokensDeBiasTailSizing is the end-to-end Phase G
// check: the tool-definition prefix riding every provider prompt must NOT
// be amortized into per-message cost. The config carries real tool
// definitions via a seeded mcpServers fixture (toolDefsForConfig: the 2
// fixture MCP tools + the 3 builtins), so dave's own countToolTokens
// estimate T > 0 is subtracted from the last turn's prompt_tokens before
// the division — T is COMPUTED in the test over the exact tool set
// (never hardcoded), so the seed tracks any drift in builtin tool
// descriptions.
//
// Seed: system + 12 turns = 25 rows (the divisor is the full live row
// count since live-path truncation was removed, Oct 2026). TurnUsage
// PromptTokens P = 2500:
//
//	AMORTIZED perMsg = P/25     = 100/msg     (pre-Phase-G behavior)
//	DE-BIASED perMsg = (P-T)/25 = 100-T/25    (Phase G, basis
//	                                        "usage-net-tools")
//
// 2/3 cut (unchanged rule): totalNonSystem = 24, target = 16 → base
// cut = 8 (turns 1–8, 16 msgs), tail = 8 msgs. Projection at base cut =
// summaryAssumed(512) + perMsg*(1+8) = 512 + 9*perMsg.
//
// The budget (window × 0.4, [compaction] fallback window) is pinned at
// the MIDPOINT of the two base-cut projections, so decisively:
//
//	512 + 9*(100-T/25)  <=  budget  <  512 + 9*100
//
// Under the AMORTIZED perMsg the base-cut projection busts the budget →
// the pre-fix advancement loop would move the cut (as in
// TestCompactSession_TokenAwareTailShrinks). Under the DE-BIASED perMsg
// it fits → NO advancement: the archived range must equal the 2/3 base
// cut exactly (16 msgs, idx 1–16; 10 live rows).
func TestCompactSession_ToolTokensDeBiasTailSizing(t *testing.T) {
	setupTestDB(t)

	// Same fixture shape as TestToolDefsForConfig: seed the mcpServers
	// map directly — getMCPTools reads only that in-memory map (no MCP
	// I/O), so no live server or transport is needed. Mutex-guarded for
	// -race hygiene, per TestRegisterBackgroundJob's pattern.
	mcpServersMu.Lock()
	origServers := mcpServers
	mcpServers = map[string]*MCPServer{"img-mcp": {
		Tools: []*mcp.Tool{
			{Name: "generate_image", Description: "Generate an image"},
			{Name: "get_transcript", Description: "Fetch a transcript"},
		},
	}}
	mcpServersMu.Unlock()
	t.Cleanup(func() {
		mcpServersMu.Lock()
		mcpServers = origServers
		mcpServersMu.Unlock()
	})

	stub, getBodies := newRecordingSummarizerStubServer(t, "DEBIAS_SUMMARY")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
	}
	defer func() { config.Services = prevServices }()

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256, MCPs: []string{"img-mcp"}}

	// T: dave's own count of the EXACT tool set the next request
	// serializes (2 fixture MCP tools + the 3 builtins).
	toolDefs := toolDefsForConfig(cfg)
	require.Len(t, toolDefs, 5, "2 fixture MCP tools + 3 builtins")
	toolEncName, _ := resolveEncodingForModel(cfg.Model)
	toolEnc, err := getEncoder(toolEncName)
	require.NoError(t, err)
	T := countToolTokens(toolEnc, toolDefs)
	require.Greater(t, T, 0, "fixture must contribute a positive tool-token count")

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 12; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}
	const promptTokens = 2500
	require.NoError(t, theDB.Create(&TurnUsage{SessionID: sid, PromptTokens: promptTokens, APIPath: "chat"}).Error)

	original, err := loadDBSessionMessagesAll(sid)
	require.NoError(t, err)
	require.Len(t, original, 25)

	// Hand-computed budget math (see the function comment): pin the
	// budget at the midpoint of the two base-cut projections, realized
	// as [compaction] window × 0.4. int() truncation lowers the derived
	// budget by < 0.4 tokens — far inside the margin (the gap halves at
	// the midpoint), which the two require.True calls below pin exactly.
	const (
		effective        = 25
		summaryAssumed   = 512 // defaultSummaryTokensAssumed (no max_summary_tokens set)
		baseTailMessages = 8
	)
	require.GreaterOrEqual(t, promptTokens-T, effective,
		"seed sanity: the de-biased dividend must clear the effective floor so CompactSession takes the de-bias path, not the legacy fallback")
	amortizedPerMsg := float64(promptTokens) / effective
	deBiasedPerMsg := float64(promptTokens-T) / effective
	amortizedProjection := float64(summaryAssumed) + float64(1+baseTailMessages)*amortizedPerMsg
	deBiasedProjection := float64(summaryAssumed) + float64(1+baseTailMessages)*deBiasedPerMsg
	require.Greater(t, amortizedProjection-deBiasedProjection, 10.0,
		"seed sanity: the fixture's tool tokens must move the projection by a decisive margin")
	window := int((amortizedProjection + deBiasedProjection) / 2 / 0.4)
	budget := float64(window) * 0.4
	require.True(t, deBiasedProjection <= budget,
		"de-biased base-cut projection (%.1f) must fit the budget (%.1f) — no advancement", deBiasedProjection, budget)
	require.True(t, budget < amortizedProjection,
		"amortized base-cut projection (%.1f) must bust the budget (%.1f) — the pre-fix code would have advanced", amortizedProjection, budget)

	prevCompaction := config.Compaction
	config.Compaction = CompactionConfig{
		Enabled:        true,
		MinTurns:       3,
		TargetFraction: 0.4,
		ContextWindow:  window,
	}
	defer func() { config.Compaction = prevCompaction }()

	res, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "auto",
	}, cfg)
	require.NoError(t, err)

	// Final archived range is EXACTLY the 2/3 base cut: the de-biased
	// perMsg fits the budget at cut 8, so the cut never advanced (the
	// amortized perMsg would have, TokenAwareTailShrinks-style).
	assert.Equal(t, 16, res.ArchivedCount,
		"de-biased perMsg must NOT advance the cut when the message-only tail fits the budget")
	assert.Equal(t, original[1].ID, res.FirstArchivedID)
	assert.Equal(t, original[16].ID, res.LastArchivedID,
		"archived range must end exactly at the 2/3 boundary (row idx 16)")

	// fresh system + summary + the 8 tail copies = 10 live rows.
	assert.Equal(t, 10, res.LiveMessages)
	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Len(t, live, 10)
	assert.Equal(t, RoleSystem, live[0].Role)
	assert.Equal(t, RoleSystem, live[1].Role)
	assert.Contains(t, live[1].Content, "DEBIAS_SUMMARY")
	tailCopies := 0
	for _, m := range live[2:] {
		require.NotNil(t, m.SourceCompactionID)
		tailCopies++
	}
	assert.Equal(t, 8, tailCopies, "preserved tail must be the 8 messages of turns 9–12")

	// ContextBefore still reports the RAW provider prompt (tools
	// included) — the de-bias shapes the sizing decision only.
	assert.Equal(t, promptTokens, res.ContextBefore)

	require.Len(t, getBodies(), 1)
}

// TestCompactSession_NoWindowKeepsTwoThirdsCut pins the inert path: with
// no resolvable context window (service 0, compaction fallback 0) the
// budget is disabled and the archived range is EXACTLY the 2/3 cut —
// even when usage rows exist that could otherwise drive sizing.
//
// Cut math for the seed: system + 12 turns; totalNonSystem = 24,
// target = (24*2)/3 = 16 → cut = 8 → archived range = rows idx 1–16
// (16 msgs), preserved tail = turns 9–12 = 8 msgs.
func TestCompactSession_NoWindowKeepsTwoThirdsCut(t *testing.T) {
	setupTestDB(t)

	stub, getBodies := newRecordingSummarizerStubServer(t, "PLAIN_SUMMARY")
	defer stub.Close()
	prevServices := config.Services
	config.Services = map[string]Service{
		"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
	}
	defer func() { config.Services = prevServices }()

	prevCompaction := config.Compaction
	config.Compaction = CompactionConfig{
		Enabled:        true,
		MinTurns:       3,
		TargetFraction: 0.4, // active fraction, but no window to multiply
	}
	defer func() { config.Compaction = prevCompaction }()

	sid := createTestSession(t, "net", "#c", "u1", "cmd", "stubsvc", "stubmodel")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	for i := 0; i < 12; i++ {
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "u"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "a"}))
	}
	// Usage rows alone must not trigger sizing — no window, no budget.
	require.NoError(t, theDB.Create(&TurnUsage{SessionID: sid, PromptTokens: 999999, APIPath: "chat"}).Error)

	original, err := loadDBSessionMessagesAll(sid)
	require.NoError(t, err)
	require.Len(t, original, 25)

	cfg := AIConfig{Service: "stubsvc", Model: "m", Timeout: 5 * time.Second, MaxTokens: 256}
	res, err := sessionMgr.CompactSession(context.Background(), CompactSessionInputs{
		SessionID: sid, Network: Network{Name: "net"}, Channel: "#c", UserNick: "u1", Trigger: "manual",
	}, cfg)
	require.NoError(t, err)

	// Exactly the 2/3 cut: 16 archived, 8-message tail, 10 live rows.
	assert.Equal(t, 16, res.ArchivedCount)
	assert.Equal(t, original[1].ID, res.FirstArchivedID)
	assert.Equal(t, original[16].ID, res.LastArchivedID,
		"archived range must end exactly at the 2/3 boundary (row idx 16)")
	assert.Equal(t, 10, res.LiveMessages)
	assert.Equal(t, 999999, res.ContextBefore)

	live, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Len(t, live, 10)
	require.Len(t, getBodies(), 1)
}

// TestCompactSession_MaxSummaryTokensCapsRequest verifies the
// max_summary_tokens plumbing end to end: with the cap configured, the
// summarizer request's max_tokens JSON field equals the cap (overriding
// the command's maxtokens); with it unset (0), the command's value is
// inherited.
func TestCompactSession_MaxSummaryTokensCapsRequest(t *testing.T) {
	maxTokensFromBody := func(t *testing.T, body string) float64 {
		t.Helper()
		var parsed map[string]interface{}
		require.NoError(t, json.Unmarshal([]byte(body), &parsed))
		v, ok := parsed["max_tokens"]
		require.True(t, ok, "request must carry max_tokens: %s", body)
		f, ok := v.(float64)
		require.True(t, ok, "max_tokens must be numeric: %v", v)
		return f
	}

	run := func(t *testing.T, ccfg CompactionConfig, want float64) {
		setupTestDB(t)

		stub, getBodies := newRecordingSummarizerStubServer(t, "CAPPED_SUMMARY")
		defer stub.Close()
		prevServices := config.Services
		config.Services = map[string]Service{
			"stubsvc": {BaseURL: stub.URL, Timeout: 5 * time.Second},
		}
		defer func() { config.Services = prevServices }()

		prevCompaction := config.Compaction
		config.Compaction = ccfg
		defer func() { config.Compaction = prevCompaction }()

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

		bodies := getBodies()
		require.Len(t, bodies, 1)
		assert.Equal(t, want, maxTokensFromBody(t, bodies[0]))
	}

	t.Run("cap overrides command maxtokens", func(t *testing.T) {
		run(t, CompactionConfig{Enabled: true, MinTurns: 3, MaxSummaryTokens: 777}, 777)
	})

	t.Run("unset inherits command maxtokens", func(t *testing.T) {
		run(t, CompactionConfig{Enabled: true, MinTurns: 3}, 256)
	})
}
