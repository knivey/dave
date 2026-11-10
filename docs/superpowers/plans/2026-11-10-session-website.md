# Session History Website (`sesssite`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A read-only website (`sesssite/`) over dave's DB showing full session histories and per-user session lists at unlisted random-slug URLs, with live updates and a config-gated pastebin replacement, plus the dave-side slug minting and link plumbing.

**Architecture:** dave stays the single DB writer: it mints 8-char base62 slugs onto `sessions`/`users` (new columns + migration #10 backfill), links over-long replies to the site when `[website] url` is configured, and answers `^link`. The site is a sibling package (imgsite pattern): single binary, TOML config, GORM read-only models, server-rendered HTML + vanilla JS, SSE fed by 1s DB polls. Markdown rendering is ported from `~/markdown-parsebin` (goldmark + GFM + Chroma + IRC-codes→spans transformer).

**Tech Stack:** Go 1.25, GORM (glebarez/sqlite + gorm postgres dialectors — all already deps), goldmark v1.8.2, `github.com/yuin/goldmark-highlighting/v2` (NEW dep), `github.com/alecthomas/chroma/v2` (already dep), BurntSushi/toml, net/http + html/template, testify.

**Spec:** `docs/superpowers/specs/2026-11-10-session-website-design.md` — the plan argues from the spec; executors read both.

## Global Constraints

- Go module is `github.com/knivey/dave` — `sesssite/` is a package in the SAME module; no new go.mod.
- Only ONE new dependency may be added: `github.com/yuin/goldmark-highlighting/v2`. If a task seems to need another, stop and reconsider.
- dave's DB is written ONLY by dave. `sesssite` never issues INSERT/UPDATE/DELETE and never calls AutoMigrate in its production open path.
- Slugs are exactly 8 chars from `abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789`, minted with `crypto/rand` (unbiased via `math/big`), never rotated.
- Every config file in `config/` and `sesssite/` keeps the reference-section + commented-example-block documentation convention (see AGENTS.md).
- All loggers created with `logxi.New` MUST call `SetLevel(logxi.LevelAll)`.
- Style: `go fmt ./...` + `go vet ./...` clean before every commit; tests via `github.com/stretchr/testify`.
- Commit after every task. NEVER `git add -f`.
- Run `go build ./...` and `go test ./...` from the repo root (covers both packages).

## Review Focus

The spec implies these hazards; each is pinned by a test in the task that owns the code (test names in parentheses):

1. **Raw endpoint fidelity** — IRC control codes and markdown syntax must come back byte-for-byte, never translated (`TestRawEndpointByteForByte` — Task 8).
2. **Compaction tail-copy duplication** — re-inserted tail rows would show the same messages twice in the flat transcript (`TestSessionMessagesExcludesTailCopies` — Task 5; `TestSessionPageTailCopiesNotDoublyRendered` — Task 8).
3. **Ephemeral generator overflow with website URL set** — `sessionID == 0` has no session to link; must fall back to the pastebin path, never emit a link with an empty slug (`TestSendWithPastebinEphemeralFallsBack` — Task 3).
4. **Soft-deleted session slug** — a deleted session's slug must 404 like an unknown one, no existence leak (`TestSessionPageSoftDeleted404` — Task 8).
5. **SSE reconnect gap** — rows inserted while a viewer was disconnected must still arrive via `?since=` (`TestSessionEventsSinceGapFill` — Task 10).
6. **Slug shape on lookup** — 7-char, 9-char, or non-base62 slugs 404 without touching the DB (`TestSlugValidation` — Task 8; dave-side `TestGenerateSlugShape` — Task 1).

---

### Task 1: dave — slug generation, DB columns, migration #10, mint at creation

**Files:**
- Create: `slug.go`
- Create: `slug_test.go`
- Modify: `db.go` (Session struct ~line 47, User struct ~line 208)
- Modify: `migrations.go` (migrations list ~line 46, new func near the other migration funcs)
- Modify: `sessionManager.go` (`CreateSession` ~line 63)
- Modify: `users.go` (`createNewUser` ~line 600)
- Test: `slug_test.go`, additions to `migrations_test.go`, `sessionManager`/`users` tests colocated in `slug_test.go`

**Interfaces:**
- Consumes: existing `initDB`/migration runner, `setupMigrationDB` helper in `migrations_test.go`.
- Produces: `generateSlug() (string, error)` and `validSlug(s string) bool` (package main, dave root) — later tasks and `sesssite` (re-copied) rely on the exact 8-base62 contract; `Session.Slug` / `User.Slug` string fields.

- [ ] **Step 1: Write failing tests**

`slug_test.go`:

```go
package main

import (
	"regexp"
	"testing"
	"time"

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
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()
	_ = dbPath

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
	sid := insertMigrationSession(t, "libera", "#dave", "olduser", "dave", "svc", "m", "active")
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
		ids = append(ids, insertMigrationSession(t, "libera", "#dave", "u"+string(rune('a'+i%26)), "dave", "svc", "m", "active"))
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
	_ = time.Now
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
```

Note: `insertMigrationSession` exists in `migrations_test.go` but inserts a `nick` column — check its current signature before use; if the helper has drifted, insert via `theDB.Raw` the same way with the columns that exist today (`network, channel, user_id, chat_command, conv_id, service, model, status, first_message, created_at, last_active`).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestGenerateSlug|TestValidSlug|TestCreateSessionMints|TestCreateNewUserMints|TestMigrationBackfills|TestMigrationSlugUnique' .`
Expected: FAIL — `generateSlug`/`validSlug` undefined, `Slug` fields don't exist.

- [ ] **Step 3: Implement**

`slug.go`:

```go
package main

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// Slugs are the public identifiers for the session-history website
// (docs/superpowers/specs/2026-11-10-session-website-design.md): 8 random
// base62 chars, ~2e14 space, minted once, never rotated. Uniqueness is
// enforced by a unique index; the space is large enough that collisions
// are a non-event (and callers error out rather than retry-loop).
const (
	slugLen  = 8
	slugChar = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// generateSlug draws slugLen unbiased random chars (math/big Int, not
// byte%len — that is modulo-biased). Ported from markdown-parsebin's
// util.GenerateSlug.
func generateSlug() (string, error) {
	max := big.NewInt(int64(len(slugChar)))
	b := make([]byte, slugLen)
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("generate slug: %w", err)
		}
		b[i] = slugChar[n.Int64()]
	}
	return string(b), nil
}

// validSlug reports whether s is a well-formed slug. Used to reject
// malformed lookups before they touch the DB.
func validSlug(s string) bool {
	if len(s) != slugLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}
```

`db.go` — add to `Session` (after `ConvID` line) and `User` (after `IRCAccount` line):

```go
	// Slug is the public 8-char identifier for the session-history
	// website. Minted once at creation; empty only on legacy rows before
	// migration #10 backfilled them. Unique index; '' never reaches the
	// DB because the only writers mint before insert.
	Slug string `gorm:"uniqueIndex"`
```

(Comment on User can be shorter: `// Slug: public website identifier, see Session.Slug.`)

`sessionManager.go` `CreateSession` — mint before insert:

```go
	convID := generateConvID()
	slug, err := generateSlug()
	if err != nil {
		return 0, fmt.Errorf("minting session slug: %w", err)
	}
	session := Session{
		Network:     network,
		Channel:     channel,
		UserID:      &userID,
		ChatCommand: chatCommand,
		ConvID:      &convID,
		Slug:        slug,
		Service:     service,
		Model:       model,
		Status:      StatusActive,
	}
```

`users.go` `createNewUser` — same pattern:

```go
	slug, err := generateSlug()
	if err != nil {
		return nil, fmt.Errorf("minting user slug: %w", err)
	}
	user := User{
		Network:        network,
		CurrentNick:    nick,
		NormalizedNick: normalizedNick,
		IRCAccount:     account,
		Slug:           slug,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
```

Then verify no other creation site exists: `grep -n 'Create(&Session{\|Create(&User{\|Create(&user)\|Create(&session)' *.go` — every hit must go through the minting paths above (fix any that don't).

`migrations.go` — add to the `migrations` slice after ID 9:

```go
	{ID: 10, Name: "add_session_user_slugs", Up: addSessionUserSlugs},
```

and the func (near the other migration funcs):

```go
// addSessionUserSlugs backfills website slugs for rows created before the
// slug columns existed. The columns + unique indexes are created by
// AutoMigrate from the struct tags; this migration only fills data. NULL
// (legacy rows) is the backfill target — '' never occurs because the
// creation paths mint before insert.
func addSessionUserSlugs(db *gorm.DB) error {
	for _, table := range []string{"sessions", "users"} {
		var ids []int64
		if err := db.Table(table).Where("slug IS NULL").Pluck("id", &ids).Error; err != nil {
			return fmt.Errorf("migration 10: listing %s: %w", table, err)
		}
		for _, id := range ids {
			slug, err := generateSlug()
			if err != nil {
				return fmt.Errorf("migration 10: minting slug: %w", err)
			}
			// Retry on collision: the unique index rejects it, we redraw.
			// (Practically unreachable — 8 base62 chars vs table size.)
			for retry := 0; retry < 5; retry++ {
				res := db.Table(table).Where("id = ? AND slug IS NULL", id).Update("slug", slug)
				if res.Error != nil {
					if isUniqueConstraintErr(res.Error) && retry < 4 {
						slug, err = generateSlug()
						if err != nil {
							return fmt.Errorf("migration 10: minting slug: %w", err)
						}
						continue
					}
					return fmt.Errorf("migration 10: updating %s id %d: %w", table, id, res.Error)
				}
				if res.RowsAffected > 0 {
					break
				}
			}
		}
	}
	return nil
}
```

(`isUniqueConstraintErr` already exists in `users.go`.)

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestGenerateSlug|TestValidSlug|TestCreateSessionMints|TestCreateNewUserMints|TestMigrationBackfills|TestMigrationSlugUnique' . -v`
Expected: PASS.

Run: `go test .`
Expected: PASS (whole root package — catches struct/migration fallout).

- [ ] **Step 5: go fmt / vet + commit**

```bash
go fmt ./... && go vet ./...
git add slug.go slug_test.go db.go migrations.go sessionManager.go users.go migrations_test.go
git commit -m "feat: session/user website slugs — columns, migration #10 backfill, mint at creation"
```

---

### Task 2: dave — `[website]` config section + website notices

**Files:**
- Modify: `config.go` (Config struct ~line 21, helpers near `GetPastebinPreviewLines`)
- Modify: `notices.go` (NoticesConfig ~line 14, struct defs, `setNoticesDefaults`)
- Modify: `config/config.toml` (reference block + commented example)
- Modify: `config/notices.toml` (reference block + commented example)
- Test: `config_test.go`, `notices_test.go` (add cases)

**Interfaces:**
- Produces: `WebsiteConfig{URL string}`; `websiteURL() string` helper (returns configured base URL, "" when unset); `NoticesConfig.Website WebsiteNotices` with fields `FullLine`, `Link`, `LinkNoSession`; notice vars `{url}`, `{user_url}`, `{session_url}`.

- [ ] **Step 1: Write failing tests**

In `config_test.go` (use `createTestConfigDir` helper pattern from `TestLoadConfigDirNetworkEnabledDefault`):

```go
func TestLoadConfigDirWebsite(t *testing.T) {
	dir := createTestConfigDir(t, `
[website]
url = "https://sessions.example.com"
`)
	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "https://sessions.example.com", cfg.Website.URL)
}

func TestWebsiteURLEmptyByDefault(t *testing.T) {
	dir := createTestConfigDir(t, "")
	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "", cfg.Website.URL)
}
```

In `notices_test.go`:

```go
func TestNoticesWebsiteDefaults(t *testing.T) {
	n := NoticesConfig{}
	setNoticesDefaults(&n)
	assert.Contains(t, n.Website.FullLine, "{url}")
	assert.Contains(t, n.Website.Link, "{user_url}")
	assert.Contains(t, n.Website.Link, "{session_url}")
	assert.Contains(t, n.Website.LinkNoSession, "{user_url}")
	assert.NotEmpty(t, n.Website.FullLine)
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'TestLoadConfigDirWebsite|TestWebsiteURLEmpty|TestNoticesWebsiteDefaults' .`
Expected: FAIL — unknown fields.

- [ ] **Step 3: Implement**

`config.go` — add to `Config` (next to `Pastebin`):

```go
	Website              WebsiteConfig       `toml:"website"`
```

and:

```go
// WebsiteConfig points dave at the session-history website (sesssite).
// url is the ONLY knob: set = link long replies to the site instead of
// the pastebin (coexist switch); unset/empty = old pastebin behavior.
type WebsiteConfig struct {
	URL string `toml:"url"`
}

// websiteURL returns the configured session-site base URL (no trailing
// slash), or "" when unset.
func websiteURL() string {
	var u string
	readConfig(func() { u = config.Website.URL })
	return strings.TrimRight(u, "/")
}
```

`notices.go` — add to `NoticesConfig`:

```go
	Website    WebsiteNotices    `toml:"website"`
```

with:

```go
type WebsiteNotices struct {
	// FullLine is sent after the preview lines when a reply overflows and
	// the website is configured (replaces the pastebin link).
	FullLine string `toml:"full_line"`
	// Link is the ^link reply when the user has an active session.
	Link string `toml:"link"`
	// LinkNoSession is the ^link reply when there is no active session.
	LinkNoSession string `toml:"link_no_session"`
}
```

In `setNoticesDefaults`:

```go
	if n.Website.FullLine == "" {
		n.Website.FullLine = "full message: {url}"
	}
	if n.Website.Link == "" {
		n.Website.Link = "session: {session_url} · all your sessions: {user_url}"
	}
	if n.Website.LinkNoSession == "" {
		n.Website.LinkNoSession = "all your sessions: {user_url}"
	}
```

`config/config.toml` — extend the reference list and commented block:

```toml
# [website] — session-history website (sesssite)
# url: base URL of the session site (e.g. "https://sessions.example.com").
#      When set, over-long replies link to the site's session page instead
#      of uploading to the pastebin (the pastebin stays configured as the
#      fallback for ^help and ephemeral generator output). Unset = always
#      pastebin. See docs/superpowers/specs/2026-11-10-session-website-design.md
#
# [website]
# url = ""
```

`config/notices.toml` — same convention for the `[website]` section (`full_line`, `link`, `link_no_session` with the defaults above as the example).

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestLoadConfigDirWebsite|TestWebsiteURLEmpty|TestNoticesWebsiteDefaults' . -v && go test .`
Expected: PASS both.

- [ ] **Step 5: fmt/vet + commit**

```bash
go fmt ./... && go vet ./...
git add config.go config_test.go notices.go notices_test.go config/config.toml config/notices.toml
git commit -m "feat: [website] config + website notices (pastebin-link + ^link templates)"
```

---

### Task 3: dave — pastebin-link mode in `sendWithPastebin`

**Files:**
- Modify: `aiCmds.go` (`sendWithPastebin` ~line 440)
- Create: `website.go` (link builder + latest-assistant-id helper)
- Create: `website_test.go`
- Modify: `db.go` (one helper query)

**Interfaces:**
- Consumes: `websiteURL()` (Task 2), `getDBSessionByID` (db.go), `getNotices()`/`expandNotice` (notices.go), `n.Website.FullLine`.
- Produces: `websiteMessageLink(sessionID int64) (string, bool)` — absolute `…/s/<slug>#m<id>` URL built from `websiteURL()`, false when unconfigured/session-less/slug-less; `latestAssistantMessageID(sessionID int64) (int64, bool)`.

- [ ] **Step 1: Write failing tests**

`website_test.go` — construct the runner the same way `TestSendRenderedBBCode` in `bbcode_test.go` does (mirror its `chatRunner` literal and channel wiring; only the fields below matter):

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWebsiteMessageLink(t *testing.T) {
	dbPath, cleanup := setupMigrationDB(t)
	defer cleanup()
	_ = dbPath

	id, err := sessionMgr.CreateSession("libera", "#dave", 7, "dave", "svc", "m")
	require.NoError(t, err)
	sess, err := sessionMgr.GetSession(id)
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(id, ChatMessage{Role: RoleUser, Content: "hi"}))
	require.NoError(t, sessionMgr.AddMessage(id, ChatMessage{Role: RoleAssistant, Content: "hello"}))

	writeConfigValue(t, "https://s.example.com")

	link, ok := websiteMessageLink(id)
	require.True(t, ok)
	assert.Equal(t, "https://s.example.com/s/"+sess.Slug+"#m", link[:len("https://s.example.com/s/"+sess.Slug+"#m")])
	mid, ok := latestAssistantMessageID(id)
	require.True(t, ok)
	assert.Contains(t, link, "#m"+fmtInt(mid))
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

func TestWebsiteMessageLinkUnconfigured(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	id, err := sessionMgr.CreateSession("libera", "#dave", 7, "dave", "svc", "m")
	require.NoError(t, err)

	setWebsiteURL(t, "")
	_, ok := websiteMessageLink(id)
	assert.False(t, ok)
}

func TestSendWithPastebinWebsiteLinkMode(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	id, err := sessionMgr.CreateSession("libera", "#dave", 7, "dave", "svc", "m")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.AddMessage(id, ChatMessage{Role: RoleAssistant, Content: "long reply"}))

	setWebsiteURL(t, "https://s.example.com")

	cr, out := newSendTestRunner(t) // helper below
	cr.sessionID = id
	n := getNotices()

	lines := make([]string, 0, 8)
	go func() {
		for l := range out {
			lines = append(lines, l)
		}
	}()
	sent := cr.sendWithPastebin(wrapForIRC("long reply\nsecond line"), "long reply\nsecond line")
	assert.True(t, sent)
	joined := joinLines(lines)
	assert.Contains(t, joined, "https://s.example.com/s/")
	assert.NotContains(t, joined, n.Pastebin.Failed)
}

func TestSendWithPastebinEphemeralFallsBack(t *testing.T) {
	// Ephemeral generators run with sessionID == 0: there is no session to
	// link, so the website mode MUST fall back to the pastebin path —
	// with the pastebin unconfigured that path emits its upload error,
	// never a website link with an empty slug.
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	setWebsiteURL(t, "https://s.example.com")
	config.Pastebin.URL = ""

	cr, out := newSendTestRunner(t)
	cr.sessionID = 0
	lines := make([]string, 0, 8)
	done := make(chan struct{})
	go func() {
		for l := range out {
			lines = append(lines, l)
		}
		close(done)
	}()
	cr.sendWithPastebin("long reply\nsecond line", "long reply\nsecond line")
	assert.NotContains(t, joinLines(lines), "https://s.example.com/s/")
}
```

Helpers at the bottom of `website_test.go` (adjust the literal to whatever fields `chatRunner` actually requires — mirror `TestSendRenderedBBCode`'s construction):

```go
func newSendTestRunner(t *testing.T) (*chatRunner, chan string) {
	t.Helper()
	out := make(chan string, 32)
	network := getTestNetwork("libera") // or inline Network{Name: "libera"} if no helper exists
	cr := &chatRunner{
		network: network,
		channel: "#dave",
		ctx:     context.Background(),
		outputCh: out,
		logger:  newLogger("test"),
		cfg:     AIConfig{Service: "svc", Model: "m"},
	}
	return cr, out
}

func fmtInt(i int64) string { return strconv.FormatInt(i, 10) }

func joinLines(lines []string) string { return strings.Join(lines, "\n") }
```

Note: `sendWithPastebin` needs a channel config with `Pastebin: true` and a low `max_lines` so the overflow branch fires — set `network.Channels["#dave"] = ChannelConfig{Pastebin: true, MaxLines: 2}` inside the test (match how other tests seed network config; if `GetChannelConfig` reads global config instead, seed that).

- [ ] **Step 2: Run to verify failure**

Run: `go test -run 'TestWebsiteMessageLink|TestSendWithPastebin' .`
Expected: FAIL — `websiteMessageLink` undefined, link mode absent.

- [ ] **Step 3: Implement**

`db.go` (near other query helpers):

```go
// latestAssistantMessageID returns the newest assistant-role message row
// id for a session. Used by the website-link path: the final assistant
// message is persisted BEFORE the send (turn.Add → sendFinalText) and the
// session turn gate serializes writers, so at link-build time this IS the
// reply being sent. The reasoning-fallback edge lands on the nearest
// assistant row — accepted (spec).
func latestAssistantMessageID(sessionID int64) (int64, bool) {
	var id int64
	err := theDB.Model(&Message{}).
		Where("session_id = ? AND role = ?", sessionID, RoleAssistant).
		Order("id DESC").Limit(1).Pluck("id", &id).Error
	if err != nil || id == 0 {
		return 0, false
	}
	return id, true
}
```

`website.go`:

```go
package main

import "fmt"

// websiteMessageLink builds the absolute session-page anchor URL for the
// session's latest assistant message. ok=false when the website is
// unconfigured, the session is unknown, or it has no slug (pre-migration
// legacy row) — callers fall back to the pastebin path.
func websiteMessageLink(sessionID int64) (string, bool) {
	base := websiteURL()
	if base == "" || sessionID == 0 {
		return "", false
	}
	sess, err := getDBSessionByID(sessionID)
	if err != nil || sess == nil || !validSlug(sess.Slug) {
		return "", false
	}
	mid, ok := latestAssistantMessageID(sessionID)
	if !ok {
		// No assistant row (yet): link the session page itself.
		return fmt.Sprintf("%s/s/%s", base, sess.Slug), true
	}
	return fmt.Sprintf("%s/s/%s#m%d", base, sess.Slug, mid), true
}
```

`aiCmds.go` `sendWithPastebin` — insert the website branch after the `if len(lines) >= chCfg.GetMaxLines()` check, before `pasteTitle := …`:

```go
		if len(lines) >= chCfg.GetMaxLines() {
			// Website link mode (coexist): when [website] url is set and
			// this turn has a session, link the already-persisted reply on
			// the session site instead of uploading a paste. Ephemeral
			// (sessionID 0) and any link-build failure fall through to the
			// pastebin path below.
			if link, ok := websiteMessageLink(cr.sessionID); ok {
				n := getNotices()
				preview := chCfg.GetPastebinPreviewLines(config.Pastebin)
				if preview > len(lines) {
					preview = len(lines)
				}
				for i := 0; i < preview; i++ {
					cr.sendIRC(lines[i])
				}
				cr.sendIRC(expandNotice(n.Website.FullLine, map[string]string{"url": link}))
				return true
			}
			pasteTitle := cr.cfg.Service + "/" + cr.cfg.Model
			// … existing pastebin code unchanged …
```

- [ ] **Step 4: Run tests**

Run: `go test -run 'TestWebsiteMessageLink|TestSendWithPastebin' . -v && go test .`
Expected: PASS. Also confirm `go test -run TestHelp ./...` still passes (`^help` pastebin path untouched).

- [ ] **Step 5: fmt/vet + commit**

```bash
go fmt ./... && go vet ./...
git add aiCmds.go website.go website_test.go db.go
git commit -m "feat: website link mode replaces pastebin for over-long session replies"
```

---

### Task 4: dave — `^link` builtin command

**Files:**
- Modify: `website.go` (add handler)
- Modify: `main.go` (`builtInCmds` ~line 353)
- Test: `website_test.go` (append)

**Interfaces:**
- Consumes: `websiteURL()` (Task 2), `resolveIRCUserWithID` (historyCmds.go:107), `sessionMgr.GetActiveSession`, `getUserByID`-style lookup (add if missing — see step 3), `sendOrDone`, `dbGuard`, notices `n.Website.Link`/`LinkNoSession`.
- Produces: `linkCommand(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string)`; registered as `link_re`.

- [ ] **Step 1: Write failing test**

Append to `website_test.go`:

```go
func TestLinkCommand(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()

	uid := insertMigrationUser(t, "libera", "shrew")
	sid, err := sessionMgr.CreateSession("libera", "#dave", uid, "dave", "svc", "m")
	require.NoError(t, err)
	sess, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)

	setWebsiteURL(t, "https://s.example.com")

	out := make(chan string, 8)
	linkCommand(Network{Name: "libera", Trigger: "^"}, nil,
		gircEventFromNick("#dave", "shrew"), context.Background(), out)

	close(out)
	var got []string
	for l := range out {
		got = append(got, l)
	}
	joined := strings.Join(got, "\n")
	assert.Contains(t, joined, "https://s.example.com/s/"+sess.Slug)
	assert.Contains(t, joined, "https://s.example.com/u/")
}

func TestLinkCommandNoWebsiteConfigured(t *testing.T) {
	_, cleanup := setupMigrationDB(t)
	defer cleanup()
	setWebsiteURL(t, "")

	out := make(chan string, 8)
	linkCommand(Network{Name: "libera"}, nil,
		gircEventFromNick("#dave", "shrew"), context.Background(), out)
	close(out)
	var got []string
	for l := range out {
		got = append(got, l)
	}
	// Some error line must come back naming the config.
	assert.NotEmpty(t, got)
}
```

`gircEventFromNick` — build a minimal `girc.Event` the way other tests in `irc_handlers_test.go`/`historyCmds_test.go` construct events for `resolveIRCUserWithID` (copy the exact construction from a passing test there; if a helper already exists, reuse it instead).

- [ ] **Step 2: Run to verify failure**

Run: `go test -run TestLinkCommand .`
Expected: FAIL — `linkCommand` undefined.

- [ ] **Step 3: Implement**

Add to `website.go` (imports: context, girc):

```go
// linkCommand replies with the caller's session-site URLs: the active
// session page and the user page. Plain channel messages via notice
// templates (dave "notices" are templated channel messages, not IRC
// NOTICEs).
func linkCommand(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
	base := websiteURL()
	if base == "" {
		n := getNotices()
		sendOrDone(ctx, output, errorNotice(n.DB.NoWebsiteURL, map[string]string{}))
		return
	}
	if !dbGuard(output, ctx) {
		return
	}
	userID := resolveIRCUserWithID(network, c, e)
	if userID == 0 {
		n := getNotices()
		sendOrDone(ctx, output, errorNotice(n.DB.NoWebsiteURL, map[string]string{}))
		return
	}
	userSlug, ok := userSlugByID(userID)
	n := getNotices()
	userURL := ""
	if ok {
		userURL = fmt.Sprintf("%s/u/%s", base, userSlug)
	}
	casemapping := getCasemapping(network.Name)
	channel := normalizeIRC(e.Params[0], casemapping)
	sess, err := sessionMgr.GetActiveSession(network.Name, channel, userID)
	if err != nil || sess == nil || !validSlug(sess.Slug) {
		if userURL == "" {
			sendOrDone(ctx, output, n.Sessions.None)
			return
		}
		sendOrDone(ctx, output, expandNotice(n.Website.LinkNoSession, map[string]string{"user_url": userURL}))
		return
	}
	if userURL == "" {
		// Session exists but the user row somehow lacks a slug: link the
		// session alone rather than failing.
		sendOrDone(ctx, output, expandNotice(n.Website.LinkNoSession, map[string]string{
			"user_url": fmt.Sprintf("%s/s/%s", base, sess.Slug)}))
		return
	}
	sendOrDone(ctx, output, expandNotice(n.Website.Link, map[string]string{
		"session_url": fmt.Sprintf("%s/s/%s", base, sess.Slug),
		"user_url":    userURL,
	}))
}
```

`userSlugByID` in `website.go`:

```go
func userSlugByID(userID int64) (string, bool) {
	var slug string
	err := theDB.Model(&User{}).Where("id = ?", userID).Pluck("slug", &slug).Error
	if err != nil || !validSlug(slug) {
		return "", false
	}
	return slug, true
}
```

Notices: add `NoWebsiteURL string `toml:"no_website_url`` to `DBNotices` in `notices.go`, default `"website url not configured"` in `setNoticesDefaults`, documented in `config/notices.toml`. (Keeps the error-notice funnel pattern; one line.)

`main.go` in `builtInCmds`:

```go
	link_re: func(n Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, s ...string) {
		linkCommand(n, c, e, ctx, output, s...)
	},
```

Update `help.go`'s builtin command list the same way `^sessions` etc. are listed (find the list, add a one-line `^link` entry: "get web links to your current session and session history").

- [ ] **Step 4: Run tests**

Run: `go test -run TestLinkCommand . -v && go test .`
Expected: PASS.

- [ ] **Step 5: fmt/vet + commit**

```bash
go fmt ./... && go vet ./...
git add website.go website_test.go main.go help.go notices.go config/notices.toml
git commit -m "feat: ^link — hand out session-site URLs in channel"
```

---

### Task 5: sesssite — scaffold, config, read-only DB open, models, queries

**Files:**
- Create: `sesssite/main.go`, `sesssite/config.go`, `sesssite/config_test.go`, `sesssite/db.go`, `sesssite/db_test.go`, `sesssite/models.go`, `sesssite/queries.go`, `sesssite/queries_test.go`, `sesssite/example.toml`, `sesssite/testhelpers_test.go`

**Interfaces:**
- Consumes: dave's schema (sessions/users/messages/turn_usage/compactions tables — columns per Task 1).
- Produces (package main in `sesssite/`):
  - `type Config struct { Server ServerConfig; Database DatabaseConfig }`, `ServerConfig{Addr, BaseURL string}`, `DatabaseConfig{Driver, Path, DSN string}`, `loadConfig(path string) (*Config, error)` with defaults (`addr ":8060"`, `driver "sqlite"`, `path "data/dave.db"` relative to binary dir).
  - `openDB(cfg DatabaseConfig) (*gorm.DB, error)` — read-only open.
  - Read models (all with explicit `TableName()`): `Session`, `User`, `Message`, `TurnUsage`, `Compaction` — field names EXACTLY: `Session{ID int64, Slug, Network, Channel, ChatCommand, FirstMessage, Service, Model, Status string, CreatedAt, LastActive time.Time, DeletedAt gorm.DeletedAt, UserID *int64}`, `User{ID int64, Slug, Network, CurrentNick, IRCAccount string, CreatedAt, UpdatedAt time.Time}`, `Message{ID, SessionID int64, Role, Content string, ToolCalls, ToolCallID, ReasoningContent, ReasoningSummary, MultiContent *string, Archived bool, CompactionID, SourceCompactionID *int64, Superseded bool, CreatedAt time.Time}`, `TurnUsage{ID, SessionID int64, Model, Service, ReasoningEffort, FinishReason, APIPath string, PromptTokens, CompletionTokens, CachedTokens, ReasoningTokens, DurationMs int, CreatedAt time.Time}`, `Compaction{ID, SessionID, SummaryMessageID, FirstArchivedID, LastArchivedID, ArchivedCount int64, Service, Model, Trigger string, PromptTokens, CompletionTokens, DurationMs int, CreatedAt time.Time}`.
  - Queries: `sessionBySlug(db, slug) (*Session, error)` (gorm.ErrRecordNotFound passthrough), `userBySlug(db, slug) (*User, error)`, `sessionMessagesPage(db, sessionID int64, beforeID int64, limit int) ([]Message, error)` — `superseded = false AND source_compaction_id IS NULL` (+ `id < beforeID` when beforeID > 0), `ORDER BY id DESC LIMIT ?` returned ASC-ending, `sessionMessageByID(db, sessionID, messageID) (*Message, error)`, `userSessionsPage(db, userID int64, beforeLastActive time.Time, beforeID int64, limit int) ([]Session, error)` — keyset `(last_active, id) DESC`, ASC-ending, `sessionUsageRows(db, sessionID) ([]TurnUsage, error)` (ordered by created_at ASC, id ASC), `sessionCompactions(db, sessionID) ([]Compaction, error)` (ordered by id ASC).

- [ ] **Step 1: Write failing tests**

`sesssite/testhelpers_test.go`:

```go
package main

import (
	"path/filepath"
	"testing"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// newTestDB creates a schema compatible with the columns sesssite reads.
// sesssite NEVER AutoMigrates in production (read-only contract); tests
// are the only place the models create tables.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")+"?_pragma=journal_mode(WAL)"), &gorm.Config{
		Logger: newGormLogger(logxi.New("test")),
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
```

(If dave root has a `newGormLogger` you cannot reach from another package, copy the small wrapper into `sesssite/db.go`.)

`sesssite/db_test.go`:

```go
package main

import (
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestOpenDBSQLiteReadOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dave.db")

	// Create a writable dave-style DB, then open it read-only.
	w, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, w.AutoMigrate(&Session{}, &User{}))
	require.NoError(t, w.Create(&Session{Slug: "aaaaaaaa", Network: "n", Channel: "#c", ChatCommand: "d", Status: "active"}).Error)
	sqlDB, _ := w.DB()
	sqlDB.Close()

	ro, err := openDB(DatabaseConfig{Driver: "sqlite", Path: path})
	require.NoError(t, err)

	var s Session
	require.NoError(t, ro.Where("slug = ?", "aaaaaaaa").First(&s).Error)
	assert.Equal(t, "n", s.Network)

	// Writes through the read-only handle must fail.
	err = ro.Exec("UPDATE sessions SET status = 'x'").Error
	assert.Error(t, err, "read-only handle must reject writes")
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(filepath.Join(t.TempDir(), "missing.toml"))
	require.NoError(t, err)
	assert.Equal(t, ":8060", cfg.Server.Addr)
	assert.Equal(t, "sqlite", cfg.Database.Driver)
}
```

`sesssite/queries_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./sesssite/`
Expected: FAIL — package doesn't build (missing funcs).

- [ ] **Step 3: Implement**

`sesssite/config.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config mirrors the reference block in example.toml (repo convention).
type Config struct {
	Server   ServerConfig   `toml:"server"`
	Database DatabaseConfig `toml:"database"`
}

type ServerConfig struct {
	Addr    string `toml:"addr"`
	BaseURL string `toml:"base_url"`
}

type DatabaseConfig struct {
	Driver string `toml:"driver"` // "sqlite" | "postgres"
	Path   string `toml:"path"`   // sqlite
	DSN    string `toml:"dsn"`    // postgres
}

func (c *Config) setDefaults() {
	if c.Server.Addr == "" {
		c.Server.Addr = ":8060"
	}
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.Path == "" {
		c.Database.Path = "data/dave.db"
	}
}

func loadConfig(path string) (*Config, error) {
	var cfg Config
	if _, err := os.Stat(path); err == nil {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", path, err)
		}
	}
	cfg.setDefaults()
	return &cfg, nil
}

// resolvePath resolves a config-relative path against the binary dir
// (imgsite/main.go precedent: config and data live next to the binary).
func resolvePath(binaryDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(binaryDir, p)
}
```

`sesssite/models.go` — the read models exactly as in Interfaces, with:

```go
package main

import (
	"time"

	"gorm.io/gorm"
)

// Read-only mirrors of dave's tables. sesssite never writes and never
// AutoMigrates outside tests. Column sets are the subset the site reads.
type Session struct { /* fields per Interfaces block */ }

func (Session) TableName() string    { return "sessions" }
func (User) TableName() string       { return "users" }
func (Message) TableName() string    { return "messages" }
func (TurnUsage) TableName() string  { return "turn_usage" }
func (Compaction) TableName() string { return "compactions" }
```

(Fill the structs field-for-field from the Interfaces block — tags mirror dave's `gorm:"column:…"` names: `chat_command`, `first_message`, `last_active`, `session_id`, `tool_calls`, `tool_call_id`, `reasoning_content`, `reasoning_summary`, `multi_content`, `archived`, `compaction_id`, `source_compaction_id`, `superseded`, `summary_message_id`, `first_archived_id`, `last_archived_id`, `archived_count`, `prompt_tokens`, `completion_tokens`, `cached_tokens`, `reasoning_tokens`, `duration_ms`, `finish_reason`, `api_path`.)

`sesssite/db.go`:

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	logxi "github.com/mgutz/logxi/v1"
)

// openDB opens dave's database READ-ONLY. sqlite: mode=ro DSN (WAL readers
// work when dave is running and keeps -shm alive; if the ro open fails
// because dave is stopped and no -wal/-shm exists, fall back to a normal
// open with PRAGMA query_only=1 — SQL-level write rejection, which still
// honors the read-only contract). postgres: the DSN should belong to a
// read-only role; the code never issues writes.
func openDB(cfg DatabaseConfig) (*gorm.DB, error) {
	switch cfg.Driver {
	case "postgres":
		db, err := gorm.Open(postgres.Open(cfg.DSN), &gorm.Config{Logger: newGormLogger(loggerDB)})
		if err != nil {
			return nil, fmt.Errorf("opening postgres: %w", err)
		}
		return db, nil
	default:
		abs, err := filepath.Abs(cfg.Path)
		if err != nil {
			return nil, fmt.Errorf("resolving db path: %w", err)
		}
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("database %s: %w", abs, err)
		}
		dsn := "file:" + abs + "?mode=ro&_pragma=busy_timeout(5000)"
		db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: newGormLogger(loggerDB)})
		if err == nil {
			return db, nil
		}
		dsn = "file:" + abs + "?_pragma=busy_timeout(5000)&_pragma=query_only(1)"
		db, err = gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: newGormLogger(loggerDB)})
		if err != nil {
			return nil, fmt.Errorf("opening sqlite read-only: %w", err)
		}
		return db, nil
	}
}
```

`newGormLogger` — copy dave's `dbLogger.go` wrapper (it's ~40 lines; copy the file to `sesssite/dbLogger.go`, change package, keep behavior) plus `loggerDB = newLogger("db")` with `SetLevel(logxi.LevelAll)`.

`sesssite/queries.go`:

```go
package main

import (
	"time"

	"gorm.io/gorm"
)

func sessionBySlug(db *gorm.DB, slug string) (*Session, error) {
	var s Session
	if err := db.Where("slug = ?", slug).First(&s).Error; err != nil {
		return nil, err
	}
	return &s, nil
}

func userBySlug(db *gorm.DB, slug string) (*User, error) {
	var u User
	if err := db.Where("slug = ?", slug).First(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// sessionMessagesPage returns up to limit messages ending at beforeID
// (exclusive; 0 = newest), oldest-first. Tail-copies (source_compaction_id
// set — compaction re-inserted rows whose content duplicates the archived
// originals) and superseded rows are excluded: the flat transcript shows
// each message exactly once (spec).
func sessionMessagesPage(db *gorm.DB, sessionID int64, beforeID int64, limit int) ([]Message, error) {
	q := db.Where("session_id = ? AND superseded = false AND source_compaction_id IS NULL", sessionID)
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}
	var msgs []Message
	if err := q.Order("id DESC").Limit(limit).Find(&msgs).Error; err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

func sessionMessageByID(db *gorm.DB, sessionID, messageID int64) (*Message, error) {
	var m Message
	if err := db.Where("session_id = ? AND id = ?", sessionID, messageID).First(&m).Error; err != nil {
		return nil, err
	}
	return &m, nil
}

// userSessionsPage keyset-paginates by (last_active DESC, id DESC),
// oldest-first result order for rendering.
func userSessionsPage(db *gorm.DB, userID int64, beforeLastActive time.Time, beforeID int64, limit int) ([]Session, error) {
	q := db.Where("user_id = ?", userID)
	if !beforeLastActive.IsZero() {
		q = q.Where("(last_active, id) < (?, ?)", beforeLastActive, beforeID)
	}
	var sessions []Session
	if err := q.Order("last_active DESC, id DESC").Limit(limit).Find(&sessions).Error; err != nil {
		return nil, err
	}
	for i, j := 0, len(sessions)-1; i < j; i, j = i+1, j-1 {
		sessions[i], sessions[j] = sessions[j], sessions[i]
	}
	return sessions, nil
}

func sessionUsageRows(db *gorm.DB, sessionID int64) ([]TurnUsage, error) {
	var rows []TurnUsage
	err := db.Where("session_id = ?", sessionID).Order("created_at ASC, id ASC").Find(&rows).Error
	return rows, err
}

func sessionCompactions(db *gorm.DB, sessionID int64) ([]Compaction, error) {
	var rows []Compaction
	err := db.Where("session_id = ?", sessionID).Order("id ASC").Find(&rows).Error
	return rows, err
}
```

Note the sqlite tuple compare `(last_active, id) < (?, ?)` — sqlite supports row values since 3.15, postgres natively; modernc (pure-Go) is well past that.

`sesssite/main.go`:

```go
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
```

`NewServer`/`Run` arrive in Task 8 — for THIS task add a minimal placeholder-free stub in `main.go` itself that serves nothing yet but exists so the package builds:

```go
type server struct{ db dbType; cfg *Config }

func NewServer(db *gorm.DB, cfg *Config) *server { return &server{db: db, cfg: cfg} }
func (s *server) Run(addr string) error {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	return http.ListenAndServe(addr, nil)
}
```

with `dbType = *gorm.DB` (define `type dbType = *gorm.DB` above). Task 8 replaces this stub wholesale with `server.go`.

`sesssite/example.toml` — reference + commented example per convention:

```toml
# sesssite config — session-history website for dave
#
# Reference:
#   [server] addr     string  listen address (default ":8060")
#   [server] base_url string  external base URL for absolute links/OG tags
#   [database] driver string  "sqlite" (default) | "postgres"
#   [database] path   string  sqlite path, binary-relative (default "data/dave.db")
#   [database] dsn    string  postgres DSN — use a READ-ONLY role; the code never writes
#
# [server]
# addr = ":8060"
# base_url = "https://sessions.example.com"
#
# [database]
# driver = "sqlite"
# path = "data/dave.db"
# # driver = "postgres"
# # dsn = "postgres://sesssite_ro:pass@localhost:5432/dave?sslmode=disable"
```

- [ ] **Step 4: Run tests**

Run: `go test ./sesssite/ -v`
Expected: PASS. Run: `go build ./...` Expected: OK.

- [ ] **Step 5: fmt/vet + commit**

```bash
go fmt ./... && go vet ./...
git add sesssite/
git commit -m "feat(sesssite): scaffold — config, read-only DB open, models, queries"
```

---

### Task 6: sesssite — port the markdown+IRC renderer

**Files:**
- Create: `sesssite/renderer/markdown.go`, `sesssite/renderer/irc.go`, `sesssite/renderer/markdown_test.go`, `sesssite/renderer/irc_test.go`
- Modify: `go.mod`/`go.sum` (one new dep)

**Interfaces:**
- Produces: package `renderer` with `Render(source string) (string, error)` and `IRCExtension` — behaviorally identical to `~/markdown-parsebin/internal/renderer`.

- [ ] **Step 1: Port the files**

Copy from `/home/knivey/markdown-parsebin/internal/renderer/`: `markdown.go`, `irc.go`, `markdown_test.go`, `irc_test.go` into `sesssite/renderer/`. Apply exactly these edits:

1. `markdown.go` header comment:

```go
// Package renderer converts stored message markdown to HTML for the
// session-history site. Ported from markdown-parsebin (dave-web), MIT,
// (c) dave project — same rendering stack the pastebin used: GFM,
// Chroma/Dracula highlighting, linkify, and the IRC control-code
// transformer below (IRC clients embed \x02/\x03/… codes in user text;
// the transformer maps them to styled spans).
```

2. `markdown.go`: rename the package-private symbol `md` stays fine; expose an init-safe `Render` as-is. Change `html.WithXHTML()` stays. No functional edits.
3. `irc.go`: unchanged except the package doc comment:

```go
// irc.go — goldmark AST transformer mapping IRC formatting control codes
// (\x02 bold, \x1d italic, \x1f underline, \x1e strike, \x11 monospace,
// \x16 reverse, \x0f reset, \x03 fg[,bg] 0-99, \x04 hex) to HTML spans.
// fg==bg renders as a "spoiler" class. Ported from markdown-parsebin.
```

4. Tests: keep them whole — they are the port's acceptance proof.

- [ ] **Step 2: Add the dependency**

Run: `go get github.com/yuin/goldmark-highlighting/v2@latest && go mod tidy`
Expected: go.mod gains exactly `github.com/yuin/goldmark-highlighting/v2` (chroma + goldmark already present).

- [ ] **Step 3: Run ported tests**

Run: `go test ./sesssite/renderer/ -v`
Expected: PASS — all ported markdown + IRC cases green unchanged.

- [ ] **Step 4: fmt/vet + commit**

```bash
go fmt ./... && go vet ./...
git add sesssite/renderer go.mod go.sum
git commit -m "feat(sesssite): port markdown+IRC renderer from markdown-parsebin"
```

---

### Task 7: sesssite — message view-models (row rendering core)

**Files:**
- Create: `sesssite/render.go`, `sesssite/render_test.go`
- Create: `sesssite/templates/rows.html` (the message-row partial)

**Interfaces:**
- Consumes: `renderer.Render` (Task 6), models + `sessionMessagesPage`/`sessionCompactions`/`sessionUsageRows` (Task 5).
- Produces:
  - `type MessageView struct { ID int64; Role string; Time time.Time; ContentHTML template.HTML; ToolCalls []ToolCallView; ToolCallID string; HasReasoning bool; ReasoningContent, ReasoningSummary string; ImageParts []string; Archived bool; RawURL string; Usage *UsageView }`
  - `type ToolCallView struct { ID, Name string; ArgsJSON string }`
  - `type UsageView struct { Model, Service string; PromptTokens, CompletionTokens, CachedTokens, ReasoningTokens, DurationMs int; FinishReason, APIPath string }`
  - `type CompactionView struct { Compaction; SummaryHTML template.HTML }`
  - `buildMessageView(m Message, sessionSlug string) MessageView`
  - `buildMessageViews(msgs []Message, sessionSlug string) []MessageView`
  - `assignUsage(views []MessageView, msgs []Message, usage []TurnUsage)` — time-window join `(prev assistant msg, this assistant msg]`, sets `views[i].Usage` for assistant rows (last row in window wins if several; nil when none — line omitted).
  - `renderRowsHTML(views []MessageView, comps []CompactionView, opts rowsOpts) (template.HTML, error)` — executes `rows.html` with dividers interleaved before the first archived row of each compaction range.

- [ ] **Step 1: Write failing tests**

`sesssite/render_test.go`:

```go
package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildMessageViewMarkdown(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "**bold** and `code`")

	v := buildMessageView(m, s.Slug)
	assert.Contains(t, string(v.ContentHTML), "<strong>bold</strong>")
	assert.Contains(t, string(v.ContentHTML), "<code>code</code>")
	assert.Equal(t, "/s/aaaaaaaa/m/"+itoa(m.ID)+"/raw", v.RawURL)
}

func TestBuildMessageViewIRCCodes(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "user", "\x02bold text\x02 plain")

	v := buildMessageView(m, s.Slug)
	assert.Contains(t, string(v.ContentHTML), `class="irc-bold"`)
	assert.NotContains(t, string(v.ContentHTML), "\x02")
}

func TestBuildMessageViewToolCalls(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "", func(msg *Message) {
		tc := `[{"id":"call_1","function":{"name":"generate_image","arguments":"{\"prompt\":\"a shrew\"}"}}]`
		msg.ToolCalls = &tc
	})
	v := buildMessageView(m, s.Slug)
	require.Len(t, v.ToolCalls, 1)
	assert.Equal(t, "generate_image", v.ToolCalls[0].Name)
	assert.Contains(t, v.ToolCalls[0].ArgsJSON, "prompt")
}

func TestBuildMessageViewReasoningCollapsed(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "answer", func(msg *Message) {
		r := "chain of thought"; msg.ReasoningContent = &r
		sum := "short"; msg.ReasoningSummary = &sum
	})
	v := buildMessageView(m, s.Slug)
	assert.True(t, v.HasReasoning)
	assert.Equal(t, "chain of thought", v.ReasoningContent)
}

func TestBuildMessageViewImages(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "user", "", func(msg *Message) {
		mc := `[{"type":"image_url","image_url":{"url":"https://img.example.com/x.png"}},{"type":"text","text":"look"}]`
		msg.MultiContent = &mc
	})
	v := buildMessageView(m, s.Slug)
	require.Len(t, v.ImageParts, 1)
	assert.Equal(t, "https://img.example.com/x.png", v.ImageParts[0])
	// Multicontent text is rendered too — content is empty, parts carry it.
	assert.Contains(t, string(v.ContentHTML), "look")
}

func TestAssignUsageWindow(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	base := time.Now()
	u1 := mkMessage(t, db, s.ID, "user", "q1", func(m *Message) { m.CreatedAt = base })
	a1 := mkMessage(t, db, s.ID, "assistant", "r1", func(m *Message) { m.CreatedAt = base.Add(2 * time.Second) })
	u2 := mkMessage(t, db, s.ID, "user", "q2", func(m *Message) { m.CreatedAt = base.Add(3 * time.Second) })
	a2 := mkMessage(t, db, s.ID, "assistant", "r2", func(m *Message) { m.CreatedAt = base.Add(5 * time.Second) })
	msgs := []Message{u1, a1, u2, a2}

	usage := []TurnUsage{
		{SessionID: s.ID, Model: "m1", PromptTokens: 10, CompletionTokens: 5, CreatedAt: base.Add(1 * time.Second)},
		{SessionID: s.ID, Model: "m2", PromptTokens: 20, CompletionTokens: 8, CreatedAt: base.Add(4 * time.Second)},
		{SessionID: s.ID, Model: "m2", PromptTokens: 30, CompletionTokens: 9, CreatedAt: base.Add(4 * time.Second)},
	}
	views := buildMessageViews(msgs, s.Slug)
	assignUsage(views, msgs, usage)
	require.NotNil(t, views[1].Usage)
	assert.Equal(t, "m1", views[1].Usage.Model)
	require.NotNil(t, views[3].Usage)
	assert.Equal(t, "m2", views[3].Usage.Model)
	assert.Equal(t, 50, views[3].Usage.PromptTokens) // summed across the window's rows
	assert.Nil(t, views[0].Usage)
}

func TestRenderRowsCompactionDivider(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m1 := mkMessage(t, db, s.ID, "user", "old", func(m *Message) { m.CreatedAt = time.Now().Add(-time.Hour) })
	m2 := mkMessage(t, db, s.ID, "user", "new")
	comp := Compaction{SessionID: s.ID, SummaryMessageID: m1.ID, FirstArchivedID: m1.ID,
		LastArchivedID: m1.ID, ArchivedCount: 1, Trigger: "auto", Model: "m", CreatedAt: time.Now()}
	require.NoError(t, db.Create(&comp).Error)

	views := buildMessageViews([]Message{m1, m2}, s.Slug)
	comps := []CompactionView{{Compaction: comp}}
	html, err := renderRowsHTML(views, comps)
	require.NoError(t, err)
	assert.Contains(t, string(html), "compaction-divider")
	assert.Contains(t, string(html), "archived") // archived tag on m1
}

func itoa(i int64) string { return time.Time{}.AppendFormat(nil, "")[:0].String() + fmtInt(i) }
```

(If `fmtInt` doesn't exist here, use `strconv.FormatInt` — define `func fmtInt(i int64) string { return strconv.FormatInt(i, 10) }` in render_test.go and drop the silly itoa helper.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./sesssite/ -run TestBuild|TestAssign|TestRenderRows`
Expected: FAIL — undefined symbols.

- [ ] **Step 3: Implement**

`sesssite/render.go`:

```go
package main

import (
	"encoding/json"
	"html/template"
	"strings"
	"time"

	"github.com/knivey/dave/sesssite/renderer"
	"gorm.io/gorm"
)

type ToolCallView struct {
	ID       string
	Name     string
	ArgsJSON string
}

type UsageView struct {
	Model, Service, FinishReason, APIPath string
	PromptTokens, CompletionTokens        int
	CachedTokens, ReasoningTokens         int
	DurationMs                            int
}

type MessageView struct {
	ID                  int64
	Role                string
	Time                time.Time
	ContentHTML         template.HTML
	ToolCalls           []ToolCallView
	ToolCallID          string
	HasReasoning        bool
	ReasoningContent    string
	ReasoningSummary    string
	ImageParts          []string
	Archived            bool
	RawURL              string
	Usage               *UsageView
	IsSystem            bool
}

type CompactionView struct {
	Compaction
	SummaryHTML template.HTML
}

type messagePart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

type toolCallJSON struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// buildMessageView converts one stored row into a render-ready view.
// User/assistant content renders as markdown (IRC codes styled by the
// ported transformer); tool results render escaped in <pre> via the
// template (no markdown); system rows are template-collapsed.
func buildMessageView(m Message, sessionSlug string) MessageView {
	v := MessageView{
		ID: m.ID, Role: m.Role, Time: m.CreatedAt,
		Archived: m.Archived,
		RawURL:   "/s/" + sessionSlug + "/m/" + fmtInt(m.ID) + "/raw",
		IsSystem: m.Role == "system",
	}
	if m.ReasoningContent != nil && *m.ReasoningContent != "" {
		v.HasReasoning = true
		v.ReasoningContent = *m.ReasoningContent
		if m.ReasoningSummary != nil {
			v.ReasoningSummary = *m.ReasoningSummary
		}
	} else if m.ReasoningSummary != nil && *m.ReasoningSummary != "" {
		v.HasReasoning = true
		v.ReasoningSummary = *m.ReasoningSummary
	}
	if m.ToolCallID != nil {
		v.ToolCallID = *m.ToolCallID
	}
	if m.ToolCalls != nil {
		var calls []toolCallJSON
		if json.Unmarshal([]byte(*m.ToolCalls), &calls) == nil {
			for _, c := range calls {
				v.ToolCalls = append(v.ToolCalls, ToolCallView{ID: c.ID, Name: c.Function.Name, ArgsJSON: prettyJSON(c.Function.Arguments)})
			}
		}
	}
	var textParts []string
	if m.MultiContent != nil {
		var parts []messagePart
		if json.Unmarshal([]byte(*m.MultiContent), &parts) == nil {
			for _, p := range parts {
				switch {
				case p.Type == "image_url" && p.ImageURL != nil:
					v.ImageParts = append(v.ImageParts, p.ImageURL.URL)
				case p.Type == "text" && p.Text != "":
					textParts = append(textParts, p.Text)
				}
			}
		}
	}
	content := m.Content
	if content == "" && len(textParts) > 0 {
		content = strings.Join(textParts, "\n\n")
	}
	if content != "" && (m.Role == "user" || m.Role == "assistant") {
		if html, err := renderer.Render(content); err == nil {
			v.ContentHTML = template.HTML(html)
		} else {
			// Degradation: escaped plain text — content is never lost.
			v.ContentHTML = template.HTML(template.HTMLEscapeString(content))
		}
	}
	return v
}
```

Add `prettyJSON(s string) string` (indent-or-verbatim: `json.Indent`, fall back to the original string), `fmtInt`, `buildMessageViews` (loop), and:

```go
// assignUsage joins turn_usage rows to assistant messages by time window:
// rows in (previous assistant message, this assistant message] belong to
// this message. Multiple rows (tool-loop iterations) sum; the LAST row's
// model/service/finish/API path win (they describe the final call).
// Display-only, approximate by construction (usage rows carry no message
// id) — the spec labels it as such.
func assignUsage(views []MessageView, msgs []Message, usage []TurnUsage) {
	ui := 0
	prev := time.Time{}
	for i := range views {
		if msgs[i].Role != "assistant" {
			continue
		}
		uv := &UsageView{}
		seen := false
		for ui < len(usage) && !usage[ui].CreatedAt.After(msgs[i].CreatedAt) {
			if usage[ui].CreatedAt.After(prev) {
				uv.PromptTokens += usage[ui].PromptTokens
				uv.CompletionTokens += usage[ui].CompletionTokens
				uv.CachedTokens += usage[ui].CachedTokens
				uv.ReasoningTokens += usage[ui].ReasoningTokens
				uv.DurationMs += usage[ui].DurationMs
				uv.Model, uv.Service = usage[ui].Model, usage[ui].Service
				uv.FinishReason, uv.APIPath = usage[ui].FinishReason, usage[ui].APIPath
				seen = true
			}
			ui++
		}
		if seen {
			views[i].Usage = uv
		}
		prev = msgs[i].CreatedAt
	}
}
```

`renderRowsHTML` — parse `templates/rows.html` once (package-level `var rowsTpl = template.Must(template.ParseFS(templatesFS, "templates/rows.html"))`; `templatesFS` embed lands in Task 8 — for THIS task embed it from `render.go`):

```go
//go:embed templates
var templatesFS embed.FS
```

`sesssite/templates/rows.html`:

```html
{{range $i, $v := .Views}}
	{{if and $.Comps (index $.CompBefore $i)}}{{range $c := index $.CompBefore $i}}
	<div class="compaction-divider" id="c{{.ID}}">
		<span class="tag">🗜 compacted</span>
		{{.Trigger}} · {{.Model}} · {{.ArchivedCount}} messages · {{.PromptTokens}}→{{.CompletionTokens}} tok
		{{if .SummaryHTML}}<details class="compaction-summary"><summary>summary</summary>{{.SummaryHTML}}</details>{{end}}
	</div>
	{{end}}{{end}}
	<div class="msg role-{{$v.Role}}{{if $v.Archived}} archived{{end}}" id="m{{$v.ID}}">
		<div class="meta">
			<span class="role">{{$v.Role}}</span>
			<time title="{{$v.Time.Format "2006-01-02 15:04:05"}}">{{$v.Time.Format "15:04:05"}}</time>
			{{if $v.Archived}}<span class="tag">archived</span>{{end}}
			<a class="rawlink" href="{{$v.RawURL}}">raw</a>
		</div>
		{{if $v.IsSystem}}
			<details class="system"><summary>system prompt</summary><div class="content md">{{$v.ContentHTML}}</div></details>
		{{else if eq $v.Role "tool"}}
			<details class="toolresult"><summary>tool result · {{$v.ToolCallID}}</summary><pre class="content">{{$v.Content}}</pre></details>
		{{else}}
			<div class="content md">{{$v.ContentHTML}}</div>
		{{end}}
		{{range $v.ImageParts}}<img class="chatimg" src="{{.}}" alt="conversation image" loading="lazy">{{end}}
		{{range $v.ToolCalls}}
			<div class="toolcall"><span class="name">{{.Name}}</span><pre class="args">{{.ArgsJSON}}</pre></div>
		{{end}}
		{{if $v.HasReasoning}}
			<details class="reasoning"><summary>reasoning</summary>
				{{if $v.ReasoningSummary}}<div class="summary md">summary: {{$v.ReasoningSummary}}</div>{{end}}
				{{if $v.ReasoningContent}}<div class="content md">{{$v.ReasoningContent}}</div>{{end}}
			</details>
		{{end}}
		{{if $v.Usage}}
			<details class="usage"><summary>{{$v.Usage.Model}} · {{$v.Usage.PromptTokens}}→{{$v.Usage.CompletionTokens}} tok · {{$v.Usage.DurationMs}}ms</summary>
				cached {{$v.Usage.CachedTokens}} · reasoning {{$v.Usage.ReasoningTokens}} · {{$v.Usage.Service}} · {{$v.Usage.FinishReason}} · {{$v.Usage.APIPath}}
			</details>
		{{end}}
	</div>
{{end}}
```

`renderRowsHTML(views, comps)` builds: for each view index, which compactions have `LastArchivedID < msgs[i].ID && SummaryMessageID… — simplest correct rule: a divider for compaction C renders immediately BEFORE the first message with `ID > C.LastArchivedID` (that's the post-compaction boundary — the summary row itself is a system message inside the archived range and renders on its own). Compute `CompBefore map[int][]CompactionView` in Go and pass `{Views, CompBefore}`; SummaryHTML renders the summary system message's content via `renderer.Render` when it's in the page (lookup by SummaryMessageID among msgs; else empty). Note: reasoning content inside details is plain-text rendered via the template (escaped) — markdown rendering of reasoning is unnecessary; keep `{{$v.ReasoningContent}}` escaped (fix the `.content md` class there — use `<pre class="content">` instead; adjust the test if it checks classes, it doesn't).

- [ ] **Step 4: Run tests**

Run: `go test ./sesssite/ -v`
Expected: PASS.

- [ ] **Step 5: fmt/vet + commit**

```bash
go fmt ./... && go vet ./...
git add sesssite/render.go sesssite/render_test.go sesssite/templates
git commit -m "feat(sesssite): message view rendering — markdown, IRC codes, tool cards, reasoning, usage, compaction dividers"
```

---

### Task 8: sesssite — HTTP server: session page, pagination, raw endpoint, landing, 404s

**Files:**
- Create: `sesssite/server.go` (replaces main.go's stub), `sesssite/pages.go`, `sesssite/pages_test.go`, `sesssite/slug.go` (validSlug copy), `sesssite/static/style.css`, `sesssite/templates/base.html`, `sesssite/templates/session.html`, `sesssite/templates/landing.html`
- Modify: `sesssite/main.go` (drop stub), delete stub parts
- Modify: `sesssite/render.go` if the embed moves to `web.go` — keep it where it is; `server.go` imports it

**Interfaces:**
- Consumes: Tasks 5–7 products.
- Produces: `NewServer(db *gorm.DB, cfg *Config) *Server`, `(*Server).Run(addr string) error`; routes:
  - `GET /` landing
  - `GET /s/{slug}` session page (HTML; `?fragment=rows&before={id}` older-rows fragment)
  - `GET /s/{slug}/m/{id}/raw` raw message
  - `GET /static/` embedded assets
  - (Task 10 adds `/s/{slug}/events`, `/u/{slug}/events`; Task 9 adds `/u/{slug}`.)
- Security headers on every response: `Content-Security-Policy: default-src 'none'; img-src * data:; style-src 'self'; script-src 'self'; connect-src 'self'`, `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`.

- [ ] **Step 1: Write failing tests**

`sesssite/pages_test.go`:

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPageServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	db := newTestDB(t)
	srv := NewServer(db, &Config{Server: ServerConfig{BaseURL: "https://s.example.com"}})
	ts := httptest.NewServer(srv.mux())
	t.Cleanup(ts.Close)
	return srv, ts
}

func TestSessionPageRenders(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa", func(x *Session) { x.UserID = ptrInt64(1) })
	mkMessage(t, db, s.ID, "user", "hello **world**")
	m := mkMessage(t, db, s.ID, "assistant", "hi there")

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "hello <strong>world</strong>")
	assert.Contains(t, body, `id="m`)
	assert.Contains(t, body, "/s/aaaaaaaa/m/"+fmtInt(m.ID)+"/raw")
}

func TestSlugValidation(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	for _, slug := range []string{"short", "toolongslug1", "with/slash", "SYMBOLS!", "aaaaaaaa ", ""} {
		resp, err := http.Get(ts.URL + "/s/" + slug)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, 404, resp.StatusCode, "slug %q", slug)
	}
}

func TestSessionPageSoftDeleted404(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	require.NoError(t, db.Delete(&Session{}, s.ID).Error) // soft delete

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
}

func TestSessionPageTailCopiesNotDoublyRendered(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	mkMessage(t, db, s.ID, "user", "unique-text-xyz")
	mkMessage(t, db, s.ID, "user", "unique-text-xyz", func(m *Message) { c := int64(9); m.SourceCompactionID = &c })

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 1, strings.Count(readBody(t, resp), "unique-text-xyz"))
}

func TestRawEndpointByteForByte(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	stored := "\x02bold\x02 **not strong** `code` http://x.example/"
	m := mkMessage(t, db, s.ID, "user", stored)

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/m/" + fmtInt(m.ID) + "/raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
	assert.Equal(t, stored, readBody(t, resp)) // byte-for-byte, IRC codes intact
}

func TestRawEndpointWrongSession404(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s1 := mkSession(t, db, "aaaaaaaa")
	s2 := mkSession(t, db, "bbbbbbbb")
	m := mkMessage(t, db, s2.ID, "user", "x")
	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/m/" + fmtInt(m.ID) + "/raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
}

func TestOlderRowsFragment(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	for i := 0; i < 5; i++ {
		mkMessage(t, db, s.ID, "user", "row")
	}
	resp, err := http.Get(ts.URL + "/s/aaaaaaaa?fragment=rows&before=4")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, `id="m3"`)
	assert.NotContains(t, body, `id="m4"`)
}

func TestLanding(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), "^link")
}
```

Helpers `readBody(t, resp) string`, `ptrInt64(i int64) *int64` into `testhelpers_test.go`.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./sesssite/ -run TestSession|TestSlug|TestRaw|TestOlder|TestLanding`
Expected: FAIL — no Server/mux.

- [ ] **Step 3: Implement**

`sesssite/slug.go` — copy of dave's `validSlug` (package main, same body; comment noting the mirror).

`sesssite/server.go`:

```go
package main

import (
	"net/http"
	"strings"

	"gorm.io/gorm"
)

type Server struct {
	db  *gorm.DB
	cfg *Config
}

func NewServer(db *gorm.DB, cfg *Config) *Server { return &Server{db: db, cfg: cfg} }

func (s *Server) Run(addr string) error { return http.ListenAndServe(addr, s.mux()) }

func (s *Server) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /", s.handleLanding)
	m.HandleFunc("GET /static/", s.handleStatic)
	m.HandleFunc("GET /s/{slug}", s.handleSessionPage)
	m.HandleFunc("GET /s/{slug}/m/{id}/raw", s.handleRawMessage)
	return m
}

// secure wraps handlers with the site-wide headers (spec: CSP etc.).
func secure(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'none'; img-src * data:; style-src 'self'; script-src 'self'; connect-src 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next(w, r)
	}
}
```

(Go 1.22+ pattern routing `{slug}`/`{id}` is available on 1.25 — use it; wrap every handler registration in `secure(...)`.)

`sesssite/pages.go` — handlers:

- `handleLanding`: renders `landing.html` (one paragraph + the `^link` hint).
- `handleSessionPage`:
  1. `slug := r.PathValue("slug")`; `if !validSlug(slug) { 404 }`
  2. `fragment=rows` query → `before` id: fetch page of 200 via `sessionMessagesPage`, render rows fragment (`renderRowsHTML` with comps whose range intersects), write `Cache-Control: no-store`.
  3. full page: session by slug (404 on not-found — GORM's soft-delete filter applies), `userID` header link, latest page of messages (`sessionMessagesPage(db, id, 0, 200)`), `hasOlder = len==200 && older exist` (probe `before=msgs[0].ID` limit 1), usage rows + `assignUsage`, compactions → `CompactionView`s, render `session.html` with `{Session, Rows template.HTML, HasOlder, OldestID, BaseURL}`; `Cache-Control: no-store`.
- `handleRawMessage`: slug + `id` (validate `strconv.ParseInt`, 404 malformed); `sessionMessageByID` (scoped to session — cross-session ids 404); body: for `MultiContent != nil && Content == ""` write the raw JSON string, for `ToolCalls != nil && Content == ""` write the tool-calls JSON, else `Content` — verbatim bytes, `text/plain; charset=utf-8`, nosniff (set by secure), `Cache-Control: no-store`.
- `handleStatic`: `http.FileServer(http.FS(staticFS))` restricted under `/static/` prefix strip.

`sesssite/templates/base.html` — html skeleton, dark theme, `<link rel="stylesheet" href="/static/style.css">`, `{{template "content" .}}` pattern; `session.html` defines "content": header block (network/channel/command/service/model/status badges/dates/usage-summary `<details>` built from a `usageByModel` aggregation — compute in the handler with a small GROUP BY via `sessionUsageRows` loop), `{{.Rows}}`, load-older button (`data-oldest`, hidden when !HasOlder), `{{if .Session.Slug}}<script src="/static/session.js" data-slug="{{.Session.Slug}}" defer></script>{{end}}` (Task 10 fills the JS; reference it now so no second touch).

`sesssite/static/style.css` — port the parsebin Dracula palette basics + `.msg`, `.role-user/.role-assistant/.role-system/.role-tool`, `.archived` (dimmed `opacity: .55`), `.compaction-divider` (border-top + tag), `.toolcall/.toolresult pre` styling, `.chatimg { max-width: 100%; }`, `#m<id>:target` flash highlight (`.msg:target { outline: 2px solid …; }`). Keep it lean (~150 lines).

Delete the Task 5 `server` stub from `main.go` (now duplicated by `server.go`).

- [ ] **Step 4: Run tests**

Run: `go test ./sesssite/ -v && go build ./...`
Expected: PASS.

- [ ] **Step 5: fmt/vet + commit**

```bash
go fmt ./... && go vet ./...
git add sesssite/
git commit -m "feat(sesssite): session page, rows fragment pagination, raw endpoint, landing, 404s"
```

---

### Task 9: sesssite — user page

**Files:**
- Create: `sesssite/templates/user.html`
- Modify: `sesssite/pages.go` (handler + fragment), `sesssite/server.go` (route)
- Test: `sesssite/pages_test.go` (append)

**Interfaces:**
- Consumes: `userBySlug`, `userSessionsPage` (Task 5).
- Produces: `GET /u/{slug}` (HTML; `?fragment=list` renders just the first-page list fragment — what user.js refetches on live updates; `?fragment=list&before_last_active=…&before_id=…` renders the next page for load-older).

- [ ] **Step 1: Write failing test**

```go
func TestUserPageListsSessions(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()

	uid := int64(1)
	require.NoError(t, db.Create(&User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "shrew", NormalizedNick: "shrew"}).Error)
	mkSession(t, db, "aaaaaaaa", func(s *Session) { s.UserID = &uid; s.FirstMessage = "tell me about shrews" })
	mkSession(t, db, "bbbbbbbb", func(s *Session) { s.UserID = &uid; s.FirstMessage = "another one" })
	other := int64(2)
	mkSession(t, db, "cccccccc", func(s *Session) { s.UserID = &other })

	resp, err := http.Get(ts.URL + "/u/uuuuuuuu")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	body := readBody(t, resp)
	assert.Contains(t, body, "shrew")
	assert.Contains(t, body, "tell me about shrews")
	assert.Contains(t, body, "/s/aaaaaaaa")
	assert.Contains(t, body, "/s/bbbbbbbb")
	assert.NotContains(t, body, "/s/cccccccc")
}

func TestUserPageFragmentPagination(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	uid := int64(1)
	require.NoError(t, db.Create(&User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "shrew", NormalizedNick: "shrew"}).Error)
	var last *Session
	for i := 0; i < 3; i++ {
		last = mkSession(t, db, "aaaaaaaa"+strings.Repeat("b", i), func(s *Session) { s.UserID = &uid })
	}
	resp, err := http.Get(ts.URL + "/u/uuuuuuuu?fragment=list&before_last_active=" +
		urlQueryTime(last.LastActive) + "&before_id=" + fmtInt(last.ID))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	assert.Contains(t, readBody(t, resp), `/s/`) // the older two rows
}
```

(`urlQueryTime` = `t.UTC().Format(time.RFC3339Nano)` helper in testhelpers.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./sesssite/ -run TestUserPage`
Expected: FAIL — no route.

- [ ] **Step 3: Implement**

Route `m.HandleFunc("GET /u/{slug}", secure(s.handleUserPage))`. Handler: validate slug → `userBySlug` (404) → `userSessionsPage(db, u.ID, zero, 0, 50)` → template `user.html`: header (nick/network/account), rows (command, `network/channel`, first-message preview clamped server-side to ~120 runes + HTML-escaped by template, model, status badge, `last-active` `<time>`), each row `<a href="/s/{{.Slug}}">`; "load older" button carrying `data-last-active`/`data-id` of the last row; fragment mode renders only the list `<tbody>`-equivalent fragment. Preview: `runesClamp(s.FirstMessage, 120)` helper in pages.go. `Cache-Control: no-store`.

- [ ] **Step 4: Run tests**

Run: `go test ./sesssite/ -v`
Expected: PASS.

- [ ] **Step 5: fmt/vet + commit**

```bash
go fmt ./... && go vet ./...
git add sesssite/
git commit -m "feat(sesssite): user page — session list with keyset pagination"
```

---

### Task 10: sesssite — SSE live updates + JS

**Files:**
- Create: `sesssite/events.go`, `sesssite/events_test.go`, `sesssite/static/session.js`, `sesssite/static/user.js`
- Modify: `sesssite/server.go` (routes), `sesssite/templates/session.html`, `sesssite/templates/user.html` (script tags)

**Interfaces:**
- Consumes: queries (Task 5), `renderRowsHTML` (Task 7).
- Produces: `GET /s/{slug}/events?since={id}` — SSE `messages` (data = rendered rows HTML for ids > since), `session` (`{"status":…}`), heartbeat comments; `GET /u/{slug}/events` — SSE `updated` pings; per-IP cap `ssePerIPLimit = 10` (HTTP 503 beyond), poll interval 1s, heartbeat 15s.

- [ ] **Step 1: Write failing test**

`sesssite/events_test.go`:

```go
package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readSSE reads events until one of the wanted names arrives (or timeout).
func readSSE(t *testing.T, body *bufio.Reader, want ...string) (name string, data string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := body.ReadString('\n')
		require.NoError(t, err)
		line = trimCR(line)
		if strings.HasPrefix(line, "event: ") {
			name = strings.TrimPrefix(line, "event: ")
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
			for _, w := range want {
				if name == w {
					return name, data
				}
			}
			data = ""
		}
	}
	t.Fatal("timed out waiting for SSE event " + strings.Join(want, "/"))
	return "", ""
}

func TestSessionEventsNewMessage(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	mkMessage(t, db, s.ID, "user", "first")

	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events?since=0")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	reader := bufio.NewReader(resp.Body)

	m := mkMessage(t, db, s.ID, "assistant", "fresh reply")
	name, data := readSSE(t, reader, "messages")
	assert.Equal(t, "messages", name)
	assert.Contains(t, data, `id="m`+fmtInt(m.ID)+`"`)
	assert.Contains(t, data, "fresh reply")
}

func TestSessionEventsSinceGapFill(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	s := mkSession(t, db, "aaaaaaaa")
	m1 := mkMessage(t, db, s.ID, "user", "before disconnect")
	m2 := mkMessage(t, db, s.ID, "assistant", "while disconnected")

	// Reconnect with since=m1: m2 must still arrive.
	resp, err := http.Get(ts.URL + "/s/aaaaaaaa/events?since=" + fmtInt(m1.ID))
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	name, data := readSSE(t, reader, "messages")
	assert.Equal(t, "messages", name)
	assert.Contains(t, data, "while disconnected")
	assert.NotContains(t, data, "before disconnect")
}

func TestSessionEventsUnknownSlug404(t *testing.T) {
	_, ts := newPageServer(t)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/s/zzzzzzzz/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
}

func TestUserEventsPing(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := httptest.NewServer(srv.mux())
	defer ts.Close()
	require.NoError(t, db.Create(&User{Slug: "uuuuuuuu", Network: "libera", CurrentNick: "shrew", NormalizedNick: "shrew"}).Error)

	resp, err := http.Get(ts.URL + "/u/uuuuuuuu/events")
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)

	uid := int64(1)
	mkSession(t, db, "aaaaaaaa", func(s *Session) { s.UserID = &uid })
	name, _ := readSSE(t, reader, "updated")
	assert.Equal(t, "updated", name)
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./sesssite/ -run TestSessionEvents|TestUserEvents`
Expected: FAIL — no routes.

- [ ] **Step 3: Implement**

`sesssite/events.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	ssePollInterval = time.Second
	sseHeartbeat    = 15 * time.Second
	ssePerIPLimit   = 10
)

var (
	sseMu    sync.Mutex
	sseConns = map[string]int{}
)

func sseIPKey(r *http.Request) string { return r.RemoteAddr } // host:port → host only, strip port

func acquireSSE(r *http.Request) bool {
	host := hostOnly(r.RemoteAddr)
	sseMu.Lock()
	defer sseMu.Unlock()
	if sseConns[host] >= ssePerIPLimit {
		return false
	}
	sseConns[host]++
	return true
}

func releaseSSE(r *http.Request) {
	host := hostOnly(r.RemoteAddr)
	sseMu.Lock()
	defer sseMu.Unlock()
	sseConns[host]--
	if sseConns[host] <= 0 {
		delete(sseConns, host)
	}
}

// handleSessionEvents streams live updates for a session: 'messages'
// (rendered row HTML, ids > since), 'session' (status change), heartbeat
// comments. A 1s poller per connection reads MAX(id)/count/status/
// compaction count — cheap indexed queries; messages land at turn
// granularity anyway (spec accepts this).
func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		http.NotFound(w, r)
		return
	}
	sess, err := sessionBySlug(s.db, slug)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !acquireSSE(r) {
		http.Error(w, "too many event-stream connections", http.StatusServiceUnavailable)
		return
	}
	defer releaseSSE(r)

	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")

	since := int64(0)
	fmt.Sscanf(r.URL.Query().Get("since"), "%d", &since)

	type state struct {
		maxID  int64
		count  int64
		status string
		comps  int64
	}
	readState := func() state {
		var st state
		s.db.Raw("SELECT COALESCE(MAX(id),0), COUNT(*), COALESCE(SUM(superseded=0 AND source_compaction_id IS NULL),0) FROM messages WHERE session_id = ?", sess.ID).Row().Scan(&st.maxID, &st.count, &st.liveCount) // see note
		s.db.Raw("SELECT status FROM sessions WHERE id = ?", sess.ID).Row().Scan(&st.status)
		s.db.Raw("SELECT COUNT(*) FROM compactions WHERE session_id = ?", sess.ID).Row().Scan(&st.comps)
		return st
	}

	last := readState()
	// Gap fill: rows newer than `since` are sent immediately on connect.
	emitSince := since
	if emitSince > 0 && emitSince < last.maxID {
		if frag, err := s.renderRowsAfter(sess, emitSince); err == nil && frag != "" {
			writeSSE(w, "messages", frag)
		}
	}
	writeSSEComment(w, "ready")
	fl.Flush()

	tick := time.NewTicker(ssePollInterval)
	defer tick.Stop()
	heart := time.NewTicker(sseHeartbeat)
	defer heart.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heart.C:
			writeSSEComment(w, "hb")
			fl.Flush()
		case <-tick.C:
			cur := readState()
			if cur.maxID > last.maxID {
				if frag, err := s.renderRowsAfter(sess, maxI64(last.maxID, since)); err == nil && frag != "" {
					writeSSE(w, "messages", frag)
				}
			}
			if cur.status != last.status || cur.comps != last.comps {
				payload, _ := json.Marshal(map[string]any{"status": cur.status})
				writeSSE(w, "session", string(payload))
			}
			last = cur
			fl.Flush()
		}
	}
}
```

Note the `readState` sketch above has a typo'd Scan target on purpose of the note: implement it with three simple typed queries instead of one clever one — `MAX(id)`, then `COUNT(*) WHERE superseded=false AND source_compaction_id IS NULL` (the live transcript shape the page renders), then status, then compaction count. Four one-row queries per second per viewer; no aggregation tricks.

Helpers on Server in `events.go`: `renderRowsAfter(sess *Session, afterID int64) (string, error)` — `SELECT` rows `id > afterID AND superseded = false AND source_compaction_id IS NULL ORDER BY id ASC` (limit 500 for safety), build views (+ usage assign with `sessionUsageRows`), `renderRowsHTML`; `writeSSE(w, name, data string)` — `event: <name>\ndata: <data>\n\n` (data with newlines → repeat `data: ` lines per SSE spec); `writeSSEComment`, `hostOnly`, `maxI64`, `trimCR` lives in tests.

`handleUserEvents` — same skeleton, state = `MAX(last_active)`+`COUNT(*)` over sessions `user_id = u.ID` (soft-delete filtered — use GORM model queries, not raw, so the DeletedAt filter applies); on change `event: updated` with empty data; no since.

Routes in `server.go`:

```go
	m.HandleFunc("GET /s/{slug}/events", secure(s.handleSessionEvents))
	m.HandleFunc("GET /u/{slug}/events", secure(s.handleUserEvents))
```

`static/session.js`:

```js
(function () {
	const script = document.currentScript;
	const slug = script.dataset.slug;
	const list = document.getElementById("msgs");
	if (!slug || !list) return;
	const lastId = () => {
		const rows = list.querySelectorAll(".msg[id]");
		return rows.length ? parseInt(rows[rows.length - 1].id.slice(1), 10) : 0;
	};
	const firstId = () => {
		const rows = list.querySelectorAll(".msg[id]");
		return rows.length ? parseInt(rows[0].id.slice(1), 10) : 0;
	};
	const pinned = () =>
		window.innerHeight + window.scrollY >= document.body.scrollHeight - 40;

	const es = new EventSource("/s/" + slug + "/events?since=" + lastId());
	es.addEventListener("messages", (ev) => {
		const stick = pinned();
		list.insertAdjacentHTML("beforeend", ev.data.replace(/^data: /, ""));
		if (stick) window.scrollTo(0, document.body.scrollHeight);
	});
	es.addEventListener("session", (ev) => {
		try {
			const st = JSON.parse(ev.data);
			const el = document.getElementById("status-badge");
			if (el && st.status) el.textContent = st.status;
		} catch (e) { /* ignore */ }
	});

	const older = document.getElementById("load-older");
	if (older) older.addEventListener("click", () => {
		fetch("/s/" + slug + "?fragment=rows&before=" + firstId())
			.then((r) => (r.ok ? r.text() : ""))
			.then((html) => {
				if (!html) { older.hidden = true; return; }
				const stick = pinned();
				const prevHeight = document.body.scrollHeight;
				list.insertAdjacentHTML("afterbegin", html);
				if (!stick) window.scrollBy(0, document.body.scrollHeight - prevHeight);
			});
	});
})();
```

(The `replace(/^data: /)` is wrong — the EventSource API already unwraps `data:`; drop that call. `ev.data` is the payload itself.)

`static/user.js`:

```js
(function () {
	const slug = document.body.dataset.userSlug;
	if (!slug) return;
	const list = document.getElementById("session-list");
	const es = new EventSource("/u/" + slug + "/events");
	es.addEventListener("updated", () => {
		fetch(location.pathname + "?fragment=list")
			.then((r) => (r.ok ? r.text() : ""))
			.then((html) => { if (html && list) list.innerHTML = html; });
	});
})();
```

Template wiring: `session.html` wraps rows in `<div id="msgs">` + `#load-older` button with `data-*`; `user.html` gets `<body data-user-slug="{{.User.Slug}}">`, `#session-list`, and the two script tags (`session.js` only on session page, `user.js` on user page; both `defer`).

- [ ] **Step 4: Run tests**

Run: `go test ./sesssite/ -v`
Expected: PASS (SSE tests are timing-based but the 5s deadline with 1s polls is generous).

- [ ] **Step 5: fmt/vet + commit**

```bash
go fmt ./... && go vet ./...
git add sesssite/
git commit -m "feat(sesssite): SSE live updates — messages/session events, user pings, per-IP cap"
```

---

### Task 11: wrap-up — docs, build wiring, full verification

**Files:**
- Create: `sesssite/README.md`
- Modify: `AGENTS.md` (Architecture section entry + build commands), `build.sh` (add sesssite build line), `README.md` (component list mention, one line)
- Modify: `config/config.toml` only if Tasks 2–4 drifted from the documented block

**Interfaces:**
- Consumes: everything above.
- Produces: documented, fully-building, fully-tested tree.

- [ ] **Step 1: README for sesssite**

`sesssite/README.md`: what it is, the read-only contract, config reference (mirroring example.toml), the `mode=ro`/query_only fallback note, the postgres read-only-role recommendation, deployment sketch (reverse proxy, run alongside dave), and the MaxAgeDays link-lifetime caveat.

- [ ] **Step 2: Wire the build**

`build.sh`: add a target line next to the imgsite one (~line 86): `add_target "sesssite" "./sesssite" "sesssite/sesssite"`. Also add `sesssite` to the usage list alongside imgsite (~line 23). `AGENTS.md`: add `sesssite/` to the architecture section (mirror the imgsite entry's depth — summary + key invariants: read-only contract, slug system, tail-copy exclusion, SSE poll model, pastebin coexist knob) and add the build command to the Commands block.

- [ ] **Step 3: Full verification**

Run: `go fmt ./... && go vet ./... && go build ./... && go test ./...`
Expected: ALL PASS, zero output from fmt (no diffs), vet clean.

- [ ] **Step 4: Manual smoke test**

```bash
go build -o sesssite/sesssite ./sesssite
cd sesssite && ./sesssite --config example.toml &  # needs a real dave db — point path at test/ data if present; otherwise verify startup error is the documented "database …: does not exist" and exit 1
kill %1
```

Verify: binary starts, config errors are readable, `curl -s localhost:8060/` returns the landing page (if a DB was available), unknown slug 404s.

- [ ] **Step 5: Commit**

```bash
git add sesssite/README.md AGENTS.md build.sh README.md
git commit -m "docs: sesssite — README, build wiring, AGENTS entry"
```

---

## Post-plan note for executors

- Tests in this plan reference `readConfig`, `getNotices`, `setupMigrationDB`, mock-runner construction patterns, and girc event helpers — all exist in the dave root test corpus except where the plan itself defines them (`setWebsiteURL`, `setWebsiteURL`'s save/restore is the repo's established `config.X = …` + `t.Cleanup` pattern). When a plan snippet's construction disagrees with the current source, TRUST THE SOURCE and adapt the literal (the plan's assertions are the contract, its literals are sketches of fields that existed at plan time).
- The sesssite package builds against the SAME go.mod; `go get` only in Task 6.
- If `girc.Event` construction for `resolveIRCUserWithID` proves hairy, test `linkCommand` one level lower: extract `linkCommandCore(network, channel, userID, output, ctx)` and unit-test that, leaving the IRC-glue thin.
