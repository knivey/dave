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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type reloadResponse struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings,omitempty"`
	Message  string   `json:"message,omitempty"`
}

// liveServer retains a live MCP server together with the closure that
// re-registers its edit tools under a new config — the hot-reload path for
// [safety] edit_policy, which rides the edit tool descriptions and is
// frozen at AddTool time. The SDK serializes the mutations
// (changeAndNotify) and fires notifications/tools/list_changed to
// connected clients; dave applies the new descriptions on its next tool
// re-list or /reload.
type liveServer struct {
	server *mcp.Server
	// refresh re-registers THIS server's edit tools under a new config
	// (RemoveTools both names — idempotent — then AddTool per kind).
	refresh func(cfg Config)
}

// newLiveServer wraps a live server with its refresh closure. carrySync /
// carryAsync select which edit registrations this server flavor carries:
// the stdio full server has both, the HTTP sync server only edit_image,
// the HTTP async server only edit_image_async. RemoveTools is idempotent
// (removing a name the server doesn't carry is a no-op), so every refresh
// removes both names and re-adds only the kinds this flavor carries.
func newLiveServer(server *mcp.Server, handlers *ToolHandlers, carrySync, carryAsync bool) liveServer {
	return liveServer{
		server: server,
		refresh: func(cfg Config) {
			server.RemoveTools("edit_image", "edit_image_async")
			if carrySync {
				registerEditToolSync(server, handlers, cfg)
			}
			if carryAsync {
				registerEditToolAsync(server, handlers, cfg)
			}
		},
	}
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

	if *httpMode && cfg.Auth.APIKey == "" {
		fmt.Fprintf(os.Stderr, "config error: auth.api_key is required in HTTP mode\n")
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

	// Build the server(s) and their live-reload handles BEFORE the reload
	// loop starts, so a SIGHUP arriving during startup still finds them.
	var liveServers []liveServer
	var run func()
	if *httpMode {
		handler, ls := buildHTTPHandler(cfg, handlers, queue, configPath)
		liveServers = ls
		run = func() { serveHTTP(ctx, cfg.Server.Addr, handler) }
	} else {
		server := createFullServer(cfg, handlers)
		liveServers = []liveServer{newLiveServer(server, handlers, true, true)}
		run = func() { serveStdio(ctx, server) }
	}

	go func() {
		for range reloadCh {
			resp := doReload(configPath, handlers, queue, liveServers)
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

	run()
}

func doReload(configPath string, handlers *ToolHandlers, queue *JobQueue, liveServers []liveServer) reloadResponse {
	oldCfg := handlers.getConfig()
	newCfg, warnings, err := reloadConfigFromFile(configPath, oldCfg)
	if err != nil {
		return reloadResponse{Status: "error", Message: err.Error()}
	}
	handlers.setConfig(newCfg)
	queue.setConfig(newCfg)
	// [safety] edit_policy rides the edit tool descriptions, which are
	// frozen at registration — a changed policy re-registers the edit
	// tools on every live server (tools/list_changed fires; dave applies
	// the new text on its next tool re-list or /reload).
	if oldCfg.Safety.EditPolicy != newCfg.Safety.EditPolicy {
		for _, ls := range liveServers {
			ls.refresh(newCfg)
		}
		logger.Info("edit tool descriptions re-registered (edit_policy changed)")
	}
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

func serveHTTP(ctx context.Context, addr string, handler http.Handler) {
	httpServer := &http.Server{
		Addr:    addr,
		Handler: handler,
	}

	go func() {
		<-ctx.Done()
		httpServer.Shutdown(context.Background())
	}()

	logger.Info("HTTP server listening", "addr", addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
		os.Exit(1)
	}
}

// buildHTTPHandler wires the two HTTP MCP servers (sync on /sync, async on
// /async), the admin reload endpoint, and the middleware chain. It also
// returns the liveServer handles so the SIGHUP path in main shares the
// same edit-policy refresh closures as POST /admin/reload.
func buildHTTPHandler(cfg Config, handlers *ToolHandlers, queue *JobQueue, configPath string) (http.Handler, []liveServer) {
	syncServer := createSyncServer(cfg, handlers)
	asyncServer := createAsyncServer(cfg, handlers)
	liveServers := []liveServer{
		newLiveServer(syncServer, handlers, true, false),
		newLiveServer(asyncServer, handlers, false, true),
	}

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
		resp := doReload(configPath, handlers, queue, liveServers)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	var handler http.Handler = mux

	if cfg.Auth.APIKey != "" {
		handler = apiKeyMiddleware(cfg.Auth.APIKey)(handler)
	}
	handler = requestLogMiddleware(handler)

	return handler, liveServers
}

func requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lrw := &loggingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(lrw, r)
		logger.Info("http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", lrw.statusCode,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote_addr", r.RemoteAddr,
		)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *loggingResponseWriter) WriteHeader(code int) {
	w.statusCode = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
