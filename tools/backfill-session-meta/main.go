package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	_ "modernc.org/sqlite"
)

type chatConfig struct {
	Service string `toml:"service"`
	Model   string `toml:"model"`
}

type dbConfig struct {
	Path string `toml:"path"`
}

type configFile struct {
	Database dbConfig `toml:"database"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: backfill-session-meta <config-dir>")
		fmt.Fprintln(os.Stderr, "")
		fmt.Fprintln(os.Stderr, "Backfills service and model columns in the sessions table")
		fmt.Fprintln(os.Stderr, "by looking up each session's chat_command in chats.toml.")
		fmt.Fprintln(os.Stderr, "Sessions whose chat_command is not in the current config are skipped.")
		os.Exit(1)
	}

	configDir := os.Args[1]

	var cfg configFile
	mainFile := filepath.Join(configDir, "config.toml")
	if _, err := toml.DecodeFile(mainFile, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "error loading %s: %v\n", mainFile, err)
		os.Exit(1)
	}
	if cfg.Database.Path == "" {
		cfg.Database.Path = "data/dave.db"
	}

	chatsFile := filepath.Join(configDir, "chats.toml")
	chats := make(map[string]chatConfig)
	if _, err := toml.DecodeFile(chatsFile, &chats); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "error loading %s: %v\n", chatsFile, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "no chats.toml found, nothing to backfill\n")
		os.Exit(0)
	}

	fmt.Printf("loaded %d chat commands from %s\n", len(chats), chatsFile)

	db, err := sql.Open("sqlite", cfg.Database.Path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error opening database %s: %v\n", cfg.Database.Path, err)
		os.Exit(1)
	}
	defer db.Close()

	rows, err := db.Query("SELECT id, chat_command FROM sessions WHERE service = '' OR model = ''")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error querying sessions: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()

	type update struct {
		id      int64
		service string
		model   string
		command string
	}
	var updates []update
	var skipped []string

	for rows.Next() {
		var id int64
		var command string
		if err := rows.Scan(&id, &command); err != nil {
			fmt.Fprintf(os.Stderr, "error scanning row: %v\n", err)
			os.Exit(1)
		}
		chat, ok := chats[command]
		if !ok {
			skipped = append(skipped, command)
			continue
		}
		updates = append(updates, update{id: id, service: chat.Service, model: chat.Model, command: command})
	}

	fmt.Printf("found %d sessions to update, %d skipped (unknown command)\n", len(updates), len(skipped))
	for _, s := range skipped {
		fmt.Printf("  skipped: %s\n", s)
	}

	if len(updates) == 0 {
		fmt.Println("nothing to do")
		return
	}

	tx, err := db.Begin()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error starting transaction: %v\n", err)
		os.Exit(1)
	}

	stmt, err := tx.Prepare("UPDATE sessions SET service = ?, model = ? WHERE id = ?")
	if err != nil {
		tx.Rollback()
		fmt.Fprintf(os.Stderr, "error preparing statement: %v\n", err)
		os.Exit(1)
	}
	defer stmt.Close()

	updated := 0
	for _, u := range updates {
		if _, err := stmt.Exec(u.service, u.model, u.id); err != nil {
			fmt.Fprintf(os.Stderr, "error updating session %d (%s): %v\n", u.id, u.command, err)
			continue
		}
		updated++
	}

	if err := tx.Commit(); err != nil {
		fmt.Fprintf(os.Stderr, "error committing: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("updated %d/%d sessions\n", updated, len(updates))
}
