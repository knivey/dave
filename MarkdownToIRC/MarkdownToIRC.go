package markdowntoirc

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/knivey/dave/MarkdownToIRC/irc"
	"github.com/knivey/dave/MarkdownToIRC/tables"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
)

type Renderer struct {
	listCounter []int
	source      []byte
	streaming   bool
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
	err := ast.Walk(n, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		return r.renderNode(&buf, node, entering), nil
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
		if !entering {
			writes(w, node, fmt.Sprintf(" (%s)", link.Destination))
		}
	case ast.KindImage:
		img := node.(*ast.Image)
		if entering {
			writes(w, node, "[image: ")
		} else {
			writes(w, node, fmt.Sprintf("](%s)", img.Destination))
		}
	case ast.KindText:
		t := node.(*ast.Text)
		if !entering || t.IsRaw() {
			return ast.WalkContinue
		}
		txt := r.segmentText(t.Segment)
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
			if r.streaming {
				// Tables not supported in streaming mode
				writes(w, node, "[table omitted in streaming mode]\n")
				return ast.WalkSkipChildren
			}
			data := extractTableData(node, r)
			formatted := tables.RenderTable(data)
			writes(w, node, formatted)
			return ast.WalkSkipChildren
		}
	case extast.KindTableHeader, extast.KindTableRow:
	case extast.KindTableCell:
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

func (r *Renderer) renderNodeTo(w io.Writer, node ast.Node) {
	switch node.Kind() {
	case ast.KindEmphasis:
		em := node.(*ast.Emphasis)
		if em.Level == 2 {
			fmt.Fprint(w, "\x02")
		} else {
			fmt.Fprint(w, "\x1D")
		}
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			r.renderNodeTo(w, c)
		}
		if em.Level == 2 {
			fmt.Fprint(w, "\x02")
		} else {
			fmt.Fprint(w, "\x1D")
		}
	case ast.KindCodeSpan:
		fmt.Fprintf(w, "\x030,90%s\x03", r.nodeText(node))
	case ast.KindLink:
		link := node.(*ast.Link)
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			r.renderNodeTo(w, c)
		}
		fmt.Fprintf(w, " (%s)", link.Destination)
	case ast.KindImage:
		img := node.(*ast.Image)
		fmt.Fprint(w, "[image: ")
		for c := node.FirstChild(); c != nil; c = c.NextSibling() {
			r.renderNodeTo(w, c)
		}
		fmt.Fprintf(w, "](%s)", img.Destination)
	case ast.KindText:
		t := node.(*ast.Text)
		if !t.IsRaw() {
			txt := r.segmentText(t.Segment)
			if t.HardLineBreak() {
				fmt.Fprint(w, strings.TrimRight(txt, "\n")+"\n")
			} else if t.SoftLineBreak() {
				fmt.Fprint(w, strings.TrimRight(txt, "\n")+" ")
			} else {
				fmt.Fprint(w, strings.TrimRight(txt, "\n"))
			}
		}
	case ast.KindRawHTML:
		html := node.(*ast.RawHTML)
		for i := 0; i < html.Segments.Len(); i++ {
			seg := html.Segments.At(i)
			tag := strings.ToLower(string(seg.Value(r.source)))
			if tag == "<br>" || tag == "<br/>" || tag == "<br />" {
				fmt.Fprint(w, "\n")
			} else {
				fmt.Fprint(w, string(seg.Value(r.source)))
			}
		}
	default:
		if node.Type() == ast.TypeBlock || node.Type() == ast.TypeInline {
			for c := node.FirstChild(); c != nil; c = c.NextSibling() {
				r.renderNodeTo(w, c)
			}
		}
	}
}

func MarkdownToIRC(response string) string {
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.Table,
			extension.TaskList,
		),
		goldmark.WithRenderer(&Renderer{}),
	)
	var buf bytes.Buffer
	md.Convert([]byte(response), &buf)
	return buf.String()
}

func MarkdownToIRCStream(response string) string {
	r := &Renderer{streaming: true}
	md := goldmark.New(
		goldmark.WithExtensions(
			extension.TaskList, // no tables in streaming
		),
		goldmark.WithRenderer(r),
	)
	var buf bytes.Buffer
	md.Convert([]byte(response), &buf)
	return buf.String()
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
// stream is safe to cut, and lets goldmark render each settled prefix:
//
//   - a prefix ending at a blank-line run is settled when the run is not
//     interior to a block that can span blank lines — fenced code, an
//     indented-code continuation, a list (items or 2+-space continuation
//     content), or <pre>/<script>/<style>/<textarea> HTML spans;
//   - settled prefixes are rendered with MarkdownToIRCStream and the
//     not-yet-emitted complete lines are appended; rendering a longer
//     settled prefix must extend the previous output (verified by the
//     chunk-invariance and parity oracles), so nothing already sent ever
//     changes;
//   - flush renders the WHOLE buffer — an unclosed fence or trailing
//     partial line renders exactly like the non-streaming path would,
//     instead of being dropped.
//
// Settling is deliberately conservative: holding longer only delays output,
// never corrupts it. Known (accepted) divergence: a link-reference
// definition arriving after a paragraph that referenced it — goldmark
// resolves references per document, and we render per prefix.
type StreamingRenderer struct {
	buf string

	scanEnd  int // offset just past the last complete line scanned
	settled  int // length of the render-final prefix (always <= scanEnd)
	rendered int // settled length already rendered (emission bookkeeping)

	fenceChar   byte // 0 = none, '`' or '~' — open fence state
	fenceLen    int
	fenceIndent int    // leading-column of the opener; 0 = top-level fence
	htmlTag     string // open <pre>/<script>/<style>/<textarea> span, "" = none
	blankEnd    int    // offset past the last blank line of the current run; 0 = none

	emitted string // output prefix already returned to the caller
	broken  bool   // monotonicity invariant broke; incremental emission stopped
}

func NewStreamingRenderer() *StreamingRenderer {
	return &StreamingRenderer{}
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

		switch {
		case s.fenceChar != 0:
			if isClosingFence(trimmed, s.fenceChar, s.fenceLen) {
				// Fast-settle top-level fences: a column-0 fence is never
				// container content (an indent-0 fence interrupts a list
				// or paragraph), so the block is complete at the closing
				// fence's newline — no need to wait for the blank line
				// after it. Indented openers may be list-item content;
				// those keep waiting for the blank-line rule.
				if s.fenceIndent == 0 {
					s.settled = max(s.settled, lineEnd)
				}
				s.fenceChar = 0
			}
			s.blankEnd = 0
		case s.htmlTag != "":
			if strings.Contains(strings.ToLower(line), "</"+s.htmlTag) {
				s.htmlTag = ""
			}
			s.blankEnd = 0
		case trimmed == "":
			s.blankEnd = lineEnd
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
			// Fence/span openers may be written on a list-marker line
			// ("- ```py"): detect through the marker so the block's real
			// closer is not misread as a new opener. Marker-line fences
			// are container content — fenceIndent >= 1 disables the
			// top-level closing-fence fast-settle for them.
			detect := line
			containerFence := false
			if stripped, ok := afterListMarker(line); ok {
				detect = stripped
				containerFence = true
			}
			if ch, n, ok := openingFence(detect); ok {
				s.fenceChar, s.fenceLen = ch, n
				s.fenceIndent = leadingWidth(line)
				if containerFence {
					s.fenceIndent = max(1, s.fenceIndent)
				}
			} else if tag, ok := htmlSpanOpen(detect); ok {
				s.htmlTag = tag
			}
		}
		s.scanEnd = lineEnd
	}
}

// Process handles an incremental delta and returns newly settled output
// (possibly multi-line). delta == "" is the FLUSH signal: everything still
// held — unclosed fences, trailing partial lines — is rendered then.
//
// Cost note: each settle re-renders the whole settled prefix with goldmark,
// so cumulative work is quadratic in the document size. Fine at IRC scale
// (a 22KB/300-paragraph reply measured ~230ms total, spread across the
// generation; pastebin wrapping kicks in long before this matters) — if the
// renderer is ever reused for larger surfaces, cache the rendered prefix
// and re-parse from the last block boundary instead.
func (s *StreamingRenderer) Process(delta string) []string {
	if delta == "" {
		out := MarkdownToIRCStream(s.buf)
		s.settled, s.rendered = len(s.buf), len(s.buf)
		return s.emit(out, true)
	}
	s.buf += delta
	s.scan()
	if s.settled <= s.rendered {
		return nil
	}
	out := MarkdownToIRCStream(s.buf[:s.settled])
	s.rendered = s.settled
	return s.emit(out, false)
}

// emit returns the portion of out beyond what was already sent. A settled
// prefix's rendering is frozen, so the whole remainder is released at the
// settle point — the completed block does NOT wait for a following newline.
// This is safe because every block kind renders with a LEADING newline
// (KindHTMLBlock included — see the note there), so the remainder always
// begins at a clean block boundary; the boundary newline itself is trimmed
// from the returned entry because the entry boundary already stands for it
// (each entry is sent as its own IRC message). If a longer settled prefix
// ever renders without the previous output as its prefix (an invariant
// break the fuzz oracles watch for), incremental emission stops and the
// flush emits the remainder raw — IRC lines cannot be unsent, but data
// must not be dropped either.
func (s *StreamingRenderer) emit(out string, flush bool) []string {
	if s.emitted != "" && !strings.HasPrefix(out, s.emitted) {
		s.broken = true
	}
	if s.broken {
		if !flush {
			return nil
		}
		n := min(len(s.emitted), len(out))
		// Resync to a line boundary before emitting: slicing at an
		// arbitrary byte offset can splice a token or an IRC color code
		// mid-sequence. Bytes between the boundary and n may re-send a
		// few already-delivered partial lines — degraded-mode duplication
		// beats mangled bytes, and nothing is dropped.
		if i := strings.LastIndexByte(out[:n], '\n'); i >= 0 {
			n = i + 1
		} else {
			n = 0
		}
		s.emitted = out
		if n < len(out) {
			return []string{out[n:]}
		}
		return nil
	}
	rem := out[len(s.emitted):]
	s.emitted = out
	rem = strings.TrimPrefix(rem, "\n")
	if rem != "" {
		return []string{rem}
	}
	return nil
}
