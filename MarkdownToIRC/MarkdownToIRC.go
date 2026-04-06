package markdowntoirc

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
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

var colorCancelRE = regexp.MustCompile("\x03(?:\\D)")

func (r *Renderer) Render(w io.Writer, source []byte, n ast.Node) error {
	r.source = source
	return ast.Walk(n, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		return r.renderNode(w, node, entering), nil
	})
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
			}
		}
	case extast.KindTable:
		if entering {
			renderTable(w, node, r)
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
	for len(lines) > 0 && StripCodes(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && StripCodes(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	maxWidth := 0
	for _, v := range lines {
		v = colorCancelRE.ReplaceAllLiteralString(v, "\x0300")
		v = StripCodes(v)
		if maxWidth < utf8.RuneCountInString(v) {
			maxWidth = utf8.RuneCountInString(v)
		}
	}
	padWidth := min(maxWidth, maxCodeBlockPadWidth)
	var outs []string
	for _, v := range lines {
		v := colorCancelRE.ReplaceAllLiteralString(v, "\x0300")
		rpad := strings.Repeat(" ", max(padWidth-utf8.RuneCountInString(StripCodes(v)), 0))
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

func renderTableCellContent(r *Renderer, cell *extast.TableCell) string {
	var buf bytes.Buffer
	for c := cell.FirstChild(); c != nil; c = c.NextSibling() {
		r.renderNodeTo(&buf, c)
	}
	return strings.TrimSpace(strings.ReplaceAll(buf.String(), "\\|", "|"))
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

const maxTableCellWidth = 40

func renderTable(w io.Writer, table ast.Node, r *Renderer) {
	type cellData struct {
		text  string
		align extast.Alignment
		isHdr bool
	}

	var rows [][]cellData
	var headerRowIdx = -1

	for section := table.FirstChild(); section != nil; section = section.NextSibling() {
		isHeader := section.Kind() == extast.KindTableHeader
		if isHeader && headerRowIdx == -1 {
			headerRowIdx = len(rows)
		}
		if isHeader {
			var rowData []cellData
			for cellNode := section.FirstChild(); cellNode != nil; cellNode = cellNode.NextSibling() {
				if cellNode.Kind() != extast.KindTableCell {
					continue
				}
				cell := cellNode.(*extast.TableCell)
				text := renderTableCellContent(r, cell)
				rowData = append(rowData, cellData{
					text:  text,
					align: cell.Alignment,
					isHdr: true,
				})
			}
			if len(rowData) > 0 {
				rows = append(rows, rowData)
			}
		} else if section.Kind() == extast.KindTableRow {
			var rowData []cellData
			for cellNode := section.FirstChild(); cellNode != nil; cellNode = cellNode.NextSibling() {
				if cellNode.Kind() != extast.KindTableCell {
					continue
				}
				cell := cellNode.(*extast.TableCell)
				text := renderTableCellContent(r, cell)
				rowData = append(rowData, cellData{
					text:  text,
					align: cell.Alignment,
					isHdr: false,
				})
			}
			if len(rowData) > 0 {
				rows = append(rows, rowData)
			}
		}
	}

	if len(rows) == 0 {
		return
	}

	numCols := 0
	for _, row := range rows {
		if len(row) > numCols {
			numCols = len(row)
		}
	}
	if numCols == 0 {
		return
	}

	colWidths := make([]int, numCols)
	for _, row := range rows {
		for i, cell := range row {
			lines := strings.Split(cell.text, "\n")
			for _, line := range lines {
				w := utf8.RuneCountInString(StripCodes(line))
				if w > colWidths[i] {
					colWidths[i] = min(w, maxTableCellWidth)
				}
			}
		}
	}

	var border strings.Builder
	border.WriteString("+")
	for _, cw := range colWidths {
		border.WriteString(strings.Repeat("-", cw+2))
		border.WriteString("+")
	}
	borderStr := border.String()

	var lines []string
	lines = append(lines, borderStr)

	for ri, row := range rows {
		if ri > 0 && ri == headerRowIdx+1 {
			lines = append(lines, borderStr)
		}

		var cellLines [][]string
		maxCellLines := 1

		for ci := 0; ci < numCols; ci++ {
			var text string
			if ci < len(row) {
				text = row[ci].text
			}
			cw := colWidths[ci]
			wrapped := wrapCellText(text, cw)
			cellLines = append(cellLines, wrapped)
			if len(wrapped) > maxCellLines {
				maxCellLines = len(wrapped)
			}
		}

		for li := 0; li < maxCellLines; li++ {
			var rowLine strings.Builder
			rowLine.WriteString("|")
			for ci := 0; ci < numCols; ci++ {
				var line string
				var align extast.Alignment
				if ci < len(row) {
					align = row[ci].align
				}
				if li < len(cellLines[ci]) {
					line = cellLines[ci][li]
				}
				cw := colWidths[ci]
				plainW := utf8.RuneCountInString(StripCodes(line))

				var padded string
				switch align {
				case extast.AlignRight:
					padded = strings.Repeat(" ", cw-plainW) + line
				case extast.AlignCenter:
					left := (cw - plainW) / 2
					right := cw - plainW - left
					padded = strings.Repeat(" ", left) + line + strings.Repeat(" ", right)
				default:
					padded = line + strings.Repeat(" ", cw-plainW)
				}

				rowLine.WriteString(" " + padded + " |")
			}
			lines = append(lines, rowLine.String())
		}
	}

	lines = append(lines, borderStr)

	writes(w, table, "\n"+strings.Join(lines, "\n"))
}

func wrapCellText(text string, maxWidth int) []string {
	if text == "" {
		return []string{""}
	}

	var allLines []string
	for _, segment := range strings.Split(text, "\n") {
		if segment == "" {
			allLines = append(allLines, "")
			continue
		}
		wrapped := WordWrap(segment, maxWidth)
		allLines = append(allLines, wrapped...)
	}

	if len(allLines) == 0 {
		allLines = []string{""}
	}
	return allLines
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
