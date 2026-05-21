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
| `start_index` | Current offset into the full content |
| `next_index` | Offset for the next page (0 if no more content) |
| `truncated` | Whether the content was cut off at `max_length` |
| `cache_status` | HTTP cache status (`HIT`, `MISS`, `REVALIDATED`, `STALE`, `BYPASS`) |

## Architecture

Two-layer caching:

1. **HTTP cache** (`bartventer/httpcache`, RFC 9111 compliant) — respects `Cache-Control`, `ETag`, `Last-Modified`, handles `304 Not Modified` revalidation automatically
2. **Markdown cache** — in-memory TTL-based cache of converted content, avoids re-parsing on pagination calls

Pipeline: URL → cached HTTP fetch → readability extraction → html-to-markdown conversion → paginate and return.

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
- `SIGHUP` — reloads env vars (note: HTTP client settings like timeout and proxy are set at startup and require restart to change)
