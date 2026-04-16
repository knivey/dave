package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	httpMode := flag.Bool("http", false, "use HTTP transport instead of stdio")
	flag.Parse()

	configDir := "mcp-config"
	if args := flag.Args(); len(args) > 0 {
		configDir = args[0]
	}

	cfg, err := loadConfig(configDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	queue := NewJobQueue(cfg)
	defer queue.Stop()

	handlers := NewToolHandlers(cfg, queue)
	server := createServer(cfg, handlers)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	if *httpMode {
		serveHTTP(ctx, cfg, server)
	} else {
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

func serveHTTP(ctx context.Context, cfg Config, server *mcp.Server) {
	handler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		return server
	}, &mcp.StreamableHTTPOptions{JSONResponse: true})

	httpServer := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: handler,
	}

	go func() {
		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	log.Printf("dave-mcp HTTP server listening on %s", cfg.Server.Addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
		os.Exit(1)
	}
}
