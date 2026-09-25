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

## Run

```bash
./imgsite              # uses config.toml next to the binary
./imgsite prod.toml    # or a named config (relative to the binary dir)
```

Reload hot-reloadable settings (`site.*`, `thumbnails.*` minus workers,
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
- Re-extract metadata: if workflow parsing improves (it has — e.g. GGUF
  loader variants were initially missed), heal existing rows without
  re-uploading: `curl -X POST -H "X-API-Key: <key>" https://…/admin/reextract`
  → `{"considered":N,"updated":M}`. Re-runs extraction from each row's
  stored `workflow_json` with the current rules; provenance and visibility
  are preserved, unparseable rows are skipped.
