package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func baseTestConfigToml(comfyURL string) string {
	return testConfigToml(testConfigTomlOpts{ComfyURL: comfyURL})
}

type testConfigTomlOpts struct {
	ComfyURL        string
	UploadURL       string
	NoUploadSection bool
	MaxWorkers      int
	MaxDepth        int
	ResultTTL       string
	ServerName      string
	ServerVersion   string
	ServerAddr      string
}

func testConfigToml(opts testConfigTomlOpts) string {
	if opts.ComfyURL == "" {
		opts.ComfyURL = "http://localhost:8188"
	}
	if opts.UploadURL == "" {
		opts.UploadURL = "https://img.example.com"
	}
	if opts.MaxWorkers == 0 {
		opts.MaxWorkers = 1
	}
	if opts.MaxDepth == 0 {
		opts.MaxDepth = 100
	}
	if opts.ResultTTL == "" {
		opts.ResultTTL = "1h"
	}
	if opts.ServerName == "" {
		opts.ServerName = "img-mcp"
	}
	if opts.ServerVersion == "" {
		opts.ServerVersion = "0.1.0"
	}
	if opts.ServerAddr == "" {
		opts.ServerAddr = ":8080"
	}
	uploadSection := `
[upload]
url = "` + opts.UploadURL + `"
`
	if opts.NoUploadSection {
		uploadSection = ""
	}
	return `
[server]
name = "` + opts.ServerName + `"
version = "` + opts.ServerVersion + `"
addr = "` + opts.ServerAddr + `"

[comfy]
baseurl = "` + opts.ComfyURL + `"
timeout = 60
default_workflow = "test"
` + uploadSection + `
[queue]
max_workers = ` + fmt.Sprintf("%d", opts.MaxWorkers) + `
max_depth = ` + fmt.Sprintf("%d", opts.MaxDepth) + `
result_ttl = "` + opts.ResultTTL + `"

[workflow.test]
workflow_path = "test_workflow.json"
clientid = "test-client"
output_node = "output-node"
prompt_node = "prompt-node"
timeout = 60
`
}

func TestLoadConfigUploadURLRequired(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	path := writeTestConfigFile(t, dir, testConfigToml(testConfigTomlOpts{NoUploadSection: true}))

	_, err := loadConfig(path)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "upload.url is required")
}

func TestLoadConfigLegacyUploadKeysIgnored(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	// Configs from the previous upload host still carry url_len/expiry;
	// the decoder ignores unknown keys, so they must load cleanly.
	toml := testConfigToml(testConfigTomlOpts{})
	toml = strings.Replace(toml,
		"[upload]\nurl = \"https://img.example.com\"",
		"[upload]\nurl = \"https://img.example.com\"\nurl_len = 16\nexpiry = 86400",
		1)
	path := writeTestConfigFile(t, dir, toml)

	cfg, err := loadConfig(path)

	require.NoError(t, err)
	assert.Equal(t, "https://img.example.com", cfg.Upload.URL)
}

func TestReloadConfigFromFile_ValidSwap(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))

	original, err := loadConfig(path)
	require.NoError(t, err)
	original.Database.Resolved = "/some/path.db"

	updated := testConfigToml(testConfigTomlOpts{
		ComfyURL:  "http://localhost:9999",
		UploadURL: "https://new-img.example.com",
		ResultTTL: "2h",
	})
	writeTestConfigFile(t, dir, updated)

	newCfg, warnings, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.Equal(t, "http://localhost:9999", newCfg.Comfy.BaseURL)
	assert.Equal(t, "https://new-img.example.com", newCfg.Upload.URL)
	assert.Equal(t, 2*time.Hour, newCfg.Queue.ResultTTL)
	assert.Equal(t, original.Server.Name, newCfg.Server.Name)
	assert.Equal(t, original.Server.Addr, newCfg.Server.Addr)
	assert.Equal(t, original.Queue.MaxWorkers, newCfg.Queue.MaxWorkers)
	assert.Equal(t, original.Queue.MaxDepth, newCfg.Queue.MaxDepth)
	assert.Equal(t, "/some/path.db", newCfg.Database.Resolved)
}

func TestReloadConfigFromFile_NonReloadableWarnings(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))

	original, err := loadConfig(path)
	require.NoError(t, err)

	changed := testConfigToml(testConfigTomlOpts{
		ServerName:    "img-mcp-v2",
		ServerVersion: "0.2.0",
		ServerAddr:    ":9090",
		MaxWorkers:    4,
		MaxDepth:      200,
	})
	writeTestConfigFile(t, dir, changed)

	newCfg, warnings, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)

	assert.Len(t, warnings, 5)
	assert.Contains(t, warnings[0], "server.name")
	assert.Contains(t, warnings[1], "server.version")
	assert.Contains(t, warnings[2], "server.addr")

	assert.Equal(t, "img-mcp", newCfg.Server.Name, "server.name should be preserved")
	assert.Equal(t, "0.1.0", newCfg.Server.Version, "server.version should be preserved")
	assert.Equal(t, ":8080", newCfg.Server.Addr, "server.addr should be preserved")
	assert.Equal(t, 1, newCfg.Queue.MaxWorkers, "max_workers should be preserved")
	assert.Equal(t, 100, newCfg.Queue.MaxDepth, "max_depth should be preserved")
}

func TestReloadConfigFromFile_InvalidConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfigFile(t, dir, `
[comfy]
this is not valid toml {{{
`)

	cfg := testConfig("http://localhost:8188")
	cfg.Server.Name = "img-mcp"
	cfg.Server.Version = "0.1.0"

	_, _, err := reloadConfigFromFile(path, cfg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "loading")
}

func TestReloadConfigFromFile_WorkflowResolvesPaths(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	workflowContent := `{"prompt-node": {"inputs": {"text": ""}, "class": "CLIPTextEncode"}}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new_workflow.json"), []byte(workflowContent), 0644))

	content := baseTestConfigToml("http://localhost:8188") + `
[workflow.new]
workflow_path = "new_workflow.json"
clientid = "new-client"
output_node = "prompt-node"
prompt_node = "prompt-node"
`
	path := writeTestConfigFile(t, dir, content)

	original, err := loadConfig(path)
	require.NoError(t, err)

	writeTestConfigFile(t, dir, content)

	newCfg, _, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)
	assert.Contains(t, newCfg.Workflows, "new")
	assert.Equal(t, filepath.Join(dir, "new_workflow.json"), newCfg.Workflows["new"].WorkflowPath)
	assert.Contains(t, newCfg.Workflows, "test", "original workflow should still be present")
}

func TestReloadConfigFromFile_PreservesDatabaseResolved(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))

	original, err := loadConfig(path)
	require.NoError(t, err)
	original.Database.Resolved = "/var/lib/img-mcp/img-mcp.db"
	original.Database.Path = "data/img-mcp.db"

	writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))

	newCfg, _, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)
	assert.Equal(t, "/var/lib/img-mcp/img-mcp.db", newCfg.Database.Resolved)
}

func TestReloadConfigFromFile_NewEnhancement(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))

	original, err := loadConfig(path)
	require.NoError(t, err)
	assert.Empty(t, original.Enhancements)

	updated := baseTestConfigToml("http://localhost:8188") + `
[enhancement.default]
baseurl = "https://api.example.com/v1/"
key = "test-key"
model = "test-model"
systemprompt = "enhance this"
timeout = 30
`
	writeTestConfigFile(t, dir, updated)

	newCfg, _, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)
	assert.Contains(t, newCfg.Enhancements, "default")
	assert.Equal(t, "test-model", newCfg.Enhancements["default"].Model)
}

func TestReloadConfigFromFile_PreservesAuthApiKey(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	cfgToml := baseTestConfigToml("http://localhost:8188") + `
[auth]
api_key = "original-secret"
`
	path := writeTestConfigFile(t, dir, cfgToml)

	original, err := loadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, "original-secret", original.Auth.APIKey)

	changed := baseTestConfigToml("http://localhost:8188") + `
[auth]
api_key = "changed-secret"
`
	writeTestConfigFile(t, dir, changed)

	newCfg, warnings, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)
	assert.Equal(t, "original-secret", newCfg.Auth.APIKey, "auth.api_key should be preserved on reload")
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], "auth.api_key")
}

func TestLoadConfig_NetworkPolicyValidation(t *testing.T) {
	t.Run("ValidPolicy", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteWorkflow(t, dir)
		content := baseTestConfigToml("http://localhost:8188") + `
[enhancement.default]
baseurl = "https://api.example.com/v1/"
key = "test-key"
model = "test-model"
systemprompt = "enhance this"

[network_policy.libera]
enhancement = "default"
force = true
`
		path := writeTestConfigFile(t, dir, content)
		cfg, err := loadConfig(path)
		require.NoError(t, err)
		require.Contains(t, cfg.NetworkPolicies, "libera")
		assert.Equal(t, "default", cfg.NetworkPolicies["libera"].Enhancement)
		assert.True(t, cfg.NetworkPolicies["libera"].Force)
	})

	t.Run("InvalidEnhancementReference", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteWorkflow(t, dir)
		content := baseTestConfigToml("http://localhost:8188") + `
[network_policy.libera]
enhancement = "nonexistent"
force = true
`
		path := writeTestConfigFile(t, dir, content)
		_, err := loadConfig(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "network_policy.libera")
		assert.Contains(t, err.Error(), "nonexistent")
	})

	t.Run("EmptyPoliciesAllowed", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteWorkflow(t, dir)
		path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))
		cfg, err := loadConfig(path)
		require.NoError(t, err)
		assert.Empty(t, cfg.NetworkPolicies)
	})

	t.Run("ForceDefaultsFalse", func(t *testing.T) {
		dir := t.TempDir()
		mustWriteWorkflow(t, dir)
		content := baseTestConfigToml("http://localhost:8188") + `
[enhancement.safe]
baseurl = "https://api.example.com/v1/"
key = "test-key"
model = "test-model"
systemprompt = "safe enhance"

[network_policy.graped]
enhancement = "safe"
`
		path := writeTestConfigFile(t, dir, content)
		cfg, err := loadConfig(path)
		require.NoError(t, err)
		assert.False(t, cfg.NetworkPolicies["graped"].Force)
	})
}

func TestReloadConfigFromFile_NetworkPolicies(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)

	enhancementToml := `
[enhancement.default]
baseurl = "https://api.example.com/v1/"
key = "test-key"
model = "test-model"
systemprompt = "enhance this"
`
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188")+enhancementToml)
	original, err := loadConfig(path)
	require.NoError(t, err)
	assert.Empty(t, original.NetworkPolicies)

	updated := baseTestConfigToml("http://localhost:8188") + enhancementToml + `
[network_policy.libera]
enhancement = "default"
force = true
`
	writeTestConfigFile(t, dir, updated)

	newCfg, warnings, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)
	assert.Empty(t, warnings)
	require.Contains(t, newCfg.NetworkPolicies, "libera")
	assert.Equal(t, "default", newCfg.NetworkPolicies["libera"].Enhancement)
	assert.True(t, newCfg.NetworkPolicies["libera"].Force)
}

func TestReloadConfigFromFile_NetworkPolicyValidationFails(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188"))

	original, err := loadConfig(path)
	require.NoError(t, err)

	badConfig := baseTestConfigToml("http://localhost:8188") + `
[network_policy.libera]
enhancement = "nonexistent"
`
	writeTestConfigFile(t, dir, badConfig)

	_, _, err = reloadConfigFromFile(path, original)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network_policy.libera")
}

func TestCompareNonReloadable(t *testing.T) {
	base := Config{
		Server:   ServerConfig{Name: "img-mcp", Version: "0.1.0", Addr: ":8080"},
		Database: DatabaseConfig{Path: "data/img-mcp.db"},
		Queue:    QueueConfig{MaxWorkers: 1, MaxDepth: 100},
	}

	t.Run("NoChanges", func(t *testing.T) {
		warnings := compareNonReloadable(base, base)
		assert.Empty(t, warnings)
	})

	t.Run("ServerNameChanged", func(t *testing.T) {
		changed := base
		changed.Server.Name = "new-name"
		warnings := compareNonReloadable(base, changed)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "server.name")
		assert.Contains(t, warnings[0], "requires restart")
	})

	t.Run("MultipleChanges", func(t *testing.T) {
		changed := base
		changed.Server.Name = "new"
		changed.Queue.MaxWorkers = 4
		changed.Queue.MaxDepth = 200
		changed.Database.Path = "other.db"
		warnings := compareNonReloadable(base, changed)
		assert.Len(t, warnings, 4)
	})

	t.Run("ReloadableFieldsIgnored", func(t *testing.T) {
		changed := base
		changed.Comfy.Timeout = 999
		changed.Comfy.BaseURL = "http://new:8188"
		changed.Upload.URL = "https://new-upload.com"
		changed.Queue.ResultTTL = 5 * time.Minute
		warnings := compareNonReloadable(base, changed)
		assert.Empty(t, warnings)
	})

	t.Run("AuthApiKeyChanged", func(t *testing.T) {
		changed := base
		changed.Auth.APIKey = "new-secret"
		warnings := compareNonReloadable(base, changed)
		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "auth.api_key")
		assert.Contains(t, warnings[0], "requires restart")
	})
}

func TestLoadConfigEnhancementAPIFields(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	content := baseTestConfigToml("http://localhost:8188") + `
[enhancement.default]
baseurl = "https://api.x.ai/v1/"
key = "test-key"
model = "grok-4.6"
systemprompt = "enhance"
timeout = 30
responses_api = true
reasoning_effort = "low"
`
	path := writeTestConfigFile(t, dir, content)

	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Contains(t, cfg.Enhancements, "default")
	assert.True(t, cfg.Enhancements["default"].ResponsesAPI)
	assert.Equal(t, "low", cfg.Enhancements["default"].ReasoningEffort)
}

func TestLoadConfigEnhancementAPIDefaults(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	content := baseTestConfigToml("http://localhost:8188") + `
[enhancement.default]
baseurl = "https://api.x.ai/v1/"
key = "test-key"
model = "grok-4.6"
systemprompt = "enhance"
timeout = 30
`
	path := writeTestConfigFile(t, dir, content)

	cfg, err := loadConfig(path)
	require.NoError(t, err)
	require.Contains(t, cfg.Enhancements, "default")
	assert.False(t, cfg.Enhancements["default"].ResponsesAPI, "responses_api must default to false (Chat Completions)")
	assert.Empty(t, cfg.Enhancements["default"].ReasoningEffort, "reasoning_effort must default to empty (not sent)")
}

func TestReloadConfigFromFile_EnhancementAPIFieldsReloadable(t *testing.T) {
	dir := t.TempDir()
	mustWriteWorkflow(t, dir)
	base := `
[enhancement.default]
baseurl = "https://api.x.ai/v1/"
key = "k"
model = "grok-4.6"
systemprompt = "enhance"
timeout = 30
`
	path := writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188")+base)

	original, err := loadConfig(path)
	require.NoError(t, err)
	assert.False(t, original.Enhancements["default"].ResponsesAPI)

	writeTestConfigFile(t, dir, baseTestConfigToml("http://localhost:8188")+base+`responses_api = true
reasoning_effort = "high"
`)

	newCfg, warnings, err := reloadConfigFromFile(path, original)
	require.NoError(t, err)
	assert.Empty(t, warnings, "enhancement API fields must be reloadable without restart warnings")
	assert.True(t, newCfg.Enhancements["default"].ResponsesAPI)
	assert.Equal(t, "high", newCfg.Enhancements["default"].ReasoningEffort)
}
