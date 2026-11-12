// irc.go — goldmark AST transformer mapping IRC formatting control codes
// (\x02 bold, \x1d italic, \x1f underline, \x1e strike, \x11 monospace,
// \x16 reverse, \x0f reset, \x03 fg[,bg] 0-99, \x04 hex) to HTML spans.
// fg==bg renders as a "spoiler" class. Ported from markdown-parsebin.

package renderer

import (
	"strconv"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

var ircFgColors = map[int]string{
	0:  "#ffffff",
	1:  "#000000",
	2:  "#000080",
	3:  "#008000",
	4:  "#ff0000",
	5:  "#800000",
	6:  "#800080",
	7:  "#ff6600",
	8:  "#ffff00",
	9:  "#00ff00",
	10: "#008080",
	11: "#00ffff",
	12: "#4169e1",
	13: "#ff00ff",
	14: "#808080",
	15: "#c0c0c0",
	16: "#470000", 17: "#472100", 18: "#474700", 19: "#324700",
	20: "#004700", 21: "#00472c", 22: "#004747", 23: "#002747",
	24: "#000047", 25: "#2e0047", 26: "#470047", 27: "#47002a",
	28: "#740000", 29: "#743a00", 30: "#747400", 31: "#517400",
	32: "#007400", 33: "#007449", 34: "#007474", 35: "#004074",
	36: "#000074", 37: "#4b0074", 38: "#740074", 39: "#740045",
	40: "#b50000", 41: "#b56300", 42: "#b5b500", 43: "#7db500",
	44: "#00b500", 45: "#00b571", 46: "#00b5b5", 47: "#0063b5",
	48: "#0000b5", 49: "#7500b5", 50: "#b500b5", 51: "#b5006b",
	52: "#ff0000", 53: "#ff8c00", 54: "#ffff00", 55: "#b2ff00",
	56: "#00ff00", 57: "#00ffa0", 58: "#00ffff", 59: "#008cff",
	60: "#0000ff", 61: "#a500ff", 62: "#ff00ff", 63: "#ff0098",
	64: "#ff5959", 65: "#ffb459", 66: "#ffff71", 67: "#cfff60",
	68: "#6fff6f", 69: "#65ffc9", 70: "#6dffff", 71: "#59b4ff",
	72: "#5959ff", 73: "#c459ff", 74: "#ff66ff", 75: "#ff59bc",
	76: "#ff9c9c", 77: "#ffd39c", 78: "#ffff9c", 79: "#e2ff9c",
	80: "#9cff9c", 81: "#9cffdb", 82: "#9cffff", 83: "#9cd3ff",
	84: "#9c9cff", 85: "#dc9cff", 86: "#ff9cff", 87: "#ff94d3",
	88: "#000000", 89: "#131313", 90: "#282828", 91: "#363636",
	92: "#4d4d4d", 93: "#656565", 94: "#818181", 95: "#9f9f9f",
	96: "#bcbcbc", 97: "#e2e2e2", 98: "#ffffff",
}

type ircState struct {
	bold, italic, underline, strikethrough, monospace, reverse bool
	fg, bg                                                     int
}

func newIRCState() ircState {
	return ircState{fg: -1, bg: -1}
}

func (s ircState) classes() []string {
	var cls []string
	if s.bold {
		cls = append(cls, "irc-bold")
	}
	if s.italic {
		cls = append(cls, "irc-italic")
	}
	if s.underline {
		cls = append(cls, "irc-underline")
	}
	if s.strikethrough {
		cls = append(cls, "irc-strikethrough")
	}
	if s.monospace {
		cls = append(cls, "irc-monospace")
	}

	fg := s.fg
	bg := s.bg
	if s.reverse {
		fg, bg = bg, fg
	}

	if fg >= 0 && fg <= 98 {
		if fg == bg && bg >= 0 {
			cls = append(cls, "irc-spoiler")
		} else {
			cls = append(cls, ircFgClass(fg))
		}
	}
	if bg >= 0 && bg <= 98 && fg != bg {
		cls = append(cls, ircBgClass(bg))
	}

	return cls
}

func (s ircState) hasFormatting() bool {
	return s.bold || s.italic || s.underline || s.strikethrough || s.monospace || s.reverse || s.fg >= 0 || s.bg >= 0
}

func ircFgClass(code int) string {
	return "irc-fg-" + strconv.Itoa(code)
}

func ircBgClass(code int) string {
	return "irc-bg-" + strconv.Itoa(code)
}

type ircSegment struct {
	text  []byte
	state ircState
}

func parseIRCSegments(data []byte) []ircSegment {
	segs, _ := parseIRCSegmentsWithState(data, newIRCState())
	return segs
}

func parseIRCSegmentsWithState(data []byte, initialState ircState) ([]ircSegment, ircState) {
	if len(data) == 0 {
		return nil, initialState
	}

	state := initialState
	var segments []ircSegment
	var buf []byte

	i := 0
	for i < len(data) {
		b := data[i]

		switch {
		case b == 0x02:
			flush(&segments, &buf, state)
			state.bold = !state.bold
			i++
		case b == 0x1d:
			flush(&segments, &buf, state)
			state.italic = !state.italic
			i++
		case b == 0x1f:
			flush(&segments, &buf, state)
			state.underline = !state.underline
			i++
		case b == 0x1e:
			flush(&segments, &buf, state)
			state.strikethrough = !state.strikethrough
			i++
		case b == 0x11:
			flush(&segments, &buf, state)
			state.monospace = !state.monospace
			i++
		case b == 0x16:
			flush(&segments, &buf, state)
			state.reverse = !state.reverse
			i++
		case b == 0x0f:
			flush(&segments, &buf, state)
			state = newIRCState()
			i++
		case b == 0x03:
			flush(&segments, &buf, state)
			i++
			if i < len(data) && data[i] == ',' {
				state.fg = -1
				state.bg = -1
				i++
			} else {
				fg, adv := parseIRCColorDigits(data, i)
				i += adv
				state.fg = fg
				if i < len(data) && data[i] == ',' {
					i++
					bg, adv := parseIRCColorDigits(data, i)
					i += adv
					state.bg = bg
				} else {
					state.bg = -1
				}
			}
		case b == 0x04:
			flush(&segments, &buf, state)
			i++
			hexAdv := 0
			for hexAdv < 6 && i+hexAdv < len(data) {
				c := data[i+hexAdv]
				if !isHexDigit(c) {
					break
				}
				hexAdv++
			}
			i += hexAdv
			if i < len(data) && data[i] == ',' {
				i++
				hexAdv2 := 0
				for hexAdv2 < 6 && i+hexAdv2 < len(data) {
					c := data[i+hexAdv2]
					if !isHexDigit(c) {
						break
					}
					hexAdv2++
				}
				i += hexAdv2
			}
		case b < 0x20:
			i++
		default:
			buf = append(buf, b)
			i++
		}
	}

	flush(&segments, &buf, state)
	return segments, state
}

func flush(segments *[]ircSegment, buf *[]byte, state ircState) {
	if len(*buf) == 0 && !state.hasFormatting() {
		return
	}
	if len(*buf) > 0 {
		*segments = append(*segments, ircSegment{text: *buf, state: state})
		*buf = nil
	}
}

func parseIRCColorDigits(data []byte, pos int) (int, int) {
	if pos >= len(data) || data[pos] < '0' || data[pos] > '9' {
		return -1, 0
	}
	d1 := int(data[pos] - '0')
	if pos+1 < len(data) && data[pos+1] >= '0' && data[pos+1] <= '9' {
		d2 := int(data[pos+1] - '0')
		code := d1*10 + d2
		if code <= 99 {
			return code, 2
		}
	}
	return d1, 1
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// hasIRCControl reports whether data contains any IRC formatting
// control byte (the transformer's actual vocabulary). Deliberately NOT
// a naive byte<0x20 check — that would match \n and \t and route
// ordinary multi-line code spans down the rebuild path.
func hasIRCControl(data []byte) bool {
	for _, b := range data {
		switch b {
		case 0x02, 0x03, 0x04, 0x0f, 0x11, 0x16, 0x1d, 0x1e, 0x1f:
			return true
		}
	}
	return false
}

// stripIRCCodes removes IRC control sequences from text destined for
// contexts that must show plain text (inline code spans). Handles the
// full vocabulary the site's pipeline can produce: single-byte toggles
// (\x02 \x11 \x16 \x0f \x1d \x1e \x1f), \x03 with up to two fg digits
// and an optional ,bg pair, and \x04 with up to six hex digits and an
// optional ,hex pair.
func stripIRCCodes(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		b := data[i]
		switch {
		case b == 0x02, b == 0x11, b == 0x16, b == 0x0f, b == 0x1d, b == 0x1e, b == 0x1f:
			i++
		case b == 0x03:
			i++
			i += ircDigitLen(data, i)
			if i < len(data) && data[i] == ',' {
				i++
				i += ircDigitLen(data, i)
			}
		case b == 0x04:
			i++
			i += ircHexLen(data, i)
			if i < len(data) && data[i] == ',' {
				i++
				i += ircHexLen(data, i)
			}
		default:
			out = append(out, b)
			i++
		}
	}
	return out
}

func ircDigitLen(data []byte, pos int) int {
	n := 0
	for n < 2 && pos+n < len(data) && data[pos+n] >= '0' && data[pos+n] <= '9' {
		n++
	}
	return n
}

func ircHexLen(data []byte, pos int) int {
	n := 0
	for n < 6 && pos+n < len(data) && isHexDigit(data[pos+n]) {
		n++
	}
	return n
}

func isInCode(n ast.Node) bool {
	for p := n.Parent(); p != nil; p = p.Parent() {
		if p.Kind() == ast.KindCodeSpan || p.Kind() == ast.KindRawHTML {
			return true
		}
	}
	return false
}

type ircTransformer struct{}

func (t *ircTransformer) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	source := reader.Source()

	type textNodeInfo struct {
		node   *ast.Text
		parent ast.Node
	}

	var textNodes []textNodeInfo
	// Code spans carrying IRC codes are rebuilt AFTER the walk —
	// mutating the tree mid-walk breaks goldmark's sibling traversal
	// (RemoveChild nils the removed node's next-sibling, ending the
	// parent's child loop early), silently dropping every following
	// sibling's transformation. Same collect-then-mutate discipline as
	// the text-node pass below.
	type codeSpanFix struct {
		node     ast.Node
		stripped []byte
	}
	var codeSpanFixes []codeSpanFix

	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if n.Kind() == ast.KindCodeSpan {
			// Inline code spans: upstream BBCode translation (the site
			// runs TranslateBBCode before Render) may have left IRC
			// control sequences inside code text; leaving them as bytes
			// shows the color digits ("04text"). Strip the codes and
			// rebuild the span as raw wrapper + escaped text — the
			// renderer's code-span walker asserts *ast.Text children,
			// so swapping the inner text node panics it; replacing the
			// WHOLE span is safe. hasIRCControl (not a
			// naive byte<0x20 gate, which would catch \n/\t) keeps
			// untouched spans on the no-op path.
			var buf []byte
			for c := n.FirstChild(); c != nil; c = c.NextSibling() {
				if t, ok := c.(*ast.Text); ok {
					buf = append(buf, t.Value(source)...)
				}
			}
			if hasIRCControl(buf) {
				codeSpanFixes = append(codeSpanFixes, codeSpanFix{node: n, stripped: stripIRCCodes(buf)})
			}
			return ast.WalkSkipChildren, nil
		}
		if n.Kind() != ast.KindText {
			return ast.WalkContinue, nil
		}
		if isInCode(n) {
			return ast.WalkContinue, nil
		}

		textNodes = append(textNodes, textNodeInfo{
			node:   n.(*ast.Text),
			parent: n.Parent(),
		})
		return ast.WalkContinue, nil
	})

	for _, fix := range codeSpanFixes {
		open := codeString([]byte("<code>"))
		body := ast.NewString(fix.stripped)
		close := codeString([]byte("</code>"))
		p := fix.node.Parent()
		p.ReplaceChild(p, fix.node, open)
		p.InsertAfter(p, open, body)
		p.InsertAfter(p, body, close)
	}

	var currentState ircState
	var lastParent ast.Node

	for _, info := range textNodes {
		if info.parent != lastParent {
			currentState = newIRCState()
			lastParent = info.parent
		}

		textNode := info.node
		value := textNode.Value(source)

		segments, nextState := parseIRCSegmentsWithState(value, currentState)

		var replacements []ast.Node

		for _, seg := range segments {
			classes := seg.state.classes()
			if len(classes) == 0 {
				textStr := ast.NewString(seg.text)
				replacements = append(replacements, textStr)
			} else {
				span := buildSpanOpen(classes)
				span.SetCode(true)
				replacements = append(replacements, span)
				textStr := ast.NewString(seg.text)
				replacements = append(replacements, textStr)
				closeSpan := ast.NewString([]byte("</span>"))
				closeSpan.SetCode(true)
				replacements = append(replacements, closeSpan)
			}
		}

		if textNode.HardLineBreak() || (textNode.SoftLineBreak()) {
			replacements = append(replacements, codeString([]byte("<br />\n")))
		}

		for _, repl := range replacements {
			info.parent.InsertBefore(info.parent, textNode, repl)
		}
		info.parent.RemoveChild(info.parent, textNode)

		currentState = nextState
	}
}

func codeString(s []byte) *ast.String {
	n := ast.NewString(s)
	n.SetCode(true)
	return n
}

func buildSpanOpen(classes []string) *ast.String {
	var buf []byte
	buf = append(buf, `<span class="`...)
	for i, c := range classes {
		if i > 0 {
			buf = append(buf, ' ')
		}
		buf = append(buf, c...)
	}
	buf = append(buf, '"', '>')
	return ast.NewString(buf)
}

type ircExtension struct{}

var IRCExtension = &ircExtension{}

func (e *ircExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithASTTransformers(
			util.Prioritized(&ircTransformer{}, 999),
		),
	)
}
