package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mkToolPairFixture builds an assistant view carrying calls and tool
// rows, the shapes pairToolCalls consumes.
func mkToolPairFixture() []MessageView {
	av := MessageView{ID: 1, Role: "assistant", Content: "thinking out loud",
		ToolCalls: []ToolCallView{
			{ID: "call_a", Name: "generate_image", ArgsJSON: `{"prompt": "a shrew"}`},
			{ID: "call_b", Name: "fetch", ArgsJSON: `{"url": "https://x.example"}`},
		}}
	tv1 := MessageView{ID: 2, Role: "tool", ToolCallID: "call_a", Content: "IMAGE-DONE"}
	tv2 := MessageView{ID: 3, Role: "tool", ToolCallID: "call_b", Content: "FETCHED-TEXT"}
	return []MessageView{av, tv1, tv2}
}

func TestPairToolCallsPairsByCallID(t *testing.T) {
	views := pairToolCalls(mkToolPairFixture())

	require.Len(t, views, 1, "consumed tool rows drop from the transcript")
	assert.Equal(t, "assistant", views[0].Role)
	require.Len(t, views[0].ToolCalls, 2)
	require.NotNil(t, views[0].ToolCalls[0].Result, "call_a pairs")
	assert.Equal(t, "IMAGE-DONE", views[0].ToolCalls[0].Result.Content)
	require.NotNil(t, views[0].ToolCalls[1].Result, "call_b pairs")
	assert.Equal(t, "FETCHED-TEXT", views[0].ToolCalls[1].Result.Content)
}

func TestPairToolCallsOrphanCallStays(t *testing.T) {
	views := []MessageView{{ID: 1, Role: "assistant",
		ToolCalls: []ToolCallView{{ID: "call_a", Name: "fetch", ArgsJSON: "{}"}}}}

	got := pairToolCalls(views)
	require.Len(t, got, 1)
	assert.Nil(t, got[0].ToolCalls[0].Result, "no result landed — call renders unpaired")
}

func TestPairToolCallsOrphanResultStays(t *testing.T) {
	// A page starting mid-round: the call row is on an older chunk.
	views := []MessageView{{ID: 9, Role: "tool", ToolCallID: "call_x", Content: "ORPHAN"}}

	got := pairToolCalls(views)
	require.Len(t, got, 1, "orphan result renders as the standalone row")
	assert.Equal(t, "tool", got[0].Role)
}

func TestPairToolCallsOnlyPairsForward(t *testing.T) {
	// A result BEFORE its call (never healthy, but the pass must not
	// pair backwards and silently eat the row).
	views := []MessageView{
		{ID: 1, Role: "tool", ToolCallID: "call_a", Content: "EARLY"},
		{ID: 2, Role: "assistant", ToolCalls: []ToolCallView{{ID: "call_a", Name: "fetch"}}},
	}
	got := pairToolCalls(views)
	require.Len(t, got, 2, "nothing pairs — both render standalone")
	assert.Nil(t, got[1].ToolCalls[0].Result)
}

func TestPairToolCallsDuplicateCallIDFirstWins(t *testing.T) {
	// Duplicate ids never occur in production (call ids are unique per
	// response) but the pending map's documented contract is
	// first-wins: the second occurrence stays unpairable.
	views := []MessageView{
		{ID: 1, Role: "assistant", ToolCalls: []ToolCallView{
			{ID: "call_a", Name: "first"},
			{ID: "call_a", Name: "second"},
		}},
		{ID: 2, Role: "tool", ToolCallID: "call_a", Content: "RESULT"},
	}
	got := pairToolCalls(views)
	require.Len(t, got, 1)
	assert.NotNil(t, got[0].ToolCalls[0].Result, "first call wins the result")
	assert.Nil(t, got[0].ToolCalls[1].Result, "duplicate id never pairs")
}

// TestRenderRowsToolPairTemplate: through rows.html a paired call
// renders as ONE collapsed unit (params + result nested), the consumed
// tool row is gone, and an orphan result keeps the standalone shape.
// data-callid stamps every shape — the client replay filter keys on
// them (session.js toolpair replay filter).
func TestRenderRowsToolPairTemplate(t *testing.T) {
	paired := mkToolPairFixture()
	orphan := MessageView{ID: 9, Role: "tool", ToolCallID: "call_z", Content: "ORPHAN-RESULT"}

	html, err := renderRowsHTML(pairToolCalls(append(paired, orphan)), nil)
	require.NoError(t, err)
	frag := string(html)

	assert.Contains(t, frag, `details class="toolpair" data-callid="call_a"`)
	assert.Contains(t, frag, "generate_image")
	assert.Contains(t, frag, `details class="params"`)
	assert.Contains(t, frag, `details class="result"`)
	assert.Contains(t, frag, "IMAGE-DONE")
	assert.Equal(t, 1, strings.Count(frag, `class="msg role-tool"`), "only the orphan result keeps a tool row")
	assert.Contains(t, frag, "ORPHAN-RESULT")
	assert.Contains(t, frag, `data-callid="call_z"`, "orphan result carries its call id")
}

// TestRenderRowsAfterPairsToolRound: end-to-end wiring — the SSE render
// path pairs a call+result span into one toolpair unit and drops the
// standalone row; a result-only span (call in an earlier batch) stays
// an orphan row, which the client may legitimately need to append.
func TestRenderRowsAfterPairsToolRound(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	s := mkSession(t, db, "aaaaaaaa")
	tc := `[{"id":"call_a","function":{"name":"fetch","arguments":"{}"}}]`
	call := mkMessage(t, db, s.ID, "assistant", "working on it", func(m *Message) { m.ToolCalls = &tc })
	result := mkMessage(t, db, s.ID, "tool", "FETCHED", func(m *Message) { id := "call_a"; m.ToolCallID = &id })

	frag, through, err := srv.renderRowsAfter(s, call.ID-1)
	require.NoError(t, err)
	assert.Equal(t, result.ID, through)
	assert.Contains(t, frag, `toolpair" data-callid="call_a"`)
	assert.Contains(t, frag, "FETCHED")
	assert.NotContains(t, frag, `class="msg role-tool"`)

	orphanFrag, _, err := srv.renderRowsAfter(s, result.ID-1)
	require.NoError(t, err)
	assert.Contains(t, orphanFrag, `class="msg role-tool"`, "result-only span stays an orphan row")
	assert.Contains(t, orphanFrag, "FETCHED")
}
