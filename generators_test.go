package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func genEvent(channel string) girc.Event {
	return girc.Event{
		Source: &girc.Source{Name: "shrew", Ident: "~s", Host: "example.com"},
		Params: []string{channel},
	}
}

// withGeneratorRunner swaps newChatRunnerFn for one returning a real runner
// against an httptest server, restoring it after the test.
func withGeneratorRunner(t *testing.T, cfg AIConfig, handler http.HandlerFunc) (outputCh chan string) {
	t.Helper()
	outputCh = make(chan string, 64)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	orig := newChatRunnerFn
	newChatRunnerFn = func(network Network, client *girc.Client, c AIConfig, ctx context.Context, out chan<- string) chatRunnerInterface {
		cr := newStreamTestRunner(t, server, c, 0, outputCh)
		cr.ctx = ctx
		cr.outputCh = out
		return cr
	}
	t.Cleanup(func() { newChatRunnerFn = orig })
	return outputCh
}

func drainGenOutput(t *testing.T, ch chan string, n int) []string {
	t.Helper()
	return drainOutput(t, ch, n, 2*time.Second)
}

func TestGeneratorLogCommandBuildsEphemeralTurn(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var gotPath, gotBody string
	var calls int32
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"all quiet"},"finish_reason":null}]}`)+
			streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)+
			"data: [DONE]\n\n")
	})

	// Seed a log query result via the injectable fetch fn.
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local)
	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, n2 time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{
			Lines:  []string{"[11:00] <alice> hi"},
			Tokens: 5, TotalLines: 1,
			FirstKept: now.Add(-time.Hour), LastKept: now.Add(-time.Hour),
			Files: []string{"x.db"},
		}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

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

	lines := drainGenOutput(t, outputCh, 16)
	joined := strings.Join(lines, "\n")
	assert.Equal(t, int32(1), atomic.LoadInt32(&calls), "exactly one LLM call")
	assert.Contains(t, joined, "all quiet")
	assert.Contains(t, gotPath, "chat/completions")
	assert.Contains(t, gotBody, "focus on drama", "focus text is the instruction")
	assert.Contains(t, gotBody, "[11:00] <alice> hi", "transcript appended to user message")
	// DEVIATION from the task brief (recorded in task-5-report.md): the brief
	// asserted Contains here ("default instruction used when no focus") — a
	// stale expectation from before plan fix 94da76f merged the args into
	// "12h focus on drama". With focus text present, the design spec
	// (§Executor item 5: "focus text, else cfg.Prompt, else builtin default")
	// means the configured default instruction must NOT be sent.
	assert.NotContains(t, gotBody, "Summarize the following channel activity.", "default instruction must not be used when focus text is given")
	assert.Contains(t, gotBody, "Channel activity for #chan on testnet", "transcript header names channel/network")

	var messagesAfter int64
	require.NoError(t, theDB.Model(&Message{}).Count(&messagesAfter).Error)
	assert.Equal(t, messagesBefore, messagesAfter, "ephemeral turn persists nothing")
}

func TestGeneratorNoActivitySendsNoticeWithoutLLMCall(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var calls int32
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	})

	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1}, "6h")
	}()
	<-done

	lines := drainGenOutput(t, outputCh, 4)
	joined := strings.Join(lines, "\n")
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls), "no LLM call on empty window")
	assert.Contains(t, joined, "No logged activity", "no_activity notice sent")
}

func TestGeneratorTruncationNotice(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)+"data: [DONE]\n\n")
	})
	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{Lines: []string{"[11:00] <a> x"}, Tokens: 59000, Truncated: true,
			DroppedLines: 120, TotalLines: 420}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{MaxTokens: 60000}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1})
	}()
	<-done
	lines := drainGenOutput(t, outputCh, 8)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "420", "truncation notice carries totals")
	assert.Contains(t, joined, "120", "dropped count present")
	// Hand-built result with zero FirstKept/LastKept: the coverage var must
	// stay empty rather than render a bogus "00:00 to 00:00" range.
	assert.NotContains(t, joined, "00:00", "zero FirstKept/LastKept must not render a bogus coverage range")
}

func TestGeneratorTruncationNoticeCoverage(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)+"data: [DONE]\n\n")
	})
	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{Lines: []string{"[11:00] <a> x"}, Tokens: 59000, Truncated: true,
			DroppedLines: 120, TotalLines: 420,
			FirstKept: time.Date(2026, 10, 8, 14, 32, 0, 0, time.Local),
			LastKept:  time.Date(2026, 10, 8, 16, 45, 0, 0, time.Local)}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{MaxTokens: 60000}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1})
	}()
	<-done
	lines := drainGenOutput(t, outputCh, 8)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "14:32 to 16:45", "truncation notice carries the actual coverage range")
}

func TestGeneratorTruncationNoticeCoverageRequiresBothTimes(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)+"data: [DONE]\n\n")
	})
	origFetch := fetchChannelLogFn
	// FirstKept only (hand-built result): a partial range must not render —
	// a FirstKept-only guard would produce "14:32 to 00:00". Production
	// results always set both times together.
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{Lines: []string{"[11:00] <a> x"}, Tokens: 59000, Truncated: true,
			DroppedLines: 120, TotalLines: 420,
			FirstKept: time.Date(2026, 10, 8, 14, 32, 0, 0, time.Local)}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{MaxTokens: 60000}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1})
	}()
	<-done
	lines := drainGenOutput(t, outputCh, 8)
	joined := strings.Join(lines, "\n")
	assert.NotContains(t, joined, "to 00:00", "partial times must not render a half range")
	assert.NotContains(t, joined, "14:32", "FirstKept alone must not leak into the coverage range")
}

func TestGeneratorNoFocusUsesConfiguredPrompt(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var gotBody string
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)+"data: [DONE]\n\n")
	})

	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{
			Lines:  []string{"[11:00] <alice> hi"},
			Tokens: 5, TotalLines: 1,
			Files: []string{"x.db"},
		}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	// Zero LogQuerySpec on purpose: the executor must re-apply log-query
	// defaults locally (production specs are defaulted at load time), or the
	// transcript header renders "last 0s".
	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary", Model: "qwen3", Streaming: true,
		StreamTimeout: 5 * time.Second, Timeout: 10 * time.Second},
		Prompt: "CUSTOM DEFAULT",
		Log:    &LogQuerySpec{}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1})
	}()
	<-done
	drainGenOutput(t, outputCh, 8)
	assert.Contains(t, gotBody, "CUSTOM DEFAULT", "no focus text -> cfg.Prompt is the instruction")
	assert.Contains(t, gotBody, "Channel activity for #chan on testnet", "transcript header also present")
	assert.Contains(t, gotBody, "last 1d0h", "zero log spec defaults applied at the executor (24h window)")
	assert.NotContains(t, gotBody, "Summarize the following channel activity.", "builtin default must not be used when cfg.Prompt is set")
}

func TestGeneratorWindowTooLargeNotice(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var calls int32
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	})
	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return nil, fmt.Errorf("window too large: %d rows (cap %d): %w", 2000000, logQueryRowCap, errLogWindowTooLarge)
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })

	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1}, "3650d")
	}()
	<-done
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls))
	lines := drainGenOutput(t, outputCh, 4)
	assert.Contains(t, strings.Join(lines, "\n"), "row cap", "window_too_large notice sent")
}

func TestGeneratorNonLogArgsPassedThrough(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var gotBody string
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"qwen3","choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)+"data: [DONE]\n\n")
	})

	// DEVIATION from the task brief (recorded in task-5-report.md): the
	// brief's cfg carried no Timeout — runTurn does
	// context.WithTimeout(ctx, 0), which is already expired, and Go's http
	// transport aborts before dialing (roundTrip's ctx.Done check), so the
	// request would never reach the handler and gotBody would stay empty.
	// Streaming+StreamTimeout+Timeout let the handler's SSE response be
	// consumed as written.
	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "fakenews", Model: "qwen3", System: "snarky",
		Streaming: true, StreamTimeout: 5 * time.Second, Timeout: 10 * time.Second}, Prompt: "default topic"}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1}, "the moon landing")
	}()
	<-done
	drainGenOutput(t, outputCh, 8)
	assert.Contains(t, gotBody, "the moon landing", "args become the user message verbatim")
}

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
		ID:       id,
		Type:     "function",
		Function: FunctionCall{Name: queryChannelLogsToolName, Arguments: argsJSON},
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
	// FunctionDefinition.Parameters is `any` — assert the map shape, then
	// the required list (brief's verbatim line indexed `any` directly,
	// which does not compile).
	params, ok := tools[0].Function.Parameters.(map[string]any)
	require.True(t, ok, "parameters must be an object schema")
	assert.Equal(t, []string{}, params["required"], "window must be optional")
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
			Lines:  []string{"[09:00] <a> hello"},
			Tokens: 5, TotalLines: 1,
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
	assert.Contains(t, result, "Channel activity for #st on testnet, last 7d0h (1 lines, 5 tokens, covering 09:00 to 09:30):")
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
		spec:       LogQuerySpec{Window: "12h", MaxTokens: 60000, Events: defaultLogEvents},
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
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
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
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
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
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
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
			Lines:  []string{"[... 40 earlier lines omitted to fit the 60000-token budget ...]", "[09:00] <a> hi"},
			Tokens: 10, TotalLines: 41, DroppedLines: 40, Truncated: true,
		}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
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

func TestGeneratorErrorsWrappedFromFetch(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)
	var calls int32
	outputCh := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	})
	origFetch := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, raw, norm, model string, now time.Time) (*LogWindowResult, error) {
		return nil, errors.New("disk exploded")
	}
	t.Cleanup(func() { fetchChannelLogFn = origFetch })
	cfg := GeneratorConfig{AIConfig: AIConfig{Name: "summary"}, Log: &LogQuerySpec{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		generator(Network{Name: "testnet"}, nil, genEvent("#chan"), cfg, context.Background(), outputCh, &User{ID: 1})
	}()
	<-done
	assert.Equal(t, int32(0), atomic.LoadInt32(&calls))
	lines := drainGenOutput(t, outputCh, 4)
	assert.Contains(t, strings.Join(lines, "\n"), "disk exploded", "generic fetch errors surface to the user")
}
