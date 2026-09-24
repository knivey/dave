# Image Site (imgsite) — Design & Feature Plan

Status: implemented (2026-09-24) — milestones 1–7 shipped in `imgsite/` with the
img-mcp switch-over; each milestone was independently reviewed. Grounded in
verified production artifacts: two live webp uploads from img.zkpq.ca (`526`,
`528`) were dissected to pin down the exact EXIF/workflow embedding; details
in "Input format (verified)".

## Goal

Replace the third-party photo site (img.zkpq.ca) with a self-hosted gallery
purpose-built for dave's image generations. The site:

- serves **permanent direct image links** — the upload response hands dave
  the exact ready-to-paste URL, no client-side derivation or verification,
- gives every image a **details page** built from the workflow graph embedded in the file's EXIF,
- offers a **gallery with background-generated thumbnails** and **live updates** (SSE) when new images land,
- provides **fast prompt search** — original prompts ranked first, enhanced prompts second,
- accepts **uploads only from dave** (API-key authenticated).

## Non-goals (for v1)

- User accounts, comments, likes, albums.
- Re-hosting arbitrary uploads; the only writer is dave (img-mcp).
- Migrating history from img.zkpq.ca (possible later backfill; old links stay on the old host).
- Editing images or re-running workflows from the site.
- GIF/video support.

## Input format (verified)

Production images are ComfyUI webp outputs (lossy, q80, `embed_workflow=true`)
with this structure:

```
RIFF container
├── "VP8X" chunk (extended format, flags, canvas size)
├── "VP8 " chunk (lossy image data; note the trailing space in the fourcc)
└── "EXIF" chunk (raw TIFF)
    ├── header "MM\x00*" (big-endian)
    └── IFD0, exactly one entry:
        tag 0x0110 (Model), type 2 (ASCII), count = payload length
        payload = "prompt:" + <API-format workflow JSON> + "\x00"
```

The workflow JSON is the full submitted graph — 11 nodes in production today —
including the disconnected `dave_original_prompt` node img-mcp injects.
Verified payload of that node (sample 528):

```json
{
  "prompt": "shrew walkin down main street",
  "llm_generated": false,
  "job_id": "ed974b6d",
  "enhancement_reasoning": "The user described a shrew walking down Main Street.\n\n…(677 chars)"
}
```

`enhancement_reasoning` is present only on enhanced jobs. Everything else the
details page wants is discoverable by graph traversal (sample 528):

| Field | Source node | Value |
|---|---|---|
| Enhanced prompt | `KSampler.positive` → `CLIPTextEncode.text` | "A cute anthropomorphic brown shrew …" |
| Negative prompt | `KSampler.negative` → `CLIPTextEncode.text`; `ConditioningZeroOut` ⇒ none | (zeroed) |
| Seed / steps / cfg | `KSampler` inputs | 5702895218061442231 / 8 / 1.0 |
| Sampler / scheduler / denoise | `KSampler` inputs | euler / simple / 1.0 |
| Models | `UNETLoader.unet_name`, `CLIPLoader.clip_name`, `VAELoader.vae_name` | krea2_turbo_int8_convrot, qwen3vl_4b_fp8_scaled, qwen_image_vae |
| LoRAs | any `LoraLoader*` node's `lora_name` + strength | — |
| Dimensions | `EmptyLatentImage`/`EmptySD3LatentImage` width/height | 1920×1080 |

**Critical**: node IDs are NOT stable — the checked-in `z_image_turbo.json`
uses IDs 3–29 while production output uses 55–76 for the same logical graph.
Extraction must key on `class_type` + edge references, never node IDs.

## Architecture

Single Go binary, no runtime dependencies, mirroring the conventions of the
rest of this repo (std-lib `net/http`, `html/template`, TOML config with
reference header + commented example, sqlx + goose + modernc.org/sqlite,
logxi logging with `SetLevel(logxi.LevelAll)`, SIGHUP hot-reload).

Server-side rendered HTML for initial page loads (works without JS, sane with
curl), progressively enhanced by a JS layer (`web/`) for SSE, infinite
scroll, live search,
and dynamic next-buttons.

```
dave / img-mcp ──POST /updo (X-API-Key, multipart file+meta)──┐
                                                              ▼
                                                    ┌──────────────────┐
        browsers ──GET HTML/JSON/SSE────────────────│      imgsite      │
                       │                           │  ┌──────────────┐ │
                       ▼                           │  │ upload store │ │ hash + write file + INSERT row
              ┌─────────────────┐                  │  └──────┬───────┘ │
              │ SQLite (WAL)    │◄─────────────────│         │ enqueue│
              │ images + FTS5   │                  │  ┌──────▼───────┐ │
              └─────────────────┘                  │  │ thumb worker │ │ decode → resize → encode
                              ▲                    │  └──────┬───────┘ │
                              │  thumb-ready event │  ┌──────▼───────┐ │
                              └────────────────────│  │ SSE hub      │ │ fan-out to all /events conns
                                                   │  └──────────────┘ │
                                                   └──────────────────┘
```

### Repository layout

```
imgsite/
  main.go            config load, server bootstrap, SIGHUP
  config.go          Config struct + validation + reloadable set
  config.toml        live config (reference header + commented example block)
  example.toml       documentation copy
  db.go              sqlx helpers, migrations bootstrap
  migrations/        001_init.sql …
  store.go           content-addressed file storage (sha256 fan-out dirs)
  upload.go          POST /updo handler: auth, limits, dedupe, metadata merge
  extract.go         webp/PNG metadata chunk parsing (verified format above)
  workflow.go        graph traversal → ImageMetadata (class_type-keyed rules)
  thumbs.go          background thumbnailer (worker pool, restart-safe)
  events.go          SSE hub (subscribe/publish, heartbeats, overflow policy)
  search.go          FTS5 queries + snippet building
  pages.go           HTML handlers (gallery, image page, search)
  api.go             JSON handlers (neighbors) — folded into server.go/search.go in the implementation
  server.go          router wiring, cache headers, middleware
  logging.go         logxi loggers
  web/               style.css, app.js + page modules (see "Frontend structure")
  testdata/          526-style plain webp, 528-style enhanced webp fixtures
  *_test.go          parser/extractor/handler/SSE/FTS tests (testify)
```

## Configuration

`imgsite/config.toml`, same conventions as sibling packages (reference
section listing every option + copy-pasteable commented block):

```toml
[server]
name = "imgsite"          # identification
addr = ":8081"            # listen address
base_url = ""             # external URL for absolute links; empty = derive from request

[database]
path = "data/imgsite.db"  # SQLite (WAL), relative to binary dir

[storage]
path = "data/images"      # originals fan-out: <path>/ab/<sha256>
thumbs_path = "data/thumbs"  # derivatives: <path>/ab/<sha256>-<size>.jpg

[auth]
api_key = "…"             # REQUIRED. Uploads must send X-API-Key (constant-time compare)

[thumbnails]
small_width = 480         # gallery card thumb
display_width = 1280      # details-page / og:image size
jpeg_quality = 78
workers = 2
max_dimension = 8192      # refuse absurd decodes

[upload]
max_bytes = 33554432      # 32 MiB
rate_per_minute = 30      # token bucket (dave only, but cheap defense)

[search]
snippet_chars = 160
prefix_min = 2            # enable prefix token indexing from 2 chars

[site]
title = "dave's image dump"
description = "generations from IRC"
```

Reloadable via SIGHUP: `site.*`, `thumbnails.*` (except worker count),
`search.*`, `upload.rate_per_minute`. Not reloadable (restart required):
`server.*`, `database.*`, `storage.*`, `auth.*`. Same pattern as img-mcp.

## Data model

```sql
CREATE TABLE images (
  id            TEXT PRIMARY KEY,   -- short public id (below)
  sha256        TEXT NOT NULL,        -- indexed, NOT unique: N gallery
                                    -- entries can share one stored file (dedupe)
  filename      TEXT NOT NULL,      -- dave-chosen name, sanitized
  mime_type     TEXT NOT NULL,
  size_bytes    INTEGER NOT NULL,
  width         INTEGER,
  height        INTEGER,
  created_at    TEXT NOT NULL,      -- upload time, UTC (SQLite CURRENT_TIMESTAMP)
  thumb_status  TEXT NOT NULL DEFAULT 'pending',  -- pending|ready|failed
  hidden        INTEGER NOT NULL DEFAULT 0,       -- soft delete

  -- prompt metadata (source: upload meta, EXIF, or merged; see merge policy)
  original_prompt TEXT NOT NULL DEFAULT '',
  enhanced_prompt TEXT NOT NULL DEFAULT '',
  negative_prompt TEXT NOT NULL DEFAULT '',
  reasoning       TEXT NOT NULL DEFAULT '',
  job_id          TEXT,
  llm_generated   INTEGER NOT NULL DEFAULT 0,
  -- provenance extras sent by dave (display-only)
  network  TEXT, channel TEXT, nick TEXT, workflow_name TEXT,

  -- graph-derived params
  seed INTEGER, steps INTEGER, cfg REAL, denoise REAL,
  sampler TEXT, scheduler TEXT,
  model_unet TEXT, model_clip TEXT, model_vae TEXT,
  loras TEXT,                -- JSON array [{"name":…,"strength":…}]
  workflow_json TEXT NOT NULL DEFAULT '',  -- full embedded graph (pretty on demand)
  meta_source TEXT NOT NULL DEFAULT ''     -- "upload" | "exif" | "upload+exif"
);
CREATE INDEX idx_images_created ON images(created_at DESC, id DESC);
CREATE INDEX idx_images_sha ON images(sha256);

CREATE VIRTUAL TABLE images_fts USING fts5(
  original_prompt, enhanced_prompt,
  content='images', content_rowid='rowid',
  tokenize="porter unicode61", prefix='2 3 4'
);
-- + AFTER INSERT/UPDATE/DELETE triggers maintaining images_fts ('delete' + insert dance for external content)
```

### Public IDs

Random 7-char base62 (`[0-9A-Za-z]{7}`), collision-checked with a retry loop
against the PK. Reads well in URLs (`/aQ3f9xK`), doesn't leak volume, and
gives ~3.5 trillion space — collisions are a non-event in practice. Job IDs
(img-mcp's `ed974b6d`) are stored as metadata, not used in URLs.

### Keyset pagination

Neighbors, gallery paging, and search paging key on `(created_at DESC, id
DESC)` — never OFFSET — so live prepends can't shift pages under the user.
`created_at` is stored as SQLite `CURRENT_TIMESTAMP` UTC (`YYYY-MM-DD HH:MM:SS`,
second resolution); Go parses/formats it and renders RFC3339 in APIs/SSE.
Second resolution makes the `id DESC` tiebreaker load-bearing for
multi-image jobs completing in the same second — it's part of the cursor,
not an afterthought. Cursor wire format: `?after=YYYY-MM-DD%20HH%3AMM%3ASS~<id>`
(`~` separator; the timestamp is URL-encoded as one component).

## HTTP surface

| Route | What | Cache |
|---|---|---|
| `GET /` | gallery page (first ~48, infinite scroll) | no-cache (live) |
| `GET /gallery?after=YYYY-MM-DD%20HH%3AMM%3ASS~<id>` | next gallery page (HTML fragment only — the `cards` partial for infinite scroll) | no-cache |
| `GET /search?q=…` | search results (full HTML page — shareable link + no-JS form target) | no-cache |
| `GET /search-fragment?q=…&after=…` | search results page (HTML fragment — first page + infinite scroll) | no-cache |
| `GET /static/…` | embedded web/ assets (JS/CSS; no directory listings) | `public, max-age=3600` |
| `GET /favicon.ico` | inline SVG favicon | `public, max-age=86400` |
| `GET /<id>` | image details page (HTML) | no-cache (next-button is live) |
| `GET /<id>/orig/<filename>` | **permanent direct link** — original bytes served directly (200) | `public, max-age=31536000, immutable` |
| `GET /<id>/t/<size>` (`small`\|`display` — tokens mapped to the configured `thumbnails.*_width`) | thumbnail bytes served directly (200) | immutable; **404 (pending/failed) is no-cache** so the JS retry works |
| `GET /api/images/<id>/neighbors` | `{prev:{…}, next:{…}}` keyset neighbors | no-cache |
| `GET /events` | SSE stream | no-cache |
| `POST /updo` | upload (X-API-Key) | — |
| `POST /admin/reload` | hot reload (X-API-Key) | — |
| `POST /admin/reextract` | re-run workflow extraction from stored `workflow_json` under current rules (X-API-Key) — heals rows when extraction improves; preserves provenance/visibility, skips unparseable rows | — |
| `DELETE /api/images/<id>` | soft-delete → `hidden=1` (X-API-Key) | — |

Routes serve bytes directly — no redirect hops. We control both ends of
this protocol, so there is nothing to preserve from the old site's
303/307 dance; dave never derives or verifies URLs anymore (see "img-mcp
changes"). Disk layout stays content-addressed by sha256 for dedupe; the
public routes are all id-keyed because base62 ids make shorter IRC URLs
than 64-char hashes. Immutable caching on id-keyed routes is safe: ids are
never reused and the content behind a row never changes.

## Upload protocol (dave → site)

```http
POST /updo
X-API-Key: <constant-time compared>
Content-Type: multipart/form-data

  file      = <image bytes>                (required)
  meta      = <JSON>                       (optional but always sent by img-mcp)
```

(`toirc` is gone — that field only existed to silence the old site's IRC
announcer, which this site doesn't have.)

`meta` JSON (all fields optional). Prompt fields come straight off the `Job`
in `processJob`; graph-derived numbers (seed, steps, …) are intentionally NOT
sent — EXIF is the source of truth for those per the merge policy, and the
effective seed (random when `job.Input.Seed` is nil) is only materialized
inside the submitted workflow anyway:

```json
{
  "job_id": "ed974b6d",
  "original_prompt": "shrew walkin down main street",
  "enhanced_prompt": "A cute anthropomorphic brown shrew …",
  "negative_prompt": "",
  "reasoning": "The user described a shrew …",
  "llm_generated": false,
  "workflow_name": "zimage-turbo",
  "network": "libera", "channel": "#dave", "nick": "knivey"
}
```

`network`/`channel`/`nick` do not reach img-mcp today — see "img-mcp
changes" for the (small but real) plumbing they require.

Handler sequence (all synchronous, fast — heavy work is deferred):

1. Auth check (constant-time), rate-limit bucket, `Content-Length`/`MaxBytesReader` guard.
2. Sanitize filename (same rules as img-mcp's `sanitizeUploadFilename`).
3. sha256 while buffering to temp; reject empty bodies.
4. **Dedupe**: existing `sha256` ⇒ same bytes. Still create a new `images`
   row with a fresh id (each IRC generation is its own gallery entry with its
   own prompt metadata), pointing at the same stored file. Files are
   content-addressed so nothing is written twice.
5. Parse EXIF/webp synchronously (it's a few-KB chunk scan — sub-millisecond)
   and merge with `meta` (policy below). If EXIF parse fails, log WARN and
   proceed on `meta` alone.
6. INSERT row with `thumb_status='pending'`; publish `image-new` SSE event; enqueue thumb job.
7. Respond `201 Created` with JSON — the direct link is handed to dave
   verbatim, nothing derived client-side:

```json
{
  "id": "aQ3f9xK",
  "url": "https://img.example.com/aQ3f9xK/orig/2026-09-23-234552__0.webp",
  "page": "https://img.example.com/aQ3f9xK",
  "filename": "2026-09-23-234552__0.webp"
}
```

   `url` and `page` are absolute, built from `server.base_url` — which
   therefore must be set correctly in prod config, since dave stores `url`
   verbatim into the job result and pastes it to IRC.

Thumbnail generation is NOT in the upload path — an upload answers in the
time it takes to hash + write the file + one INSERT.

### Metadata merge policy (upload meta vs EXIF)

EXIF is the source of truth for *graph-derived* fields (seed, sampler,
models, dims, enhanced/negative prompt, workflow JSON) because it is what the
image actually executed with. Upload `meta` wins for *provenance* fields dave
knows at submit time (network/channel/nick/workflow_name) and serves as the
fallback for prompt fields if EXIF parsing ever fails. `original_prompt`,
`reasoning`, `llm_generated`, `job_id` are cross-checked between both; a
mismatch (e.g. different job_id) logs a WARN and prefers EXIF. `meta_source`
records which side(s) contributed.

## Metadata extraction (extract.go / workflow.go)

1. **Container scan**: RIFF walk (`VP8X`, `VP8 `, `EXIF` chunks, odd-size
   padding) → EXIF bytes. PNG fallback for future workflows: `tEXt` chunks
   keyed `prompt`/`workflow` (ComfyUI's standard SaveImage convention).
2. **TIFF IFD0 walk**: big- or little-endian; collect ASCII entries; look for
   payload prefix `prompt:` (API graph) and `workflow:` (UI graph, present
   when `save_workflow_as_json=true`; ignored by us). Strip trailing NUL.
3. **Graph traversal** (`workflow.go`) — class_type-keyed, ID-agnostic:
   - `dave_original_prompt` node → JSON payload → original prompt, llm_generated, job_id, reasoning
   - sampler classes `KSampler`, `KSamplerAdvanced` (+ future additions) → seed/steps/cfg/sampler/scheduler/denoise; follow `positive`/`negative` edge refs (2-tuples `[nodeID, slot]`)
   - negative endpoint `ConditioningZeroOut`/`ConditioningCombine` ⇒ no text; `CLIPTextEncode.text` ⇒ negative text
   - loader classes by case-insensitive PREFIX on `class_type`
     (`unetloader*`, `cliploader*` + `DualCLIPLoader`, `vaeloader*`,
     `loraloader*`) → model names, LoRA strengths. Prefix, not exact,
     because custom packs register variants — the ComfyUI-GGUF pack ships
     `UnetLoaderGGUF` (lowercase "net") and `CLIPLoaderGGUF` alongside the
     core classes, and exact matching silently dropped both qwen GGUF
     models in production. DualCLIPLoader names its field
     `dual_clip_name`; TripleCLIPLoader is deliberately unmatched (its
     clip_name1/2/3 don't map to one display field).
   - latent sources (`EmptyLatentImage`, `EmptySD3LatentImage`, …) → width/height; fallback: decoded image bounds
4. Everything unknown is preserved verbatim in `workflow_json` for the page's raw viewer. Extraction failure of any single field degrades to blank — never fails the upload.

## Thumbnail pipeline (thumbs.go)

- Worker pool (`thumbnails.workers`, default 2) draining an in-process
  channel; pending set re-scanned from the DB on startup so a crash between
  INSERT and thumbnail doesn't strand placeholders forever.
- Decode: `golang.org/x/image/webp` (handles the production lossy VP8 files;
  also VP8L lossless). Resize: `x/image/draw.CatmullRom`. Encode: **JPEG**
  q78 via stdlib (stdlib has no webp encoder; universal support, zero extra
  deps; originals stay the webp source of truth).
- Two derivatives per image: `small` (480w — gallery cards) and `display`
  (1280w — details page `<img>` and `og:image`).
- Failure (corrupt exotic input, decode bomb): mark `thumb_status='failed'`,
  gallery falls back to the original URL as `src` — worst case slower, never
  broken. Decode guarded by `max_dimension`.
- On completion: update row, publish `thumb-ready` SSE.
- Gallery renders pending thumbs as a shimmer placeholder; `gallery.js`
  swaps shimmer→thumb on the `thumb-ready` SSE event. Missed events (dead
  conn, tab asleep past the replay ring) are covered by the card image's
  error-listener retry: one refetch ~1.5s after the pending-thumb 404,
  then fallback to the original bytes.

## Live updates (events.go — SSE)

One hub; every `GET /events` connection gets a buffered channel (cap 32).

- Heartbeat comment `:hb` every 20s keeps proxies honest.
- Overflow policy: drop the slow client's entire buffer and send an event
  instructing a full state refresh (`event: reset`), client re-fetches the
  current page slice. Simpler than lossless backfill and correct.
- `Last-Event-ID`/`since` honored on reconnect via a small ring buffer of
  recent events (cap 128) — replays missed events on tab wake.

Events (JSON payloads):

```
event: image-new
data: {"id":"aQ3f9xK","created_at":"2026-09-24T03:12:00Z","original_prompt":"shrew…",
       "thumb_status":"pending","page_url":"/aQ3f9xK"}

event: thumb-ready
data: {"id":"aQ3f9xK","thumb_status":"ready"}

event: image-hidden          (soft delete — clients drop the card)
data: {"id":"aQ3f9xK"}
```

Client behaviors:

- **Gallery**: every query (gallery, keyset pages, `/<id>` pages, neighbors)
  filters `hidden = 0`. On `image-new`, prepend card — unless a search filter
  is active, in which case arrivals buffer into a "+N new" pill (clicking
  clears the filter and flushes the batch; no client-side match guessing —
  the grid never shows anything the active query didn't return). On
  `thumb-ready`, swap placeholder→thumb.
- **Image page**: page renders with `prev`/`next` from keyset neighbors
  (`prev` = newer, `next` = older). If `prev` is null (viewing the newest
  image — "at the end" in browsing order), subscribe; on `image-new` fetch
  `/api/images/<id>/neighbors` once and fade in the newer/prev chevron. New
  arrivals while browsing mid-history do nothing (correct — they only extend
  navigation from the newest end).

## Pages

### Gallery (`/`)

- Responsive masonry-ish grid (CSS columns), cards show small thumb + prompt
  preview (original prompt, `snippet_chars` clamp) + timestamp.
- Infinite scroll via `IntersectionObserver` fetching `?after=<keyset>`
  fragments; DOM capped at ~200 cards (furthest trimmed, scroll-up restores
  via same fetch).
- Search box pinned to header — debounce 150ms → fetch `/search-fragment?q=`
  fragment (`/search` is the full page + no-JS form target), replace grid,
  keep SSE wiring live.

### Image details (`/<id>`)

- Main image at `display` size, click-through to `/orig/` full bytes.
- Details panel:
  - **Original prompt** (prominent — it's what the user actually typed)
  - Enhanced prompt (collapsible, default open when it differs from original)
  - Enhancement reasoning (collapsible, default collapsed; `<pre>` wrapped)
  - Negative prompt (if any)
  - Params table: seed (click-to-copy), steps, cfg, sampler/scheduler,
    denoise, dimensions, file size/format, models (unet/clip/vae), LoRAs
  - Provenance: timestamp, job_id, workflow name, network/#channel/nick (when provided)
  - Raw workflow JSON (collapsed `<details>` with syntax-pretty JSON)
- **Navigation**: large prev/next chevron zones on image edges + ←/→
  keyboard arrows + `n`/`p` keys; neighbor URLs server-rendered for
  no-JS users; live next-button per SSE above.
- Copy-link button (copies `/orig/` direct URL), download link.
- OpenGraph tags: `og:image` = display thumb, `og:title` = original prompt,
  `og:description` = params summary — pasted links embed nicely in
  Discord/clients that unfurl.

### Search

- **FTS5** is SQLite's built-in full-text engine (an inverted index with
  BM25 ranking — the same family of ranking Elasticsearch/Lucene uses, not a
  `LIKE` scan). **Verified empirically** against the exact driver this repo
  uses (`modernc.org/sqlite v1.49.1`, default import, FTS5 compiled in,
  no build tags): prefix queries, porter stemming, phrases, `snippet()`,
  and BM25 column weights all work; a prefix query over 100k rows answered
  in ~2ms. At gallery scale (thousands of rows) this is effectively
  instantaneous and there is no extra service to run.
- Ranking is **strictly two-tier**, per the requirement ("original prompts,
  maybe enhanced after that"): every image whose ORIGINAL prompt matches
  ranks above every image that matched only via the enhanced prompt. The
  verification also PROVED the blended alternative fails: with
  `bm25(images_fts, 8.0, 1.0)` alone, a test row whose original prompt said
  "shrew" once but whose enhanced prompt repeated it ten times outranked the
  genuine "shrew comin in hot" original — column weights cannot beat term
  frequency. Tiering is therefore structural, via FTS5's column-filter
  MATCH syntax (verified):

```sql
-- tier 1: terms match in the original prompt column.
-- NOTE (verified): the filter expression MUST be parenthesized — the bare
-- `{original_prompt}: t1 t2` form binds the column filter to only the FIRST
-- term, leaking rows whose original matches t1 but not t2 into tier 1.
-- `{original_prompt}: (t1 t2)` applies the filter to every term.
SELECT i.* FROM images_fts JOIN images i ON i.rowid = images_fts.rowid
WHERE images_fts MATCH '{original_prompt}: (?)' AND i.hidden = 0
ORDER BY bm25(images_fts, 8.0, 1.0), images_fts.rowid;

-- tier 2: everything else that matches at all
SELECT i.* FROM images_fts JOIN images i ON i.rowid = images_fts.rowid
WHERE images_fts MATCH '?'
  AND images_fts.rowid NOT IN (SELECT rowid FROM images_fts
                               WHERE images_fts MATCH '{original_prompt}: (?)')
  AND i.hidden = 0
ORDER BY bm25(images_fts, 8.0, 1.0), images_fts.rowid;
```

  (bm25 still orders WITHIN each tier, so original-heavy phrasing also wins
  inside tier 1; the rowid tiebreak keeps ordering deterministic for the
  stitched offset-based paging cursor. Tier stitching pages tier 1 to
  exhaustion, then tier 2 — an offset-per-tier cursor `1:<n>|2:<m>` —
  documented on searchCursor in the implementation. True cross-tier keyset
  would have to round-trip the bm25 float through the cursor, which is
  unsafe. Standing gotchas: external-content FTS exposes only its own
  columns — always join back to `images` on rowid; auxiliary functions like
  `bm25(images_fts, …)` must reference the table by its un-aliased name;
  `NOT` is a binary operator in FTS5 query syntax, so tier exclusion is a
  `rowid NOT IN (…)` subquery, not a `NOT {col}: term` prefix.)
- Prefix index (`prefix='2 3 4'`) makes as-you-type token prefixes match
  ("shre" → shrew); porter stemming means "shrews" finds "shrew".
- Phrase queries via quoted input pass through to FTS5 (`"walkin down"`).
  Terms with no FTS match fall back to LIKE substring scan (this catches
  mid-word substrings FTS can't).
- With external-content FTS5 the index keeps tokens of soft-deleted rows,
  so search joins back to `images` for `hidden = 0` filtering (and the
  UPDATE/DELETE triggers run the FTS 'delete' dance) — covered explicitly
  in `search_test.go`.
- Results use the same card component as gallery; highlighted snippets via
  FTS5 `snippet()` with custom markers.

## Security & caching

- All HTML output through `html/template` auto-escaping — prompts and
  reasoning are LLM/user text and MUST never hit the page raw. JSON viewer
  content is template-escaped inside `<pre>`.
- No cookies, no sessions, no CSRF surface (read-only public + header-authed
  mutations from dave only).
- Uploads: `X-API-Key` constant-time compare, size cap, rate bucket,
  filename sanitization, `Content-Type` derived from magic bytes (not
  client's claim), stored files have no execute bits.
- Content-addressed routes get `immutable` year-long cache; HTML stays
  no-cache for liveness. `X-Content-Type-Options: nosniff` everywhere.
- SSE connections capped per IP (e.g. 8) as a cheap guard.

## img-mcp changes (upload side)

Larger than it looks at first glance — two of the provenance fields don't
exist inside img-mcp today, so this milestone is its own task:

1. `UploadConfig` gains `api_key` (loaded, hot-reloadable, logged-on-missing).
   `upload.url` now points at the new site.
2. **Provenance plumbing** (`network`/`channel`/`nick`):
   - New `_dave_inject_channel` / `_dave_inject_nick` input fields on all 4
     generation tool inputs (`generate_image`, `generate_image_async`,
     `enhance_and_generate`, `enhance_and_generate_async`), alongside the
     existing `_dave_inject_network`. **All must use `omitempty`** —
     jsonschema-go marks non-omitempty fields `required` and the SDK server
     then rejects callers that omit them (this exact bug has shipped twice:
     `llm_generated`, then `_dave_inject_network`).
   - New `JobInput` fields (Network/Channel/Nick) + threading through all 4
     `Submit` call sites in tools.go; `input.Network` is currently consumed
     by `applyNetworkPolicy` and discarded.
   - New img-mcp jobs-table migration adding `network`, `channel`, `nick`
     columns so restart recovery (which rebuilds `Job` from the DB) doesn't
     lose provenance mid-job.
3. `uploadImage` signature change: it currently receives only
   `(cfg Config, data []byte, filename string)` — it gains a meta parameter
   (or the call sites build it). Both upload call sites in `processJob`
   (queue.go ~781 for `url` format, ~790 for `both`) and the direct
   `upload_image` tool handler (tools.go ~567) pass a `meta` built from the
   `Job`; the direct tool path sends a thinner meta (no enhancement fields).
4. **Response handling simplified** (this is the payoff for owning both
   ends): `uploadImage` POSTs, parses the `201` JSON, and returns
   `resp.url` as the image URL — full stop. Deleted outright:
   the 303/Location parsing, the client-side `/<id>/orig/<filename>`
   derivation, the verification GET with its redirect-check, and
   `uploadHTTPClient`'s no-follow-redirect constraint (a plain client with
   a sane timeout is enough). The DESIGN NOTE in upload.go explaining the
   verification hop goes away with it.
5. `upload_test.go` cases extended: meta marshaling, api-key header present,
   401 handling.

## Frontend structure

No artificial constraint on the frontend: start with a small hand-rolled
ES-module layer, and graduate to a real toolchain (Vite + Preact, or
similar) whenever it earns its keep — e.g. if the gallery grows virtualized
scrolling, view transitions, or richer image viewers.

```
imgsite/web/
  style.css          dark-friendly default
  app.js             module entry: boot(), wires pages by <body data-page>
  sse.js             EventSource wrapper w/ since/replay + visibilitychange
  gallery.js         infinite scroll, live prepend, +N pill, thumb swap
  image.js           neighbors prefetch, keyboard nav, live next-button
  search.js          debounced fetch + history.replaceState
```

The one hard rule that survives any toolchain choice: **the server renders
complete HTML for every state** (page 1, results, image page) and JS only
augments. That keeps curl/noscript sane, makes the pages shareable as
plain links, and means whatever frontend layer exists is a progressive
enhancement, not a requirement. If/when a build step is adopted, built
assets are committed or embedded via `go:embed` so the single-binary
deployment story doesn't change.

## Testing

- `extract_test.go`: golden tests against the two verified production
  fixtures (checked into `testdata/` as regenerated minimal webp files with
  identical chunk structure) — plain (526-style) and enhanced-with-reasoning
  (528-style); plus adversarial cases: little-endian TIFF, `workflow:`-only
  payload, truncated EXIF, no EXIF chunk, odd-length chunk padding.
- `workflow_test.go`: extraction from the checked-in
  `mcps/img-mcp/workflows/*.json` graphs (different node-ID numbering!) to
  prove ID-agnosticism; ConditioningZeroOut vs CLIPTextEncode negative;
  LoRA graph (z_image_turbo has `LoraLoaderModelOnly`).
- `upload_test.go`: httptest server — auth, oversize, dedupe→two-ids-one-hash,
  meta merge policy table, EXIF-fallback path, 201 JSON shape (url/page
  absolute, id/filename echoed).
- `thumbs_test.go`: pending→ready lifecycle, restart re-scan, failed decode.
- `search_test.go`: tier separation (original beats enhanced on same term),
  the **enhanced-spam fixture** (one original mention vs ten enhanced repeats
  must stay in tier 1 above tier 2 — the exact case that broke blended bm25
  in verification), tier-2 exclusion via `rowid NOT IN`, prefix, phrase,
  LIKE fallback, snippet output, `hidden=0` filtering.
- `events_test.go`: fan-out, overflow reset, replay ring.
- Conventions: testify assert/require, table-driven, `go fmt ./…` +
  `go vet ./…` + `go test ./…` green before done.

## Milestones

1. **Skeleton + upload + direct links** — config, migrations, store, `/updo`
   (auth/hash/201 JSON), `/<id>/orig/<filename>` direct serving with
   immutable caching. Done = dave can upload and the URL works in IRC.
2. **Extraction + details page** — extract.go/workflow.go, `/<id>` page with
   full details panel, prev/next nav (static), workflow viewer.
3. **Gallery + thumbnails** — grid page, background worker, placeholders,
   keyset infinite scroll.
4. **SSE live updates** — hub, gallery prepend/swap, live next-button.
5. **Search** — FTS5, ranking, snippets, as-you-type UI.
6. **img-mcp switch-over** — provenance plumbing (`_dave_inject_channel`/
   `_dave_inject_nick` with `omitempty`, `JobInput` fields, jobs migration),
   api_key + meta in `uploadImage`, prod config flip, docs (AGENTS.md
   architecture entry + example.toml reference update).
7. **Polish** — og tags, soft-delete + admin delete, favicon/empty states,
   logxi request logging.

Milestone 6 is the only coupling point — the new JSON protocol and the old
303/derive/verify code can't talk to each other — so the site and the
img-mcp change deploy together: stand the site up, ship the img-mcp
uploadImage rework, flip `upload.url` + key in one go. (Milestones 1–5 can
still be developed and demoed against a hand-rolled curl client before any
dave-side change exists.)

## Future ideas (explicitly out of v1)

- Backfill/import from img.zkpq.ca history.
- "Related images" by FTS similarity on the details page.
- Per-workflow or per-channel gallery filters (fields already stored).
- WebP/AVIF thumbnail encoding once a zero-CGO encoder is blessed.
- Re-run-with-seed (deep link to ComfyUI with the workflow JSON — it's
  already stored verbatim).
