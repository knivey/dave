package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupAdminTest(t *testing.T) (*ToolHandlers, *JobQueue, string) {
	t.Helper()
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	configPath := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))

	cfg, err := loadConfig(configPath)
	require.NoError(t, err)
	cfg.Database.Resolved = filepath.Join(dir, "test.db")

	db, err := initDB(cfg.Database.Resolved)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	queue := NewJobQueue(cfg, db)
	t.Cleanup(func() { queue.Stop() })
	handlers := NewToolHandlers(cfg, queue)

	return handlers, queue, configPath
}

func TestHTTPAdminReload_Success(t *testing.T) {
	handlers, queue, configPath := setupAdminTest(t)

	cfg := handlers.getConfig()
	handler := buildHTTPHandler(cfg, handlers, queue, configPath)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Post(server.URL+"/admin/reload", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result reloadResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Equal(t, "ok", result.Status)
	assert.Empty(t, result.Warnings)
}

func TestHTTPAdminReload_Error(t *testing.T) {
	handlers, queue, _ := setupAdminTest(t)

	cfg := handlers.getConfig()
	handler := buildHTTPHandler(cfg, handlers, queue, "/nonexistent/config.toml")
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Post(server.URL+"/admin/reload", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result reloadResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Equal(t, "error", result.Status)
	assert.NotEmpty(t, result.Message)
}

func TestHTTPAdminReload_WithWarnings(t *testing.T) {
	handlers, queue, configPath := setupAdminTest(t)

	cfg := handlers.getConfig()
	handler := buildHTTPHandler(cfg, handlers, queue, configPath)
	server := httptest.NewServer(handler)
	defer server.Close()

	changed := testConfigToml(testConfigTomlOpts{
		MaxWorkers: 4,
	})
	dir := filepath.Dir(configPath)
	writeTestConfigFile(t, dir, changed)

	resp, err := http.Post(server.URL+"/admin/reload", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()

	var result reloadResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	assert.Equal(t, "ok", result.Status)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "max_workers")
}

func TestDoReload_Success(t *testing.T) {
	handlers, queue, configPath := setupAdminTest(t)

	result := doReload(configPath, handlers, queue)
	assert.Equal(t, "ok", result.Status)
	assert.Empty(t, result.Warnings)

	got := handlers.getConfig()
	assert.Equal(t, "http://localhost:8188", got.Comfy.BaseURL)
}

func TestDoReload_Error(t *testing.T) {
	handlers, queue, _ := setupAdminTest(t)

	result := doReload("/nonexistent/path.toml", handlers, queue)
	assert.Equal(t, "error", result.Status)
	assert.NotEmpty(t, result.Message)
}

func TestDoReload_WithWarnings(t *testing.T) {
	handlers, queue, configPath := setupAdminTest(t)

	dir := filepath.Dir(configPath)
	changed := testConfigToml(testConfigTomlOpts{
		ServerName: "img-mcp-v2",
	})
	writeTestConfigFile(t, dir, changed)

	result := doReload(configPath, handlers, queue)
	assert.Equal(t, "ok", result.Status)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "server.name")
}
