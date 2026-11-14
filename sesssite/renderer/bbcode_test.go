package renderer

// Tests for the bbcode-aware render path (bbcode.go): the shared
// markdowntoirc parser feeds color-tag nodes, and this package renders
// them as the same CSP-safe irc-fg-N spans the raw-code IRC transformer
// uses. Spec: docs/superpowers/specs/2026-11-13-bbcode-in-markdown-design.md
// §6 — one parser, two renderers; the site's translator port is gone.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBBCodeAssistantSpans(t *testing.T) {
	html, err := RenderOpts("[color=red]x[/color]", true)
	require.NoError(t, err)
	assert.Contains(t, html, `class="irc-fg-4"`)
	assert.Contains(t, html, ">x</span>")
	assert.NotContains(t, html, "[color=red]", "the tag itself must not render")
}

func TestBBCodeLiteralInCode(t *testing.T) {
	html, err := RenderOpts("```\n[color=red]x[/color]\n```", true)
	require.NoError(t, err)
	assert.Contains(t, html, "[color=red]x[/color]") // no span, no codes
	assert.NotContains(t, html, "irc-fg-", "tags inside fences are never parsed")
}

func TestBBCodeInvalidTransparent(t *testing.T) {
	html, err := RenderOpts("[color=bogus]x[/color]", true)
	require.NoError(t, err)
	assert.NotContains(t, html, "irc-fg-", "invalid color emits no span")
	assert.Contains(t, html, ">x<", "content survives")
}

func TestBBCodeUnbalancedAutoCloses(t *testing.T) {
	html, err := RenderOpts("[color=red]x", true)
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(html, "<span"), strings.Count(html, "</span>"))
}

// TestBBCodeColorsBeatLinks pins the owner ruling at the SITE level:
// the shared parser runs before the link parser, so [color=red](url) is
// a color open followed by literal "(url)" — never an anchor.
func TestBBCodeColorsBeatLinks(t *testing.T) {
	html, err := RenderOpts("[color=red](url)", true)
	require.NoError(t, err)
	assert.Contains(t, html, `class="irc-fg-4"`)
	assert.Contains(t, html, "(url)")
	assert.NotContains(t, html, "<a href", "colors win over links")
}

// TestBBCodeNestedStackShadow: CSS nesting IS the color stack — a real
// inner color shadows the enclosing span exactly as the IRC stack does,
// with no effective-color computation at render time.
func TestBBCodeNestedStackShadow(t *testing.T) {
	html, err := RenderOpts("[color=red]a[color=blue]b[/color]c[/color]", true)
	require.NoError(t, err)
	assert.Contains(t, html, `<span class="irc-fg-4">a<span class="irc-fg-2">b</span>c</span>`)
}

// TestBBCodeNestedInvalidStaysEnclosed pins IRC placeholder semantics:
// [color=bogus] is a TRANSPARENT placeholder — its [/color] pairs with
// IT and emits nothing, so the enclosing real color keeps painting the
// text after the inner span ("c" stays red, exactly like IRC).
func TestBBCodeNestedInvalidStaysEnclosed(t *testing.T) {
	html, err := RenderOpts("[color=red]a[color=bogus]b[/color]c[/color]", true)
	require.NoError(t, err)
	assert.Contains(t, html, `<span class="irc-fg-4">abc</span>`, "the placeholder's close must not end the enclosing color")
	assert.Equal(t, 1, strings.Count(html, "<span"), strings.Count(html, "</span>"))
}

// TestBBCodeInvalidPairEmitsNothing: a balanced invalid-color pair
// leaves no markup at all — neither side renders (IRC emits nothing for
// either; the site must not leak a stray </span>).
func TestBBCodeInvalidPairEmitsNothing(t *testing.T) {
	html, err := RenderOpts("[color=bogus]x[/color]", true)
	require.NoError(t, err)
	assert.Contains(t, html, ">x<", "content survives")
	assert.NotContains(t, html, "<span")
	assert.NotContains(t, html, "</span>")
}

// TestBBCodeStrayCloseStripped: a close with no open is stripped — the
// IRC renderer drops it with a WARN; the site has no warn channel, so
// silently.
func TestBBCodeStrayCloseStripped(t *testing.T) {
	html, err := RenderOpts("a[/color]b", true)
	require.NoError(t, err)
	assert.NotContains(t, html, "</span>")
	assert.Contains(t, html, ">ab<")
}

// TestBBCodePlainInstanceKeepsTagsLiteral: the plain instance (bbcode
// off, user rows) never registers the parser — tags stay literal text.
func TestBBCodePlainInstanceKeepsTagsLiteral(t *testing.T) {
	html, err := RenderOpts("[color=red]x[/color]", false)
	require.NoError(t, err)
	assert.Contains(t, html, "[color=red]x[/color]")
	assert.NotContains(t, html, "irc-fg-")
}
