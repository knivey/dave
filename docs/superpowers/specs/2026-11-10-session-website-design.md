# Session History Website (`sesssite`) — Design

Status: approved design (2026-11-10). Companion repo pieces: dave root
changes (slug minting, migration #10, pastebin-link mode, `^link` command),
new sibling package `sesssite/`.

## Goal

Give IRC users a web view of their conversations with dave: the **full**
session history (every stored message, including archived/compacted
material, tool calls, reasoning, images) and a **user page** listing all
their other sessions. The bot hands out unlisted links on request; anyone
holding a link can view (sessions are not secret — everyone in the channel
already sees the interactions). Session pages **live update** while the
conversation continues. For the user the site also **replaces the pastebin**
for over-long replies (config-gated, coexisting with it).

Grounding facts (verified in code):

- `messages.content` is raw markdown (IRC rendering happens at send time);
  user messages may carry real IRC control codes (`\x02`, `\x03`, …) from
  IRC clients.
- The final assistant message is persisted (`turn.Add`) **before**
  `sendFinalText` → `sendRendered` → `sendWithPastebin` runs, and the
  session turn gate serializes writers on one session — so at link-build
  time the just-sent reply is the session's latest assistant row.
- dave's DB (sqlite WAL or postgres) is written only by dave; both
  dialects allow concurrent readers.
- `~/markdown-parsebin` (`dave-web`) already contains the exact rendering
  stack this site needs: goldmark + GFM + Chroma (Dracula) + an IRC
  control-code → `<span class="irc-fg-N irc-bold …">` AST transformer with
  the full 99-color mIRC palette and fg==bg spoiler detection. It is MIT
  licensed and owned by us; we copy it in rather than take a module
  dependency.

## Non-goals (v1)

- Authentication, login, or any writer surface on the site — it is
  strictly read-only over dave's DB.
- Slug rotation (a minted slug is permanent for the life of its row).
- Streaming mid-turn updates (messages land at turn granularity; the page
  shows the user message as soon as it is persisted, the reply when the
  turn completes).
- Replacing the pastebin for `^help` (help text is not a session; it keeps
  `uploadToPastebin`).
- Per-message editing/hiding/deleting from the web.
- Comments, likes, or any interactivity beyond collapsibles and
  pagination.

## Architecture

Approach (chosen over extending markdown-parsebin or embedding an HTTP
server in dave): a **new sibling package `sesssite/`** in this repo,
following the `imgsite/` convention — single Go binary, own TOML config,
server-rendered HTML templates with small vanilla-JS enhancement, GORM
**read-only** models over dave's database.

- sqlite is opened `mode=ro` (WAL readers work; the site physically cannot
  write). postgres connects with the configured DSN; the code only ever
  issues SELECTs (docs recommend a read-only role). No AutoMigrate, ever.
- The renderer is ported from `markdown-parsebin/internal/renderer`
  (`markdown.go`, `irc.go`, tests) into `sesssite/renderer/` with
  attribution in the file headers.
- dave remains the single writer: it mints slugs, and the site reads them.

## Slug system

- Format: 8 random base62 characters, `crypto/rand`, rejection sampling to
  avoid modulo bias (imgsite's `randomImageID` pattern). ~2×10¹⁴ space.
- New columns `sessions.slug` and `users.slug` (TEXT) with unique indexes.
- **Migration #10 `add_session_user_slugs`**: adds the columns, backfills
  every existing row with a fresh unique slug (retry on collision),
  creates the unique indexes. Per-dialect DDL through the existing
  migration runner.
- Mint sites in dave: `SessionManager.CreateSession` (every session row)
  and `createNewUser` (every user row — the flagged-fallback creation path
  included; implementation must verify via grep that no other
  `Create(&User{…})`/`Create(&Session{…})` site exists and route them
  through the minter). Shared helper `generateSlug()` in a new `slug.go`.
- Slugs are minted once and never rotated; they die with their row
  (`CleanupByAge`/`DeleteSession` ⇒ link 404s — documented behavior: links
  live exactly as long as the session row, `MaxAgeDays` governs).
- User merges (`/usermerge`) reassign sessions to the surviving user id;
  the surviving user's slug naturally absorbs them. No special handling.
- URL shapes: `/s/<slug>` session page, `/u/<slug>` user page. Slug
  charset validated on lookup (exactly 8 base62 chars — imgsite's
  `validImageID` pattern); anything else is a plain 404.

## Site pages

### Session page `/s/<slug>`

Header: network/channel, chat command, service + model (with a visible
marker whenever the model changed mid-session, derived from
`turn_usage.model`), status badge (active/completed), created/last-active,
message count, aggregate token usage per model (collapsed `<details>`).

Transcript — flat chronological list of **all** non-superseded messages:

- **Compaction tail-copies are excluded** (`source_compaction_id IS NULL`):
  their content duplicates the archived originals, and without the filter
  the flat view shows the tail twice. Superseded rows are excluded as
  everywhere else.
- Compaction boundaries render as dividers carrying the compaction
  metadata (trigger, model, tokens, duration) with the summary message
  inline; archived originals below the boundary are shown dimmed with an
  "archived" tag. Nothing is omitted — the complete record is visible.
- System prompt rows render collapsed (`<details>`).
- User/assistant content renders as markdown (goldmark, GFM, Chroma
  highlighting for code blocks); embedded IRC control codes become styled
  spans via the ported transformer.
- Tool calls: the assistant row's `tool_calls` render as cards (tool name
  + pretty-printed JSON args); each matching role=tool result row renders
  as a collapsed block with a short preview.
- Reasoning: `reasoning_content` (and `reasoning_summary`, labeled) render
  collapsed per message.
- Images: `multicontent` image parts render inline (`<img>` for both URL
  and data-URI forms).
- Per-assistant-message usage line: `model · in→out tok · duration`,
  expandable to the full `turn_usage` rows (cached/reasoning split,
  finish reason, API path). Joined by time window per session (usage rows
  carry no message id): rows with `created_at` in (previous assistant
  message, this assistant message]. Display-only and labeled approximate.
- Anchors: every message row carries `id="m<id>"`; deep links
  (`#m<id>`) scroll to the row and flash-highlight it.
- **Raw view (first-class)**: every message row has a "raw" link to
  `/s/<slug>/m/<id>/raw` which serves the **exact stored content**
  byte-for-byte as `text/plain; charset=utf-8` — no markdown, no HTML,
  no IRC-code translation. (For multicontent-only messages the raw
  endpoint returns the raw JSON of the parts; for tool-call rows, the raw
  tool_calls JSON.)
- Pagination: latest 200 messages render initially; "load older" fetches
  previous 200-message chunks (keyset by id DESC).

### User page `/u/<slug>`

Header: current nick, network, IRC account if any (display-only identity
context). Session list: command, network/channel, first-message preview
(clamped), model, status badge, last-active; each row links to its session
page. Keyset paginated by `last_active DESC`, 50 per page. Sessions with
`NULL` user_id have no user page and are reachable only by their own slug
(this is fine — they are the minority; the bot links the user page).

### Landing `/`

A tiny static page: what the site is, and that pages are unlisted — get
yours from the bot via `^link`.

## Live updates (SSE)

- `/s/<slug>/events`: one poller goroutine per connection, 1s tick,
  polling `MAX(id)` + live message count + session `status`/`last_active`
  + compaction count. On change:
  - `messages` event: server-rendered HTML rows for ids greater than the
    client's last-seen id; client appends. Auto-scroll pinned to bottom
    unless the user has scrolled up.
  - `session` event: updated status/last-active for the badge.
  - Heartbeat comment every 15s (proxy keepalive).
  - Client sends its last-seen id as a query param (`?since=<id>`), so a
    reconnect after a dropped SSE never misses rows.
- `/u/<slug>/events`: 1s poll of max(`last_active`)/count over the user's
  sessions; on change an `updated` ping — the client refetches the list
  fragment (`?fragment=1`).
- Per-IP SSE connection cap: 10 concurrent event-stream connections per IP
  (imgsite precedent; 503 when exceeded). Pollers stop on connection
  close.

## dave-side changes

- **Config**: new `[website]` section in `config.toml` — `url` (base URL
  of the site, e.g. `https://sessions.example.com`). Unset/empty = old
  pastebin behavior. This single knob IS the pastebin coexist switch.
  Documented in the config reference block per repo convention.
- **`sendWithPastebin`**: when `config.Website.URL` is set **and**
  `cr.sessionID != 0`: skip the upload, send the usual preview lines, then
  send a "full message" line linking `<url>/s/<slug>#m<id>`. The message
  id is the latest assistant-role row id for the session at send time
  (persist-before-send + session gate make this the just-sent reply; the
  reasoning-fallback edge lands on the nearest assistant row — accepted).
  When `sessionID == 0` (ephemeral generators) the link mode falls back
  to the pastebin — there is no session row to link. `^help`'s pastebin
  path is untouched.
- **Notices**: new `[website]` notices section (`full_line` for the
  pastebin-replacement line, `link_line` for `^link`) with
  `setNoticesDefaults` defaults and reload support — templated channel
  messages like every other notice.
- **`^link` builtin command** (registered in `builtInCmds`): resolves the
  calling user (`GetActiveSession` on the calling network/channel/user),
  replies (channel message via the output chan, notice templates) with
  the current active session's URL and the user page URL. No active
  session ⇒ user-page URL only. Website URL unconfigured ⇒ error notice
  naming the missing config.
- **`SessionManager.AddMessage`**: no signature change needed (the anchor
  id is queried, not threaded).

## Error handling & edges

- Unknown/malformed slug → 404 (no existence leak; landing page explains).
- Soft-deleted sessions excluded from every query (GORM `DeletedAt`
  filter on the read models).
- Site down ⇒ its links 404; accepted (nothing was uploaded, so there is
  no upload failure path; the pastebin coexist knob lets the admin fall
  back by unsetting `[website] url`).
- XSS: goldmark's default escaping (raw HTML is NOT enabled), tool
  args/results in escaped `<pre>`, the IRC transformer only emits its own
  known class names, `rel="noopener"` on external links, plus a
  restrictive `Content-Security-Policy` header as defense in depth.
- The read-only sqlite open guarantees the site can never write dave's
  DB; postgres deployments should use a read-only role (documented).
- Sessions in DMs or other channels appear on the user page — links are
  the only gate, per the non-secret decision.

## Testing

dave (root package):

- Migration #10: backfill covers all rows, slugs unique, charset valid;
  idempotent via the schema_migrations runner (existing migration test
  pattern).
- Slug minting at both creation sites (and any discovered extra sites).
- Pastebin-link mode: website url set + session > 0 ⇒ link line, no
  upload call; ephemeral ⇒ pastebin fallback; url unset ⇒ old path;
  `^help` path untouched.
- `^link`: with/without active session, unconfigured website.
- Notices defaults + reload for the new section; config load tests for
  `[website]`.

sesssite:

- Renderer tests ported from parsebin (markdown rendering, IRC code
  spans, spoiler, palette) — adapted to the copied package.
- Page tests (httptest): each surface's rendered HTML — roles, tool call
  cards, collapsed reasoning, compaction dividers, tail-copy exclusion,
  archived dimming, image parts, anchors, raw endpoint byte-for-byte
  equality with stored content, pagination keysets, 404s.
- SSE tests: insert a row → `messages` event with the rendered fragment;
  status flip → `session` event; `since=` reconnect gap-fill; user-page
  `updated` ping.
- Read-only DB open test (sqlite `mode=ro` write attempt fails).

## Future work (explicitly out of v1)

- Slug rotation knob (user asks for a fresh link).
- Mid-turn streaming to the session page.
- `?raw=1` whole-session raw view.
- Search across a user's sessions.
