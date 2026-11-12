package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAuthorServer builds a cloned-session fixture where two different
// humans wrote user rows in one transcript (alice's words inherited
// via clone, bob's typed after) plus one unattributed legacy row.
func newAuthorServer(t *testing.T) (*httptest.Server, *Session) {
	t.Helper()
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	t.Cleanup(ts.Close)

	require.NoError(t, db.Create(&User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "alice"}).Error)
	require.NoError(t, db.Create(&User{Slug: "vvvvvvvv", Network: "libera", CurrentNick: "bob"}).Error)
	alice, bob := int64(1), int64(2)

	s := mkSession(t, db, "aaaaaaaa", func(x *Session) { x.UserID = &bob })
	mkMessage(t, db, s.ID, "user", "alice asks", func(m *Message) { m.UserID = &alice })
	mkMessage(t, db, s.ID, "assistant", "dave answers")
	mkMessage(t, db, s.ID, "user", "bob replies", func(m *Message) { m.UserID = &bob })
	mkMessage(t, db, s.ID, "user", "legacy unattributed") // no UserID
	return ts, s
}

// TestSessionPageShowsAuthors: user rows carry the speaker's nick
// (several speakers may appear in one cloned transcript); the legacy
// unattributed row and non-user rows keep the bare role label.
func TestSessionPageShowsAuthors(t *testing.T) {
	ts, s := newAuthorServer(t)

	resp, err := http.Get(ts.URL + "/s/" + s.Slug)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)

	assert.Contains(t, body, `class="role author"`, "attributed rows carry the author class")
	assert.Contains(t, body, ">alice</span>", "alice's inherited row shows her nick")
	assert.Contains(t, body, ">bob</span>", "bob's row shows his nick")
	assert.Contains(t, body, `class="role">user</span>`, "legacy row falls back to the role label")
	assert.Contains(t, body, `class="role">assistant</span>`, "assistant rows keep the role label")
}

// TestSessionFragmentShowsAuthors: the older-rows fragment path
// resolves authors too (shared applyAuthors wiring).
func TestSessionFragmentShowsAuthors(t *testing.T) {
	ts, s := newAuthorServer(t)

	resp, err := http.Get(ts.URL + "/s/" + s.Slug + "?fragment=rows&before=999")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, ">alice</span>")
	assert.Contains(t, body, ">bob</span>")
}

// TestRenderRowsAfterShowsAuthors: the SSE path resolves authors.
func TestRenderRowsAfterShowsAuthors(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	require.NoError(t, db.Create(&User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "alice"}).Error)
	alice := int64(1)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "user", "hello there", func(x *Message) { x.UserID = &alice })

	frag, _, err := srv.renderRowsAfter(s, m.ID-1)
	require.NoError(t, err)
	assert.Contains(t, frag, `class="role author"`)
	assert.Contains(t, frag, ">alice</span>")
}
