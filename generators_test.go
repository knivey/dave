package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func drainGenOutput(t *testing.T, ch chan string, n int) []string {
	t.Helper()
	return drainOutput(t, ch, n, 2*time.Second)
}

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

func TestGeneratorNonLogArgsPassedThrough(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	var gotBody string
	outputCh, _ := withGeneratorRunner(t, AIConfig{Model: "qwen3"}, func(w http.ResponseWriter, r *http.Request) {
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
	// sanitizeKey replaces non-alphanumerics with _ — "#chan" becomes
	// "_chan", hence the double underscore in the filename.
	assert.Contains(t, filepath.Base(path), "testnet__chan_user7_",
		"filename carries the runner identity shape")
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
	// The configured default window AND the current time are embedded in
	// the per-run description (the range-mode grounding).
	assert.Contains(t, tools[0].Function.Description, `default 24h)`)
	assert.Regexp(t, `The current time is \w+day 20\d\d-\d\d-\d\d \d\d:\d\d`, tools[0].Function.Description)
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
	result := lastToolResultText(t, turn)
	assert.Contains(t, result, "omitted to fit")
	// The header must be SELF-DISCLOSING when clipped: kept/total/dropped
	// + budget in the header line itself, so the model cannot miss the
	// truncation even without parsing the marker (and the header stops
	// overstating the pre-budget total as the delivered line count).
	assert.Contains(t, result,
		"1 of 41 lines (40 dropped to fit the 60000-token budget), 10 tokens")
}

func TestHandleGeneratorLogQueryHeaderHonestWhenNotTruncated(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		return &LogWindowResult{
			Lines:  []string{"[09:00] <a> hi"},
			Tokens: 5, TotalLines: 1,
			FirstKept: time.Date(2026, 10, 9, 9, 0, 0, 0, time.Local),
			LastKept:  time.Date(2026, 10, 9, 9, 30, 0, 0, time.Local),
		}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	turn := handleLogQueryForTest(t, cr, `{}`)
	// Unclipped: the header keeps its exact historical shape.
	assert.Contains(t, lastToolResultText(t, turn),
		"(1 lines, 5 tokens, covering 09:00 to 09:30):")
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

func TestHandleGeneratorLogQueryRange(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	var gotSpec LogQuerySpec
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		gotSpec = spec
		return &LogWindowResult{
			Lines:  []string{"[18:00] <a> evening"},
			Tokens: 4, TotalLines: 1,
			FirstKept: time.Date(2026, 10, 6, 18, 0, 0, 0, time.Local),
			LastKept:  time.Date(2026, 10, 6, 23, 59, 0, 0, time.Local),
		}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	turn := handleLogQueryForTest(t, cr, `{"from":"2026-10-06 18:00","to":"2026-10-06 23:59"}`)

	assert.Equal(t, "2026-10-06 18:00", gotSpec.From, "from rides to the fetch")
	assert.Equal(t, "2026-10-06 23:59", gotSpec.To, "to rides to the fetch")
	result := lastToolResultText(t, turn)
	assert.Contains(t, result,
		"Channel activity for #st on testnet, from 2026-10-06 18:00 to 2026-10-06 23:59 (1 lines, 4 tokens, covering 18:00 to 23:59):")
	assert.Contains(t, result, "[18:00] <a> evening")
}

func TestHandleGeneratorLogQueryRangeToDefaultsNow(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	var gotSpec LogQuerySpec
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		gotSpec = spec
		return &LogWindowResult{Lines: []string{"x"}, Tokens: 1, TotalLines: 1}, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	turn := handleLogQueryForTest(t, cr, `{"from":"2026-10-06 18:00"}`)

	assert.Equal(t, "2026-10-06 18:00", gotSpec.From)
	assert.Empty(t, gotSpec.To, "omitted to stays empty — the fetch defaults it to now")
	// The header shows the RESOLVED range; the to value is the live clock, so
	// pin the from side and the wording, not the exact minute.
	assert.Contains(t, lastToolResultText(t, turn), "from 2026-10-06 18:00 to 2")
}

func TestHandleGeneratorLogQueryWindowAndRangeError(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		t.Fatal("fetch must not run when window and from/to are both given")
		return nil, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	turn := handleLogQueryForTest(t, cr, `{"window":"12h","from":"2026-10-06 18:00","to":"2026-10-06 23:59"}`)
	assert.Contains(t, lastToolResultText(t, turn), "either window or from/to")
}

func TestHandleGeneratorLogQueryRangeValidation(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		t.Fatal("fetch must not run for invalid range args")
		return nil, nil
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})

	turn := handleLogQueryForTest(t, cr, `{"to":"2026-10-06 23:59"}`)
	assert.Contains(t, lastToolResultText(t, turn), `"from"`)

	turn = handleLogQueryForTest(t, cr, `{"from":"2026-10-06"}`)
	assert.Contains(t, lastToolResultText(t, turn), "2006-01-02 15:04")

	turn = handleLogQueryForTest(t, cr, `{"from":"2026-10-07 18:00","to":"2026-10-06 23:59"}`)
	assert.Contains(t, lastToolResultText(t, turn), "after")
}

// TestHandleGeneratorLogQueryFetchError pins the generic fetch-failure
// branch (carried finding from Task 3's review): a fetch error that is
// NOT the row cap surfaces as "error: <err>" tool-result content the
// model can act on — never the row-cap wording.
func TestHandleGeneratorLogQueryFetchError(t *testing.T) {
	setupNoticesDefaults(t)
	orig := fetchChannelLogFn
	fetchChannelLogFn = func(spec LogQuerySpec, network, channelRaw, channelNorm, model string, now time.Time) (*LogWindowResult, error) {
		return nil, fmt.Errorf("log database is locked")
	}
	t.Cleanup(func() { fetchChannelLogFn = orig })

	cr := newGeneratorToolTestRunner(true, &generatorLogQuery{
		spec:       LogQuerySpec{Window: "24h", MaxTokens: 60000, Events: defaultLogEvents},
		channelRaw: "#St", channel: "#st", name: "summary",
	})
	turn := handleLogQueryForTest(t, cr, `{}`)
	result := lastToolResultText(t, turn)
	assert.Contains(t, result, "error: log database is locked")
	assert.NotContains(t, result, "row cap", "generic fetch errors must not read as the row-cap branch")
	assert.NotContains(t, result, "narrower window")
}

// TestLogCoverageRange pins the INFO log field's rendering: the bare
// "HH:MM to HH:MM" range, with NO leading comma — the comma belongs to
// the tool-result header fragment (", covering …"), and sharing one
// string printed "coverage: , covering 01:02 to 00:41" in production
// logs (observed 2026-10-10, post-merge).
func TestLogCoverageRange(t *testing.T) {
	both := &LogWindowResult{
		FirstKept: time.Date(2026, 10, 10, 1, 2, 0, 0, time.Local),
		LastKept:  time.Date(2026, 10, 11, 0, 41, 0, 0, time.Local),
	}
	assert.Equal(t, "01:02 to 00:41", logCoverageRange(both))
	assert.NotContains(t, logCoverageRange(both), ",")

	for name, lw := range map[string]*LogWindowResult{
		"both zero":  {},
		"last zero":  {FirstKept: both.FirstKept},
		"first zero": {LastKept: both.LastKept},
	} {
		assert.Empty(t, logCoverageRange(lw), "%s: either endpoint unset → no range", name)
	}
}

// --- respond tool ---

func TestGetToolsEphemeralOffersRespondWhenEnabled(t *testing.T) {
	setupNoticesDefaults(t)
	cr := newGeneratorToolTestRunner(true, nil)
	cr.respondTool = true
	tools := cr.getTools()
	require.Len(t, tools, 1)
	assert.Equal(t, respondToolName, tools[0].Function.Name)
	// FunctionDefinition.Parameters is `any` — assert the map shape, then
	// the required list (the brief's verbatim line indexed `any` directly,
	// which does not compile; same fix as the log-tool test above).
	params, ok := tools[0].Function.Parameters.(map[string]any)
	require.True(t, ok, "parameters must be an object schema")
	assert.Equal(t, []string{"text"}, params["required"])
}

func respondToolCall(id, argsJSON string) ToolCall {
	return ToolCall{
		ID:       id,
		Type:     "function",
		Function: FunctionCall{Name: respondToolName, Arguments: argsJSON},
	}
}

func TestHandleGeneratorRespondSendsAndFlags(t *testing.T) {
	setupNoticesDefaults(t)
	out := make(chan string, 16)
	cr := &chatRunner{
		cfg:     AIConfig{Name: "summary", ToolVerbose: boolPtr(false)},
		network: Network{Name: "testnet"},
		logger:  newTestLogger(), ctx: context.Background(),
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
		cfg:     AIConfig{Name: "summary", ToolVerbose: boolPtr(false)},
		network: Network{Name: "testnet"},
		logger:  newTestLogger(), ctx: context.Background(),
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
		cfg:     AIConfig{Name: "chat", ToolVerbose: boolPtr(false)},
		network: Network{Name: "testnet"},
		logger:  newTestLogger(), ctx: context.Background(),
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
		cfg:     AIConfig{Name: "summary", ToolVerbose: boolPtr(false)},
		network: Network{Name: "testnet"},
		logger:  newTestLogger(), ctx: context.Background(),
		outputCh: out, ephemeral: true,
	}
	turn := newEphemeralTurnContext(nil)
	turn.Add(ChatMessage{Role: RoleUser, Content: "go"})
	cr.executeToolCalls(turn, []ToolCall{
		{ID: "call_1", Type: "function", Function: FunctionCall{Name: "nonexistent_tool", Arguments: "{}"}},
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
			cfg:     AIConfig{Name: "x", ToolVerbose: boolPtr(false)},
			network: Network{Name: "testnet"},
			logger:  newTestLogger(), ctx: context.Background(),
			outputCh: out, ephemeral: ephemeral,
		}
		turn := newEphemeralTurnContext(nil)
		turn.Add(ChatMessage{Role: RoleUser, Content: "go"})
		// DEVIATION from the brief (house precedent, recorded in
		// task-5-report.md): ToolCall.Function is FunctionCall{Name,
		// Arguments}, not FunctionDefinition (which has no Arguments
		// field — the brief's literal does not compile).
		cr.handleToolCallResponse(turn, "preamble text", []ToolCall{{
			ID: "c1", Type: "function",
			Function: FunctionCall{Name: "nonexistent_tool", Arguments: "{}"},
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
