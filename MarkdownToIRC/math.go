package markdowntoirc

// LaTeX math extension for goldmark: $...$, \(...\), \[...\] inline math and
// $$...$$ math blocks, all rendered to single-line Unicode by termtex (see
// writeMath in MarkdownToIRC.go for the rendering decisions).
//
// ATTRIBUTION: the inline delimiter rules and the outline of both inline
// scans are adapted from github.com/doug/termtex/goldmark (Apache License
// 2.0).
//
// DESIGN NOTE — why our own parsers instead of termtex's goldmark
// extension: termtex's math BLOCK parser calls reader.AdvanceLine() in Open
// and in Continue for lines that the goldmark parseBlocks framework ALSO
// advances (verified against goldmark v1.8.2, termtex
// v0.0.0-20260918165034-c407d5cf5251): every multi-line $$ block loses its
// first content line and every second line after — "$$\na = b\nb = c\nc =
// d\n$$" keeps only "b = c", "$$\np \rightarrow q\n$$" parses EMPTY. The
// lost lines are not in the AST at all, so no render-layer fallback can
// recover them. This file therefore re-implements the block parser on
// goldmark's own fenced-code-block reader contract (parser/fcode_block.go):
// Open never advances (the framework's AdvanceLine after a successful open
// consumes the opener line); Continue consumes a line with AdvanceToEOL
// ONLY and lets the framework step the newline. termtex itself is used
// solely for termtex.Render(expr, Style{Inline: true}).

import (
	"bytes"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// ---------------------------------------------------------------------------
// AST nodes
// ---------------------------------------------------------------------------

var (
	// KindMathInline is the AST node kind for inline math ($...$, \(...\),
	// \[...\]).
	KindMathInline = ast.NewNodeKind("MathInline")
	// KindMathBlock is the AST node kind for display math blocks ($$...$$).
	KindMathBlock = ast.NewNodeKind("MathBlock")
)

// mathInline is an inline math expression. We render display and inline
// math identically (single-line), so all three delimiter styles produce
// this node type.
type mathInline struct {
	ast.BaseInline
	Expression []byte
}

func (n *mathInline) Kind() ast.NodeKind { return KindMathInline }
func (n *mathInline) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

// mathBlock is a display math block. Multi-line content accumulates in the
// BaseBlock's own segment list (the fenced-code-block pattern) and is
// joined into Expression when the parser's Close runs. A single-line
// $$...$$ block is complete at Open. For a multi-line block whose opener
// line carries a tail after the leading $$, openerTail stashes that tail —
// it is the block's FIRST CONTENT LINE (Pandoc semantics), prepended by
// Close.
type mathBlock struct {
	ast.BaseBlock
	Expression []byte
	complete   bool // single-line $$...$$: Expression is final at Open
	openerTail []byte
}

func (n *mathBlock) Kind() ast.NodeKind { return KindMathBlock }
func (n *mathBlock) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, nil, nil)
}

// IsRaw implements ast.Node.IsRaw: the accumulated Lines() are consumed by
// the block parser's Close (joined into Expression) and must NOT also be
// inline-parsed into Text children — the fenced-code-block pattern (without
// this, goldmark parses the math source as markdown prose and the renderer
// emits the expression AND the raw lines).
func (n *mathBlock) IsRaw() bool { return true }

// ---------------------------------------------------------------------------
// inline $...$ parser
// ---------------------------------------------------------------------------

type mathDollarParser struct{}

func (p *mathDollarParser) Trigger() []byte { return []byte{'$'} }

// Parse implements the Pandoc TeX-math delimiter rules (adapted from
// termtex/goldmark, Apache 2.0 — see the file attribution comment):
//
//  1. the opening $ must have a non-space to its right;
//  2. the closing $ must have a non-space to its left;
//  3. the closing $ must not be followed by an ASCII digit;
//  4. $$ is skipped (display math is the block parser's job);
//  5. a backslash escapes the next byte, so \$ inside cannot close;
//  6. an empty trimmed expression is not math.
//
// Rules 2 and 3 together keep "$5 and $10" prose; rule 1 rejects the
// "$ ... $" typo shape.
func (p *mathDollarParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if len(line) < 2 || line[0] != '$' {
		return nil
	}
	if line[1] == '$' { // rule 4
		return nil
	}
	if isMathSpace(line[1]) { // rule 1
		return nil
	}
	for i := 2; i < len(line); i++ {
		if line[i] == '\\' && i+1 < len(line) { // rule 5
			i++
			continue
		}
		if line[i] != '$' {
			continue
		}
		if isMathSpace(line[i-1]) { // rule 2
			continue
		}
		if i+1 < len(line) && line[i+1] >= '0' && line[i+1] <= '9' { // rule 3
			continue
		}
		expr := line[1:i]
		if len(bytes.TrimSpace(expr)) == 0 { // rule 6
			return nil
		}
		node := &mathInline{Expression: bytes.Clone(expr)}
		block.Advance(i + 1)
		return node
	}
	return nil
}

func isMathSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// ---------------------------------------------------------------------------
// inline \(...\) and \[...\] parser
// ---------------------------------------------------------------------------

type mathTeXParser struct{}

func (p *mathTeXParser) Trigger() []byte { return []byte{'\\'} }

// Parse recognizes KaTeX/Pandoc TeX delimiters: \(...\) inline and \[...\]
// display — both become mathInline (we render the two identically:
// single-line). No delimiter-ambiguity rules are needed, just a literal
// closer search on the same line. Every decline returns nil BEFORE
// advancing, so goldmark's regular backslash handling still applies to
// everything else (a lone \(, \\, \*, ...). Registered at priority 100 to
// win the backslash byte for \( (termtex used the same value). Outline
// adapted from termtex/goldmark, Apache 2.0.
func (p *mathTeXParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if len(line) < 4 || line[0] != '\\' {
		return nil
	}
	var closer string
	switch line[1] {
	case '(':
		closer = `\)`
	case '[':
		closer = `\]`
	default:
		return nil
	}
	rest := line[2:]
	pos := bytes.Index(rest, []byte(closer))
	if pos < 0 {
		return nil
	}
	expr := rest[:pos]
	if len(bytes.TrimSpace(expr)) == 0 {
		return nil
	}
	node := &mathInline{Expression: bytes.Clone(expr)}
	block.Advance(2 + pos + 2)
	return node
}

// ---------------------------------------------------------------------------
// $$...$$ block parser
// ---------------------------------------------------------------------------

type mathBlockParser struct{}

func (p *mathBlockParser) Trigger() []byte { return []byte{'$'} }

// Open matches a line whose TRIMMED content starts with $$ (the rule the
// streaming settle classifier mirrors). If the rest of the line after the
// leading $$ contains another $$ it is a single-line block and Expression
// is final here — UNLESS a non-whitespace tail follows the closing $$: then
// Open DECLINES (returns nil, never advancing) so the whole line parses as
// a paragraph and the inline $..$ parser renders the math while the tail
// survives as prose (degradation equals the pre-math behavior; the
// framework's AdvanceLine after a successful open would otherwise eat the
// tail — silent loss). A whitespace-only tail still takes the block path.
// Otherwise the block accumulates lines until a $$-prefixed line closes it
// (the closer consumes exactly the $$ run, re-opening its tail as prose —
// see Continue). Per the fenced-code-block contract Open does NOT consume
// the line: the framework's AdvanceLine after a successful open does.
// (termtex's parser advances here AND the framework advances again — that
// double-advance eats the first content line; see the file comment.)
// The opener line's tail after the leading $$ is CONTENT — Pandoc
// display-math semantics: it is stashed on the node and Close prepends it
// as the first content line. Fenced-code info-string semantics (which
// termtex's parser follows) would silently drop equation text — one more
// reason we diverge from termtex's parser here. An empty/whitespace-only
// tail stashes nothing (the canonical bare $$ opener is unchanged).
func (p *mathBlockParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("$$")) {
		return nil, parser.NoChildren
	}
	rest := trimmed[2:]
	if end := bytes.Index(rest, []byte("$$")); end >= 0 {
		if len(bytes.TrimSpace(rest[end+2:])) > 0 {
			// non-whitespace tail after the closing $$: decline to the
			// inline path — the tail is prose, never dropped
			return nil, parser.NoChildren
		}
		// single-line $$...$$ block: complete on the opener line
		node := &mathBlock{
			Expression: bytes.Clone(bytes.TrimSpace(rest[:end])),
			complete:   true,
		}
		return node, parser.NoChildren
	}
	if tail := bytes.TrimSpace(rest); len(tail) > 0 {
		return &mathBlock{openerTail: bytes.Clone(tail)}, parser.NoChildren
	}
	return &mathBlock{}, parser.NoChildren
}

// Continue follows goldmark's fenced-code-block contract exactly: a line
// whose trimmed content starts with $$ closes the block, consuming EXACTLY
// the $$ delimiter run (plus any leading whitespace) — NOT the whole line:
// after Close returns, the framework re-runs openBlocks on the unconsumed
// remainder (parser.go parseBlocks), so the closer line's tail re-opens as
// a new block (paragraph) and renders as prose — Pandoc semantics, the
// closer-tail analogue of the opener-tail rule; AdvanceToEOL here would
// silently drop that text. Every other line — blanks included — is
// expression content, appended as a segment and consumed with
// AdvanceToEOL ONLY. Never AdvanceLine here: the framework advances past
// the newline itself, and double-advancing is what dropped every second
// content line in termtex's parser.
func (p *mathBlockParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	mb := node.(*mathBlock)
	if mb.complete {
		// Single-line block was finished at Open; this line belongs to
		// whatever opens next (the framework re-runs openBlocks on it).
		return parser.Close
	}
	line, segment := reader.PeekLine()
	trimmed := bytes.TrimSpace(line)
	if bytes.HasPrefix(trimmed, []byte("$$")) {
		// line[i:] starts at the delimiter run: everything before the
		// first $ is whitespace (trimmed starts with $$).
		i := bytes.IndexByte(line, '$')
		for i < len(line) && line[i] == '$' {
			i++
		}
		reader.Advance(i)
		return parser.Close
	}
	seg := segment
	seg.ForceNewline = true // EOF as newline (fenced-code pattern)
	mb.Lines().Append(seg)
	reader.AdvanceToEOL()
	return parser.Continue | parser.NoChildren
}

// Close joins the accumulated content lines into Expression: each segment
// right-trimmed, lines joined with \n, the result trimmed overall (math has
// no significant whitespace). A stashed opener-line tail is prepended as
// the FIRST content line (Pandoc semantics — see Open). Close runs for
// terminator-closed AND EOF-terminated blocks, so an unclosed block still
// renders everything it accumulated — no line is ever dropped.
func (p *mathBlockParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {
	mb := node.(*mathBlock)
	if mb.complete {
		return // Expression set at Open
	}
	source := reader.Source()
	lines := mb.Lines()
	parts := make([][]byte, 0, lines.Len()+1)
	if len(mb.openerTail) > 0 {
		parts = append(parts, mb.openerTail)
	}
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		parts = append(parts, util.TrimRightSpace(seg.Value(source)))
	}
	mb.Expression = bytes.TrimSpace(bytes.Join(parts, []byte("\n")))
}

func (p *mathBlockParser) CanInterruptParagraph() bool { return true }
func (p *mathBlockParser) CanAcceptIndentedLine() bool { return false }

// ---------------------------------------------------------------------------
// wiring
// ---------------------------------------------------------------------------

// mathExtension registers the math parsers. It deliberately registers NO
// renderer: rendering happens in Renderer's own node switches
// (renderNode/renderNodeTo) — Renderer.AddOptions is a no-op anyway.
type mathExtension struct{}

func (e *mathExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithBlockParsers(
			util.Prioritized(&mathBlockParser{}, 500),
		),
		parser.WithInlineParsers(
			util.Prioritized(&mathDollarParser{}, 500),
			// priority 100 beats goldmark's backslash handling for \(
			util.Prioritized(&mathTeXParser{}, 100),
		),
	)
}
