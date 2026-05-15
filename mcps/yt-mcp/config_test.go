package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTestConfigFile(t *testing.T, dir string, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestLoadConfigNoFile(t *testing.T) {
	dir := t.TempDir()
	cfg, err := loadConfig(filepath.Join(dir, "config.toml"))
	require.NoError(t, err)

	assert.Equal(t, "yt-mcp", cfg.Server.Name)
	assert.Equal(t, "0.1.0", cfg.Server.Version)
	assert.Equal(t, "yt-dlp", cfg.Ytdlp.Path)
	assert.Equal(t, 2*time.Minute, cfg.Ytdlp.Timeout)
	assert.Equal(t, []string{"en"}, cfg.Ytdlp.Languages)
	assert.Equal(t, 50000, cfg.Ytdlp.MaxLength)
	assert.Equal(t, os.TempDir(), cfg.Ytdlp.TempDir)
}

func TestLoadConfigCustom(t *testing.T) {
	dir := t.TempDir()
	writeTestConfigFile(t, dir, `
[server]
name = "yt-mcp-test"
version = "0.2.0"

[ytdlp]
timeout = "30s"
languages = ["en", "es"]
max_length = 5000
temp_dir = "/tmp"
`)
	cfg, err := loadConfig(filepath.Join(dir, "config.toml"))
	require.NoError(t, err)

	assert.Equal(t, "yt-mcp-test", cfg.Server.Name)
	assert.Equal(t, "0.2.0", cfg.Server.Version)
	assert.Equal(t, "yt-dlp", cfg.Ytdlp.Path)
	assert.Equal(t, 30*time.Second, cfg.Ytdlp.Timeout)
	assert.Equal(t, []string{"en", "es"}, cfg.Ytdlp.Languages)
	assert.Equal(t, 5000, cfg.Ytdlp.MaxLength)
	assert.Equal(t, "/tmp", cfg.Ytdlp.TempDir)
}

func TestLoadConfigMultipleLanguages(t *testing.T) {
	dir := t.TempDir()
	writeTestConfigFile(t, dir, `
[ytdlp]
languages = ["en", "es", "fr"]
`)
	cfg, err := loadConfig(filepath.Join(dir, "config.toml"))
	require.NoError(t, err)

	assert.Equal(t, []string{"en", "es", "fr"}, cfg.Ytdlp.Languages)
}

func TestLoadConfigInvalidToml(t *testing.T) {
	dir := t.TempDir()
	writeTestConfigFile(t, dir, `this is not valid toml [[[[`)

	_, err := loadConfig(filepath.Join(dir, "config.toml"))
	assert.Error(t, err)
}
