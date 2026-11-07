package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
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
	handler, _ := buildHTTPHandler(cfg, handlers, queue, configPath)
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
	handler, _ := buildHTTPHandler(cfg, handlers, queue, "/nonexistent/config.toml")
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
	handler, _ := buildHTTPHandler(cfg, handlers, queue, configPath)
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

	result := doReload(configPath, handlers, queue, nil)
	assert.Equal(t, "ok", result.Status)
	assert.Empty(t, result.Warnings)

	got := handlers.getConfig()
	assert.Equal(t, "http://localhost:8188", got.Comfy.BaseURL)
}

func TestDoReload_Error(t *testing.T) {
	handlers, queue, _ := setupAdminTest(t)

	result := doReload("/nonexistent/path.toml", handlers, queue, nil)
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

	result := doReload(configPath, handlers, queue, nil)
	assert.Equal(t, "ok", result.Status)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "server.name")
}

// TestDoReloadRefreshesEditPolicy pins the edit_policy hot-reload seam in
// doReload: a CHANGED [safety] edit_policy calls every liveServer's refresh
// with the new config (re-registering the edit tools), an UNCHANGED policy
// does not, and a failed reload never reaches the comparison. The stub
// records refresh calls so no real server is needed.
func TestDoReloadRefreshesEditPolicy(t *testing.T) {
	recording := func() (liveServer, func() []Config) {
		var mu sync.Mutex
		var calls []Config
		return liveServer{refresh: func(cfg Config) {
			mu.Lock()
			calls = append(calls, cfg)
			mu.Unlock()
		}}, func() []Config {
			mu.Lock()
			defer mu.Unlock()
			return append([]Config{}, calls...)
		}
	}

	t.Run("unchanged policy: no refresh", func(t *testing.T) {
		handlers, queue, configPath := setupAdminTest(t)
		stub, calls := recording()

		result := doReload(configPath, handlers, queue, []liveServer{stub})

		assert.Equal(t, "ok", result.Status)
		assert.Empty(t, calls(), "same edit_policy must not re-register")
	})

	t.Run("changed policy: refresh called with the new config", func(t *testing.T) {
		handlers, queue, configPath := setupAdminTest(t)
		stub, calls := recording()

		writeTestConfigFile(t, filepath.Dir(configPath), testConfigToml(testConfigTomlOpts{
			EditPolicy: "no ducks",
		}))

		result := doReload(configPath, handlers, queue, []liveServer{stub})

		assert.Equal(t, "ok", result.Status)
		got := calls()
		require.Len(t, got, 1, "changed edit_policy must refresh the live server once")
		assert.Equal(t, "no ducks", got[0].Safety.EditPolicy, "refresh receives the NEW config")
	})

	t.Run("reload error: no refresh", func(t *testing.T) {
		handlers, queue, _ := setupAdminTest(t)
		stub, calls := recording()

		result := doReload("/nonexistent/path.toml", handlers, queue, []liveServer{stub})

		assert.Equal(t, "error", result.Status)
		assert.Empty(t, calls(), "a failed reload must never re-register")
	})
}
