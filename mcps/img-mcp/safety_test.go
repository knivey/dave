package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vetStubServer stands in for the vetting LLM: an OpenAI-compatible Chat
// Completions endpoint whose assistant content and HTTP status are
// configurable. Every request is counted so tests can assert whether a vet
// call was made at all.
type vetStubServer struct {
	server  *httptest.Server
	calls   atomic.Int32
	status  int
	content string
}

func newVetStubServer(t *testing.T, status int, content string) *vetStubServer {
	t.Helper()
	v := &vetStubServer{status: status, content: content}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		v.calls.Add(1)
		if v.status != http.StatusOK {
			w.WriteHeader(v.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatc-1","object":"chat.completion","created":1,"model":"stub",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":` + v.content + `},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	})
	v.server = httptest.NewServer(mux)
	t.Cleanup(v.server.Close)
	return v
}

// vetTestConfig builds a Config whose safety-vet enhancement points at srv.
func vetTestConfig(srv *httptest.Server) Config {
	return Config{
		Enhancements: map[string]EnhancementConfig{
			"safety-vet": {
				BaseURL:      srv.URL + "/v1",
				Key:          "test-key",
				Model:        "stub-model",
				SystemPrompt: "judge the prompts",
				Timeout:      10,
			},
		},
	}
}

// TestStartSafetyVetMapping is the verdict mapping table: the nsfw first
// pass short-circuits to unsafe with NO vet call, the vet's {"safe":bool}
// maps through, vet failures degrade to unknown, skip_networks jobs are not
// vetted at all, and a missing enhancement config degrades to unknown.
// Call counts are pinned exactly for the no-call and happy cases only —
// failing endpoints are retried by the openai-go SDK (part of the machinery
// the vet deliberately inherits), so those assert "at least one attempt".
func TestStartSafetyVetMapping(t *testing.T) {
	tests := []struct {
		name string
		// cfg built per case below
		network      string
		skipNetworks []string
		nsfw         bool
		noVetConfig  bool
		vetStatus    int
		vetContent   string
		wantVerdict  string
		noVetCall    bool
		someVetCall  bool
	}{
		{
			name:        "nsfw true short-circuits to unsafe without a vet call",
			nsfw:        true,
			vetStatus:   http.StatusOK,
			vetContent:  `{"safe":true,"reason":"fine"}`,
			wantVerdict: safetyVerdictUnsafe,
			noVetCall:   true,
		},
		{
			name:        "vet safe true",
			vetStatus:   http.StatusOK,
			vetContent:  `{"safe":true,"reason":"fine"}`,
			wantVerdict: safetyVerdictSafe,
			someVetCall: true,
		},
		{
			name:        "vet safe false",
			vetStatus:   http.StatusOK,
			vetContent:  `{"safe":false,"reason":"sexual content"}`,
			wantVerdict: safetyVerdictUnsafe,
			someVetCall: true,
		},
		{
			name:        "vet http 500 degrades to unknown",
			vetStatus:   http.StatusInternalServerError,
			wantVerdict: safetyVerdictUnknown,
			someVetCall: true,
		},
		{
			name:        "vet unparseable verdict degrades to unknown",
			vetStatus:   http.StatusOK,
			vetContent:  `"not json"`,
			wantVerdict: safetyVerdictUnknown,
			someVetCall: true,
		},
		{
			name:        "missing enhancement config degrades to unknown",
			noVetConfig: true,
			wantVerdict: safetyVerdictUnknown,
			noVetCall:   true,
		},
		{
			name:         "skip network case-insensitive skips vetting entirely",
			network:      "Libera",
			skipNetworks: []string{"libera"},
			wantVerdict:  safetyVerdictUnvetted,
			noVetCall:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newVetStubServer(t, tt.vetStatus, tt.vetContent)
			cfg := vetTestConfig(srv.server)
			if tt.noVetConfig {
				// Point the vet name at an enhancement entry that does not
				// exist; use a per-case name so the process-global WARN-once
				// map never bleeds between cases.
				cfg.Enhancements = nil
				cfg.Safety.VetEnhancement = "missing-" + tt.name
			}
			cfg.Safety.SkipNetworks = tt.skipNetworks

			fut := startSafetyVet(context.Background(), cfg, tt.network, tt.nsfw, "a cat", "a majestic cat")
			verdict := fut.wait()

			assert.Equal(t, tt.wantVerdict, verdict)
			if tt.noVetCall {
				assert.Zero(t, srv.calls.Load(), "no vet endpoint request may be made")
			}
			if tt.someVetCall {
				assert.NotZero(t, srv.calls.Load(), "the vet endpoint must have been tried")
			}
		})
	}
}

// TestRunSafetyVetMissingConfigWarnsOnce pins the WARN-once semantics: the
// missing-config WARN fires exactly one time per process even while every
// subsequent job keeps degrading to unknown.
func TestRunSafetyVetMissingConfigWarnsOnce(t *testing.T) {
	var warns atomic.Int32
	prev := vetMissingWarnSink
	vetMissingWarnSink = func(vetName string) { warns.Add(1) }
	t.Cleanup(func() { vetMissingWarnSink = prev })

	// Unique name so the process-global once-map cannot have been primed
	// by an earlier test.
	cfg := Config{Safety: SafetyConfig{VetEnhancement: "missing-" + t.Name()}}

	v1, err1 := runSafetyVet(context.Background(), cfg, "a cat", "a majestic cat")
	v2, err2 := runSafetyVet(context.Background(), cfg, "a cat", "a majestic cat")

	assert.Equal(t, safetyVerdictUnknown, v1)
	assert.Equal(t, safetyVerdictUnknown, v2)
	assert.Error(t, err1)
	assert.Error(t, err2)
	assert.Equal(t, int32(1), warns.Load(), "the missing-config WARN must fire exactly once per process")
}

// TestRunSafetyVetSendsBothPrompts pins the vet call's wire shape: the user
// message carries BOTH the original and the enhanced prompt, and the
// structured-output schema is the safety-verdict schema (not the
// enhancement one), on the shared enhancement machinery.
func TestRunSafetyVetSendsBothPrompts(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatc-1","object":"chat.completion","created":1,"model":"stub",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"{\"safe\":true,\"reason\":\"fine\"}"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	t.Cleanup(srv.Close)

	cfg := Config{
		Enhancements: map[string]EnhancementConfig{
			"safety-vet": {
				BaseURL:      srv.URL + "/v1",
				Key:          "test-key",
				Model:        "stub-model",
				SystemPrompt: "judge the prompts",
				Timeout:      10,
			},
		},
	}

	verdict, err := runSafetyVet(context.Background(), cfg, "a cat sitting on a mat", "a majestic cat, studio lighting")
	require.NoError(t, err)
	assert.Equal(t, safetyVerdictSafe, verdict)

	messages, ok := gotBody["messages"].([]any)
	require.True(t, ok, "vet call must carry a messages array")
	require.Len(t, messages, 2)
	userMsg, ok := messages[1].(map[string]any)
	require.True(t, ok)
	content, _ := userMsg["content"].(string)
	assert.Contains(t, content, "a cat sitting on a mat", "vet input must carry the original prompt")
	assert.Contains(t, content, "a majestic cat, studio lighting", "vet input must carry the enhanced prompt")

	respFormat, ok := gotBody["response_format"].(map[string]any)
	require.True(t, ok, "vet call must request structured output")
	schema, ok := respFormat["json_schema"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "safety_verdict", schema["name"], "vet call must use the verdict schema, not the enhancement schema")
}

// TestLoadConfigSafetyDefaults pins the [safety] defaults: vet_enhancement
// defaults to "safety-vet" and skip_networks defaults to ["libera"], while
// an explicit empty skip_networks means "vet everything" (nil vs [] keeps
// default-vs-override distinguishable, matching dave's config convention).
func TestLoadConfigSafetyDefaults(t *testing.T) {
	build := func(extra string) Config {
		t.Helper()
		dir := t.TempDir()
		mustWriteWorkflow(t, dir)
		path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188")+extra)
		cfg, err := loadConfig(path)
		require.NoError(t, err)
		return cfg
	}

	t.Run("absent section uses defaults", func(t *testing.T) {
		cfg := build("")
		assert.Equal(t, "safety-vet", cfg.Safety.VetEnhancement)
		assert.Equal(t, []string{"libera"}, cfg.Safety.SkipNetworks)
	})

	t.Run("explicit values honored", func(t *testing.T) {
		cfg := build(`
[safety]
vet_enhancement = "my-vetter"
skip_networks = ["EFnet", "graped"]
`)
		assert.Equal(t, "my-vetter", cfg.Safety.VetEnhancement)
		assert.Equal(t, []string{"EFnet", "graped"}, cfg.Safety.SkipNetworks)
	})

	t.Run("empty skip_networks vets everywhere", func(t *testing.T) {
		cfg := build(`
[safety]
skip_networks = []
`)
		assert.Empty(t, cfg.Safety.SkipNetworks)
	})
}
