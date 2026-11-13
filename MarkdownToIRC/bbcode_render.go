package markdowntoirc

// BBCode render state (Nov 2026; spec:
// docs/superpowers/specs/2026-11-13-bbcode-in-markdown-design.md): the
// AST-level twin of the byte translator's semantics (bbcode.go —
// applyTag/emitRun are the reference; the tests there pin the same
// behavior). The goldmark extension (bbcode_ast.go) turns color tags into
// nodes; THIS file holds the state those nodes drive while the Renderer
// walks the document.
//
// DESIGN NOTE — why codes bypass the text writer: during a walk, every
// TEXT byte flows through bbTextWriter → textInto (guard + newline
// re-emit), but tag nodes write their control codes STRAIGHT into the
// buffer via openInto/closeInto. If codes went through textInto they
// would count as visible text and re-arm/clear the pending-newline state
// — a \x03 emission must be transparent to the state machine except for
// arming the digit-guard, which openInto/closeInto do themselves. The
// walk is single-threaded and call-ordered, so interleaving direct code
// writes with guarded text writes on the same buffer reproduces the byte
// translator's exact emission order.
//
// DESIGN NOTE — deferred emission while a newline is pending: a tag on
// its own line after a trailing newline would otherwise put its codes on
// an empty line — a codes-only PRIVMSG (a blank bot message). Open and
// close apply STATE ONLY in that case; the re-emit at the next visible
// character reconstructs the post-tag state, and if nothing visible ever
// follows, the codes are pure artifact (clients reset formatting per
// message; autoClose also suppresses its tail on a pending newline).

import (
	"bytes"
	"strconv"
)

// bbRenderState carries the color stack across the render walk AND across
// streaming fragments (the StreamingRenderer owns one instance for its
// lifetime; one-shot renders construct a fresh one per call). All methods
// take the target buffer explicitly: the walk passes its own buffer
// (outBuf, set by Renderer.Render), table-cell extraction (renderNodeTo)
// passes the cell buffer — order is call-order on the same/own buffer,
// single-threaded walk.
type bbRenderState struct {
	stack     []string // two-digit codes, innermost last; "" = transparent placeholder
	pendingNL bool     // a \n awaits re-emission before the next visible byte
	guard     bool     // a \x03 emission awaits the digit/comma decision

	// outBuf is the walk's own buffer, set by Renderer.Render so the
	// renderNode tag cases (which receive only an io.Writer) can hand
	// the state the buffer their codes belong in. Table cells never use
	// it — they pass their own buffer explicitly.
	outBuf *bytes.Buffer
}

// effective returns the nearest REAL color in the stack, skipping
// transparent placeholders ("" entries) — the color visually active right
// now. "" means nothing is colored.
func (s *bbRenderState) effective() string {
	for i := len(s.stack) - 1; i >= 0; i-- {
		if s.stack[i] != "" {
			return s.stack[i]
		}
	}
	return ""
}

// textInto is the single choke point for TEXT bytes (the translator's old
// emitRun, verbatim semantics): newlines arm the re-emit (a real color
// open), the first following byte re-emits it, and a digit/comma right
// after a color emission gets the \x02\x02 no-op guard. Code emissions
// between a \x03 and the next visible byte clear color adjacency
// naturally (a guard is only needed when text directly follows the \x03).
func (s *bbRenderState) textInto(buf *bytes.Buffer, p []byte) {
	for i := 0; i < len(p); i++ {
		c := p[i]
		if c == '\n' {
			buf.WriteByte('\n')
			if s.effective() != "" {
				s.pendingNL = true
			}
			s.guard = false
			continue
		}
		if s.pendingNL {
			s.pendingNL = false
			if eff := s.effective(); eff != "" {
				buf.WriteString("\x03" + eff)
				s.guard = true
			}
		}
		if s.guard && (c >= '0' && c <= '9' || c == ',') {
			buf.WriteString("\x02\x02")
		}
		s.guard = false
		buf.WriteByte(c)
	}
}

// openInto applies a [color=X] node: the resolved two-digit Code (parse
// time) is pushed; "" (an invalid value, warned at parse time by the
// extension) is a TRANSPARENT placeholder — no code emitted, but the
// eventual close pairs with IT instead of popping an innocent enclosing
// color.
func (s *bbRenderState) openInto(buf *bytes.Buffer, n *BBCodeColorOpen) {
	s.stack = append(s.stack, n.Code)
	if n.Code == "" || s.pendingNL {
		return // transparent placeholder, or deferred: re-emit reconstructs
	}
	buf.WriteString("\x03" + n.Code)
	s.guard = true
}

// closeInto applies a [/color] (or [/color=X]) node: pops the stack and
// re-emits the enclosing color, or a bare \x03 at top level — the
// color-only reset (markdown-emitted bold/italic survive it; \x0F is
// NEVER emitted). A close with no open is stripped with a WARN (the same
// text as the byte translator — Global Constraints pin the wording).
func (s *bbRenderState) closeInto(buf *bytes.Buffer, n *BBCodeColorClose, warn func(string)) {
	if len(s.stack) == 0 {
		if warn != nil {
			warn("bbcode: stray close " + strconv.Quote(n.Raw) + " with no open color stripped")
		}
		return
	}
	popped := s.stack[len(s.stack)-1]
	s.stack = s.stack[:len(s.stack)-1]
	if popped == "" || s.pendingNL {
		return // placeholder span never emitted; deferred close: re-emit reconstructs
	}
	if eff := s.effective(); eff != "" {
		buf.WriteString("\x03" + eff)
	} else {
		buf.WriteByte(0x03)
	}
	s.guard = true
}

// autoClose is the end-of-render/flush tail: a bare \x03 when a real
// color is open AND something visible followed the last newline
// (codes-only-line suppression — a codes-only PRIVMSG is a blank bot
// message). The one-shot entry point appends it after the render; the
// StreamingRenderer owns flush-time auto-close for its long-lived state.
func (s *bbRenderState) autoClose() string {
	if s.pendingNL || s.effective() == "" {
		return ""
	}
	return "\x03"
}

// bbTextWriter wraps the walk writer: every TEXT byte the Renderer
// writes (paragraph text, indents, code-block wrappers, table glyphs)
// passes through the state; tag-node codes never do (they write straight
// to the buffer via openInto/closeInto).
type bbTextWriter struct {
	st  *bbRenderState
	buf *bytes.Buffer
}

func (w *bbTextWriter) Write(p []byte) (int, error) {
	w.st.textInto(w.buf, p)
	return len(p), nil
}
