# Generators: Tool-Driven Retrieval, Output, and Logging Parity — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the generators feature's pre-built transcript injection with a model-driven `query_channel_logs` tool, quiet ephemeral output (suppression + config-gated `respond` tool), and per-run api-log sessions.

**Architecture:** The generator executor (`generators.go`) stops fetching/injecting transcripts and instead arms the ephemeral `chatRunner` with a log-query context; two ephemeral-only tools (`query_channel_logs`, `respond`) dispatch through a dedicated branch in `executeToolCalls`. Intermediate model text never auto-sends on ephemeral turns; streaming defers emission to the iteration's classification. Every ephemeral run gets a unique negative api-log session id so its API traffic lands in its own `api_logs/` file. Host-side caps (per-call token budget, row cap, events, 20-iteration loop bound) are unchanged.

**Tech Stack:** Go 1.25, testify, BurntSushi/toml-config, openai-go SDK, logxi, httptest SSE servers.

**Spec:** `docs/superpowers/specs/2026-10-09-generators-tool-driven-design.md` (binding; amended 2026-10-09: streaming on ephemeral turns defers emission). The v1 spec `2026-10-09-generators-design.md` is superseded history for retrieval/output/logging; its config/registration/dispatch/queue model is unchanged and still binding.

## Global Constraints

- Go 1.25; all root `.go` files are `package main` (module `github.com/knivey/dave`).
- Test framework: `github.com/stretchr/testify` (`assert`/`require`); table-driven tests with `t.Run()` where the house style uses them.
- `go build ./... && go vet ./... && go fmt ./... && go test ./...` MUST pass before every commit.
- NEVER use `git add -f` — `.superpowers/` and other ignored paths stay untracked.
- No LSP server exists; per-file type-check with `gopls check <file>.go` if needed.
- Duration grammar for windows: `^(?:\d+[smhd])+$` (compound allowed, `d` = 24h); parsed by `parseWindowDuration` (logquery.go:195); empty means unset (defaulted by `applyLogQueryDefaults`), zero/negative/invalid is an error.
- The apiLogger's `sessionID == 0` no-op guards (apiLog.go:87,111,124,137,150,175,188) are UNTOUCHED — zero keeps meaning "no session attached"; ephemeral traffic flows through unique negative ids instead.
- `chatRunner.sessionID` stays 0 on ephemeral runs — load-bearing for `storeUsage` attribution (SessionID-0 `turn_usage` rows) and other ephemeral gates.
- Non-ephemeral (chat/completion/tools/job) behavior must be byte-identical; existing pins (`TestRunTurnStreamToolCallSendsStreamedTextOnce`, `TestToolDefsForConfig`, etc.) must stay green without modification unless a task says to extend them.
- Preserve block comments explaining design decisions (house rule); new non-obvious code gets them.
- Queue semantics explicitly unchanged (owner-confirmed): generators stay ordinary queue citizens.
- `hidden_tools` (root-only, config.go:641 default) gains the new tool names; `disabled_builtin_tools` cascade (command > service > root, rides AIConfig which GeneratorConfig embeds) applies to both new tools.

## Review Focus

The five input classes / failure modes most likely to bite a person using this software (spec-implied, not all directly tested below without pinning):

1. **A hallucinated `query_channel_logs`/`respond` call on a non-ephemeral turn** (the model saw the tool in another command's context) must produce an error tool result, not a panic or silent success. → `TestHandleGeneratorLogQueryGuardNonEphemeral` (Task 2), `TestHandleGeneratorRespondGuardNonEphemeral` (Task 4).
2. **Two concurrent generator runs** must land in distinct api-log files with no cross-run bleed (negative-id uniqueness; bare-sessionID map stays correct). → `TestEphemeralAPILogPerRunFiles` (Task 1).
3. **`respond` called alongside other tools in one iteration**: calls execute in order, the text is sent exactly once, the remaining calls still execute, the turn ends with no second API request. → `TestRunTurnRespondEndsTurnSingleRequest` (Task 4).
4. **A streaming generator whose stream dies mid-flight** must deliver whatever text arrived before the error notice (deferred-emission parity), and must not misbehave when zero text arrived. → `TestRunTurnStreamEphemeralErrorDeliversPartialText` (Task 5).
5. **Bare `^summary` (no args)** must still dispatch (optional-args) and give the model a sane user message (`prompt` fallback) with the tool defaulting the window. → `TestGeneratorNoArgsUsesConfiguredPrompt` (Task 3).
6. **A stale notices.toml still carrying a `[generators]` section** must load fine (unknown TOML keys are ignored). → `TestNoticesLoadWithStaleGeneratorsSection` (Task 6).

---

### Task 1: api-log identity — per-run negative ids and the runner seam

**Files:**
- Modify: `aiCmds.go` (chatRunner fields ~line 237; `syncAPISessionID` ~line 321; chat() breadcrumb line 1890)
- Modify: `apiLog.go` (allocator; imports)
- Modify: `incident.go:178` (one call-site argument)
- Test: `apiLog_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces (used by Task 3): `func nextEphemeralAPILogID() int64`; `chatRunner.apiLogSessionID int64` field; `func (cr *chatRunner) apiLogID() int64`; `syncAPISessionID` now logging under `apiLogID()`.

- [ ] **Step 1: Write the failing tests**

Append to `apiLog_test.go` (uses existing imports `os`, `time`, testify; add `"sync/atomic"` only if not present — the tests use `ephemeralAPILogSeq` directly for the cross-boot case):

```go
func TestNextEphemeralAPILogIDUniqueNegative(t *testing.T) {
	seen := make(map[int64]bool)
	for i := 0; i < 100; i++ {
		id := nextEphemeralAPILogID()
		assert.Negative(t, id, "ids must never collide with DB session ids (>= 1)")
		assert.False(t, seen[id], "id %d repeated within a boot", id)
		seen[id] = true
	}
}

func TestNextEphemeralAPILogIDCrossBoot(t *testing.T) {
	// Simulate a restart: re-seed the counter as a later boot would and
	// confirm the new ids never repeat the old boot's (clock seed differs).
	old := map[int64]bool{
		nextEphemeralAPILogID(): true,
		nextEphemeralAPILogID(): true,
	}
	ephemeralAPILogSeq.Store(time.Now().UnixNano() + int64(time.Hour))
	for i := 0; i < 10; i++ {
		id := nextEphemeralAPILogID()
		assert.False(t, old[id], "id %d repeated across boots", id)
	}
}

func TestSyncAPISessionIDPrefersEphemeralOverride(t *testing.T) {
	transport := newDaveTransport(nil, nil)
	cr := &chatRunner{transport: transport, sessionID: 77}
	cr.syncAPISessionID()
	assert.Equal(t, int64(77), transport.sessionID, "zero override means log under sessionID")

	cr.apiLogSessionID = -42
	cr.syncAPISessionID()
	assert.Equal(t, int64(-42), transport.sessionID, "ephemeral override wins")
}

func TestEphemeralAPILogPerRunFiles(t *testing.T) {
	dir := t.TempDir()
	l, err := NewAPILogger(APILogConfig{Dir: dir}, dir)
	require.NoError(t, err)

	id1, id2 := nextEphemeralAPILogID(), nextEphemeralAPILogID()
	l.RestoreSession(id1, "net", "#a", 7)
	l.RestoreSession(id2, "net", "#a", 7)
	l.LogRequest(id1, []byte(`{"a":1}`))
	l.LogRequest(id2, []byte(`{"b":2}`))

	p1 := l.GetSessionFilePath(id1)
	p2 := l.GetSessionFilePath(id2)
	require.NotEmpty(t, p1)
	require.NotEmpty(t, p2)
	assert.NotEqual(t, p1, p2, "each run gets its own file")

	b1, err := os.ReadFile(p1)
	require.NoError(t, err)
	assert.Contains(t, string(b1), `{"a":1}`)
	assert.NotContains(t, string(b1), `{"b":2}`, "no cross-run bleed")

	b2, err := os.ReadFile(p2)
	require.NoError(t, err)
	assert.Contains(t, string(b2), `{"b":2}`)
	assert.NotContains(t, string(b2), `{"a":1}`)

	// The legacy guard is untouched: session 0 still refuses.
	assert.Empty(t, l.GetSessionFilePath(0))
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestNextEphemeralAPILogID|TestSyncAPISessionIDPrefersEphemeralOverride|TestEphemeralAPILogPerRunFiles' .`
Expected: FAIL — `nextEphemeralAPILogID` undefined, `apiLogSessionID` unknown field.

- [ ] **Step 3: Implement**

In `apiLog.go`, add `"sync/atomic"` to imports, then after the `apiLogger` var (line ~37):

```go
// ephemeralAPILogSeq seeds per-run api-log session ids for ephemeral
// (generator) turns. Seeded from the clock once at boot and stepped
// atomically: ids are unique within a boot AND across restarts, and
// negative so they can never collide with DB session ids (which start
// at 1). DESIGN NOTE: the alternative — one shared session-0 bucket
// per network/channel/user — was rejected by the owner: every
// generator run would append into one ever-growing file; per-run ids
// give each run its own file, exactly like real sessions.
var ephemeralAPILogSeq atomic.Int64

func init() {
	ephemeralAPILogSeq.Store(time.Now().UnixNano())
}

// nextEphemeralAPILogID allocates the api-log session id for one
// ephemeral run.
func nextEphemeralAPILogID() int64 {
	return -ephemeralAPILogSeq.Add(1)
}
```

In `aiCmds.go`, add the field to `chatRunner` (after `ephemeral bool`, ~line 237):

```go
	// apiLogSessionID overrides the id the apiLogger logs this runner's
	// traffic under. Zero (the default) means "log under sessionID" —
	// normal turns never touch it. Ephemeral (generator) runs set it to
	// a unique negative id (nextEphemeralAPILogID): their sessionID is
	// 0 (a no-op in the apiLogger, and load-bearing for usage
	// attribution), so each run gets its own api-log session instead.
	apiLogSessionID int64
```

Replace `syncAPISessionID` (line 321-323):

```go
// apiLogID is the id api-logging paths use for this runner: the
// ephemeral override when set, else the session id.
func (cr *chatRunner) apiLogID() int64 {
	if cr.apiLogSessionID != 0 {
		return cr.apiLogSessionID
	}
	return cr.sessionID
}

func (cr *chatRunner) syncAPISessionID() {
	cr.transport.setAPILogger(apiLogger, cr.apiLogID())
}
```

In `aiCmds.go:1890`, change the chat() breadcrumb to the helper (same value for normal turns — consistency only):

```go
	runner.logger.Debug("completion finished", "api_log", apiLogger.GetSessionFilePath(runner.apiLogID()))
```

In `incident.go:178`, change:

```go
	info.APILogCopied = copyAPILog(cr.apiLogID(), incidentDir)
```

(All other transport-side logging — `LogStreamChunk(cr.transport.sessionID, …)` at aiCmds.go:918/1691, `logStreamCompletion` at 1022/1732 — reads the transport field, which `syncAPISessionID` now sets to `apiLogID()`, so they follow automatically.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestNextEphemeralAPILogID|TestSyncAPISessionIDPrefersEphemeralOverride|TestEphemeralAPILogPerRunFiles|TestAPILog' .`
Expected: PASS (including the existing apiLog tests — the `sessionID == 0` guards are untouched).

- [ ] **Step 5: Full gate + commit**

```bash
go build ./... && go vet ./... && go fmt ./... && go test ./...
git add aiCmds.go apiLog.go incident.go apiLog_test.go
git commit -m "feat(generators): per-run negative api-log session ids and the runner seam"
```

---

### Task 2: the `query_channel_logs` tool — plumbing, dispatch, handler

**Files:**
- Modify: `generators.go` (tool name const, `generatorLogQuery`, def, entry type, map, handler)
- Modify: `aiCmds.go` (chatRunner `logQuery` field ~line 237; `getTools` 475-483; `executeToolCalls` dispatch branch ~line 1231; `getToolServerName` 2048-2053)
- Modify: `config.go:641` (hidden default)
- Modify: `config_test.go:1453` area (hidden default expectation)
- Test: `generators_test.go`

**Interfaces:**
- Consumes: `fetchChannelLogFn` (generators.go:15), `applyLogQueryDefaults`/`windowDuration`/`LogWindowResult`/`errLogWindowTooLarge`/`logQueryRowCap` (logquery.go), `toolResultMsg`, `isToolDisabled`, `isToolHidden`, `expandNotice`/`getNotices().Tools.Call`.
- Produces (used by Tasks 3-5): `const queryChannelLogsToolName = "query_channel_logs"`; `type generatorLogQuery struct { spec LogQuerySpec; channelRaw, channel, name string }`; `var generatorTools map[string]generatorToolEntry` (dispatch registry; Task 4 adds `respond` to it); `chatRunner.logQuery *generatorLogQuery`; `func handleGeneratorLogQuery(cr *chatRunner, turn *turnContext, call ToolCall)`; getTools' ephemeral branch now appends the log tool when `cr.logQuery != nil`.

- [ ] **Step 1: Write the failing tests**

Append to `generators_test.go` (mirror the file's existing hand-built-runner style; needs `"encoding/json"`, `"strings"`, `"time"`, testify, and the `newTestLogger` helper):

```go
func newGeneratorToolTestRunner(ephemeral bool, lq *generatorLogQuery) *chatRunner {
	return &chatRunner{
		// ToolVerbose=false keeps output-channel assertions
		// deterministic: executeToolCalls would otherwise emit
		// per-tool notices into outputCh depending on global config
		// state left by other tests.
		cfg:       AIConfig{Name: "summary", Model: "m", ToolVerbose: boolPtr(false)},
		network:   Network{Name: "testnet"},
		channel:   "#st",
		logger:    newTestLogger(),
		ctx:       context.Background(),
		outputCh:  make(chan string, 16),
		ephemeral: ephemeral,
		logQuery:  lq,
	}
}

func generatorLogToolCall(id, argsJSON string) ToolCall {
	return ToolCall{
		ID:     id,
		Type:   "function",
		Function: FunctionDefinition{Name: queryChannelLogsToolName, Arguments: argsJSON},
	}
}

func TestGetToolsEphemeralOffersLogTool(t *testing.T) {
	setupNoticesDefaults(t)
	lq := &generatorLogQuery{
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	}
	cr := newGeneratorToolTestRunner(true, lq)
	tools := cr.getTools()
	require.Len(t, tools, 1, "no MCP servers configured → only the log tool")
	assert.Equal(t, queryChannelLogsToolName, tools[0].Function.Name)
	assert.Contains(t, tools[0].Function.Description, `Default: 24h.`)
	assert.Equal(t, []string{}, tools[0].Function.Parameters["required"], "window must be optional")
}

func TestGetToolsEphemeralWithoutLogBlock(t *testing.T) {
	setupNoticesDefaults(t)
	cr := newGeneratorToolTestRunner(true, nil)
	assert.Empty(t, cr.getTools())
}

func TestGetToolsNonEphemeralNeverOffersGeneratorTools(t *testing.T) {
	setupNoticesDefaults(t)
	// Even an (impossible-in-production) non-ephemeral runner carrying a
	// logQuery must not offer the generator tool: chats never see it.
	cr := newGeneratorToolTestRunner(false, &generatorLogQuery{name: "x"})
	assert.Equal(t, toolDefsForConfig(cr.cfg), cr.getTools())
}

func TestGetToolServerNameLabelsGeneratorToolsBuiltin(t *testing.T) {
	assert.Equal(t, "builtin", getToolServerName(queryChannelLogsToolName))
}

func handleLogQueryForTest(t *testing.T, cr *chatRunner, argsJSON string) *turnContext {
	t.Helper()
	turn := newEphemeralTurnContext(nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "go"})
	cr.executeToolCalls(turn, []ToolCall{generatorLogToolCall("call_1", argsJSON)})
	return turn
}

func lastToolResultText(t *testing.T, turn *turnContext) string {
	t.Helper()
	msgs := turn.Messages()
	require.NotEmpty(t, msgs, "expected a tool result row")
	last := msgs[len(msgs)-1]
	require.Equal(t, RoleTool, last.Role)
	return last.Content
}

func TestHandleGeneratorLogQueryWindowOverrideAndResult(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	var gotSpec LogQuerySpec
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		gotSpec = spec
		return &LogWindowResult{
			Lines:      []string{"[09:00] <a> hello"},
			Tokens:     5, TotalLines: 1,
			FirstKept: time.Date(2026, 10, 9, 9, 0, 0, 0, time.Local),
			LastKept:  time.Date(2026, 10, 9, 9, 30, 0, 0, time.Local),
		}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	lq := &generatorLogQuery{
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	}
	cr := newGeneratorToolTestRunner(true, lq)
	turn := handleLogQueryForTest(t, cr, `{"window":"7d"}`)

	assert.Equal(t, "7d", gotSpec.Window, "the tool argument overrides the configured default")
	result := lastToolResultText(t, turn)
	assert.Contains(t, result, "Channel activity for #st on testnet, last 168h0m0s (1 lines, 5 tokens, covering 09:00 to 09:30):")
	assert.Contains(t, result, "[09:00] <a> hello")
}

func TestHandleGeneratorLogQueryDefaultsWindow(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	var gotSpec LogQuerySpec
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		gotSpec = spec
		return &LogWindowResult{Lines: []string{"x"}}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec: LogQuerySpec{Window: "12h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	handleLogQueryForTest(t, cr, `{}`)
	assert.Equal(t, "12h", gotSpec.Window, "omitted window → configured default")
}

func TestHandleGeneratorLogQueryInvalidWindow(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		t.Fatal("fetch must not run for an invalid window")
		return nil, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec: LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	turn := handleLogQueryForTest(t, cr, `{"window":"12x"}`)
	assert.Contains(t, lastToolResultText(t, turn), "invalid window duration")
}

func TestHandleGeneratorLogQueryRowCap(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		return nil, fmt.Errorf("%w: %d rows (cap %d)", errLogWindowTooLarge, 2, logQueryRowCap)
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec: LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	turn := handleLogQueryForTest(t, cr, `{}`)
	assert.Contains(t, lastToolResultText(t, turn), "row cap")
	assert.Contains(t, lastToolResultText(t, turn), "narrower window")
}

func TestHandleGeneratorLogQueryNoActivity(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec: LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	turn := handleLogQueryForTest(t, cr, `{}`)
	assert.Contains(t, lastToolResultText(t, turn), "No channel activity found in the last")
}

func TestHandleGeneratorLogQueryTruncationMarkerRides(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{
			Lines: []string{"[... 40 earlier lines omitted to fit the 60000-token budget ...]", "[09:00] <a> hi"},
			Tokens: 10, TotalLines: 41, DroppedLines: 40, Truncated: true,
		}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec: LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	turn := handleLogQueryForTest(t, cr, `{}`)
	assert.Contains(t, lastToolResultText(t, turn), "omitted to fit")
}

func TestHandleGeneratorLogQueryGuardNonEphemeral(t *testing.T) {
	setupNoticesDefaults(t)
	// A hallucinated call from a non-ephemeral turn (logQuery nil or
	// ephemeral false) must error, not panic or succeed silently.
	for _, cr := range []*chatRunner{
		newGeneratorToolTestRunner(false, &generatorLogQuery{name: "x"}),
		newGeneratorToolTestRunner(true, nil),
	} {
		turn := handleLogQueryForTest(t, cr, `{}`)
		assert.Contains(t, lastToolResultText(t, turn), "not available on this command")
	}
}

func TestHandleGeneratorLogQueryDisabled(t *testing.T) {
	setupNoticesDefaults(t)
	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{name: "x"})
	cr.cfg.DisabledBuiltinTools = []string{queryChannelLogsToolName}
	turn := handleLogQueryForTest(t, cr, `{}`)
	assert.Contains(t, lastToolResultText(t, turn), "is disabled for this command")
}
```

Extend the hidden-default test in `config_test.go` (the `hidden_tools defaults to all builtin tools` subtest at ~line 1453): change the expected default to include `"query_channel_logs"`:

```go
	assert.Equal(t, []string{"register_background_job", "check_ban_history", "query_channel_logs"}, config.HiddenTools)
```

(If the subtest is table-shaped, update its expectation row; read the test first.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestGetToolsEphemeral|TestGetToolServerNameLabelsGeneratorToolsBuiltin|TestHandleGeneratorLogQuery|TestLoadConfigDirHiddenTools' .` (adjust the last pattern to the actual hidden-default test name).
Expected: FAIL — undefined `queryChannelLogsToolName`, `generatorLogQuery`, `generatorTools`.

- [ ] **Step 3: Implement**

In `generators.go`, add `"encoding/json"` to imports and after the `fetchChannelLogFn` var:

```go
const queryChannelLogsToolName = "query_channel_logs"

// generatorLogQuery is the per-run log-retrieval context a log-fed
// generator carries on its runner: the defaulted spec plus the raw and
// normalized channel names the SQL match needs (setChannel keeps only
// the normalized one) and the generator name for logs.
type generatorLogQuery struct {
	spec       LogQuerySpec
	channelRaw string
	channel    string
	name       string
}

// generatorLogToolDef builds the per-run tool definition. The default
// window is embedded in the description so the model knows what it gets
// when it omits the argument. DESIGN NOTE: this is NOT a static
// builtinTools entry — the definition is per-run (default window) and
// the handler needs the runner's generator context.
func generatorLogToolDef(lq *generatorLogQuery) Tool {
	return Tool{
		Type: "function",
		Function: &FunctionDefinition{
			Name: queryChannelLogsToolName,
			Description: fmt.Sprintf(
				"Retrieve the transcript of this IRC channel's recent activity. Returns timestamped lines (newest last) with a header giving the covered time range and line/token counts; a keep-newest token budget is applied and any truncation is disclosed in the result. Call this before summarizing or referencing channel history. window: how far back to look — a duration string of <number><unit> groups with units s, m, h, d (e.g. \"90m\", \"12h\", \"7d\", \"1d12h\"). Default: %s.",
				lq.spec.Window),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"window": map[string]any{
						"type":        "string",
						"description": "How far back to retrieve (e.g. \"12h\", \"7d\"). Defaults to the configured window.",
					},
				},
				"required": []string{},
			},
		},
	}
}

type generatorToolEntry struct {
	handler func(cr *chatRunner, turn *turnContext, call ToolCall)
}

// generatorTools are the ephemeral-turn-only tools generators offer
// (query_channel_logs for log-fed commands; respond joins in a later
// task for respond_tool generators). Deliberately separate from the
// global builtinTools map: definitions are per-run and handlers need
// the runner's generator context. Both honor disabled_builtin_tools
// and hidden_tools exactly like builtins.
var generatorTools = map[string]generatorToolEntry{
	queryChannelLogsToolName: {handler: handleGeneratorLogQuery},
}

// handleGeneratorLogQuery executes query_channel_logs: resolves the
// window (configured default when omitted), fetches via the injection
// seam, and returns the transcript as the tool result — every failure
// mode is tool-result content the model can act on (self-correct or
// relay), never a user-facing notice.
func handleGeneratorLogQuery(cr *chatRunner, turn *turnContext, call ToolCall) {
	if cr.logQuery == nil || !cr.ephemeral {
		// Offered only on ephemeral log-fed turns; a hallucinated call
		// from any other turn gets an error result, not a panic.
		turn.Add(toolResultMsg(call.ID, "error: query_channel_logs is not available on this command"))
		return
	}
	var args struct {
		Window string `json:"window"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		turn.Add(toolResultMsg(call.ID, "error: failed to parse tool arguments: "+err.Error()))
		return
	}
	spec := cr.logQuery.spec
	// Defense in depth: production specs are defaulted at config load
	// and again by generator(); hand-built contexts (tests) may not be.
	applyLogQueryDefaults(&spec)
	if args.Window != "" {
		// The token IS the duration string (same grammar as the config
		// field); windowDuration below rejects anything else with a
		// clear message.
		spec.Window = args.Window
	}
	window, werr := spec.windowDuration()
	if werr != nil {
		turn.Add(toolResultMsg(call.ID, "error: "+werr.Error()))
		return
	}
	lw, err := fetchChannelLogFn(spec, cr.network.Name, cr.logQuery.channelRaw, cr.logQuery.channel, cr.cfg.Model, time.Now())
	if err != nil {
		if errors.Is(err, errLogWindowTooLarge) {
			turn.Add(toolResultMsg(call.ID, fmt.Sprintf(
				"error: that window exceeds the row cap (%d rows) — request a narrower window", logQueryRowCap)))
			return
		}
		cr.logger.Error("generator log query failed", "trigger", cr.logQuery.name, "error", err)
		turn.Add(toolResultMsg(call.ID, "error: "+err.Error()))
		return
	}
	// Coverage carries the kept rows' actual time range; BOTH endpoints
	// must be set or it stays empty (a hand-built result with only one
	// would render "14:32 to 00:00") — same rule the v1 notice used.
	coverage := ""
	if !lw.FirstKept.IsZero() && !lw.LastKept.IsZero() {
		coverage = fmt.Sprintf(", covering %s to %s", lw.FirstKept.Format("15:04"), lw.LastKept.Format("15:04"))
	}
	cr.logger.Info("generator log query",
		"trigger", cr.logQuery.name, "window", spec.Window, "coverage", coverage,
		"files", len(lw.Files), "lines", lw.TotalLines, "dropped", lw.DroppedLines,
		"tokens", lw.Tokens, "budget", spec.MaxTokens, "truncated", lw.Truncated)
	if lw.TotalLines == 0 {
		turn.Add(toolResultMsg(call.ID, fmt.Sprintf("No channel activity found in the last %s.", formatDuration(window))))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Channel activity for %s on %s, last %s (%d lines, %d tokens%s):\n",
		cr.logQuery.channel, cr.network.Name, formatDuration(window), lw.TotalLines, lw.Tokens, coverage)
	b.WriteString(strings.Join(lw.Lines, "\n"))
	turn.Add(toolResultMsg(call.ID, b.String()))
}
```

In `aiCmds.go`, add the runner field (after `ephemeral`, before `apiLogSessionID`):

```go
	// logQuery is the generator's log-retrieval context — nil on
	// non-generator turns and log-less generators. Gates the
	// query_channel_logs offering and carries the channel names and
	// generator name its handler needs.
	logQuery *generatorLogQuery
```

Rewrite `getTools` (line 475-483):

```go
func (cr *chatRunner) getTools() []Tool {
	if cr.ephemeral {
		// Generator turns: MCP tools work normally, but the builtin LLM
		// tools are never offered (register_background_job delivery is
		// session-bound; ban tools are chat-moderation concerns). The
		// generator tools below are offered per-config — NOT gated on
		// MCP tools existing, unlike the regular builtins.
		tools := mcpToolDefsForConfig(cr.cfg)
		if cr.logQuery != nil {
			tools = append(tools, generatorLogToolDef(cr.logQuery))
		}
		return tools
	}
	return toolDefsForConfig(cr.cfg)
}
```

In `executeToolCalls`, inside the `for _, tc := range toolCalls` loop, add this branch BEFORE the `builtinTools` lookup (line ~1232):

```go
		if entry, ok := generatorTools[tc.Function.Name]; ok {
			if isToolDisabled(tc.Function.Name, cr.cfg.DisabledBuiltinTools) {
				toolMsg := toolResultMsg(tc.ID, fmt.Sprintf("error: tool %q is disabled for this command", tc.Function.Name))
				turn.Add(toolMsg)
				continue
			}
			if verbose && !isToolHidden(tc.Function.Name, hiddenTools) && len(visibleTools) <= 1 {
				cr.sendIRC(expandNotice(getNotices().Tools.Call, map[string]string{"server": "builtin", "tool": tc.Function.Name}))
			}
			cr.logger.Info("generator tool call", "tool", tc.Function.Name)
			entry.handler(cr, turn, tc)
			continue
		}
```

In `getToolServerName` (line 2048-2053), add the generator-tools case:

```go
func getToolServerName(toolName string) string {
	if _, ok := generatorTools[toolName]; ok {
		return "builtin"
	}
	if _, ok := builtinTools[toolName]; ok {
		return "builtin"
	}
	return getMCPServerForTool(toolName)
}
```

In `config.go:641`:

```go
		config.HiddenTools = []string{"register_background_job", "check_ban_history", "query_channel_logs"}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestGetTools|TestGetToolServerName|TestHandleGeneratorLogQuery|TestToolDefsForConfig' .`
Expected: PASS — including the existing `TestToolDefsForConfig` (non-ephemeral path untouched).

- [ ] **Step 5: Full gate + commit**

```bash
go build ./... && go vet ./... && go fmt ./... && go test ./...
git add generators.go aiCmds.go config.go config_test.go generators_test.go
git commit -m "feat(generators): query_channel_logs tool — plumbing, dispatch, handler"
```

---

### Task 3: executor rewrite — verbatim args, armed runner, per-run api-log session

**Files:**
- Modify: `generators.go` (generator() body, lines ~17-177)
- Modify: `main.go:494-521` area (registration comment about optional-args semantics — comment-only)
- Test: `generators_test.go` (rewrite the v1 executor tests)

**Interfaces:**
- Consumes: Task 1's `nextEphemeralAPILogID`/`apiLogID`; Task 2's `generatorLogQuery`/`chatRunner.logQuery`.
- Produces: `generator()` with unchanged signature (main.go's registration calls it unchanged); behavior per the spec — verbatim args, no transcript, per-run api-log session, post-turn breadcrumb.

- [ ] **Step 1: Rewrite the failing tests**

In `generators_test.go`, DELETE these v1 tests (their behaviors die with injection): `TestGeneratorNoActivitySendsNoticeWithoutLLMCall`, `TestGeneratorTruncationNotice`, `TestGeneratorTruncationNoticeCoverage`, `TestGeneratorTruncationNoticeCoverageRequiresBothTimes`, `TestGeneratorWindowTooLargeNotice`, `TestGeneratorErrorsWrappedFromFetch`.

First extend the harness so tests can capture the real runner (update ALL remaining `withGeneratorRunner` callers to the new two-value signature — after the deletions above the callers are the rewritten tests plus `TestGeneratorNonLogArgsPassedThrough`, which keeps its assertions and only adapts to the new return shape):

```go
// withGeneratorRunner swaps newChatRunnerFn for one returning a real runner
// against an httptest server, restoring it after the test. runnerCh yields
// the created runner so tests can assert on its ephemeral context.
func withGeneratorRunner(t *testing.T, cfg AIConfig, handler http.HandlerFunc) (outputCh chan string, runnerCh chan *chatRunner) {
	t.Helper()
	outputCh = make(chan string, 64)
	runnerCh = make(chan *chatRunner, 1)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	orig := newChatRunnerFn
	newChatRunnerFn = func(network Network, client *girc.Client, c AIConfig, ctx context.Context, out chan<- string) chatRunnerInterface {
		cr := newStreamTestRunner(t, server, c, 0, outputCh)
		cr.ctx = ctx
		cr.outputCh = out
		runnerCh <- cr
		return cr
	}
	t.Cleanup(func() { newChatRunnerFn = orig })
	return outputCh, runnerCh
}
```

Then write the rewritten/new tests (the `genEvent`/`drainGenOutput`/`streamChunk` helpers already exist in the file; `newStreamTestRunner` lives in aiStreamOutput_test.go; add `"path/filepath"` to imports):

```go
func TestGeneratorLogCommandBuildsEphemeralTurn(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var gotBody string
	var calls int32
	outputCh, runnerCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"all quiet"},"finish_reason":null}]}`)+
			streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+
			"data: [DONE]\n\n")
	})

	// Retrieval must NOT happen at setup: no fetch stub is installed —
	// if generator() fetches, the real fetcher runs and this test fails
	// loudly (missing log dir). That absence IS the deferred-to-tool pin.

	var messagesBefore int64
	require.NoError(t, theDB.Model(&Message{}).Count(&messagesBefore).Error)

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary", Model: "qwen3", Streaming: true,
		StreamTimeout: 5 * time.Second, Timeout: 10 * time.Second, System: "You are {{.BotNick}}."},
		Prompt: "Summarize the following channel activity.",
		Log:    &LogQuerySpec{}}

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, ctx, outputCh, &User{ID: 1, CurrentNick: "shrew"}, "12h focus on drama")
	}()
	<-done
	runner := <-runnerCh

	lines := drainGenOutput(t, outputCh, 16)
	joined := strings.Join(lines, "\n")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "exactly one LLM call")
	assert.Contains(t, joined, "all quiet")

	// The user message is the VERBATIM arg string — the request body is
	// the turn (no duration grammar, no transcript injection).
	assert.Contains(t, gotBody, "12h focus on drama")
	assert.NotContains(t, gotBody, "Channel activity for", "no transcript injection")
	assert.NotContains(t, gotBody, "Summarize the following channel activity.",
		"args present → configured prompt is not the instruction")

	// The runner is armed for the tool.
	require.NotNil(t, runner.logQuery)
	assert.Equal(t, "24h", runner.logQuery.spec.Window, "spec defaulted at arming")
	assert.Equal(t, 60000, runner.logQuery.spec.MaxTokens)
	assert.Equal(t, defaultLogEvents, runner.logQuery.spec.Events)
	assert.Equal(t, "#chan", runner.logQuery.channelRaw)
	assert.Equal(t, "#chan", runner.logQuery.channel, "already-lowercase stays")
	assert.Equal(t, "summary", runner.logQuery.name)
	assert.True(t, runner.ephemeral)

	var messagesAfter int64
	require.NoError(t, theDB.Model(&Message{}).Count(&messagesAfter).Error)
	assert.Equal(t, messagesBefore, messagesAfter, "ephemeral turn persists nothing")
}

func TestGeneratorNoArgsUsesConfiguredPrompt(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var gotBody string
	outputCh, _ := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`)+
			streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+
			"data: [DONE]\n\n")
	})

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary", Model: "qwen3", Streaming: true,
		StreamTimeout: 5 * time.Second, Timeout: 10 * time.Second, System: "sys"},
		Prompt: "CUSTOM DEFAULT",
		Log:    &LogQuerySpec{}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1, CurrentNick: "shrew"})
	}()
	<-done
	drainGenOutput(t, outputCh, 16)
	assert.Contains(t, gotBody, "CUSTOM DEFAULT", "bare invocation → configured prompt is the instruction")
}

func TestGeneratorOpensOwnAPILogSession(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	dir := t.TempDir()
	orig := apiLogger
	l, err := NewAPILogger(APILogConfig{Dir: dir}, dir)
	require.NoError(t, err)
	apiLogger = l
	t.Cleanup(func() { apiLogger = orig })

	outputCh, runnerCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	})

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary", Model: "qwen3", Streaming: true,
		StreamTimeout: 5 * time.Second, Timeout: 10 * time.Second, System: "sys"},
		Prompt: "p", Log: &LogQuerySpec{}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 7, CurrentNick: "shrew"})
	}()
	<-done
	runner := <-runnerCh
	drainGenOutput(t, outputCh, 16)

	assert.Negative(t, runner.apiLogSessionID, "per-run negative id")
	assert.Zero(t, runner.sessionID, "sessionID stays 0 (attribution)")
	assert.Equal(t, runner.apiLogSessionID, runner.transport.sessionID,
		"transport logs under the ephemeral id")
	path := apiLogger.GetSessionFilePath(runner.apiLogSessionID)
	assert.NotEmpty(t, path, "the run's api-log file is open")
	assert.Contains(t, filepath.Base(path), "testnet_#chan_user7_",
		"filename carries the runner identity shape")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestGenerator' .`
Expected: FAIL — the rewritten assertions (verbatim args, `logQuery` armed, no fetch at setup, negative api-log id) don't hold against the v1 executor.

- [ ] **Step 3: Rewrite generator()**

Replace the body of `generator()` in `generators.go` (lines 56-158 — everything between the user-resolution block and the system-prompt rendering) with:

```go
	runner.ephemeral = true
	if cfg.Log != nil {
		spec := *cfg.Log
		// Production specs are defaulted at config-load time; hand-built
		// or partial specs (tests, future callers) re-default here so an
		// empty window never becomes the tool's advertised default.
		applyLogQueryDefaults(&spec)
		runner.logQuery = &generatorLogQuery{
			spec:       spec,
			channelRaw: channelRaw,
			channel:    channel,
			name:       cfg.Name,
		}
	}
	// One api-log session per run: a unique negative id (never a DB
	// session id; unique across concurrent runs and restarts) opened
	// with the run's real identity. sessionID stays 0 — load-bearing
	// for usage attribution (SessionID-0 turn_usage rows) and the
	// apiLogger's legacy no-session guard.
	runner.apiLogSessionID = nextEphemeralAPILogID()
	apiLogger.RestoreSession(runner.apiLogSessionID, network.Name, channel, resolvedUser.ID)
	// setChannel syncs the transport (apiLogger under apiLogID()).
	runner.setChannel(channel, nick, resolvedUser.ID)

	userText := ""
	if len(args) > 0 {
		userText = args[0]
	}
	if userText == "" {
		// Bare invocation (optional-args dispatch) — the model defaults
		// the window via the tool; the prompt is the instruction.
		userText = cfg.Prompt
	}
	if userText == "" {
		userText = "Summarize the following channel activity."
	}
```

Order matters: `apiLogSessionID` and `logQuery` are set BEFORE `setChannel` so its internal `syncAPISessionID` wires the transport to the ephemeral id. The system-prompt rendering block (lines 160-170) and the ephemeral turn construction (172-175) are unchanged. After `runner.runTurn(turn)` add:

```go
	runner.logger.Debug("generator finished", "api_log", apiLogger.GetSessionFilePath(runner.apiLogID()))
```

Update the function's doc comment (lines 17-23) to the tool-driven model:

```go
// generator is the executor for one-shot generator commands (spec
// docs/superpowers/specs/2026-10-09-generators-tool-driven-design.md):
// chat() minus persistence. It renders the system prompt, builds one
// user message from the verbatim args, and runs the UNCHANGED runTurn
// agentic loop on an ephemeral turn — streaming, tools, empty-response
// retry all apply. Log-fed generators expose the query_channel_logs
// tool (the model chooses its windows; [name.log] configures defaults
// and caps). No session row is created; usage rows are attributed with
// SessionID 0; the run's API traffic logs to its own api-log file
// under a unique negative id.
```

Delete now-unused imports if the compiler flags them (v1 used `errors` — still used by the Task 2 handler; `splitFirstWord`/`parseWindowDuration`/`expandNotice`/`formatDuration` calls in generator() die — `formatDuration` and `expandNotice` remain used by the handler/notices elsewhere; `time` remains used by the handler). In `main.go`, update the registration-loop comment for the optional-args branch (line ~511): the rationale changes from "duration arg is optional" to "args are optional — the model defaults the window via the query_channel_logs tool".

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestGenerator|TestDispatchOptionalArgs|TestRegisterGenerators' .`
Expected: PASS — registration/dispatch tests stay green (truth table unchanged: log-fed → optional-args).

- [ ] **Step 5: Full gate + commit**

```bash
go build ./... && go vet ./... && go fmt ./... && go test ./...
git add generators.go main.go generators_test.go
git commit -m "feat(generators): executor rewrite — verbatim args, armed log-query context, per-run api-log session"
```

---

### Task 4: the `respond` tool — config gate, handler, turn completion

**Files:**
- Modify: `config.go` (GeneratorConfig field + doc comment ~line 256)
- Modify: `generators.go` (respond const, def, handler, registry entry)
- Modify: `aiCmds.go` (chatRunner `respondTool`/`responded` fields; getTools respond branch; the four loop-variant completion checks at lines ~823, ~1089, ~1186, ~1618)
- Modify: `config.go:641` (hidden default += `respond`) and `config_test.go` hidden-default expectation
- Modify: `generators.go` generator() (set `runner.respondTool = cfg.RespondTool` right after `runner.ephemeral = true`)
- Test: `generators_test.go`, `config_test.go`, `aiStreamOutput_test.go` (one integration test)

**Interfaces:**
- Consumes: Task 2's `generatorTools` registry + dispatch branch; `sendRendered`; `toolResultMsg`.
- Produces: `const respondToolName = "respond"`; `GeneratorConfig.RespondTool bool` (toml `respond_tool`); `chatRunner.respondTool bool`; `chatRunner.responded bool`; `func handleGeneratorRespond(cr *chatRunner, turn *turnContext, call ToolCall)`; `func generatorRespondToolDef() Tool`. The completion contract: every `runTurn` variant returns done immediately after `executeToolCalls` when `cr.responded` is set.

- [ ] **Step 1: Write the failing tests**

Append to `generators_test.go`:

```go
func TestGetToolsEphemeralOffersRespondWhenEnabled(t *testing.T) {
	setupNoticesDefaults(t)
	cr := newGeneratorToolTestRunner(true, nil)
	cr.respondTool = true
	tools := cr.getTools()
	require.Len(t, tools, 1)
	assert.Equal(t, respondToolName, tools[0].Function.Name)
	assert.Equal(t, []string{"text"}, tools[0].Function.Parameters["required"])
}

func respondToolCall(id, argsJSON string) ToolCall {
	return ToolCall{
		ID:     id,
		Type:   "function",
		Function: FunctionDefinition{Name: respondToolName, Arguments: argsJSON},
	}
}

func TestHandleGeneratorRespondSendsAndFlags(t *testing.T) {
	setupNoticesDefaults(t)
	out := make(chan string, 16)
	cr := &chatRunner{
		cfg: AIConfig{Name: "summary", ToolVerbose: boolPtr(false)},
		network: Network{Name: "testnet"},
		logger: newTestLogger(), ctx: context.Background(),
		outputCh: out, ephemeral: true, responded: false,
	}
	turn := newEphemeralTurnContext(nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "go"})
	cr.executeToolCalls(turn, []ToolCall{respondToolCall("call_1", `{"text":"FINAL ANSWER"}`)})

	assert.True(t, cr.responded, "responded flag ends the turn")
	select {
	case got := <-out:
		assert.Contains(t, got, "FINAL ANSWER")
	default:
		t.Fatal("respond text was not sent")
	}
	last := turn.Messages()[len(turn.Messages())-1]
	assert.Equal(t, RoleTool, last.Role, "a tool result closes the round trip")
}

func TestHandleGeneratorRespondEmptyText(t *testing.T) {
	setupNoticesDefaults(t)
	out := make(chan string, 16)
	cr := &chatRunner{
		cfg: AIConfig{Name: "summary", ToolVerbose: boolPtr(false)},
		network: Network{Name: "testnet"},
		logger: newTestLogger(), ctx: context.Background(),
		outputCh: out, ephemeral: true,
	}
	turn := newEphemeralTurnContext(nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "go"})
	cr.executeToolCalls(turn, []ToolCall{respondToolCall("call_1", `{"text":"   "}`)})
	assert.False(t, cr.responded)
	assert.Contains(t, turn.Messages()[len(turn.Messages())-1].Content, "non-empty")
	select {
	case <-out:
		t.Fatal("nothing is sent for an empty respond")
	default:
	}
}

func TestHandleGeneratorRespondGuardNonEphemeral(t *testing.T) {
	setupNoticesDefaults(t)
	out := make(chan string, 16)
	cr := &chatRunner{
		cfg: AIConfig{Name: "chat", ToolVerbose: boolPtr(false)},
		network: Network{Name: "testnet"},
		logger: newTestLogger(), ctx: context.Background(),
		outputCh: out, // ephemeral false — hallucinated call
	}
	turn := newEphemeralTurnContext(nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "go"})
	cr.executeToolCalls(turn, []ToolCall{respondToolCall("call_1", `{"text":"x"}`)})
	assert.False(t, cr.responded)
	assert.Contains(t, turn.Messages()[len(turn.Messages())-1].Content, "not available on this command")
}

func TestHandleGeneratorRespondAlongsideOtherTools(t *testing.T) {
	setupNoticesDefaults(t)
	out := make(chan string, 16)
	cr := &chatRunner{
		cfg: AIConfig{Name: "summary", ToolVerbose: boolPtr(false)},
		network: Network{Name: "testnet"},
		logger: newTestLogger(), ctx: context.Background(),
		outputCh: out, ephemeral: true,
	}
	turn := newEphemeralTurnContext(nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "go"})
	cr.executeToolCalls(turn, []ToolCall{
		{ID: "call_1", Type: "function", Function: FunctionDefinition{Name: "nonexistent_tool", Arguments: "{}"}},
		respondToolCall("call_2", `{"text":"THE ANSWER"}`),
	})

	// Calls execute in order; both produce tool results (the turn is
	// ephemeral anyway); respond's text is sent exactly once and the
	// flag ends the turn after the batch.
	msgs := turn.Messages()
	toolResults := 0
	for _, m := range msgs {
		if m.Role == RoleTool {
			toolResults++
		}
	}
	assert.Equal(t, 2, toolResults, "both calls produce results")
	assert.True(t, cr.responded)

	var sent []string
	for {
		select {
		case s := <-out:
			sent = append(sent, s)
		default:
			goto done
		}
	}
done:
	require.Len(t, sent, 1, "exactly one send")
	assert.Contains(t, sent[0], "THE ANSWER")
}

func TestHandleGeneratorRespondDisabled(t *testing.T) {
	setupNoticesDefaults(t)
	cr := newGeneratorToolTestRunner(true, nil)
	cr.cfg.DisabledBuiltinTools = []string{respondToolName}
	turn := newEphemeralTurnContext(nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "go"})
	cr.executeToolCalls(turn, []ToolCall{respondToolCall("call_1", `{"text":"x"}`)})
	assert.Contains(t, turn.Messages()[len(turn.Messages())-1].Content, "is disabled for this command")
}
```

In `aiStreamOutput_test.go` (reusing `newStreamTestRunner` + the SSE patterns; the runner needs `ephemeral: true` — set it on the returned runner):

```go
// TestRunTurnRespondEndsTurnSingleRequest: a respond tool call ends the
// turn with NO second API request — the completion signal is checked
// right after executeToolCalls in every loop variant.
func TestRunTurnRespondEndsTurnSingleRequest(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	respondStream := streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"respond","arguments":"{\"text\":\"ALL DONE\"}"}}]},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		"data: [DONE]\n\n"

	// Always serve the respond stream: if the loop erroneously continues,
	// a second request would re-deliver "ALL DONE" and both post-hoc
	// assertions (reqs == 1, single delivery) catch it. No assertions
	// inside the handler goroutine (require is test-goroutine-only).
	var reqs int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		atomic.AddInt32(&reqs, 1)
		fmt.Fprint(w, respondStream)
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		StreamTimeout: 5 * time.Second,
	}, 0, outputCh)
	cr.ephemeral = true
	cr.respondTool = true

	done := make(chan struct{})
	go func() { cr.runTurn(newEphemeralTurnContext([]ChatMessage{{Role: RoleUser, Content: "sum"}})); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	var sent []string
	for {
		select {
		case s := <-outputCh:
			sent = append(sent, s)
		default:
			goto asserted
		}
	}
asserted:
	joined := strings.Join(sent, "\n")
	assert.Contains(t, joined, "ALL DONE")
	assert.Equal(t, 1, strings.Count(joined, "ALL DONE"), "delivered exactly once")
	assert.EqualValues(t, 1, atomic.LoadInt32(&reqs), "respond ends the turn — no second request")
}
```

In `config_test.go`, extend the generators loading test (or add one beside `TestLoadConfigDirGenerators`):

```go
func TestLoadConfigDirGeneratorsRespondTool(t *testing.T) {
	dir := createTestConfigDir(t, `
[services.local]
baseurl = "http://localhost:1"
type = "llama"

[generators.responder]
service = "local"
model = "m"
respond_tool = true
prompt = "p"
`)
	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	assert.True(t, cfg.Commands.Generators["responder"].RespondTool)
	assert.False(t, cfg.Commands.Generators["summary"].RespondTool, "default false")
}
```

(Adjust the TOML prelude to whatever `createTestConfigDir` + neighbors actually require — read `TestLoadConfigDirGenerators` first and mirror its service block.)

Update the hidden-default expectation (Task 2 touched it once; now):

```go
	assert.Equal(t, []string{"register_background_job", "check_ban_history", "query_channel_logs", "respond"}, config.HiddenTools)
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestGetToolsEphemeralOffersRespond|TestHandleGeneratorRespond|TestRunTurnRespondEndsTurnSingleRequest|TestLoadConfigDirGeneratorsRespondTool' .`
Expected: FAIL — undefined `respondToolName`, no respond branch, loop doesn't end.

- [ ] **Step 3: Implement**

`config.go` — add the field to `GeneratorConfig` (line 256-260) and update the struct's doc comment (the "argument grammar" sentence dies):

```go
type GeneratorConfig struct {
	AIConfig
	Prompt      string        `toml:"prompt"`
	Log         *LogQuerySpec `toml:"log"`
	RespondTool bool          `toml:"respond_tool"`
}
```

`generators.go` — add after `queryChannelLogsToolName`:

```go
const respondToolName = "respond"
```

Add the registry entry and the two functions:

```go
var generatorTools = map[string]generatorToolEntry{
	queryChannelLogsToolName: {handler: handleGeneratorLogQuery},
	respondToolName:          {handler: handleGeneratorRespond},
}

// generatorRespondToolDef builds the respond tool definition. Its text
// argument is the model's complete final answer for the user.
func generatorRespondToolDef() Tool {
	return Tool{
		Type: "function",
		Function: &FunctionDefinition{
			Name: respondToolName,
			Description: "Deliver your complete final answer to the user and end your turn. Use this once your task is finished; the text is rendered and sent verbatim. Do not call any other tool after this.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "Your complete final answer for the user.",
					},
				},
				"required": []string{"text"},
			},
		},
	}
}

// handleGeneratorRespond delivers the model's final answer and ends
// the turn: the text goes through the normal render/pastebin plumbing,
// and the responded flag tells every runTurn variant to return right
// after executeToolCalls — no second API round-trip (a respond
// iteration carries a tool call, so it never hits the empty-response
// machinery by the existing definition: tool-call branches reset
// emptyRetries). When respond rides alongside other tool calls in one
// iteration, calls execute in order and the rest still run (results
// logged; the turn is ephemeral anyway) — the flag simply ends the
// loop after the batch.
func handleGeneratorRespond(cr *chatRunner, turn *turnContext, call ToolCall) {
	if !cr.ephemeral {
		// Offered only on ephemeral turns; a hallucinated call from a
		// chat turn gets an error result.
		turn.Add(toolResultMsg(call.ID, "error: respond is not available on this command"))
		return
	}
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		turn.Add(toolResultMsg(call.ID, "error: failed to parse tool arguments: "+err.Error()))
		return
	}
	text := strings.TrimSpace(args.Text)
	if text == "" {
		turn.Add(toolResultMsg(call.ID, "error: respond requires a non-empty \"text\" argument — provide your complete final answer"))
		return
	}
	cr.sendRendered(text)
	cr.responded = true
	cr.logger.Info("generator respond delivered", "chars", len(text))
	turn.Add(toolResultMsg(call.ID, "delivered"))
}
```

`aiCmds.go` — add fields after `logQuery`:

```go
	// respondTool mirrors the generator's respond_tool config: offer
	// the respond tool on this ephemeral turn.
	respondTool bool
	// responded is set by handleGeneratorRespond; every runTurn variant
	// checks it after executeToolCalls and ends the turn without
	// another API round-trip.
	responded bool
```

`getTools` ephemeral branch gains:

```go
		if cr.respondTool {
			tools = append(tools, generatorRespondToolDef())
		}
```

The four completion checks (after each `executeToolCalls`/`handleToolCallResponse` site):

1. `runTurnResponsesStream` (after line 823 `cr.executeToolCalls(turn, toolCalls)`):

```go
	if cr.responded {
		return responsesStreamResult{done: true, currentResponseID: currentResponseID, usePrevID: usePrevID, emptyRetries: emptyRetries}
	}
```

2. `runTurnStream` (after line 1089 `cr.executeToolCalls(turn, accumulatedToolCalls)`):

```go
	if cr.responded {
		return true, emptyRetries
	}
	return false, emptyRetries
```

3. `runTurn` (after line 1186 `cr.handleToolCallResponse(...)`):

```go
		if cr.responded {
			return true
		}
```

4. `runTurnResponses` (after line 1618 `cr.handleToolCallResponse(...)`):

```go
		if cr.responded {
			return true
		}
```

`generators.go` `generator()` — after `runner.ephemeral = true`:

```go
	runner.respondTool = cfg.RespondTool
```

`config.go:641` hidden default and the `config_test.go` expectation gain `"respond"`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestHandleGeneratorRespond|TestGetToolsEphemeral|TestRunTurnRespond|TestLoadConfigDirGenerators|TestRunTurnStreamToolCallSendsStreamedTextOnce' .`
Expected: PASS — including the existing non-ephemeral streaming pin (responded stays false on chat turns).

- [ ] **Step 5: Full gate + commit**

```bash
go build ./... && go vet ./... && go fmt ./... && go test ./...
git add config.go config_test.go generators.go generators_test.go aiCmds.go aiStreamOutput_test.go
git commit -m "feat(generators): respond tool — config gate, handler, turn completion in all four loop variants"
```

---

### Task 5: suppression — quiet intermediates, deferred streaming emission

**Files:**
- Modify: `aiCmds.go` (`handleToolCallResponse` 684-690; `runTurnStream` 852, 989, 903, 1013, 1040, 1085; `callResponsesStream` 1646-1649, 1680, 1700, 1717, 1726; `runTurnResponsesStream` final branch ~807-813)
- Test: `aiStreamOutput_test.go`, `generators_test.go`

**Interfaces:**
- Consumes: `cr.ephemeral` (existing), `cr.responded` (Task 4), `sendFinalText`/`sendRendered` (existing).
- Produces: the output policy — ephemeral turns never auto-send intermediate tool-loop text; streaming defers emission (final text via `sendFinalText`, once, complete); non-ephemeral paths byte-identical.

- [ ] **Step 1: Write the failing tests**

Append to `aiStreamOutput_test.go`:

```go
// TestRunTurnStreamEphemeralSuppressesIntermediateText: on an
// ephemeral turn, text streamed before a tool call never reaches IRC;
// only the final iteration's text is sent, once, complete (deferred
// emission — finality is unknowable mid-stream).
func TestRunTurnStreamEphemeralSuppressesIntermediateText(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	toolStream := streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"let me fetch"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"nonexistent_tool","arguments":"{}"}}]},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		"data: [DONE]\n\n"
	finalStream := streamChunk(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"THE FINAL"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		"data: [DONE]\n\n"

	var reqs int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&reqs, 1) == 1 {
			fmt.Fprint(w, toolStream)
			return
		}
		fmt.Fprint(w, finalStream)
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		RenderMarkdown: true, StreamTimeout: 5 * time.Second,
	}, 0, outputCh)
	cr.ephemeral = true

	done := make(chan struct{})
	go func() { cr.runTurn(newEphemeralTurnContext([]ChatMessage{{Role: RoleUser, Content: "sum"}})); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	var sent []string
collect:
	for {
		select {
		case s := <-outputCh:
			sent = append(sent, s)
		default:
			break collect
		}
	}
	joined := strings.Join(sent, "\n")
	assert.NotContains(t, joined, "let me fetch", "intermediate chatter is suppressed")
	assert.Contains(t, joined, "THE FINAL", "the final answer is delivered once")
	assert.EqualValues(t, 2, atomic.LoadInt32(&reqs), "the nonexistent tool result drives a second iteration")
}

// TestRunTurnStreamEphemeralErrorDeliversPartialText: a stream that
// dies mid-flight delivers whatever text arrived before the error
// notice — parity with the non-ephemeral "deliver what arrived"
// philosophy — and does not misbehave when nothing arrived.
func TestRunTurnStreamEphemeralErrorDeliversPartialText(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial wisdom"},"finish_reason":null}]}`))
		// then the connection dies without [DONE]
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		StreamTimeout: 5 * time.Second,
	}, 0, outputCh)
	cr.ephemeral = true

	done := make(chan struct{})
	go func() { cr.runTurn(newEphemeralTurnContext([]ChatMessage{{Role: RoleUser, Content: "sum"}})); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	var sent []string
	for {
		select {
		case s := <-outputCh:
			sent = append(sent, s)
		default:
			goto check
		}
	}
check:
	joined := strings.Join(sent, "\n")
	assert.Contains(t, joined, "partial wisdom", "deferred text is delivered before the error notice")
}
```

Append to `generators_test.go`:

```go
// TestHandleToolCallResponseEphemeralSuppressesIntermediateText:
// non-streaming intermediate text enters the turn history but never
// IRC; the non-ephemeral send is pinned by existing aiCmds tests.
func TestHandleToolCallResponseEphemeralSuppressesIntermediateText(t *testing.T) {
	setupNoticesDefaults(t)
	out := make(chan string, 16)
	for _, ephemeral := range []bool{true, false} {
		// ToolVerbose=false: executeToolCalls would otherwise emit a
		// per-tool notice into `out` for the nonexistent tool, breaking
		// the exactly-one-send assertion below.
		cr := &chatRunner{
			cfg: AIConfig{Name: "x", ToolVerbose: boolPtr(false)},
			network: Network{Name: "testnet"},
			logger: newTestLogger(), ctx: context.Background(),
			outputCh: out, ephemeral: ephemeral,
		}
		turn := newEphemeralTurnContext(nil)
		turn.Add(ChatMessage{Role: RoleUser, Content: "go"})
		cr.handleToolCallResponse(turn, "preamble text", []ToolCall{{
			ID: "c1", Type: "function",
			Function: FunctionDefinition{Name: "nonexistent_tool", Arguments: "{}"},
		}}, "")

		msgs := turn.Messages()
		require.GreaterOrEqual(t, len(msgs), 2)
		assert.Equal(t, "preamble text", msgs[1].Content, "text enters the turn history in both modes")
	}
	// drain: only the non-ephemeral run's preamble was sent
	var sent []string
	for {
		select {
		case s := <-out:
			sent = append(sent, s)
		default:
			goto done
		}
	}
done:
	require.Len(t, sent, 1, "exactly one send — the non-ephemeral one")
	assert.Contains(t, sent[0], "preamble text")
}
```

Also add a Responses-API suppression test mirroring `TestRunTurnStreamEphemeralSuppressesIntermediateText` but with `ResponsesAPI: true` and the SSE event shapes used by the existing responses stream tests (read `responses_test.go` for the exact `response.output_text.delta` / `response.completed` event JSON and mirror them): two iterations — first carries text + a function_call item, second carries final text — assert only the final text reached the channel. Name it `TestRunTurnResponsesStreamEphemeralSuppressesIntermediateText`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'EphemeralSuppress|EphemeralErrorDelivers' .`
Expected: FAIL — the intermediate text still streams/sends.

- [ ] **Step 3: Implement**

`handleToolCallResponse` (line 684-690) — gate the send:

```go
	if text != "" && !cr.ephemeral {
		// Ephemeral (generator) turns suppress intermediate tool-loop
		// text: it enters the turn history (the model sees its own
		// words) and the logs, but never IRC — the channel only sees
		// the curated answer (respond tool or the final no-tool-call
		// iteration).
		t := ExtractFinalText(text)
		if cr.cfg.RenderMarkdown {
			t = markdowntoirc.MarkdownToIRC(t)
		}
		cr.sendIRC(t)
	}
```

`runTurnStream` — five edits:

1. Renderer init (line 852): `if cr.cfg.RenderMarkdown && !cr.ephemeral {`
2. Live delta (line 989): `if !cr.ephemeral { sOut.HandleDelta(textDelta, cr.sendIRC) }` (the `fullContent += textDelta` accumulation at 988 stays ungated).
3. Stream-error path (line 903) — replace `sOut.Flush(cr.sendIRC)`:

```go
				if cr.ephemeral {
					// Deferred emission: deliver what arrived (parity
					// with the non-ephemeral flush), complete.
					cr.sendFinalText(fullContent)
				} else {
					sOut.Flush(cr.sendIRC)
				}
```

4. Idle-timeout path (line 1013) — same replacement.
5. `flushStreamedOutput` (line 1040) — replace `sOut.Flush(cr.sendIRC)`:

```go
		if cr.ephemeral {
			// Deferred emission: the final iteration's text, complete.
			// (content may be the "..." sentinel — sendFinalText skips
			// it exactly like the non-streaming path.)
			cr.sendFinalText(content)
		} else {
			sOut.Flush(cr.sendIRC)
		}
```

6. Tool-call branch (line 1085) — replace `sOut.Flush(cr.sendIRC)`:

```go
	if cr.ephemeral {
		// Suppressed from IRC (intermediate iteration); keep it visible
		// in logs for parity.
		if fullContent != "" {
			cr.logger.Info(FormatOutput(fullContent))
		}
	} else {
		// Flush ONLY the renderer's unsent tail (see the comment this
		// replaces — non-ephemeral behavior is byte-identical).
		sOut.Flush(cr.sendIRC)
	}
```

`callResponsesStream` — five edits:

1. Renderer init (line 1647): `if cr.cfg.RenderMarkdown && !cr.ephemeral {`
2. Live delta (line 1700): `if !cr.ephemeral { sOut.HandleDelta(textDelta, cr.sendIRC) }`
3. Error path (line 1680): `if cr.ephemeral { cr.sendFinalText(fullText) } else { sOut.Flush(cr.sendIRC) }`
4. Timeout path (line 1717): same.
5. `streamDone` flush (line 1726): `if !cr.ephemeral { sOut.Flush(cr.sendIRC) }` — the caller now classifies (see next edit).

`runTurnResponsesStream` no-tool-calls branch — add deferred emission just before the `return` at line 813 (after `assistantMsg.Content` was finalized by the retry handling):

```go
		if cr.ephemeral {
			// Deferred emission (suppression): the collector no longer
			// flushes; the final iteration's text is sent here,
			// complete — mirroring the non-streaming Responses path.
			cr.sendFinalText(text)
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'EphemeralSuppress|EphemeralErrorDelivers|TestRunTurnStreamToolCallSendsStreamedTextOnce|TestRunTurnRespondEndsTurnSingleRequest' .`
Expected: PASS — the non-ephemeral pins stay green (regression proof).

- [ ] **Step 5: Full gate + commit**

```bash
go build ./... && go vet ./... && go fmt ./... && go test ./...
git add aiCmds.go aiStreamOutput_test.go generators_test.go
git commit -m "feat(generators): quiet intermediates — suppression and deferred streaming emission on ephemeral turns"
```

---

### Task 6: notices cleanup — the `[generators]` section dies with injection

**Files:**
- Modify: `notices.go` (delete `GeneratorNotices` struct lines 196-204, the `Generators` field on `NoticesConfig` line 30, the three `setNoticesDefaults` entries lines ~493-511)
- Modify: `notices_test.go` (delete `TestGeneratorNoticesDefaults` line 529)
- Modify: `config/notices.toml` (delete the `[generators]` reference entries + commented defaults + live section — read the file first, follow its layout)
- Test: `notices_test.go`

**Interfaces:**
- Consumes: Task 3 removed every `getNotices().Generators` call site (generator() no longer references them) — verify with `grep -n 'Generators\.' **/*.go` before starting: only notices.go/notices_test.go hits remain.
- Produces: `NoticesConfig` without a `Generators` field; stale configs carrying `[generators]` load fine.

- [ ] **Step 1: Write the failing test**

In `notices_test.go`, replace the deleted test with:

```go
// TestNoticesLoadWithStaleGeneratorsSection: old notices.toml files
// still carrying a [generators] section load fine — unknown TOML keys
// are ignored; the section is dead, not fatal.
func TestNoticesLoadWithStaleGeneratorsSection(t *testing.T) {
	dir := createTestConfigDir(t, `
[queue]
msg = "q"

[generators]
no_activity = "stale"
truncated = "stale"
window_too_large = "stale"
`)
	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "q", cfg.Notices.Queue.Msg)
	setNoticesDefaults(&cfg.Notices)
	assert.NotContains(t, fmt.Sprintf("%+v", cfg.Notices), "stale",
		"the dead section's values must not leak anywhere")
}
```

(Read `createTestConfigDir`'s notices handling first — if notices load from a separate file/dir convention, mirror `TestLoadConfigDirNotices`-style neighbors exactly.)

- [ ] **Step 2: Run test to verify behavior**

Run: `go test -run 'TestNoticesLoadWithStaleGeneratorsSection' .`
Expected: PASS already (unknown keys ignored) — this test PINS the forward-compatibility; the compile error from the struct deletion is the real red. Proceed to Step 3 and let the deletion turn the package red first if `TestGeneratorNoticesDefaults` references vanish with it — order: delete struct → package breaks → delete stale test → green.

- [ ] **Step 3: Delete the dead code**

In `notices.go`: remove the `GeneratorNotices` struct + its comment (196-204), the `Generators GeneratorNotices \`toml:"generators"\`` field (line 30), and the three default assignments in `setNoticesDefaults` (493-511 area, including their comments). In `notices_test.go`: delete `TestGeneratorNoticesDefaults`. In `config/notices.toml`: delete the `[generators]` block (reference lines + commented defaults + live section). Run `grep -rn 'Generators\.' *.go` — expect zero hits.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestNotices|TestLoadConfigDir' .`
Expected: PASS.

- [ ] **Step 5: Full gate + commit**

```bash
go build ./... && go vet ./... && go fmt ./... && go test ./...
git add notices.go notices_test.go config/notices.toml
git commit -m "refactor(generators): delete the dead [generators] notices section"
```

---

### Task 7: docs and shipped config

**Files:**
- Modify: `AGENTS.md` (rewrite the `**Generators**` Architecture bullet)
- Modify: `config/generators.toml` (reference block + example + live `[summary]` rewrite)
- Test: `config_test.go` (one scratch load verification — see Step 3)

**Interfaces:**
- Consumes: everything shipped by Tasks 1-6 (guard-test names must exist and claims must be verified against code before writing them — house rule: AGENTS.md carries verified facts only).
- Produces: documentation matching the tool-driven model.

- [ ] **Step 1: Rewrite `config/generators.toml`**

Update the header comment and reference block:
- `[name.log]` semantics: "configures the `query_channel_logs` tool offered to the model — `window` is the tool's default window, `max_tokens` the per-call transcript budget, `events` the transcript shape. The model can call the tool multiple times with different windows; caps are host-enforced."
- New option: `respond_tool (bool, default: false) — offer the respond tool; the model submits its final answer through it and the turn ends there. When false (or the model finishes with plain text), the final no-tool-call reply is sent automatically. Intermediate model text is never sent to IRC either way.`
- The `hidden_tools` note: both generator tools are hidden by default; `disabled_builtin_tools` DOES apply to them (fix the existing "Not useful for generators" line that says it doesn't).
- Keep: the QUIT/NICK network-scoped note, the `[logging] enabled` requirement, the service-reference rules, the duration grammar documentation.

Rewrite the live `[summary]` section to instruct the tool loop and demonstrate `respond_tool`:

```toml
[summary]
description = "Summarize recent channel activity"
service = "local"
model = "qwen3-32b"
streaming = true
rendermarkdown = true
respond_tool = true
prompt = "Summarize recent channel activity."
system = """\
You are {{.BotNick}}, summarizing recent activity in {{.Channel}} on {{.Network}}.
First call query_channel_logs to retrieve the channel transcript — map any
time range in the request (e.g. "7d", "since yesterday", "the last 12 hours")
to the window argument, defaulting to the tool's default when none is given.
Then summarize: cover the main topics, notable events, and who participated;
be concise but specific. If the result begins with a truncation marker, say
that coverage starts mid-window. Deliver your complete summary with the
respond tool.\
"""
[summary.log]
window = "24h"
events = ["PRIVMSG", "NOTICE", "TOPIC", "KICK"]
max_tokens = 60000
```

- [ ] **Step 2: Rewrite the AGENTS.md bullet**

Replace the `**Generators**` bullet (after the notices.go bullet) with a dense, factual bullet in sibling style covering: tool-driven retrieval (`query_channel_logs`, per-run def embedding the default window, dispatch via the `generatorTools` registry branch in `executeToolCalls`, hidden-by-default + `disabled_builtin_tools` applies, multi-call within `maxToolIterations`, all caps host-side, `[name.log]` = tool config); output policy (ephemeral suppression of intermediate tool-loop text, deferred streaming emission — final text via `sendFinalText` once, stream-death delivers what arrived, user-stop delivers nothing; `respond_tool` gate, `respond` ends the turn via the `responded` flag checked in all four loop variants, fallback = final no-tool-call reply); api-log parity (per-run unique negative ids via `nextEphemeralAPILogID`, `RestoreSession` at runner setup, `apiLogID()` seam, transport/breadcrumb/incident follow it, session-0 guards untouched, SessionID-0 usage rows unchanged); the dead notices section; guard-test names (every test this plan adds, spot-checked to exist). Also update the notices.toml entry in the config-file list if it mentions `[generators]`.

- [ ] **Step 3: Verify the shipped config loads**

Same discipline as the v1 Task 7: write a temporary test (never committed) that runs `loadConfigDir` on a copy of `config/` minus the two pre-existing broken files (`chats.toml`'s `contains` template func, `tools.toml`'s undefined img-mcp refs — out of scope, see ledger), assert the generators parse (`summary` present, `RespondTool` true, log spec intact), run it, delete it. Confirm `git status` shows only the two tracked doc files changed.

- [ ] **Step 4: Full gate + commit**

```bash
go build ./... && go vet ./... && go fmt ./... && go test ./...
git add AGENTS.md config/generators.toml
git commit -m "docs(generators): tool-driven retrieval, respond tool, api-log parity — AGENTS.md and shipped config"
```

---

## Plan self-review notes (for the executor)

- Spec coverage: §1 retrieval → Tasks 2+3; §2 output → Tasks 4+5; §3 api-log parity → Tasks 1+3; §4 config/docs → Tasks 6+7 (+4's config field); §5 queue → explicitly no task (constraint); §6 testing → the test steps above + the Review Focus pins.
- Ordering: Task 3 sets `runner.respondTool` (Task 4 field) — if executing strictly in order, Task 3's executor snippet omits that one line until Task 4 adds it; Task 3's tests must not assert `respondTool`. The plan shows Task 3's exact scope; do not pull Task 4 lines forward.
- The `handleLogQueryForTest` helper drives the REAL dispatch (`cr.executeToolCalls`), not a reimplementation — it pins the branch, the disabled check, and the notice path together.
- Any place the plan's literal code contradicts live code (renamed helpers, shifted line numbers, fixture drift), verify against the live file and record the deviation in your report — the plan's line numbers are from commit `e998a29` and will drift.
