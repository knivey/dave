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
	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/parser"
)

type Renderer struct {
	listIdx     []int
	lastCounter int
	lastWasCode bool
	// lastCounter and lastWasCode work around two gomarkdown bugs:
	// 1. gomarkdown always sets List.Start=0 regardless of the markdown number (e.g. "2." becomes start=0)
	// 2. gomarkdown splits a single logical list into multiple List nodes when a code block appears
	//    between list items, even without a blank line. The code block is parsed as a Document-level
	//    sibling, not as part of the list item.
	// We track the last counter value and whether a top-level code block was just rendered, so we
	// can continue numbering when a new List node with start=0 appears after a code block.
}

var colorRE = regexp.MustCompile("\x03(\\d\\d)?(,\\d\\d)?")

// only expected to be good for our syntax formatter as we know it outputs nothing too crazy
func stripIRCCodes(line string) string {
	out := strings.ReplaceAll(line, "\x02", "")
	out = strings.ReplaceAll(out, "\x1D", "")
	out = strings.ReplaceAll(out, "\x1F", "")
	out = colorRE.ReplaceAllLiteralString(out, "")
	return out
}

func makeIndents(node ast.Node) string {
	var out string
	var prevWasQuote bool
	for n := node; n != nil; n = n.GetParent() {
		switch n.(type) {
		case *ast.Document:
			return out
		case *ast.List:
			out = "   " + out
			prevWasQuote = false
		case *ast.BlockQuote:
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

func (r *Renderer) RenderNode(w io.Writer, node ast.Node, entering bool) ast.WalkStatus {
	switch node := node.(type) {
	case *ast.Strong:
		writes(w, node, "\x02")
	case *ast.Emph:
		writes(w, node, "\x1D")
	case *ast.Hardbreak:
		writes(w, node, "\n")
	case *ast.Softbreak:
		writes(w, node, " ")
	case *ast.Heading:
		if entering {
			writes(w, node, "\n\x02")
		} else {
			writes(w, node, "\x02")
		}
	case *ast.Paragraph:
		if _, ok := node.GetParent().(*ast.ListItem); ok {
			return ast.GoToNext
		}
		if entering {
			writes(w, node, "\n")
		}
	case *ast.Code:
		writes(w, node, fmt.Sprintf("\x030,90%s\x03", string(node.Literal)))
	case *ast.CodeBlock:
		if _, ok := node.GetParent().(*ast.ListItem); !ok {
			r.lastWasCode = true
		}
		writes(w, node, "\n")
		lexer := lexers.Get(string(node.Info))
		if lexer != nil {
			style := styles.Get("github-dark")
			if style == nil {
				style = styles.Fallback
			}
			var lineBuffer bytes.Buffer
			formatter := IRC16
			text := strings.ReplaceAll(string(node.Literal), "\t", "        ")
			iterator, err := lexer.Tokenise(nil, text)
			if err == nil {
				formatter.Format(&lineBuffer, style, iterator)
				writeHighlightedCodeBlock(w, node, lineBuffer.String())
				return ast.GoToNext
			}
		}
		writePlainCodeBlock(w, node, string(node.Literal))
	case *ast.List:
		if entering {
			start := node.Start
			if node.ListFlags&ast.ListTypeOrdered != 0 && start == 0 {
				if r.lastWasCode {
					start = r.lastCounter
				}
			}
			r.listIdx = append(r.listIdx, start)
		} else {
			if len(r.listIdx) > 0 {
				r.lastCounter = r.listIdx[len(r.listIdx)-1]
			}
			r.listIdx = r.listIdx[:len(r.listIdx)-1]
			r.lastWasCode = false
		}
	case *ast.ListItem:
		if entering {
			var lead string
			if node.ListFlags&ast.ListTypeOrdered != 0 {
				r.listIdx[len(r.listIdx)-1]++
				lead = fmt.Sprintf("%d%s ", r.listIdx[len(r.listIdx)-1], string(node.Delimiter))
			} else {
				lead = " \u2022 "
			}
			writesWithNegOffset(w, node, "\n"+lead, utf8.RuneCountInString(lead))
		}
	case *ast.Link:
		if !entering {
			writes(w, node, fmt.Sprintf(" (%s)", node.Destination))
		}
	case *ast.Image:
		if entering {
			writes(w, node, "[image: ")
		} else {
			writes(w, node, fmt.Sprintf("](%s)", node.Destination))
		}
	case *ast.HorizontalRule:
		if entering {
			writes(w, node, "\n"+strings.Repeat("-", 40))
		}
	default:
		if leaf := node.AsLeaf(); leaf != nil {
			writes(w, node, strings.TrimRight(string(leaf.Literal), "\n"))
		}
	}
	return ast.GoToNext
}

func (r *Renderer) RenderHeader(w io.Writer, ast ast.Node) {

}

func (r *Renderer) RenderFooter(w io.Writer, ast ast.Node) {

}

func writeHighlightedCodeBlock(w io.Writer, node ast.Node, highlighted string) {
	lines := strings.Split(highlighted, "\n")
	for len(lines) > 0 && stripIRCCodes(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && stripIRCCodes(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	maxWidth := 0
	for _, v := range lines {
		v = colorCancelRE.ReplaceAllLiteralString(v, "\x0300")
		v = stripIRCCodes(v)
		if maxWidth < utf8.RuneCountInString(v) {
			maxWidth = utf8.RuneCountInString(v)
		}
	}
	var outs []string
	for _, v := range lines {
		v := colorCancelRE.ReplaceAllLiteralString(v, "\x0300")
		rpad := strings.Repeat(" ", maxWidth-utf8.RuneCountInString(stripIRCCodes(v)))
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
	var outs []string
	for _, v := range lines {
		outs = append(outs, fmt.Sprintf(" \x030,90%-*s\x03 ", maxWidth, v))
	}
	if len(outs) > 0 {
		writes(w, node, strings.Join(outs, "\n"))
	}
}

func MarkdownToIRC(response string) string {
	p := parser.New()
	doc := p.Parse([]byte(response))
	//	fmt.Println(ast.ToString(doc))
	return string(markdown.Render(doc, &Renderer{}))
}
