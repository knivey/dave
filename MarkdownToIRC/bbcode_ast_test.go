package markdowntoirc

// Tests for the BBCode goldmark inline parser + AST nodes (bbcode_ast.go),
// the markdown-aware half of the 2026-11-13 bbcode redesign (spec:
// docs/superpowers/specs/2026-11-13-bbcode-in-markdown-design.md).
//
// Structural pins:
//   - colors win over links: [color=red](url) is a color-open node plus
//     literal "(url)"; every OTHER tag-shaped label ([b](url), [1](url),
//     [label](url)) declines here and stays a markdown link
//   - unknown names, non-tag shapes, and newline-crossing candidates
//     decline (the '[' stays text — prose/markdown jurisdiction, verbatim,
//     no warn)
//   - known tags with INVALID values ([color], [color=], [color=16],
//     [color=bogus]) are ACCEPTED opens carrying Code == "" — the
//     transparent placeholder the render state pairs with its close (spec
//     §2, preserving the byte translator's span-tracking semantics) — and
//     warn once at the open with the byte translator's exact text
//   - the parser never fires inside inline code spans: the backtick parser
//     consumes the span's content first (structure, not parser logic)

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// parseDoc runs text through a goldmark instance with the BBCode extension
// and returns the document node for structural assertions.
func parseDoc(t *testing.T, src string) ([]byte, ast.Node) {
	t.Helper()
	md := goldmark.New(goldmark.WithExtensions(&BBCodeExtension{}))
	doc := md.Parser().Parse(text.NewReader([]byte(src)))
	return []byte(src), doc
}

// findKind returns the first node (depth-first) with the given kind.
func findKind(n ast.Node, kind ast.NodeKind) ast.Node {
	if n == nil {
		return nil
	}
	if n.Kind() == kind {
		return n
	}
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if found := findKind(c, kind); found != nil {
			return found
		}
	}
	return nil
}

func containsKind(n ast.Node, kind ast.NodeKind) bool {
	return findKind(n, kind) != nil
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
	// NOTE: the invalid-VALUE color shapes ([color], [color=], [color=16])
	// are NOT declines — spec §2 pins them as accepted transparent
	// placeholder opens (TestBBCodeParserInvalidOpenIsTransparent below).
	// Everything here must leave no color node at all.
	for _, in := range []string{
		"[b]x", "[table]", "[1]", "a[i] + b[j]", "[]", "[=x]",
		"[color=red x", "[col\nor=red]x", // newline in tag shape: never a tag
	} {
		_, doc := parseDoc(t, in)
		assert.False(t, containsKind(doc, KindBBCodeColorOpen), in)
		assert.False(t, containsKind(doc, KindBBCodeColorClose), in)
	}
}

// TestBBCodeParserInvalidOpenIsTransparent pins the invalid-color contract:
// a KNOWN tag whose value fails to resolve is still an open — Code == ""
// marks the transparent placeholder span (spec §2: today's translator
// tracks the span so its [/color] pairs with IT, never an innocent
// enclosing color) — and the open warns once with the byte translator's
// exact text (strconv.Quote == fmt's %q for these ASCII tags).
func TestBBCodeParserInvalidOpenIsTransparent(t *testing.T) {
	for _, tc := range []struct{ in, raw string }{
		{"[color]", "[color]"},
		{"[color=]", "[color=]"},
		{"[color=16]", "[color=16]"},
		{"[color=bogus]", "[color=bogus]"},
	} {
		var warns []string
		md := goldmark.New(goldmark.WithExtensions(&BBCodeExtension{
			Warn: func(msg string) { warns = append(warns, msg) },
		}))
		doc := md.Parser().Parse(text.NewReader([]byte(tc.in)))
		node := findKind(doc, KindBBCodeColorOpen)
		require.NotNil(t, node, tc.in)
		open, ok := node.(*BBCodeColorOpen)
		require.True(t, ok, tc.in)
		assert.Equal(t, "", open.Code, tc.in)
		assert.Equal(t, tc.raw, open.Raw, tc.in)
		require.Len(t, warns, 1, tc.in)
		assert.Equal(t, "bbcode: invalid tag "+strconv.Quote(tc.raw)+
			": unknown color, span tracked transparent", warns[0], tc.in)
	}
}

// TestBBCodeParserNodeFields pins parse-time resolution and the Raw
// carriage: Code is the final two-digit IRC code (renderers never
// re-resolve), Raw is the original tag bytes both node kinds carry for
// warns.
func TestBBCodeParserNodeFields(t *testing.T) {
	for _, tc := range []struct {
		in       string
		openCode string
		openRaw  string
		closeRaw string
	}{
		{"[color=red]y[/color]", "04", "[color=red]", "[/color]"},
		{"[COLOR=light green]y[/COLOR=red]", "09", "[COLOR=light green]", "[/COLOR=red]"},
		{"[color=12]y[/color=]", "12", "[color=12]", "[/color=]"},
	} {
		_, doc := parseDoc(t, tc.in)
		open, ok := findKind(doc, KindBBCodeColorOpen).(*BBCodeColorOpen)
		require.True(t, ok, tc.in)
		assert.Equal(t, tc.openCode, open.Code, tc.in)
		assert.Equal(t, tc.openRaw, open.Raw, tc.in)
		close, ok := findKind(doc, KindBBCodeColorClose).(*BBCodeColorClose)
		require.True(t, ok, tc.in)
		assert.Equal(t, tc.closeRaw, close.Raw, tc.in)
	}
}

// Colors win over links: the tag is consumed; (url) is plain text.
func TestBBCodeParserColorsBeatLinks(t *testing.T) {
	_, doc := parseDoc(t, "[color=red](http://x.example)")
	require.True(t, containsKind(doc, KindBBCodeColorOpen))
	assert.False(t, containsKind(doc, ast.KindLink)) // no markdown link node
}

// Unknown tag-shaped labels fall through to the link parser.
func TestBBCodeParserUnknownLabelStaysLink(t *testing.T) {
	_, doc := parseDoc(t, "[b](http://x.example)")
	assert.True(t, containsKind(doc, ast.KindLink))
}

// The parser must not fire inside inline code spans: the backtick parser
// consumes the span's content first, so the '[' is raw code text, never a
// trigger byte. The codespan presence assertion is the positive control
// (the test would otherwise pass vacuously).
func TestBBCodeParserNotInsideCodeSpan(t *testing.T) {
	_, doc := parseDoc(t, "`[color=red]`")
	require.True(t, containsKind(doc, ast.KindCodeSpan))
	assert.False(t, containsKind(doc, KindBBCodeColorOpen))
	assert.False(t, containsKind(doc, KindBBCodeColorClose))
}
