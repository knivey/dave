<div align="center">

# 🤖 dave

### The IRC chatbot that brings LLMs, MCP tools & image generation straight to your channels

[![Go Version](https://img.shields.io/badge/Go-1.25%2B-00ADD8?style=flat-square&logo=go)](go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/knivey/dave?style=flat-square)](https://goreportcard.com/report/github.com/knivey/dave)
[![License](https://img.shields.io/badge/license-see%20repo-lightgrey?style=flat-square)](LICENSE)
[![Go Reference](https://pkg.go.dev/badge/github.com/knivey/dave?style=flat-square)](https://pkg.go.dev/github.com/knivey/dave)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen?style=flat-square)](../../pulls)
[![Issues](https://img.shields.io/github/issues/knivey/dave?style=flat-square)](../../issues)

[![IRC](https://img.shields.io/badge/protocol-IRC-4e4e4e?style=flat-square&logo=data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNCAyNCI+PHRleHQgZmlsbD0id2hpdGUiIGZvbnQtc2l6ZT0iMTYiIHg9IjIiIHk9IjE4Ij4jPC90ZXh0Pjwvc3ZnPg==)](https://en.wikipedia.org/wiki/Internet_Relay_Chat)
[![OpenAI Compatible](https://img.shields.io/badge/API-OpenAI%20Compatible-412991?style=flat-square&logo=openai)](https://platform.openai.com/docs/api-reference)
[![MCP](https://img.shields.io/badge/tools-MCP-FF6B35?style=flat-square)](https://modelcontextprotocol.io/)
[![ComfyUI](https://img.shields.io/badge/images-ComfyUI-blueviolet?style=flat-square)](https://github.com/comfyanonymous/ComfyUI)
[![TOML Config](https://img.shields.io/badge/config-TOML-9C4121?style=flat-square)](https://toml.io)

**Single binary · TOML-driven · Multi-network · Streaming · TUI · MCP · Image gen**

</div>

---

## ✨ Features

<table>
<tr>
<td width="50%">

### 🧠 LLM Chat & Completions
- **Chat API** with persistent per-user context history
- **Completion API** for one-shot prompts
- **Streaming** — watch responses arrive in real-time
- **System prompt templates** with `{{.Nick}}`, `{{.Channel}}`, `{{.Network}}`, `{{.ChanNicks}}`
- Markdown → IRC formatting (bold, color, underline, tables, code blocks)
- Multiple services: OpenAI, Grok/xAI, local vLLM, OpenRouter, any OpenAI-compatible API

</td>
<td width="50%">

### 🖼️ Image Generation
- **ComfyUI integration** via bundled MCP server (`img-mcp`)
- LLM-powered **prompt enhancement** before generation
- Multiple workflow support (SDXL, Flux, custom)
- Job queue with status, ETA, cancel
- Automatic image upload & URL hosting

</td>
</tr>
<tr>
<td width="50%">

### 🔌 MCP (Model Context Protocol)
- Connect **any MCP server** via stdio or HTTP
- MCP tools auto-injected into LLM requests (opt-in per command)
- Resources & prompts via MCP
- Auto-reconnect with exponential backoff
- KeepAlive pings for HTTP sessions

</td>
<td width="50%">

### 🖥️ Built-in TUI
- **tview**-based terminal UI with scrollback
- Command history (↑/↓), PgUp/PgDn navigation
- Runtime commands: `/reload`, `/join`, `/part`, `/nick`, `/quit`
- ANSI color log output
- Set `DAVE_NO_TUI=1` to run headless

</td>
</tr>
<tr>
<td width="50%">

### 🌐 Multi-Network IRC
- Connect to **multiple IRC networks** simultaneously
- Multiple servers per network with round-robin failover
- Auto-reconnect (60s interval)
- Per-network throttle, trigger, quit message
- SASL / server password support
- Wildcard-based host ignores (`ignores.txt`)

</td>
</tr>
<tr>
<td width="50%">

### 👁️ Vision / Image Detection
- Users paste image URLs in chat → bot downloads & sends to vision models
- Auto-resize, convert (JPG/WebP), compress
- Configurable max images per message & per context
- Works with GPT-4o, Gemini, any vision-capable model

</td>
</tr>
</table>

---

## 🚀 Quick Start

### Prerequisites

- **Go 1.25+**
- An OpenAI-compatible API key (OpenAI, Grok, local vLLM, etc.)
- An IRC server to connect to

### Build & Run

```bash
# Clone
git clone https://github.com/knivey/dave.git
cd dave

# Build
go build -o dave .

# Copy and edit config
cp -r config prod
$EDITOR prod/config.toml prod/services.toml

# Run (uses prod/ directory)
./dave prod
```

That's it. The TUI appears, the bot connects, and you're live.

### Config Directory Structure

```
prod/
├── config.toml              # Networks, trigger, global settings
├── services.toml            # API keys & base URLs
├── chats.toml               # Chat commands (persistent context)
├── completions.toml         # Completion commands (one-shot)
├── tools.toml               # MCP tool commands
├── mcps.toml                # MCP server connections
├── promptenhancements.toml  # Image prompt enhancement configs
├── sd.toml                  # Stable Diffusion settings
└── comfy.toml               # ComfyUI settings

# These live in the working directory (where you run ./dave):
contexts.json                # Persistent chat history (auto-created, from persist.file_path)
ignores.txt                  # Wildcard host ignores (optional)
```

---

## ⚙️ Configuration

### `config.toml` — Networks & Globals

```toml
trigger = "-"
quitmsg = "unplugged"
uploadurl = "https://upload.beer"

busymsgs = ["hold on i'm already busy"]
ratemsgs = ["whoa!! slow down!!!"]

[persist]
file_path = "contexts.json"
max_age_days = 7
max_contexts = 100
save_delay = "30s"

[api_log]
enabled = true
dir = "api_logs"

[networks.libera]
enabled = true
nick = "dave"
throttle = 750
channels = ["#mychannel"]
[[networks.libera.servers]]
host = "irc.libera.chat"
ssl = true
port = 6697
```

### `services.toml` — API Endpoints

```toml
[openai]
key = "sk-..."
baseurl = "https://api.openai.com/v1/"
maxcompletiontokens = 600
temperature = 0.7

[local]
baseurl = "http://localhost:8000/v1"
maxtokens = 500

[grok]
key = "xai-..."
baseurl = "https://api.x.ai/v1/"
maxtokens = 6000
```

### `chats.toml` — Chat Commands

```toml
[chat]
description = "General IRC-aware chat"
service = "openai"
model = "gpt-4o"
renderMarkdown = true
system = """\
You are dave, a chatbot on IRC that responds using IRC formatting.
IRC Color Codes: 04=Red, 09=Light Green, 12=Light Blue ...
"""

[yo]
service = "local"
streaming = true
system = "you are an unprofessional and rude chatbot..."

[view]
description = "Chat with image detection"
service = "openai"
model = "gpt-4o"
detectimages = true
maximages = 2
system = "You are dave chatting with {{.Nick}} in {{.Channel}} on {{.Network}}."
```

### `mcps.toml` — MCP Servers

```toml
[img-mcp]
transport = "stdio"
command = "./mcps/img-mcp/img-mcp"
timeout = "2m"

[github]
transport = "http"
url = "http://localhost:3001/mcp"
timeout = "30s"
```

### `tools.toml` — MCP Tool Commands

```toml
[qwen]
description = "Qwen enhanced image generation"
mcp = "img-mcp"
tool = "enhance_and_generate"
arg = "prompt"
timeout = "2m"
args = { workflow = "qwen", output_format = "url" }

[queue]
description = "View generation queue status"
mcp = "img-mcp"
tool = "queue_status"
skipbusy = true
```

---

## 💬 Usage

### Starting a Chat

```
<knivey> -chat how are you today?
<dave>   I'm doing well! How can I help you?
```

### Continuing a Conversation

Just reply to the bot's nick:

```
<knivey> dave: tell me more about that
<dave>   Sure! Here's the details...
```

### Image Generation

```
<knivey> -qwen a sunset over mountains
<dave>   https://upload.beer/abc123.jpg
<dave>   All done ;)
```

### Stop Generation

```
<knivey> -stop
```

### Help

```
<knivey> -help
<knivey> -help chat
```

### Built-in TUI Commands

| Command | Description |
|---------|-------------|
| `/help` | Show TUI commands |
| `/reload` | Hot-reload config from disk |
| `/join <net> <chan>` | Join a channel |
| `/part <net> <chan> [msg]` | Leave a channel |
| `/nick <net> <nick>` | Change nickname |
| `/quit` | Shut down the bot |

---

## 🖼️ Image Generation MCP (`img-mcp`)

The bundled `img-mcp` server provides ComfyUI-powered image generation with prompt enhancement.

### Build

```bash
go build -o mcps/img-mcp/img-mcp ./mcps/img-mcp
```

### Configure

Copy and edit the example config:

```bash
cp mcps/img-mcp/example.toml mcps/img-mcp/config.toml
```

Key sections in the config:

```toml
[comfy]
baseurl = "http://localhost:8188"
default_workflow = "zimage"

[upload]
url = "https://upload.example.com"

[enhancement.default]
baseurl = "https://api.x.ai/v1/"
key = "YOUR_KEY"
model = "grok-4-1-fast-reasoning"
systemprompt = "You are an expert at writing prompts for AI image generation..."

[workflow.zimage]
workflow_path = "workflows/z_image_turbo.json"
output_node = "28"
prompt_node = "6"
```

### Run

```bash
# stdio mode (launched by dave automatically)
./mcps/img-mcp/img-mcp

# HTTP mode
./mcps/img-mcp/img-mcp --http
```

### Available Tools

| Tool | Description |
|------|-------------|
| `enhance_prompt` | Enhance a raw prompt using LLM |
| `generate_image` | Generate image (sync, waits for result) |
| `generate_image_async` | Generate image (async, returns job ID) |
| `enhance_and_generate` | Enhance + generate (sync) |
| `enhance_and_generate_async` | Enhance + generate (async) |
| `queue_status` | View job queue state |
| `job_status` | Check a specific job |
| `list_jobs` | List jobs with optional filter |
| `cancel_job` | Cancel a queued/running job |
| `list_workflows` | List available workflows |
| `list_enhancements` | List enhancement configs |
| `upload_image` | Upload base64 image data |

---

## 🏗️ Architecture

```
┌─────────────────────────────────────────────────────────┐
│                        dave (main)                       │
│                                                          │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌────────┐ │
│  │  config/  │  │   TUI    │  │  IRC     │  │Context │ │
│  │  (TOML)   │  │ (tview)  │  │ (girc)   │  │ Store  │ │
│  └─────┬────┘  └────┬─────┘  └────┬─────┘  └───┬────┘ │
│        │            │             │              │       │
│  ┌─────┴────────────┴─────────────┴────────────┴─────┐ │
│  │              Command Dispatch (regex)               │ │
│  │  builtInCmds (stop, help) + configCmds (hot-swap)  │ │
│  └──┬──────────────┬──────────────────┬───────────────┘ │
│     │              │                  │                  │
│  ┌──┴───┐    ┌─────┴──────┐    ┌──────┴──────┐         │
│  │Chat  │    │Completion  │    │  MCP Tools  │         │
│  │(API) │    │  (API)     │    │  (any MCP)  │         │
│  └──┬───┘    └────────────┘    └──────┬──────┘         │
│     │                                  │                 │
│  ┌──┴──────────────┐          ┌────────┴────────┐       │
│  │ MarkdownToIRC   │          │    img-mcp      │       │
│  │ (goldmark+IRC)  │          │  (ComfyUI MCP)  │       │
│  └─────────────────┘          └─────────────────┘       │
└─────────────────────────────────────────────────────────┘
```

### Key Design Decisions

- **Single binary** — no runtime dependencies, no npm/node, just Go
- **Config directory** — each file has a purpose, hot-reloadable without restart
- **Atomic command swap** — `configCmds` replaced as a whole map, never mutated in-place
- **Per-user context** — chat history keyed by `network+channel+nick`
- **Dirty-flag persistence** — contexts saved atomically via `.tmp` + rename
- **MCP auto-reconnect** — exponential backoff (2^n seconds, capped at 60s) with jitter

---

## 🧪 Testing

```bash
# Run all tests
go test ./...

# Specific packages
go test ./MarkdownToIRC/...
go test -v -run TestCodeBlocks

# Single test
go test -run TestContextStoreRoundtrip

# Format & vet
go fmt ./...
go vet ./...
```

---

## 🔧 Advanced

### System Prompt Templates

Chat command system prompts support Go template syntax:

```toml
system = """You are {{.BotNick}} chatting with {{.Nick}} in {{.Channel}} on {{.Network}}.
Channel users: {{.ChanNicks}}"""
```

Variables: `{{.Nick}}`, `{{.BotNick}}`, `{{.Channel}}`, `{{.Network}}`, `{{.ChanNicks}}` (JSON array)

### Vision / Image Detection

Enable on any chat command to let users paste image URLs:

```toml
[view]
service = "openai"
model = "gpt-4o"
detectimages = true
maximages = 2
maxcontextimages = 2
imageformat = "jpg"
imagequality = 75
maximagesize = "1024x1024"
```

### MCP Tools in Chat

Give a chat command access to MCP tools:

```toml
[chat]
service = "openai"
model = "gpt-4o"
mcps = ["filesystem", "github"]
paralleltoolcalls = true
```

The LLM will automatically use available MCP tools when relevant.

### Provider-Specific Parameters

```toml
# OpenRouter reasoning
extra_body = {reasoning = {effort = "high"}}

# vLLM / Qwen3 thinking mode
chat_template_kwargs = {enable_thinking = false}

# Custom sampling
chat_template_kwargs = {top_k = 20}
```

### Hot Reload

Type `/reload` in the TUI to reload MCPs, services, prompt enhancements, and command definitions without restarting. MCP clients are closed and reconnected automatically.

### Host Ignores

Create `ignores.txt` with wildcard patterns (one per line):

```
#knivey*
*spammer*!*@*.example.com
```

---

## 📦 Dependencies

| Library | Purpose |
|---------|---------|
| [lrstanley/girc](https://github.com/lrstanley/girc) | IRC client |
| [sashabaranov/go-openai](https://github.com/sashabaranov/go-openai) | OpenAI API |
| [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) | MCP protocol |
| [rivo/tview](https://github.com/rivo/tview) | Terminal UI |
| [yuin/goldmark](https://github.com/yuin/goldmark) | Markdown parsing |
| [alecthomas/chroma](https://github.com/alecthomas/chroma) | Syntax highlighting |
| [BurntSushi/toml](https://github.com/BurntSushi/toml) | TOML config |
| [chai2010/webp](https://github.com/chai2010/webp) | WebP encoding |
| [vodkaslime/wildcard](https://github.com/vodkaslime/wildcard) | Wildcard matching |

---

## 🤝 Contributing

1. Fork the repo
2. Create a feature branch (`git checkout -b feature/my-feature`)
3. Make your changes
4. Run `go fmt ./...` and `go vet ./...`
5. Run `go test ./...`
6. Open a pull request

---

## 📝 License

This project is open source. See the repository for license details.

---

<div align="center">

**[Report a Bug](../../issues) · [Request a Feature](../../issues) · [Open a PR](../../pulls)**

</div>
