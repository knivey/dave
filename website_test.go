package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebsiteMessageLink: a session with messages and a configured website
// URL yields an absolute /s/<slug>#m<id> anchor URL pointing at the
// session's LATEST assistant row.
func TestWebsiteMessageLink(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	id, err := sessionMgr.CreateSession("libera", "#dave", 7, "dave", "svc", "m")
	require.NoError(t, err)
	sess, err := sessionMgr.GetSession(id)
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(id, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(id, ChatMessage{Role: RoleAssistant, Content: "hello"}))
	// A second exchange: the anchor must track the NEWEST assistant row.
	require.NoError(t, sessionMgr.AddMessage(id, ChatMessage{Role: RoleUser, Content: "again"}))
	require.NoError(t, sessionMgr.AddMessage(id, ChatMessage{Role: RoleAssistant, Content: "hello again"}))

	setWebsiteURL(t, "https://s.example.com")

	link, ok := websiteMessageLink(id)
	require.True(t, ok)
	mid, ok := latestAssistantMessageID(id)
	require.True(t, ok)
	assert.Equal(t, "https://s.example.com/s/"+sess.Slug+"#m"+fmtInt(mid), link)
}

// TestWebsiteMessageLinkNoAssistantRow: a session with no assistant message
// yet still links — to the session page itself, without an anchor.
func TestWebsiteMessageLinkNoAssistantRow(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	id, err := sessionMgr.CreateSession("libera", "#dave", 7, "dave", "svc", "m")
	require.NoError(t, err)
	sess, err := sessionMgr.GetSession(id)
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(id, ChatMessage{Role: RoleUser, Content: "hi"}))

	setWebsiteURL(t, "https://s.example.com")

	link, ok := websiteMessageLink(id)
	require.True(t, ok)
	assert.Equal(t, "https://s.example.com/s/"+sess.Slug, link)
}

// TestWebsiteMessageLinkLegacyRowNoSlug: pre-migration rows carry no slug;
// there is nothing to link, so the caller falls back to the pastebin.
func TestWebsiteMessageLinkLegacyRowNoSlug(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	id, err := sessionMgr.CreateSession("libera", "#dave", 7, "dave", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, theDB.Model(&Session{}).Where("id = ?", id).Update("slug", "").Error)

	setWebsiteURL(t, "https://s.example.com")

	_, ok := websiteMessageLink(id)
	assert.False(t, ok)
}

func TestWebsiteMessageLinkUnconfigured(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	id, err := sessionMgr.CreateSession("libera", "#dave", 7, "dave", "svc", "m")
	require.NoError(t, err)

	setWebsiteURL(t, "")
	_, ok := websiteMessageLink(id)
	assert.False(t, ok)
}

func TestWebsiteMessageLinkNoSession(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	setWebsiteURL(t, "https://s.example.com")
	// sessionID 0 = ephemeral generator turn: no session to link.
	_, ok := websiteMessageLink(0)
	assert.False(t, ok)
}

// TestWebsiteURLTrimsTrailingSlash pins websiteURL()'s normalization — the
// link builder concatenates base + "/s/..." and a trailing slash would
// produce a double-slash path.
func TestWebsiteURLTrimsTrailingSlash(t *testing.T) {
	setWebsiteURL(t, "https://s.example.com//")
	assert.Equal(t, "https://s.example.com", websiteURL())
}

func TestSendWithPastebinWebsiteLinkMode(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	setupNoticesDefaults(t)

	id, err := sessionMgr.CreateSession("libera", "#dave", 7, "dave", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(id, ChatMessage{Role: RoleAssistant, Content: "long reply"}))

	setWebsiteURL(t, "https://s.example.com")

	cr, out := newSendTestRunner(t)
	cr.sessionID = id
	n := getNotices()

	sent := cr.sendWithPastebin("line one\nline two\nline three", "line one\nline two\nline three")
	assert.True(t, sent)
	lines := drainOutput(t, out, 3, time.Second)
	joined := strings.Join(lines, "\n")
	sess, err := sessionMgr.GetSession(id)
	require.NoError(t, err)
	assert.Contains(t, joined, "https://s.example.com/s/"+sess.Slug, "website link line")
	assert.Contains(t, joined, "line one", "preview lines still send")
	assert.Contains(t, joined, "line two", "preview lines still send")
	assert.NotContains(t, joined, "line three", "the overflow tail lives on the website, not IRC")
	assert.NotContains(t, joined, n.Pastebin.Failed, "pastebin path must not run")
	// max_lines 3 ⇒ preview capped at 2 (GetPastebinPreviewLines always
	// leaves the last line for the link): two preview lines + the link.
	assert.Equal(t, 3, len(lines), "two preview lines + the full-message link line")
}

func TestSendWithPastebinEphemeralFallsBack(t *testing.T) {
	// Ephemeral generators run with sessionID == 0: there is no session to
	// link, so the website mode MUST fall back to the pastebin path —
	// with the pastebin unconfigured that path emits its upload error,
	// never a website link with an empty slug.
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	setupNoticesDefaults(t)

	setWebsiteURL(t, "https://s.example.com")
	setPastebinURL(t, "")

	cr, out := newSendTestRunner(t)
	cr.sessionID = 0
	n := getNotices()

	cr.sendWithPastebin("line one\nline two\nline three", "line one\nline two\nline three")
	// Error path emits: upload-error notice + 3 preview lines (GetMaxLines)
	// + the failed notice.
	lines := drainOutput(t, out, 5, time.Second)
	joined := strings.Join(lines, "\n")
	assert.NotContains(t, joined, "https://s.example.com/s/", "no website link for ephemeral turns")
	assert.Contains(t, joined, n.Pastebin.Failed, "the pastebin fallback path ran")
}

// setWebsiteURL points the global config's website URL at base for the
// duration of the test (direct mutation + restore — the repo's test
// pattern; there is no writeConfig helper).
func setWebsiteURL(t *testing.T, base string) {
	t.Helper()
	prev := config.Website.URL
	config.Website.URL = base
	t.Cleanup(func() { config.Website.URL = prev })
}

func setPastebinURL(t *testing.T, url string) {
	t.Helper()
	prev := config.Pastebin.URL
	config.Pastebin.URL = url
	t.Cleanup(func() { config.Pastebin.URL = prev })
}

// newSendTestRunner mirrors TestSendRenderedBBCode's chatRunner
// construction (bbcode_test.go), with a channel configured so the
// three-line overflow branch fires: Pastebin on, max_lines 3.
func newSendTestRunner(t *testing.T) (*chatRunner, chan string) {
	t.Helper()
	out := make(chan string, 32)
	cr := &chatRunner{
		cfg:      AIConfig{Service: "svc", Model: "m"},
		network:  Network{Name: "libera", Channels: map[string]ChannelConfig{"#dave": {Pastebin: true, MaxLines: 3}}},
		channel:  "#dave",
		ctx:      context.Background(),
		outputCh: out,
		logger:   newTestLogger(),
	}
	return cr, out
}

func fmtInt(i int64) string { return strconv.FormatInt(i, 10) }

// newLinkTestClient builds an unconnected girc client — linkCommand's only
// client use is accountFromEvent's LookupUser fallback, which panics on a
// nil client (girc's LookupUser dereferences c.Config). Same construction
// as TestAccountFromEvent (main_test.go).
func newLinkTestClient() *girc.Client {
	return girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})
}

// drainLinkOutput closes out and returns everything linkCommand sent.
func drainLinkOutput(t *testing.T, out chan string) []string {
	t.Helper()
	close(out)
	var got []string
	for l := range out {
		got = append(got, l)
	}
	return got
}

// TestLinkCommand: the happy path — caller resolves, has an active session,
// and both the user row and the session row carry slugs. The reply carries
// the session URL (/s/<slug>) and the user URL (/u/<slug>).
func TestLinkCommand(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	setupNoticesDefaults(t)

	uid := ensureTestUser(t, "libera", "shrew")
	sid, err := sessionMgr.CreateSession("libera", "#dave", uid, "dave", "svc", "m")
	require.NoError(t, err)
	sess, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	var u User
	require.NoError(t, theDB.First(&u, uid).Error)

	setWebsiteURL(t, "https://s.example.com")

	out := make(chan string, 8)
	linkCommand(Network{Name: "libera", Trigger: "^"}, newLinkTestClient(),
		makeHistoryEvent("#dave", "shrew"), context.Background(), out)
	got := drainLinkOutput(t, out)

	joined := strings.Join(got, "\n")
	assert.Contains(t, joined, "https://s.example.com/s/"+sess.Slug)
	assert.Contains(t, joined, "https://s.example.com/u/"+u.Slug)
}

// TestLinkCommandNoWebsiteConfigured: website.url unset — the command must
// not touch the DB or silently drop; an error line naming the config comes
// back (nil client is safe: the base=="" branch returns before any client
// use).
func TestLinkCommandNoWebsiteConfigured(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	setupNoticesDefaults(t)
	setWebsiteURL(t, "")

	out := make(chan string, 8)
	linkCommand(Network{Name: "libera"}, nil,
		makeHistoryEvent("#dave", "shrew"), context.Background(), out)
	got := drainLinkOutput(t, out)

	// Some error line must come back naming the config.
	assert.NotEmpty(t, got)
	assert.Contains(t, strings.Join(got, "\n"), "website url not configured")
}

// TestLinkCommandNoActiveSession: caller has a slug but no active session —
// the LinkNoSession template carries the user URL alone.
func TestLinkCommandNoActiveSession(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	setupNoticesDefaults(t)

	uid := ensureTestUser(t, "libera", "shrew")
	var u User
	require.NoError(t, theDB.First(&u, uid).Error)

	setWebsiteURL(t, "https://s.example.com")

	out := make(chan string, 8)
	linkCommand(Network{Name: "libera", Trigger: "^"}, newLinkTestClient(),
		makeHistoryEvent("#dave", "shrew"), context.Background(), out)
	got := drainLinkOutput(t, out)

	joined := strings.Join(got, "\n")
	assert.Contains(t, joined, "https://s.example.com/u/"+u.Slug)
	assert.NotContains(t, joined, "https://s.example.com/s/", "no session to link")
}

// TestLinkCommandUserSlugMissing: the caller has an active session but the
// user row lacks a slug (legacy pre-migration shape) — the session URL
// rides the {user_url} placeholder of LinkNoSession rather than failing.
func TestLinkCommandUserSlugMissing(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	setupNoticesDefaults(t)

	uid := insertMigrationUser(t, "libera", "shrew") // raw insert: no slug
	sid, err := sessionMgr.CreateSession("libera", "#dave", uid, "dave", "svc", "m")
	require.NoError(t, err)
	sess, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)

	setWebsiteURL(t, "https://s.example.com")

	out := make(chan string, 8)
	linkCommand(Network{Name: "libera", Trigger: "^"}, newLinkTestClient(),
		makeHistoryEvent("#dave", "shrew"), context.Background(), out)
	got := drainLinkOutput(t, out)

	joined := strings.Join(got, "\n")
	assert.Contains(t, joined, "https://s.example.com/s/"+sess.Slug)
	assert.NotContains(t, joined, "https://s.example.com/u/", "no user slug to link")
}

// TestLinkCommandNothingToLink: no active session AND no user slug (a
// caller whose rows predate slugs entirely) — the generic "no sessions"
// notice is the reply; no half-empty website link is sent.
func TestLinkCommandNothingToLink(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	setupNoticesDefaults(t)

	insertMigrationUser(t, "libera", "shrew") // raw insert: no slug
	setWebsiteURL(t, "https://s.example.com")

	out := make(chan string, 8)
	linkCommand(Network{Name: "libera", Trigger: "^"}, newLinkTestClient(),
		makeHistoryEvent("#dave", "shrew"), context.Background(), out)
	got := drainLinkOutput(t, out)

	joined := strings.Join(got, "\n")
	n := getNotices()
	assert.Contains(t, joined, n.Sessions.None)
	assert.NotContains(t, joined, "https://s.example.com/", "nothing linkable")
}

// TestLinkCommandUnresolvableUser: an event with no source cannot resolve a
// user — the command must still say something (the shared no-website error
// notice), never silently return.
func TestLinkCommandUnresolvableUser(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	setupNoticesDefaults(t)
	setWebsiteURL(t, "https://s.example.com")

	out := make(chan string, 8)
	linkCommand(Network{Name: "libera", Trigger: "^"}, newLinkTestClient(),
		girc.Event{Params: []string{"#dave"}}, context.Background(), out)
	got := drainLinkOutput(t, out)

	assert.NotEmpty(t, got, "no silent return on unresolvable user")
}
