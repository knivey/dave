package main

import (
	"context"
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

	cfg, err := loadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	handlers, err := NewToolHandlers(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

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
			newCfg, err := loadConfig()
			if err != nil {
				logger.Error("config reload failed", "error", err)
				continue
			}
			if err := handlers.setConfig(newCfg); err != nil {
				logger.Error("config reload failed", "error", err)
				continue
			}
			logger.Info("config reloaded")
		}
	}()

	if *httpMode {
		serveHTTP(ctx, cfg, handlers)
	} else {
		server := createServer(handlers)
		serveStdio(ctx, server)
	}
}

func serveHTTP(ctx context.Context, cfg Config, handlers *ToolHandlers) {
	server := createServer(handlers)

	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	mux := http.NewServeMux()
	mux.Handle("/", handler)

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

func createServer(handlers *ToolHandlers) *mcp.Server {
	server := mcp.NewServer(
		&mcp.Implementation{
			Name:    "fetch-mcp",
			Version: "0.1.0",
		},
		&mcp.ServerOptions{
			Instructions: "MCP server for fetching web pages and converting them to markdown. Use fetch to retrieve a URL and get back clean markdown content suitable for LLM consumption. Supports pagination for large pages.",
		},
	)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "fetch",
		Description: "Fetch a URL and convert its content to markdown. Returns the page content as clean markdown text. For large pages, use start_index to paginate through the content. Non-HTML pages (JSON, plain text, etc.) are returned as-is.",
	}, handlers.handleFetch)

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
