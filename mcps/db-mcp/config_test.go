package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "nonexistent.toml")

	cfg, err := loadConfig(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, "db-mcp", cfg.Server.Name)
	assert.Equal(t, "0.1.0", cfg.Server.Version)
	assert.Equal(t, filepath.Join(dir, "data/notes.db"), cfg.Database.Path)
	assert.Equal(t, 10000, cfg.Database.MaxValueSize)
	assert.Equal(t, 500, cfg.Database.MaxNotesPerUser)
}

func TestLoadConfigFromFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	content := `
[server]
name = "custom-db"
version = "2.0.0"

[database]
path = "custom/path.db"
max_value_size = 5000
max_notes_per_user = 100
`
	err := os.WriteFile(cfgPath, []byte(content), 0644)
	require.NoError(t, err)

	cfg, err := loadConfig(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, "custom-db", cfg.Server.Name)
	assert.Equal(t, "2.0.0", cfg.Server.Version)
	assert.Equal(t, filepath.Join(dir, "custom/path.db"), cfg.Database.Path)
	assert.Equal(t, 5000, cfg.Database.MaxValueSize)
	assert.Equal(t, 100, cfg.Database.MaxNotesPerUser)
}

func TestLoadConfigPartialOverride(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	content := `
[database]
max_value_size = 2000
`
	err := os.WriteFile(cfgPath, []byte(content), 0644)
	require.NoError(t, err)

	cfg, err := loadConfig(cfgPath)
	require.NoError(t, err)

	assert.Equal(t, "db-mcp", cfg.Server.Name)
	assert.Equal(t, 2000, cfg.Database.MaxValueSize)
	assert.Equal(t, 500, cfg.Database.MaxNotesPerUser)
}

func TestResolvePath(t *testing.T) {
	assert.Equal(t, "/abs/path.db", resolvePath("/base", "/abs/path.db"))
	assert.Equal(t, "/base/rel/path.db", resolvePath("/base", "rel/path.db"))
	assert.Equal(t, "", resolvePath("/base", ""))
}
