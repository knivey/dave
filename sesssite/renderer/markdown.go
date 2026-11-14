// Package renderer converts stored message markdown to HTML for the
// session-history site. Ported from markdown-parsebin (dave-web), MIT,
// (c) dave project — same rendering stack the pastebin used: GFM,
// Chroma/Dracula highlighting, linkify, and the IRC control-code
// transformer below (IRC clients embed \x02/\x03/… codes in user text;
// the transformer maps them to styled spans).
//
// Deliberate divergence from the parsebin original (controller ruling,
// Nov 2026): Chroma emits CLASSES, not inline styles — the site's CSP
// blocks inline styles, and the Dracula palette ships as a generated
// stylesheet (static/chroma.css) instead. See the WithClasses note in
// init().
//
// Two instances live here: the plain one (Render — user rows, compaction
// summaries, bbcode-off assistant rows) and the bbcode-aware one
// (RenderOpts with bbcode=true — assistant rows under [render] bbcode;
// bbcode.go builds it from the SAME option set plus the shared
// markdowntoirc parser).
package renderer

import (
	"bytes"
	"fmt"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

var md goldmark.Markdown

func init() {
	md = newMarkdown()
}

// newMarkdown builds a goldmark instance from the base option set
// (baseExtensions + baseRendererOptions) plus any extra extensions —
// the bbcode instance (bbcode.go) passes exactly the shared parser and
// its HTML-side pair, so the two pipelines differ in nothing else.
func newMarkdown(extra ...goldmark.Extender) goldmark.Markdown {
	exts := append(baseExtensions(), extra...)
	opts := append(
		[]goldmark.Option{goldmark.WithExtensions(exts...)},
		baseRendererOptions()...,
	)
	return goldmark.New(opts...)
}

// baseExtensions is the plain instance's extension set, factored out so
// the bbcode instance (bbcode.go) reuses it verbatim — the two
// pipelines may differ ONLY in the bbcode pair.
func baseExtensions() []goldmark.Extender {
	return []goldmark.Extender{
		extension.GFM,
		extension.NewLinkify(
			extension.WithLinkifyAllowedProtocols([][]byte{
				[]byte("http:"),
				[]byte("https:"),
			}),
		),
		highlighting.NewHighlighting(
			highlighting.WithStyle("dracula"),
			// DIVERGENCE from the parsebin original (controller
			// ruling, Nov 2026): class-based Chroma output instead
			// of inline style= attributes. The site's strict CSP
			// (style-src 'self') blocks inline styles, so the
			// parsebin inline form shipped uncolored code — and the
			// spec promises Chroma highlighting. The Dracula colors
			// ship as a generated stylesheet at static/chroma.css.
			highlighting.WithFormatOptions(chromahtml.WithClasses(true)),
		),
		// IRCExtension belongs to BOTH instances: raw IRC control codes
		// (\x02/\x03/…) in stored text render as styled spans whichever
		// pipeline processes the row.
		IRCExtension,
	}
}

// baseRendererOptions is the plain instance's renderer option set (see
// baseExtensions) — hard-wrapped, XHTML output.
func baseRendererOptions() []goldmark.Option {
	return []goldmark.Option{
		goldmark.WithRendererOptions(
			html.WithHardWraps(),
			html.WithXHTML(),
		),
	}
}

func Render(source string) (string, error) {
	return convert(md, source)
}

// RenderOpts renders with the instance chosen by bbcode: true selects
// the bbcode-aware instance (color tags parse into styled spans);
// false is exactly Render. Role gating (assistant + reasoning only)
// lives in the site's view layer, not here.
func RenderOpts(source string, bbcode bool) (string, error) {
	if bbcode {
		return convert(mdBBCode, source)
	}
	return convert(md, source)
}

func convert(m goldmark.Markdown, source string) (string, error) {
	var buf bytes.Buffer
	if err := m.Convert([]byte(source), &buf); err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	return buf.String(), nil
}
