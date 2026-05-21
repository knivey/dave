# brave-mcp

Native Go Brave Search MCP server. Replaces the TypeScript `brave-search-mcp-server` with aggressively trimmed tool schemas (~5KB vs 46KB).

## Tools

| Tool | Description |
|---|---|
| `brave_web_search` | Web search with FAQ, news, and video results |
| `brave_image_search` | Image search |
| `brave_video_search` | Video search with duration and metadata |
| `brave_news_search` | News search with source and age |
| `brave_local_search` | Local business/place search (falls back to web results) |
| `brave_answers` | AI-generated answers with citations |
| `brave_llm_context` | Pre-extracted web content for RAG/AI reasoning |
| `brave_place_search` | Geographic place/POI search by coordinates or name |

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `BRAVE_API_KEY` | yes | — | Brave Search API subscription token |
| `BRAVE_BASE_URL` | no | `https://api.search.brave.com` | API base URL |
| `BRAVE_TIMEOUT` | no | `30s` | HTTP request timeout (Go duration) |
| `BRAVE_DEFAULT_COUNTRY` | no | `US` | Country code for all requests |
| `BRAVE_DEFAULT_LANG` | no | `en` | Search language for all requests |
| `BRAVE_ENABLED_TOOLS` | no | (all) | Comma-separated tool whitelist. Empty = all. |

### Tool Names for `BRAVE_ENABLED_TOOLS`

Short names (auto-prefixed with `brave_`): `web_search`, `image_search`, `video_search`, `news_search`, `local_search`, `answers`, `llm_context`, `place_search`

Full names also accepted: `brave_web_search`, `brave_answers`, etc.

```
BRAVE_ENABLED_TOOLS=web_search,news_search,llm_context
```

## Build

```bash
go build -o mcps/brave-mcp/brave-mcp ./mcps/brave-mcp
```

## Run

```bash
BRAVE_API_KEY=your-key ./mcps/brave-mcp/brave-mcp
```

Communicates via stdio (MCP protocol). No HTTP mode, no config files, no hot reload.

## Dave Integration

In `mcps.toml`:

```toml
[brave]
transport = "stdio"
command = "mcps/brave-mcp/brave-mcp"
env = ["BRAVE_API_KEY=your-api-key"]
timeout = "30s"
```

With optional settings:

```toml
[brave]
transport = "stdio"
command = "mcps/brave-mcp/brave-mcp"
env = [
  "BRAVE_API_KEY=your-api-key",
  "BRAVE_DEFAULT_COUNTRY=US",
  "BRAVE_DEFAULT_LANG=en",
  "BRAVE_ENABLED_TOOLS=web_search,news_search,answers,llm_context",
]
timeout = "30s"
```

Then reference in a service or chat via `mcps = ["brave"]`.

## Logs

Daily log files written to `mcps/brave-mcp/logs/` (same pattern as yt-mcp).
