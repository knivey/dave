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
