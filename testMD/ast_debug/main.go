package main

import (
	"fmt"
	"strings"

	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/parser"
)

func main() {
	cases := map[string]string{
		"Normal ordered list": `1. First item
2. Second item
3. Third item`,

		"List split by code block (BUG)": `1. Simple echo example
` + "```bash" + `
echo hello
` + "```" + `

2. Echo with variable
` + "```bash" + `
echo bye
` + "```",

		"List split by code block no blank line (BUG)": `1. Simple echo example
` + "```bash" + `
echo hello
` + "```" + `
2. Echo with variable
` + "```bash" + `
echo bye
` + "```",

		"Separate lists (both get start=0)": `1. First list item
2. Second list item

Some paragraph text.

1. Second list first item
2. Second list second item`,

		"Blockquote with list after paragraph (BUG)": `> ### Nested Quote Heading
>
> This blockquote includes:
> - A list item
> - Another item with a sub-list:
>   - Sub-list item
>   - Another sub-list item
>
> > Nested blockquote inside a blockquote`,

		"List item with inline code": `- use ` + "`ip route add`" + ` command to exclude specific IPs. Example:
  ` + "```" + `
  sudo ip route add <IP Address> via <Gateway IP>
  ` + "```",
	}

	for name, md := range cases {
		fmt.Println(strings.Repeat("=", 70))
		fmt.Printf("CASE: %s\n", name)
		fmt.Println(strings.Repeat("=", 70))
		fmt.Printf("Input:\n%s\n\n", md)
		fmt.Println("AST:")
		p := parser.New()
		doc := p.Parse([]byte(md))
		printAST(doc, 0)
		fmt.Println()
	}
}

func printAST(node ast.Node, indent int) {
	prefix := strings.Repeat("  ", indent)
	switch n := node.(type) {
	case *ast.Text:
		text := string(n.Literal)
		if len(text) > 60 {
			text = text[:57] + "..."
		}
		fmt.Printf("%sText: %q\n", prefix, text)
	case *ast.List:
		fmt.Printf("%sList: start=%d flags=%v tight=%v\n", prefix, n.Start, n.ListFlags, n.Tight)
	case *ast.ListItem:
		fmt.Printf("%sListItem: flags=%v\n", prefix, n.ListFlags)
	case *ast.CodeBlock:
		lang := string(n.Info)
		if lang == "" {
			lang = "(none)"
		}
		fmt.Printf("%sCodeBlock: lang=%q\n", prefix, lang)
	case *ast.Code:
		fmt.Printf("%sCode: %q\n", prefix, string(n.Literal))
	case *ast.BlockQuote:
		fmt.Printf("%sBlockQuote\n", prefix)
	case *ast.Heading:
		fmt.Printf("%sHeading: level=%d\n", prefix, n.Level)
	case *ast.Paragraph:
		fmt.Printf("%sParagraph\n", prefix)
	case *ast.Strong:
		fmt.Printf("%sStrong\n", prefix)
	case *ast.Emph:
		fmt.Printf("%sEmph\n", prefix)
	case *ast.Document:
		fmt.Printf("%sDocument\n", prefix)
	default:
		fmt.Printf("%s%T\n", prefix, node)
	}
	for _, child := range node.GetChildren() {
		printAST(child, indent+1)
	}
}
