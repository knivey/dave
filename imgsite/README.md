# imgsite

Single-binary image gallery for dave's generations. Receives uploads from
img-mcp, extracts the workflow embedded in each image's EXIF, and serves a
live gallery with prompt search. Full design + wire-format documentation:
[`docs/image-site.md`](../docs/image-site.md).

## Build

From the repo root:

```bash
./build.sh imgsite          # or: go build -o imgsite/imgsite ./imgsite
```

## Configure

```bash
cd imgsite
cp example.toml config.toml
```

Edit `config.toml` — the reference section at the top documents every option.
The ones you must set for production:

| Option | What |
|---|---|
| `auth.api_key` | Upload/delete secret. **Must not** start with `CHANGE_ME` (the placeholder is rejected at startup). Must match `upload.api_key` in img-mcp's config. |
| `server.base_url` | Public URL (e.g. `https://img.example.com`). Used to build the absolute direct links img-mcp pastes to IRC, plus `og:url`/`og:image`. If empty, URLs are derived from request headers — set it for production. |
| `server.addr` | Listen address (default `:8081`). |

Everything else has sane defaults. Data lives next to the binary by default:
`data/imgsite.db` (SQLite, WAL), `data/images/` (originals, sha256 fan-out),
`data/thumbs/` (JPEG derivatives), `logs/` (logxi, daily files).

## Safe site (optional)

The optional `[safe_site]` section serves a second logical site from the
same process, selected by the request Host header (port stripped,
case-insensitive): requests whose Host is in `hosts` see only images
whose provenance network is in `allowed_networks` (case-insensitive) OR
whose `safety` verdict is `safe`. Everything else — including `unknown`
verdicts and rows without provenance — is default-deny there. The
default site (any other Host) is unchanged and shows everything.

```toml
[safe_site]
hosts = ["safe.example.com"]
base_url = "https://safe.example.com"
allowed_networks = ["libera"]
```

All three fields are required when the section is present; omit the
section entirely for single-site behavior. Hot-reloadable via SIGHUP.
`base_url` builds the upload-response links for allowed-network uploads
(so dave pastes safe links into Libera channels) plus `og:image` /
`og:url` on the safe host. Site-aware surfaces and the full visibility
rule: `docs/image-site.md`, "Safe-site host split". The classification
pipeline that produces verdicts lives in img-mcp (`[safety]` +
`[enhancement.safety-vet]` — see img-mcp's example.toml); manual
marking is `./imgsite -safety` below.

## Run

```bash
./imgsite              # uses config.toml next to the binary
./imgsite prod.toml    # or a named config (relative to the binary dir)
```

Reload hot-reloadable settings (`site.*`, `safe_site.*`, `thumbnails.*` minus workers,
`search.*`, `upload.*`) with `SIGHUP` or:

```bash
curl -X POST -H "X-API-Key: <key>" http://127.0.0.1:8081/admin/reload
```

## Import legacy ComfyUI outputs

One-shot ingestion of an existing ComfyUI `output/` archive (recursive,
webp/PNG only) into the same database and store the upload path uses.
**Stop the server first** — WAL tolerates a second writer, but running
the import against a live server risks lock contention.

```bash
./imgsite -import /path/to/ComfyUI/output -tz America/New_York
```

- `-tz` gives the timezone the FILENAME timestamps (`%Y-%m-%d-%H%M%S`
  prefix, e.g. `2026-09-24-185355__0.webp`) were written in; default is
  this machine's local zone. A `__N` batch suffix orders same-second
  files by adding N ms. Non-matching names fall back to file mtime with
  a per-file warning.
- Non-destructive: originals are copied into the store, never moved or
  deleted. Idempotent: re-running skips every hash the DB already knows
  (including files previously uploaded via dave).
- Oldest files (no dave original-prompt note in the workflow) import
  with an empty original prompt and the workflow's final positive prompt
  as the enhanced prompt — searchable like any enhanced-only match
  (tier 2). Files with a note import with full fidelity.
- Thumbnails and missing dims are generated inline at the end of the
  run; an interrupted import resumes via the normal startup re-scan on
  the next server start.
- Per-file skips (no embedded workflow, non-image files) are report
  lines, never failures: the process exits non-zero only on fatal setup
  errors (bad dir, bad `-tz`, config/DB problems).

Details and design rationale: `docs/image-site.md`, "Importing legacy
outputs".

## Delete images (admin)

Hide an inappropriate image — soft delete, reversible, no confirmation
prompt. Two surfaces, one DB path (`dbHideImage`: `hidden=1`, files
retained):

```bash
# over HTTP (server running; connected browsers drop the card live)
curl -X DELETE -H "X-API-Key: <key>" https://…/api/images/<id>

# or offline from the shell (one id or a comma-separated list)
./imgsite -delete <id>[,<id>…] [config]
```

Galleries and search exclude hidden rows immediately. The HTTP endpoint
publishes a live `image-hidden` event; the CLI is offline like
`-import` — **stop the server first** (or accept that connected pages
keep the card until their next load).

- Per-id report lines never stop the rest of the list: `hidden <id> —
  "<prompt snippet>" (files retained)`, `already hidden: <id>` (the
  HTTP 410 equivalent), `not found: <id>`.
- Exit status: `0` only when every requested id was hidden by this run;
  any not-found / already-hidden / per-id error ⇒ `1`. Usage mistakes
  (empty list, malformed id, `-delete` combined with `-import`) ⇒ `2`.
- Reversible: restore with `UPDATE images SET hidden=0 WHERE id='<id>'`
  (the CLI summary prints this reminder after every run). Bytes are
  never purged — content-addressed storage is shared by dedupe.

## Set safety verdicts (admin)

Manual override for an image's safety classification — the tool that
clears the 'unknown' pile the automatic pipeline leaves behind
(pre-safety history, vet failures and timeouts) and corrects mis-vetted
verdicts. One DB path (`dbSetSafety`: writes `images.safety`, nothing
else — no files, no other metadata):

```bash
./imgsite -safety <id>[,<id>…] safe|unsafe|unknown [config]
```

- The verdict is a positional argument after the id list; the optional
  config path follows it. `unknown` is a full reset: the row returns to
  no-verdict, so on the safe host a NULL/empty-network row goes back to
  default-deny exactly as if never classified.
- Applies over any prior value — that is the point. A re-extract or
  metadata merge never overwrites a stored verdict; only another
  explicit `-safety` run changes it.
- Per-id report lines never stop the rest of the list:
  `set safety=<v> <id> — "<prompt snippet>" (was <old>)`,
  `not found: <id>`.
- Exit status: `0` only when every requested id was set by this run —
  a row already at the requested value still counts (the contract is
  "these rows now have this verdict"; there is no HTTP 410 analogue to
  mirror, unlike `-delete` repeats); any not-found / per-id error ⇒
  `1`. Usage mistakes (missing, empty, or invalid value, empty or
  malformed id list, combining with `-import` or `-delete`) ⇒ `2`.
- Offline like `-import`/`-delete` — **stop the server first** (or
  accept that safe-site visibility lags until the next page load: no
  live-update event is published). Undo: run it again with another
  value.

## Deploy behind a reverse proxy

- Terminate TLS at the proxy and pass the scheme through — absolute URLs
  honor `X-Forwarded-Proto` (first value) and `Host` when `base_url` is
  empty.
- For `/events` (SSE), response buffering must be off. The server sends
  `X-Accel-Buffering: no`, so **nginx needs no extra config**; other proxies
  may need explicit buffering disabled. Raise proxy read timeouts;
  connections are long-lived with 20s heartbeats.
- Since direct connections can spoof `X-Forwarded-For` (see below), bind to
  loopback when fully behind a proxy: `server.addr = "127.0.0.1:8081"`.
- The per-IP SSE connection cap (8) trusts the first `X-Forwarded-For`
  value. That is only correct when every client reaches the server through
  the proxy — direct connections can spoof it (documented trade-off; the cap
  is a guard, not auth).

Example systemd unit (adjust paths):

```ini
[Unit]
Description=dave imgsite
After=network-online.target

[Service]
User=dave
WorkingDirectory=/opt/imgsite
ExecStart=/opt/imgsite/imgsite
Restart=on-failure
# graceful stop: imgsite drains the SSE hub, HTTP server, and thumb workers
KillSignal=SIGINT

[Install]
WantedBy=multi-user.target
```

## Point img-mcp at it

In img-mcp's config (`mcps/img-mcp/config.toml` or `prod.toml`):

```toml
[upload]
url = "https://img.example.com"
api_key = "<same secret as auth.api_key>"
```

Then SIGHUP img-mcp (or `/reload <img-mcp>` in dave's TUI). The next
generation uploads via `POST <url>/updo` and dave pastes the returned
direct link to IRC.

## Day-to-day

- Uploads: only img-mcp (API-key). Watch img-mcp's logs for `upload complete`
  lines with httptrace stage timings; imgsite logs each upload with its
  total duration.
- Backups: `data/` is the only state (SQLite + originals + thumbs).
  `data/imgsite.db`, `data/images/`, and `data/thumbs/` are enough to
  restore the whole site next to a fresh binary.
- Delete an image: `curl -X DELETE -H "X-API-Key: <key>" https://…/api/images/<id>`
  — soft delete (`hidden=1`, files retained, galleries/search exclude it,
  connected browsers drop the card live). A second delete returns 410.
  Same soft delete offline via `./imgsite -delete <id>[,<id>…]` (see
  "Delete images (admin)").
- Set a safety verdict manually: `./imgsite -safety <id>[,<id>…]
  safe|unsafe|unknown` — writes `images.safety` over any prior value
  (offline; safe-site visibility follows on the next page load; the
  verdict survives re-extract). See "Set safety verdicts (admin)".
- Re-extract metadata: if workflow parsing improves (it has — e.g. GGUF
  loader variants were initially missed), heal existing rows without
  re-uploading: `curl -X POST -H "X-API-Key: <key>" https://…/admin/reextract`
  → `{"considered":N,"updated":M}`. Re-runs extraction from each row's
  stored `workflow_json` with the current rules; provenance and visibility
  are preserved, unparseable rows are skipped.
