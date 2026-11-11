package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newClonedServer: user "cloner" with two sessions — one cloned from
// shrew's session (which exists), one plain — plus shrew's own row.
func newClonedServer(t *testing.T) (*httptest.Server, *gorm.DB, *Session, *Session, *Session) {
	t.Helper()
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	t.Cleanup(ts.Close)

	require.NoError(t, db.Create(&User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "shrew"}).Error)
	require.NoError(t, db.Create(&User{Slug: "vvvvvvvv", Network: "libera", CurrentNick: "cloner"}).Error)
	shrewUID := int64(1)
	clonerUID := int64(2)

	src := mkSession(t, db, "aaaaaaaa", func(s *Session) { s.UserID = &shrewUID; s.FirstMessage = "original shrew question" })
	clone := mkSession(t, db, "bbbbbbbb", func(s *Session) {
		s.UserID = &clonerUID
		srcID := src.ID
		s.ClonedFromID = &srcID
		s.ClonedFromNick = "shrew"
		s.FirstMessage = "inherited from shrew"
	})
	plain := mkSession(t, db, "cccccccc", func(s *Session) { s.UserID = &clonerUID; s.FirstMessage = "homegrown" })
	return ts, db, src, clone, plain
}

// TestSessionPageClonedFromBadge: the session page shows a cloned-from
// badge carrying the source owner's nick, linking to the source
// session's page when the source still resolves.
func TestSessionPageClonedFromBadge(t *testing.T) {
	ts, _, src, clone, _ := newClonedServer(t)

	resp, err := http.Get(ts.URL + "/s/" + clone.Slug)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "cloned from")
	assert.Contains(t, body, "shrew")
	assert.Contains(t, body, `href="/s/`+src.Slug+`"`, "badge links the source session")
}

// TestSessionPageClonedFromMissingSource: a dangling ClonedFromID
// (source deleted) still shows the badge — no link, no error.
func TestSessionPageClonedFromMissingSource(t *testing.T) {
	ts, db, _, clone, _ := newClonedServer(t)
	var src Session
	require.NoError(t, db.Where("slug = ?", "aaaaaaaa").First(&src).Error)
	require.NoError(t, db.Delete(&src).Error)

	resp, err := http.Get(ts.URL + "/s/" + clone.Slug)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "cloned from")
	assert.Contains(t, body, "shrew")
	assert.NotContains(t, body, `href="/s/aaaaaaaa`)
}

// TestSessionPageNoCloneBadgeWhenPlain: sessions without lineage show
// nothing.
func TestSessionPageNoCloneBadgeWhenPlain(t *testing.T) {
	ts, _, _, _, plain := newClonedServer(t)

	resp, err := http.Get(ts.URL + "/s/" + plain.Slug)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.NotContains(t, readBody(t, resp), "cloned from")
}

// TestUserPageClonedBadge: the user list marks cloned rows with the
// source owner's nick; plain rows carry no marker.
func TestUserPageClonedBadge(t *testing.T) {
	ts, _, _, _, _ := newClonedServer(t)

	resp, err := http.Get(ts.URL + "/u/vvvvvvvv")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "⤺ shrew", "visible badge text")
	assert.Contains(t, body, "cloned from shrew", "tooltip")
	assert.Equal(t, 1, strings.Count(body, "cloned from"), "exactly the clone row carries the marker")
}
