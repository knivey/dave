package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

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
