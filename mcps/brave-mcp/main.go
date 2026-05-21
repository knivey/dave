package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Config struct {
	APIKey         string
	BaseURL        string
	Timeout        time.Duration
	DefaultCountry string
	DefaultLang    string
	EnabledTools   map[string]bool
}

func loadConfig() Config {
	apiKey := os.Getenv("BRAVE_API_KEY")
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "BRAVE_API_KEY environment variable is required\n")
		os.Exit(1)
	}

	cfg := Config{
		APIKey:         apiKey,
		BaseURL:        envOrDefault("BRAVE_BASE_URL", "https://api.search.brave.com"),
		Timeout:        envDuration("BRAVE_TIMEOUT", 30*time.Second),
		DefaultCountry: envOrDefault("BRAVE_DEFAULT_COUNTRY", "US"),
		DefaultLang:    envOrDefault("BRAVE_DEFAULT_LANG", "en"),
	}

	enabledStr := os.Getenv("BRAVE_ENABLED_TOOLS")
	cfg.EnabledTools = parseEnabledTools(enabledStr)

	return cfg
}

func parseEnabledTools(s string) map[string]bool {
	if s == "" {
		return nil
	}
	m := make(map[string]bool)
	for _, name := range strings.Split(s, ",") {
		trimmed := strings.TrimSpace(name)
		if trimmed != "" {
			if !strings.HasPrefix(trimmed, "brave_") {
				trimmed = "brave_" + trimmed
			}
			m[trimmed] = true
		}
	}
	return m
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid duration for %s: %v\n", key, err)
		os.Exit(1)
	}
	return d
}

func main() {
	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting executable path: %v\n", err)
		os.Exit(1)
	}
	exeDir := filepath.Dir(exePath)

	initLogger(exeDir)
	defer closeLogger()

	cfg := loadConfig()

	client := newBraveClient(cfg.APIKey, cfg.BaseURL, cfg.Timeout, cfg.DefaultCountry, cfg.DefaultLang)
	handlers := NewToolHandlers(client)

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "brave-mcp",
			Version: "0.1.0",
		},
		&mcp.ServerOptions{
			Instructions: "MCP server for Brave Search. Provides web, image, video, news, local, answers, LLM context, and place search tools.",
		},
	)

	registerTools(server, handlers, cfg.EnabledTools)

	enabledCount := len(allToolNames)
	if cfg.EnabledTools != nil {
		enabledCount = len(cfg.EnabledTools)
	}
	logger.Info("starting brave-mcp", "tools", enabledCount, "country", cfg.DefaultCountry, "lang", cfg.DefaultLang)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-shutdownCh
		logger.Info("shutting down")
		cancel()
	}()

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "stdio server error: %v\n", err)
		os.Exit(1)
	}
}
