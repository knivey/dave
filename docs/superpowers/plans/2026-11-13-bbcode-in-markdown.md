# BBCode In The Markdown Parser — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move BBCode color translation from a pre-markdown byte pass into the goldmark parser (inline AST nodes rendered by the IRC renderer), fixing the session-3178 `**`-flanking bug and tag-translation-inside-code-blocks, deleting the style-toggle half entirely.

**Architecture:** A color-only goldmark inline-parser extension in `MarkdownToIRC/` produces `BBCodeColorOpen/Close` AST nodes (priority above the link parser so `[color=X]` wins over `[X](url)`); the IRC renderer emits codes through a per-render color-stack state whose text-writer handles newline re-emission and digit guards; the `StreamingRenderer` switches to per-fragment rendering (settles are block-aligned) with the state carried forward; sesssite imports the shared parser and renders HTML spans.

**Tech Stack:** Go 1.25, goldmark (custom inline parsers per `MarkdownToIRC/math.go` precedent), testify.

**Spec:** `docs/superpowers/specs/2026-11-13-bbcode-in-markdown-design.md` — the plan argues from the spec; read both.

## Global Constraints

- Color-only vocabulary, unchanged: 16 canonical names + aliases + numerics 0–15 (two-digit, case-insensitive); value charset letters/digits/spaces, never newlines.
- Emission invariants unchanged: every color exactly two digits; `[/color]` re-emits the nearest real enclosing color or a bare `\x03`; `\x0F` never emitted; `\x02\x02` guard before digit/comma text following a `\x03` emission; no codes-only line.
- Style toggles (`[b] [i] [u] [s]`) are deleted: unknown tokens, verbatim, no warn. `bbcode_styles` knob deleted everywhere; old configs carrying it must load (unknown TOML key).
- Warn texts preserved verbatim: `bbcode: stray close %q with no open color stripped`, `bbcode: invalid tag %q: unknown color, span tracked transparent`.
- The `bbcode` knob (cascade command > service > false, `bbcodeEnabled`) keeps its exact meaning.
- Storage stays raw; logs show raw; pastes stay byte-identical to today (color-only one-shot translation).
- goldmark inline-parser priority for BBCode: 150 (link parser is 200 — verified in goldmark `parser.go` `defaultInlineParsers`; lower value runs first; returning nil passes to the next parser with position reset).
- Every task ends with `go test ./...`, `go fmt ./...`, `go vet ./...` green, then a commit.
- After every task: MANDATORY code review (spec-compliance + quality) per AGENTS.md before moving on.

## Review Focus

Spec-implied input classes no single task fully exercises; each is pinned by the named test in its owning task:

1. **Streaming delta splitting a tag mid-line** (`[col` + `or=red]x`): must not freeze literal — the incomplete line never settles, the tag completes inside it. Pinned by `TestBBCodeStreamTagSplitAcrossDeltas` (Task 4).
2. **Digit/comma text immediately after any `\x03` emission** (open, re-emit after newline, close-re-emit): guard `\x02\x02` inserted in all three positions through the AST path. Pinned by `TestBBRenderGuards` rows `guard_after_open`, `guard_after_reemit`, `guard_after_close` (Task 3).
3. **Emphasis adjacent to tags, both orders**: `**[color=red]x[/color]**` and `[color=cyan]**x**[/color]` (the 3178 class) — bold AND color apply. Pinned by `TestBBRenderFlanking` (Task 3).
4. **Color span crossing a fenced code block** (open before, close after): tags inside the fence stay literal, color resumes after the block. Pinned by `TestBBRenderSpanCrossingCodeBlock` (Task 3) and `TestBBCodeStreamSpanCrossingSettleBoundary` (Task 4).
5. **Stray `[/color]` with nothing open** (warn + strip, no code, no crash) and **invalid-color placeholder pairing across a streaming fragment boundary**: pinned by `TestBBRenderStrayAndPlaceholder` (Task 3) and `TestBBCodeStreamPlaceholderAcrossFragments` (Task 4).

---

### Task 1: Move the BBCode translator into markdowntoirc (color-only)

**Files:**
- Create: `MarkdownToIRC/bbcode.go` (moved from root `bbcode.go`, slimmed)
- Create: `MarkdownToIRC/bbcode_test.go` (moved color-only tests)
- Modify: `bbcode.go` → shrinks to `bbcodeEnabled` + `bbcodeWarnFn` only
- Modify: `aiCmds.go:124,811,846,1052,2063` → call `markdowntoirc.TranslateBBCode` / `markdowntoirc.NewBBCodeStream`
- Modify: `bbcode_test.go` → translator-family tests move out; wiring tests stay (adapted)

**Interfaces:**
- Produces: `markdowntoirc.TranslateBBCode(text string, warn func(string)) string` (one-shot, color-only)
- Produces: `markdowntoirc.NewBBCodeStream(warn func(string)) *markdowntoirc.BBCodeStream` with `Process(delta string) string` and `Flush() string` (the old streaming translator, styles field deleted — still needed for plain-mode streaming and the paste one-shot's internals)
- Produces (package-internal, later tasks build on): `parseBBTag`, `bbColorValue`, `bbColorNames`, `bbPad2`
- Root keeps: `bbcodeEnabled(cfg AIConfig) bool`, `bbcodeWarnFn(logger) func(string)` in `bbcode.go`

**What moves (verbatim except styles/toggle deletion):** `bbMaxTagLen`, `bbColorNames`, `bbCanonicalOrder` constant dies, `bbToggleCode` dies, `bbcodeTranslator` → renamed `BBCodeStream` (exported; `bold/ital/under/str` fields die, `styles` field dies, `toggleOpen`/`setToggle`/`anyOpen` die — `anyOpen`'s only remaining use is arming `pendingNL`, which becomes `if t.bbEffectiveColor() != ""`), `Process`, `Flush` (toggle loop deleted; keeps the bare-`\x03` + pendingNL suppression), `emitRun` (toggle re-emit block deleted), `parseBBTag` (known = `name == "color"` only; the `bbToggleCode` lookups die), `applyTag` (whole b/i/u/s branch dies; unknown-name branch simplifies), `bbColorValue`, `bbPad2`, `isASCIILetter/Digit`, `TranslateBBCode`.

- [ ] **Step 1: Create `MarkdownToIRC/bbcode_test.go` with the color-only translator tests (moved + slimmed)**

Move from root `bbcode_test.go`, adapting: `TranslateBBCode(x, nil, true)` → `TranslateBBCode(x, nil)`; every case whose input was a toggle (`[b]…`) flips to a verbatim expectation. Representative shape (keep ALL existing color rows from `TestTranslateBBCodeColors`, `InvalidTags`, `Guards`, `ColorStack`, `LinePersistence`, `InvalidColorTracking`, `AutoClose`, `UnknownCloseWithValue`, `ValueHoldCap`, `ValueHoldInvariance`, chunk-invariance, idempotency):

```go
package markdowntoirc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTranslateBBCodeColors(t *testing.T) {
	cases := []struct{ value, want string }{
		{"red", "\x0304x\x03"},
		{"light green", "\x0309x\x03"},
		{"12", "\x0312x\x03"},
		{"16", "[color=16]x[/color=16]"}, // invalid numeric: unknown token, verbatim
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, TranslateBBCode("[color="+tc.value+"]x[/color]", nil), tc.value)
	}
}

// Toggles are gone: verbatim unknown tokens, colors still translate.
func TestTranslateBBCodeTogglesVerbatim(t *testing.T) {
	assert.Equal(t, "[b]bold[/b] and [i]it[/i]",
		TranslateBBCode("[b]bold[/b] and [i]it[/i]", nil))
	assert.Equal(t, "\x0304x\x03", TranslateBBCode("[color=red]x[/color]", nil))
}

func TestTranslateBBCodeColorStack(t *testing.T) {
	assert.Equal(t, "\x0304a\x0302b\x0304c\x03d",
		TranslateBBCode("[color=red]a[color=blue]b[/color]c[/color]d", nil))
	assert.Equal(t, "abc",
		TranslateBBCode("a[color=bogus]b[/color]c", nil)) // placeholder pairs silently
}
```

- [ ] **Step 2: Run — verify it fails to compile (no TranslateBBCode in package)**

Run: `go test ./MarkdownToIRC/ -run TestTranslateBBCode`
Expected: FAIL (undefined: TranslateBBCode)

- [ ] **Step 3: Move + slim the translator into `MarkdownToIRC/bbcode.go`**

Copy root `bbcode.go` to `MarkdownToIRC/bbcode.go`, change package to `markdowntoirc`, apply the deletions listed in Interfaces. Header comment rewritten to describe the new split (byte translator = paste/legacy/plain-mode path; the markdown-aware path arrives in Tasks 2–4). Export `TranslateBBCode` and `BBCodeStream`/`NewBBCodeStream`. Delete root's moved code, leaving only:

```go
package main

// bbcodeEnabled resolves whether BBCode is on for this config (cascade
// command > service > false, materialized in ApplyDefaults; nil check is
// defensive for hand-built configs). Gates the markdown-renderer BBCode
// extension and the plain-mode byte translator (see MarkdownToIRC/bbcode.go).
func bbcodeEnabled(cfg AIConfig) bool { return cfg.BBCode != nil && *cfg.BBCode }

func bbcodeWarnFn(l logxi.Logger) func(string) {
	if l == nil {
		return nil
	}
	return func(msg string) { l.Warn(msg) }
}
```

Update the five root call sites (aiCmds.go 124, 811, 846, 1052, 2063) to the new signatures — pass no styles arg:

```go
text = markdowntoirc.TranslateBBCode(text, bbcodeWarnFn(logger))
...
sOut.bb = markdowntoirc.NewBBCodeStream(bbcodeWarnFn(cr.logger))
```

- [ ] **Step 4: Adapt root `bbcode_test.go` wiring tests, delete moved ones**

Root keeps (with `newBBCodeTranslator(nil, true)` → `markdowntoirc.NewBBCodeStream(nil)`): `TestStreamOutputBBCode` (both subtests), `TestBBCodeNoCodesOnlyLine`, `TestBBCodeTagOnlyLineNoCodes`, `TestSendRenderedBBCode` and the wiring family (they assert call-site behavior, still valid — output bytes are unchanged by this task). Delete the translator-family tests that moved.

- [ ] **Step 5: Full verification + commit**

Run: `go test ./... && go fmt ./... && go vet ./...`
Expected: all PASS (byte-for-byte behavior preserved for colors; toggle outputs now verbatim)
Commit: `refactor: move color-only bbcode translator into markdowntoirc`

### Task 2: BBCode AST nodes + inline parser + extension

**Files:**
- Create: `MarkdownToIRC/bbcode_ast.go`
- Create: `MarkdownToIRC/bbcode_ast_test.go`

**Interfaces:**
- Consumes: `parseBBTag`, `bbColorValue` (Task 1)
- Produces: `KindBBCodeColorOpen`, `KindBBCodeColorClose` (`ast.NodeKind`); `BBCodeColorOpen` struct (`ast.BaseInline` + `Code string` — resolved two-digit code, `""` = invalid/transparent; `Raw string` — original tag bytes for warns); `BBCodeColorClose` struct (`ast.BaseInline` + `Raw string`); `BBCodeExtension` struct (`Warn func(string)`) implementing `goldmark.Extender` — registers the inline parser at priority 150 (link parser is 200)

- [ ] **Step 1: Write the failing parser tests**

```go
package markdowntoirc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/util"
)

// parseDoc runs text through a goldmark instance with the BBCode extension
// and returns the document node for structural assertions.
func parseDoc(t *testing.T, text string) (source []byte, doc ast.Node) {
	t.Helper()
	md := goldmark.New(goldmark.WithExtensions(&BBCodeExtension{}))
	node := mockDocParse(t, md, text) // helper: md.Parser().Parse(text.NewReader(src))
	return []byte(text), node
}

func TestBBCodeParserAccepts(t *testing.T) {
	for _, in := range []string{
		"[color=red]x", "[COLOR=RED]x", "[color=light green]x", "[color=12]x",
		"a [/color] b", "[color=red]y[/color=red]z",
	} {
		_, doc := parseDoc(t, in)
		assert.True(t, containsKind(doc, KindBBCodeColorOpen) || containsKind(doc, KindBBCodeColorClose), in)
	}
}

func TestBBCodeParserDeclines(t *testing.T) {
	for _, in := range []string{
		"[b]x", "[table]", "[1]", "a[i] + b[j]", "[]", "[=x]", "[color]", "[color=]",
		"[color=16]", "[color=red x", "[col\nor=red]x", // newline in tag shape: never a tag
	} {
		_, doc := parseDoc(t, in)
		assert.False(t, containsKind(doc, KindBBCodeColorOpen), in)
	}
}

// Colors win over links: the tag is consumed; (url) is plain text.
func TestBBCodeParserColorsBeatLinks(t *testing.T) {
	_, doc := parseDoc(t, "[color=red](http://x.example)")
	require.True(t, containsKind(doc, KindBBCodeColorOpen))
	assert.False(t, containsKind(doc, astKindLink(t))) // no markdown link node
}

// Unknown tag-shaped labels fall through to the link parser.
func TestBBCodeParserUnknownLabelStaysLink(t *testing.T) {
	_, doc := parseDoc(t, "[b](http://x.example)")
	assert.True(t, containsKind(doc, astKindLink(t)))
}
```

(Implement `containsKind`/`astKindLink`/`mockDocParse` as small local helpers walking the tree; use `github.com/yuin/goldmark/ast`.)

- [ ] **Step 2: Run — verify compile failure (types undefined)**

Run: `go test ./MarkdownToIRC/ -run TestBBCodeParser`
Expected: FAIL (undefined: BBCodeExtension …)

- [ ] **Step 3: Implement nodes + parser + extension**

```go
package markdowntoirc

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var (
	KindBBCodeColorOpen  = ast.NewNodeKind("BBCodeColorOpen")
	KindBBCodeColorClose = ast.NewNodeKind("BBCodeColorClose")
)

// BBCodeColorOpen is a [color=X] tag. Code is the RESOLVED two-digit IRC
// code (parse-time resolution — both renderers read it without re-resolving);
// "" means the value was invalid: the span is a TRANSPARENT placeholder.
type BBCodeColorOpen struct {
	ast.BaseInline
	Code string
	Raw  string
}
type BBCodeColorClose struct{ ast.BaseInline; Raw string }

func (n *BBCodeColorOpen) Kind() ast.NodeKind  { return KindBBCodeColorOpen }
func (n *BBCodeColorClose) Kind() ast.NodeKind { return KindBBCodeColorClose }
func (n *BBCodeColorOpen) Dump(source []byte, level int)  { ast.DumpHelper(n, source, level, nil, nil) }
func (n *BBCodeColorClose) Dump(source []byte, level int) { ast.DumpHelper(n, source, level, nil, nil) }

// bbcodeParser triggers on '[' at priority 150 — BELOW the numeric value of
// goldmark's link parser (200), i.e. it runs FIRST (lower = higher priority)
// and its decline (nil) hands the '[' back for link/text jurisdiction. That
// ordering IS the "colors win over links" ruling: [color=red](url) is a
// color tag followed by literal "(url)", while [b](url) (unknown name)
// declines and parses as a markdown link.
type bbcodeParser struct{ warn func(string) }

func (p *bbcodeParser) Trigger() []byte { return []byte{'['} }

func (p *bbcodeParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if len(line) == 0 || line[0] != '[' {
		return nil
	}
	// PeekLine includes the trailing newline, so a tag can never consume
	// past the line — an unterminated candidate at line end declines here
	// and renders as literal '[' (one-shot agreement: parseBBTag hits '\n'
	// before ']'). Settles are whole lines (StreamingRenderer), so a
	// decided line can never split a decidable tag.
	tag, ok := parseBBTag(string(line))
	if !ok || tag.consumed <= 1 || !tag.known {
		return nil // incomplete at final-line end, not tag-shaped, or unknown name
	}
	raw := string(line[:tag.consumed])
	if tag.closing {
		block.Advance(tag.consumed)
		return &BBCodeColorClose{Raw: raw}
	}
	code, valid := bbColorValue(tag.value)
	if !valid && p.warn != nil {
		// Same text as the byte translator (Task 1) — pinned by Global Constraints.
		p.warn("bbcode: invalid tag " + strconv.Quote(raw) + ": unknown color, span tracked transparent")
	}
	block.Advance(tag.consumed)
	return &BBCodeColorOpen{Code: code, Raw: raw}
}

// BBCodeExtension registers the parser only — rendering lives in the IRC
// Renderer's node cases (mathExtension precedent) and, for sesssite, in the
// site's own HTML node renderer.
type BBCodeExtension struct{ Warn func(string) }

func (e *BBCodeExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(
		util.Prioritized(&bbcodeParser{warn: e.Warn}, 150),
	))
}
```

Note: after Task 1, `tag.known` is already `name == "color"` only. `strconv` import needed.

- [ ] **Step 4: Run parser tests**

Run: `go test ./MarkdownToIRC/ -run 'TestBBCodeParser' -v`
Expected: PASS

- [ ] **Step 5: Full verification + commit**

Run: `go test ./... && go fmt ./... && go vet ./...`
Commit: `feat(markdowntoirc): bbcode inline parser + ast nodes (colors beat links)`

### Task 3: Render state + renderer cases + opts entry points

**Files:**
- Create: `MarkdownToIRC/bbcode_render.go`
- Create: `MarkdownToIRC/bbcode_render_test.go`
- Modify: `MarkdownToIRC/MarkdownToIRC.go` — `Renderer` struct (+`bb *bbRenderState`), `Render()` (wrap the walk writer), `renderNode` (+2 cases), `renderNodeTo` (+2 cases for table cells), new entry points near `MarkdownToIRC` (line ~580)

**Interfaces:**
- Consumes: node kinds (Task 2)
- Produces: `BBRenderOptions{BBCode bool; Warn func(string)}`; `MarkdownToIRCOpts(opts BBRenderOptions, response string) string`; `MarkdownToIRCStreamOpts(opts BBRenderOptions, response string) string` (unexported-state field inside opts lets `StreamingRenderer` inject its carried state — same package). `MarkdownToIRC`/`MarkdownToIRCStream` keep their signatures (bbcode off).

- [ ] **Step 1: Write the failing render tests**

```go
package markdowntoirc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func bb(s string) string { return MarkdownToIRCOpts(BBRenderOptions{BBCode: true}, s) }

func TestBBRenderSemantics(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"basic", "[color=red]x[/color]", "\x0304x\x03"},
		{"nested stack", "[color=red]a[color=blue]b[/color]c[/color]d", "\x0304a\x0302b\x0304c\x03d"},
		{"placeholder pairs", "a[color=bogus]b[/color]c", "abc"},
		{"paragraph re-emit", "[color=red]a\n\nb[/color]", "\x0304a\n\x0304b\x03"},
		{"auto-close suppressed after newline", "[color=red]a\n", "\x0304a\n"},
		{"auto-close when open", "[color=red]a", "\x0304a\x03"},
		{"stray close stripped", "a[/color]b", "ab"},
		{"toggles verbatim", "[b]x[/b]", "[b]x[/b]"},
		{"tags literal in fence", "```\n[color=red]x[/color]\n```", " \x030,90[color=red]x[/color]\x03 "},
		{"tags literal in inline code", "use `[color=red]`", "use \x030,90[color=red]\x03"},
		{"unknown token verbatim", "see [table] and [1]", "see [table] and [1]"},
		{"link with unknown label still link", "[b](http://x.example)", "b: http://x.example"},
		{"colors beat links", "[color=red](http://x.example)", "\x0304(http://x.example)\x03"},
		{"tags literal in math", "$[color=red]x$", "$[color=red]x$"}, // fallback form; no codes inside
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, bb(tc.in), tc.name)
	}
}

// The session-3178 fixture: bold AND color, every line (today: literal **).
func TestBBRenderFlanking(t *testing.T) {
	in := "[color=cyan]**A**\n**B**\n**C**[/color]"
	out := bb(in)
	assert.Contains(t, out, "\x02A\x02")
	assert.Contains(t, out, "\x02C\x02")
	assert.NotContains(t, out, "**")
	assert.Contains(t, out, "\x0310", "colored")
}

func TestBBRenderGuards(t *testing.T) {
	assert.Equal(t, "\x0304\x02\x025x\x03", bb("[color=red]5x[/color]"), "guard_after_open")
	assert.Equal(t, "\x0304a\n\x0304\x02\x025\x03", bb("[color=red]a\n5[/color]"), "guard_after_reemit")
	assert.Equal(t, "\x0304a\x0304\x02\x025", bb("[color=red]a[color=red]5"), "guard_after_close")
}

func TestBBRenderSpanCrossingCodeBlock(t *testing.T) {
	out := bb("[color=red]before\n```\n[color=blue]x[/color]\n```\nafter[/color]")
	assert.Contains(t, out, "[color=blue]x[/color]", "tags inside fence literal")
	assert.Regexp(t, `\x0304after`, out, "color resumes after the block")
}

func TestBBRenderStrayAndPlaceholder(t *testing.T) {
	var warns []string
	warn := func(m string) { warns = append(warns, m) }
	out := MarkdownToIRCOpts(BBRenderOptions{BBCode: true, Warn: warn}, "a[/color]b[color=bogus]c[/color]d")
	assert.Equal(t, "abcd", out)
	assert.Contains(t, strings.Join(warns, "\n"), "stray close")
	assert.Contains(t, strings.Join(warns, "\n"), "unknown color")
}

// Pinned divergences (spec §2): soft-wrap drops the no-op re-emit; table
// rows inside a span now re-emit on every rendered row line.
func TestBBRenderPinnedDivergences(t *testing.T) {
	assert.Equal(t, "\x0304a b\x03", bb("[color=red]a\nb[/color]"), "soft wrap joins, one color")
	// Table rows: every row line carries the color (today only coded rows did).
	out := bb("[color=red]\n| a | b |\n|---|---|\n| 1 | 2 |\n[/color]")
	rows := strings.Split(strings.TrimSpace(out), "\n")
	n := strings.Count(out, "\x0304")
	assert.GreaterOrEqual(t, n, len(rows)-1, "each rendered row line re-emits")
}

// Idempotence at the pipeline level: re-rendering output adds no codes.
func TestBBRenderIdempotent(t *testing.T) {
	once := bb("[color=red]x[/color] and [table]")
	assert.Equal(t, once, bb(once))
}

// bbcode off: tags never render as anything (existing signature unchanged).
func TestBBRenderOff(t *testing.T) {
	assert.Equal(t, "[color=red]x[/color]", MarkdownToIRC("[color=red]x[/color]"))
}
```

- [ ] **Step 2: Run — verify failure**

Run: `go test ./MarkdownToIRC/ -run TestBBRender -v`
Expected: FAIL (undefined: MarkdownToIRCOpts)

- [ ] **Step 3: Implement `bbcode_render.go` (the state machine)**

```go
package markdowntoirc

import "bytes"

// bbRenderState carries the color stack across the render walk AND across
// streaming fragments (the StreamingRenderer owns one instance for its
// lifetime; one-shot renders construct a fresh one per call). All methods
// take the target buffer explicitly: the walk passes its own buffer, table
// cell extraction (renderNodeTo) passes the cell buffer — order is
// call-order on the same/own buffer, single-threaded walk.
type bbRenderState struct {
	stack     []string // two-digit codes, innermost last; "" = transparent placeholder
	pendingNL bool     // a \n awaits re-emission before the next visible byte
	guard     bool     // a \x03 emission awaits the digit/comma decision
}

func (s *bbRenderState) effective() string {
	for i := len(s.stack) - 1; i >= 0; i-- {
		if s.stack[i] != "" {
			return s.stack[i]
		}
	}
	return ""
}

// textInto is the single choke point for TEXT bytes (the translator's old
// emitRun, verbatim semantics): newlines arm the re-emit (a real color open),
// the first following byte re-emits it, and a digit/comma right after a
// color emission gets the \x02\x02 no-op guard.
func (s *bbRenderState) textInto(buf *bytes.Buffer, p []byte) {
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\n' {
			buf.WriteByte('\n')
			if s.effective() != "" {
				s.pendingNL = true
			}
			s.guard = false
			continue
		}
		if s.pendingNL {
			s.pendingNL = false
			if eff := s.effective(); eff != "" {
				buf.WriteString("\x03" + eff)
				s.guard = true
			}
		}
		if s.guard && (c >= '0' && c <= '9' || c == ',') {
			buf.WriteString("\x02\x02")
		}
		s.guard = false
		buf.WriteByte(c)
	}
}

func (s *bbRenderState) openInto(buf *bytes.Buffer, n *BBCodeColorOpen) {
	s.stack = append(s.stack, n.Code)
	if n.Code == "" || s.pendingNL {
		return // transparent placeholder, or deferred: re-emit reconstructs
	}
	buf.WriteString("\x03" + n.Code)
	s.guard = true
}

func (s *bbRenderState) closeInto(buf *bytes.Buffer, n *BBCodeColorClose, warn func(string)) {
	if len(s.stack) == 0 {
		if warn != nil {
			warn("bbcode: stray close " + quoteBB(n.Raw) + " with no open color stripped")
		}
		return
	}
	popped := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	if popped == "" || s.pendingNL {
		return // placeholder span never emitted; deferred close: re-emit reconstructs
	}
	if eff := s.effective(); eff != "" {
		buf.WriteString("\x03" + eff)
	} else {
		buf.WriteByte(0x03)
	}
	s.guard = true
}

// autoClose is the end-of-render/flush tail: a bare \x03 when a real color
// is open AND something visible followed the last newline (codes-only-line
// suppression — a codes-only PRIVMSG is a blank bot message).
func (s *bbRenderState) autoClose() string {
	if s.pendingNL || s.effective() == "" {
		return ""
	}
	return "\x03"
}

type bbTextWriter struct{ st *bbRenderState; buf *bytes.Buffer }

func (w *bbTextWriter) Write(p []byte) (int, error) {
	w.st.textInto(w.buf, p)
	return len(p), nil
}
```

Add a tiny `quoteBB(s string) string` (or reuse `strconv.Quote`) so warn texts match Global Constraints byte-for-byte.

- [ ] **Step 4: Wire the Renderer**

In `MarkdownToIRC.go`: add `bb *bbRenderState` and `bbWarn func(string)` to `Renderer`. In `Render()`:

```go
	var buf bytes.Buffer
	var walkW io.Writer = &buf
	if r.bb != nil {
		walkW = &bbTextWriter{st: r.bb, buf: &buf}
	}
	err := ast.Walk(n, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		return r.renderNode(walkW, node, entering), nil
	})
```

In `renderNode` (and `renderNodeTo`, whose cell text must carry codes — pass the cell buffer):

```go
	case KindBBCodeColorOpen:
		if entering && r.bb != nil {
			r.bb.openInto(/* the buffer this case can reach: see note */ , node.(*BBCodeColorOpen))
		}
	case KindBBCodeColorClose:
		if entering && r.bb != nil {
			r.bb.closeInto(..., node.(*BBCodeColorClose), r.bbWarn)
		}
```

NOTE for the implementer: the walk cases write codes straight to the same `bytes.Buffer` the walk writes into (`r.bb.outBuf`, set in `Render()`); `renderNodeTo` builds cell text in its own buffer — call `openInto`/`closeInto` with THAT buffer, and route its text-segment writes through `r.bb.textInto` so guards apply inside cells. `extractTableData`'s text collection (`collectText`) needs the two node kinds appended as code bytes via the same state methods (cells are single-line: re-emit N/A, guard required to match today's translated cell bytes).

Entry points:

```go
type BBRenderOptions struct {
	BBCode bool
	Warn   func(string)
	state  *bbRenderState // same-package injection for the StreamingRenderer
}

func MarkdownToIRCOpts(opts BBRenderOptions, response string) string {
	st := opts.state
	if st == nil && opts.BBCode {
		st = &bbRenderState{}
	}
	r := &Renderer{}
	if st != nil {
		r.bb, r.bbWarn = st, opts.Warn
	}
	md := goldmark.New(goldmark.WithExtensions(extension.Table, extension.TaskList, &mathExtension{}, bbExt(opts)),
		goldmark.WithRenderer(r))
	var buf bytes.Buffer
	md.Convert([]byte(response), &buf)
	out := buf.String()
	if st != nil {
		out += st.autoClose()
	}
	return out
}

func bbExt(opts BBRenderOptions) goldmark.Extender {
	if !opts.BBCode {
		return nil // nil extensions are skipped by a small wrapper: build the
	}
	return &BBCodeExtension{Warn: opts.Warn}
}
```

(If goldmark's variadic `WithExtensions` cannot take a nil, build the slice conditionally. `MarkdownToIRCStreamOpts` mirrors this with `&Renderer{streaming: true, …}` — no autoClose append there; the StreamingRenderer owns flush.)

- [ ] **Step 5: Run render tests**

Run: `go test ./MarkdownToIRC/ -run 'TestBBRender|TestBBCode' -v`
Expected: PASS. If the table-cell guard rows diverge, fix `renderNodeTo`/`collectText` routing — do not weaken the test.

- [ ] **Step 6: Full verification + commit**

Run: `go test ./... && go fmt ./... && go vet ./...`
Commit: `feat(markdowntoirc): bbcode render state + renderer cases + opts entry points`

### Task 4: StreamingRenderer fragment rendering + state carry

**Files:**
- Modify: `MarkdownToIRC/MarkdownToIRC.go` — `StreamingRenderer` struct, `Process`, `Flush` (render path only; `scan`/settle logic UNTOUCHED), `NewStreamingRendererOpts`
- Modify: `MarkdownToIRC/streaming_test.go` (or new `bbcode_streaming_test.go`)

**Interfaces:**
- Consumes: `MarkdownToIRCStreamOpts` with state injection (Task 3)
- Produces: `NewStreamingRendererOpts(opts BBRenderOptions) *StreamingRenderer` (bbcode-aware, state carried across fragments; auto-close only at Flush). `NewStreamingRenderer()` unchanged (plain).

- [ ] **Step 1: Write the failing streaming tests**

```go
package markdowntoirc

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func streamChunks(t *testing.T, opts BBRenderOptions, doc string, chunks []string) string {
	t.Helper()
	var sb strings.Builder
	sr := NewStreamingRendererOpts(opts)
	for _, c := range chunks {
		for _, line := range sr.Process(c) {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}
	for _, line := range sr.Process("") { // flush
		sb.WriteString(line)
		sb.WriteString("\n")
	}
	return sb.String()
}

// Streaming ≡ one-shot byte-equality for bbcode docs, across chunkings.
func TestBBCodeStreamChunkInvariance(t *testing.T) {
	docs := []string{
		"[color=red]a\n\nb[/color]",
		"[color=red]para\n\n- item\n- item2\n\nafter[/color]",
		"[color=red]before\n```\ncode\n```\nafter[/color]",
		"[color=red]**bold** and `code`[/color]",
		"[color=bogus]x[/color] and [/color] stray",
	}
	for _, doc := range docs {
		want := MarkdownToIRCOpts(BBRenderOptions{BBCode: true}, doc) + "\n"
		for size := 1; size <= len(doc); size++ {
			var chunks []string
			for i := 0; i < len(doc); i += size {
				e := min(i+size, len(doc))
				chunks = append(chunks, doc[i:e])
			}
			require.Equal(t, want, streamChunks(t, BBRenderOptions{BBCode: true}, doc, chunks), "size %d", size)
		}
	}
}

// The blank-line boundary: state carries, NO auto-close at the fragment edge
// — concatenation is byte-identical to one-shot.
func TestBBCodeStreamSpanCrossingSettleBoundary(t *testing.T) {
	got := streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]a\n\nb[/color]",
		[]string{"[color=red]a\n", "\nb[/color]"})
	assert.Equal(t, "\x0304a\n\x0304b\x03\n", got)
}

// A tag split across deltas within a line: the line never settles until
// complete, so the tag is never frozen literal.
func TestBBCodeStreamTagSplitAcrossDeltas(t *testing.T) {
	got := streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]x\n",
		[]string{"[col", "or=red]x\n"})
	assert.Equal(t, "\x0304x\n", got)
}

// Invalid-color placeholder opened in one fragment, closed in a later one.
func TestBBCodeStreamPlaceholderAcrossFragments(t *testing.T) {
	got := streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]a\n\n[color=bogus]b\n\nc[/color]d[/color]",
		[]string{"[color=red]a\n\n", "[color=bogus]b\n\nc[/color]d[/color]"})
	want := MarkdownToIRCOpts(BBRenderOptions{BBCode: true}, "[color=red]a\n\n[color=bogus]b\n\nc[/color]d[/color]") + "\n"
	assert.Equal(t, want, got)
}

// Flush auto-closes a still-open color (and suppresses after a newline).
func TestBBCodeStreamFlushAutoClose(t *testing.T) {
	assert.Equal(t, "\x0304x\x03\n", streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]x", []string{"[color=red]x"}))
	assert.Equal(t, "\x0304x\n", streamChunks(t, BBRenderOptions{BBCode: true}, "[color=red]x\n", []string{"[color=red]x\n"}))
}
```

- [ ] **Step 2: Run — verify failure**

Run: `go test ./MarkdownToIRC/ -run TestBBCodeStream -v`
Expected: FAIL (undefined: NewStreamingRendererOpts)

- [ ] **Step 3: Implement fragment rendering**

Struct changes: add `bbOpts BBRenderOptions`; delete `emitted string` and `broken bool` (the whole-prefix `HasPrefix` oracle and the resync fallback die — fragment renders are frozen at settle, emission is append-only); `rendered` keeps meaning "start offset of the next unrendered fragment".

`Process`/flush render path becomes:

```go
func (s *StreamingRenderer) renderFragment(end int, flush bool) []string {
	opts := s.bbOpts
	if opts.BBCode {
		if s.bbState == nil {
			s.bbState = &bbRenderState{}
		}
		opts.state = s.bbState
	}
	out := MarkdownToIRCStreamOpts(opts, s.buf[s.rendered:end])
	if flush && opts.BBCode {
		out += s.bbState.autoClose()
	}
	s.rendered = end
	out = strings.TrimPrefix(out, "\n") // block-leading \n: entry boundary stands for it
	if out == "" {
		return nil
	}
	return []string{out}
}

func (s *StreamingRenderer) Process(delta string) []string {
	if delta == "" {
		return s.renderFragment(len(s.buf), true)
	}
	s.buf += delta
	s.scan()
	if s.settled <= s.rendered {
		return nil
	}
	return s.renderFragment(s.settled, false)
}
```

(Add `bbState *bbRenderState` to the struct. `NewStreamingRendererOpts(opts)` stores opts. Verify against the CURRENT emit() semantics: today's entry is `out[len(emitted):]` with one leading `\n` trimmed — the fragment render's own `Render()` already trims the leading `\n`, so the extra TrimPrefix is a no-op safety; keep it.) The old `emit()` and its `broken` resync are deleted; `s.emitted` bookkeeping goes with them.

- [ ] **Step 4: Run the new tests AND the full existing streaming oracles**

Run: `go test ./MarkdownToIRC/ -run 'TestBBCodeStream|TestStreaming|TestLatency' -v && go test ./MarkdownToIRC/`
Expected: ALL PASS — the plain-markdown goldens, chunk invariance, parity, and latency tests must pass UNCHANGED (fragment rendering must be byte-identical to the old incremental emission for plain markdown; a failure here means a settle boundary is not block-aligned — fix the boundary classification, never the oracle).

- [ ] **Step 5: Full verification + commit**

Run: `go test ./... && go fmt ./... && go vet ./...`
Commit: `feat(markdowntoirc): streaming renderer fragment rendering with bbcode state carry`

### Task 5: streamfuzz BBCode generators

**Files:**
- Modify: `MarkdownToIRC/streamfuzz_test.go`

**Interfaces:**
- Consumes: `NewStreamingRendererOpts`, `MarkdownToIRCOpts` (Tasks 3–4)

- [ ] **Step 1: Add BBCode document generation + opts plumbing**

In the generator: add a `bbTags bool` mode flag threaded from the test entry points. When on, random paragraphs get wrapped with random opens/closes from `{red, blue, light green, 12, bogus, ""}` — including shapes around emphasis, inside fences (must stay literal), spanning blank-line runs, and split mid-tag in the delta stream (the delta splitter already cuts anywhere). Parity reference for P2 becomes:

```go
func referenceRender(doc string, bb bool) string {
	if !bb {
		return normalizeIRC(MarkdownToIRCStream(doc))
	}
	return normalizeIRC(MarkdownToIRCStreamOpts(BBRenderOptions{BBCode: true}, doc))
}
```

with the streaming side using `NewStreamingRendererOpts(BBRenderOptions{BBCode: true})`. Run `TestStreamingChunkInvariance` and the P2 parity path over BOTH modes (double the seeded corpus; same seeds for reproducibility).

- [ ] **Step 2: Survey run (env-gated, one-off)**

Run: `DAVE_STREAM_SURVEY=1 go test ./MarkdownToIRC/ -run TestStreamingSurvey -v` (with a time-box; shrink recorded breakers if any)
Expected: 0 new breakers for bbcode-mode documents. If breakers appear: fix the settle/renderer, record nothing (they are bugs, not fixtures), re-run.

- [ ] **Step 3: Full verification + commit**

Run: `go test ./... && go fmt ./... && go vet ./...`
Commit: `test(markdowntoirc): streamfuzz bbcode generators + parity modes`

### Task 6: Root wiring

**Files:**
- Modify: `aiCmds.go` — `sendRendered` (~809), `handleToolCallResponse` (~846), `completion` (~124), both stream construction sites (~1045, ~2056), `streamOutput` struct + `HandleDelta`/`Flush`
- Modify: `bbcode_test.go`, `aiStreamOutput_test.go` (wiring tests reworked)
- Delete: `bbcode.go` (helpers relocate to `aiCmds.go` near their call sites: `bbcodeEnabled`, `bbcodeWarnFn`)

**Interfaces:**
- Consumes: `MarkdownToIRCOpts`, `NewStreamingRendererOpts`, `TranslateBBCode`, `NewBBCodeStream` (Tasks 1–4)

- [ ] **Step 1: Rework the wiring tests first (they define the contract)**

`sendRendered` contract: IRC text via `MarkdownToIRCOpts` when RenderMarkdown; when NOT RenderMarkdown but bbcode on, text = `TranslateBBCode(raw)` (plain mode keeps byte translation); paste source = `TranslateBBCode(raw)` when bbcode on (byte-identical to today), else raw. In `TestSendRenderedBBCode` add:

```go
t.Run("paste source stays one-shot translated", func(t *testing.T) {
	// long colored reply forced through the pastebin path: uploaded text
	// equals TranslateBBCode(raw) — codes, markdown syntax intact.
})
t.Run("rendermarkdown off keeps byte translation", func(t *testing.T) { ... })
t.Run("3178 fixture renders bold via opts", func(t *testing.T) { ... })
```

`TestStreamOutputBBCode` renderer-mode subtest feeds RAW deltas (no `bb`) and expects the same IRC codes (the renderer now owns translation); plain-mode subtest keeps `bb = markdowntoirc.NewBBCodeStream(nil)`.

- [ ] **Step 2: Run — verify failures**

Run: `go test . -run 'TestSendRenderedBBCode|TestStreamOutputBBCode|TestHandleToolCallResponseBBCode|TestCompletionBBCode|TestRunTurnStreamBBCode|TestRunTurnResponsesStreamBBCode'`
Expected: FAIL (old wiring still pre-translates / constructs `bb` in renderer mode)

- [ ] **Step 3: Rewire**

```go
func (cr *chatRunner) sendRendered(rawText string) {
	pasteSrc := rawText
	text := rawText
	bbOpts := markdowntoirc.BBRenderOptions{BBCode: bbcodeEnabled(cr.cfg), Warn: bbcodeWarnFn(cr.logger)}
	if bbcodeEnabled(cr.cfg) {
		pasteSrc = markdowntoirc.TranslateBBCode(rawText, bbcodeWarnFn(cr.logger))
	}
	if cr.cfg.RenderMarkdown {
		text = markdowntoirc.MarkdownToIRCOpts(bbOpts, text)
	} else if bbcodeEnabled(cr.cfg) {
		text = pasteSrc
	}
	cr.sendWithPastebin(text, pasteSrc)
}
```

`handleToolCallResponse`: same pattern for `t`. `completion()`: `text = markdowntoirc.TranslateBBCode(text, bbcodeWarnFn(logger))` (already from Task 1 — verify only). Both stream sites:

```go
	if cr.cfg.RenderMarkdown && !cr.ephemeral {
		sOut.renderer = markdowntoirc.NewStreamingRendererOpts(markdowntoirc.BBRenderOptions{
			BBCode: bbcodeEnabled(cr.cfg), Warn: bbcodeWarnFn(cr.logger),
		})
	} else if bbcodeEnabled(cr.cfg) && !cr.ephemeral {
		// Plain mode: no markdown structure exists, so the byte translator
		// remains the right tool (holds partial tags across deltas).
		sOut.bb = markdowntoirc.NewBBCodeStream(bbcodeWarnFn(cr.logger))
	}
```

Update the `streamOutput` struct comment (`bb` = plain-mode only). Delete root `bbcode.go`; move `bbcodeEnabled`/`bbcodeWarnFn` into `aiCmds.go`.

- [ ] **Step 4: Rework remaining root tests**

`TestBBCodeNoCodesOnlyLine`: drive `[color=red]a\n[/color]b` through `MarkdownToIRCOpts` + `wrapForIRC`, assert no codes-only line. `TestBBCodeTagOnlyLineNoCodes`: plain-mode translator variant stays (Task 1) + add renderer-mode equivalent. Ephemeral/no-rendermatrix variants of `TestRunTurn*BBCode` keep asserting the same user-visible bytes.

- [ ] **Step 5: Full verification + commit**

Run: `go test ./... && go fmt ./... && go vet ./...`
Commit: `feat: bbcode-aware markdown rendering at every emission hook`

### Task 7: Delete the bbcode_styles knob

**Files:**
- Modify: `config.go` (AIConfig + Service `BBCodeStyles` fields, `ApplyDefaults` cascade, `sanitizeAIConfig`), wherever `bbcodeStylesEnabled` remains referenced (grep)
- Modify: `config/chats.toml`, `config/services.toml` (reference entries + example blocks)
- Modify: `config/templatevars.toml` (delete `BBCodeStylesFormatting`) + its loader if name-enumerated (grep `BBCodeStylesFormatting`)
- Modify: `bbcode_test.go`, `config_test.go` (delete `TestBBCodeStylesTemplateVars`, `TestAIConfigApplyDefaults_BBCodeStyles`, `TestLoadConfigDirBBCodeStyles`; adjust `TestAIConfigApplyDefaults_BBCode`, `TestLoadConfigDirBBCode`, `TestBBCodeInstructionNamesCovered`)

- [ ] **Step 1: Failing tests for the knob's absence**

```go
func TestLoadConfigDirBBCodeStylesIgnored(t *testing.T) {
	dir := createTestConfigDir(t, `
[chats.stylescmd]
regex = "styles"
service = "test"
bbcode = true
bbcode_styles = true
`)
	cfg, err := loadConfigDir(dir)
	require.NoError(t, err) // unknown key: loads fine
	// And the command renders [b] verbatim (toggles are gone).
}
```

- [ ] **Step 2: Run — verify it fails** (field still exists / loads with semantics)
Run: `go test . -run TestLoadConfigDirBBCodeStylesIgnored`

- [ ] **Step 3: Delete the knob everywhere** (grep `BBCodeStyles|bbcode_styles|bbcodeStylesEnabled` and remove: struct fields, TOML tags, ApplyDefaults lines, resolver, templatevar entry + loader refs, doc reference lines + example blocks in chats.toml/services.toml — keep the files' reference sections otherwise untouched per the config docs convention)

- [ ] **Step 4: Full verification + commit**

Run: `go test ./... && go fmt ./... && go vet ./...`
Commit: `refactor: delete bbcode_styles knob (toggles dropped)`

### Task 8: sesssite — shared parser, HTML spans, deletions

**Files:**
- Create: `sesssite/renderer/bbcode.go` (HTML node renderer + balance transformer + second goldmark instance)
- Modify: `sesssite/renderer/markdown.go` (two instances, `RenderOpts`)
- Modify: `sesssite/render.go` (`buildMessageView`/`buildMessageViews`: drop `TranslateBBCode` pre-pass; assistant + reasoning content route to the bbcode instance under `opts.BBCode`)
- Modify: `sesssite/config.go` (delete `BBCodeStyles` from `RenderConfig`/`viewOpts`)
- Delete: `sesssite/bbcode.go` (the verbatim port), its call sites, `sesssite/bbcode_site_test.go` styles cases
- Modify: `sesssite/renderer/irc.go` (delete the code-span IRC-stripping pass — its comment names translator fallout as its reason)

**Interfaces:**
- Consumes: `markdowntoirc.BBCodeExtension`, `markdowntoirc.BBCodeColorOpen/Close`, `markdowntoirc.KindBBCodeColorOpen/Close` (Task 2) — `import markdowntoirc "github.com/knivey/dave/MarkdownToIRC"`
- Produces: `renderer.RenderOpts(source string, bbcode bool) (string, error)` (plain `Render` stays for user rows)

- [ ] **Step 1: Failing site tests**

```go
func TestBBCodeAssistantSpans(t *testing.T) {
	html, err := renderer.RenderOpts("[color=red]x[/color]", true)
	require.NoError(t, err)
	assert.Contains(t, html, `class="irc-fg-4"`)
}

func TestBBCodeLiteralInCode(t *testing.T) {
	html, err := renderer.RenderOpts("```\n[color=red]x[/color]\n```", true)
	require.NoError(t, err)
	assert.Contains(t, html, "[color=red]x[/color]") // no span, no codes
}

func TestBBCodeInvalidTransparent(t *testing.T) {
	html, err := renderer.RenderOpts("[color=bogus]x[/color]", true)
	require.NoError(t, err)
	assert.NotContains(t, html, "irc-fg-", "invalid color emits no span")
}

func TestBBCodeUnbalancedAutoCloses(t *testing.T) {
	html, err := renderer.RenderOpts("[color=red]x", true)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(html, "<span"), strings.Count(html, "</span>"))
}

// User rows stay literal even with bbcode on (role gating, not parser gating).
func TestBBCodeUserRowsStillLiteral(t *testing.T) { /* via buildMessageView: Role user + opts.BBCode → no spans */ }
```

- [ ] **Step 2: Run — verify failure** (`RenderOpts` undefined)

- [ ] **Step 3: Implement**

`sesssite/renderer/bbcode.go`: build a second goldmark instance in `init()` (or a lazy sync.Once) = the existing extensions + `&markdowntoirc.BBCodeExtension{}` + a NodeRenderer registered for the two kinds (open: if `Code != ""` emit `<span class="irc-fg-N">` with N = `strconv.Atoi(Code)`; close: `</span>`) + an AST transformer (priority 999, next to the IRC transformer) that appends `&markdowntoirc.BBCodeColorClose{}` nodes to balance unmatched opens (append after the document's last inline content — `AppendChild` on the last block). `RenderOpts(source, bbcode)` picks the instance. Role gating stays at the `buildMessageView` layer (assistant + reasoning only). Delete the port + stripping pass; delete `BBCodeStyles` from config/viewOpts (its tests).

- [ ] **Step 4: Full site verification**

Run: `go test ./sesssite/... -v`
Expected: PASS including `TestRenderConfigBBCodeDefaults` (adjusted: no styles field), the CSP/security header tests, and the IRC-code span tests (raw model codes still render).

- [ ] **Step 5: Full verification + commit**

Run: `go test ./... && go fmt ./... && go vet ./...`
Commit: `feat(sesssite): shared bbcode parser + html spans; delete translator port`

### Task 9: AGENTS.md + spec close-out

**Files:**
- Modify: `AGENTS.md` — rewrite the **BBCode→IRC output translation** bullet to the new architecture (parser extension, colors-beat-links priority 150, render-state/writer choke point, fragment streaming + state carry, hold machinery deleted, toggles/styles knob gone, paste one-shot, sesssite shared parser), and touch the **Streaming markdown** bullet (fragment rendering replaces whole-prefix re-render; `HasPrefix`/broken fallback deleted; oracles unchanged) and the sesssite bullet (BBCode via shared parser, port deleted)
- Modify: `docs/superpowers/specs/2026-11-13-bbcode-in-markdown-design.md` — Status → implemented

- [ ] **Step 1: Update AGENTS.md** per the house style (dense, test-name-enumerating, design-rationale-preserving — carry over the surviving design comments' content: why colors beat links via priority, why fragments, why the hold machinery vanished)

- [ ] **Step 2: Final full verification**

Run: `go build -o /tmp/opencode/dave-check . && go test ./... && go fmt ./... && go vet ./...`
Expected: build + all tests green

- [ ] **Step 3: Commit**

Commit: `docs: rewrite bbcode architecture notes for parser-level bbcode`

---

## Self-Review (done at plan time)

- **Spec coverage:** §1 approach → Tasks 2–3; §2 core semantics + divergences → Task 3 (incl. all three pinned divergences); §3 toggle deletion → Tasks 1+7; §4 streaming (fragments, carry, hold deletion, oracle, flush) → Task 4 (+5 fuzz); §5 wiring/paste/completion/plain-mode → Task 6; §6 sesssite → Task 8; §7 testing → every task + 5 (fuzz) + 9; config docs → Task 7; AGENTS.md → Task 9. Gap check: spec's "paste tags-in-code-blocks still translated" quirk is pinned by Task 6's paste-source subtest ✓.
- **Placeholders:** none — every step carries code or an exact file/line instruction; the two `...` in Task 3 Step 4's case sketch are resolved by the NOTE paragraph that follows it.
- **Type consistency:** `TranslateBBCode(text, warn)` / `NewBBCodeStream(warn)` (T1) match T6 usage; `BBRenderOptions{BBCode, Warn}` + `MarkdownToIRCOpts(opts, response)` + `MarkdownToIRCStreamOpts` + state injection (T3) match T4/T6; `NewStreamingRendererOpts(opts)` (T4) matches T5/T6; node kinds/types + `BBCodeExtension{Warn}` (T2) match T8. `renderFragment` referenced only within T4.
- **Review Focus:** all five lines have named tests in Tasks 3–4.
