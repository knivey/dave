package main

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var slugRe = regexp.MustCompile(`^[A-Za-z0-9]{8}$`)

func TestGenerateSlugShape(t *testing.T) {
	seen := make(map[string]bool, 1000)
	for i := 0; i < 1000; i++ {
		s, err := generateSlug()
		require.NoError(t, err)
		assert.Regexp(t, slugRe, s)
		assert.False(t, seen[s], "slug repeated within 1000 draws: %s", s)
		seen[s] = true
	}
}

func TestValidSlug(t *testing.T) {
	s, err := generateSlug()
	require.NoError(t, err)
	assert.True(t, validSlug(s))
	assert.False(t, validSlug("short"))
	assert.False(t, validSlug("toolongslug1"))
	assert.False(t, validSlug(" spaces "))
	assert.False(t, validSlug("slash/s"))
	assert.False(t, validSlug(""))
}

func TestCreateSessionMintsSlug(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	id, err := sessionMgr.CreateSession("libera", "#dave", 42, "dave", "svc", "model")
	require.NoError(t, err)
	sess, err := sessionMgr.GetSession(id)
	require.NoError(t, err)
	assert.Regexp(t, slugRe, sess.Slug)
}

func TestCreateNewUserMintsSlug(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	u, err := createNewUser("libera", "shrew", "shrew", "", "~s", "example.com")
	require.NoError(t, err)
	assert.Regexp(t, slugRe, u.Slug)
}

func TestMigrationBackfillsSlugs(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	// Legacy rows: slug column exists (AutoMigrate added it) but is NULL.
	uid := insertMigrationUser(t, "libera", "olduser")
	sid := insertMigrationSessionRow(t, "libera", "#dave", uid, "dave", "svc", "m", "active")
	require.NoError(t, theDB.Exec("UPDATE sessions SET slug = NULL WHERE id = ?", sid).Error)
	require.NoError(t, theDB.Exec("UPDATE users SET slug = NULL WHERE id = ?", uid).Error)

	require.NoError(t, addSessionUserSlugs(theDB))

	var sess Session
	require.NoError(t, theDB.First(&sess, sid).Error)
	assert.Regexp(t, slugRe, sess.Slug)
	var user User
	require.NoError(t, theDB.First(&user, uid).Error)
	assert.Regexp(t, slugRe, user.Slug)
}

func TestMigrationSlugUniquenessAcrossRows(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	ids := make([]int64, 0, 50)
	for i := 0; i < 50; i++ {
		ids = append(ids, insertMigrationSessionRow(t, "libera", "#dave", int64(i+1), "dave", "svc", "m", "active"))
	}
	require.NoError(t, theDB.Exec("UPDATE sessions SET slug = NULL").Error)
	require.NoError(t, addSessionUserSlugs(theDB))

	slugs := make(map[string]bool)
	for _, id := range ids {
		var sess Session
		require.NoError(t, theDB.First(&sess, id).Error)
		assert.False(t, slugs[sess.Slug], "duplicate slug %s", sess.Slug)
		slugs[sess.Slug] = true
	}
}

func insertMigrationUser(t *testing.T, network, nick string) int64 {
	t.Helper()
	var id int64
	rows, err := theDB.Raw(
		"INSERT INTO users (network, current_nick, normalized_nick, created_at, updated_at) VALUES (?, ?, ?, datetime('now'), datetime('now')) RETURNING id",
		network, nick, nick,
	).Rows()
	require.NoError(t, err, "insert migration user")
	defer rows.Close()
	if rows.Next() {
		require.NoError(t, rows.Scan(&id))
	}
	return id
}

// insertMigrationSessionRow inserts a session row using the columns that
// exist on today's sessions table. (The legacy insertMigrationSession helper
// in migrations_test.go still targets the long-gone nick column for the
// pre-migration-#4 shape; slug tests need current-shape rows.) Omitting slug
// leaves it NULL — the legacy-row shape that migration #10 backfills.
func insertMigrationSessionRow(t *testing.T, network, channel string, userID int64, chatCmd, service, model, status string) int64 {
	t.Helper()
	convID := "migration-test-conv"
	var id int64
	rows, err := theDB.Raw(
		"INSERT INTO sessions (network, channel, user_id, chat_command, conv_id, service, model, status, first_message, created_at, last_active) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', datetime('now'), datetime('now')) RETURNING id",
		network, channel, userID, chatCmd, convID, service, model, status,
	).Rows()
	require.NoError(t, err, "insert migration session row")
	defer rows.Close()
	if rows.Next() {
		require.NoError(t, rows.Scan(&id))
	}
	return id
}
