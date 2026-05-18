package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server   ServerConfig   `toml:"server"`
	Database DatabaseConfig `toml:"database"`
}

type ServerConfig struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
}

type DatabaseConfig struct {
	Path            string `toml:"path"`
	MaxValueSize    int    `toml:"max_value_size"`
	MaxNotesPerUser int    `toml:"max_notes_per_user"`
}

func loadConfig(configFile string) (Config, error) {
	var cfg Config

	configDir := filepath.Dir(configFile)

	if _, err := os.Stat(configFile); err == nil {
		if _, err := toml.DecodeFile(configFile, &cfg); err != nil {
			return cfg, fmt.Errorf("loading %s: %w", configFile, err)
		}
	}

	cfg.Server.Name = defaultString(cfg.Server.Name, "db-mcp")
	cfg.Server.Version = defaultString(cfg.Server.Version, "0.1.0")

	cfg.Database.Path = defaultString(cfg.Database.Path, "data/notes.db")
	cfg.Database.Path = resolvePath(configDir, cfg.Database.Path)
	if cfg.Database.MaxValueSize == 0 {
		cfg.Database.MaxValueSize = 10000
	}
	if cfg.Database.MaxNotesPerUser == 0 {
		cfg.Database.MaxNotesPerUser = 500
	}

	return cfg, nil
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

func defaultString(val, def string) string {
	if val == "" {
		return def
	}
	return val
}
