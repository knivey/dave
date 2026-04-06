# AGENTS.md - dave IRC Bot

## Project Overview
Go IRC chatbot interfacing with OpenAI-compatible APIs, Stable Diffusion, and ComfyUI for image generation. Single binary, config-driven via TOML.

**Module**: `github.com/knivey/dave`
**Go version**: 1.25.0

## Commands

```bash
# Build
go build -o dave .

# Run (default config)
./dave
# Run with custom config
./dave prod.toml

# Run all tests
go test ./...

# Run tests for specific package
go test ./MarkdownToIRC/

# Run a single test
go test ./MarkdownToIRC/ -run TestInlineFormatting

# Run a single subtest
go test ./MarkdownToIRC/ -run "TestCodeBlocks/CodeBlockWithLang"

# Run with verbose output
go test -v ./...

# Install dependencies
go mod tidy
```

**No lint/typecheck/formatter config exists** - no golangci-lint, no Makefile. Follow `go fmt` and `go vet` defaults.

## Architecture

- **Root package**: `package main` - all `.go` files in root
- **Subpackage**: `MarkdownToIRC/` - converts markdown to IRC formatting codes
- **Entry point**: `main.go` - loads TOML config, registers commands, starts IRC clients
- **Config**: TOML files define networks, services, commands, and prompt enhancements
  - Copy `config.toml` to `prod.toml` or `test.toml` and edit
  - `ignores.txt` for host-based user ignoring (see `ignores.txt.example`)

### Key files
| File | Purpose |
|------|---------|
| `main.go` | Entry, IRC client lifecycle, message routing |
| `config.go` | TOML config structs and validation |
| `contexts.go` | Chat context/history management |
| `aiCmds.go` | OpenAI completion/chat commands |
| `sdCmds.go` | Stable Diffusion image generation |
| `comfyCmds.go` | ComfyUI workflow execution |
| `running.go` | Concurrency control (busy tracking) |
| `checkRate.go` | Rate limiting per channel |
| `image_detect.go` | Image URL detection in messages |
| `uploadr.go` | Image upload functionality |

## Code Style

- **Imports**: Standard library first, blank line, then third-party (grouped)
- **Naming**: `camelCase` for local vars, `PascalCase` for exported. Struct fields use TOML tags for snake_case config keys
- **Error handling**: `log.Fatalln` for fatal config errors at startup; return errors otherwise
- **Globals**: `config`, `wg`, `logger`, `bots`, `commands` are package-level
- **Concurrency**: Goroutines for command handlers (`go cmd(...)`), `sync.WaitGroup` for lifecycle
- **Regex**: Commands use compiled `*regexp.Regexp` keys in `CmdMap`
- **Logging**: `logxi` with per-network named loggers

## Testing

- Tests only exist in `MarkdownToIRC/` subpackage
- Uses table-driven tests with `t.Run()` and a shared `runTests()` helper
- Test assertions check `contain` / `notContain` substrings (no testify assertions in tests despite dependency)
- IRC control codes (`\x02` bold, `\x03` color, `\x1D` italic) are common in test expectations

## Config Quirks

- Command `regex` field is used to build larger pattern (don't use `^$` anchors)
- If command `regex` is empty, defaults to the key name
- Service `maxhistory` defaults to 8 if unset
- `renderMarkdown` and `streaming` are mutually exclusive (fatal error if both true)
- ComfyUI commands require: `workflow_path`, `clientid`, `output_node`, `prompt_node`
- Networks inherit `trigger` and `quitmsg` from root config if not specified
- Multiple servers per network: cycles through on reconnect
