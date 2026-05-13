package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server       ServerConfig                 `toml:"server"`
	Database     DatabaseConfig               `toml:"database"`
	Comfy        ComfyServiceConfig           `toml:"comfy"`
	Upload       UploadConfig                 `toml:"upload"`
	Queue        QueueConfig                  `toml:"queue"`
	Auth         AuthConfig                   `toml:"auth"`
	Enhancements map[string]EnhancementConfig `toml:"enhancement"`
	Workflows    map[string]WorkflowConfig    `toml:"workflow"`
}

type AuthConfig struct {
	APIKey string `toml:"api_key"`
}

type ServerConfig struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
	Addr    string `toml:"addr"`
}

type DatabaseConfig struct {
	Path     string `toml:"path"`
	Resolved string `toml:"-"`
}

type ComfyServiceConfig struct {
	BaseURL         string `toml:"baseurl"`
	Timeout         int    `toml:"timeout"`
	DefaultWorkflow string `toml:"default_workflow"`
}

type UploadConfig struct {
	URL    string `toml:"url"`
	URLLen int    `toml:"url_len"`
	Expiry int    `toml:"expiry"`
}

type QueueConfig struct {
	MaxWorkers int           `toml:"max_workers"`
	MaxDepth   int           `toml:"max_depth"`
	ResultTTL  time.Duration `toml:"result_ttl"`
}

type EnhancementConfig struct {
	BaseURL      string `toml:"baseurl"`
	Key          string `toml:"key"`
	Model        string `toml:"model"`
	SystemPrompt string `toml:"systemprompt"`
	Timeout      int    `toml:"timeout"`
	Description  string `toml:"description"`
}

type WorkflowConfig struct {
	WorkflowPath       string        `toml:"workflow_path"`
	ClientID           string        `toml:"clientid"`
	OutputNode         string        `toml:"output_node"`
	PromptNode         string        `toml:"prompt_node"`
	NegativePromptNode string        `toml:"negative_prompt_node"`
	SeedNodes          []string      `toml:"seed_nodes"`
	Timeout            int           `toml:"timeout"`
	TypicalTime        time.Duration `toml:"typical_time"`
	Description        string        `toml:"description"`
}

func loadConfig(configFile string) (Config, error) {
	var cfg Config

	if _, err := toml.DecodeFile(configFile, &cfg); err != nil {
		return cfg, fmt.Errorf("loading %s: %w", configFile, err)
	}

	configDir := filepath.Dir(configFile)

	cfg.Database.Path = defaultString(cfg.Database.Path, "data/img-mcp.db")

	cfg.Server.Name = defaultString(cfg.Server.Name, "img-mcp")
	cfg.Server.Version = defaultString(cfg.Server.Version, "0.1.0")
	cfg.Server.Addr = defaultString(cfg.Server.Addr, ":8080")

	if cfg.Comfy.BaseURL == "" {
		return cfg, fmt.Errorf("comfy.baseurl is required")
	}
	if cfg.Comfy.Timeout == 0 {
		cfg.Comfy.Timeout = 300
	}

	cfg.Upload.URL = defaultString(cfg.Upload.URL, "https://upload.beer")
	if cfg.Upload.URLLen == 0 {
		cfg.Upload.URLLen = 16
	}
	if cfg.Upload.Expiry == 0 {
		cfg.Upload.Expiry = 86400
	}

	if cfg.Queue.MaxWorkers == 0 {
		cfg.Queue.MaxWorkers = 1
	}
	if cfg.Queue.MaxDepth == 0 {
		cfg.Queue.MaxDepth = 100
	}
	if cfg.Queue.ResultTTL == 0 {
		cfg.Queue.ResultTTL = 1 * time.Hour
	}

	if cfg.Enhancements == nil {
		cfg.Enhancements = make(map[string]EnhancementConfig)
	}
	for name, ec := range cfg.Enhancements {
		if ec.BaseURL == "" {
			return cfg, fmt.Errorf("enhancement.%s baseurl is required", name)
		}
		if ec.Key == "" {
			return cfg, fmt.Errorf("enhancement.%s key is required", name)
		}
		if ec.Model == "" {
			return cfg, fmt.Errorf("enhancement.%s model is required", name)
		}
		if ec.SystemPrompt == "" {
			return cfg, fmt.Errorf("enhancement.%s systemprompt is required", name)
		}
		if ec.Timeout == 0 {
			ec.Timeout = 30
		}
		cfg.Enhancements[name] = ec
	}

	if cfg.Workflows == nil {
		cfg.Workflows = make(map[string]WorkflowConfig)
	}
	for name, wc := range cfg.Workflows {
		wc.WorkflowPath = resolvePath(configDir, wc.WorkflowPath)
		if wc.WorkflowPath == "" {
			return cfg, fmt.Errorf("workflow.%s workflow_path is required", name)
		}
		if _, err := os.Stat(wc.WorkflowPath); err != nil {
			return cfg, fmt.Errorf("workflow.%s workflow_path %s: %w", name, wc.WorkflowPath, err)
		}
		if wc.ClientID == "" {
			return cfg, fmt.Errorf("workflow.%s clientid is required", name)
		}
		if wc.OutputNode == "" {
			return cfg, fmt.Errorf("workflow.%s output_node is required", name)
		}
		if wc.PromptNode == "" {
			return cfg, fmt.Errorf("workflow.%s prompt_node is required", name)
		}
		if wc.Timeout == 0 {
			wc.Timeout = cfg.Comfy.Timeout
		}
		cfg.Workflows[name] = wc
	}

	if cfg.Comfy.DefaultWorkflow != "" {
		if _, ok := cfg.Workflows[cfg.Comfy.DefaultWorkflow]; !ok {
			return cfg, fmt.Errorf("comfy.default_workflow %q is not defined in [workflow]", cfg.Comfy.DefaultWorkflow)
		}
	}

	return cfg, nil
}

func defaultString(val, def string) string {
	if val == "" {
		return def
	}
	return val
}

func resolvePath(baseDir, path string) string {
	if path == "" {
		return ""
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(baseDir, path)
}

func reloadConfigFromFile(configFile string, current Config) (Config, []string, error) {
	newCfg, err := loadConfig(configFile)
	if err != nil {
		return current, nil, err
	}

	newCfg.Database.Resolved = current.Database.Resolved

	warnings := compareNonReloadable(current, newCfg)

	newCfg.Server = current.Server
	newCfg.Database = current.Database
	newCfg.Queue.MaxWorkers = current.Queue.MaxWorkers
	newCfg.Queue.MaxDepth = current.Queue.MaxDepth
	newCfg.Auth = current.Auth

	return newCfg, warnings, nil
}

func compareNonReloadable(current, newCfg Config) []string {
	var warnings []string

	if current.Server.Name != newCfg.Server.Name {
		warnings = append(warnings, fmt.Sprintf("server.name changed from %q to %q: requires restart", current.Server.Name, newCfg.Server.Name))
	}
	if current.Server.Version != newCfg.Server.Version {
		warnings = append(warnings, fmt.Sprintf("server.version changed from %q to %q: requires restart", current.Server.Version, newCfg.Server.Version))
	}
	if current.Server.Addr != newCfg.Server.Addr {
		warnings = append(warnings, fmt.Sprintf("server.addr changed from %q to %q: requires restart", current.Server.Addr, newCfg.Server.Addr))
	}
	if current.Database.Path != newCfg.Database.Path {
		warnings = append(warnings, fmt.Sprintf("database.path changed from %q to %q: requires restart", current.Database.Path, newCfg.Database.Path))
	}
	if current.Queue.MaxWorkers != newCfg.Queue.MaxWorkers {
		warnings = append(warnings, fmt.Sprintf("queue.max_workers changed from %d to %d: requires restart", current.Queue.MaxWorkers, newCfg.Queue.MaxWorkers))
	}
	if current.Queue.MaxDepth != newCfg.Queue.MaxDepth {
		warnings = append(warnings, fmt.Sprintf("queue.max_depth changed from %d to %d: requires restart", current.Queue.MaxDepth, newCfg.Queue.MaxDepth))
	}
	if current.Auth.APIKey != newCfg.Auth.APIKey {
		warnings = append(warnings, fmt.Sprintf("auth.api_key changed: requires restart"))
	}

	return warnings
}
