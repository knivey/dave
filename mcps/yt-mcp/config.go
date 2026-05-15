package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server ServerConfig `toml:"server"`
	Ytdlp  YtdlpConfig  `toml:"ytdlp"`
}

type ServerConfig struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
}

type YtdlpConfig struct {
	Path       string        `toml:"path"`
	Timeout    time.Duration `toml:"timeout"`
	Languages  []string      `toml:"languages"`
	PreferAuto bool          `toml:"prefer_auto"`
	MaxLength  int           `toml:"max_length"`
	TempDir    string        `toml:"temp_dir"`
}

func loadConfig(configFile string) (Config, error) {
	var cfg Config

	if _, err := os.Stat(configFile); err == nil {
		if _, err := toml.DecodeFile(configFile, &cfg); err != nil {
			return cfg, fmt.Errorf("loading %s: %w", configFile, err)
		}
	}

	cfg.Server.Name = defaultString(cfg.Server.Name, "yt-mcp")
	cfg.Server.Version = defaultString(cfg.Server.Version, "0.1.0")

	cfg.Ytdlp.Path = defaultString(cfg.Ytdlp.Path, "yt-dlp")
	if cfg.Ytdlp.Timeout == 0 {
		cfg.Ytdlp.Timeout = 2 * time.Minute
	}
	if len(cfg.Ytdlp.Languages) == 0 {
		cfg.Ytdlp.Languages = []string{"en"}
	}
	if cfg.Ytdlp.MaxLength == 0 {
		cfg.Ytdlp.MaxLength = 50000
	}
	cfg.Ytdlp.TempDir = defaultString(cfg.Ytdlp.TempDir, os.TempDir())

	if _, err := exec.LookPath(cfg.Ytdlp.Path); err != nil {
		return cfg, fmt.Errorf("ytdlp.path %q not found on PATH: %w", cfg.Ytdlp.Path, err)
	}

	return cfg, nil
}

func defaultString(val, def string) string {
	if val == "" {
		return def
	}
	return val
}
