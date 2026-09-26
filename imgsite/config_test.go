package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0644))
	return path
}

func TestLoadConfigDefaults(t *testing.T) {
	path := writeConfigFile(t, `
[auth]
api_key = "secret"
`)

	cfg, err := loadConfig(path)

	require.NoError(t, err)
	assert.Equal(t, "imgsite", cfg.Server.Name)
	assert.Equal(t, ":8081", cfg.Server.Addr)
	assert.Equal(t, "", cfg.Server.BaseURL)
	assert.Equal(t, "data/imgsite.db", cfg.Database.Path)
	assert.Equal(t, "data/images", cfg.Storage.Path)
	assert.Equal(t, "data/thumbs", cfg.Storage.ThumbsPath)
	assert.Equal(t, 480, cfg.Thumbnails.SmallWidth)
	assert.Equal(t, 1280, cfg.Thumbnails.DisplayWidth)
	assert.Equal(t, 78, cfg.Thumbnails.JPEGQuality)
	assert.Equal(t, 2, cfg.Thumbnails.Workers)
	assert.Equal(t, 8192, cfg.Thumbnails.MaxDimension)
	assert.Equal(t, int64(32<<20), cfg.Upload.MaxBytes)
	assert.Equal(t, 30, cfg.Upload.RatePerMinute)
	assert.Equal(t, 160, cfg.Search.SnippetChars)
	assert.Equal(t, 2, cfg.Search.PrefixMin)
	assert.Equal(t, "dave's image dump", cfg.Site.Title)
	assert.Equal(t, "generations from IRC", cfg.Site.Description)
}

func TestLoadConfigMissingAPIKey(t *testing.T) {
	path := writeConfigFile(t, `
[server]
name = "x"
`)

	_, err := loadConfig(path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "auth.api_key is required")
}

func TestLoadConfigPlaceholderAPIKeyRejected(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"ShippedPlaceholder", "CHANGE_ME_TO_A_STRONG_RANDOM_SECRET"},
		{"PlaceholderPrefix", "CHANGE_ME_short"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, "[auth]\napi_key = \""+tt.key+"\"\n")

			_, err := loadConfig(path)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "placeholder")
		})
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	tests := []struct {
		name   string
		extra  string
		errMsg string
	}{
		{"JPEGQualityTooHigh", "[thumbnails]\njpeg_quality = 101", "jpeg_quality"},
		{"JPEGQualityTooLow", "[thumbnails]\njpeg_quality = -1", "jpeg_quality"},
		{"WorkersNegative", "[thumbnails]\nworkers = -1", "workers"},
		{"MaxBytesNegative", "[upload]\nmax_bytes = -5", "max_bytes"},
		{"RatePerMinuteNegative", "[upload]\nrate_per_minute = -1", "rate_per_minute"},
		{"SnippetCharsNegative", "[search]\nsnippet_chars = -1", "snippet_chars"},
		{"PrefixMinNegative", "[search]\nprefix_min = -1", "prefix_min"},
		{"SmallWidthNegative", "[thumbnails]\nsmall_width = -1", "small_width"},
		{"MaxDimensionNegative", "[thumbnails]\nmax_dimension = -1", "max_dimension"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, "[auth]\napi_key = \"secret\"\n\n"+tt.extra)

			_, err := loadConfig(path)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestLoadConfigCustom(t *testing.T) {
	path := writeConfigFile(t, `
[server]
name = "my-imgsite"
addr = ":9999"
base_url = "https://img.example.com/"

[database]
path = "/var/lib/imgsite/site.db"

[storage]
path = "/srv/images"
thumbs_path = "/srv/thumbs"

[auth]
api_key = "real-key"

[thumbnails]
small_width = 320
display_width = 1024
jpeg_quality = 85
workers = 4
max_dimension = 4096

[upload]
max_bytes = 1048576
rate_per_minute = 12

[search]
snippet_chars = 100
prefix_min = 3

[site]
title = "custom title"
description = "custom desc"
`)

	cfg, err := loadConfig(path)

	require.NoError(t, err)
	assert.Equal(t, "my-imgsite", cfg.Server.Name)
	assert.Equal(t, ":9999", cfg.Server.Addr)
	assert.Equal(t, "https://img.example.com/", cfg.Server.BaseURL)
	assert.Equal(t, "/var/lib/imgsite/site.db", cfg.Database.Path)
	assert.Equal(t, "/srv/images", cfg.Storage.Path)
	assert.Equal(t, "/srv/thumbs", cfg.Storage.ThumbsPath)
	assert.Equal(t, "real-key", cfg.Auth.APIKey)
	assert.Equal(t, 320, cfg.Thumbnails.SmallWidth)
	assert.Equal(t, 1024, cfg.Thumbnails.DisplayWidth)
	assert.Equal(t, 85, cfg.Thumbnails.JPEGQuality)
	assert.Equal(t, 4, cfg.Thumbnails.Workers)
	assert.Equal(t, 4096, cfg.Thumbnails.MaxDimension)
	assert.Equal(t, int64(1048576), cfg.Upload.MaxBytes)
	assert.Equal(t, 12, cfg.Upload.RatePerMinute)
	assert.Equal(t, 100, cfg.Search.SnippetChars)
	assert.Equal(t, 3, cfg.Search.PrefixMin)
	assert.Equal(t, "custom title", cfg.Site.Title)
	assert.Equal(t, "custom desc", cfg.Site.Description)
}

func TestLoadConfigMissingFile(t *testing.T) {
	_, err := loadConfig(filepath.Join(t.TempDir(), "nope.toml"))
	require.Error(t, err)
}

func TestReloadConfigFromFileAppliesReloadableSet(t *testing.T) {
	path := writeConfigFile(t, `
[auth]
api_key = "test-api-key"

[server]
addr = ":0"

[site]
title = "reloaded title"
description = "reloaded desc"

[upload]
rate_per_minute = 55

[thumbnails]
small_width = 640
jpeg_quality = 90

[search]
snippet_chars = 200
`)
	current := testConfig()
	current.Database.Resolved = "/resolved/db.sqlite"
	current.Storage.ResolvedPath = "/resolved/images"
	current.Storage.ResolvedThumbsPath = "/resolved/thumbs"

	newCfg, warnings, err := reloadConfigFromFile(path, current)

	require.NoError(t, err)
	assert.Empty(t, warnings, "nothing non-reloadable changed")

	// Reloadable fields took the new values.
	assert.Equal(t, "reloaded title", newCfg.Site.Title)
	assert.Equal(t, "reloaded desc", newCfg.Site.Description)
	assert.Equal(t, 55, newCfg.Upload.RatePerMinute)
	assert.Equal(t, 640, newCfg.Thumbnails.SmallWidth)
	assert.Equal(t, 90, newCfg.Thumbnails.JPEGQuality)
	assert.Equal(t, 200, newCfg.Search.SnippetChars)
}

func TestReloadConfigFromFileNonReloadableFrozen(t *testing.T) {
	path := writeConfigFile(t, `
[server]
addr = ":9999"

[database]
path = "data/other.db"

[storage]
path = "data/other-images"

[auth]
api_key = "changed-key"

[thumbnails]
workers = 9

[upload]
rate_per_minute = 42
`)
	current := testConfig()
	current.Database.Resolved = "/resolved/db.sqlite"
	current.Storage.ResolvedPath = "/resolved/images"
	current.Storage.ResolvedThumbsPath = "/resolved/thumbs"

	newCfg, warnings, err := reloadConfigFromFile(path, current)

	require.NoError(t, err)

	// Non-reloadable sections stay frozen at their startup values,
	// including the resolved paths.
	assert.Equal(t, current.Server, newCfg.Server)
	assert.Equal(t, current.Auth, newCfg.Auth)
	assert.Equal(t, current.Database, newCfg.Database)
	assert.Equal(t, current.Storage, newCfg.Storage)
	assert.Equal(t, current.Thumbnails.Workers, newCfg.Thumbnails.Workers)
	assert.Equal(t, "/resolved/db.sqlite", newCfg.Database.Resolved)
	assert.Equal(t, "/resolved/images", newCfg.Storage.ResolvedPath)

	// ...and every attempted change is warned about.
	assert.Contains(t, warningsString(warnings), "server.addr")
	assert.Contains(t, warningsString(warnings), "database.path")
	assert.Contains(t, warningsString(warnings), "storage.path")
	assert.Contains(t, warningsString(warnings), "auth.api_key")
	assert.Contains(t, warningsString(warnings), "thumbnails.workers")

	// Reloadable fields still applied.
	assert.Equal(t, 42, newCfg.Upload.RatePerMinute)
}

func TestReloadConfigFromFileInvalidKeepsCurrent(t *testing.T) {
	path := writeConfigFile(t, `
[site]
title = "no key configured"
`)
	current := testConfig()

	newCfg, warnings, err := reloadConfigFromFile(path, current)

	require.Error(t, err)
	assert.Nil(t, warnings)
	assert.Equal(t, current, newCfg, "failed reloads return the current config untouched")
}

func warningsString(warnings []string) string {
	out := ""
	for _, w := range warnings {
		out += w + "\n"
	}
	return out
}

func TestLoadConfigSafeSite(t *testing.T) {
	t.Run("AbsentSectionMeansNil", func(t *testing.T) {
		path := writeConfigFile(t, "[auth]\napi_key = \"secret\"\n")

		cfg, err := loadConfig(path)

		require.NoError(t, err)
		assert.Nil(t, cfg.SafeSite, "no [safe_site] section = single-site behavior identical to today")
	})

	t.Run("ValidSectionLoaded", func(t *testing.T) {
		path := writeConfigFile(t, `
[auth]
api_key = "secret"

[safe_site]
hosts = ["safe.example.com", "alt.example.org"]
base_url = "https://safe.example.com"
allowed_networks = ["libera", "efnet"]
`)

		cfg, err := loadConfig(path)

		require.NoError(t, err)
		require.NotNil(t, cfg.SafeSite)
		assert.Equal(t, []string{"safe.example.com", "alt.example.org"}, cfg.SafeSite.Hosts)
		assert.Equal(t, "https://safe.example.com", cfg.SafeSite.BaseURL)
		assert.Equal(t, []string{"libera", "efnet"}, cfg.SafeSite.AllowedNetworks)
	})
}

func TestLoadConfigSafeSiteInvalid(t *testing.T) {
	tests := []struct {
		name    string
		section string
		errMsg  string
	}{
		{"MissingHosts", `
[safe_site]
base_url = "https://safe.example.com"
allowed_networks = ["libera"]`, "safe_site.hosts"},
		{"ExplicitEmptyHosts", `
[safe_site]
hosts = []
base_url = "https://safe.example.com"
allowed_networks = ["libera"]`, "safe_site.hosts"},
		{"MissingBaseURL", `
[safe_site]
hosts = ["safe.example.com"]
allowed_networks = ["libera"]`, "safe_site.base_url"},
		{"MissingAllowedNetworks", `
[safe_site]
hosts = ["safe.example.com"]
base_url = "https://safe.example.com"`, "safe_site.allowed_networks"},
		{"ExplicitEmptyAllowedNetworks", `
[safe_site]
hosts = ["safe.example.com"]
base_url = "https://safe.example.com"
allowed_networks = []`, "safe_site.allowed_networks"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, "[auth]\napi_key = \"secret\"\n"+tt.section)

			_, err := loadConfig(path)

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestReloadConfigSafeSiteHotSwaps(t *testing.T) {
	safe := &SafeSiteConfig{
		Hosts:           []string{"safe.example.com"},
		BaseURL:         "https://safe.example.com",
		AllowedNetworks: []string{"libera"},
	}

	t.Run("AddedOnReload", func(t *testing.T) {
		path := writeConfigFile(t, `
[auth]
api_key = "test-api-key"

[server]
addr = ":0"

[safe_site]
hosts = ["safe.example.com"]
base_url = "https://safe.example.com"
allowed_networks = ["libera"]
`)
		current := testConfig()

		newCfg, warnings, err := reloadConfigFromFile(path, current)

		require.NoError(t, err)
		assert.Empty(t, warnings, "safe_site is reloadable — no restart warning")
		require.NotNil(t, newCfg.SafeSite)
		assert.Equal(t, safe, newCfg.SafeSite)
	})

	t.Run("RemovedOnReload", func(t *testing.T) {
		path := writeConfigFile(t, `
[auth]
api_key = "test-api-key"

[server]
addr = ":0"
`)
		current := testConfig()
		current.SafeSite = safe

		newCfg, _, err := reloadConfigFromFile(path, current)

		require.NoError(t, err)
		assert.Nil(t, newCfg.SafeSite, "omitting the section disables the safe site without a restart")
	})
}

func TestReloadConfigSafeSiteInvalidKeepsCurrent(t *testing.T) {
	path := writeConfigFile(t, `
[auth]
api_key = "test-api-key"

[server]
addr = ":0"

[safe_site]
hosts = ["safe.example.com"]
base_url = ""
allowed_networks = ["libera"]
`)
	current := testConfig()
	current.SafeSite = &SafeSiteConfig{
		Hosts:           []string{"safe.example.com"},
		BaseURL:         "https://safe.example.com",
		AllowedNetworks: []string{"libera"},
	}

	newCfg, warnings, err := reloadConfigFromFile(path, current)

	require.Error(t, err)
	assert.Nil(t, warnings)
	assert.Equal(t, current, newCfg, "failed reloads return the current config untouched")
}
