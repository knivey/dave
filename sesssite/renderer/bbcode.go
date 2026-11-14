// bbcode.go — the bbcode-aware goldmark instance (Nov 2026; spec:
// docs/superpowers/specs/2026-11-13-bbcode-in-markdown-design.md §6).
//
// The site does NOT carry its own BBCode translator anymore: the parser
// is SHARED with dave's IRC emission (markdowntoirc.BBCodeExtension
// turns [color=X] tags into AST nodes — one parser, two renderers), so
// the verbatim-port drift risk dies at the source. This file only adds
// what the HTML side needs on top of the plain instance:
//
//   - a NodeRenderer mapping the two node kinds to the SAME CSP-safe
//     classes the raw-code IRC transformer uses (irc-fg-N via
//     ircFgClass; the site's style-src 'self' CSP forbids inline
//     styles, and the stylesheet already defines irc-fg-0..98). CSS
//     nesting IS the color stack: a real inner color shadows the
//     enclosing span exactly as the IRC stack does, with no
//     effective-color computation at render time. An INVALID color
//     (Code == "") emits nothing — transparent; its content falls to
//     the enclosing span, the same semantics for free.
//
//   - a balance transformer appending BBCodeColorClose nodes for
//     unmatched opens (HTML validity is the sibling of the IRC
//     auto-close: an unclosed span inside a transcript page would
//     color everything after it in the DOM). Counting is
//     document-wide; the closes append to the LAST block that directly
//     contains inline children (AppendChild on that block, after its
//     last inline content). Blocks without inline children — fenced
//     code, HTML blocks — never receive them: their renderers ignore
//     children entirely, so a close appended there would be dropped
//     bytes. Collect-then-mutate: appends happen only after the walk
//     (mid-walk tree surgery breaks goldmark's sibling traversal — see
//     ircTransformer).
//
// Tags inside fenced code, inline code spans, HTML blocks, and math
// stay literal for free: those regions are never inline-parsed, so the
// shared parser never sees them (verified by probe in
// MarkdownToIRC/bbcode_ast.go).

package renderer

import (
	"strconv"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	markdowntoirc "github.com/knivey/dave/MarkdownToIRC"
)

var mdBBCode goldmark.Markdown

func init() {
	// The ONLY differences from the plain instance are the shared
	// BBCode parser (markdowntoirc) and this file's HTML-side pair —
	// same GFM + linkify + class-based highlighting + IRC transformer,
	// same hard-wrap XHTML output.
	mdBBCode = newMarkdown(
		&markdowntoirc.BBCodeExtension{},
		&bbcodeHTMLExtension{},
	)
}

// bbcodeHTMLExtension registers the HTML-side node renderer and the
// balance transformer on the bbcode instance.
type bbcodeHTMLExtension struct{}

func (e *bbcodeHTMLExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithASTTransformers(
			// 999: next to the IRC transformer (order between the two
			// is irrelevant — neither touches the other's nodes).
			util.Prioritized(&bbcodeBalanceTransformer{}, 999),
		),
	)
	m.Renderer().AddOptions(
		renderer.WithNodeRenderers(
			util.Prioritized(&bbcodeHTMLRenderer{}, 100),
		),
	)
}

// bbcodeHTMLRenderer renders the shared parser's node kinds. Goldmark's
// renderer silently DROPS nodes of unregistered kinds (no error —
// renderer/renderer.go's func-table lookup returns nil), so
// registering these is what makes tags render at all; and because the
// PARSER is only registered on this instance, the plain instance never
// produces the nodes in the first place.
type bbcodeHTMLRenderer struct{}

func (r *bbcodeHTMLRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(markdowntoirc.KindBBCodeColorOpen, r.renderColorOpen)
	reg.Register(markdowntoirc.KindBBCodeColorClose, r.renderColorClose)
}

func (r *bbcodeHTMLRenderer) renderColorOpen(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		if open, ok := n.(*markdowntoirc.BBCodeColorOpen); ok && open.Code != "" {
			// Code is exactly "00".."15" (bbColorValue, two-digit by
			// construction); "" is the transparent placeholder — emit
			// nothing and the eventual close still balances it.
			if num, err := strconv.Atoi(open.Code); err == nil {
				w.WriteString(`<span class="`)
				w.WriteString(ircFgClass(num))
				w.WriteString(`">`)
			}
		}
	}
	return ast.WalkContinue, nil
}

func (r *bbcodeHTMLRenderer) renderColorClose(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		w.WriteString("</span>")
	}
	return ast.WalkContinue, nil
}

// bbcodeBalanceTransformer appends BBCodeColorClose nodes until the
// document's open/close counts match.
type bbcodeBalanceTransformer struct{}

func (t *bbcodeBalanceTransformer) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	opens, closes := 0, 0
	var lastInlineBlock ast.Node
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n.Kind() {
		case markdowntoirc.KindBBCodeColorOpen:
			opens++
		case markdowntoirc.KindBBCodeColorClose:
			closes++
		}
		// Track the last BLOCK parent of an inline node: appending the
		// balance closes there puts them after the document's last
		// inline content, in a block whose renderer walks children.
		// Inline nodes whose parent is also inline (text inside a code
		// span; the IRC transformer's String nodes are block-parented)
		// are skipped — a close must never land inside a code span.
		if n.Type()&ast.TypeInline != 0 {
			if p := n.Parent(); p != nil && p.Type()&ast.TypeBlock != 0 {
				lastInlineBlock = p
			}
		}
		return ast.WalkContinue, nil
	})
	if opens <= closes || lastInlineBlock == nil {
		return
	}
	for i := 0; i < opens-closes; i++ {
		lastInlineBlock.AppendChild(lastInlineBlock, &markdowntoirc.BBCodeColorClose{})
	}
}
