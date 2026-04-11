# AGENTS.md - dave IRC Bot

## Project Overview
Go IRC chatbot for OpenAI-compatible APIs, Stable Diffusion, ComfyUI image gen. Single binary, TOML-driven.

**Module**: `github.com/knivey/dave`
**Go**: 1.25.0

## Commands
```bash
go build -o dave .

./dave              # config.toml
./dave prod.toml
./dave test.toml

go test ./...                    # all tests
go test ./MarkdownToIRC/...      # markdown tests only
go test -run TestContextStoreRoundtrip  # one test
go test -v -run TestCodeBlocks   # subtest

go fmt ./...
go vet ./...
go mod tidy
```
No Makefile, no linter config. Use `go fmt` + `go vet`.

## Architecture
- All root `.go` files = `package main` (globals: `config`, `commands` *CmdMap*, `bots`, `wg`, context maps, `logger`).
- `MarkdownToIRC/` (with `irc/`, `tables/`) converts markdown to IRC codes (`\x02`, `\x03`, `\x1D`).
- `main.go`: loads config, registers regex commands, starts girc clients per network.
- Config in TOML: networks, services, commands (completions/chats/sd/comfy), promptenhancements.
  - Copy `config.toml` to `prod.toml`/`test.toml`.
  - `ignores.txt` (see `.example`) for host ignores (wildcard).
  - `contexts.json` (gitignored) for persistent chat history.

## High-Signal Gotchas
- Config validation: `log.Fatalln` on any error (undefined services, missing Comfy fields, bad templates). `renderMarkdown+streaming` now supported (tables omitted, code blocks are plain-text with fixed 80-char padding).
- Command regex: empty = key name; registered as `^<regex> (.+)$` in main.go.
- Networks inherit root `trigger`/`quitmsg`; cycle multiple servers on reconnect.
- Service `maxhistory` defaults to 8.
- ComfyConfig requires `workflow_path`, `clientid`, `output_node`, `prompt_node`.
- Context store: dirty flag, atomic (`.tmp`+rename), timer, age+count cleanup. Tests mock via `persistCfg.FilePath`.
- Tests: table-driven + `t.Run()`, substring `contain`/`notContain` (no testify). MarkdownToIRC uses shared `runTests()` helper. Root has context/config/ai tests.

## Code Style
- Imports: stdlib, blank, third-party.
- Globals heavily used. Concurrency via `go cmd(...)` + `sync.WaitGroup`.
- Struct fields use TOML snake_case tags.
- Error: `log.Fatalln` at startup only.

Preserve this file. Update only verified facts.
