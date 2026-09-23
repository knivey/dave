package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
