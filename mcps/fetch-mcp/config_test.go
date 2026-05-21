package main

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestLoadConfigDefaults(t *testing.T) {
	for _, key := range []string{
		"FETCH_SERVER_NAME", "FETCH_SERVER_VERSION", "FETCH_SERVER_ADDR",
		"FETCH_USER_AGENT", "FETCH_TIMEOUT", "FETCH_MAX_REDIRECTS",
		"FETCH_PROXY_URL", "FETCH_CACHE_DSN", "FETCH_CACHE_MARKDOWN_TTL",
		"FETCH_READABILITY_DISABLED",
	} {
		os.Unsetenv(key)
	}

	cfg, err := loadConfig()
	assert.NoError(t, err)
	assert.Equal(t, "fetch-mcp", cfg.Server.Name)
	assert.Equal(t, "0.1.0", cfg.Server.Version)
	assert.Equal(t, ":8080", cfg.Server.Addr)
	assert.Equal(t, "fetch-mcp/0.1.0", cfg.Fetch.UserAgent)
	assert.Equal(t, 30*time.Second, cfg.Fetch.Timeout)
	assert.Equal(t, 10, cfg.Fetch.MaxRedirects)
	assert.Equal(t, "", cfg.Fetch.ProxyURL)
	assert.Equal(t, "memcache://", cfg.Cache.DSN)
	assert.Equal(t, 15*time.Minute, cfg.Cache.MaxMarkdownAge)
	assert.False(t, cfg.Readability.Disabled)
}

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("FETCH_USER_AGENT", "TestBot/1.0")
	t.Setenv("FETCH_TIMEOUT", "45s")
	t.Setenv("FETCH_MAX_REDIRECTS", "5")
	t.Setenv("FETCH_PROXY_URL", "socks5://127.0.0.1:1080")
	t.Setenv("FETCH_CACHE_DSN", "memcache://")
	t.Setenv("FETCH_CACHE_MARKDOWN_TTL", "30m")
	t.Setenv("FETCH_READABILITY_DISABLED", "true")

	cfg, err := loadConfig()
	assert.NoError(t, err)
	assert.Equal(t, "TestBot/1.0", cfg.Fetch.UserAgent)
	assert.Equal(t, 45*time.Second, cfg.Fetch.Timeout)
	assert.Equal(t, 5, cfg.Fetch.MaxRedirects)
	assert.Equal(t, "socks5://127.0.0.1:1080", cfg.Fetch.ProxyURL)
	assert.Equal(t, 30*time.Minute, cfg.Cache.MaxMarkdownAge)
	assert.True(t, cfg.Readability.Disabled)
}

func TestLoadConfigInvalidProxyURL(t *testing.T) {
	t.Setenv("FETCH_PROXY_URL", "://bad")
	_, err := loadConfig()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "FETCH_PROXY_URL")
}

func TestLoadConfigInvalidDuration(t *testing.T) {
	t.Setenv("FETCH_TIMEOUT", "not-a-duration")
	cfg, err := loadConfig()
	assert.NoError(t, err)
	assert.Equal(t, 30*time.Second, cfg.Fetch.Timeout)
}
