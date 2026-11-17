package markdowntoirc

// BBCode markdown-parser half (Nov 2026; spec:
// docs/superpowers/specs/2026-11-13-bbcode-in-markdown-design.md):
// goldmark AST nodes + the '['-triggered inline parser + the extension
// that registers it. Color-only, like the byte translator (bbcode.go).
//
// DESIGN NOTE — why an inline parser instead of a parse-then-transform AST
// pass: the colors-win-over-links owner ruling is NATIVE here. Goldmark's
// link parser also triggers on '['; registered at a lower priority VALUE
// (150 vs the link parser's 200) our parser runs FIRST on the shared byte
// and either consumes it (a color tag — the link parser never sees it) or
// declines (nil), handing the '[' back for link/text jurisdiction. Under
// transform-after-parse the link node would already exist and we would
// have to intercept Links with tag-shaped labels and split them: more
// rules, more corners.
//
// What the parser gets for free (verified by probe, spec §1): fenced and
// indented code content is never inline-parsed, inline-code content is
// consumed raw by the backtick parser, HTML-block content is not
// inline-parsed, and math spans are consumed by the math parsers first —
// tags stay literal in all four. Rendering lives elsewhere: the IRC
// Renderer's node cases (mathExtension precedent) and, for sesssite, the
// site's own HTML node renderer.

import (
	"strconv"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var (
	// KindBBCodeColorOpen is the AST node kind for [color=X] tags.
	KindBBCodeColorOpen = ast.NewNodeKind("BBCodeColorOpen")
	// KindBBCodeColorClose is the AST node kind for [/color] (and
	// [/color=X]) tags.
	KindBBCodeColorClose = ast.NewNodeKind("BBCodeColorClose")
)

// BBCodeColorOpen is a [color=X] tag. Code is the RESOLVED two-digit IRC
// code (parse-time resolution — both renderers read it without
// re-resolving); "" means the value was invalid: the span is a TRANSPARENT
// placeholder (spec §2: today's translator pushes "" onto the color stack
// so the eventual [/color] pairs with IT instead of popping an innocent
// enclosing color — dropping the node would break that pairing). Raw is
// the original tag bytes for warns.
type BBCodeColorOpen struct {
	ast.BaseInline
	Code string
	Raw  string
}

// BBCodeColorClose is a [/color] or [/color=X] tag (a close-with-value is
// a plain close: close-intent is unambiguous). Raw is the original tag
// bytes for warns.
type BBCodeColorClose struct {
	ast.BaseInline
	Raw string
}

func (n *BBCodeColorOpen) Kind() ast.NodeKind  { return KindBBCodeColorOpen }
func (n *BBCodeColorClose) Kind() ast.NodeKind { return KindBBCodeColorClose }
func (n *BBCodeColorOpen) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}
func (n *BBCodeColorClose) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

// bbcodeParser triggers on '[' at priority 150 — BELOW the numeric value
// of goldmark's link parser (200), i.e. it runs FIRST (lower = higher
// priority) and its decline (nil) hands the '[' back for link/text
// jurisdiction. That ordering IS the "colors win over links" ruling:
// [color=red](url) is a color tag followed by literal "(url)", while
// [b](url) (unknown name) declines and parses as a markdown link.
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
		// Same text as the byte translator (Task 1) — pinned by Global
		// Constraints. strconv.Quote is fmt's %q for these ASCII tags.
		p.warn("bbcode: invalid tag " + strconv.Quote(raw) + ": unknown color, span tracked transparent")
	}
	block.Advance(tag.consumed)
	return &BBCodeColorOpen{Code: code, Raw: raw}
}

// BBCodeExtension registers the parser only — rendering lives in the IRC
// Renderer's node cases (mathExtension precedent) and, for sesssite, in
// the site's own HTML node renderer. Warn receives one message per
// invalid-color open (nil is silent); it is per-render construction — the
// goldmark instance is already built per call.
type BBCodeExtension struct{ Warn func(string) }

func (e *BBCodeExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(parser.WithInlineParsers(
		util.Prioritized(&bbcodeParser{warn: e.Warn}, 150),
	))
}
