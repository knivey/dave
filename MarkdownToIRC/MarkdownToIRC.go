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
}

var colorRE = regexp.MustCompile("\x03(\\d\\d)?(,\\d\\d)?")

// only expected to be good for our syntax formatter as we know it outputs nothing too crazy
func stripIRCCodes(line string) (out string) {
	out = strings.ReplaceAll(line, "\x02", "")
	out = strings.ReplaceAll(out, "\x1D", "")
	out = strings.ReplaceAll(out, "\x1F", "")
	out = colorRE.ReplaceAllLiteralString(out, "")
	return
}

func makeIndents(node ast.Node) (out string) {
	var prevWasQuote bool
	for n := node; ; n = n.GetParent() {
		switch n.(type) {
		case *ast.Document:
			return
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
}

func writes(w io.Writer, node ast.Node, text string) {
	for _, v := range text {
		if v == '\n' {
			fmt.Fprint(w, "\n"+makeIndents(node))
		} else {
			fmt.Fprint(w, string(v))
		}
	}
}

func writesWithNegOffset(w io.Writer, node ast.Node, text string, negativeOffset int) {
	for _, v := range text {
		if v == '\n' {
			indent := makeIndents(node)
			off := max(utf8.RuneCountInString(indent)-negativeOffset, 0)
			trimmed := string([]rune(indent)[:off])
			fmt.Fprint(w, "\n"+trimmed)
		} else {
			fmt.Fprint(w, string(v))
		}
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
			iterator, _ := lexer.Tokenise(nil, text)
			formatter.Format(&lineBuffer, style, iterator)

			max := 0
			lines := strings.Split(lineBuffer.String(), "\n")
			for len(lines) > 0 && stripIRCCodes(lines[0]) == "" {
				lines = lines[1:]
			}
			for len(lines) > 0 && stripIRCCodes(lines[len(lines)-1]) == "" {
				lines = lines[:len(lines)-1]
			}
			for _, v := range lines {
				v = colorCancelRE.ReplaceAllLiteralString(v, "\x0300")
				v = stripIRCCodes(v)
				if max < utf8.RuneCountInString(v) {
					max = utf8.RuneCountInString(v)
				}
			}
			var outs []string

			for _, v := range lines {
				//prevent clearing our background
				v := colorCancelRE.ReplaceAllLiteralString(v, "\x0300")
				rpad := strings.Repeat(" ", max-utf8.RuneCountInString(stripIRCCodes(v)))
				outs = append(outs, fmt.Sprintf(" \x030,90%s%s\x03 ", v, rpad))
			}
			if len(outs) > 0 {
				writes(w, node, strings.Join(outs, "\n"))
			}
		} else {
			max := 0
			text := strings.ReplaceAll(string(node.Literal), "\t", "        ")
			lines := strings.Split(text, "\n")
			for len(lines) > 0 && lines[0] == "" {
				lines = lines[1:]
			}
			for len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			for _, v := range lines {
				if max < utf8.RuneCountInString(v) {
					max = utf8.RuneCountInString(v)
				}
			}
			var outs []string
			for _, v := range lines {
				outs = append(outs, fmt.Sprintf(" \x030,90%-*s\x03 ", max, v))
			}
			if len(outs) > 0 {
				writes(w, node, strings.Join(outs, "\n"))
			}
		}
	case *ast.List:
		if entering {
			start := node.Start
			if node.ListFlags&ast.ListTypeOrdered != 0 && start == 0 {
				start = r.lastCounter
			}
			r.listIdx = append(r.listIdx, start)
		} else {
			if len(r.listIdx) > 0 {
				r.lastCounter = r.listIdx[len(r.listIdx)-1]
			}
			r.listIdx = r.listIdx[:len(r.listIdx)-1]
		}
	case *ast.ListItem:
		if entering {
			var lead string
			if node.ListFlags&ast.ListTypeOrdered != 0 {
				r.listIdx[len(r.listIdx)-1]++
				lead = fmt.Sprintf("%d%s ", r.listIdx[len(r.listIdx)-1], string(node.Delimiter))
			} else {
				lead = " \u2022 " //"•"
			}
			writesWithNegOffset(w, node, "\n"+lead, utf8.RuneCountInString(lead))
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

func MarkdownToIRC(response string) string {
	p := parser.New()
	doc := p.Parse([]byte(response))
	//	fmt.Println(ast.ToString(doc))
	return string(markdown.Render(doc, &Renderer{}))
}
