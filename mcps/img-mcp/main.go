package main

import (
	"context"
	"flag"
	"fmt"
	"log"
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

	queue := NewJobQueue(cfg)
	defer queue.Stop()

	handlers := NewToolHandlers(cfg, queue)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	if *httpMode {
		serveHTTP(ctx, cfg, handlers)
	} else {
		server := createFullServer(cfg, handlers)
		serveStdio(ctx, server)
	}
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

func serveHTTP(ctx context.Context, cfg Config, handlers *ToolHandlers) {
	syncServer := createSyncServer(cfg, handlers)
	asyncServer := createAsyncServer(cfg, handlers)

	syncHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return syncServer
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	asyncHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return asyncServer
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	mux := http.NewServeMux()
	mux.Handle(cfg.Server.SyncPath, syncHandler)
	mux.Handle(cfg.Server.AsyncPath, asyncHandler)

	httpServer := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	log.Printf("img-mcp HTTP server listening on %s (sync=%s, async=%s)", cfg.Server.Addr, cfg.Server.SyncPath, cfg.Server.AsyncPath)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
		os.Exit(1)
	}
}
