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

	if *httpMode && cfg.Auth.APIKey == "" {
		fmt.Fprintf(os.Stderr, "config error: auth.api_key is required in HTTP mode\n")
		os.Exit(1)
	}

	handlers := NewToolHandlers(cfg)

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

	if *httpMode {
		serveHTTP(ctx, cfg, handlers, configPath)
	} else {
		server := createServer(cfg, handlers)
		serveStdio(ctx, server)
	}
}

func serveHTTP(ctx context.Context, cfg Config, handlers *ToolHandlers, configPath string) {
	server := createServer(cfg, handlers)

	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	mux := http.NewServeMux()
	mux.Handle("/", handler)

	mux.HandleFunc("POST /admin/reload", func(w http.ResponseWriter, r *http.Request) {
		newCfg, err := loadConfig(configPath)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		handlers.setConfig(newCfg)
		logger.Info("config reloaded via admin endpoint")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	var h http.Handler = mux

	if cfg.Auth.APIKey != "" {
		h = apiKeyMiddleware(cfg.Auth.APIKey)(mux)
	}

	httpServer := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: h,
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

func createServer(cfg Config, handlers *ToolHandlers) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    cfg.Server.Name,
			Version: cfg.Server.Version,
		},
		&mcp.ServerOptions{
			Instructions: "MCP server for fetching YouTube video transcripts and metadata. Only supports YouTube URLs. Use get_transcript to fetch the spoken text of a video, or get_video_info to get metadata like title, channel, and duration.",
		},
	)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_transcript",
		Description: "Fetch the transcript (spoken text) from a YouTube video. Returns plain text of what is said in the video. Only works with YouTube URLs (youtube.com or youtu.be). Uses auto-generated captions by default.",
	}, handlers.handleGetTranscript)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_video_info",
		Description: "Fetch metadata for a YouTube video: title, channel name, duration, description, upload date, and view count. Only works with YouTube URLs (youtube.com or youtu.be). Useful to get context about a video before or alongside its transcript.",
	}, handlers.handleGetVideoInfo)

	return server
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

const apiKeyHeader = "X-API-Key"

func apiKeyMiddleware(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(apiKeyHeader) != key {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
