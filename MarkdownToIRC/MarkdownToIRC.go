package markdowntoirc

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/doug/termtex"
	"github.com/knivey/dave/MarkdownToIRC/irc"
	"github.com/knivey/dave/MarkdownToIRC/tables"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type Renderer struct {
	listCounter []int
	source      []byte
	streaming   bool

	// bb is the BBCode render state (nil = bbcode off, byte-identical
	// legacy behavior). Set at construction, never inside Render — a
	// one-shot render builds a fresh state per call, while the
	// StreamingRenderer injects ONE long-lived state across fragment
	// renders (BBRenderOptions.state, same package).
	bb     *bbRenderState
	bbWarn func(string)
}

func plainLength(s string) int {
	return utf8.RuneCountInString(irc.StripCodes(s))
}

func extractTableData(table ast.Node, r *Renderer) tables.TableData {
	var rows []tables.TableRow
	headerCount := 0

	for section := table.FirstChild(); section != nil; section = section.NextSibling() {
		isHeader := section.Kind() == extast.KindTableHeader
		var row tables.TableRow
		for cellNode := section.FirstChild(); cellNode != nil; cellNode = cellNode.NextSibling() {
			if cellNode.Kind() != extast.KindTableCell {
				continue
			}
			cell := cellNode.(*extast.TableCell)
			var buf bytes.Buffer
			for c := cell.FirstChild(); c != nil; c = c.NextSibling() {
				r.renderNodeTo(&buf, c)
			}
			text := strings.TrimSpace(strings.ReplaceAll(buf.String(), "\\|", "|"))
			align := tables.AlignLeft
			switch cell.Alignment {
			case extast.AlignRight:
				align = tables.AlignRight
			case extast.AlignCenter:
				align = tables.AlignCenter
			}
			row = append(row, tables.TableCell{Text: text, Align: align})
		}
		if len(row) > 0 {
			rows = append(rows, row)
			if isHeader {
				headerCount++
			}
		}
	}

	return tables.TableData{Rows: rows, HeaderRowCount: headerCount}
}

func (r *Renderer) segmentText(seg text.Segment) string {
	return string(seg.Value(r.source))
}

// unescapeMarkdownText implements the RENDERER's half of goldmark's text
// contract. The parser keeps backslash-escape bytes in the Text segments
// (parser.go only suppresses parser triggers on the escaped byte) and
// leaves unescaping to the renderer — goldmark's html defaultWriter.Write
// does it for HTML; dave's IRC renderer must do the same for IRC. Rule
// (CommonMark): a backslash before ASCII punctuation is CONSUMED, before
// anything else it stays literal; raw contexts (code spans/fences, HTML
// blocks, link labels' nodeText predicate) never pass through here. Prod
// incident 2026-11-21 (session 3399): the model escaped every dollar and
// one BBCode close tag and the raw backslashes rode to the channel
// verbatim. An escaped bracket also never triggers any bracket parser
// (links or BBCode) — the tag renders as literal text, deliberately.
func unescapeMarkdownText(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && util.IsPunct(s[i+1]) {
			continue // drop the backslash, keep the punct char
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (r *Renderer) nodeText(n ast.Node) string {
	var buf bytes.Buffer
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		r.collectText(c, &buf)
	}
	return buf.String()
}

func (r *Renderer) collectText(n ast.Node, buf *bytes.Buffer) {
	switch n := n.(type) {
	case *ast.Text:
		buf.WriteString(r.segmentText(n.Segment))
		if n.HardLineBreak() {
			buf.WriteByte('\n')
		} else if n.SoftLineBreak() {
			buf.WriteByte(' ')
		}
	case *ast.RawHTML:
		for i := 0; i < n.Segments.Len(); i++ {
			buf.WriteString(r.segmentText(n.Segments.At(i)))
		}
	case *ast.String:
		buf.Write(n.Value)
	default:
		if n.Type() == ast.TypeInline || n.Type() == ast.TypeBlock {
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				r.collectText(c, buf)
			}
		}
	}
}

func makeIndents(node ast.Node) string {
	var out string
	var prevWasQuote bool
	for n := node; n != nil; n = n.Parent() {
		switch n.Kind() {
		case ast.KindDocument:
			return out
		case ast.KindList:
			out = "   " + out
			prevWasQuote = false
		case ast.KindBlockquote:
			if prevWasQuote {
				out = "\x0309>" + out
			} else {
				out = "\x0309> " + out
			}
			prevWasQuote = true
		}
	}
	return out
}

func writes(w io.Writer, node ast.Node, text string) {
	indent := makeIndents(node)
	parts := strings.Split(text, "\n")
	for i, part := range parts {
		if i > 0 {
			fmt.Fprint(w, "\n"+indent)
		}
		fmt.Fprint(w, part)
	}
}

func writesWithNegOffset(w io.Writer, node ast.Node, text string, negativeOffset int) {
	indent := makeIndents(node)
	off := max(utf8.RuneCountInString(indent)-negativeOffset, 0)
	trimmed := string([]rune(indent)[:off])
	parts := strings.Split(text, "\n")
	for i, part := range parts {
		if i > 0 {
			fmt.Fprint(w, "\n"+trimmed)
		}
		fmt.Fprint(w, part)
	}
}

func (r *Renderer) Render(w io.Writer, source []byte, n ast.Node) error {
	r.source = source
	if r.streaming {
		r.listCounter = nil
	}
	var buf bytes.Buffer
	// With bbcode active, every TEXT byte of the walk flows through the
	// render state (digit guards, per-line color re-emission); tag nodes
	// write their codes straight into buf via openInto/closeInto with the
	// state's outBuf — code emissions must never be seen by textInto as
	// visible text. No state reset here: one-shot paths construct a
	// fresh state per call and the StreamingRenderer deliberately reuses
	// one across fragments.
	var walkW io.Writer = &buf
	if r.bb != nil {
		r.bb.outBuf = &buf
		walkW = &bbTextWriter{st: r.bb, buf: &buf}
	}
	err := ast.Walk(n, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		return r.renderNode(walkW, node, entering), nil
	})
	if err != nil {
		return err
	}
	out := buf.String()
	out = strings.TrimPrefix(out, "\n")
	w.Write([]byte(out))
	return nil
}

func (r *Renderer) AddOptions(...renderer.Option) {}

func (r *Renderer) renderNode(w io.Writer, node ast.Node, entering bool) ast.WalkStatus {
	switch node.Kind() {
	case ast.KindEmphasis:
		em := node.(*ast.Emphasis)
		if em.Level == 2 {
			writes(w, node, "\x02")
		} else {
			writes(w, node, "\x1D")
		}
	case ast.KindHeading:
		if entering {
			writes(w, node, "\n\x02")
		} else {
			writes(w, node, "\x02")
		}
	case ast.KindParagraph:
		if node.Parent() != nil && node.Parent().Kind() == ast.KindListItem {
			return ast.WalkContinue
		}
		if entering {
			writes(w, node, "\n")
		}
	case ast.KindCodeSpan:
		if entering {
			writes(w, node, fmt.Sprintf("\x030,90%s\x03", r.nodeText(node)))
		}
	case ast.KindFencedCodeBlock:
		if entering {
			fcb := node.(*ast.FencedCodeBlock)
			writes(w, node, "\n")
			lang := string(fcb.Language(r.source))
			var codeBuf bytes.Buffer
			lines := fcb.Lines()
			for i := 0; i < lines.Len(); i++ {
				seg := lines.At(i)
				if i > 0 {
					codeBuf.WriteByte('\n')
				}
				codeBuf.WriteString(strings.TrimRight(r.segmentText(seg), "\n"))
			}
			if r.streaming {
				writePlainCodeBlockFixed(w, node, codeBuf.String())
				return ast.WalkContinue
			}
			lexer := lexers.Get(lang)
			if lexer != nil {
				style := styles.Get("github-dark")
				if style == nil {
					style = styles.Fallback
				}
				var lineBuffer bytes.Buffer
				formatter := IRC16
				codeText := strings.ReplaceAll(codeBuf.String(), "\t", "        ")
				iterator, err := lexer.Tokenise(nil, codeText)
				if err == nil {
					formatter.Format(&lineBuffer, style, iterator)
					writeHighlightedCodeBlock(w, node, lineBuffer.String())
					return ast.WalkContinue
				}
			}
			writePlainCodeBlock(w, node, codeBuf.String())
		}
	case ast.KindCodeBlock:
		if entering {
			writes(w, node, "\n")
			var codeBuf bytes.Buffer
			lines := node.Lines()
			for i := 0; i < lines.Len(); i++ {
				seg := lines.At(i)
				if i > 0 {
					codeBuf.WriteByte('\n')
				}
				codeBuf.WriteString(strings.TrimRight(r.segmentText(seg), "\n"))
			}
			if r.streaming {
				writePlainCodeBlockFixed(w, node, codeBuf.String())
				return ast.WalkContinue
			}
			writePlainCodeBlock(w, node, codeBuf.String())
		}
	case ast.KindList:
		l := node.(*ast.List)
		if entering {
			r.listCounter = append(r.listCounter, l.Start-1)
		} else {
			r.listCounter = r.listCounter[:len(r.listCounter)-1]
		}
	case ast.KindListItem:
		if entering {
			l := node.Parent().(*ast.List)
			var lead string
			if l.IsOrdered() {
				r.listCounter[len(r.listCounter)-1]++
				lead = fmt.Sprintf("%d%c ", r.listCounter[len(r.listCounter)-1], l.Marker)
			} else {
				lead = " \u2022 "
			}
			writesWithNegOffset(w, node, "\n"+lead, utf8.RuneCountInString(lead))
		}
	case ast.KindLink:
		link := node.(*ast.Link)
		// DESIGN NOTE — IRC clickability (production report, Nov 2026:
		// a URL wrapped in parens was unclickable for many users).
		// Parens are LEGAL URI characters (RFC 3986 sub-delims), so IRC
		// autolinkers swallow a trailing ) into the URL and the link
		// 404s. The URL is therefore delimited by spaces/end-of-line,
		// never parens: "text: url". Labels that carry no text of their
		// own — self-referential links ([url](url), a constant model
		// emission) and empty/whitespace labels ([](url), or a label
		// that is itself an autolink — autolinks contribute no walked
		// text, see KindAutoLink below) — collapse to the bare URL;
		// the generic form would render "url: url" or a dangling ": ".
		//
		// The destination is UNESCAPED here (2026-11-21): goldmark's
		// html renderLink/renderImage run util.URLEscape(dest) whose
		// first step is UnescapePunctuations — the href never carries
		// the backslash, and neither may the IRC URL (a literal \_ or
		// \) inside a pasted URL breaks the click). The collapse
		// predicate compares like-for-like post-unescape (label vs
		// destination), so a visually-self link collapses even when
		// the label's backslash differs.
		dest := unescapeMarkdownText(string(link.Destination))
		if trimmed := strings.TrimSpace(r.nodeText(node)); trimmed == "" || unescapeMarkdownText(trimmed) == dest {
			if entering {
				writes(w, node, dest)
				return ast.WalkSkipChildren
			}
			return ast.WalkContinue
		}
		if !entering {
			writes(w, node, fmt.Sprintf(": %s", dest))
		}
	case ast.KindAutoLink:
		// DESIGN NOTE — goldmark stores an autolink's URL in an
		// unexported value field, NOT as walked children (ast/inline.go
		// NewAutoLink), so the default walk below finds nothing and the
		// URL would silently VANISH from the output (code-review finding
		// on the clickability fix: "see <url> now" rendered "see  now"
		// — data loss, worse than unclickable). URL(source) returns the
		// full address for both URL autolinks (<https://…>) and email
		// autolinks (<user@host>). Bare URL, same paren-free contract.
		if entering {
			writes(w, node, string(node.(*ast.AutoLink).URL(r.source)))
		}
	case ast.KindImage:
		img := node.(*ast.Image)
		if entering {
			writes(w, node, "[image: ")
		} else {
			// Destination unescaped like KindLink's (see the DESIGN
			// NOTE there): the IRC URL must match the href's bytes.
			writes(w, node, fmt.Sprintf("] %s", unescapeMarkdownText(string(img.Destination))))
		}
	case ast.KindText:
		t := node.(*ast.Text)
		if !entering || t.IsRaw() {
			return ast.WalkContinue
		}
		txt := unescapeMarkdownText(r.segmentText(t.Segment))
		if t.HardLineBreak() {
			writes(w, node, strings.TrimRight(txt, "\n")+"\n")
		} else if t.SoftLineBreak() {
			writes(w, node, strings.TrimRight(txt, "\n")+" ")
		} else {
			writes(w, node, strings.TrimRight(txt, "\n"))
		}
	case ast.KindRawHTML:
		if entering {
			html := node.(*ast.RawHTML)
			for i := 0; i < html.Segments.Len(); i++ {
				seg := html.Segments.At(i)
				tag := strings.ToLower(string(seg.Value(r.source)))
				if tag == "<br>" || tag == "<br/>" || tag == "<br />" {
					writes(w, node, "\n")
				} else {
					writes(w, node, string(seg.Value(r.source)))
				}
			}
		}
	case extast.KindTable:
		if entering {
			// Tables render identically in both modes: a GFM table can't
			// span blank lines, so the streaming renderer's settle points
			// (blank-line boundaries, top-level fence closers) always fall
			// OUTSIDE a table — it is held as a unit and every row is
			// present when RenderTable computes column widths, so the
			// prefix-monotonicity invariant holds (pinned by the latency
			// tests + the fuzz survey's table generator).
			data := extractTableData(node, r)
			formatted := tables.RenderTable(data)
			writes(w, node, formatted)
			return ast.WalkSkipChildren
		}
	case extast.KindTableHeader, extast.KindTableRow:
	case extast.KindTableCell:
	case KindMathInline:
		if entering {
			r.writeMath(w, node.(*mathInline).Expression, "$")
		}
	case KindMathBlock:
		if entering {
			// Leading newline like every other block kind (see the note at
			// KindHTMLBlock): without it a math block glues onto the previous
			// block's last line, and the streaming renderer's settle points
			// coincide with these block boundaries.
			writes(w, node, "\n")
			r.writeMath(w, node.(*mathBlock).Expression, "$$")
		}
	case KindBBCodeColorOpen:
		// Codes go straight to the walk's buffer (r.bb.outBuf), never
		// through the wrapped writer — see bbcode_render.go's design
		// notes. The node's Code was resolved at parse time; "" (an
		// invalid color) is a transparent placeholder.
		if entering && r.bb != nil {
			r.bb.openInto(r.bb.outBuf, node.(*BBCodeColorOpen))
		}
	case KindBBCodeColorClose:
		if entering && r.bb != nil {
			r.bb.closeInto(r.bb.outBuf, node.(*BBCodeColorClose), r.bbWarn)
		}
	case extast.KindTaskCheckBox:
		tc := node.(*extast.TaskCheckBox)
		if entering {
			if tc.IsChecked {
				writes(w, node, "[x] ")
			} else {
				writes(w, node, "[ ] ")
			}
		}
	case ast.KindThematicBreak:
		if entering {
			writes(w, node, "\n"+strings.Repeat("-", 40))
		}
	case ast.KindBlockquote:
	case ast.KindHTMLBlock:
		if entering {
			html := node.(*ast.HTMLBlock)
			var buf bytes.Buffer
			for i := 0; i < html.Lines().Len(); i++ {
				if i > 0 {
					buf.WriteByte('\n')
				}
				buf.WriteString(r.segmentText(html.Lines().At(i)))
			}
			// Leading newline like every other block kind: without it an
			// HTML block glues directly onto the previous line's text
			// ("para<div>"). The streaming renderer also relies on every
			// block opening with \n — its entry boundaries coincide with
			// the render's own newlines, so a settled block can be
			// emitted the moment the next line classifies its blank line
			// as a boundary (streamfuzz_test.go pins the oracles).
			writes(w, node, "\n"+strings.TrimSpace(buf.String()))
		}
		return ast.WalkSkipChildren
	}
	return ast.WalkContinue
}

const maxCodeBlockPadWidth = 80

func writeHighlightedCodeBlock(w io.Writer, node ast.Node, highlighted string) {
	lines := strings.Split(highlighted, "\n")
	for len(lines) > 0 && plainLength(lines[0]) == 0 {
		lines = lines[1:]
	}
	for len(lines) > 0 && plainLength(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	maxWidth := 0
	for _, v := range lines {
		if l := plainLength(v); maxWidth < l {
			maxWidth = l
		}
	}
	padWidth := min(maxWidth, maxCodeBlockPadWidth)
	var outs []string
	for _, v := range lines {
		rpad := strings.Repeat(" ", max(padWidth-plainLength(v), 0))
		outs = append(outs, fmt.Sprintf(" \x030,90%s%s\x03 ", v, rpad))
	}
	if len(outs) > 0 {
		writes(w, node, strings.Join(outs, "\n"))
	}
}

func writePlainCodeBlock(w io.Writer, node ast.Node, text string) {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	maxWidth := 0
	for _, v := range lines {
		if maxWidth < utf8.RuneCountInString(v) {
			maxWidth = utf8.RuneCountInString(v)
		}
	}
	padWidth := min(maxWidth, maxCodeBlockPadWidth)
	var outs []string
	for _, v := range lines {
		outs = append(outs, fmt.Sprintf(" \x030,90%-*s\x03 ", padWidth, v))
	}
	if len(outs) > 0 {
		writes(w, node, strings.Join(outs, "\n"))
	}
}

func writePlainCodeBlockFixed(w io.Writer, node ast.Node, text string) {
	lines := strings.Split(text, "\n")
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	const padWidth = 80
	var outs []string
	for _, v := range lines {
		outs = append(outs, fmt.Sprintf(" \x030,90%-*s\x03 ", padWidth, v))
	}
	if len(outs) > 0 {
		writes(w, node, strings.Join(outs, "\n"))
	}
}

// renderNodeTo renders a table cell's content into buf (extractTableData
// builds each cell's text here). With bbcode active the writes flow
// through the render state so digit guards apply inside cells (matching
// today's translated cell bytes — translation used to run before
// markdown); cells are single-line, so the newline re-emit cannot fire,
// but the guard and the color stack must. Signature takes *bytes.Buffer,
// not io.Writer, so the tag cases can hand the state THIS buffer —
// openInto/closeInto write codes into it directly while text goes through
// the guarded path.
func (r *Renderer) renderNodeTo(buf *bytes.Buffer, node ast.Node) {
	switch node.Kind() {
	case ast.KindEmphasis:
		em := node.(*ast.Emphasis)
		if em.Level == 2 {
			r.cellWrite(buf, "\x02")
		} else {
			r.cellWrite(buf, "\x1D")
		}
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			r.renderNodeTo(buf, c)
		}
		if em.Level == 2 {
			r.cellWrite(buf, "\x02")
		} else {
			r.cellWrite(buf, "\x1D")
		}
	case ast.KindCodeSpan:
		r.cellWrite(buf, fmt.Sprintf("\x030,90%s\x03", r.nodeText(node)))
	case ast.KindLink:
		link := node.(*ast.Link)
		// Same clickability contract as renderNode's KindLink (see the
		// DESIGN NOTE there): paren-free "text: url", self-link and
		// empty-label collapse to the bare URL, destination unescaped,
		// predicate compared like-for-like post-unescape.
		dest := unescapeMarkdownText(string(link.Destination))
		if trimmed := strings.TrimSpace(r.nodeText(node)); trimmed == "" || unescapeMarkdownText(trimmed) == dest {
			r.cellWrite(buf, dest)
			return
		}
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			r.renderNodeTo(buf, c)
		}
		r.cellWrite(buf, fmt.Sprintf(": %s", dest))
	case ast.KindAutoLink:
		// Same as renderNode's KindAutoLink: the URL is not walked
		// children — without this case the cell renders empty.
		r.cellWrite(buf, string(node.(*ast.AutoLink).URL(r.source)))
	case ast.KindImage:
		img := node.(*ast.Image)
		r.cellWrite(buf, "[image: ")
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			r.renderNodeTo(buf, c)
		}
		r.cellWrite(buf, fmt.Sprintf("] %s", unescapeMarkdownText(string(img.Destination))))
	case ast.KindText:
		t := node.(*ast.Text)
		if !t.IsRaw() {
			txt := unescapeMarkdownText(r.segmentText(t.Segment))
			if t.HardLineBreak() {
				r.cellWrite(buf, strings.TrimRight(txt, "\n")+"\n")
			} else if t.SoftLineBreak() {
				r.cellWrite(buf, strings.TrimRight(txt, "\n")+" ")
			} else {
				r.cellWrite(buf, strings.TrimRight(txt, "\n"))
			}
		}
	case ast.KindRawHTML:
		html := node.(*ast.RawHTML)
		for i := 0; i < html.Segments.Len(); i++ {
			seg := html.Segments.At(i)
			tag := strings.ToLower(string(seg.Value(r.source)))
			if tag == "<br>" || tag == "<br/>" || tag == "<br />" {
				r.cellWrite(buf, "\n")
			} else {
				r.cellWrite(buf, string(seg.Value(r.source)))
			}
		}
	case KindMathInline:
		r.writeMathTo(buf, node.(*mathInline).Expression, "$")
	case KindMathBlock:
		r.writeMathTo(buf, node.(*mathBlock).Expression, "$$")
	case KindBBCodeColorOpen:
		if r.bb != nil {
			r.bb.openInto(buf, node.(*BBCodeColorOpen))
		}
	case KindBBCodeColorClose:
		if r.bb != nil {
			r.bb.closeInto(buf, node.(*BBCodeColorClose), r.bbWarn)
		}
	default:
		if node.Type() == ast.TypeBlock || node.Type() == ast.TypeInline {
			for c := node.FirstChild(); c != nil; c = c.NextSibling() {
				r.renderNodeTo(buf, c)
			}
		}
	}
}

// cellWrite is renderNodeTo's write choke point: with bbcode active,
// every cell byte (text AND renderer-generated emphasis/code-span codes)
// flows through the render state — a \x02 after a color emission must
// clear the digit-guard adjacency exactly like the byte-translated cells
// of today, and a color open inside a cell must arm it.
func (r *Renderer) cellWrite(buf *bytes.Buffer, s string) {
	if r.bb != nil {
		r.bb.textInto(buf, []byte(s))
		return
	}
	buf.WriteString(s)
}

// writeMathTo is renderNodeTo's math twin of writeMath (same rendering
// decisions; cell-buffer shaped so it can route through cellWrite).
func (r *Renderer) writeMathTo(buf *bytes.Buffer, expr []byte, delim string) {
	var tmp bytes.Buffer
	r.writeMath(&tmp, expr, delim)
	r.cellWrite(buf, tmp.String())
}

// writeMath renders a math node's expression to single-line Unicode via
// termtex.
//
// DESIGN NOTE — Inline:true is forced for BOTH inline and block math.
// termtex's default (display) style typesets fractions stacked over a ─ bar
// across multiple rows — correct for a monospace terminal, useless on IRC
// where clients use proportional fonts and every line is an independent
// PRIVMSG (stacked art cannot align). The Inline "text style" flattens
// fractions to a/b and keeps everything on ONE row, which survives every
// client. termtex's own goldmark renderer uses display style for $$ blocks;
// we deliberately override that because our output target is IRC, not a
// terminal.
//
// DESIGN NOTE — raw fallback: on a render error (malformed LaTeX, e.g.
// "\frac{" → "unclosed brace group") or empty rendered output, the ORIGINAL
// delimiters + raw expression are written verbatim so no model output is
// ever lost: the raw $...$ text is exactly what the user saw before math
// support existed, so degradation equals the previous behavior.
func (r *Renderer) writeMath(w io.Writer, expr []byte, delim string) {
	if out, err := termtex.Render(string(expr), termtex.Style{Inline: true}); err == nil && out != "" {
		fmt.Fprint(w, out)
		return
	}
	fmt.Fprint(w, delim+string(expr)+delim)
}

func MarkdownToIRC(response string) string {
	return MarkdownToIRCOpts(BBRenderOptions{}, response)
}

func MarkdownToIRCStream(response string) string {
	return MarkdownToIRCStreamOpts(BBRenderOptions{}, response)
}

// BBRenderOptions selects markdown-aware BBCode rendering: BBCode enables
// the tag extension + render state, Warn receives one message per
// malformed tag (per parse for invalid colors, per render for stray
// closes; nil is silent). The unexported state field lets the
// StreamingRenderer inject its LONG-LIVED state so color spans survive
// fragment boundaries (same package).
type BBRenderOptions struct {
	BBCode bool
	Warn   func(string)
	state  *bbRenderState
}

// MarkdownToIRCOpts is MarkdownToIRC with BBCode rendering options. The
// one-shot form: a fresh render state per call, and the auto-close tail
// (a bare \x03 if a real color is still open and something visible
// followed the last newline) is appended AFTER the render.
func MarkdownToIRCOpts(opts BBRenderOptions, response string) string {
	st := opts.state
	if st == nil && opts.BBCode {
		st = &bbRenderState{}
	}
	r := &Renderer{}
	if st != nil {
		r.bb, r.bbWarn = st, opts.Warn
	}
	var buf bytes.Buffer
	bbGoldmark(r, opts).Convert([]byte(response), &buf)
	out := buf.String()
	if st != nil {
		out += st.autoClose()
	}
	return out
}

// MarkdownToIRCStreamOpts is MarkdownToIRCStream with BBCode rendering
// options. It deliberately does NOT append autoClose: the
// StreamingRenderer injects its long-lived state here per fragment and
// owns flush-time auto-close itself (a stream is not finished at a
// fragment boundary).
func MarkdownToIRCStreamOpts(opts BBRenderOptions, response string) string {
	st := opts.state
	if st == nil && opts.BBCode {
		st = &bbRenderState{}
	}
	r := &Renderer{streaming: true}
	if st != nil {
		r.bb, r.bbWarn = st, opts.Warn
	}
	var buf bytes.Buffer
	bbGoldmark(r, opts).Convert([]byte(response), &buf)
	return buf.String()
}

// bbGoldmark builds the per-call goldmark instance (per-call by design:
// the BBCode parser warns per PARSE, so each byte range must parse
// exactly once — task-2 review forward note). The BBCode extension is
// appended to the slice only when enabled — a nil Extender must never
// reach goldmark.WithExtensions (preflight P-2).
func bbGoldmark(r *Renderer, opts BBRenderOptions) goldmark.Markdown {
	exts := []goldmark.Extender{extension.Table, extension.TaskList, &mathExtension{}}
	if opts.BBCode {
		exts = append(exts, &BBCodeExtension{Warn: opts.Warn})
	}
	return goldmark.New(
		goldmark.WithExtensions(exts...),
		goldmark.WithRenderer(r),
	)
}

// StreamingRenderer incrementally converts a markdown stream to IRC text.
//
// DESIGN (Oct 2026 rewrite): the old implementation was a line state machine
// that tried to detect code fences with `strings.TrimSpace(line) == "```"`.
// It missed info-string fences (```go), ~~~ and 4+-backtick fences, misread
// closing fences as openers after a blank-line split, and dropped buffered
// content at flush — ~29% of generated nesting combinations diverged from a
// whole-document render (docs/streaming-markdown-investigation.md, with the
// fuzz harness in streamfuzz_test.go that proved it).
//
// The rewrite never guesses markdown structure. It only decides WHERE the
// stream is safe to cut, and lets goldmark render each settled region:
//
//   - a prefix ending at a blank-line run is settled when the run is not
//     interior to a block that can span blank lines — fenced code, an
//     indented-code continuation, a list (items or 2+-space continuation
//     content), or <pre>/<script>/<style>/<textarea> HTML spans;
//   - the unrendered tail buf[rendered:settled] is rendered as a STANDALONE
//     document (a fragment) and appended — every byte range parses exactly
//     once (the BBCode parser warns per parse, so whole-prefix re-renders
//     would double-fire warns), and a fragment's rendering is frozen the
//     moment it is emitted, so nothing already sent ever changes;
//   - flush renders the remaining tail — an unclosed fence or trailing
//     partial line renders exactly like the non-streaming path would,
//     instead of being dropped.
//
// Fragment boundaries are always block-aligned (settled only ever advances
// to a blank-line end or a top-level closing fence — positions where the
// next byte starts a fresh block), so a fragment parses standalone into the
// same blocks it contributed to the whole document, and the concatenation
// of fragment renders is byte-identical to the whole-document render with
// each fragment's leading block newline trimmed (the entry boundary stands
// for it — each entry is sent as its own IRC message). The
// chunk-invariance and parity oracles in streamfuzz_test.go pin this.
//
// With BBRenderOptions.BBCode set (NewStreamingRendererOpts), ONE
// bbRenderState is carried across fragment renders: a color span opened in
// one fragment stays open (per-line re-emission, stack discipline) into the
// next, and autoClose is spent exactly once per stream — at flush, never at
// a fragment edge. BBCode tags cannot span lines (the value charset
// excludes \n), so a settled complete line can never split a decidable tag
// and the byte translator's trailing-partial-tag hold is not needed here:
// the incomplete last line never settles.
//
// Settling is deliberately conservative: holding longer only delays output,
// never corrupts it. Known (accepted) divergence: a link-reference
// definition arriving after a paragraph that referenced it — goldmark
// resolves references per document, and we render per fragment.
type StreamingRenderer struct {
	buf string

	scanEnd  int // offset just past the last complete line scanned
	settled  int // length of the render-final prefix (always <= scanEnd)
	rendered int // start offset of the next unrendered fragment (always <= settled)

	fenceChar   byte // 0 = none, '`' or '~' — open fence state
	fenceLen    int
	fenceIndent int // leading-column of the opener; 0 = top-level fence
	// fenceContentIndent is the content column of the container a fence
	// was opened in: 0 for document-level fences, the marker's content
	// column for a fence opened ON a list-marker line. A fence's closer
	// is only valid indented INTO its container (content indent .. +3).
	fenceContentIndent int
	htmlTag            string // open <pre>/<script>/<style>/<textarea> span, "" = none
	// htmlSpanContent is the content column of the item a span HTML
	// block was opened in (0 = document); htmlBlank and htmlBlankContent
	// are the same pair for type-6/7 HTML blocks, and mathOpen /
	// mathContent for $$ blocks. Every blank-spanning block the scanner
	// tracks dies with its item when a shallower line arrives (see the
	// fence branch's DESIGN NOTE).
	htmlSpanContent  int
	htmlBlank        bool
	htmlBlankContent int
	mathOpen         bool
	mathContent      int
	// htmlEnd is the terminator of an open type-2..5 HTML block ("-->"
	// for comments, "?>" for processing instructions, ">" for
	// declarations, "]]>" for CDATA; "" = none) and htmlEndContent is
	// the content column of the item it was opened in (0 = document).
	// These blocks span BLANK lines — only their terminator line ends
	// them (review finding on 995d032: settling at an interior blank
	// split a fragment inside the block and the entry join dropped the
	// blank's bytes) — and they die with their item on a shallower line,
	// same rule as fences. htmlSpanContent above is the same container
	// tracking for type-1 spans: NOT dead — a span opened inside an item
	// and later "closed" by a shallower-line regime diverges in the
	// RELEASE direction without it, exactly the class htmlEnd fixes.
	htmlEnd        string
	htmlEndContent int
	blankEnd       int // offset past the last blank line of the current run; 0 = none
	// runListStack holds the content columns of the list items live in
	// the stream (innermost last). A fence opened while items are live
	// lives INSIDE the innermost one, so its closer is only valid
	// indented into the item — and any line shallower than an item's
	// content column kills that item and everything in it, UNLESS the
	// line lazily continues the item's open paragraph (see the fence
	// branch's and the default branch's DESIGN NOTEs). Blank lines do
	// NOT clear the stack: a loose list's items survive blanks exactly
	// like the parser keeps them alive (content indented into the item
	// continues it); only a line too shallow to be item content ends an
	// item.
	runListStack []int
	// itemParaOpen: the innermost live item's last block is a paragraph
	// that shallow lines may lazily continue. Maintained from the same
	// lines the parser sees: markers open items with a paragraph (unless
	// the marker line itself opens a block), item content text and lazy
	// continuations keep one open, block openers and blank lines close
	// it. docParaOpen is the same state for the document level — its
	// only reader is the type-7 HTML gate (htmlBlankOpen).
	itemParaOpen bool
	docParaOpen  bool
	// prevLineBlank: the previous complete line was blank — lazy
	// continuation is impossible across a blank (the paragraph ended).
	prevLineBlank bool

	// bbOpts configures fragment renders; the zero value is the plain
	// legacy renderer (byte-identical output — BBCode off means no state,
	// no extension, no auto-close). bbState is the ONE long-lived render
	// state carried across fragments when bbOpts.BBCode is set.
	bbOpts  BBRenderOptions
	bbState *bbRenderState
}

func NewStreamingRenderer() *StreamingRenderer {
	return &StreamingRenderer{}
}

// NewStreamingRendererOpts is NewStreamingRenderer with BBCode rendering:
// opts.BBCode enables markdown-aware color-tag rendering with ONE render
// state carried across fragments (a span opened in one fragment survives
// into the next; auto-close fires only at flush). opts.Warn receives one
// message per malformed tag occurrence across the whole stream.
func NewStreamingRendererOpts(opts BBRenderOptions) *StreamingRenderer {
	return &StreamingRenderer{bbOpts: opts}
}

// leadingWidth counts the display column of the leading whitespace of line
// (a tab counts as 4).
func leadingWidth(line string) int {
	w := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			w++
		case '\t':
			w += 4
		default:
			return w
		}
	}
	return w
}

// openingFence reports a CommonMark fence opener: up to 3 leading spaces,
// 3+ backticks or tildes, and an info string (which for backtick fences may
// not contain backticks).
func openingFence(line string) (char byte, n int, ok bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i > 3 || len(line)-i < 3 {
		return 0, 0, false
	}
	rest := line[i:]
	ch := rest[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	c := 0
	for c < len(rest) && rest[c] == ch {
		c++
	}
	if c < 3 {
		return 0, 0, false
	}
	info := strings.TrimSpace(rest[c:])
	if ch == '`' && strings.ContainsRune(info, '`') {
		return 0, 0, false
	}
	return ch, c, true
}

// isClosingFence matches a trimmed line that closes an open fence: only
// fence characters, at least as many as the opener, nothing after.
func isClosingFence(trimmed string, char byte, n int) bool {
	if trimmed == "" {
		return false
	}
	c := 0
	for c < len(trimmed) && trimmed[c] == char {
		c++
	}
	return c >= n && strings.TrimSpace(trimmed[c:]) == ""
}

// isListMarkerLine matches a bullet or ordered-list marker line (marker
// indented 0-3, followed by a space/tab or end of line).
func isListMarkerLine(line string) bool {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i > 3 || i == len(line) {
		return false
	}
	rest := line[i:]
	switch rest[0] {
	case '-', '*', '+':
		rest = rest[1:]
	default:
		d := 0
		for d < len(rest) && rest[d] >= '0' && rest[d] <= '9' {
			d++
		}
		if d == 0 || d > 9 || d >= len(rest) || (rest[d] != '.' && rest[d] != ')') {
			return false
		}
		rest = rest[d+1:]
	}
	return rest == "" || rest[0] == ' ' || rest[0] == '\t'
}

// afterListMarker strips a leading list-marker prefix ("  - ", "1. ",
// "3) ") so fence and HTML-span detection also recognizes openers written
// ON a marker line ("- ```py" — a shape LLMs emit constantly). Without
// this, the block's real closer was misread as a NEW top-level opener and
// the scanner held the entire remaining stream until flush (found in code
// review: silent batch degradation, byte-correct output so no oracle
// caught it). ok is false when the line does not start with a marker
// followed by whitespace.
func afterListMarker(line string) (string, bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i > 3 || i == len(line) {
		return line, false
	}
	rest := line[i:]
	var m int
	switch rest[0] {
	case '-', '*', '+':
		m = 1
	default:
		d := 0
		for d < len(rest) && rest[d] >= '0' && rest[d] <= '9' {
			d++
		}
		if d == 0 || d > 9 || d >= len(rest) || (rest[d] != '.' && rest[d] != ')') {
			return line, false
		}
		m = d + 1
	}
	rest = rest[m:]
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return line, false
	}
	return rest[1:], true
}

var htmlSpanTags = []string{"pre", "script", "style", "textarea"}

// isMathBlockOpener reports whether a trimmed line opens a multi-line $$
// math block: it must start with $$ and NOT carry another $$ later on the
// same line (that is a complete single-line block, which never holds the
// stream). Mirrors mathBlockParser.Open's rule exactly.
//
// Safety of the classifier-vs-parser mismatch: the classifier opens on any
// trimmed-$$-prefixed line, while the parser only runs where a block start
// is legal — a $$ line inside an open fence or HTML span (handled earlier
// in scan's switch) or indented ≥4 (indented code) never opens a math
// block, so both directions of divergence only make the classifier hold
// LONGER. The parser also opens math the classifier cannot see
// (container-prefixed openers like "- $$" or "> $$"), but those can never
// span a CLASSIFIER-VISIBLE blank run that a later top-level line would
// settle on: a blank line closes a blockquote (and its math child), and
// post-blank list-item content is indented or marker-lined, which the
// settle rule already holds. The classifier therefore never settles inside
// a math block the parser still has open. Settling is deliberately
// conservative: holding longer only delays output, never corrupts it.
func isMathBlockOpener(trimmed string) bool {
	return strings.HasPrefix(trimmed, "$$") && !strings.Contains(trimmed[2:], "$$")
}

// htmlSpanOpen matches an HTML block whose close tag may be far away and
// which can therefore span blank lines (CommonMark HTML block types 1/2).
func htmlSpanOpen(line string) (string, bool) {
	l := strings.ToLower(strings.TrimSpace(line))
	for _, tag := range htmlSpanTags {
		if !strings.HasPrefix(l, "<"+tag) {
			continue
		}
		after := l[1+len(tag):]
		if after == "" || after[0] == ' ' || after[0] == '>' || after[0] == '/' || after[0] == '\t' {
			if strings.Contains(l, "</"+tag) {
				return "", false // opened and closed on the same line
			}
			return tag, true
		}
	}
	return "", false
}

// htmlBlockTags is CommonMark's type-6 HTML block tag list: a line
// starting with one of these tags opens an HTML block regardless of what
// follows on the line.
var htmlBlockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "base": true,
	"basefont": true, "blockquote": true, "body": true, "caption": true,
	"center": true, "col": true, "colgroup": true, "dd": true,
	"details": true, "dialog": true, "dir": true, "div": true, "dl": true,
	"dt": true, "fieldset": true, "figcaption": true, "figure": true,
	"footer": true, "form": true, "frame": true, "frameset": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"head": true, "header": true, "hr": true, "html": true,
	"iframe": true, "legend": true, "li": true, "link": true, "main": true,
	"menu": true, "menuitem": true, "nav": true, "noframes": true,
	"ol": true, "optgroup": true, "option": true, "p": true, "param": true,
	"section": true, "source": true, "summary": true, "table": true,
	"tbody": true, "td": true, "tfoot": true, "th": true, "thead": true,
	"title": true, "tr": true, "track": true, "ul": true,
}

// htmlBlankOpen reports whether a trimmed line opens a type-6/7 HTML
// block — one that ends at the next BLANK line and swallows
// fence/math/span-shaped lines as content. The scanner must know these
// are open or it invents phantom fences and settles inside them
// (fragment renders then diverge from the whole document; found by the
// streaming survey). Parser-faithful shapes only (fixture class
// "<br> <http…"): a type-6 tag from the list above may be followed by
// anything; any other tag must be COMPLETE on the line with only
// whitespace after (type 7) — and type 7 CANNOT interrupt a paragraph,
// so under an open paragraph (paraOpen) only a type-6 tag qualifies.
// "<https://…>" autolinks are neither (the ':' after the name fails the
// tag shape), so they stay paragraph text a fence may interrupt.
func htmlBlankOpen(trimmed string, paraOpen bool) bool {
	if !strings.HasPrefix(trimmed, "<") {
		return false
	}
	t := trimmed[1:]
	if strings.HasPrefix(t, "/") {
		t = t[1:]
	}
	i := 0
	for i < len(t) && (t[i] >= 'a' && t[i] <= 'z' || t[i] >= 'A' && t[i] <= 'Z' || t[i] >= '0' && t[i] <= '9' || t[i] == '-') {
		i++
	}
	if i == 0 {
		return false // not a name: comment, PI, autolink-ish, raw '<'
	}
	rest := t[i:]
	if rest != "" {
		// A tag boundary must follow the name (':', as in
		// "<https://…>", is an autolink scheme, not a tag).
		if c0 := rest[0]; c0 != '>' && c0 != '/' && c0 != ' ' && c0 != '\t' {
			return false
		}
	}
	// Type 6: a block-list tag name (opening or closing form, bare or
	// with anything after) ALWAYS opens a block and may interrupt a
	// paragraph — goldmark checks the block list before the type-7
	// shape, and its type-6 fallback regexp also matches "</div>" and
	// bare "<div" forms.
	if htmlBlockTags[strings.ToLower(t[:i])] {
		return true
	}
	// Type 7: any other COMPLETE tag with only whitespace after it —
	// cannot interrupt a paragraph.
	if rest != "" {
		if j := strings.IndexByte(rest, '>'); j >= 0 && isOnlyWS(rest[j+1:]) {
			return !paraOpen
		}
	}
	return false
}

// htmlEndOpen returns the terminator of an HTML block type 2-5 opened by
// a trimmed line: "-->" for comments (<!--), "?>" for processing
// instructions (<?), ">" for declarations (<! followed by UPPERCASE
// letters, goldmark's type-4 shape), "]]>" for CDATA (<![CDATA[); ""
// when the line opens none of them. Unlike types 6/7 these blocks span
// BLANK lines — only a line containing the terminator ends them — so the
// scanner must hold settles until it appears (review finding on 995d032:
// goldmark parses "<!--\nfoo\n\nbar" as ONE HTMLBlock; settling at the
// interior blank split the fragment inside it and the entry join dropped
// the blank's bytes — a byte regression vs the whole-prefix base).
// An opener that already contains its terminator is a single-line block
// (goldmark closes it at the next line's Continue): no spanning state.
// Types 1-6 may interrupt paragraphs, so no paragraph gate applies.
func htmlEndOpen(trimmed string) string {
	switch {
	case strings.HasPrefix(trimmed, "<!--"):
		if strings.Contains(trimmed, "-->") {
			return ""
		}
		return "-->"
	case strings.HasPrefix(trimmed, "<?"):
		if strings.Contains(trimmed, "?>") {
			return ""
		}
		return "?>"
	case strings.HasPrefix(trimmed, "<![CDATA["):
		if strings.Contains(trimmed, "]]>") {
			return ""
		}
		return "]]>"
	case len(trimmed) >= 3 && trimmed[0] == '<' && trimmed[1] == '!':
		i := 2
		for i < len(trimmed) && trimmed[i] >= 'A' && trimmed[i] <= 'Z' {
			i++
		}
		if i == 2 {
			return "" // <! must be followed by UPPERCASE letters
		}
		if strings.Contains(trimmed, ">") {
			return ""
		}
		return ">"
	}
	return ""
}

// isSetextUnderline reports whether a trimmed line is a setext heading
// underline: a run of one or more '=' or '-' (a single kind) followed by
// TRAILING whitespace only — internal spaces/tabs ("= =") are forbidden
// (CommonMark; scoped re-review: the loose shape wrongly ended
// paragraphs whose continuation lines happened to look like underlines).
// Directly under a paragraph it consumes the paragraph into a heading
// (ending it); setext underlines can never be LAZY continuation lines,
// so the lazy path never consults this for real underlines. BLOCK-level
// only: callers must also require leading width ≤ 3 (at ≥4 the line is
// indented code or paragraph continuation, never an underline).
func isSetextUnderline(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	c := trimmed[0]
	if c != '=' && c != '-' {
		return false
	}
	i := 1
	for i < len(trimmed) && trimmed[i] == c {
		i++
	}
	for ; i < len(trimmed); i++ {
		if trimmed[i] != ' ' && trimmed[i] != '\t' {
			return false
		}
	}
	return true
}

// isOnlyWS reports whether s is empty or entirely spaces/tabs.
func isOnlyWS(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != ' ' && s[i] != '\t' {
			return false
		}
	}
	return true
}

// isATXHeading reports whether a trimmed line is an ATX heading opener
// (1-6 '#' then a space or end of line) — a block start that interrupts
// paragraphs.
func isATXHeading(t string) bool {
	if !strings.HasPrefix(t, "#") {
		return false
	}
	i := 0
	for i < len(t) && t[i] == '#' {
		i++
	}
	return i <= 6 && (i == len(t) || t[i] == ' ' || t[i] == '\t')
}

// isThematicBreak reports whether a trimmed line is a thematic break:
// 3+ of a single '-', '*' or '_' with optional internal spaces and
// nothing else.
func isThematicBreak(t string) bool {
	var ch byte
	n := 0
	for i := 0; i < len(t); i++ {
		switch c := t[i]; c {
		case '-', '*', '_':
			if ch == 0 {
				ch = c
			} else if c != ch {
				return false
			}
			n++
		case ' ', '\t':
		default:
			return false
		}
	}
	return ch != 0 && n >= 3
}

// orderedStart returns the leading ordered-list number of a marker line
// ("4. x", "2) y"), ok only for ordered markers.
func orderedStart(line string) (int, bool) {
	i := 0
	for i < len(line) && line[i] == ' ' {
		i++
	}
	if i > 3 {
		return 0, false
	}
	d := 0
	for d < len(line) && line[d+i] >= '0' && line[d+i] <= '9' {
		d++
	}
	if d == 0 || d > 9 || d+i >= len(line) || (line[d+i] != '.' && line[d+i] != ')') {
		return 0, false
	}
	n := 0
	for _, c := range []byte(line[i : i+d]) {
		n = n*10 + int(c-'0')
	}
	return n, true
}

// lazyShaped reports whether a line can extend the innermost item's open
// PARAGRAPH by lazy continuation: anything except the constructs that
// start a block and may interrupt a paragraph. Consulted only when a
// paragraph is open, which is what makes the cases unambiguous:
//
//   - blockquotes, fence openers, ATX headings, $$ math openers, type-6
//     HTML block tags and thematic breaks all interrupt; a setext
//     underline ("---", "===") LOOKS continuable but the spec forbids
//     setext underlines as lazy continuation lines — in the lazy
//     position the line is a thematic break (or text), never a setext
//     heading, so all thematic-break shapes interrupt here;
//   - type-7 HTML ("<br>") and ordered markers with start != 1 cannot
//     interrupt a paragraph, so they stay paragraph text.
func lazyShaped(line, trimmed string) bool {
	if trimmed == "" || strings.HasPrefix(trimmed, ">") {
		return false
	}
	if _, _, ok := openingFence(line); ok {
		return false
	}
	if isATXHeading(trimmed) {
		return false
	}
	if isMathBlockOpener(trimmed) {
		return false
	}
	if isThematicBreak(trimmed) {
		return false
	}
	// Type-6 HTML block tags interrupt paragraphs (only type 7 cannot):
	// htmlBlankOpen with paraOpen=true reports exactly the type-6 shape.
	// Type-2..5 openers interrupt too (types 1-6 all may).
	if htmlBlankOpen(trimmed, true) || htmlEndOpen(trimmed) != "" {
		return false
	}
	if isListMarkerLine(line) {
		return false // marker lines close the item (any number, any kind)
	}
	return true
}

// popListStackTo kills every live list item whose content column is
// deeper than col (a line shallower than an item's content column cannot
// be that item's content, and blocks have no lazy continuation — only
// paragraphs do, handled before this is consulted) and returns the
// content column of the innermost survivor (0 = none).
func (s *StreamingRenderer) popListStackTo(col int) int {
	for len(s.runListStack) > 0 && s.runListStack[len(s.runListStack)-1] > col {
		s.runListStack = s.runListStack[:len(s.runListStack)-1]
	}
	if len(s.runListStack) == 0 {
		return 0
	}
	return s.runListStack[len(s.runListStack)-1]
}

// scan advances the settle scanner over complete lines. See the struct
// comment for the rules.
func (s *StreamingRenderer) scan() {
	for {
		nl := strings.IndexByte(s.buf[s.scanEnd:], '\n')
		if nl < 0 {
			return
		}
		line := s.buf[s.scanEnd : s.scanEnd+nl]
		lineEnd := s.scanEnd + nl + 1
		trimmed := strings.TrimSpace(line)

		// A line that kills its container's open block is RE-DRIVEN
		// through the switch with the old state dropped, so the killing
		// line is classified exactly as the parser classifies it at the
		// block start it creates (it may open a fence, a $$ block, an
		// HTML block, or plain text). blankEnd is always 0 on entry to a
		// kill — the state cases reset it every line — so the re-drive's
		// default case cannot mis-settle.
		for reprocess := true; reprocess; {
			reprocess = false
			switch {
			case s.fenceChar != 0:
				lw := leadingWidth(line)
				if isClosingFence(trimmed, s.fenceChar, s.fenceLen) &&
					lw >= s.fenceContentIndent && lw <= s.fenceContentIndent+3 {
					// Fast-settle top-level fences: a column-0 fence is never
					// container content (an indent-0 fence interrupts a list
					// or paragraph), so the block is complete at the closing
					// fence's newline — no need to wait for the blank line
					// after it. Indented openers may be list-item content;
					// those keep waiting for the blank-line rule. The indent
					// range check is parser-faithful: a closer is only valid
					// indented into the fence's own container (content indent
					// .. +3) — a deeper line is fence CONTENT, not a closer.
					if s.fenceIndent == 0 {
						s.settled = max(s.settled, lineEnd)
					}
					s.fenceChar = 0
					s.itemParaOpen = false
				} else if trimmed != "" && s.fenceContentIndent > 0 && lw < s.fenceContentIndent {
					// DESIGN NOTE — fragment-render fence fidelity (classes
					// found by TestStreamingChunkInvariance's corpus and the
					// streaming survey after the fragment rewrite): a fence
					// opened inside a list item (on the marker line, or on a
					// child line indented into the item) is part of the item,
					// and a line indented SHALLOWER than the item's content
					// column is not item content — blocks have no lazy
					// continuation, so that line kills the item AND the
					// fence, then starts a block of its own. Settling while
					// misreading that line as fence content splits a fragment
					// inside whatever the parser opened there ("2. ```py" +
					// a shallow closer, or "- x\n  ```py" + shallow content,
					// are the generator-reachable shapes) and the standalone
					// tail mis-parses. The old whole-prefix re-render
					// self-healed these disagreements; fragments cannot.
					// Blank lines are exempt: inside an item's fence they
					// are CONTENT the fence spans.
					s.fenceChar = 0
					s.popListStackTo(lw)
					s.itemParaOpen = false
					reprocess = true
				} else {
					s.blankEnd = 0
				}
			case s.htmlTag != "":
				lw := leadingWidth(line)
				if trimmed != "" && s.htmlSpanContent > 0 && lw < s.htmlSpanContent {
					// Same container-kill rule as the fence branch.
					s.htmlTag = ""
					s.popListStackTo(lw)
					s.itemParaOpen = false
					reprocess = true
				} else {
					if strings.Contains(strings.ToLower(line), "</"+s.htmlTag) {
						s.htmlTag = ""
						s.itemParaOpen = false
					}
					s.blankEnd = 0
				}
			case s.htmlEnd != "":
				// Types 2-5 span blank lines: every line before the
				// terminator is content — interior blanks never arm the
				// settle rule. The terminator line itself closes the
				// block (the parser consumes it as the closure line and
				// drops it from the render). A container-kill line kills
				// the block with its item, same rule as fences.
				lw := leadingWidth(line)
				if trimmed != "" && s.htmlEndContent > 0 && lw < s.htmlEndContent {
					s.htmlEnd = ""
					s.popListStackTo(lw)
					s.itemParaOpen = false
					reprocess = true
				} else {
					if strings.Contains(line, s.htmlEnd) {
						s.htmlEnd = ""
						s.itemParaOpen = false
					}
					s.blankEnd = 0
				}
			case s.htmlBlank:
				// A type-6/7 HTML block ends at the next blank line; every
				// line before it is content — a fence or math opener on one
				// of those lines never exists for the parser, so no other
				// detection runs while this is open. The blank that closes
				// it counts as a normal blank for the settle rule. A
				// container-kill line (shallow, inside an item) kills the
				// html block with its item, same rule as fences.
				if trimmed == "" {
					s.htmlBlank = false
					s.blankEnd = lineEnd
					s.itemParaOpen = false
				} else if s.htmlBlankContent > 0 && leadingWidth(line) < s.htmlBlankContent {
					s.htmlBlank = false
					s.popListStackTo(leadingWidth(line))
					s.itemParaOpen = false
					reprocess = true
				}
			case s.mathOpen:
				// A $$ math block spans blank lines, so every line that does not
				// itself start a new trimmed $$ is expression content — blank
				// lines included. Blank runs are suppressed exactly like an open
				// fence's; the block can never be cut mid-expression. A
				// container-kill line kills it with its item, same as fences.
				lw := leadingWidth(line)
				if trimmed != "" && s.mathContent > 0 && lw < s.mathContent {
					s.mathOpen = false
					s.popListStackTo(lw)
					s.itemParaOpen = false
					reprocess = true
				} else {
					if strings.HasPrefix(trimmed, "$$") {
						s.mathOpen = false
						s.itemParaOpen = false
					}
					s.blankEnd = 0
				}
			case trimmed == "":
				s.blankEnd = lineEnd
				s.itemParaOpen = false
				s.docParaOpen = false
			default:
				if s.blankEnd != 0 {
					// Non-blank line after a blank run: the run was a block
					// boundary unless the line continues a blank-spanning
					// block (indented code, list item content, or any 2+
					// space indented continuation).
					if leadingWidth(line) < 2 && !isListMarkerLine(line) {
						s.settled = max(s.settled, s.blankEnd)
					}
					s.blankEnd = 0
				}
				// Live-item bookkeeping. A line can only belong to items
				// whose content column it meets. A marker line starts an
				// item whose content column is leading spaces + marker +
				// one space = len(line) - len(detect); deeper items die.
				// Any other line shallower than the innermost item kills
				// that item — UNLESS it lazily continues the item's open
				// paragraph (DESIGN NOTE — the parser keeps the item alive
				// across such lines, and both directions of getting this
				// wrong misplace later fence closers: killing a live item
				// widens the closer window the parser narrows, keeping a
				// dead one narrows it; only the exact rule is safe. Blank
				// lines never reach here, and lazy continuation is
				// impossible directly after one — the paragraph ended).
				lw := leadingWidth(line)
				detect := line
				containerFence := false
				marker := false
				lazyKept := false
				// An ordered marker with start != 1 cannot interrupt a
				// DOCUMENT-level paragraph: under one (directly above —
				// no blank) the line is paragraph text, not an item
				// marker. An open ITEM paragraph does NOT block a
				// marker: a shallower marker line closes the item and
				// continues the list, whatever its number.
				paraCtx := !s.prevLineBlank && s.docParaOpen
				markerOK := true
				if n, ok := orderedStart(line); ok && n != 1 && paraCtx {
					markerOK = false
				}
				if stripped, ok := afterListMarker(line); ok && markerOK && !isThematicBreak(trimmed) {
					// (A thematic-break-shaped marker line like "- - -"
					// is NOT a list item — CommonMark gives the break
					// precedence.)
					detect = stripped
					containerFence = true
					marker = true
					// The marker line can only be content of items
					// whose content column it meets, so shallower
					// items die FIRST (pop to the line's own indent) —
					// a top-level marker REPLACES a deeper list, it
					// does not stack on it. The new item's content
					// column is marker + following spaces (CommonMark:
					// all of them when ≤ 4, else just one): afterListMarker
					// strips exactly one space, so the rest of detect's
					// leading width adds on top of marker + 1.
					s.popListStackTo(lw)
					c := len(line) - len(detect)
					if k := leadingWidth(detect); k < 4 {
						c += k
					}
					s.runListStack = append(s.runListStack, c)
				} else if len(s.runListStack) > 0 && s.runListStack[len(s.runListStack)-1] > lw {
					if s.itemParaOpen && !s.prevLineBlank && lazyShaped(line, trimmed) {
						// Lazy paragraph continuation: the item survives,
						// the paragraph stays open.
						lazyKept = true
					} else {
						s.popListStackTo(lw)
						s.itemParaOpen = false
					}
				}
				inner := 0
				if n := len(s.runListStack); n > 0 {
					inner = s.runListStack[n-1]
				}
				// Inside an item, block openers live at the item's
				// content column: dedent detect by inner so a fence
				// written at a relative indent of 0-3 (absolute 4+ when
				// the item is deep) is still recognized — the parser
				// measures fence indentation relative to the container.
				if inner > 0 && len(detect) >= inner && leadingWidth(detect) >= inner {
					detect = detect[inner:]
				}
				openedBlock := false
				if isMathBlockOpener(trimmed) {
					s.mathOpen = true
					s.mathContent = inner
					openedBlock = true
				} else if ch, n, ok := openingFence(detect); ok {
					s.fenceChar, s.fenceLen = ch, n
					s.fenceIndent = lw
					if containerFence {
						s.fenceIndent = max(1, s.fenceIndent)
					}
					// inner = innermost live item at the fence line: the
					// fence lives inside it (closer must be indented into
					// it; see the fence branch's DESIGN NOTE). 0 =
					// document-level fence.
					s.fenceContentIndent = inner
					openedBlock = true
				} else if tag, ok := htmlSpanOpen(detect); ok {
					s.htmlTag = tag
					s.htmlSpanContent = inner
					openedBlock = true
				} else if end := htmlEndOpen(strings.TrimSpace(detect)); end != "" {
					// Type-2..5 HTML block: spans blanks until its
					// terminator line (see the s.htmlEnd case). May
					// interrupt paragraphs (types 1-6 all may; only
					// type 7 cannot), so no paragraph gate applies.
					s.htmlEnd = end
					s.htmlEndContent = inner
					openedBlock = true
				} else if htmlBlankOpen(strings.TrimSpace(detect),
					!marker && (s.docParaOpen || inner > 0 && s.itemParaOpen)) {
					// Type-6/7 HTML block: holds until a blank line (see
					// the s.htmlBlank case). Checked AFTER
					// fence/math/span: those are stricter classifiers for
					// specific line shapes. The paraOpen gate is
					// parser-faithful: type 7 CANNOT interrupt a
					// paragraph ("<br>" after paragraph text is inline
					// HTML and the paragraph continues), so under an
					// open paragraph — document-level or the innermost
					// item's — only a type-6 tag still opens a block. A
					// MARKER line's content is a fresh item context: the
					// marker interrupts the enclosing paragraph itself,
					// so no paragraph gate applies to it.
					s.htmlBlank = true
					s.htmlBlankContent = inner
					openedBlock = true
				}
				// The innermost item's paragraph state: a marker line
				// opens an item whose first block is a paragraph (unless
				// the marker line itself opened a block); item content
				// text and lazy continuations keep the paragraph open; a
				// block opener closed it.
				if openedBlock {
					s.itemParaOpen = false
				} else {
					// Lines indented ≥ 4 RELATIVE to the item's content
					// column PRESERVE the previous state instead of
					// opening a paragraph — at a block start they are
					// indented code inside the item, directly after
					// paragraph text they are its continuation.
					s.itemParaOpen = marker || (inner > 0 && (lazyKept || lw >= inner && (lw < inner+4 || s.itemParaOpen)))
				}
				// Document-level paragraph state, same rules (used by
				// the type-7 gate; ATX headings end paragraphs like
				// block openers — over-reporting an open paragraph only
				// holds the stream, which is safe). FINAL-REVIEW FINDING:
				// a THEMATIC BREAK ("- - -", "---") and a SETEXT UNDERLINE
				// ("===" / "---" directly under a paragraph — setext
				// beats the break there) also END the paragraph: after
				// them the parser has none open, and a stale-open flag
				// wrongly gated the next line's type-7 HTML open — the
				// scanner then saw a phantom fence inside the parser's
				// html block and fast-settled at its closer (the
				// "- - -\n<br>\n```js…" breaker). A setext underline with
				// NO paragraph above is plain text ("===" opens a
				// paragraph) — hence the docParaOpen guard on that arm.
				// RE-REVIEW REGRESSION FIX: both arms are BLOCK-level
				// only (leading width ≤ 3) — at ≥4 a "---"/"===" line
				// under an open paragraph is continuation TEXT, and
				// over-firing here cleared docParaOpen so an ordered≠1
				// marker (which cannot interrupt the still-open
				// paragraph) was misread as an item, cascading into a
				// phantom container-kill of the parser's valid fence
				// closer.
				paraEnded := isThematicBreak(trimmed) && lw <= 3 ||
					s.docParaOpen && !s.prevLineBlank && lw <= 3 && isSetextUnderline(trimmed)
				s.docParaOpen = !paraEnded && !openedBlock && !marker && !isATXHeading(trimmed) &&
					inner == 0 && (lw < 4 || s.docParaOpen)
			}
		}
		s.scanEnd = lineEnd
		s.prevLineBlank = trimmed == ""
	}
}

// Process handles an incremental delta and returns newly settled output
// (possibly multi-line). delta == "" is the FLUSH signal: everything still
// held — unclosed fences, trailing partial lines — is rendered then, and
// with BBCode on, the stream's single auto-close tail is appended.
//
// Cost note: each new fragment renders only its own byte range (the old
// whole-prefix re-render was quadratic in document size); total work is
// linear — every byte parses exactly once.
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

// renderFragment renders buf[rendered:end] as a standalone document and
// advances rendered to end. Fragment boundaries are block-aligned (see the
// struct comment), so the fragment parses into the same blocks it
// contributed to the whole document and its output appends cleanly: every
// block kind renders with a LEADING newline, Render trims the fragment's
// own first one, and the entry boundary stands for it (each entry is sent
// as its own IRC message). The TrimPrefix here is a no-op safety for that
// same contract.
//
// BBCode: the long-lived bbState is injected into the fragment render so
// color-stack/pending-newline/guard state carries across fragments — a
// fragment edge must NOT auto-close (a stream is not finished at a block
// boundary); autoClose is appended at flush ONLY, before the trim and the
// empty check. ONE exception (survey finding, Nov 2026): when the flush
// fragment's own render is EMPTY — every visible byte settled earlier and
// the remainder is trailing newline(s) — the tail is SUPPRESSED. It could
// only land as its own entry: a codes-only PRIVMSG, the blank-message
// artifact every other suppression rule in this package prevents (the
// byte translator's Flush makes the same call for the same reason). The
// one-shot comparison stays exact: MarkdownToIRCStreamOpts renders these
// bytes to nothing, so the stream total equals the whole-document render
// with the color simply left unreset (clients reset formatting per
// message anyway).
func (s *StreamingRenderer) renderFragment(end int, flush bool) []string {
	opts := s.bbOpts
	if opts.BBCode {
		if s.bbState == nil {
			s.bbState = &bbRenderState{}
		}
		opts.state = s.bbState
	}
	out := MarkdownToIRCStreamOpts(opts, s.buf[s.rendered:end])
	if flush && opts.BBCode && out != "" {
		// Single-flush assumption: autoClose is a PURE state read (it
		// does not spend the open stack), so a second Process("") would
		// re-emit the tail as its own codes-only entry — every
		// production call site is terminal (sOut is rebuilt per API
		// round-trip, so a stream is flushed exactly once).
		out += s.bbState.autoClose()
	}
	s.rendered = end
	out = strings.TrimPrefix(out, "\n") // block-leading \n: entry boundary stands for it
	if out == "" {
		return nil
	}
	return []string{out}
}
