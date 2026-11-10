package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSessionBySlug(t *testing.T) {
	db := newTestDB(t)
	mkSession(t, db, "aaaaaaaa")
	got, err := sessionBySlug(db, "aaaaaaaa")
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.ID)
	_, err = sessionBySlug(db, "zzzzzzzz")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestUserBySlug(t *testing.T) {
	db := newTestDB(t)
	u := User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "alice", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	require.NoError(t, db.Create(&u).Error)

	got, err := userBySlug(db, "uuuuuuuu")
	require.NoError(t, err)
	assert.Equal(t, "alice", got.CurrentNick)
	_, err = userBySlug(db, "zzzzzzzz")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestSessionMessagesExcludesTailCopies(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	mkMessage(t, db, s.ID, "user", "one")
	mkMessage(t, db, s.ID, "assistant", "two", func(m *Message) { m.Archived = true })
	// Tail-copy: must NOT be returned (its content duplicates the archived original).
	mkMessage(t, db, s.ID, "user", "one", func(m *Message) { c := int64(9); m.SourceCompactionID = &c })
	// Superseded: invisible everywhere.
	mkMessage(t, db, s.ID, "assistant", "ghost", func(m *Message) { m.Superseded = true })
	mkMessage(t, db, s.ID, "user", "three")

	msgs, err := sessionMessagesPage(db, s.ID, 0, 200)
	require.NoError(t, err)
	require.Len(t, msgs, 3) // one, two(archived), three
	assert.Equal(t, "one", msgs[0].Content)
	assert.Equal(t, "two", msgs[1].Content)
	assert.Equal(t, "three", msgs[2].Content)
}

func TestSessionMessagesPageKeyset(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	for i := 0; i < 5; i++ {
		mkMessage(t, db, s.ID, "user", "m")
	}
	page1, err := sessionMessagesPage(db, s.ID, 0, 3)
	require.NoError(t, err)
	require.Len(t, page1, 3)
	assert.Equal(t, int64(3), page1[0].ID) // ASC-ending despite DESC query
	page2, err := sessionMessagesPage(db, s.ID, page1[0].ID, 3)
	require.NoError(t, err)
	require.Len(t, page2, 2)
}

func TestSessionMessageByID(t *testing.T) {
	db := newTestDB(t)
	s1 := mkSession(t, db, "aaaaaaaa")
	s2 := mkSession(t, db, "bbbbbbbb")
	m := mkMessage(t, db, s1.ID, "user", "hello")

	got, err := sessionMessageByID(db, s1.ID, m.ID)
	require.NoError(t, err)
	assert.Equal(t, "hello", got.Content)

	// Same message id under a different session must not resolve.
	_, err = sessionMessageByID(db, s2.ID, m.ID)
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
}

func TestUserSessionsPageKeyset(t *testing.T) {
	db := newTestDB(t)
	uid := int64(5)
	old := mkSession(t, db, "aaaaaaaa", func(s *Session) { s.UserID = &uid; s.LastActive = time.Now().Add(-2 * time.Hour) })
	newer := mkSession(t, db, "bbbbbbbb", func(s *Session) { s.UserID = &uid; s.LastActive = time.Now() })
	mkSession(t, db, "cccccccc", func(s *Session) { u := int64(6); s.UserID = &u })

	page, err := userSessionsPage(db, uid, time.Time{}, 0, 10)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, newer.ID, page[0].ID)
	assert.Equal(t, old.ID, page[1].ID)

	next, err := userSessionsPage(db, uid, page[0].LastActive, page[0].ID, 10)
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.Equal(t, old.ID, next[0].ID)
}

func TestUserSessionsPageSameLastActiveTiebreak(t *testing.T) {
	// Sessions sharing last_active must fall through to the id
	// keyset: the tuple compare (last_active, id) < (?, ?) orders and
	// paginates by id alone when timestamps tie — higher id first
	// (newest-first page), and a cursor at the higher id still yields
	// its same-timestamp sibling.
	db := newTestDB(t)
	uid := int64(5)
	ts := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	a := mkSession(t, db, "aaaaaaaa", func(s *Session) { s.UserID = &uid; s.LastActive = ts })
	b := mkSession(t, db, "bbbbbbbb", func(s *Session) { s.UserID = &uid; s.LastActive = ts })

	page, err := userSessionsPage(db, uid, time.Time{}, 0, 10)
	require.NoError(t, err)
	require.Len(t, page, 2)
	assert.Equal(t, b.ID, page[0].ID) // newest-first: higher id first on a timestamp tie
	assert.Equal(t, a.ID, page[1].ID)

	next, err := userSessionsPage(db, uid, page[0].LastActive, page[0].ID, 10)
	require.NoError(t, err)
	require.Len(t, next, 1)
	assert.Equal(t, a.ID, next[0].ID)
}

func TestSessionUsageRows(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	base := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	rows := []TurnUsage{
		{SessionID: s.ID, Model: "b", PromptTokens: 2, CreatedAt: base.Add(2 * time.Minute)},
		{SessionID: s.ID, Model: "a", PromptTokens: 1, CreatedAt: base},
		{SessionID: s.ID, Model: "c", PromptTokens: 3, CreatedAt: base}, // same created_at: id breaks the tie
		{SessionID: 999, Model: "other", CreatedAt: base},               // other session: excluded
	}
	for i := range rows {
		require.NoError(t, db.Create(&rows[i]).Error)
	}

	got, err := sessionUsageRows(db, s.ID)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "a", got[0].Model)
	assert.Equal(t, "c", got[1].Model)
	assert.Equal(t, "b", got[2].Model)
}

func TestSessionCompactions(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	later := Compaction{SessionID: s.ID, SummaryMessageID: 20, FirstArchivedID: 11, LastArchivedID: 20, ArchivedCount: 20, Trigger: "auto"}
	earlier := Compaction{SessionID: s.ID, SummaryMessageID: 10, FirstArchivedID: 1, LastArchivedID: 10, ArchivedCount: 10, Trigger: "manual"}
	require.NoError(t, db.Create(&later).Error)
	require.NoError(t, db.Create(&earlier).Error)

	got, err := sessionCompactions(db, s.ID)
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Ordered id ASC; pin each row's payload to its position so a DESC
	// regression cannot pass by id coincidence.
	assert.Equal(t, later.ID, got[0].ID)
	assert.Equal(t, earlier.ID, got[1].ID)
	assert.EqualValues(t, 20, got[0].ArchivedCount)
	assert.EqualValues(t, 10, got[1].ArchivedCount)
}
