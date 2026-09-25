package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// Import-mode flags. -import DIR switches the binary from serve mode to
// a one-shot legacy-output import (see import.go); -tz declares the
// timezone import filename timestamps were written in.
var (
	importDirFlag = flag.String("import", "", "import legacy ComfyUI outputs from DIR (recursive webp/PNG walk), then exit")
	importTZFlag  = flag.String("tz", "", "timezone of -import filename timestamps (IANA name, e.g. America/New_York);\ndefault: this machine's local zone")
	deleteIDsFlag = flag.String("delete", "", "hide gallery images by public id, comma-separated (soft delete, files retained),\nthen exit")
)

// usage documents all modes; the flag package prints it on bad flag
// usage, import/delete/serve-mode misuse prints it explicitly.
func usage() {
	prog := filepath.Base(os.Args[0])
	fmt.Fprintf(os.Stderr, `%[1]s — image gallery for dave's generations

usage:
  %[1]s [flags] [config]

Runs the gallery server. config is a TOML file (default: config.toml next
to the binary; relative paths resolve against the binary directory).

import mode:
  %[1]s -import DIR [-tz ZONE] [config]

Walks DIR recursively and imports ComfyUI webp/PNG outputs carrying an
embedded workflow into the gallery database and store. Non-destructive
(originals are copied, never moved or deleted) and idempotent (hashes
already known to the DB are skipped). Filename timestamps (leading
%%Y-%%m-%%d-%%H%%M%%S) are interpreted in ZONE (default: the local zone).
Run with the server stopped. See docs/image-site.md, "Importing legacy
outputs".

delete mode:
  %[1]s -delete ID[,ID…] [config]

Hides one or more gallery images by public id (comma-separated). Soft
delete through the exact same DB path as the HTTP DELETE endpoint: rows
are marked hidden=1, files are retained, galleries/search exclude the
row. No confirmation prompt — it is reversible:
  UPDATE images SET hidden=0 WHERE id='<id>'
Offline like -import: no live-update events are published, so run with
the server stopped (or accept that connected pages keep the card until
their next load). Unknown ids and already-hidden ids are per-id report
lines that do not stop the rest, but make the exit status 1 (the 404/
410 equivalents); usage mistakes (empty or malformed id list, -delete
combined with -import) exit 2; 0 means every id was hidden by this run.

flags:
`, prog)
	flag.PrintDefaults()
}

func main() {
	flag.Usage = usage
	flag.Parse()

	exePath, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error getting executable path: %v\n", err)
		os.Exit(1)
	}
	exeDir := filepath.Dir(exePath)

	initLogger(exeDir)
	defer closeLogger()

	// Config path: CLI arg if given, else config.toml next to the binary
	// (same resolution as img-mcp).
	configPath := filepath.Join(exeDir, "config.toml")
	if args := flag.Args(); len(args) > 0 {
		configPath = args[0]
		if !filepath.IsAbs(configPath) {
			configPath = filepath.Join(exeDir, configPath)
		}
	}

	// Offline-mode presence (-import / -delete) is detected with
	// flag.Visit, not plain != "" tests: an explicitly empty
	// `-import ""` or `-delete ""` is an argument mistake that must
	// reach the usage errors below, not silently fall into serve mode
	// the way an unset flag does.
	importSet := false
	deleteSet := false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "import":
			importSet = true
		case "delete":
			deleteSet = true
		}
	})

	// -import and -delete are both one-shot offline modes over the same
	// DB; combining them is a usage error, checked BEFORE either branch
	// so neither mode runs.
	if importSet && deleteSet {
		fmt.Fprintf(os.Stderr, "error: -import and -delete are mutually exclusive\n\n")
		flag.Usage()
		os.Exit(2)
	}

	// Import mode: -import DIR runs the legacy-output import and exits.
	// os.Exit skips main's defers, so closeLogger runs explicitly here.
	if importSet {
		if *importDirFlag == "" {
			fmt.Fprintf(os.Stderr, "error: -import needs a directory to import from\n\n")
			flag.Usage()
			os.Exit(2)
		}
		code := importMain(exeDir, configPath, *importDirFlag, *importTZFlag)
		closeLogger()
		os.Exit(code)
	}
	if *importTZFlag != "" {
		fmt.Fprintf(os.Stderr, "error: -tz is only meaningful together with -import DIR\n\n")
		flag.Usage()
		os.Exit(2)
	}

	// Delete mode: -delete ID[,ID…] soft-hides images and exits. Bad
	// arguments are usage errors (exit 2); per-id misses are run-level
	// outcomes reported by deleteMain (exit 1). Same explicit
	// closeLogger as import mode.
	if deleteSet {
		ids, err := parseDeleteIDs(*deleteIDsFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n\n", err)
			flag.Usage()
			os.Exit(2)
		}
		code := deleteMain(exeDir, configPath, ids)
		closeLogger()
		os.Exit(code)
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	// Resolve data paths against the binary directory and freeze the
	// resolved values on the config; reloads preserve them untouched.
	dbPath := resolvePath(exeDir, cfg.Database.Path)
	cfg.Database.Resolved = dbPath
	cfg.Storage.ResolvedPath = resolvePath(exeDir, cfg.Storage.Path)
	cfg.Storage.ResolvedThumbsPath = resolvePath(exeDir, cfg.Storage.ThumbsPath)

	db, err := initDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database error: %v\n", err)
		os.Exit(1)
	}
	defer closeDB(db)

	app := NewApp(cfg, db, configPath)

	// SSE hub for /events live updates. Attached once, before serving;
	// serveHTTP's ctx drives its shutdown so streams release during
	// graceful stop. The publish helpers are nil-safe, so this ordering
	// is the only wiring the upload/thumb paths need.
	app.setEventHub(newSSEHub())

	// Background thumbnailer: fixed pool at startup, pending re-scan for
	// restart resilience. Stopped after the HTTP server exits.
	thumbW := newThumbWorker(app.getConfig, app.db, app.store)
	// thumb-ready SSE fires from the worker through this seam, after
	// the DB flip to ready (ordering pinned in thumbWorker.process).
	thumbW.onThumbReady = app.publishThumbReady
	thumbW.Start(cfg.Thumbnails.Workers)
	app.setThumbWorker(thumbW)
	defer thumbW.Stop()

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
			resp := app.doReload()
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

	serveHTTP(ctx, app)
}

func serveHTTP(ctx context.Context, app *App) {
	cfg := app.getConfig()
	httpServer := &http.Server{
		Addr:    cfg.Server.Addr,
		Handler: app.buildHandler(),
		// DESIGN NOTE (timeouts): ReadHeaderTimeout bounds
		// slowloris-style header dribble without penalizing legit
		// slow bodies; IdleTimeout reaps idle keep-alive connections
		// now that long-lived streams exist to keep worker counts
		// meaningful. WriteTimeout is deliberately LEFT ZERO: it
		// would kill /events — the SSE stream intentionally outlives
		// any sane write budget, and every write on it would have to
		// complete inside the deadline. Unbounded write time is safe
		// here because response sizes are bounded elsewhere: uploads
		// read through http.MaxBytesReader with a Content-Length
		// pre-check, and file routes serve known lengths via
		// http.ServeContent. (ReadTimeout is likewise left unset so
		// idle SSE connections — a valid state between events — are
		// not reaped mid-stream; the heartbeat detects dead peers.)
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		// Shut the hub down BEFORE http.Server.Shutdown: /events
		// handlers are active requests that never complete on their
		// own, and Shutdown waits for active requests — closing the
		// hub's done channel is what releases the streams (handlers
		// return, connections close, no goroutine leaks).
		if app.events != nil {
			app.events.shutdown()
		}
		// Bound the graceful wait: Shutdown with a bare context can
		// block forever on a connection that never completes (the hub
		// shutdown above releases /events, but nothing guarantees every
		// other connection plays nice). A 30s deadline gives listeners
		// ample time; past it, Shutdown returns and any stragglers
		// drop when the process exits.
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				logger.Error("HTTP server shutdown timed out, dropping remaining connections", "timeout", "30s")
			} else {
				logger.Error("HTTP server shutdown failed", "error", err)
			}
		}
	}()

	logger.Info("HTTP server listening", "addr", cfg.Server.Addr)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "HTTP server error: %v\n", err)
		os.Exit(1)
	}
}
