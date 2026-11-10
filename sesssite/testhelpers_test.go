package main

import (
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	logxi "github.com/mgutz/logxi/v1"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}

func ptrInt64(i int64) *int64 { return &i }

// urlQueryTime renders t in the RFC3339Nano form the user page's
// keyset cursor travels as (before_last_active). UTC matches how the
// sqlite driver normalizes stored times, so the round-trip compares
// cleanly against stored values.
func urlQueryTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// newTestDB creates a schema compatible with the columns sesssite reads.
// sesssite NEVER AutoMigrates in production (read-only contract); tests
// are the only place the models create tables.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	l := logxi.New("test")
	l.SetLevel(logxi.LevelAll)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")+"?_pragma=journal_mode(WAL)"), &gorm.Config{
		Logger: newGormLogger(l),
	})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Session{}, &User{}, &Message{}, &TurnUsage{}, &Compaction{}))
	return db
}

func mkMessage(t *testing.T, db *gorm.DB, sessionID int64, role, content string, mods ...func(*Message)) Message {
	t.Helper()
	m := Message{SessionID: sessionID, Role: role, Content: content, CreatedAt: time.Now()}
	for _, f := range mods {
		f(&m)
	}
	require.NoError(t, db.Create(&m).Error)
	return m
}

func mkSession(t *testing.T, db *gorm.DB, slug string, mods ...func(*Session)) *Session {
	t.Helper()
	s := &Session{Slug: slug, Network: "libera", Channel: "#dave", ChatCommand: "dave",
		Service: "svc", Model: "m", Status: "active", CreatedAt: time.Now(), LastActive: time.Now()}
	for _, f := range mods {
		f(s)
	}
	require.NoError(t, db.Create(s).Error)
	return s
}
