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
package renderer

import (
	"bytes"
	"fmt"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

var md goldmark.Markdown

func init() {
	style := styles.Get("dracula")
	if style == nil {
		style = styles.Fallback
	}

	md = goldmark.New(
		goldmark.WithExtensions(
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
			IRCExtension,
		),
		goldmark.WithRendererOptions(
			html.WithHardWraps(),
			html.WithXHTML(),
		),
	)
}

func Render(source string) (string, error) {
	var buf bytes.Buffer
	if err := md.Convert([]byte(source), &buf); err != nil {
		return "", fmt.Errorf("render markdown: %w", err)
	}
	return buf.String(), nil
}
