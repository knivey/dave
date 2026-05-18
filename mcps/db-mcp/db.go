package main

import (
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

type Note struct {
	ID        int64  `db:"id"`
	Network   string `db:"network"`
	Channel   string `db:"channel"`
	UserID    int64  `db:"user_id"`
	Nick      string `db:"nick"`
	Key       string `db:"key"`
	Value     string `db:"value"`
	CreatedAt string `db:"created_at"`
	UpdatedAt string `db:"updated_at"`
}

type KeyCount struct {
	Key   string `db:"key"`
	Count int    `db:"count"`
}

func initDB(dbPath string) (*sqlx.DB, error) {
	dir := filepath.Dir(dbPath)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("creating database directory %s: %w", dir, err)
		}
	}

	sqldb, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", dbPath, err)
	}

	sqldb.SetMaxOpenConns(1)

	if err := goose.SetDialect("sqlite3"); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("setting goose dialect: %w", err)
	}

	goose.SetBaseFS(embedMigrations)

	if err := goose.Up(sqldb, "migrations"); err != nil {
		sqldb.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	db := sqlx.NewDb(sqldb, "sqlite")

	if logger != nil {
		logger.Info("Database initialized", "path", dbPath)
	}
	return db, nil
}

func closeDB(db *sqlx.DB) {
	if db != nil {
		db.Close()
	}
}

func dbInsertNote(db *sqlx.DB, network, channel string, userID int64, nick, key, value string, maxValueSize int) (*Note, error) {
	if maxValueSize > 0 && len(value) > maxValueSize {
		value = value[:maxValueSize] + "[truncated]"
	}

	now := time.Now().UTC().Format("2006-01-02 15:04:05")

	result, err := db.Exec(
		`INSERT INTO notes (network, channel, user_id, nick, key, value, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		network, channel, userID, nick, key, value, now, now,
	)
	if err != nil {
		return nil, err
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}

	return &Note{
		ID:        id,
		Network:   network,
		Channel:   channel,
		UserID:    userID,
		Nick:      nick,
		Key:       key,
		Value:     value,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

func dbPruneUserNotes(db *sqlx.DB, network string, userID int64, maxNotes int) (int64, error) {
	result, err := db.Exec(
		`DELETE FROM notes WHERE network = ? AND user_id = ? AND id NOT IN (
			SELECT id FROM notes WHERE network = ? AND user_id = ? ORDER BY created_at DESC LIMIT ?
		)`,
		network, userID, network, userID, maxNotes,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func dbGetNotesByKey(db *sqlx.DB, network, channel, key, filterNick string) ([]Note, error) {
	var notes []Note
	if filterNick != "" {
		err := db.Select(&notes,
			`SELECT * FROM notes WHERE network = ? AND channel = ? AND key = ? AND nick = ? ORDER BY created_at DESC`,
			network, channel, key, filterNick,
		)
		return notes, err
	}
	err := db.Select(&notes,
		`SELECT * FROM notes WHERE network = ? AND channel = ? AND key = ? ORDER BY created_at DESC`,
		network, channel, key,
	)
	return notes, err
}

func dbSearchNotes(db *sqlx.DB, network, channel, query, filterKey, filterNick, within string, limit int) ([]Note, error) {
	if limit <= 0 {
		limit = 20
	}

	var args []interface{}
	args = append(args, network, channel, query)

	sqlQuery := `SELECT n.* FROM notes n JOIN notes_fts f ON n.id = f.rowid
		WHERE n.network = ? AND n.channel = ? AND notes_fts MATCH ?`

	if filterKey != "" {
		sqlQuery += ` AND n.key = ?`
		args = append(args, filterKey)
	}
	if filterNick != "" {
		sqlQuery += ` AND n.nick = ?`
		args = append(args, filterNick)
	}
	if within != "" {
		dur, err := parseDuration(within)
		if err == nil {
			secs := int(dur.Seconds())
			sqlQuery += ` AND n.created_at > datetime('now', ? || ' seconds')`
			args = append(args, fmt.Sprintf("-%d", secs))
		}
	}

	sqlQuery += ` ORDER BY f.rank LIMIT ?`
	args = append(args, limit)

	var notes []Note
	err := db.Select(&notes, sqlQuery, args...)
	return notes, err
}

func dbRecentNotes(db *sqlx.DB, network, channel, within, filterKey, filterNick string, limit int) ([]Note, error) {
	if limit <= 0 {
		limit = 20
	}

	var args []interface{}
	args = append(args, network, channel)

	sqlQuery := `SELECT * FROM notes WHERE network = ? AND channel = ?`

	if within != "" {
		dur, err := parseDuration(within)
		if err == nil {
			secs := int(dur.Seconds())
			sqlQuery += ` AND created_at > datetime('now', ? || ' seconds')`
			args = append(args, fmt.Sprintf("-%d", secs))
		}
	}

	if filterKey != "" {
		sqlQuery += ` AND key = ?`
		args = append(args, filterKey)
	}
	if filterNick != "" {
		sqlQuery += ` AND nick = ?`
		args = append(args, filterNick)
	}

	sqlQuery += ` ORDER BY created_at DESC LIMIT ?`
	args = append(args, limit)

	var notes []Note
	err := db.Select(&notes, sqlQuery, args...)
	return notes, err
}

func dbDeleteNote(db *sqlx.DB, id int64, network, channel string, userID int64) (bool, error) {
	result, err := db.Exec(
		`DELETE FROM notes WHERE id = ? AND network = ? AND channel = ? AND user_id = ?`,
		id, network, channel, userID,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

func dbDeleteNotesByKey(db *sqlx.DB, network, channel string, userID int64, key string) (int64, error) {
	result, err := db.Exec(
		`DELETE FROM notes WHERE network = ? AND channel = ? AND user_id = ? AND key = ?`,
		network, channel, userID, key,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func dbListKeys(db *sqlx.DB, network, channel, filterNick string, limit int) ([]KeyCount, error) {
	if limit <= 0 {
		limit = 50
	}

	var keys []KeyCount
	if filterNick != "" {
		err := db.Select(&keys,
			`SELECT key, COUNT(*) as count FROM notes WHERE network = ? AND channel = ? AND nick = ? GROUP BY key ORDER BY count DESC LIMIT ?`,
			network, channel, filterNick, limit,
		)
		return keys, err
	}
	err := db.Select(&keys,
		`SELECT key, COUNT(*) as count FROM notes WHERE network = ? AND channel = ? GROUP BY key ORDER BY count DESC LIMIT ?`,
		network, channel, limit,
	)
	return keys, err
}

func dbCountNotes(db *sqlx.DB, network, channel, filterKey, filterNick, within string) (int, error) {
	var args []interface{}
	args = append(args, network, channel)

	sqlQuery := `SELECT COUNT(*) FROM notes WHERE network = ? AND channel = ?`

	if filterKey != "" {
		sqlQuery += ` AND key = ?`
		args = append(args, filterKey)
	}
	if filterNick != "" {
		sqlQuery += ` AND nick = ?`
		args = append(args, filterNick)
	}
	if within != "" {
		dur, err := parseDuration(within)
		if err == nil {
			secs := int(dur.Seconds())
			sqlQuery += ` AND created_at > datetime('now', ? || ' seconds')`
			args = append(args, fmt.Sprintf("-%d", secs))
		}
	}

	var count int
	err := db.Get(&count, sqlQuery, args...)
	return count, err
}

func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}

	if strings.HasSuffix(s, "d") {
		daysStr := strings.TrimSuffix(s, "d")
		var days int
		if _, err := fmt.Sscanf(daysStr, "%d", &days); err != nil {
			return 0, fmt.Errorf("invalid duration: %s", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}

	return time.ParseDuration(s)
}
