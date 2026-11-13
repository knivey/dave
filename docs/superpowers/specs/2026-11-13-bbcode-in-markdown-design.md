# BBCode In The Markdown Parser — Design

Date: 2026-11-13
Status: approved approach + approved design sections, spec stage
Incident that motivated this: session 3178 — colored headings rendered with
literal `**` (`**The Technical & Sharp…**` etc. all visible, colored but unbolded)

## Problem

BBCode translation is a pre-markdown byte pass (`TranslateBBCode` /
`bbcodeTranslator`, bbcode.go) that runs BEFORE the markdown renderer on every
emission path. The translator has no knowledge of markdown structure, which
produces two verified bug classes (both reproduced through the production
pipeline `TranslateBBCode` → `MarkdownToIRC` on 2026-11-13):

1. **Emphasis breakage (session 3178).** IRC control codes are Unicode Cc
   control characters. In CommonMark's delimiter-flanking rules a Cc char is
   neither whitespace nor punctuation, so a closing `**` immediately followed by
   `\x03` fails the right-flanking test and cannot close. `[color=cyan]**X**[/color]`
   translates to `\x0310**X**\x03`; the closer `**` is followed by `\x03` → both
   `**` of that span render literally. Lines whose closer is followed by `\n`
   (whitespace) bold fine — which is why the bug hits *per-line* color-wrapped
   headings (every line ends `**[/color]` → `**\x03`) and looks intermittent.
2. **Structure blindness.** Tags inside fenced code blocks, inline code spans,
   HTML blocks, and `$…$`/`$$…$$` math are translated anyway, injecting codes
   into rendered code (`use \x030,90\x0304\x03 here\x03`). sesssite already
   carries a cleanup pass (`sesssite/renderer/irc.go`, code-span IRC-code
   stripping) that exists purely to paper over this on the HTML side.

## Goals

- BBCode tags become markdown-parser constructs: an inline parser extension
  produces AST nodes; the IRC renderer emits codes from them. Both bugs fall
  out structurally (code content is never inline-parsed; delimiters see `[`/`]`
  punctuation instead of Cc control bytes).
- Preserve today's user-visible color semantics exactly (color stack,
  invalid-color transparency, guards, newline re-emission, auto-close) on the
  non-streaming path, and byte-equality between streaming and one-shot.
- sesssite renders BBCode through the same parser with HTML spans, and its
  translator port + code-span cleanup pass are deleted.

## Owner rulings (2026-11-13)

- **Style toggles are dropped entirely.** `[b]`/`[i]`/`[u]`/`[s]`, the
  `bbcode_styles` knob, and the `BBCodeStylesFormatting` templatevar go away —
  models never used them. BBCode becomes color-only. Toggle-shaped tokens
  become unknown tokens (verbatim, no warn), exactly today's styles-off
  behavior.
- **Colors win over links.** `[color=red](url)` renders as color-open plus
  literal `(url)`. Every other tag-shaped link label (`[b](url)` — now unknown,
  `[1](url)`, `[label](url)`) falls to markdown link jurisdiction naturally.
- **Pastebin keeps one-shot translation.** The paste source stays
  translated-codes-with-markdown-syntax (byte-identical to today) via a
  slimmed color-only `TranslateBBCode` kept alive for paste + legacy
  completions. Paste-site color improvements are a separate later effort.
- **sesssite is updated in this change** (not deferred).

## Non-goals

- No new config knobs. `bbcode` (cascade command > service > false) keeps its
  exact meaning; the extension is simply gated on it per render.
- No change to model-facing instructions beyond deleting the styles templatevar
  (`BBCodeFormatting` keeps teaching the 16 canonical color names).
- No paste-site work (owner: later).
- No re-translation of anything stored — session storage keeps raw model output
  (unchanged invariant); only emission changes.

## Design

### 1. Approach: goldmark inline-parser extension (color-only)

A new `bbcodeExtension` in `MarkdownToIRC` (modeled on `mathExtension` in
math.go, the in-repo precedent for custom parsers):

- An **inline parser triggered on `[`, registered at priority above goldmark's
  link parser**. It recognizes exactly `[color=<value>]`, `[/color]`, and
  `[/color=<value>]` (close-with-value = plain close, today's rule), using the
  existing `parseBBTag` vocabulary and shape rules: 16 canonical names +
  aliases + numerics 0–15, case-insensitive, value charset letters/digits/
  spaces, **no newlines** (a tag can never span lines — load-bearing for
  streaming, §4).
- Anything else declines: `[b]` (now unknown), `[1]`, `a[i]`, `[]`, `[=x]` —
  the `[` stays text, and markdown link parsing proceeds unaffected. Unknown
  tokens pass through verbatim, no warn (today's unknown-token contract).
- Matched tags become two custom AST node kinds — `bbcodeColorOpen{value}` and
  `bbcodeColorClose` — with new cases in `renderNode` and `renderNodeTo`
  (table cells are inline-parsed, so colors work in cells and the render state
  spans cells within one table walk, as codes span cells today).
- The extension carries a warn callback (per-render construction; the goldmark
  instance is already built per call) so invalid-color warns keep today's text
  and logging path.

Why an inline parser rather than a parse-then-transform AST pass: the
colors-win ruling is native here — the color parser consumes `[color=red]`
before the link parser sees the `[`. Under transform-after-parse the link node
would already exist and we'd have to intercept Links with tag-shaped labels and
split them: more rules, more corners.

What the parser gets for free (verified by probe):

- fenced/indented code content is never inline-parsed; inline-code content is
  consumed raw by the backtick parser; HTML-block content is not inline-parsed;
  math spans are consumed by the math parser first → **tags stay literal in all
  four** (today all four get translated).
- delimiters adjacent to tags flank against `[`/`]` (punctuation) →
  `[color=cyan]**X**[/color]` renders **bold and colored**.

### 2. Core semantics (non-streaming)

All preserved from today, relocated from the byte translator into a small
render-state threaded through the AST walk:

- **Color stack with invalid-color transparent placeholders**: `[color=bogus]`/
  `[color=]`/`[color]`/`[color=16]` push a `""` placeholder, emit nothing, WARN
  once at the open; `[/color]` pairs with the innermost entry (tracked or
  placeholder); the effective color is the nearest real entry.
- **Emission invariants**: every color exactly two digits; `[/color]` re-emits
  the enclosing real color or a bare `\x03` (color-only reset; markdown-emitted
  bold/italic survive; `\x0F` never emitted); after any `\x03` emission, text
  starting with a digit or comma gets the `\x02\x02` no-op guard.
- **Newline re-emission through one choke point**: a writer decorator wrapping
  the renderer's `w` — after any emitted `\n`, before the first following byte,
  re-emit the effective color (the same discipline as today's `emitRun`, which
  is the translator's single choke point). This covers every render site —
  `writes()` line splitting, paragraph/list indents, hard breaks — regardless
  of which `renderNode` case writes the newline, and preserves the
  no-codes-only-line suppression (nothing visible after the last `\n` ⇒ no
  re-emission).
- **End-of-document auto-close**: bare `\x03` if a real color is still open,
  suppressed when nothing visible followed the last newline. With toggles
  deleted, that is the entire auto-close.

Accepted output divergences from today (benign, all noted for tests):

- **Soft-wrapped lines inside a colored paragraph**: today's translated text
  carries a re-emitted `\x0304` around the source `\n` that the renderer then
  joins with a space (`\x0304a \x0304b\x03`); the decorator emits no code for a
  soft break (no output `\n`) → `\x0304a b\x03`. The dropped code was an
  invisible no-op re-application.
- **A color span crossing into a blockquote**: the quote indent writes its own
  `\x0309>` after the newline; the decorator's re-emit lands before it and the
  indent color wins that line (today the explicit code after the indent wins).
  Rare shape, accepted.
- **Renderer-generated line breaks inside a colored span** (box-drawing table
  rows are the concrete case): today the translator only re-emitted at the
  *model's* newlines, so a color opened before a table colored only the rows
  that happened to carry codes; the decorator re-emits at every output `\n`,
  so all rows render colored. More consistent with the model's intent
  (it asked for the span); accepted and pinned.

Also accepted: a markdown link whose label *contains* a color tag
(`[see [color=red]x[/color]](url)`) no longer parses as a link — the color tag
consumes its brackets first, leaving the outer brackets unbalanced. Models
write links with plain labels; the paste/raw escape hatches exist.

### 3. Toggle deletion

`[b]`/`[i]`/`[u]`/`[s]` become unknown tokens: verbatim, no warn. Deleted
outright: the `bbcode_styles`/`BBCodeStyles` config fields (AIConfig + Service)
and cascade entries, `bbcodeStylesEnabled`, `bbToggleCode`/`bbCanonicalOrder`
state and their re-emit/auto-close loops, `BBCodeStylesFormatting` in
`config/templatevars.toml`, the `bbcode_styles` reference entries in
chats.toml/services.toml, `render.bbcode_styles` in sesssite, and the entire
styles test matrix (including every `*StylesOff` wiring variant — color-only is
now the only mode). Old configs carrying `bbcode_styles = true` load fine
(unknown TOML key, ignored silently — existing convention).

### 4. Streaming

`streamOutput` loses the `bb` field entirely; raw deltas feed the
`StreamingRenderer`, which becomes BBCode-aware:

- **Fragment rendering with forward state carry.** Today each settle re-renders
  the whole settled prefix (`MarkdownToIRCStream(s.buf[:s.settled])`). That
  must change: a re-rendered longer prefix contains the same open color tags,
  so carried state would double-apply. Instead, render only **the new region
  since the last settle**, with the color-stack state flowing forward across
  fragments — exactly how `so.bb`'s state spans deltas today. Settles are
  block-aligned by construction (verified: `s.settled` is only ever assigned
  from `lineEnd`/`blankEnd` — complete-line boundaries), so every fragment
  parses standalone.
- **The partial-tag hold machinery vanishes.** Settles only freeze whole
  lines; tag shapes cannot span newlines (charset excludes `\n`, §1); so a
  complete line ending in `[color=re` is *already decided literal* — the
  one-shot parser agrees (scan hits `\n` before `]` ⇒ not a tag). Mid-line
  splits are never frozen (incomplete lines never settle). Deleted: the
  translator's `pending` buffer, `bbMaxTagLen` name-cap, and the delta-level
  hold in `streamOutput.HandleDelta`.
- **Byte-equality with one-shot is the oracle and is preserved.** "One-shot"
  here is the NEW non-streaming render (§2), not the old `TranslateBBCode`
  output (the §2 accepted divergences apply to both new paths equally). The
  new one-shot re-emits the effective color after every output `\n`
  (`[color=red]a\n\nb[/color]` → `\x0304a\n\n\x0304b\x03`), so a stream cut at
  the blank line emits `\x0304a\n\n` then `\x0304b\x03` — concatenation is
  byte-identical. No auto-close at fragment boundaries (state simply carries);
  auto-close happens only at Flush, matching `so.bb.Flush()` today, with the
  same nothing-visible-after-newline suppression.
- **Emission bookkeeping becomes append-only per fragment**: each fragment's
  render is frozen at its settle; the whole-prefix `HasPrefix` diff and the
  `broken` monotonicity fallback/resync are replaced by per-fragment
  append-only emission. The known link-reference-definition divergence behaves
  as today (each fragment renders with what was known at its settle time —
  same as today's prefix renders; fragments additionally avoid the flush-time
  re-send the fallback could produce).
- `streamOutput.HandleDelta`'s empty-delta guard stays exactly as is
  (`renderer.Process("")` is the flush signal).
- `streamfuzz_test.go` gains BBCode input generators (tags around emphasis,
  across blank lines, inside fences/code spans, nested colors, invalid values)
  so `TestStreamingChunkInvariance` covers the new machinery; the session-3178
  shape becomes a pinned fixture in both parity tests.

### 5. Wiring & config

- `sendRendered` and `handleToolCallResponse` drop their pre-translation calls;
  `MarkdownToIRC` gains a per-call bbcode flag (options struct or functional
  option — plan's choice) so the extension is registered only when
  `bbcodeEnabled(cfg)`. Same for the streaming construction sites (aiCmds.go
  ~1052, ~2063).
- **Paste source**: `sendRendered` computes the paste text via the slimmed
  color-only `TranslateBBCode(rawText, warn, …)` — pastes stay byte-identical
  to today (codes, markdown syntax intact, tags-in-code-blocks still
  translated in the paste — known minor inconsistency with IRC, accepted until
  the paste-site work).
- `completion()` keeps the one-shot translator unchanged (it never
  markdown-renders; both bugs don't apply there).
- `bbcodeEnabled` unchanged; logs keep showing raw model output (translation
  remains emission-only; the session = wire-response invariant is untouched).

### 6. sesssite

- **The parser is shared, not re-ported**: `sesssite/renderer` imports the
  BBCode node types + extension from `markdowntoirc` (no import cycle; the
  site keeps its own HTML renderers). One parser, two renderers — the
  verbatim-port drift risk dies at the source.
- HTML emission: `bbcodeColorOpen` → `<span class="irc-fg-N">` (the existing
  CSP-safe classes; mIRC code → same class the IRC-code path uses), close →
  `</span>`. **CSS nesting is the color stack** — no effective-color
  computation at render time; nested real colors shadow enclosing ones exactly
  as the IRC stack does. Invalid-color placeholders emit nothing (transparent;
  content falls to the enclosing span — the same semantics for free).
  Unbalanced tags at message end auto-close their spans (HTML validity is the
  sibling of the IRC auto-close).
- Deleted: `sesssite/bbcode.go` (the translator port) and its call in
  `buildMessageView` (assistant rows now go through the markdown pipeline with
  the extension under `render.bbcode`, which stays, default true);
  `render.bbcode_styles` config; the code-span IRC-code stripping pass in
  `irc.go` (its stated reason — translator fallout inside code text — no
  longer exists).
- Residual noted: sessions stored during the old broken-ANSI model era may
  contain raw `\x03` bytes inside code spans; with the stripping pass gone
  those render as digits. That era predates BBCode and is gone; accepted.

### 7. Testing

- `MarkdownToIRC`: extension unit tests (accept/decline shapes; precedence over
  links incl. `[color=red](url)` and `[b](url)`-as-link; literal inside
  fences/inline-code/HTML/math); render-state tests (stack nesting,
  placeholder tracking, two-digit + guard emission, newline re-emission,
  auto-close + suppression, the two accepted divergences pinned); streaming ≡
  one-shot parity incl. the 3178 fixture; streamfuzz BBCode generators.
- Root: wiring-hook tests reworked for the new call shapes (sendRendered,
  streamOutput, tool-loop, completion); paste-source pinned byte-identical;
  knob-removal tests (config dir with `bbcode_styles = true` loads; `[b]`
  verbatim everywhere; templatevar file loses `BBCodeStylesFormatting`).
- sesssite: assistant rows show color spans, user rows stay literal, tags
  literal inside code blocks, invalid-color transparency, span auto-close,
  shared-extension import compiles both renderers against the same node kinds.
- Docs: chats.toml/services.toml reference entries, templatevars.toml, and the
  AGENTS.md BBCode bullet rewritten to the new architecture.

## Behavior changes (user-visible summary)

- Tags inside code blocks/fences/inline code/HTML/math render literally on IRC
  and on the site (were: translated).
- Session-3178 shapes render **bold and colored** (were: colored with literal
  `**`).
- `[b]`-family tags render verbatim when styles had been enabled (production
  default was already off; owner confirms models never used them).
- Pastes byte-identical; raw endpoint, storage, logs, and model instructions
  (minus the styles var) unchanged.
