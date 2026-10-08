package main

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

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseModelStatus(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "object shape (router)", raw: `{"value":"loading","progress":0.5}`, want: "loading"},
		{name: "bare string shape", raw: `"loaded"`, want: "loaded"},
		{name: "object value normalized", raw: `{"value":"  Loaded  "}`, want: "loaded"},
		{name: "string normalized", raw: `" LOADING "`, want: "loading"},
		{name: "empty", raw: ``, want: ""},
		{name: "null", raw: `null`, want: ""},
		{name: "object without value", raw: `{"progress":1}`, want: ""},
		{name: "garbage", raw: `{not json`, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseModelStatus(json.RawMessage(tt.raw)))
		})
	}
}

func TestEvaluateModelLoad(t *testing.T) {
	tests := []struct {
		name      string
		entries   []modelListEntry
		model     string
		needsLoad bool
		decided   bool
	}{
		{
			name: "router: loaded means no wait",
			entries: []modelListEntry{
				{ID: "other", Status: json.RawMessage(`{"value":"loaded"}`)},
				{ID: "want", Status: json.RawMessage(`{"value":"loaded"}`)},
			},
			model:     "want",
			needsLoad: false,
			decided:   true,
		},
		{
			name: "router: unloaded needs load",
			entries: []modelListEntry{
				{ID: "want", Status: json.RawMessage(`{"value":"unloaded"}`)},
			},
			model:     "want",
			needsLoad: true,
			decided:   true,
		},
		{
			name: "router: loading needs load",
			entries: []modelListEntry{
				{ID: "want", Status: json.RawMessage(`{"value":"loading"}`)},
			},
			model:     "want",
			needsLoad: true,
			decided:   true,
		},
		{
			name: "router: sleeping needs load",
			entries: []modelListEntry{
				{ID: "want", Status: json.RawMessage(`"sleeping"`)},
			},
			model:     "want",
			needsLoad: true,
			decided:   true,
		},
		{
			name: "router: bare-string loaded status",
			entries: []modelListEntry{
				{ID: "want", Status: json.RawMessage(`"loaded"`)},
			},
			model:     "want",
			needsLoad: false,
			decided:   true,
		},
		{
			name: "router: loaded status normalized (case/whitespace)",
			entries: []modelListEntry{
				{ID: "want", Status: json.RawMessage(`{"value":"  Loaded  "}`)},
			},
			model:     "want",
			needsLoad: false,
			decided:   true,
		},
		{
			name: "null statuses do not make the list status-aware (llama-swap semantics)",
			entries: []modelListEntry{
				{ID: "other", Status: json.RawMessage(`null`)},
			},
			model:     "want",
			needsLoad: true,
			decided:   true,
		},
		{
			name: "matched via alias",
			entries: []modelListEntry{
				{ID: "ggml-org/gemma-3-4b-it-qat-GGUF:Q4_0", Aliases: []string{"gemma"}, Status: json.RawMessage(`{"value":"unloaded"}`)},
			},
			model:     "gemma",
			needsLoad: true,
			decided:   true,
		},
		{
			name: "listed without status treated as loaded (plain llama-server)",
			entries: []modelListEntry{
				{ID: "want"},
			},
			model:     "want",
			needsLoad: false,
			decided:   true,
		},
		{
			name: "router: unknown model is undecided, no notice",
			entries: []modelListEntry{
				{ID: "known", Status: json.RawMessage(`{"value":"loaded"}`)},
			},
			model:     "unknown",
			needsLoad: false,
			decided:   false,
		},
		{
			name: "status-less list (llama-swap): absent model needs swap",
			entries: []modelListEntry{
				{ID: "currently-loaded"},
			},
			model:     "want",
			needsLoad: true,
			decided:   true,
		},
		{
			name: "status-less list (llama-swap): present model is loaded",
			entries: []modelListEntry{
				{ID: "want"},
			},
			model:     "want",
			needsLoad: false,
			decided:   true,
		},
		{
			name:      "empty status-less list (nothing loaded) needs load",
			entries:   nil,
			model:     "want",
			needsLoad: true,
			decided:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			needsLoad, decided := evaluateModelLoad(tt.entries, tt.model)
			assert.Equal(t, tt.decided, decided, "decided")
			assert.Equal(t, tt.needsLoad, needsLoad, "needsLoad")
		})
	}
}

func TestProbeModelLoad(t *testing.T) {
	t.Run("router reports unloaded", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "/v1/models", r.URL.Path, "probe must hit the OpenAI-compat model list")
			fmt.Fprint(w, `{"data":[{"id":"m","status":{"value":"unloaded"}}]}`)
		}))
		defer server.Close()

		needsLoad, decided := probeModelLoad(context.Background(), nil, server.URL+"/v1", "", "m")
		assert.True(t, decided)
		assert.True(t, needsLoad)
	})

	t.Run("trailing slash base url still joins", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{"data":[{"id":"m","status":{"value":"loaded"}}]}`)
		}))
		defer server.Close()

		needsLoad, decided := probeModelLoad(context.Background(), nil, server.URL+"/v1/", "", "m")
		assert.True(t, decided)
		assert.False(t, needsLoad)
	})

	t.Run("api key sent as bearer", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer sekrit", r.Header.Get("Authorization"))
			fmt.Fprint(w, `{"data":[{"id":"m","status":{"value":"loaded"}}]}`)
		}))
		defer server.Close()

		_, decided := probeModelLoad(context.Background(), nil, server.URL, "sekrit", "m")
		assert.True(t, decided)
	})

	t.Run("non-200 fails open undecided", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}))
		defer server.Close()

		needsLoad, decided := probeModelLoad(context.Background(), nil, server.URL, "", "m")
		assert.False(t, decided)
		assert.False(t, needsLoad)
	})

	t.Run("unparseable body fails open undecided", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `<html>not json</html>`)
		}))
		defer server.Close()

		_, decided := probeModelLoad(context.Background(), nil, server.URL, "", "m")
		assert.False(t, decided)
	})

	t.Run("connection error fails open undecided", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := server.URL
		server.Close()

		_, decided := probeModelLoad(context.Background(), nil, url, "", "m")
		assert.False(t, decided)
	})

	t.Run("cancelled context fails open undecided", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, decided := probeModelLoad(ctx, nil, "http://127.0.0.1:1", "", "m")
		assert.False(t, decided)
	})
}

func TestMaybeNotifyModelLoad(t *testing.T) {
	setupNoticesDefaults(t)

	// newStubRunner builds a runner whose service reports model "m" as
	// unloaded, counting probe requests so disabled configs can be verified
	// to never hit the endpoint.
	newStubRunner := func(t *testing.T, enabled *bool) (*chatRunner, chan string, *int32) {
		t.Helper()
		var hits int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			fmt.Fprint(w, `{"data":[{"id":"m","status":{"value":"unloaded"}}]}`)
		}))
		t.Cleanup(server.Close)

		logger := logxi.New("test")
		logger.SetLevel(logxi.LevelAll)
		outputCh := make(chan string, 4)
		transport := newDaveTransport(nil, nil)
		return &chatRunner{
			cfg:        AIConfig{Model: "m", LoadNotice: enabled},
			transport:  transport,
			httpClient: &http.Client{Transport: transport},
			baseURL:    server.URL + "/v1",
			logger:     logger,
			ctx:        context.Background(),
			outputCh:   outputCh,
			nick:       "shrew",
		}, outputCh, &hits
	}

	t.Run("notice sent when model not loaded", func(t *testing.T) {
		cr, outputCh, _ := newStubRunner(t, boolPtr(true))
		cr.maybeNotifyModelLoad()

		lines := drainOutput(t, outputCh, 2, time.Second)
		require.NotEmpty(t, lines, "expected a model-load notice")
		assert.Contains(t, lines[0], "m")
		assert.Contains(t, lines[0], "shrew")
	})

	t.Run("no probe when unset", func(t *testing.T) {
		cr, outputCh, hits := newStubRunner(t, nil)
		cr.maybeNotifyModelLoad()

		assert.Equal(t, int32(0), atomic.LoadInt32(hits), "disabled probe must not hit the endpoint")
		assert.Empty(t, drainOutput(t, outputCh, 2, 50*time.Millisecond))
	})

	t.Run("no probe when explicitly false", func(t *testing.T) {
		cr, outputCh, hits := newStubRunner(t, boolPtr(false))
		cr.maybeNotifyModelLoad()

		assert.Equal(t, int32(0), atomic.LoadInt32(hits), "disabled probe must not hit the endpoint")
		assert.Empty(t, drainOutput(t, outputCh, 2, 50*time.Millisecond))
	})
}

func TestRunTurnModelLoadNotice(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)

	seedTurn := func(t *testing.T) (int64, []ChatMessage) {
		t.Helper()
		sid := createTestSession(t, "testnet", "#101", "shrew", "testcmd", "svc", "m")
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hi"}))
		messages, err := sessionMgr.GetMessages(sid)
		require.NoError(t, err)
		return sid, messages
	}

	t.Run("notice precedes the answer when model unloaded", func(t *testing.T) {
		sid, messages := seedTurn(t)
		outputCh := make(chan string, 4)
		cr := newModelLoadTurnRunner(t, `{"data":[{"id":"m","status":{"value":"unloaded"}}]}`, outputCh)
		cr.sessionID = sid

		cr.runTurn(newTurnContext(sid, messages))

		lines := drainOutput(t, outputCh, 4, time.Second)
		require.GreaterOrEqual(t, len(lines), 2, "expected notice + answer, got %q", lines)
		assert.Contains(t, lines[0], "loading model", "notice must come first, got %q", lines)
		notices := 0
		for _, line := range lines {
			if strings.Contains(line, "loading model") {
				notices++
			}
		}
		assert.Equal(t, 1, notices, "exactly one model-load notice, got %q", lines)
	})

	t.Run("no notice when model already loaded", func(t *testing.T) {
		sid, messages := seedTurn(t)
		outputCh := make(chan string, 4)
		cr := newModelLoadTurnRunner(t, `{"data":[{"id":"m","status":{"value":"loaded"}}]}`, outputCh)
		cr.sessionID = sid

		cr.runTurn(newTurnContext(sid, messages))

		lines := drainOutput(t, outputCh, 4, time.Second)
		for _, line := range lines {
			assert.NotContains(t, line, "loading model", "no model-load notice expected, got %q", lines)
		}
	})
}

// newModelLoadTurnRunner builds a chatRunner whose service stub answers the
// /v1/models probe with modelsBody and serves a minimal chat completion, so a
// full turn can run against it.
func newModelLoadTurnRunner(t *testing.T, modelsBody string, outputCh chan string) *chatRunner {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, modelsBody)
		case "/v1/chat/completions":
			_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"cmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	transport := newDaveTransport(nil, nil)
	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(server.URL+"/v1"),
		option.WithHTTPClient(&http.Client{Transport: transport}),
	)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	return &chatRunner{
		cfg:          AIConfig{Model: "m", LoadNotice: boolPtr(true), Timeout: 10 * time.Second},
		openaiClient: &client,
		transport:    transport,
		httpClient:   &http.Client{Transport: transport},
		baseURL:      server.URL + "/v1",
		apiKey:       "test-key",
		network:      Network{Name: "testnet"},
		channel:      "#101",
		nick:         "shrew",
		logger:       logger,
		ctx:          context.Background(),
		outputCh:     outputCh,
	}
}

func TestCompletionModelLoadNoticeDisabled(t *testing.T) {
	setupNoticesDefaults(t)

	var modelsHits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			atomic.AddInt32(&modelsHits, 1)
			fmt.Fprint(w, `{"data":[{"id":"m","status":{"value":"unloaded"}}]}`)
		case "/v1/completions":
			_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"hello there","index":0,"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	prevServices := config.Services
	config.Services = map[string]Service{"localsvc": {Type: "llama", LoadNotice: boolPtr(false), BaseURL: server.URL + "/v1"}}
	defer func() { config.Services = prevServices }()

	cfg := AIConfig{Name: "c", Service: "localsvc", Model: "m", Timeout: 10 * time.Second}
	cfg.ApplyDefaults(config.Services["localsvc"])
	require.NotNil(t, cfg.LoadNotice)
	require.False(t, *cfg.LoadNotice, "service-level load_notice = false must override the llama type default")

	outputCh := make(chan string, 4)
	e := girc.Event{Source: &girc.Source{Name: "shrew"}, Params: []string{"#101"}}

	completion(Network{Name: "testnet"}, nil, e, cfg, context.Background(), outputCh, "hello")

	assert.Equal(t, int32(0), atomic.LoadInt32(&modelsHits), "disabled probe must not hit the endpoint")
	lines := drainOutput(t, outputCh, 4, time.Second)
	for _, line := range lines {
		assert.NotContains(t, line, "loading model", "no model-load notice expected, got %q", lines)
	}
	assert.NotEmpty(t, lines, "completion output expected")
}

func TestCompletionModelLoadNotice(t *testing.T) {
	setupNoticesDefaults(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"m","status":{"value":"unloaded"}}]}`)
		case "/v1/completions":
			_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"cmpl-1","object":"text_completion","created":1,"model":"m","choices":[{"text":"hello there","index":0,"finish_reason":"stop"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	prevServices := config.Services
	config.Services = map[string]Service{"localsvc": {Type: "llama", BaseURL: server.URL + "/v1"}}
	defer func() { config.Services = prevServices }()

	cfg := AIConfig{Name: "c", Service: "localsvc", Model: "m", Timeout: 10 * time.Second}
	cfg.ApplyDefaults(config.Services["localsvc"])
	require.NotNil(t, cfg.LoadNotice)
	require.True(t, *cfg.LoadNotice, "type = llama default applies after ApplyDefaults")

	outputCh := make(chan string, 4)
	e := girc.Event{Source: &girc.Source{Name: "shrew"}, Params: []string{"#101"}}

	completion(Network{Name: "testnet"}, nil, e, cfg, context.Background(), outputCh, "hello")

	lines := drainOutput(t, outputCh, 4, time.Second)
	require.NotEmpty(t, lines, "expected output from completion, got none")
	foundNotice, foundAnswer := false, false
	for _, line := range lines {
		if strings.Contains(line, "loading model") && strings.Contains(line, "m") {
			foundNotice = true
		}
		if strings.Contains(line, "hello there") {
			foundAnswer = true
		}
	}
	assert.True(t, foundNotice, "expected model-load notice, got %q", lines)
	assert.True(t, foundAnswer, "expected completion text, got %q", lines)
}
