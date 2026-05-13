package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApiKeyMiddleware_ValidKey(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := apiKeyMiddleware("secret123")(inner)
	req := httptest.NewRequest("POST", "/test", nil)
	req.Header.Set("X-API-Key", "secret123")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.True(t, called, "inner handler should be called")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestApiKeyMiddleware_InvalidKey(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := apiKeyMiddleware("secret123")(inner)
	req := httptest.NewRequest("POST", "/test", nil)
	req.Header.Set("X-API-Key", "wrong")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.False(t, called, "inner handler should not be called")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestApiKeyMiddleware_MissingKey(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := apiKeyMiddleware("secret123")(inner)
	req := httptest.NewRequest("POST", "/test", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.False(t, called, "inner handler should not be called")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestApiKeyMiddleware_EmptyKey(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	handler := apiKeyMiddleware("secret123")(inner)
	req := httptest.NewRequest("POST", "/test", nil)
	req.Header.Set("X-API-Key", "")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.False(t, called, "inner handler should not be called")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestBuildHTTPHandler_WithAuth(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	cfgToml := baseTestConfigToml("http://localhost:8188") + `
[auth]
api_key = "test-secret-key"
`
	configPath := writeTestConfigFile(t, dir, cfgToml)
	cfg, err := loadConfig(configPath)
	require.NoError(t, err)

	db, err := initDB(dir + "/test.db")
	require.NoError(t, err)
	defer db.Close()

	queue := NewJobQueue(cfg, db)
	defer queue.Stop()
	handlers := NewToolHandlers(cfg, queue)

	handler := buildHTTPHandler(cfg, handlers, queue, configPath)
	server := httptest.NewServer(handler)
	defer server.Close()

	t.Run("no key returns 401", func(t *testing.T) {
		resp, err := http.Post(server.URL+"/admin/reload", "application/json", nil)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("wrong key returns 401", func(t *testing.T) {
		req, err := http.NewRequest("POST", server.URL+"/admin/reload", nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", "wrong-key")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})

	t.Run("correct key returns 200", func(t *testing.T) {
		req, err := http.NewRequest("POST", server.URL+"/admin/reload", nil)
		require.NoError(t, err)
		req.Header.Set("X-API-Key", "test-secret-key")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode)
	})
}

func TestBuildHTTPHandler_WithoutAuth(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	configPath := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))
	cfg, err := loadConfig(configPath)
	require.NoError(t, err)

	db, err := initDB(dir + "/test.db")
	require.NoError(t, err)
	defer db.Close()

	queue := NewJobQueue(cfg, db)
	defer queue.Stop()
	handlers := NewToolHandlers(cfg, queue)

	handler := buildHTTPHandler(cfg, handlers, queue, configPath)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Post(server.URL+"/admin/reload", "application/json", nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
}
