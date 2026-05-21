package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Server      ServerConfig
	Fetch       FetchConfig
	Cache       CacheConfig
	Readability ReadabilityConfig
}

type ServerConfig struct {
	Name    string
	Version string
	Addr    string
}

type FetchConfig struct {
	UserAgent    string
	Timeout      time.Duration
	MaxRedirects int
	ProxyURL     string
}

type CacheConfig struct {
	DSN            string
	MaxMarkdownAge time.Duration
}

type ReadabilityConfig struct {
	Disabled bool
}

func loadConfig() (Config, error) {
	var cfg Config

	cfg.Server.Name = envOr("FETCH_SERVER_NAME", "fetch-mcp")
	cfg.Server.Version = envOr("FETCH_SERVER_VERSION", "0.1.0")
	cfg.Server.Addr = envOr("FETCH_SERVER_ADDR", ":8080")

	cfg.Fetch.UserAgent = envOr("FETCH_USER_AGENT", "fetch-mcp/0.1.0")
	cfg.Fetch.Timeout = envDuration("FETCH_TIMEOUT", 30*time.Second)
	cfg.Fetch.MaxRedirects = envInt("FETCH_MAX_REDIRECTS", 10)
	cfg.Fetch.ProxyURL = os.Getenv("FETCH_PROXY_URL")

	if cfg.Fetch.ProxyURL != "" {
		if _, err := url.Parse(cfg.Fetch.ProxyURL); err != nil {
			return cfg, fmt.Errorf("invalid FETCH_PROXY_URL %q: %w", cfg.Fetch.ProxyURL, err)
		}
	}

	cfg.Cache.DSN = envOr("FETCH_CACHE_DSN", "memcache://")
	cfg.Cache.MaxMarkdownAge = envDuration("FETCH_CACHE_MARKDOWN_TTL", 15*time.Minute)

	cfg.Readability.Disabled = envBool("FETCH_READABILITY_DISABLED")

	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "true" || v == "1" || v == "yes"
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
