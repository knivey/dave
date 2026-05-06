package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type reloadResponse struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings,omitempty"`
	Message  string   `json:"message,omitempty"`
}

func main() {
	httpMode := flag.Bool("http", false, "use HTTP transport instead of stdio")
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

	dbPath := cfg.Database.Path
	if dbPath == "" {
		dbPath = "data/img-mcp.db"
	}
	if !filepath.IsAbs(dbPath) {
		dbPath = filepath.Join(exeDir, dbPath)
	}
	cfg.Database.Resolved = dbPath

	db, err := initDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database error: %v\n", err)
		os.Exit(1)
	}
	defer closeDB(db)

	queue := NewJobQueue(cfg, db)
	defer queue.Stop()

	handlers := NewToolHandlers(cfg, queue)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	shutdownCh := make(chan os.Signal, 1)
	reloadCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, syscall.SIGINT, syscall.SIGTERM)
	signal.Notify(reloadCh, syscall.SIGHUP)

	go func() {
		<-shutdownCh
		queue.Stop()
		cancel()
	}()

	go func() {
		for range reloadCh {
			resp := doReload(configPath, handlers, queue)
			if resp.Status == "error" {
				logger.Error("config reload failed", "error", resp.Message)
			} else {
				logger.Info("config reloaded")
				for _, w := range resp.Warnings {
					logger.Warn("non-reloadable field changed", "warning", w)
				}
			}
		}
	}()
	if *httpMode {
		serveHTTP(ctx, cfg, handlers, queue, configPath)
	} else {
		server := createFullServer(cfg, handlers)
		serveStdio(ctx, server)
	}
}

func doReload(configPath string, handlers *ToolHandlers, queue *JobQueue) reloadResponse {
	newCfg, warnings, err := reloadConfigFromFile(configPath, handlers.getConfig())
	if err != nil {
		return reloadResponse{Status: "error", Message: err.Error()}
	}
	handlers.setConfig(newCfg)
	queue.setConfig(newCfg)
	resp := reloadResponse{Status: "ok"}
	if len(warnings) > 0 {
		resp.Warnings = warnings
	}
	return resp
}

func serveStdio(ctx context.Context, server *mcp.Server) {
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		if ctx.Err() != nil {
			return
		}
		fmt.Fprintf(os.Stderr, "stdio server error: %v\n", err)
		os.Exit(1)
	}
}

func serveHTTP(ctx context.Context, cfg Config, handlers *ToolHandlers, queue *JobQueue, configPath string) {
	mux := buildHTTPHandler(cfg, handlers, queue, configPath)

	httpServer := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	logger.Info("HTTP server listening", "addr", cfg.Server.Addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
		os.Exit(1)
	}
}

func buildHTTPHandler(cfg Config, handlers *ToolHandlers, queue *JobQueue, configPath string) http.Handler {
	syncServer := createSyncServer(cfg, handlers)
	asyncServer := createAsyncServer(cfg, handlers)

	syncHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return syncServer
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	asyncHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return asyncServer
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	mux := http.NewServeMux()
	mux.Handle("/sync", syncHandler)
	mux.Handle("/async", asyncHandler)

	mux.HandleFunc("POST /admin/reload", func(w http.ResponseWriter, r *http.Request) {
		resp := doReload(configPath, handlers, queue)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	return mux
}
