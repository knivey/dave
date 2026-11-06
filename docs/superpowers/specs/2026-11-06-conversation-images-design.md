# Conversation Images — `view_image`, Ref Store, Injection Mechanism

Date: 2026-11-06
Status: approved design (brainstorm 2026-11-06), pending implementation plan

## Context

Dave's only image-input path today is user pastes: `chat()` with `detectimages = true`
detects URLs in the IRC message, downloads them (`image_detect.go`, SSRF-guarded,
bomb-safe), and attaches them as `MultiContent` image parts on the user message.
There is no way for the **model** to pull an image into the conversation:

- `fetch-mcp` returns mangled binary when pointed at an image URL (`fetcher.go`
  raw passthrough: `string(bodyBytes)` paginated), and image URLs found on HTML
  pages survive only as markdown `![](url)` text — the model can read the URL but
  cannot see the image.
- MCP tool results cannot carry images into the conversation:
  `mcpToolResultToText` discards `ImageContent` data, and tool-role messages are
  string-only on both wire paths.
- The OpenAI-compatible SDK exposes no video input types at all (checked
  openai-go v3.33.0 and v3.85.0) — video input is deferred (see Future Work).

This feature also grounds an older, never-implemented plan (recovered from the
gitignored `todo.md`, May 2026): **img-mcp image editing**. That plan's central
mechanism — every image the model sees is paired with an adjacent text block
carrying a **reference ID**, so a future edit tool can accept refs the model
passes in its args (the model cannot forward binary through JSON tool params) —
is built here. dave-side refs (`usr_` pastes, `web_` fetched) are resolvable by
dave; img-mcp's own `gen_` refs are future work.

**Owner directive (binding):** generated images are **never auto-injected** —
every generated image entering context wastes tokens. The tool result keeps its
URL text; only an explicit `view_image` call by the model pulls an image in.

## Goals

1. `view_image` builtin tool: the model can fetch any image URL into the
   conversation as vision input (fetched pages, imgsite links, anything).
2. Ref pairing: every image the model sees (paste or viewed) carries a ref ID
   in an adjacent text part, registered in a store that survives compaction.
3. Budget enforcement at injection: existing `maxcontextimages` guard applies.
4. `trusted_image_hosts` so dave can fetch from its own privately-resolving
   imgsite despite the SSRF private-IP block.
5. fetch-mcp stops returning mangled bytes for binary content.

## Non-goals

- img-mcp changes of any kind (no `ImageContent` results, no edit workflow).
- Auto-injection of generated images on sync results or async delivery.
- Native video input (no SDK types exist; seam documented in Future Work).
- Ref interception/resolution in tool calls (the edit-workflow consumer; the
  store's `resolveImageRef` is the deliberately unused seam it will use).
- New wire-format work: user messages with image parts already serialize on
  both Chat Completions and Responses paths.

## Mechanism overview

```
view_image(url) builtin ──┐
                          ├─→ downloadImage (SSRF guard, trusted-host bypass)
user paste (existing) ────┤     → convertImage (bomb-safe, resize, re-encode)
                          │     → mint ref (usr_/web_) + store (table + file)
                          ▼
                    labeled RoleUser message, MultiContent [text: ref label, image: data URI]
                          │
                          ▼ turn.Add() — persists to messages.multi_content (existing column)
                          │
            next API round-trip sees it (both wire paths)
                          │
                          ▼ future: edit tools resolve refs via the store
```

## Component design

### 1. `view_image` builtin tool

**Registration.** `builtinTools` entry following `register_background_job`'s
pattern: handler signature `(cr *chatRunner, turn *turnContext, tc ToolCall)`,
handler does its own `turn.Add(toolResultMsg(...))`; dispatch rides the existing
builtin branch in `executeToolCalls` (visible-call notice uses `server=builtin`).

**Gating.** Offered only when BOTH:
- the existing MCP-tools-exist gate passes (builtins ride it today), AND
- `cfg.DetectImages` is true — the admin's existing declaration that this
  command runs a vision-capable model. A non-vision command offering the tool
  would produce invisible-to-model images and confused output.

`disabled_builtin_tools` cascade opts out per command/service/root.
**Not** added to the default `hidden_tools` list: users should see the
"viewing image…" tool notice (transparency, explains latency); `toolverbose`
and `hidden_tools` still silence it as usual.

**Tool definition** (assembled in `getBuiltinToolDefs`, name/description):

- `view_image` — "Download an image from a URL and view it. Use for images
  found in fetched pages or image links (including generated image pages).
  Each call views one image; the image is attached in the message right after
  the tool result, labeled with a reference ID. Subject to the context image
  budget."
- Input: `{url: string}` (required). One image per call.

**Handler flow** (in order):

1. Reject ephemeral turns with an error tool result ("not available on this
   command"). Ephemeral (generator) turns never advertise `view_image`
   (`getTools`' ephemeral branch omits all builtins), and generators are
   declared non-vision (`generators.toml` documents `detectimages` as not
   useful there) — a hallucinated call errors as tool result, mirroring the
   generator tools' `!cr.ephemeral` guards in reverse.
2. Validate URL scheme (http/https) — error as tool result.
3. Budget: `remaining = cfg.MaxContextImages - countContextImages(turn.Messages())`.
   `turn.Messages()` includes the turn's history snapshot plus everything added
   this turn, so repeated views count. `remaining <= 0` → readable refusal as
   tool result ("context image budget reached (max N)"), model relays to user.
4. `downloadImage` (SSRF-guarded; trusted-host bypass, below).
5. `convertImage` with the command's `ImageFormat`/`ImageQuality`/
   `MaxImageWidth`/`MaxImageHeight` and root `MaxImagePixels`.
6. Mint `web_<12 hex>` ref; `registerImageRef` (best-effort, see store).
7. `turn.Add(toolResultMsg(tc.ID, "Image loaded and attached in the next message — ref: web_xxxx. Source: <url>"))`.
8. Queue the injection message on the runner (NOT `turn.Add` inline — see
   Ordering).

Every failure in 1–5 is **tool-result text** the model can act on (never a
user-facing error), matching the generator-tools philosophy.

### 2. Injection message shape

```go
ChatMessage{
    Role:         RoleUser,
    MultiContent: []MessagePart{
        {Type: PartTypeText, Text: "[image — ref: web_xxxx — source: <url>]"},
        {Type: PartTypeImageURL, ImageURL: &ImageURL{URL: dataURI, Detail: ImageDetailAuto}},
    },
}
```

The label text part precedes the image part — the old plan's "adjacent text
block" pairing, so the model visually maps ref → image.

**Ordering:** the handler queues the injection on the runner;
`executeToolCalls` flushes queued injections **after the tool loop completes**
— after ALL tool results of the round. Adding inline would be a wire hazard:
with multiple tool calls in one assistant message, a later tool result would
land after the injected user message, and providers require tool results to
follow the `tool_calls` message directly.

**Persistence:** `turn.Add` is the single seam (appends to turn + persists to
`messages.multi_content`; no-ops the DB write on ephemeral turns). No new
persistence code.

**Why RoleUser:** every provider in the fleet accepts user-message images on
both wire paths; tool-role content parts are string-only on the Responses path
(`function_call_output`) and would be silently dropped by llama.cpp chat
templates. Mid-chain user rows have precedent (guidance injection).

**Compaction:** `stripImagesForSummary` swaps the image part for `[image]` as
with pastes; the label text part survives into the summary, so refs stay
meaningful post-compaction. This is why the store — not message scanning — is
the resolution source.

**Logging:** `sanitizeMessages` already truncates data URIs; nothing new.

### 3. Responses chained-input fix (required)

The Responses tool loop with `previous_response_id` chains sends **delta-only**
input: `turn.LastN(numToolCalls)` → `toolResultMsgsToInputItems`
(`responses.go` ~956). An injected RoleUser message added after the tool
results falls outside `LastN(numToolCalls)` and would be **silently dropped**
from the chained request.

Fix: build the delta input from **everything after the assistant tool-call
message** (not just the trailing tool results), and extend the item builder to
pass RoleUser messages through as `EasyInputMessageParam` content lists —
exactly the shape `messagesToResponseInputItems` already emits for user
multimodal messages. The Chat Completions path rebuilds from full
`turn.Messages()` each round; no change there.

This is the highest-risk wiring change and gets a dedicated serializer test.

### 4. Ref store

```go
type ImageRef struct {
    ID        uint64 `gorm:"primaryKey"`
    Ref       string `gorm:"uniqueIndex;size:64"` // usr_/web_ + 12 hex
    SessionID int64  `gorm:"index"`
    Origin    string // "user" (paste) | "view" (view_image)
    SourceURL string
    MIME      string
    FilePath  string
    Width     int
    Height    int
    CreatedAt time.Time
}
```

- AutoMigrate (dave's existing pattern); mirrored in
  `tools/migrate-sqlite-to-pgsql` (as `reasoning_details` was).
- Files: decoded bytes at `<images_dir>/<ref>.<ext>`; root config
  `images_dir`, default `"data/images"`.
- `registerImageRef(sessionID, origin, sourceURL, mime, w, h, bytes)`:
  mint with `crypto/rand` 12 hex chars (unique index, one retry on collision),
  write file, insert row. **Best-effort:** on disk/DB failure, WARN and return
  the ref anyway — seeing the image is the point; an unresolvable ref degrades
  to a readable error in the future edit tool. Ref labels are always emitted
  so the model can rely on their shape.
- `resolveImageRef(ref) (*ImageRef, error)`: unused by production today — the
  deliberate seam for the edit-workflow interception, kept honest by tests.
- **Cleanup:** hooked into the existing session `MaxAgeDays` sweep — delete
  rows by session id, unlink files. Orphaned files (crash between write and
  insert) swept in the same pass: files with no row older than one day.

### 5. Paste ref pairing

`buildImageMessage` gains ref pairing: for each image part, prepend a text part
`[image N — ref: usr_xxxx — <source url>]` and register each image
(`origin=user`). Existing behavior otherwise unchanged (URL stripping from
visible text, `MaxImages` cap, warnings).

### 6. Config

`config/config.toml` (root):

- `trusted_image_hosts = []` — exact hostnames, case-insensitive. **No
  wildcards** (a wildcarded SSRF-guard bypass is a footgun; admins list hosts
  explicitly). Matching hosts skip ONLY the private-IP dial block in the image
  download path; scheme, redirect, size-cap, and content-type checks stay.
  Purpose: imgsite typically resolves privately and dave must fetch its own
  gallery pages for `view_image`.
- `images_dir = "data/images"`.

Both documented in config.toml's reference block + commented example per the
config docs convention. `chats.toml` reference gains a `view_image` builtin
entry (offered when `detectimages = true`; visible tool notice; removable via
`disabled_builtin_tools`).

### 7. fetch-mcp binary guard

In `Fetcher.Fetch`, before the raw passthrough branch: classify by
`Content-Type` (HTML detection unchanged, runs first):

- `image/*` → stub: `[binary image content (image/png, 1.2 MB) — use the view_image tool to view it]`
- other non-text types (`audio/*`, `video/*`, `application/octet-stream`,
  zip, etc.) → generic binary stub.
- text-ish types (`text/*`, json, xml, csv, yaml, js, css) keep today's
  passthrough.

Stubs never enter the markdown cache. No config flag — strictly better than
mangled bytes. (fetch-mcp remains otherwise text-only; no image fetching there,
per the old plan's rejection of "MCP downloads it itself".)

## Error handling summary

| Failure | Behavior |
|---|---|
| Bad scheme / non-image content-type / bomb guard / download error | Tool-result text; model relays |
| SSRF private-IP block | Tool-result text naming the host (admin connects it to `trusted_image_hosts`) |
| Budget exhausted | Tool-result refusal with the max; model tells user |
| Store registration failure | WARN; image still injected with ref label |
| fetch-mcp binary content | Stub text redirecting to `view_image` |

## Testing

Table-driven, testify, per repo norms:

- `view_image` handler family: success (tool result + injection message shape,
  order, ref label format), budget refusal, non-image URL, SSRF block vs
  trusted-host bypass, ephemeral hallucinated-call rejection, disabled-tool
  path, injection flushed after the full tool loop (multi-tool round),
  `DetectImages` gating in `getBuiltinToolDefs`.
- Paste pairing: labels present and adjacent; registration calls.
- Store: mint uniqueness, file+row roundtrip, resolve, cleanup unlinks files,
  orphan sweep.
- Responses delta-input fix: injected user message present in chained input
  (serializer-level pin).
- End-to-end mock runner: turn with a `view_image` call — round-2 request
  contains the image part (both API paths).
- Config: `trusted_image_hosts` parse + wildcard rejection, `images_dir`
  default; chats.toml docs presence.
- Migration tool: `image_refs` mirror.
- fetch-mcp: image stub names `view_image`; text passthrough unchanged; stubs
  not markdown-cached.

## Future work (explicit seams, not built)

- **img-mcp edit workflow** (the old todo.md plan): ComfyUI edit workflows
  accepting input images + prompt; dave intercepts edit tool calls and expands
  `usr_`/`web_` refs via `resolveImageRef`; img-mcp resolves its own `gen_`
  refs (job/image id) without re-upload. The ref store and labels here are the
  foundation.
- **Video input:** add a `PartTypeVideoURL` constant + serializer branches +
  provider-gated wire injection (raw JSON; no SDK types exist). The injection
  mechanism (labeled user message + budget + store) is modality-agnostic by
  design.

## Related history

- Original image-editing discussion + ref-ID mechanism: `todo.md` (gitignored,
  main checkout), May 2026 session; recovered 2026-11-06.
- Queue/session async delivery ("AI comments on the image") intentionally
  stays text/URL-based per the no-auto-injection directive.
