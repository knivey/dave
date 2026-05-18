package main

import (
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

func TestApplyNetworkPolicy(t *testing.T) {
	cfg := testConfig("http://localhost:8188")
	cfg.Enhancements = map[string]EnhancementConfig{
		"safe":    {BaseURL: "https://api.example.com", Key: "k", Model: "m", SystemPrompt: "s"},
		"liberal": {BaseURL: "https://api.example.com", Key: "k", Model: "m", SystemPrompt: "s"},
	}
	cfg.NetworkPolicies = map[string]NetworkPolicy{
		"libera": {Enhancement: "safe", Force: true},
		"graped": {Enhancement: "liberal", Force: false},
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
