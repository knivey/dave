package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
