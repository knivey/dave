package main

// Wiring tests for BBCode→IRC translation (see MarkdownToIRC/bbcode.go
// for the COLOR-ONLY translator and its unit tests). Commands with
// `bbcode = true` get a model instructed to emit BBCode tags; dave
// converts them to IRC control codes at the EMISSION layer only (session
// storage stays raw), BEFORE the markdown renderer on every path. These
// tests pin the CALL SITES: streamOutput, sendRendered, the tool-loop
// emission, the two streaming variants, and legacy completions. Style
// toggles were deleted (owner ruling, Nov 2026): toggle tokens are
// verbatim everywhere, colors still translate — the former *StylesOff
// variants now pin the one permanent mode.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	markdowntoirc "github.com/knivey/dave/MarkdownToIRC"
	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- integration: streamOutput runs the translator BEFORE the renderer ---

func TestStreamOutputBBCode(t *testing.T) {
	t.Run("plain mode split tag across deltas", func(t *testing.T) {
		so := streamOutput{bb: markdowntoirc.NewBBCodeStream(nil)}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[col", send)
		so.HandleDelta("or=red]line1\n", send)
		assert.Equal(t, []string{"\x0304line1\n"}, sent)
		so.HandleDelta("line2[/color] tail", send)
		so.Flush(send)
		assert.Equal(t, []string{"\x0304line1\n", "\x0304line2\x03 tail"}, sent)
	})
	t.Run("renderer mode gets translated codes", func(t *testing.T) {
		so := streamOutput{renderer: markdowntoirc.NewStreamingRenderer(), bb: markdowntoirc.NewBBCodeStream(nil)}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[color=red]red **bold**[/color]\n\nplain para\n\n", send)
		so.Flush(send)
		joined := strings.Join(sent, "\n")
		assert.Contains(t, joined, "\x0304", "the color survives into the rendered markdown")
		assert.NotContains(t, joined, "[color", "tags never reach the renderer")
	})
	t.Run("flush releases auto-close tail", func(t *testing.T) {
		so := streamOutput{bb: markdowntoirc.NewBBCodeStream(nil)}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[color=red]tail", send)
		assert.Empty(t, sent, "partial line held")
		so.Flush(send)
		assert.Equal(t, []string{"\x0304tail\x03"}, sent)
	})
	t.Run("nil translator passes deltas through raw", func(t *testing.T) {
		so := streamOutput{}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[b]x[/b]\n", send)
		assert.Equal(t, []string{"[b]x[/b]\n"}, sent)
	})
	t.Run("toggles verbatim, colors translate", func(t *testing.T) {
		// The permanent mode since the toggle deletion: [b] is an
		// unknown token through the streaming hook too.
		so := streamOutput{bb: markdowntoirc.NewBBCodeStream(nil)}
		var sent []string
		send := func(s string) { sent = append(sent, s) }
		so.HandleDelta("[b]bold[/b] [color=red]line\n", send)
		so.HandleDelta("more[/color]", send)
		so.Flush(send)
		joined := strings.Join(sent, "\n")
		assert.Contains(t, joined, "[b]bold[/b]", "toggles stay verbatim")
		assert.Contains(t, joined, "\x0304", "colors still translate")
		assert.NotContains(t, joined, "\x02", "no bold codes")
	})
}

// --- integration: emission paths translate raw model output ---

func TestSendRenderedBBCode(t *testing.T) {
	newRunner := func(cfg AIConfig) (*chatRunner, chan string) {
		outputCh := make(chan string, 8)
		return &chatRunner{
			cfg:      cfg,
			network:  Network{Name: "testnet"},
			channel:  "#t",
			ctx:      context.Background(),
			outputCh: outputCh,
			logger:   newTestLogger(),
		}, outputCh
	}
	t.Run("off passes raw through", func(t *testing.T) {
		cr, outputCh := newRunner(AIConfig{})
		cr.sendRendered("[color=red]x[/color]")
		lines := drainOutput(t, outputCh, 4, time.Second)
		assert.Equal(t, []string{"[color=red]x[/color]"}, lines)
	})
	t.Run("on translates before rendering", func(t *testing.T) {
		cr, outputCh := newRunner(AIConfig{BBCode: boolPtr(true), BBCodeStyles: boolPtr(true)})
		cr.sendRendered("[color=red]x[/color]")
		lines := drainOutput(t, outputCh, 4, time.Second)
		assert.Equal(t, []string{"\x0304x\x03"}, lines)
	})
	t.Run("markdown and bbcode combine", func(t *testing.T) {
		cr, outputCh := newRunner(AIConfig{RenderMarkdown: true, BBCode: boolPtr(true), BBCodeStyles: boolPtr(true)})
		cr.sendRendered("**bold** [color=red]red[/color]")
		lines := drainOutput(t, outputCh, 4, time.Second)
		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, "\x02bold\x02", "markdown still converts")
		assert.Contains(t, joined, "\x0304", "bbcode still translates")
		assert.NotContains(t, joined, "[color")
	})
	t.Run("color-only, toggles verbatim", func(t *testing.T) {
		// bbcode = true alone (bbcode_styles unset): the flag is
		// threaded through sendRendered, not just the color decision.
		cr, outputCh := newRunner(AIConfig{BBCode: boolPtr(true)})
		cr.sendRendered("[b]bold[/b] [color=red]red[/color]")
		lines := drainOutput(t, outputCh, 4, time.Second)
		joined := strings.Join(lines, "\n")
		assert.Contains(t, joined, "[b]bold[/b]", "toggles stay verbatim")
		assert.Contains(t, joined, "\x0304red\x03", "colors still translate")
		assert.NotContains(t, joined, "\x02", "no bold codes")
	})
}

// TestRunTurnStreamBBCode: end to end over the SSE harness — tags split
// across chunk boundaries; IRC receives codes only.
func TestRunTurnStreamBBCode(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#st", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	bbStream := streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"[col"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"or=red]line1\n"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"line2[/color] tail"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		"data: [DONE]\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, bbStream)
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		RenderMarkdown: false, StreamTimeout: 5 * time.Second,
		BBCode: boolPtr(true), BBCodeStyles: boolPtr(true),
	}, sid, outputCh)

	done := make(chan struct{})
	go func() { cr.runTurn(newTurnContext(sid, messages)); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	lines := drainOutput(t, outputCh, 32, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "\x0304line1", "line 1 carries its own color (per-message reset)")
	assert.Contains(t, joined, "\x0304line2\x03 tail", "line 2 re-emits the span color")
	assert.NotContains(t, joined, "[color", "no tag reaches IRC")
}

// TestRunTurnStreamBBCodeStylesOff: bbcode = true with bbcode_styles
// unset — the permanent color-only mode on the streaming path: colors
// translate, toggle tags stay verbatim on the wire. (Named for the
// pre-deletion styles split; kept until the bbcode_styles config field
// and its test matrix are removed with it.)
func TestRunTurnStreamBBCodeStylesOff(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#st", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	bbStream := streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"[b]bo"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ld[/b] [color=red]line1\n"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"line2[/color] tail"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		"data: [DONE]\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, bbStream)
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		RenderMarkdown: false, StreamTimeout: 5 * time.Second,
		BBCode: boolPtr(true), // bbcode_styles unset = color-only
	}, sid, outputCh)

	done := make(chan struct{})
	go func() { cr.runTurn(newTurnContext(sid, messages)); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	lines := drainOutput(t, outputCh, 32, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "[b]bold[/b]", "toggle tags stay verbatim")
	assert.Contains(t, joined, "\x0304line2\x03 tail", "colors still translate")
	assert.NotContains(t, joined, "\x02", "no bold codes")
}

// TestRunTurnResponsesStreamBBCode: the Responses-API streaming path is
// separately pinned so a refactor cannot drop one path.
func TestRunTurnResponsesStreamBBCode(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#r", "shrew", "testcmd", "svc", "test-model")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	completedPayload := map[string]any{
		"id": "resp-r", "object": "response", "model": "test-model",
		"output": []any{
			map[string]any{
				"type": "message", "role": "assistant", "id": "msg_1", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "[color=red]done"}},
			},
		},
	}
	completed, err := json.Marshal(map[string]any{"type": "response.completed", "response": completedPayload})
	require.NoError(t, err)
	stream := streamChunk(`{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"[col"}`) +
		streamChunk(`{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"or=red]done"}`) +
		"data: " + string(completed) + "\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}))
	defer server.Close()

	outputCh := make(chan string, 16)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "test-model", ResponsesAPI: true, Streaming: true,
		Timeout: 10 * time.Second, StreamTimeout: 5 * time.Second,
		RenderMarkdown: false, BBCode: boolPtr(true), BBCodeStyles: boolPtr(true),
	}, sid, outputCh)
	cr.ctx = context.Background()

	done := make(chan struct{})
	go func() {
		cr.runTurn(newTurnContext(sid, messages))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	lines := drainOutput(t, outputCh, 32, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "\x0304done\x03", "responses path translates")
	assert.NotContains(t, joined, "[color")
}

// TestRunTurnResponsesStreamBBCodeStylesOff: the Responses-API streaming
// path in the permanent color-only mode — colors translate, toggles
// verbatim. (Named for the pre-deletion styles split; kept until the
// bbcode_styles config field and its test matrix are removed with it.)
func TestRunTurnResponsesStreamBBCodeStylesOff(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#r", "shrew", "testcmd", "svc", "test-model")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	completedPayload := map[string]any{
		"id": "resp-r", "object": "response", "model": "test-model",
		"output": []any{
			map[string]any{
				"type": "message", "role": "assistant", "id": "msg_1", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "[b]bold[/b] [color=red]done"}},
			},
		},
	}
	completed, err := json.Marshal(map[string]any{"type": "response.completed", "response": completedPayload})
	require.NoError(t, err)
	stream := streamChunk(`{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"[b]bold[/b] [col"}`) +
		streamChunk(`{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"or=red]done"}`) +
		"data: " + string(completed) + "\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, stream)
	}))
	defer server.Close()

	outputCh := make(chan string, 16)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "test-model", ResponsesAPI: true, Streaming: true,
		Timeout: 10 * time.Second, StreamTimeout: 5 * time.Second,
		RenderMarkdown: false, BBCode: boolPtr(true), // bbcode_styles unset = color-only
	}, sid, outputCh)
	cr.ctx = context.Background()

	done := make(chan struct{})
	go func() {
		cr.runTurn(newTurnContext(sid, messages))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runTurn did not finish")
	}

	lines := drainOutput(t, outputCh, 32, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "[b]bold[/b]", "toggle tags stay verbatim")
	assert.Contains(t, joined, "\x0304done\x03", "colors still translate")
	assert.NotContains(t, joined, "\x02", "no bold codes")
}

// TestHandleToolCallResponseBBCode: intermediate tool-loop text translates
// before its inline render.
func TestHandleToolCallResponseBBCode(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#t", "shrew", "testcmd", "svc", "m")
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	outputCh := make(chan string, 16)
	cr := &chatRunner{
		cfg:       AIConfig{BBCode: boolPtr(true), BBCodeStyles: boolPtr(true)},
		network:   Network{Name: "testnet"},
		channel:   "#t",
		nick:      "shrew",
		ctx:       context.Background(),
		outputCh:  outputCh,
		logger:    newTestLogger(),
		sessionID: sid,
	}

	cr.handleToolCallResponse(newTurnContext(sid, messages),
		"[color=red]1. TILLANDSIA",
		[]ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "nonexistent_tool", Arguments: "{}"}}},
		reasoningParts{})

	lines := drainOutput(t, outputCh, 8, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "\x0304\x02\x021. TILLANDSIA\x03", "tool-loop text translates (guard on digit text)")
	assert.NotContains(t, joined, "[color")
}

// TestHandleToolCallResponseBBCodeStylesOff: the tool-loop emission hook
// in the permanent color-only mode — colors translate, toggles verbatim.
// (Named for the pre-deletion styles split; kept until the bbcode_styles
// config field and its test matrix are removed with it.)
func TestHandleToolCallResponseBBCodeStylesOff(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#t", "shrew", "testcmd", "svc", "m")
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	outputCh := make(chan string, 16)
	cr := &chatRunner{
		cfg:       AIConfig{BBCode: boolPtr(true)}, // bbcode_styles unset = color-only
		network:   Network{Name: "testnet"},
		channel:   "#t",
		nick:      "shrew",
		ctx:       context.Background(),
		outputCh:  outputCh,
		logger:    newTestLogger(),
		sessionID: sid,
	}

	cr.handleToolCallResponse(newTurnContext(sid, messages),
		"[b]x[/b] [color=red]red text",
		[]ToolCall{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "nonexistent_tool", Arguments: "{}"}}},
		reasoningParts{})

	lines := drainOutput(t, outputCh, 8, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "[b]x[/b]", "toggle tags stay verbatim")
	assert.Contains(t, joined, "\x0304red text\x03", "colors still translate")
	assert.NotContains(t, joined, "\x02", "no bold codes")
}

// TestCompletionBBCode: the legacy completions path translates too.
func TestCompletionBBCode(t *testing.T) {
	setupNoticesDefaults(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/completions":
			_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"[color=red]1. TILLANDSIA[/color]","index":0,"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	prevServices := config.Services
	config.Services = map[string]Service{"localsvc": {BaseURL: server.URL + "/v1"}}
	defer func() { config.Services = prevServices }()

	cfg := AIConfig{Name: "c", Service: "localsvc", Model: "m", Timeout: 10 * time.Second, BBCode: boolPtr(true), BBCodeStyles: boolPtr(true)}
	cfg.ApplyDefaults(config.Services["localsvc"])

	outputCh := make(chan string, 4)
	e := girc.Event{Source: &girc.Source{Name: "shrew"}, Params: []string{"#101"}}

	completion(Network{Name: "testnet"}, nil, e, cfg, context.Background(), outputCh, "hello")

	lines := drainOutput(t, outputCh, 4, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "\x0304\x02\x021. TILLANDSIA\x03")
	assert.NotContains(t, joined, "[color")
}

// TestCompletionBBCodeStylesOff: the legacy completions hook in the
// permanent color-only mode — colors translate, toggles verbatim. (Named
// for the pre-deletion styles split; kept until the bbcode_styles config
// field and its test matrix are removed with it.)
func TestCompletionBBCodeStylesOff(t *testing.T) {
	setupNoticesDefaults(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/completions":
			_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"[b]bold[/b] [color=red]TILLANDSIA[/color]","index":0,"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	prevServices := config.Services
	config.Services = map[string]Service{"localsvc": {BaseURL: server.URL + "/v1"}}
	defer func() { config.Services = prevServices }()

	cfg := AIConfig{Name: "c", Service: "localsvc", Model: "m", Timeout: 10 * time.Second, BBCode: boolPtr(true)}
	cfg.ApplyDefaults(config.Services["localsvc"])

	outputCh := make(chan string, 4)
	e := girc.Event{Source: &girc.Source{Name: "shrew"}, Params: []string{"#101"}}

	completion(Network{Name: "testnet"}, nil, e, cfg, context.Background(), outputCh, "hello")

	lines := drainOutput(t, outputCh, 4, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "[b]bold[/b]", "toggle tags stay verbatim")
	assert.Contains(t, joined, "\x0304TILLANDSIA\x03", "colors still translate")
	assert.NotContains(t, joined, "\x02", "no bold codes")
}

// TestBBCodeNoCodesOnlyLine: a trailing newline with an open color must
// not produce a codes-only PRIVMSG from the auto-close tail (each output
// line is its own message; codes alone render as a blank bot message) —
// review finding 2, Oct 2026. Drives the translated output through the
// production wrapForIRC splitter.
func TestBBCodeNoCodesOnlyLine(t *testing.T) {
	assertNoCodesOnly := func(t *testing.T, in string) {
		t.Helper()
		codeOnly := regexp.MustCompile(`^([\x02\x1D\x1F\x1E]|\x03[0-9]{0,2}(,[0-9]{0,2})?)*$`)
		for _, line := range wrapForIRC(markdowntoirc.TranslateBBCode(in, nil)) {
			if line == "" {
				continue // trailing-newline split artifact; sendToOutput skips these too
			}
			assert.False(t, codeOnly.MatchString(line),
				"line %q is codes-only (blank PRIVMSG) for input %q", line, in)
		}
	}
	t.Run("trailing newline with open styles", func(t *testing.T) {
		assertNoCodesOnly(t, "[b][color=red]x\n")
	})
	t.Run("trailing newline unclosed color only", func(t *testing.T) {
		assertNoCodesOnly(t, "[color=red]x\n")
	})
	t.Run("trailing newline after visible text still colored", func(t *testing.T) {
		// suppression must not lose the auto-close when visible text
		// follows the newline on the same line
		assert.Equal(t, "\x0304x\n\x0304y\x03", markdowntoirc.TranslateBBCode("[color=red]x\ny", nil))
	})
	t.Run("no trailing newline keeps auto-close", func(t *testing.T) {
		// [b] is verbatim now: only the color auto-closes
		assert.Equal(t, "[b]\x0304x\x03", markdowntoirc.TranslateBBCode("[b][color=red]x", nil))
	})
}

// TestBBCodeTagOnlyLineNoCodes: tags on their own line after a newline
// emit no codes there — the codes would be a codes-only PRIVMSG; the
// next visible character's re-emit reconstructs the post-tag state.
// Re-review finding B, Oct 2026.
func TestBBCodeTagOnlyLineNoCodes(t *testing.T) {
	t.Run("close tag alone on line", func(t *testing.T) {
		// [b] never opens anything now — fully verbatim, no codes at all
		assert.Equal(t, "[b]a\n[/b]c", markdowntoirc.TranslateBBCode("[b]a\n[/b]c", nil))
	})
	t.Run("close tag alone at end", func(t *testing.T) {
		assert.Equal(t, "\x0304a\n", markdowntoirc.TranslateBBCode("[color=red]a\n[/color]", nil))
	})
	t.Run("open tag alone on line", func(t *testing.T) {
		// the [b] token is the first VISIBLE text after the newline:
		// the color re-emit lands on it, and it carries no toggle code
		assert.Equal(t, "\x0304a\n\x0302[b]b\x03",
			markdowntoirc.TranslateBBCode("[color=red]a\n[color=blue][b]b", nil))
	})
	t.Run("wire level never codes-only", func(t *testing.T) {
		codeOnly := regexp.MustCompile(`^([\x02\x1D\x1F\x1E]|\x03[0-9]{0,2}(,[0-9]{0,2})?)*$`)
		for _, in := range []string{
			"[b]a\n[/b]",
			"[color=red]a\n[/color]",
			"[color=red]a\n[color=blue]",
			"[i]a\n[/i]\n",
			"[b][color=red]x\n",
		} {
			for _, line := range wrapForIRC(markdowntoirc.TranslateBBCode(in, nil)) {
				if line == "" {
					continue
				}
				assert.False(t, codeOnly.MatchString(line),
					"line %q is codes-only (blank PRIVMSG) for input %q", line, in)
			}
		}
	})
}

// TestBBCodeInstructionNamesCovered: the 16 canonical color names taught
// by the {{.Vars.BBCodeFormatting}} instruction var (config/
// templatevars.toml) must each resolve as a word value in the translator
// (MarkdownToIRC/bbcode.go) — the instructions promise one unambiguous
// name per color (no numerics, no aliases), so a name that failed to
// translate would be a doc bug. (Table access became behavioral when the
// translator moved packages: a word value translates iff it is a
// bbColorNames key.)
func TestBBCodeInstructionNamesCovered(t *testing.T) {
	for _, name := range []string{
		"white", "black", "blue", "green", "red", "brown", "purple",
		"orange", "yellow", "lightgreen", "cyan", "lightcyan",
		"lightblue", "pink", "grey", "silver",
	} {
		got := markdowntoirc.TranslateBBCode("[color="+name+"]x[/color]", nil)
		assert.True(t, strings.HasPrefix(got, "\x03"),
			"instruction name %q must resolve in the color table, got %q", name, got)
	}
}

// TestBBCodeStylesTemplateVars: the shipped instruction vars split the
// vocabulary exactly (bbcode_styles, Oct 2026) — BBCodeFormatting teaches
// ONLY color tags (every tag-shaped token is [color=…], every taught
// color value resolves in the translator's color table),
// BBCodeStylesFormatting teaches ONLY toggle tags (every taught tag name
// is one of the four canonical toggles), for admins who enable
// bbcode_styles = true. Kept compiling until the bbcode_styles knob and
// its templatevar are deleted (spec §3); bbToggleCode went with the
// toggle deletion, so the four names are pinned literally.
func TestBBCodeStylesTemplateVars(t *testing.T) {
	var vars map[string]string
	_, err := toml.DecodeFile("config/templatevars.toml", &vars)
	require.NoError(t, err, "shipped config/templatevars.toml must decode")
	colorsVar := vars["BBCodeFormatting"]
	stylesVar := vars["BBCodeStylesFormatting"]
	require.NotEmpty(t, colorsVar, "BBCodeFormatting var must exist")
	require.NotEmpty(t, stylesVar, "BBCodeStylesFormatting var must exist")

	tagRe := regexp.MustCompile(`\[(/?)([a-zA-Z]+)(=[^\]\[]*)?\]`)

	for _, m := range tagRe.FindAllStringSubmatch(colorsVar, -1) {
		assert.Equal(t, "color", strings.ToLower(m[2]),
			"BBCodeFormatting must teach only color tags, found %q", m[0])
	}
	// Every taught color name resolves as a word value in the translator
	// (no aliases/numerics taught — one unambiguous name per color).
	vals := regexp.MustCompile(`Supported colors: ([^\n]+)`).FindStringSubmatch(colorsVar)
	require.NotNil(t, vals, "BBCodeFormatting lists its supported colors")
	for _, name := range strings.Split(strings.TrimSuffix(vals[1], "."), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		got := markdowntoirc.TranslateBBCode("[color="+name+"]x[/color]", nil)
		assert.True(t, strings.HasPrefix(got, "\x03"),
			"taught color %q must resolve in the color table, got %q", name, got)
	}

	toggleNames := map[byte]bool{'b': true, 'i': true, 'u': true, 's': true}
	for _, m := range tagRe.FindAllStringSubmatch(stylesVar, -1) {
		name := strings.ToLower(m[2])
		require.Len(t, name, 1, "taught toggle tag %q must be a single letter", name)
		assert.True(t, toggleNames[name[0]],
			"taught toggle tag %q must be one of the four canonical toggles", name)
	}
	for _, tag := range []string{"[b]", "[i]", "[u]", "[s]"} {
		assert.Contains(t, stylesVar, tag, "the four canonical toggles are taught")
	}
	assert.NotContains(t, stylesVar, "[color", "BBCodeStylesFormatting teaches only toggles")
}
