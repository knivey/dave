package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	flag.Parse()

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting executable path: %v\n", err)
		os.Exit(1)
	}
	exeDir := filepath.Dir(exePath)

	initLogger(exeDir)
	defer closeLogger()

	configPath := filepath.Join(exeDir, "config.toml")
	if args := flag.Args(); len(args) > 0 {
		configPath = args[0]
		if !filepath.IsAbs(configPath) {
			configPath = filepath.Join(exeDir, configPath)
		}
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	db, err := initDB(cfg.Database.Path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database error: %v\n", err)
		os.Exit(1)
	}
	defer closeDB(db)

	handlers := NewToolHandlers(cfg, db)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdownCh := make(chan os.Signal, 1)
	reloadCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGINT, syscall.SIGTERM)
	signal.Notify(reloadCh, syscall.SIGHUP)

	go func() {
		<-shutdownCh
		cancel()
	}()

	go func() {
		for range reloadCh {
			newCfg, err := loadConfig(configPath)
			if err != nil {
				logger.Error("config reload failed", "error", err)
				continue
			}
			handlers.setConfig(newCfg)
			logger.Info("config reloaded")
		}
	}()

	server := createServer(cfg, handlers)

	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "stdio server error: %v\n", err)
		os.Exit(1)
	}
}

func createServer(cfg Config, handlers *ToolHandlers) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    cfg.Server.Name,
			Version: cfg.Server.Version,
		},
		&mcp.ServerOptions{
			Instructions: "Persistent notebook for storing and retrieving freeform notes scoped per IRC channel. Notes are organized by keys (tags) — multiple notes can share the same key. Use put_note to store a note, get_notes to retrieve by key, search_notes for full-text search, recent_notes for time-based browsing, list_keys to see available categories, and count_notes for quick stats.",
		},
	)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "put_note",
		Description: "Store a new note in the channel notebook. Multiple notes can share the same key — each call creates a new entry. Returns the note ID and timestamp.",
	}, handlers.handlePutNote)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_notes",
		Description: "Get all notes matching a key in the current channel. Optionally filter by nick to see notes from a specific user.",
	}, handlers.handleGetNotes)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_notes",
		Description: "Full-text search across note values in the current channel. Supports filtering by key, nick, and time range. Returns ranked results.",
	}, handlers.handleSearchNotes)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "recent_notes",
		Description: "Get recent notes from the current channel within a time range. Use relative offsets like '1h', '24h', '7d', '30d'. Optionally filter by key or nick.",
	}, handlers.handleRecentNotes)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "delete_note",
		Description: "Delete a single note by ID. You can only delete your own notes.",
	}, handlers.handleDeleteNote)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "delete_notes",
		Description: "Delete all notes with a given key that belong to you. Returns the count of deleted notes.",
	}, handlers.handleDeleteNotes)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_keys",
		Description: "List distinct keys in the current channel with note counts. Shows what categories exist and how many notes each has. Optionally filter by nick.",
	}, handlers.handleListKeys)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "count_notes",
		Description: "Count notes in the current channel matching optional filters (key, nick, time range). Useful for quick stats.",
	}, handlers.handleCountNotes)

	return server
}
