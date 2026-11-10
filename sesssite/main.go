package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"gorm.io/gorm"
)

func main() {
	cfgPath := flag.String("config", "config.toml", "TOML config (default: config.toml next to the binary)")
	flag.Parse()

	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "executable path: %v\n", err)
		os.Exit(1)
	}
	exeDir := filepath.Dir(exe)
	cfg, err := loadConfig(resolvePath(exeDir, *cfgPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	cfg.Database.Path = resolvePath(exeDir, cfg.Database.Path)

	db, err := openDB(cfg.Database)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		os.Exit(1)
	}

	srv := NewServer(db, cfg)
	log.Printf("sesssite listening on %s", cfg.Server.Addr)
	if err := srv.Run(cfg.Server.Addr); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// Task 8 replaces this stub wholesale with server.go. It exists only so
// the package builds and main() has its two seams (db + config) wired.
type dbType = *gorm.DB

type server struct {
	db  dbType
	cfg *Config
}

func NewServer(db *gorm.DB, cfg *Config) *server { return &server{db: db, cfg: cfg} }

func (s *server) Run(addr string) error {
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })
	return http.ListenAndServe(addr, nil)
}
