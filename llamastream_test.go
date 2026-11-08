package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// llamaStreamRecorder records method+path (and the X-Conversation-Id header)
// of every request hitting the test server, in order, so tests can assert
// both the streaming POST carrying the header and the trailing
// DELETE /v1/stream release.
type llamaStreamRecorder struct {
	mu     sync.Mutex
	events []string
}

func (rec *llamaStreamRecorder) record(event string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.events = append(rec.events, event)
}

func (rec *llamaStreamRecorder) snapshot() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, len(rec.events))
	copy(out, rec.events)
	return out
}

const llamaTestDoneStream = `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"all done text"},"finish_reason":null}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":4,"total_tokens":9}}

data: [DONE]

`

// newLlamaStreamServer serves a normal done-stream for POST
// /v1/chat/completions and a 204 for DELETE /v1/stream, recording both.
func newLlamaStreamServer(t *testing.T, rec *llamaStreamRecorder) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			rec.record("POST conv=" + r.Header.Get("X-Conversation-Id"))
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, llamaTestDoneStream)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/stream"):
			rec.record("DELETE " + r.URL.Path + "?" + r.URL.RawQuery + " auth=" + r.Header.Get("Authorization"))
			w.WriteHeader(http.StatusNoContent)
		default:
			rec.record("OTHER " + r.Method + " " + r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// runLlamaStreamTurn drives one full runTurn against the given server with a
// minimal session, returning the runner so tests can tweak fields first.
func runLlamaStreamTurn(t *testing.T, server *httptest.Server, mutate func(*chatRunner)) []string {
	t.Helper()
	setupTestDB(t)
	setupNoticesDefaults(t)

	sid := createTestSession(t, "testnet", "#st", "shrew", "testcmd", "svc", "m")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)

	outputCh := make(chan string, 64)
	cr := newStreamTestRunner(t, server, AIConfig{
		Model: "m", Timeout: 10 * time.Second, Streaming: true,
		RenderMarkdown: true, StreamTimeout: 5 * time.Second,
	}, sid, outputCh)
	if mutate != nil {
		mutate(cr)
	}

	done := make(chan struct{})
	go func() { cr.runTurn(newTurnContext(sid, messages)); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("runTurn did not finish")
	}
	return drainOutput(t, outputCh, 32, time.Second)
}

// TestRunTurnStreamLlamaConversationHeaderAndRelease: a streaming turn on a
// llama-type service with a conv id must carry X-Conversation-Id on the POST
// and release the server-side session with DELETE /v1/stream when the turn
// completes — in that order.
func TestRunTurnStreamLlamaConversationHeaderAndRelease(t *testing.T) {
	rec := &llamaStreamRecorder{}
	server := newLlamaStreamServer(t, rec)

	lines := runLlamaStreamTurn(t, server, func(cr *chatRunner) {
		cr.llamaService = true
		cr.convID = "conv-abc123"
		cr.modelsMax = -1 // gate off: this test asserts an exact request log
	})

	assert.Contains(t, strings.Join(lines, "\n"), "all done text")
	assert.Equal(t, []string{
		"POST conv=conv-abc123",
		"DELETE /v1/stream?conv_id=conv-abc123 auth=Bearer k",
	}, rec.snapshot(), "expected exactly one headered POST followed by one authenticated release DELETE")
}

// TestRunTurnStreamConversationHeaderRequiresLlamaService: non-llama services
// must be untouched — no header, no DELETE.
func TestRunTurnStreamConversationHeaderRequiresLlamaService(t *testing.T) {
	rec := &llamaStreamRecorder{}
	server := newLlamaStreamServer(t, rec)

	runLlamaStreamTurn(t, server, func(cr *chatRunner) {
		cr.convID = "conv-abc123" // llamaService left false
	})

	assert.Equal(t, []string{"POST conv="}, rec.snapshot(), "no header, no release")
}

// TestRunTurnStreamConversationHeaderRequiresConvID: ephemeral-style turns
// (llama service but no session conv id) must not send the header or the
// DELETE.
func TestRunTurnStreamConversationHeaderRequiresConvID(t *testing.T) {
	rec := &llamaStreamRecorder{}
	server := newLlamaStreamServer(t, rec)

	runLlamaStreamTurn(t, server, func(cr *chatRunner) {
		cr.llamaService = true
		cr.convID = ""
		cr.modelsMax = -1 // gate off: this test asserts an exact request log
	})

	assert.Equal(t, []string{"POST conv="}, rec.snapshot(), "no header, no release")
}

// TestRunTurnStreamReleaseOnStreamError: a stream dying mid-generation must
// still release the server-side session — the detached generation would
// otherwise keep running to max_tokens.
func TestRunTurnStreamReleaseOnStreamError(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	rec := &llamaStreamRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			rec.record("POST")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler) // kill the connection mid-stream
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/stream"):
			rec.record("DELETE " + r.URL.Path + "?" + r.URL.RawQuery)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runLlamaStreamTurn(t, server, func(cr *chatRunner) {
		cr.llamaService = true
		cr.convID = "conv-abc123"
	})

	assert.Equal(t, []string{"POST", "DELETE /v1/stream?conv_id=conv-abc123"}, rec.snapshot())
}

// TestRunTurnStreamReleaseOnIdleTimeout: the idle-timeout abort must release
// the session too.
func TestRunTurnStreamReleaseOnIdleTimeout(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	rec := &llamaStreamRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			rec.record("POST")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"partial"},"finish_reason":null}]}`))
			w.(http.Flusher).Flush()
			<-r.Context().Done() // hold the stream open, send nothing more
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/stream"):
			rec.record("DELETE " + r.URL.Path + "?" + r.URL.RawQuery)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runLlamaStreamTurn(t, server, func(cr *chatRunner) {
		cr.llamaService = true
		cr.convID = "conv-abc123"
		cr.cfg.StreamTimeout = 300 * time.Millisecond
	})

	assert.Equal(t, []string{"POST", "DELETE /v1/stream?conv_id=conv-abc123"}, rec.snapshot())
}

// TestRunTurnStreamReleaseOnCancel: ^stop / queue cancel / shutdown all
// cancel the runner ctx — the release DELETE uses a detached context so it
// still fires.
func TestRunTurnStreamReleaseOnCancel(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	rec := &llamaStreamRecorder{}
	keepAlive := streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"."},"finish_reason":null}]}`)
	ctx, cancel := context.WithCancel(context.Background())
	// Arm the cancel only once the POST is actually on the wire: arming
	// on a wall-clock timer before the turn starts races the DB-migration
	// setup (deterministically losing under -race) and turns this into a
	// pre-cancelled-ctx test. Firing 150ms after the first POST cancels
	// the stream genuinely mid-flight.
	postSeen := make(chan struct{})
	var postSeenOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			postSeenOnce.Do(func() { close(postSeen) })
			rec.record("POST")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"working"},"finish_reason":null}]}`))
			w.(http.Flusher).Flush()
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					fmt.Fprint(w, keepAlive)
					w.(http.Flusher).Flush()
				case <-r.Context().Done():
					return
				}
			}
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/stream"):
			rec.record("DELETE " + r.URL.Path + "?" + r.URL.RawQuery)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	go func() {
		<-postSeen
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	runLlamaStreamTurn(t, server, func(cr *chatRunner) {
		cr.llamaService = true
		cr.convID = "conv-abc123"
		cr.ctx = ctx
	})

	// runLlamaStreamTurn waits for runTurn to return; the cancel lands
	// mid-flight (keepalive chunks unblock the loop-top ctx check). The
	// release DELETE must follow on its detached context.
	events := rec.snapshot()
	require.NotEmpty(t, events)
	assert.Equal(t, "POST", events[0])
	assert.Contains(t, events, "DELETE /v1/stream?conv_id=conv-abc123",
		"cancel must release the server-side session; got %v", events)
}

// TestRunTurnStreamLlamaToolLoopReleasesPerIteration: each tool-loop
// iteration is its own runTurnStream call — the release DELETE for
// iteration N must complete before iteration N+1's POST re-registers the
// session (the sequencing that makes per-iteration releases safe). Pins
// the exact interleaving so a future "optimization" cannot silently drop
// or reorder it.
func TestRunTurnStreamLlamaToolLoopReleasesPerIteration(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	toolStream := streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"nonexistent_tool","arguments":"{}"}}]},"finish_reason":null}]}`) +
		streamChunk(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`) +
		"data: [DONE]\n\n"

	rec := &llamaStreamRecorder{}
	var posts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			rec.record(fmt.Sprintf("POST#%d conv=%s", atomic.AddInt32(&posts, 1), r.Header.Get("X-Conversation-Id")))
			w.Header().Set("Content-Type", "text/event-stream")
			if posts == 1 {
				fmt.Fprint(w, toolStream)
				return
			}
			fmt.Fprint(w, llamaTestDoneStream)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/stream"):
			rec.record("DELETE " + r.URL.Path + "?" + r.URL.RawQuery)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runLlamaStreamTurn(t, server, func(cr *chatRunner) {
		cr.llamaService = true
		cr.convID = "conv-abc123"
	})

	assert.Equal(t, []string{
		"POST#1 conv=conv-abc123",
		"DELETE /v1/stream?conv_id=conv-abc123",
		"POST#2 conv=conv-abc123",
		"DELETE /v1/stream?conv_id=conv-abc123",
	}, rec.snapshot(), "each iteration must release before the next POST re-registers")
}

// TestRunTurnStreamReleaseFailureNonFatal: a 500 on the release DELETE must
// not break the turn — output still delivers.
func TestRunTurnStreamReleaseFailureNonFatal(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	rec := &llamaStreamRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
			rec.record("POST")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, llamaTestDoneStream)
		case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/stream"):
			rec.record("DELETE")
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	lines := runLlamaStreamTurn(t, server, func(cr *chatRunner) {
		cr.llamaService = true
		cr.convID = "conv-abc123"
	})

	assert.Equal(t, []string{"POST", "DELETE"}, rec.snapshot())
	assert.Contains(t, strings.Join(lines, "\n"), "all done text",
		"a failed release must not break the turn's output")
}

// TestRunTurnStreamReleaseEscapesConvID: the conv id is query-escaped — a
// hostile/odd value must not be able to alter the query.
func TestRunTurnStreamReleaseEscapesConvID(t *testing.T) {
	rec := &llamaStreamRecorder{}
	server := newLlamaStreamServer(t, rec)

	runLlamaStreamTurn(t, server, func(cr *chatRunner) {
		cr.llamaService = true
		cr.convID = "weird id&x=1"
		cr.modelsMax = -1 // gate off: this test asserts an exact request log
	})

	events := rec.snapshot()
	require.Len(t, events, 2)
	assert.Equal(t, "DELETE /v1/stream?conv_id=weird+id%26x%3D1 auth=Bearer k", events[1])
}

// TestNewChatRunnerLlamaServiceFlag: the production constructor derives the
// flag from the service entry's type.
func TestNewChatRunnerLlamaServiceFlag(t *testing.T) {
	prevServices := config.Services
	config.Services = map[string]Service{
		"llamasvc": {Type: "llama", BaseURL: "http://localhost:1/v1"},
		"opensvc":  {BaseURL: "http://localhost:1/v1"},
	}
	defer func() { config.Services = prevServices }()

	assert.True(t, newChatRunner(Network{Name: "n"}, nil, AIConfig{Name: "c", Service: "llamasvc"}).llamaService)
	assert.False(t, newChatRunner(Network{Name: "n"}, nil, AIConfig{Name: "c", Service: "opensvc"}).llamaService)
	assert.False(t, newChatRunner(Network{Name: "n"}, nil, AIConfig{Name: "c", Service: "missing"}).llamaService)
}
