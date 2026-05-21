# brave-mcp: Go Brave Search MCP Server

## Problem

The existing `brave-search-mcp-server` is a TypeScript/Node.js MCP that sends 46KB of tool schemas per request. When combined with other MCP tools in a single API call, the total schema size causes xAI's Responses API to return 400 "Invalid arguments passed to the model." The schemas are bloated with 70+ item enum lists (country codes, language codes), multi-paragraph descriptions, `anyOf` patterns, `const`, `pattern`, and `exclusiveMin/Max` fields.

## Solution

Replace the TS server with a native Go MCP at `mcps/brave-mcp/`, following the same patterns as `mcps/yt-mcp/`. Aggressively trim schemas to ~5-8KB total. Configure entirely via environment variables — no config files.

## Directory Structure

```
mcps/brave-mcp/
  main.go      — entry point, env var parsing, stdio server, tool registration
  tools.go     — ToolHandlers struct, input/output types, 8 tool handler methods
  api.go       — Brave Search API client (shared GET helper, response types)
  logging.go   — logxi logging (daily files, same pattern as yt-mcp)
```

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `BRAVE_API_KEY` | yes | — | Brave Search API subscription token. Fatal if missing. |
| `BRAVE_BASE_URL` | no | `https://api.search.brave.com` | API base URL |
| `BRAVE_TIMEOUT` | no | `30s` | HTTP request timeout (Go duration) |
| `BRAVE_DEFAULT_COUNTRY` | no | `US` | Country code sent with every request (server-side default) |
| `BRAVE_DEFAULT_LANG` | no | `en` | Search language sent with every request (server-side default) |
| `BRAVE_ENABLED_TOOLS` | no | (all) | Comma-separated whitelist of tool names. Empty/unset = all enabled. |

### Tool Names for BRAVE_ENABLED_TOOLS

`web_search`, `image_search`, `video_search`, `news_search`, `local_search`, `answers`, `llm_context`, `place_search`

Example: `BRAVE_ENABLED_TOOLS=web_search,news_search` registers only those two tools.

## Architecture

### main.go

1. Parse env vars into a `Config` struct. Fatal on missing `BRAVE_API_KEY`.
2. Parse `BRAVE_ENABLED_TOOLS` into a `map[string]bool` whitelist. Empty = all enabled.
3. Create `ToolHandlers` with config.
4. Create MCP server with `mcp.NewServer()`.
5. Register only enabled tools via `mcp.AddTool()`.
6. Run stdio transport: `server.Run(ctx, &mcp.StdioTransport{})`.
7. SIGINT/SIGTERM for clean shutdown.

Config struct lives in main.go (trivial env var parsing, no separate file):

```go
type Config struct {
    APIKey        string
    BaseURL       string
    Timeout       time.Duration
    DefaultCountry string
    DefaultLang   string
    EnabledTools  map[string]bool // nil = all
}
```

Helper `isToolEnabled(name string) bool` — returns true if map is nil (all enabled) or name is in the map.

### api.go

Single shared function:

```go
func braveRequest(ctx context.Context, cfg *Config, endpoint string, params url.Values) (json.RawMessage, error)
```

- Builds URL: `cfg.BaseURL + endpoint`
- Sets header: `X-Subscription-Token: cfg.APIKey`, `Accept: application/json`
- Encodes params (including default country/lang)
- GET request with configured timeout
- Returns raw JSON body on 200, error otherwise

### API Endpoints

| Tool | Brave API Endpoint |
|---|---|
| `brave_web_search` | `GET /res/v1/web/search` |
| `brave_image_search` | `GET /res/v1/images/search` |
| `brave_video_search` | `GET /res/v1/videos/search` |
| `brave_news_search` | `GET /res/v1/news/search` |
| `brave_local_search` | `GET /res/v1/web/search` then `GET /res/v1/local/descriptions` |
| `brave_answers` | `POST /res/v1/chat/completions` |
| `brave_llm_context` | `GET /res/v1/llm/context` |
| `brave_place_search` | `GET /res/v1/local/place_search` |

### tools.go

`ToolHandlers` struct holds `Config` pointer (no mutex needed — config is read-only after startup, no reload).

Each tool:
- Input struct with `json` + `jsonschema` tags for MCP schema generation
- Handler method: `func (h *ToolHandlers) handleXxx(ctx, req, input) (*mcp.CallToolResult, Output, error)`
- Builds `url.Values`, calls `braveRequest()`, formats response

### Schema Trimming Strategy

The core goal: reduce 46KB → ~5-8KB. Rules applied to every tool:

1. **Descriptions**: One concise sentence per tool, one short phrase per parameter. No multi-paragraph explanations.
2. **Remove all enum lists**: No country code enums (70+ values), no language code enums (50+ values), no UI language enums. These are handled by `BRAVE_DEFAULT_COUNTRY` and `BRAVE_DEFAULT_LANG` server-side. Per-call override params accept a plain string.
3. **Remove**: `title`, `$schema`, `pattern`, `const`, `default`, `exclusiveMinimum`, `exclusiveMaximum`, `minLength`, `maxLength` from schemas.
4. **Replace `anyOf`** with simple `string` type where the parameter is truly optional.
5. **All tools**: `"additionalProperties": false`, explicit `"required"` array.
6. **Remove** `goggles`, `spellcheck`, `text_decorations`, `ui_lang`, `extra_snippets`, `summary`, `result_filter`, `user-agent` parameters — not useful for an IRC bot.

### Tool Schemas (trimmed)

#### brave_web_search
- `query` (string, required) — search query
- `count` (int) — number of results (1-20, default 10)
- `offset` (int) — pagination offset (0-9)
- `safesearch` (string) — "off", "moderate", or "strict"
- `freshness` (string) — "pd", "pw", "pm", "py", or "YYYY-MM-DDtoYYYY-MM-DD"

#### brave_image_search
- `query` (string, required)
- `count` (int) — 1-200, default 20
- `safesearch` (string) — "off" or "strict"

#### brave_video_search
- `query` (string, required)
- `count` (int) — 1-50, default 20
- `offset` (int) — 0-9
- `safesearch` (string) — "off", "moderate", "strict"
- `freshness` (string)

#### brave_news_search
- `query` (string, required)
- `count` (int) — 1-50, default 20
- `offset` (int) — 0-9
- `safesearch` (string)
- `freshness` (string)

#### brave_local_search
- `query` (string, required)
- `count` (int)
- `safesearch` (string)
- `freshness` (string)

Implementation: calls web search, extracts location IDs from results, fetches descriptions from local endpoint. Falls back to web results if no locations found (same as TS original).

#### brave_answers
- `query` (string, required) — question to get an AI-generated answer for
- `safesearch` (string) — "off", "moderate", or "strict"

#### brave_llm_context
- `query` (string, required)
- `count` (int) — 1-50
- `freshness` (string)

#### brave_place_search
- `query` (string)
- `latitude` (float64) — -90 to 90
- `longitude` (float64) — -180 to 180
- `location` (string) — e.g. "san francisco ca united states"
- `radius` (float64) — bias radius in meters
- `count` (int) — 1-50, default 20
- `safesearch` (string)

## Response Formatting

Each tool handler transforms the Brave API JSON response into a concise text result suitable for IRC consumption via the LLM. No raw JSON dumping — extract titles, URLs, descriptions, and relevant metadata into readable text.

## Build & Integration

```bash
go build -o mcps/brave-mcp/brave-mcp ./mcps/brave-mcp
```

Dave's `mcps.toml` entry:
```toml
[brave-mcp]
transport = "stdio"
command = "mcps/brave-mcp/brave-mcp"
env = { BRAVE_API_KEY = "..." }
# optional:
# env = { BRAVE_API_KEY = "...", BRAVE_ENABLED_TOOLS = "web_search,news_search", BRAVE_DEFAULT_COUNTRY = "US" }
```

## What's Not Included (intentionally)

- No HTTP mode (stdio only)
- No config files (env vars only)
- No hot reload (restart to change config)
- No database
- No rate limiting (Brave API handles this server-side)
- No proxy rotation (unlike yt-mcp which needs it for YouTube)

## Testing

- Table-driven tests for env var parsing, tool whitelist logic
- API response parsing tests (mock HTTP responses)
- Follow yt-mcp's test patterns: `testify` assertions, no external dependencies in unit tests
