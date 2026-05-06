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
