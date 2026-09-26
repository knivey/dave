# Image Site (imgsite) — Design & Feature Plan

Status: implemented (2026-09-24) — milestones 1–7 shipped in `imgsite/` with the
img-mcp switch-over; each milestone was independently reviewed. Legacy-output
import (`imgsite -import`) added 2026-09-25 (see "Importing legacy outputs"),
CLI soft-delete (`imgsite -delete`) the same day (see "Admin deletion").
Grounded in verified production artifacts: two live webp uploads from
img.zkpq.ca (`526`, `528`) were dissected to pin down the exact EXIF/workflow
embedding; details in "Input format (verified)".

## Goal

Replace the third-party photo site (img.zkpq.ca) with a self-hosted gallery
purpose-built for dave's image generations. The site:

- serves **permanent links** — the upload response hands dave the exact
  ready-to-paste details-page URL (plus the direct image link), no
  client-side derivation or verification,
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

`enhancement_reasoning` is present only on enhanced jobs. With the
safe-site pipeline the payload grows four more OPTIONAL fields —
`network`, `channel`, `nick` (provenance, mirroring the upload meta) and
`safety` (`"safe"`/`"unsafe"`; baked in post-generation by img-mcp's EXIF
note rewrite) — absent on every legacy note and parsed permissively:
extraction takes a `safety` value only when it is exactly a verdict, so
an unparseable value degrades to `unknown` and never elevates. A fifth
optional field, `nsfw` (first-pass sexual-content flag from img-mcp's
enhancement call; present only as `true`, absent = no signal), is parsed
onto the extracted struct for future site use — no column behind it yet.
Everything else the
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
  import.go          `imgsite -import <dir>`: offline legacy-output ingestion
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
display_width = 1280      # og:image width (px)
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

# [safe_site]  # OPTIONAL — omit the whole section for single-site behavior
# hosts = ["safe.example.com"]     # matched case-insensitively against the
#                                  # request Host (port stripped); anything
#                                  # else is the default site
# base_url = "https://safe.example.com"
# allowed_networks = ["libera"]    # compared case-insensitively against the
#                                  # stored provenance network
```

Reloadable via SIGHUP: `site.*`, `safe_site.*`, `thumbnails.*` (except
worker count), `search.*`, `upload.*` (both `max_bytes` and
`rate_per_minute` are read per request). Not reloadable
(restart required): `server.*`, `database.*`, `storage.*`, `auth.*`.
Same pattern as img-mcp.

## Safe-site host split

One process serves two logical sites, selected by the request Host
header (design: `docs/superpowers/specs/2026-09-26-safe-site-design.md`).
The **default site** (`server.base_url`) shows all non-hidden images —
today's behavior. The **safe site** (optional `[safe_site]`) shows only
non-hidden images that pass the visibility predicate:

```sql
AND (LOWER(i.network) IN (/* lowercased allowed_networks */)
     OR i.safety = 'safe')
```

`'unknown'` and `'unsafe'` verdicts fail the predicate — default-deny —
and NULL/empty provenance (imported/legacy/direct uploads) is reachable
only via `safety = 'safe'`. Site-aware surfaces, all resolving the site
once per request via the Host header:

- Gallery page + fragment (`dbGetGalleryPage`) and search page +
  fragment — the predicate lands as an outer AND on the images row in
  all three search tiers (FTS 1, FTS 2, LIKE 3 including the trigram
  prefilter set; MATCH expressions untouched).
- Details page and the neighbors API (`/api/images/<id>/neighbors`) —
  a safe-host "next" never navigates into an invisible image; an
  invisible id 404s exactly like an unknown id (no existence hint).
- Asset routes — original, thumbnails, and thereby the download target
  (the download anchor targets the orig route) — invisible ids 404 on
  the safe host only; the same ids serve normally on the default host.
- `og:image` / `og:url` (and the copy-link button) build from the
  current site's base URL — `safe_site.base_url` on the safe host.
- SSE `/events` — the subscriber is site-tagged at subscribe time;
  `image-new` / `thumb-ready` carry a publish-time visibility flag on
  the ring entry, so live delivery AND replay filter per site without
  DB reads. `image-hidden`, `hello`, and `reset` go to all sites
  (dropping an absent card is a client no-op).
- Upload responses — when the upload meta's `network` is in
  `allowed_networks`, BOTH `page` and `url` build from
  `safe_site.base_url`, so dave pastes safe links into Libera channels
  with zero dave/img-mcp changes (see "Upload protocol").

Manual marking of verdicts the pipeline leaves `unknown` (pre-safety
history, vet failures): `imgsite -safety <id>… safe|unsafe|unknown` —
see "Set safety verdicts (admin)" in imgsite/README.md.

### Rollout checklist (as shipped)

1. Deploy both rebuilt binaries. imgsite runs migration 003
   (`images.safety TEXT NOT NULL DEFAULT 'unknown'`); img-mcp runs
   migration 004 (`jobs.safety`, insert/recovery-read only).
2. Add `[safe_site]` to imgsite's config — SIGHUP applies it live; an
   absent section keeps single-site behavior.
3. Point DNS + proxy for the safe hostname at the same imgsite; the
   Host header must pass through (nginx default).
4. img-mcp config: append the nsfw instruction line to the LOOSE
   enhancement prompts (first pass; never the libera-safe one), add
   `[enhancement.safety-vet]` with the owner's judging prompt (returns
   `{"safe": bool, "reason": "…"}`), and add the `[safety]` block
   (`skip_networks` default `["libera"]`, kept in correspondence with
   imgsite's `allowed_networks` by convention). All hot-reloadable.
5. Backfill is automatic: existing libera-provenance rows become
   safe-host-visible by the origin rule the moment the config lands.
   Everything older without provenance stays `unknown`
   (default-deny) until marked with `-safety`; new uploads classify
   themselves via the img-mcp pipeline.

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
  -- LLM safety verdict for the safe-site host split (migration 003)
  safety   TEXT NOT NULL DEFAULT 'unknown',   -- 'unknown' | 'safe' | 'unsafe'

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

#### Safety column (migration 003)

`safety` holds the LLM safety classification: exactly `'unknown' | 'safe'
| 'unsafe'`, `NOT NULL`, never `''` — every writer normalizes. `'unknown'`
is **default-deny**: it is the migration default for all pre-existing
rows and means "no verdict yet", so nothing but an explicit verdict ever
writes the column. Sources of a verdict, in order: upload `meta.safety`
(handler rejects anything but `safe`/`unsafe`/absent with a 400 —
img-mcp is the only writer, this is a tripwire not a defense), the
`dave_original_prompt` note payload baked into the image's EXIF
(unparseable values degrade to unknown, never elevate), and the
`-safety` admin CLI (`imgsite -safety <id>… safe|unsafe|unknown`,
offline manual marking — see "Set safety verdicts (admin)" in
imgsite/README.md). `dbUpdateImageMetadata` and re-extract **never
reset** a stored verdict — the UPDATE is guarded so an empty payload
value keeps the stored one — and re-extract additionally *backfills*
`'unknown'` (and empty provenance) from the note. No index at gallery
scale; revisit past ~50k rows.

### Public IDs

Random 7-char base62 (`[0-9A-Za-z]{7}`), collision-checked with a retry loop
against the PK. Reads well in URLs (`/aQ3f9xK`), doesn't leak volume, and
gives ~3.5 trillion space — collisions are a non-event in practice. Job IDs
(img-mcp's `ed974b6d`) are stored as metadata, not used in URLs.

### Keyset pagination

Neighbors, gallery paging, and search paging key on `(created_at DESC, id
DESC)` — never OFFSET — so live prepends can't shift pages under the user.
`created_at` is stored as UTC text with **millisecond precision**
(`YYYY-MM-DD HH:MM:SS.mmm`): seconds are not enough — ids are random, so
same-second completions (real with `max_workers > 1`) would order
arbitrarily. Legacy second-precision rows (and their cursors) still parse
(dual-layout `parseDBTime`) and sort before ms rows in the same second —
at most a one-second inversion for pre-migration data. Go renders RFC3339
for APIs/SSE. The `id DESC` tiebreak remains for exact-duplicate
timestamps. Cursor wire format: `?after=YYYY-MM-DD%20HH%3AMM%3ASS.mmm~<id>`
(ms included, `~` separator; the timestamp is URL-encoded as one component).

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
| `POST /admin/reextract` | re-run workflow extraction from stored `workflow_json` under current rules (X-API-Key) — heals rows when extraction improves; preserves provenance/visibility/safety, backfills EMPTY provenance + `unknown` safety from the note payload, skips unparseable rows | — |
| `DELETE /api/images/<id>` | soft-delete → `hidden=1` (X-API-Key); CLI equivalent: `imgsite -delete <id>[,<id>…]` (see "Admin deletion") | — |

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
  "network": "libera", "channel": "#dave", "nick": "knivey",
  "safety": "safe"
}
```

`safety` (omitempty, safe-site pipeline): `"safe"` or `"unsafe"` when the
classification resolved, omitted when it didn't (the column then stays
`'unknown'` — default-deny). The handler rejects any other value with a
400; img-mcp is the only writer, so this is a tripwire that surfaces
writer-side bugs, not a defense.

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
7. Respond `201 Created` with JSON — nothing derived client-side:

```json
{
  "id": "aQ3f9xK",
  "url": "https://img.example.com/aQ3f9xK/orig/2026-09-23-234552__0.webp",
  "page": "https://img.example.com/aQ3f9xK",
  "filename": "2026-09-23-234552__0.webp"
}
```

   `url` and `page` are both absolute. `url` is the permanent
   direct image link; `page` is the details-page link. Since the image pages
   went live (Sep 2026), dave consumes `page`: img-mcp stores it verbatim
   into the job result and pastes it to IRC, so a pasted link lands readers
   on the details page (prompt, params, provenance — plus OpenGraph embeds)
   instead of bare bytes. img-mcp prefers `page` and falls back to `url`
   (with a WARN) only for older imgsite deployments that answer with an
   empty `page`.

   Base selection is origin-aware when a `[safe_site]` is configured: an
   upload whose `meta.network` (case-folded) is in
   `safe_site.allowed_networks` gets BOTH `url` and `page` built from
   `safe_site.base_url`, so the link dave pastes into a Libera channel
   points at the host that serves the filtered view — img-mcp and dave
   change nothing. Every other upload (other networks, absent network, or
   no `[safe_site]` section at all) keeps `server.base_url` — which
   therefore must still be set correctly in prod config — byte-identical
   to the single-site behavior. The derive-from-request fallback for an
   empty `server.base_url` applies to whichever base won the selection;
   `safe_site.base_url` is required non-empty by validation, so only the
   default side can ever derive.

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

`safety` (and, since the safe-site pipeline, note-carried provenance)
follows the *provenance* pattern, not the quartet one: the meta side's
verdict wins ('safe'/'unsafe' only; anything else, including `'unknown'`,
is "no verdict" and never writes the column), and the EXIF note payload
**backfills only what the meta side left empty**. That one rule serves
both callers through the shared merge: at upload, "meta omitted it";
at re-extract — where the stored columns are round-tripped into the
meta slot by `rowToUploadMeta` — "stored column empty", which yields
the preserve-non-empty policy for free (a stored verdict, e.g. an
admin's `-safety` mark, survives a disagreeing note; an `'unknown'`
heals from it).

## Importing legacy outputs (`imgsite -import`)

Offline, one-shot ingestion of an existing ComfyUI `output/` archive into
the same DB + store the upload path uses:

```bash
imgsite -import /path/to/ComfyUI/output [-tz America/New_York] [config.toml]
```

Run it with the **server stopped**. SQLite WAL technically tolerates a
second writer, but a concurrent import against a live server risks lock
contention under busy_timeout; the stopped-server recommendation keeps
the import deterministic. No SSE events are published (the hub is not
running) — imported images simply appear on the next page load.

Behavior:

- **Recursive walk** (ComfyUI organizes outputs into date subfolders).
  Only webp and PNG — the two containers the extraction pipeline reads —
  are considered; everything else is skipped with a one-line notice.
- **Per file**: extract the embedded workflow via the standard pipeline;
  no workflow found (or an unparseable graph) ⇒ skip with a report line
  (visible and reversible — the owner decides what to do with those).
  Compute sha256 and COPY the bytes into the content-addressed store —
  the import is non-destructive, originals are never moved or deleted.
- **Idempotent**: a sha256 already present in the DB (uploaded via dave
  previously, or a prior import run) is skipped — re-running the import
  is a report-only no-op. Hidden rows count as present, so a soft-deleted
  entry cannot resurrect itself. (This differs from the upload path,
  which deliberately mints a second gallery row for the same bytes: each
  IRC generation is its own entry, while imports must be idempotent.)
  The reverse interplay is likewise intended: importing first and having
  dave later upload identical bytes mints a second gallery row sharing
  the one stored file — the upload path's pre-existing
  dedupe-shares-the-file-not-the-row semantics, unchanged by the import
  feature.
- **created_at comes from the FILENAME**, not mtime (mtimes change when
  files are copied/moved). ComfyUI's leading `%Y-%m-%d-%H%M%S`
  (`2006-01-02-150405`, e.g. `2026-09-24-185355__0.webp`) is parsed and
  stored as UTC ms-precision text; everything after the time prefix is
  ignored for parsing. Filename times are the ComfyUI host's local time:
  `-tz` declares that zone (any IANA/`time.LoadLocation` name), defaulting
  to the importing machine's local zone. A `__N` batch suffix adds N
  milliseconds (clamped to 999) so same-second batches keep their file
  order under the ms-precision keyset ordering. Names that don't match
  fall back to the file's mtime (UTC) with a per-file warning in the
  report — never aborting the batch. Files carrying a dave note that
  also parse a filename time use the filename time (the note has no
  timestamp; consistent batch ordering wins).
- **Prompts**: images WITHOUT a dave original-prompt note — the oldest
  archive material — import with an EMPTY `original_prompt` and the
  workflow's final positive prompt (the same node extraction uses for
  the enhanced side) in `enhanced_prompt`. They are therefore searchable
  exactly like existing enhanced-only matches: FTS tier 2 by design,
  strictly below any original-prompt hit. Images WITH a note import with
  full fidelity through the same extraction path (original prompt,
  reasoning, job_id, llm_generated).
- **Rows** go through the same merge/insert code as uploads
  (`mergeUploadMetadata` + `applyMergedMetadata` + `dbInsertImage`), so
  FTS trigger rows, provenance NULL semantics, and column meanings are
  identical. Provenance (network/channel/nick) is empty, job_id empty
  unless a note carries one, `llm_generated` from the note when present
  else false, `meta_source='exif'`. Width/height come from the graph's
  latent node when present (upload parity); NULL dims are backfilled by
  the thumbnail pass below.
- **Thumbnails + dims without hand-holding**: rows are inserted
  `thumb_status='pending'`; after every row is committed, the import
  runs the worker's own per-row pipeline (`thumbWorker.process` —
  decode → resize → encode → `dbUpdateThumbReady`, which
  COALESCE-backfills NULL width/height from the decoded bounds) inline.
  This matters because the startup re-scan's enqueue is non-blocking
  against a bounded queue (cap 256): relying on "next server start"
  alone would need several restarts for a large archive. An interrupted
  import is still safe — unprocessed rows stay pending and the next
  server start finishes them. Undecodable files get the worker's normal
  terminal `thumb_status='failed'` (gallery falls back to the original
  URL); the row and its prompts/searchability survive.
- **Summary + exit code**: the run prints per-file lines as it goes and
  a summary at the end — imported / skipped-duplicate /
  skipped-no-workflow / skipped-other counts, errors, thumbnail
  outcomes, warnings, and the DB + store paths written to. Exit status
  is non-zero only for fatal setup errors (bad `-tz`, bad dir, config or
  DB open failure); per-file skips and thumbnail failures never fail the
  run.

## Admin deletion (`DELETE /api/images/<id>` · `imgsite -delete`)

Two surfaces, one DB path: every hide funnels through `dbHideImage`'s
guarded `UPDATE images SET hidden = 1 WHERE id = ? AND hidden = 0`
(exactly one winner per row, rows-affected reported), so the CLI can
never diverge from the wire path's semantics.

- **HTTP** (server running): `DELETE /api/images/<id>` with the same
  X-API-Key as `/updo`. 200 hidden by this request · 410 already hidden
  · 404 unknown/malformed id · 401 bad key · 500 DB failure. Publishes
  `image-hidden` SSE after the UPDATE commits, so connected browsers
  drop the card live.
- **CLI** (offline like `-import`): `imgsite -delete <id>[,<id>…]
  [config]` hides one id or a comma-separated list through the same
  `dbHideImage` call, with the same 404/410 distinctions as per-id
  report lines (`not found: <id>` / `already hidden: <id>`), a prompt
  snippet + "files retained" per success, and a summary with the
  reversibility note. No confirmation prompt — the operation is a
  reversible soft hide. No SSE hub runs in CLI mode: run with the
  server stopped, or accept that connected pages keep the card until
  their next load. `-delete` is mutually exclusive with `-import`
  (usage error, exit 2).

Shared semantics:

- Soft delete only — `hidden=1`, files retained forever (content-
  addressed storage is shared by dedupe, so bytes are never purged);
  hidden rows answer 410 on every id-keyed route and are filtered from
  gallery/search via the `hidden = 0` join-back.
- Reversible: `UPDATE images SET hidden=0 WHERE id='<id>'` (the CLI
  summary prints this reminder after every run). There is no dedicated
  unhide endpoint.
- CLI exit codes: `0` only when every requested id was hidden by this
  run; any not-found / already-hidden / per-id error ⇒ `1` (mirroring
  the 404/410 statuses); usage mistakes (empty or malformed id list,
  `-delete` combined with `-import`) ⇒ `2`. Malformed ids are rejected
  up front by shape (`^[0-9A-Za-z]{7}$`) — a well-formed but unknown id
  is the runtime "not found" line instead.

## Metadata extraction (extract.go / workflow.go)

1. **Container scan**: RIFF walk (`VP8X`, `VP8 `, `EXIF` chunks, odd-size
   padding) → EXIF bytes. PNG fallback for future workflows: `tEXt` chunks
   keyed `prompt`/`workflow` (ComfyUI's standard SaveImage convention).
2. **TIFF IFD0 walk**: big- or little-endian; collect ASCII entries; look for
   payload prefix `prompt:` (API graph) and `workflow:` (UI graph, present
   when `save_workflow_as_json=true`; ignored by us). Strip trailing NUL.
3. **Graph traversal** (`workflow.go`) — class_type-keyed, ID-agnostic:
   - `dave_original_prompt` node → JSON payload → original prompt, llm_generated, job_id, reasoning; optional safe-site fields `network`/`channel`/`nick` and `safety` (verdict-gated: only `safe`/`unsafe` parse, anything else degrades to empty) plus `nsfw` (first-pass flag, parsed `*bool` for future use — no column, no merge consequence)
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
  (1280w — `og:image`; the details page's main `<img>` serves the original
  bytes directly since the Sep 2026 zoom change — see "Image details").
- Failure (corrupt exotic input, decode bomb): mark `thumb_status='failed'`,
  gallery falls back to the original URL as `src` — worst case slower, never
  broken. Decode guarded by `max_dimension`.
- On completion: update row, publish `thumb-ready` SSE.
- JPEG has no alpha and the stdlib encoder drops the channel, compositing
  transparency onto black. Deliberate: every image context on the site is
  itself near-black (`#0d0d0f` card/viewer backgrounds), so a flattened
  thumbnail is visually identical to the original rendered in place, and
  production inputs are opaque regardless (lossy VP8 webp decodes to
  `*image.YCbCr`).
- Gallery renders pending thumbs as a shimmer placeholder; `gallery.js`
  swaps shimmer→thumb on the `thumb-ready` SSE event, and any successful
  load clears the shimmer (covers replayed cards and retries that land
  after the event). Pending-thumb 404s drive an error-listener retry
  loop: server-rendered cards (which carry `data-orig`) retry once
  (~2s) then fall back to the original bytes; SSE-prepended cards have
  no orig URL in the event payload, so they stay shimmer and keep
  retrying with capped exponential backoff (2s → 30s) until the thumb
  appears. (The earlier policy — stop retrying, drop the src, keep the
  card non-pending — rendered a dead near-black box that the
  `thumb-ready` swap, which targets `img.pending`, could never heal
  whenever generation outlasted the single retry window; observed in
  production Sep 2026.)
- **Thumb-heal invariant** (the `thumb-ready` handler): the `pending`
  class is the error listener's retry-arm flag and may be cleared ONLY
  by a successful `load` (capture-phase load listener) or by the
  `data-orig` terminal fallback — never by `onThumbReady`. The handler
  instead FORCES a real refetch (cache-busting `?v=` query on the thumb
  URL) and lets load/error resolution do the rest, retaining `pending`
  throughout. This is self-healing in both orders: if the forced fetch
  200s, the load listener clears pending (a late error from a
  superseded in-flight 404 lands on a non-pending img and is ignored);
  if it 404s anyway, the error listener still sees a retryable pending
  img and re-arms the backoff chain. Rationale, engine-verified in
  Chromium (probe: `~/dev/imgsite-debug/probe-restart.ts`; behavioral
  harness: `repro-thumb-race.ts`): assigning an unchanged `src` is a
  no-op (no refetch, no abort — the in-flight request keeps ownership),
  and `removeAttribute("src")` + re-assign in the same task dedupes to
  the same no-op; only a URL change reliably restarts the load. The
  pre-fix handler stripped `pending` and reassigned the same URL — when
  a retry's 404 was in flight at `thumb-ready` time (watched tab, slow
  resize host), the 404 then landed on a non-pending img, the error
  listener early-returned, and the card died unrecoverably (black box,
  no retries, shimmer gone) even though the server thumb was ready.
  Query strings are ignored by the path routes and pending 404s are
  no-cache, so the bust only costs one immutable cache entry per race
  event. Guarded in-repo by `TestGalleryThumbHealInvariant`
  (server_test.go) against the served JS source.

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
event: hello                (connect-time bookkeeping — seeds the client's
data: {"last_id":42}         last-seen id; no page handler fires for it)

event: image-new
data: {"id":"aQ3f9xK","created_at":"2026-09-24T03:12:00Z","original_prompt":"shrew…",
       "thumb_status":"pending","page_url":"/aQ3f9xK"}

event: thumb-ready
data: {"id":"aQ3f9xK","thumb_status":"ready"}

event: image-hidden          (soft delete — clients drop the card)
data: {"id":"aQ3f9xK"}
```

Client behaviors:

- **Connection lifecycle** (sse.js): pages render with an embedded SSE
  cursor — `body[data-last-event]` carries the hub's event id captured
  BEFORE the page's DB query (overlap-safe, gap-unsafe ordering: an
  event between capture and query can land in both the page and the
  replay window, where the client's data-id dedup absorbs it; one
  between query and capture would land in neither — the render→subscribe
  race). The FIRST connect of a page that carries the cursor uses
  `?since=<embedded>`, so the existing replay ring bridges everything
  published between the render and the subscribe; 0 is a meaningful
  cursor (hub present, nothing published yet — replay it all). Pages
  without the attribute (nil-hub renders) keep a live-only first
  connect. The embedded value also seeds the tracked last-seen id, so
  hello (which still takes the max) and every later open stay
  consistent. Every LATER open (visibilitychange, bfcache restore,
  429 backoff) resumes with `?since=<lastId>` — including `since=0`
  when no event was ever received, which replays the entire ring (or
  resets, correctly, when even that can't reconstruct the gap). The
  server sends a connect-time `hello` bookkeeping event
  (`{"last_id":N}`) on EVERY subscription so a client always knows the
  stream's current position, even before its first real event — that
  seed is what makes the zero-event reopen lossless. **Reset-loop
  bound**: a stale embedded cursor whose gap exceeds the ring yields
  `reset` → `location.reload()` → the reload re-renders with a FRESH
  cursor (capture is at render time, so the new gap is ~zero) — in
  practice at most one reload per stale page: a repeat would need the
  ring (>128 events) to overflow within the reload's own sub-second
  render→subscribe window, far beyond this site's traffic, but it is
  not logically impossible. Unconditional `since=0` on fresh
  loads would instead loop forever on a busy hub — which is why the
  no-attribute fallback stays live-only. Fatal-error (429) backoff
  retry; server `reset` event → full reload. **bfcache** — pagehide
  closes the stream and `pageshow` with `persisted=true` reopens it
  (clicking into an image page and pressing back freezes the gallery
  into the back/forward cache; the browser tears the EventSource
  socket down with no error event and usually no visibilitychange on
  restore, so without the pageshow hook every later arrival is
  silently missed).
- **Cursor coverage**: all three full-page templates embed
  `body[data-last-event]` — the gallery, the `/search?q=` results page
  (its replayed arrivals buffer behind the "+N new" pill exactly like
  live arrivals during a filter), and the image details page (where a
  gap event would otherwise never reveal the live next-chevron).
  Fragments (`/gallery?after=`, `/search-fragment`) deliberately carry
  no cursor: they are swapped into a page whose SSE stream already
  exists — the cursor belongs to the page body, not the fragment.
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
- Search box pinned to header — debounce 300ms → fetch `/search-fragment?q=`
  fragment (`/search` is the full page + no-JS form target), replace grid,
  keep SSE wiring live. Superseded fetches are aborted (AbortController);
  a stale response can never overwrite a newer swap (mode check).

### Image details (`/<id>`)

- Main image = the ORIGINAL bytes (`/<id>/orig/<filename>`), not the
  display thumb (owner request, Sep 2026 — uploads are already webp and
  small enough; the display derivative remains `og:image`'s source).
  Zoom is a **fullscreen overlay** (owner redesign after the in-place
  2fedc24 zoom shipped three production bugs: a height-squashed fit
  state, chevron hit-zones stacked over the image, and a "did nothing"
  zoom for screen-smaller images):
  - **fit state** — the zoom-toggle `<button>` IS the sized box: inline
    `aspect-ratio: W / H` + a single width cap
    `max-width: min(100%, Wpx, calc(85vh * W / H))` (rendered as a
    pair from the DB dims only when BOTH are known — the layout-shift
    guard: the box reserves the right shape before the bytes load),
    with the img filling it at `width/height: 100%;
    object-fit: contain`. The box cannot lose its ratio because the
    width-cap expression itself encodes the ratio: the `calc()` term
    is the width at which the ratio-derived height reaches exactly
    85vh, so the used width is always the ratio-correct minimum of the
    three constraints and the height cap never binds on this path (a
    first cut shipped `width: 100%` + a separate `max-height: 85vh` —
    engines do not transfer a cross-axis max-height violation back
    into a definite main-axis width, so the box went misshapen even
    though contain kept the painted image correct, leaving wide
    invisible click/cursor dead zones beside the letterbox). The
    original 2fedc24 bug was the same class one level down:
    width/height ATTRIBUTES on the img (presentational hints = definite
    sizes, so the attr-derived ratio had no auto dimension to resolve
    against) under dual `max-width`/`max-height` caps that clamped the
    axes independently and squashed the PAINTED image. contain stays as
    the second line of defense — if stored dims drift from the actual
    bytes, the img letterboxes instead of stretching. Small images cap
    at natural px, so the fit state still renders them 1:1. NULL-dims
    rows take a `.nodims` fallback (img `width/height: auto` + dual
    caps — safe only because that img ships no attributes).
  - **zoomed state** — click opens a server-rendered, page-covering
    overlay (`position: fixed; inset: 0` over the topnav and chevrons;
    dark backdrop, `role="dialog"` + `aria-modal`, zoom-out cursor, an
    ✕ close button). The overlay img reuses the main img's src
    (browser cache — no refetch) and is sized purely by CSS:
    `width/height: 100%; object-fit: contain`, so larger-than-screen
    images shrink to fit, smaller-than-screen images scale UP to fill,
    both letterbox — zero JS dimension math (the deleted 2fedc24
    scale-up measured the viewer's on-screen box, which is why small
    images only ever grew to box size). The whole panZoom apparatus —
    viewer-box measurement, natural-size classification, `.zoomed`
    classes, load-time re-classification, inline sizing — is gone; the
    "open file" anchor beside download serves full-resolution 1:1
    inspection.
  - **dismiss/close**: any click inside the overlay (backdrop, image,
    ✕) or Esc. `body.zoom-open` locks page scroll behind the backdrop;
    ←/→/n/p nav keys are suppressed while open (Esc is the dismissal
    key); focus moves to the ✕ on open — the CSS transitions
    `visibility` with **0s duration on open** (instant flip) and 0s
    **delayed to the fade's end on close**, because an animated
    visibility keeps computing hidden at transition progress 0 and
    silently no-ops `focus()` even a frame after the class flip; the
    focus call itself is deferred one `requestAnimationFrame` so it
    runs after the style recalc picks up `.open`, and returns to the
    toggle synchronously on close (the toggle never transitions, so
    the return focus always lands). The fade is gated behind
    `prefers-reduced-motion`. No focus trap — Esc/click/focus-return
    cover keyboard exit. The toggle is a real
    `<button>` (`aria-haspopup="dialog"` + `aria-expanded`;
    Enter/Space work natively); the overlay ships `hidden` and its CSS
    re-asserts `[hidden] { display: none }` because the author
    `display: flex` would otherwise override the UA rule. Without JS
    the page renders completely — image displays non-linking in its fit
    box, the overlay stays hidden, and the direct-file anchors above
    are real.
- Details panel:
  - **Original prompt** (prominent — it's what the user actually typed)
  - Enhanced prompt (collapsible, default open when it differs from original)
  - Enhancement reasoning (collapsible, default expanded; `<pre>` wrapped)
  - Negative prompt (if any)
  - Params table: seed (click-to-copy), steps, cfg, sampler/scheduler,
    denoise, dimensions, file size/format, models (unet/clip/vae), LoRAs
  - Provenance: timestamp, job_id, workflow name, network/#channel/nick (when provided)
  - Raw workflow JSON (collapsed `<details>` with syntax-pretty JSON)
- **Navigation**: slim prev/next chevron rails flanking the viewer
  OUTSIDE the image (flex siblings of the stage, vertically centered,
  `clamp()`-narrowing on small viewports — never overlapping the image,
  which is what made the 2fedc24 absolute strips' hover/click zones
  contest the zoom button's pixels) + ←/→ keyboard arrows + `n`/`p`
  keys; neighbor URLs server-rendered for no-JS users; live
  next-button per SSE above. When the zoom overlay is open it simply
  covers the rails and nav keys are suppressed.
- Copy-link button (copies `/orig/` direct URL), download link, and an
  "open file" anchor (the raw file opened in the browser — kept one
  click away beside download now that the main image zooms instead of
  linking through).
- **Search bar** in the topnav: a plain GET form (`action="/search"`,
  `name=q`) — Enter navigates to the server-rendered search page. No JS
  module runs a search layer here; image.js's keyboard nav ignores
  keystrokes originating from inputs.
- OpenGraph tags: `og:image` = display thumb, `og:title` = original prompt,
  `og:description` = params summary — pasted links embed nicely in
  Discord/clients that unfurl.

### Search

- **FTS5** is SQLite's built-in full-text engine (an inverted index with
  BM25 ranking — the same family of ranking Elasticsearch/Lucene uses, not a
  `LIKE` scan). **Verified empirically** against the exact driver this repo
  uses (`modernc.org/sqlite v1.49.1`, default import, FTS5 compiled in,
  no build tags): prefix queries, porter stemming, phrases, `snippet()`,
  and BM25 column weights all work, as does the trigram tokenizer
  (substring phrases with case folding — the tier-3 accelerator below);
  a prefix query over 100k rows answered
  in ~2ms. At gallery scale (thousands of rows) this is effectively
  instantaneous and there is no extra service to run.
- Ranking is **strictly three-tier** — two FTS tiers plus an always-on
  substring tier. Tiers 1–2 implement the original requirement ("original
  prompts, maybe enhanced after that"): every image whose ORIGINAL prompt
  matches ranks above every image that matched only via the enhanced
  prompt. The verification also PROVED the blended alternative fails: with
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

  Tier 3 is a **LIKE substring scan that ALWAYS runs** and appends its
  hits after both FTS tiers. Rationale: FTS5 prefix terms match only
  token-INITIAL text and the query syntax has no suffix operator, so
  `shrew` can never find `cowshrew` through the index (a fused token
  whose only relation to the query is a token-final fragment) — only
  `LIKE '%shrew%'` can. The scan excludes every rowid the overall MATCH
  returns (tier 1 ∪ tier 2 is exactly that set), which IS the id-level
  dedupe: a row that both tokenizes and substring-matches keeps its FTS
  rank and appears exactly once. Within tier 3, original-column
  substring matches rank above enhanced-only substring matches, each
  group by recency — there is no relevance rank for a match the
  tokenizer never saw:

```sql
-- tier 3: substring-only leftovers, after all FTS results
-- (t1..tn are escaped LIKE patterns, one per token, ANDed per column;
--  a row qualifies when EITHER column contains every token)
SELECT i.*, (CASE WHEN (original_prompt LIKE t1 ESCAPE '\' AND …)
                  THEN 0 ELSE 1 END) AS like_enhanced
FROM images i
WHERE i.hidden = 0
  AND ((original_prompt LIKE t1 ESCAPE '\' AND …) OR (enhanced_prompt LIKE t1 ESCAPE '\' AND …))
  AND i.rowid NOT IN (SELECT rowid FROM images_fts WHERE images_fts MATCH ?)
ORDER BY like_enhanced, i.created_at DESC, i.id DESC;
```

  The LIKE predicates remain the semantic definition (they also drive
  the `like_enhanced` CASE used for ordering and preview), but the scan
  is no longer linear at scale: a second external-content FTS5 table,
  **`images_substring_fts` (migration 002), indexes the same two prompt
  columns with the trigram tokenizer**, and when EVERY token of the
  query is at least 3 code points the query adds one more conjunct:

```sql
  AND i.rowid IN (SELECT rowid FROM images_substring_fts
                  WHERE images_substring_fts MATCH '"q1" "q2" …')
```

  In a trigram table a quoted string is a phrase of 3-code-point
  windows — a substring test — so `"shrew"` matches `cowshrew` exactly
  like `LIKE '%shrew%'`; quoting also keeps `%`/`_` literal in the MATCH
  expression (no FTS5 syntax injection from user tokens). This is a
  semantics-preserving **superset** filter: any row the LIKE patterns
  accept contains each token as a case-folded substring, which is
  precisely what the trigram index keys on, so no LIKE-visible row can
  be missed; a trigram-only match (e.g. a non-ASCII case variant LIKE
  would reject) simply filters out in the outer query's LIKE conjuncts.
  Verified via `EXPLAIN QUERY PLAN` against the bundled driver: the
  accelerated query consults the trigram table through its index
  (`SCAN images_substring_fts VIRTUAL TABLE INDEX …`) and drives
  `images` by primary key (`SEARCH i USING INTEGER PRIMARY KEY`)
  instead of `SCAN i` — pinned by `TestTier3TrigramPrefilterPlan`.
  If ANY token is shorter than 3 characters (rare: short queries) the
  trigram table cannot index it — a 1–2-char string has no trigram
  window — and the query runs the plain scan exactly as before the side
  table existed. Costs: the trigram index stores every 3-char window,
  roughly 3–5× the indexed text size (two prompt columns — trivially
  small next to the image files), and migration 002 rebuilds the table
  from existing rows once at startup (seconds at gallery scale:
  hundreds → low thousands of rows).

  (bm25 still orders WITHIN each FTS tier, so original-heavy phrasing also
  wins inside tier 1; the rowid tiebreak keeps ordering deterministic for
  the stitched offset-based paging cursor. Tier stitching pages tier 1 to
  exhaustion, then tier 2, then tier 3 — an offset-per-tier cursor
  `1:<n>|2:<m>|3:<k>` — documented on searchCursor in the implementation.
  True cross-tier keyset would have to round-trip the bm25 float through
  the cursor, which is unsafe. Standing gotchas: external-content FTS exposes only its own
  columns — always join back to `images` on rowid; auxiliary functions like
  `bm25(images_fts, …)` must reference the table by its un-aliased name;
  `NOT` is a binary operator in FTS5 query syntax, so tier exclusion is a
  `rowid NOT IN (…)` subquery, not a `NOT {col}: term` prefix.)
- Prefix index (`prefix='2 3 4'`) makes as-you-type token prefixes match
  ("shre" → shrew); porter stemming means "shrews" finds "shrew".
- Phrase queries via quoted input pass through to FTS5 (`"walkin down"`).
  Substring-only matches (mid-word fragments, suffixes inside fused
  tokens like "cowshrew") surface via the always-on tier-3 LIKE scan
  above — trigram-indexed through `images_substring_fts` whenever every
  token is ≥3 chars, plain scan otherwise.
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
   ends): `uploadImage` POSTs, parses the `201` JSON, and returns the
   link verbatim — full stop. Deleted outright:
   the 303/Location parsing, the client-side `/<id>/orig/<filename>`
   derivation, the verification GET with its redirect-check, and
   `uploadHTTPClient`'s no-follow-redirect constraint (a plain client with
   a sane timeout is enough). The DESIGN NOTE in upload.go explaining the
   verification hop goes away with it. At switch-over this returned
   `resp.url`; once the image pages were live it moved to `resp.page`
   (details-page link, preferred — `resp.url` remains the documented
   direct link and the fallback when `page` comes back empty).
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
  search.js          debounced (300ms) fetch + history.replaceState; aborts superseded fetches
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
