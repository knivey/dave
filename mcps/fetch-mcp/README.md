# fetch-mcp

Native Go web fetching MCP server. Fetches URLs and converts HTML to clean markdown for LLM consumption. Replaces the Python `mcp-server-fetch` with a single static binary, RFC 9111 HTTP caching, readability content extraction, and zero runtime dependencies.

## Tools

| Tool | Description |
|---|---|
| `fetch` | Fetch a URL and convert its content to markdown. Supports pagination for large pages via `start_index`/`max_length`. Non-HTML content (JSON, plain text) is returned as-is. |

### Tool Parameters

| Parameter | Type | Default | Description |
|---|---|---|---|
| `url` | string | required | The URL to fetch |
| `max_length` | int | 20000 | Maximum characters to return (hard cap: 100000) |
| `start_index` | int | 0 | Character offset for paginating through large pages |

### Output Fields

| Field | Description |
|---|---|
| `content` | The fetched and converted content |
| `content_type` | `"markdown"` for HTML pages, `"raw"` for non-HTML |
| `title` | Page title (from readability or `<title>` tag) |
| `images` | Direct image URLs found on the page (see below) |
| `start_index` | Current offset into the full content |
| `next_index` | Offset for the next page (0 if no more content) |
| `truncated` | Whether the content was cut off at `max_length` |
| `cache_status` | HTTP cache status (`HIT`, `MISS`, `REVALIDATED`, `STALE`, `BYPASS`) |

#### The `images` field

Readability's text-density scoring drops image-only containers entirely, so on image-centric pages (galleries, photo detail views) the direct image URL can vanish from the converted markdown — the model then cannot answer "what is the image URL behind this page" (this broke session 2993 on an img.zkpq.ca photo page). The `images` field closes that gap: direct image URLs harvested from the **raw** HTML, independent of the markdown pipeline.

Harvest sources (document order, deduped, capped at 20):

- `<img>` `data-src` / `data-srcset` / `src` / `srcset` (data-* first: lazy-load convention puts the real image in `data-src` and a placeholder in `src`)
- `<picture>` `<source srcset>` (HTML guarantees an `<img>` fallback inside `<picture>`, so this only adds alternative sizes/encodings)
- `<a href>` whose path has an image extension (`.jpg`, `.png`, `.webp`, …). Extension-less wrapping hrefs are deliberately NOT harvested — gallery filmstrips use `<a href="/page"><img src="/thumb.webp"></a>` where the href is a page, and harvesting those floods the cap with page URLs. The wrapped `<img src>` always lands instead.
- `og:image`, `og:image:url`, `og:image:secure_url`, `twitter:image`, `twitter:image:src` meta
- `<link rel="preload" as="image">` (`imagesrcset`/`imagesrc`, `as` matched case-insensitively) and `<link rel="image_src">`

All URLs are resolved to absolute against the page URL — or the first `<base href>` when the page carries one; `data:` and non-http(s) schemes are skipped; the field is omitted when nothing is found. The harvested list rides the markdown cache alongside the content.

Data-URI lazy-load placeholders (`<img src="data:image/webp;base64,...">`) are also elided from the converted markdown itself (`![alt](data URI elided)`), so the no-readability path can no longer inline tens of KB of base64 per placeholder image.

## Architecture

Two-layer caching:

1. **HTTP cache** (`bartventer/httpcache`, RFC 9111 compliant) — respects `Cache-Control`, `ETag`, `Last-Modified`, handles `304 Not Modified` revalidation automatically
2. **Markdown cache** — in-memory TTL-based cache of converted content, avoids re-parsing on pagination calls

Pipeline: URL → cached HTTP fetch → readability extraction → html-to-markdown conversion (data-URI placeholders elided) + image URL harvest from the raw HTML → paginate and return.

If readability fails or is disabled, falls back to converting the full HTML. Non-HTML responses pass through as-is.

## Environment Variables

All configuration is via environment variables. No config file is loaded.

| Variable | Default | Description |
|---|---|---|
| `FETCH_SERVER_NAME` | `fetch-mcp` | Server name shown to MCP clients |
| `FETCH_SERVER_VERSION` | `0.1.0` | Server version |
| `FETCH_SERVER_ADDR` | `:8080` | HTTP listen address (only with `--http`) |
| `FETCH_USER_AGENT` | `fetch-mcp/0.1.0` | User-Agent header for HTTP requests |
| `FETCH_TIMEOUT` | `30s` | HTTP request timeout (Go duration) |
| `FETCH_MAX_REDIRECTS` | `10` | Maximum HTTP redirects to follow |
| `FETCH_MAX_BODY_SIZE` | `20971520` | Maximum response body size in bytes (default 20MB) |
| `FETCH_PROXY_URL` | (empty) | Explicit proxy URL (e.g. `socks5://127.0.0.1:1080`). If empty, uses `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` env vars. |
| `FETCH_CACHE_DSN` | `memcache://` | HTTP cache backend DSN. Use `fscache://?appname=fetch-mcp` for persistent disk cache. |
| `FETCH_CACHE_MARKDOWN_TTL` | `15m` | How long converted markdown stays in memory (Go duration) |
| `FETCH_READABILITY_DISABLED` | `false` | Set to `true`/`1`/`yes` to skip readability extraction and convert full HTML |

## Build

```bash
go build -o mcps/fetch-mcp/fetch-mcp ./mcps/fetch-mcp
```

## Run

```bash
# stdio mode (default)
FETCH_USER_AGENT="MyBot/1.0" ./mcps/fetch-mcp/fetch-mcp

# HTTP mode
FETCH_USER_AGENT="MyBot/1.0" ./mcps/fetch-mcp/fetch-mcp --http
```

## Dave Integration

In `mcps.toml`:

```toml
[fetch-mcp]
transport = "stdio"
command = "mcps/fetch-mcp/fetch-mcp"
env = [
  "FETCH_USER_AGENT=Mozilla/5.0 (X11; Linux x86_64; rv:144.0) Gecko/20100101 Firefox/144.0",
  "FETCH_TIMEOUT=60s",
  "FETCH_PROXY_URL=socks5://127.0.0.1:1080",
]
timeout = "60s"
```

Then reference in a service or chat via `mcps = ["fetch-mcp"]`.

## Logs

Daily log files written to `mcps/fetch-mcp/logs/` (same pattern as yt-mcp).

## Signal Handling

- `SIGINT`/`SIGTERM` — graceful shutdown
