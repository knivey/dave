package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config mirrors the reference block in example.toml (repo convention).
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Database DatabaseConfig `toml:"database"`
}

type ServerConfig struct {
	Addr    string `toml:"addr"`
	BaseURL string `toml:"base_url"`
}

type DatabaseConfig struct {
	Driver string `toml:"driver"` // "sqlite" | "postgres"
	Path   string `toml:"path"`   // sqlite
	DSN    string `toml:"dsn"`    // postgres
}

func (c *Config) setDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8060"
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.Path == "" {
		c.Database.Path = "data/dave.db"
	}
}

func loadConfig(path string) (*Config, error) {
	var cfg Config
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", path, err)
		}
	}
	cfg.setDefaults()
	return &cfg, nil
}

// resolvePath resolves a config-relative path against the binary dir
// (imgsite/main.go precedent: config and data live next to the binary).
func resolvePath(binaryDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(binaryDir, p)
}
