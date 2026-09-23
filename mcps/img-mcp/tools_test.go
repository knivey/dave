package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInjectLLMGeneratedNotRequired guards the regression where
// _dave_inject_llm_generated was a REQUIRED schema property: jsonschema-go
// marks every field without `omitempty` as required, and the MCP SDK server
// rejects tool calls that miss required properties. dave's direct tools.toml
// commands never send _dave_inject_* fields, so a required inject field
// breaks every non-LLM caller with
// "validating root: required: missing properties".
// The same applies to _dave_inject_network (pre-existing instance of the
// same bug, caught by the in-memory round-trip test below).
func TestInjectLLMGeneratedNotRequired(t *testing.T) {
	schemas := map[string]*jsonschema.Schema{}
	for name, infer := range map[string]func() (*jsonschema.Schema, error){
		"GenerateImageInput":           func() (*jsonschema.Schema, error) { return jsonschema.For[GenerateImageInput](nil) },
		"GenerateImageAsyncInput":      func() (*jsonschema.Schema, error) { return jsonschema.For[GenerateImageAsyncInput](nil) },
		"EnhanceAndGenerateInput":      func() (*jsonschema.Schema, error) { return jsonschema.For[EnhanceAndGenerateInput](nil) },
		"EnhanceAndGenerateAsyncInput": func() (*jsonschema.Schema, error) { return jsonschema.For[EnhanceAndGenerateAsyncInput](nil) },
		"EnhancePromptInput":           func() (*jsonschema.Schema, error) { return jsonschema.For[EnhancePromptInput](nil) },
	} {
		s, err := infer()
		require.NoError(t, err, name)
		schemas[name] = s
	}

	expectLLMGenerated := map[string]bool{
		"GenerateImageInput":           true,
		"GenerateImageAsyncInput":      true,
		"EnhanceAndGenerateInput":      true,
		"EnhanceAndGenerateAsyncInput": true,
		"EnhancePromptInput":           false,
	}
	for name, s := range schemas {
		for _, req := range s.Required {
			assert.NotEqual(t, "_dave_inject_llm_generated", req,
				"%s: inject fields must be optional (add omitempty to the json tag)", name)
			assert.NotEqual(t, "_dave_inject_network", req,
				"%s: inject fields must be optional (add omitempty to the json tag)", name)
		}
		// The properties must still be advertised so dave can discover and inject them.
		_, hasNet := s.Properties["_dave_inject_network"]
		assert.True(t, hasNet, "%s: _dave_inject_network should still be a declared property", name)
		_, hasGen := s.Properties["_dave_inject_llm_generated"]
		assert.Equal(t, expectLLMGenerated[name], hasGen,
			"%s: unexpected presence of _dave_inject_llm_generated property", name)
	}
}

// TestCallGenerationToolsWithoutInjectFields exercises the SDK server's
// argument validation over a real (in-memory) MCP round trip: a caller that
// sends no _dave_inject_* fields — exactly what dave's direct tools.toml
// commands look like — must be accepted.
func TestCallGenerationToolsWithoutInjectFields(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:0")
	h := NewToolHandlers(cfg, queuedTestQueue(t))
	server := createAsyncServer(cfg, h)

	ct, st := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go server.Run(ctx, st)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	require.NoError(t, err, "connect")
	defer cs.Close()

	for _, tool := range []string{"generate_image_async", "enhance_and_generate_async"} {
		t.Run(tool, func(t *testing.T) {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{
				Name:      tool,
				Arguments: map[string]any{"prompt": "a cat", "output_format": "base64"},
			})
			require.NoError(t, err, "tool call must pass schema validation without inject fields")
			var texts []string
			for _, c := range res.Content {
				if tc, ok := c.(*mcp.TextContent); ok {
					texts = append(texts, tc.Text)
				}
			}
			require.False(t, res.IsError, "tool call should not return an error result: %s", strings.Join(texts, "; "))
		})
	}
}

func TestToolHandlersConfigSwap(t *testing.T) {
	cfg := testConfig("http://localhost:8188")
	cfg.Comfy.Timeout = 60
	cfg.Upload.URL = "https://upload.example.com"

	queue, cleanup := setupTestQueue(t, Config{})
	defer cleanup()

	h := NewToolHandlers(cfg, queue)

	got := h.getConfig()
	assert.Equal(t, 60, got.Comfy.Timeout)
	assert.Equal(t, "https://upload.example.com", got.Upload.URL)
	assert.Equal(t, "http://localhost:8188", got.Comfy.BaseURL)

	newCfg := testConfig("http://localhost:9999")
	newCfg.Comfy.Timeout = 120
	newCfg.Upload.URL = "https://new-upload.example.com"

	h.setConfig(newCfg)

	got = h.getConfig()
	assert.Equal(t, 120, got.Comfy.Timeout)
	assert.Equal(t, "https://new-upload.example.com", got.Upload.URL)
	assert.Equal(t, "http://localhost:9999", got.Comfy.BaseURL)

	require.Same(t, queue, h.queue, "queue reference should be unchanged")
}

func TestToolHandlersResolveWorkflow_AfterConfigSwap(t *testing.T) {
	cfg := testConfig("http://localhost:8188")
	cfg.Comfy.DefaultWorkflow = "test"

	queue, cleanup := setupTestQueue(t, Config{})
	defer cleanup()

	h := NewToolHandlers(cfg, queue)

	name, err := h.resolveWorkflow("")
	require.NoError(t, err)
	assert.Equal(t, "test", name)

	newCfg := testConfig("http://localhost:8188")
	newCfg.Comfy.DefaultWorkflow = "other"
	newCfg.Workflows["other"] = WorkflowConfig{
		ClientID: "other-client", OutputNode: "out", PromptNode: "in", Timeout: 60,
	}
	h.setConfig(newCfg)

	name, err = h.resolveWorkflow("")
	require.NoError(t, err)
	assert.Equal(t, "other", name)
}

// queuedTestQueue builds a JobQueue with no workers so submitted jobs stay
// queued and inspectable.
func queuedTestQueue(t *testing.T) *JobQueue {
	t.Helper()
	cfg := testConfig("http://127.0.0.1:0")
	cfg.Queue.MaxWorkers = 0
	db := setupTestDB(t)
	_, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	q := &JobQueue{
		cfg:     cfg,
		db:      db,
		pending: make(chan *Job, cfg.Queue.MaxDepth),
		results: make(map[string]*Job),
		cancel:  cancel,
	}
	return q
}

func TestGenerateToolsPassLLMGeneratedFlag(t *testing.T) {
	tests := []struct {
		name   string
		submit func(t *testing.T, h *ToolHandlers) (string, error)
	}{
		{
			name: "generate_image_async",
			submit: func(t *testing.T, h *ToolHandlers) (string, error) {
				_, out, err := h.handleGenerateImageAsync(context.Background(), nil,
					GenerateImageAsyncInput{Prompt: "a cat", LLMGenerated: true})
				return out.JobID, err
			},
		},
		{
			name: "enhance_and_generate_async",
			submit: func(t *testing.T, h *ToolHandlers) (string, error) {
				_, out, err := h.handleEnhanceAndGenerateAsync(context.Background(), nil,
					EnhanceAndGenerateAsyncInput{Prompt: "a cat", LLMGenerated: true})
				return out.JobID, err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewToolHandlers(testConfig("http://127.0.0.1:0"), queuedTestQueue(t))

			jobID, err := tt.submit(t, h)
			require.NoError(t, err, "tool handler")

			job, ok := h.queue.Get(jobID)
			require.True(t, ok, "job should be findable in queue")
			assert.True(t, job.Input.LLMGenerated,
				"LLMGenerated from tool input should reach JobInput")
		})
	}
}

// TestGenerateToolsSyncPassLLMGeneratedFlag runs the blocking tool handlers
// end-to-end against a mock ComfyUI and asserts the flag reaches the job.
func TestGenerateToolsSyncPassLLMGeneratedFlag(t *testing.T) {
	mockComfy := newMockComfyFlowServer(t)

	tests := []struct {
		name string
		call func(t *testing.T, h *ToolHandlers) error
	}{
		{
			name: "generate_image",
			call: func(t *testing.T, h *ToolHandlers) error {
				_, out, err := h.handleGenerateImage(context.Background(), nil,
					GenerateImageInput{Prompt: "a cat", LLMGenerated: true, OutputFormat: "base64"})
				assert.Equal(t, "completed", out.Status)
				return err
			},
		},
		{
			name: "enhance_and_generate",
			call: func(t *testing.T, h *ToolHandlers) error {
				_, out, err := h.handleEnhanceAndGenerate(context.Background(), nil,
					EnhanceAndGenerateInput{Prompt: "a cat", LLMGenerated: true, OutputFormat: "base64"})
				assert.Equal(t, "completed", out.Status)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(mockComfy.URL())
			wc := cfg.Workflows["test"]
			wc.WorkflowPath = mustWriteWorkflow(t, t.TempDir())
			cfg.Workflows["test"] = wc
			cfg.Enhancements = map[string]EnhancementConfig{
				"default": {
					BaseURL:      newMockEnhanceServer(t).URL,
					Key:          "test-key",
					Model:        "enhancer",
					SystemPrompt: "enhance",
				},
			}
			q, cleanup := setupTestQueue(t, cfg)
			defer cleanup()
			h := NewToolHandlers(cfg, q)

			require.NoError(t, tt.call(t, h), "tool handler")

			submitted := mockComfy.submittedPrompts()
			require.NotEmpty(t, submitted, "job should have been submitted to comfy")
			node, ok := submitted[len(submitted)-1].Prompt[davePromptNoteNodeID]
			require.True(t, ok, "submitted workflow should contain the prompt note node")
			var payload promptNotePayload
			require.NoError(t, json.Unmarshal([]byte(node.Inputs["text"].(string)), &payload))
			assert.True(t, payload.LLMGenerated, "note payload llm_generated")
		})
	}
}

func TestApplyNetworkPolicy(t *testing.T) {
	cfg := testConfig("http://localhost:8188")
	cfg.Enhancements = map[string]EnhancementConfig{
		"safe":    {BaseURL: "https://api.example.com", Key: "k", Model: "m", SystemPrompt: "s"},
		"liberal": {BaseURL: "https://api.example.com", Key: "k", Model: "m", SystemPrompt: "s"},
	}
	cfg.NetworkPolicies = map[string]NetworkPolicy{
		"libera":     {Enhancement: "safe", Force: true},
		"graped":     {Enhancement: "liberal", Force: false},
		"empty-test": {Enhancement: "", Force: true},
	}

	queue, cleanup := setupTestQueue(t, Config{})
	defer cleanup()

	tests := []struct {
		name              string
		network           string
		inputEnhancement  string
		inputJobType      JobType
		expectEnhancement string
		expectJobType     JobType
	}{
		{
			name:              "empty network passes through",
			network:           "",
			inputEnhancement:  "safe",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "network not in policies passes through",
			network:           "unknown",
			inputEnhancement:  "safe",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "policy overrides enhancement on enhance_generate",
			network:           "libera",
			inputEnhancement:  "liberal",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "policy with force changes generate to enhance_generate",
			network:           "libera",
			inputEnhancement:  "",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "policy without force sets enhancement but keeps generate as generate",
			network:           "graped",
			inputEnhancement:  "",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "liberal",
			expectJobType:     JobTypeGenerate,
		},
		{
			name:              "policy without force overrides enhancement on enhance_generate",
			network:           "graped",
			inputEnhancement:  "",
			inputJobType:      JobTypeEnhanceGenerate,
			expectEnhancement: "liberal",
			expectJobType:     JobTypeEnhanceGenerate,
		},
		{
			name:              "generate with no matching policy passes through",
			network:           "unknown",
			inputEnhancement:  "",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "",
			expectJobType:     JobTypeGenerate,
		},
		{
			name:              "policy with empty enhancement passes through",
			network:           "empty-test",
			inputEnhancement:  "safe",
			inputJobType:      JobTypeGenerate,
			expectEnhancement: "safe",
			expectJobType:     JobTypeGenerate,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewToolHandlers(cfg, queue)
			enhancement, jobType := h.applyNetworkPolicy(tt.network, tt.inputEnhancement, tt.inputJobType)
			assert.Equal(t, tt.expectEnhancement, enhancement, "enhancement")
			assert.Equal(t, tt.expectJobType, jobType, "jobType")
		})
	}
}
