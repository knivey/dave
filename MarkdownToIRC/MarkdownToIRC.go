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
			writes(w, node, strings.TrimSpace(buf.String()))
		}
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

func normalizeForStreaming(input string) string {
	lines := strings.Split(input, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "+ ") ||
			(len(trimmed) > 2 && trimmed[1] == '.' && (trimmed[0] >= '0' && trimmed[0] <= '9')) {
			// Reduce leading whitespace for list items to prevent code block misparse
			lines[i] = "  " + trimmed
		}
	}
	return strings.Join(lines, "\n")
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

// RenderCodeLine renders a single line inside a code block with fixed 80-char background padding (no Chroma).
// Used by streaming to handle blank lines inside code blocks without premature cutoff.
func RenderCodeLine(line string) string {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		line = " "
	}
	const padWidth = 80
	return fmt.Sprintf(" \x030,90%-*s\x03 ", padWidth, line)
}

type StreamingRenderer struct {
	inCodeBlock bool
	buffer      string
	codeLines   []string
}

func NewStreamingRenderer() *StreamingRenderer {
	return &StreamingRenderer{}
}

// Process handles incremental streaming deltas, maintains state for code blocks, and returns rendered lines when complete.
// Fence lines (```) are skipped. Non-code text accumulates until \n\n for correct multi-line structures (lists, quotes, paragraphs).
func (s *StreamingRenderer) Process(delta string) []string {
	var lines []string
	s.buffer += delta
	for {
		if s.inCodeBlock {
			before, after, found := strings.Cut(s.buffer, "\n")
			if !found {
				break
			}
			trimmed := strings.TrimSpace(before)
			if trimmed == "```" {
				// closing fence
				var codeText strings.Builder
				for _, codeLine := range s.codeLines {
					codeText.WriteString(RenderCodeLine(codeLine))
					codeText.WriteString("\n")
				}
				lines = append(lines, strings.TrimSuffix(codeText.String(), "\n"))
				s.codeLines = nil
				lines = append(lines, "")
				s.inCodeBlock = false
				s.buffer = after
				continue
			}
			s.codeLines = append(s.codeLines, before)
			s.buffer = after
		} else {
			// check for code start
			before, after, found := strings.Cut(s.buffer, "\n")
			if !found {
				break
			}
			trimmed := strings.TrimSpace(before)
			if trimmed == "```" {
				// opening fence
				s.inCodeBlock = true
				s.buffer = after
				continue
			}
			// put back
			s.buffer = before + "\n" + after
			// check for paragraph
			before, after, found = strings.Cut(s.buffer, "\n\n")
			if !found {
				break
			}
			text := MarkdownToIRCStream(before)
			lines = append(lines, text)
			s.buffer = after
		}
	}
	if delta == "" && s.buffer != "" {
		if s.inCodeBlock {
			var codeText strings.Builder
			for _, codeLine := range s.codeLines {
				codeText.WriteString(RenderCodeLine(codeLine))
				codeText.WriteString("\n")
			}
			lines = append(lines, strings.TrimSuffix(codeText.String(), "\n"))
			s.buffer = ""
		} else {
			text := MarkdownToIRCStream(s.buffer)
			lines = append(lines, text)
			s.buffer = ""
		}
	}
	return lines
}
