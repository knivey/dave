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
	listIdx []int
}

// why isnt this a built in?
func Sum(numbers ...int) (total int) {
	for _, v := range numbers {
		total += v
	}
	return
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

var writeDebug = false

func writes(w io.Writer, node ast.Node, text string) {
	if writeDebug {
		fmt.Fprintf(w, "[%T]", node)
	}
	for _, v := range text {
		if v == '\n' {
			fmt.Fprint(w, "\n"+makeIndents(node))
		} else {
			fmt.Fprint(w, string(v))
		}
	}
	if writeDebug {
		fmt.Fprintf(w, "[/%T]", node)
	}
}

func writesWithNegOffset(w io.Writer, node ast.Node, text string, negativeOffset int) {
	if writeDebug {
		fmt.Fprintf(w, "[[%T]]", node)
	}
	for _, v := range text {
		if v == '\n' {
			indent := makeIndents(node)
			off := max(len(indent)-negativeOffset, 0)
			fmt.Fprint(w, "\n"+indent[:off])
		} else {
			fmt.Fprint(w, string(v))
		}
	}
	if writeDebug {
		fmt.Fprintf(w, "[[/%T]]", node)
	}
}

func (r *Renderer) RenderNode(w io.Writer, node ast.Node, entering bool) ast.WalkStatus {
	switch node := node.(type) {
	case *ast.Strong:
		writes(w, node, "\x02")
	case *ast.Emph:
		writes(w, node, "\x1D")
	case *ast.Hardbreak:
		writes(w, node, "\n")
	case *ast.Heading:
		writes(w, node, "\n\x02")
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
			iterator, _ := lexer.Tokenise(nil, string(node.Literal))
			formatter.Format(&lineBuffer, style, iterator)

			max := 0
			lines := strings.Split(lineBuffer.String(), "\n")
			for _, v := range lines {
				v = stripIRCCodes(v)
				if max < utf8.RuneCountInString(v) {
					max = utf8.RuneCountInString(v)
				}
			}
			var outs []string

			for _, v := range lines {
				rpad := strings.Repeat(" ", max-utf8.RuneCountInString(stripIRCCodes(v)))
				outs = append(outs, fmt.Sprintf(" \x030,90%s%s\x03 ", v, rpad))
			}
			writes(w, node, strings.Join(outs[:len(outs)-1], "\n"))
		} else {
			max := 0
			lines := strings.Split(string(node.Literal), "\n")
			for _, v := range lines {
				if max < utf8.RuneCountInString(v) {
					max = utf8.RuneCountInString(v)
				}
			}
			var outs []string
			for _, v := range lines {
				outs = append(outs, fmt.Sprintf(" \x0315,90%-*s\x03 ", max, v))
			}
			writes(w, node, strings.Join(outs[:len(outs)-1], "\n"))
		}
	case *ast.List:
		if entering {
			r.listIdx = append(r.listIdx, node.Start)
		} else {
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
		} else {
			// For IRC lets keep them all tight to minimize wasted lines
			//seems ListItem.Tight doesnt get set but parent does
			//if n, ok := node.GetParent().(*ast.List); ok && !n.Tight {
			//writes(w, node, "\n")
			//}
		}
	default:
		if leaf := node.AsLeaf(); leaf != nil {
			writes(w, node, string(leaf.Literal))
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
