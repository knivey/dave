package main

// Tests for the streamOutput wiring fixes (Oct 2026):
//   1. empty content deltas must NOT trigger the markdown renderer's
//      end-of-stream flush (they are routine wire noise)
//   2. the tool-call branch must flush only the UNSENT tail, never re-send
//      text that was already streamed live
//   3. error / idle-timeout stream deaths must flush the held tail before
//      the error notice instead of dropping it

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	markdowntoirc "github.com/knivey/dave/MarkdownToIRC"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStreamOutputEmptyDeltaDoesNotFlush(t *testing.T) {
	t.Run("renderer mode", func(t *testing.T) {
		so := streamOutput{renderer: markdowntoirc.NewStreamingRenderer()}
		var sent []string
		so.HandleDelta("para one", func(s string) { sent = append(sent, s) })
		// empty delta = wire noise (tool-call/finish/keepalive chunk) —
		// must be a no-op, NOT the end-of-stream flush
		so.HandleDelta("", func(s string) { sent = append(sent, s) })
		so.HandleDelta("\n\npara two", func(s string) { sent = append(sent, s) })
		so.HandleDelta("", func(s string) { sent = append(sent, s) })
		// Emission timing is the renderer's business (it holds a settled
		// line until a newline follows it); the assertion that matters is
		// that NO empty delta ever flushed the buffer.
		assert.Empty(t, sent, "empty deltas must not flush the renderer")

		so.Flush(func(s string) { sent = append(sent, s) })
		joined := strings.Join(sent, "\n")
		assert.Equal(t, 1, strings.Count(joined, "para one"), "each paragraph exactly once")
		assert.Equal(t, 1, strings.Count(joined, "para two"), "each paragraph exactly once")
	})

	t.Run("plain mode", func(t *testing.T) {
		so := streamOutput{}
		var sent []string
		so.HandleDelta("hello", func(s string) { sent = append(sent, s) })
		so.HandleDelta("", func(s string) { sent = append(sent, s) })
		assert.Empty(t, sent)
		so.HandleDelta(" world\n", func(s string) { sent = append(sent, s) })
		assert.Equal(t, []string{"hello world\n"}, sent)
		so.Flush(func(s string) { sent = append(sent, s) })
		assert.Empty(t, sent[1:], "nothing pending after newline send")
	})
}

// newStreamTestRunner builds a chatRunner pointed at an SSE test server.
func newStreamTestRunner(t *testing.T, server *httptest.Server, cfg AIConfig, sid int64, outputCh chan string) *chatRunner {
	t.Helper()
	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("k"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	return &chatRunner{
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		baseURL:      server.URL + "/v1",
		apiKey:       "k",
		cfg:          cfg,
		network:      Network{Name: "testnet"},
		channel:      "#st",
		nick:         "shrew",
		logger:       newTestLogger(),
		ctx:          context.Background(),
		outputCh:     outputCh,
		sessionID:    sid,
	}
}

func streamChunk(s string) string {
	return fmt.Sprintf("data: %s\n\n", s)
}

// TestRunTurnStreamToolCallSendsStreamedTextOnce: text streamed before a
// tool call must reach IRC exactly once (live), with only the unsent tail
// flushed by the tool-call branch.
func TestRunTurnStreamToolCallSendsStreamedTextOnce(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#st", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	toolStream := streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"para one here"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"\n\npara two words"},"finish_reason":null}]}`) +
		// tool-call chunk carries empty content — the old empty-delta flush
		// fired here, and the old re-send duplicated everything after it
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"nonexistent_tool","arguments":"{}"}}]},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		"data: [DONE]\n\n"
	doneStream := streamChunk(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"done now"},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c2","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
		"data: [DONE]\n\n"

	var reqs int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if atomic.AddInt32(&reqs, 1) == 1 {
			fmt.Fprint(w, toolStream)
			return
		}
		fmt.Fprint(w, doneStream)
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		RenderMarkdown: true, StreamTimeout: 5 * time.Second,
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
	assert.Equal(t, 1, strings.Count(joined, "para one here"), "streamed paragraph must appear exactly once\ngot: %q", joined)
	assert.Equal(t, 1, strings.Count(joined, "para two words"), "held tail must be flushed exactly once\ngot: %q", joined)
	assert.Equal(t, 1, strings.Count(joined, "done now"), "post-tool continuation must appear exactly once")
}

// TestRunTurnStreamErrorFlushesTail: a connection dying mid-generation must
// deliver the held tail before the error notice.
func TestRunTurnStreamErrorFlushesTail(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#st", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial tail text"},"finish_reason":null}]}`))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler) // kill the connection mid-stream
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		RenderMarkdown: true, StreamTimeout: 5 * time.Second,
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
	assert.Contains(t, joined, "partial tail text", "held tail must be flushed on stream error")
}

// TestRunTurnStreamIdleTimeoutFlushesTail: an idle stream that never
// finishes must deliver the held tail before the timeout error.
func TestRunTurnStreamIdleTimeoutFlushesTail(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#st", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"stalled tail text"},"finish_reason":null}]}`))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stall forever
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		RenderMarkdown: true, StreamTimeout: 200 * time.Millisecond,
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
	assert.Contains(t, joined, "stalled tail text", "held tail must be flushed on idle timeout")
	assert.Contains(t, joined, "timed out", "timeout error must be reported")
}

// TestCallResponsesStreamMissingCompletedFlushesTail: a Responses stream
// that ends without response.completed must still deliver streamed text.
func TestCallResponsesStreamMissingCompletedFlushesTail(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#st", "shrew", "testcmd", "svc", "m")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, streamChunk(`{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"responses tail text"}`))
		w.(http.Flusher).Flush()
		// clean end-of-stream, but no response.completed event ever arrives
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second,
		StreamTimeout: 200 * time.Millisecond,
	}, sid, outputCh)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cr.callResponsesStream(ctx, responses.ResponseNewParams{})
	require.Error(t, err, "missing response.completed must surface an error")
	assert.Contains(t, err.Error(), "response.completed")

	lines := drainOutput(t, outputCh, 32, time.Second)
	joined := strings.Join(lines, "\n")
	assert.Contains(t, joined, "responses tail text", "streamed text must be flushed before the error return")
}

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
	go func() {
		cr.runTurn(newEphemeralTurnContext([]ChatMessage{{Role: RoleUser, Content: "sum"}}))
		close(done)
	}()
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
	go func() {
		cr.runTurn(newEphemeralTurnContext([]ChatMessage{{Role: RoleUser, Content: "sum"}}))
		close(done)
	}()
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
		w.(http.Flusher).Flush()
		// DEVIATION from the brief (recorded in task-5-report.md): the
		// brief's handler returned cleanly after the chunk, which the
		// openai-go ssestream decoder treats as a CLEAN end (EOF without
		// [DONE] is not an error — bufio.Scanner EOF → Next()=false,
		// Err()=nil), so the text was delivered by flushStreamedOutput
		// and the error branch was never reached. Aborting the handler
		// mid-stream (the TestRunTurnStreamErrorFlushesTail shape) makes
		// the reader see an unexpected EOF, so the ephemeral error-path
		// delivery (sendFinalText(fullContent)) is what this pins.
		panic(http.ErrAbortHandler)
	}))
	defer server.Close()

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		StreamTimeout: 5 * time.Second,
	}, 0, outputCh)
	cr.ephemeral = true

	done := make(chan struct{})
	go func() {
		cr.runTurn(newEphemeralTurnContext([]ChatMessage{{Role: RoleUser, Content: "sum"}}))
		close(done)
	}()
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

// TestRunTurnResponsesStreamEphemeralSuppressesIntermediateText: the
// Responses-API mirror of the chat-completions suppression test — on an
// ephemeral turn, delta text streamed before a function call never
// reaches IRC; only the final iteration's text is sent, once, complete.
// Event shapes mirror the existing responses stream tests: live
// response.output_text.delta events plus a response.completed event
// whose payload carries the output items (parseSDKResponseOutput reads
// the completed response; the first iteration's payload carries BOTH
// the intermediate message text and the function_call item, exactly
// like the chat test's text+tool-call iteration).
func TestRunTurnResponsesStreamEphemeralSuppressesIntermediateText(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	combinedPayload := map[string]any{
		"id":     "resp-1",
		"object": "response",
		"model":  "test-model",
		"output": []any{
			map[string]any{
				"type": "message", "role": "assistant", "id": "msg_1", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "let me fetch"}},
			},
			map[string]any{
				"type": "function_call", "id": "fc_1", "call_id": "call_1", "status": "completed",
				"name": "nonexistent_tool", "arguments": "{}",
			},
		},
	}
	firstCompleted, err := json.Marshal(map[string]any{"type": "response.completed", "response": combinedPayload})
	require.NoError(t, err)
	finalCompleted, err := json.Marshal(map[string]any{"type": "response.completed", "response": makeResponsesAPIResponse("resp-2", "THE FINAL")})
	require.NoError(t, err)
	toolStream := streamChunk(`{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"logprobs":[],"delta":"let me fetch"}`) +
		"data: " + string(firstCompleted) + "\n\n"
	finalStream := streamChunk(`{"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_2","output_index":0,"content_index":0,"logprobs":[],"delta":"THE FINAL"}`) +
		"data: " + string(finalCompleted) + "\n\n"

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
		Model: "test-model", ResponsesAPI: true, Streaming: true,
		RenderMarkdown: true, Timeout: 10 * time.Second, StreamTimeout: 5 * time.Second,
	}, 0, outputCh)
	cr.ephemeral = true

	done := make(chan struct{})
	go func() {
		cr.runTurn(newEphemeralTurnContext([]ChatMessage{{Role: RoleUser, Content: "sum"}}))
		close(done)
	}()
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
	assert.Equal(t, 1, strings.Count(joined, "THE FINAL"), "the final answer is delivered exactly once")
	assert.EqualValues(t, 2, atomic.LoadInt32(&reqs), "the nonexistent tool result drives a second iteration")
}
