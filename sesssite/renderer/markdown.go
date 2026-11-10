// Package renderer converts stored message markdown to HTML for the
// session-history site. Ported from markdown-parsebin (dave-web), MIT,
// (c) dave project — same rendering stack the pastebin used: GFM,
// Chroma/Dracula highlighting, linkify, and the IRC control-code
// transformer below (IRC clients embed \x02/\x03/… codes in user text;
// the transformer maps them to styled spans).
package renderer

import (
	"bytes"
	"fmt"

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
				highlighting.WithFormatOptions(),
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
