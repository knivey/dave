package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadConfigDefaults pins the documented defaults (example.toml
// reference block): a missing config file is not an error — defaults
// fill in.
func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	require.NoError(t, err)
	assert.Equal(t, ":8060", cfg.Server.Addr)
	assert.Equal(t, "sqlite", cfg.Database.Driver)
}

func TestLoadConfigFile(t *testing.T) {
	toml := `
[server]
addr = ":9090"
base_url = "https://sessions.example.com"

[database]
driver = "postgres"
dsn = "postgres://sesssite_ro:pass@localhost:5432/dave?sslmode=disable"
`
	path := filepath.Join(t.TempDir(), "sesssite.toml")
	require.NoError(t, os.WriteFile(path, []byte(toml), 0o644))

	cfg, err := loadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.Server.Addr)
	assert.Equal(t, "https://sessions.example.com", cfg.Server.BaseURL)
	assert.Equal(t, "postgres", cfg.Database.Driver)
	assert.Equal(t, "postgres://sesssite_ro:pass@localhost:5432/dave?sslmode=disable", cfg.Database.DSN)
	// setDefaults fills path even under postgres (harmless: openDB
	// ignores path for the postgres driver).
	assert.Equal(t, "data/dave.db", cfg.Database.Path)
}

func TestLoadConfigMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.toml")
	require.NoError(t, os.WriteFile(path, []byte("this is not = toml [[["), 0o644))
	_, err := loadConfig(path)
	require.Error(t, err)
}

func TestResolvePath(t *testing.T) {
	tests := []struct {
		name      string
		binaryDir string
		p         string
		want      string
	}{
		{"relative joins binary dir", "/opt/sesssite", "data/dave.db", "/opt/sesssite/data/dave.db"},
		{"absolute stays", "/opt/sesssite", "/var/lib/dave/dave.db", "/var/lib/dave/dave.db"},
		{"empty stays empty", "/opt/sesssite", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolvePath(tt.binaryDir, tt.p))
		})
	}
}
