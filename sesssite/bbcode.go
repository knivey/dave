package main

// BBCode translation for the session-history site — ported VERBATIM
// from dave's bbcode.go (the emission-layer translator). dave stores
// raw model output (BBCode included; IRC translation happens at send
// time), so the site re-runs the SAME translation before markdown
// rendering: parity with what IRC users saw, same color vocabulary,
// same styles gate, same unknown-tag-verbatim rule. Only the dave
// config resolvers were dropped (the site carries its own
// [render] bbcode / bbcode_styles knobs).

import (
	"fmt"
	"strconv"
	"strings"
)

// bbMaxTagLen bounds the held partial-tag region: a longer unterminated
// candidate is not a tag and releases as text. Purely a streaming hold
// bound — no semantic decision hangs on it (the chunk-invariance lesson).
const bbMaxTagLen = 64

// bbColorNames maps color words (lowercased, trimmed) to two-digit IRC
// codes. Spaced and unspaced variants both exist because models write
// both ("light green" / "lightgreen").
var bbColorNames = map[string]string{
	"white": "00", "black": "01", "blue": "02", "green": "03", "red": "04",
	"brown": "05", "maroon": "05", "magenta": "06", "purple": "06",
	"orange": "07", "yellow": "08",
	"lightgreen": "09", "light green": "09", "lime": "09",
	"cyan": "10", "teal": "10",
	"lightcyan": "11", "light cyan": "11",
	"lightblue": "12", "light blue": "12",
	"royal": "12", "royalblue": "12", "royal blue": "12",
	"pink": "13", "grey": "14", "gray": "14",
	"lightgrey": "15", "lightgray": "15", "light grey": "15", "light gray": "15",
	"silver": "15",
}

var bbToggleCode = map[byte]byte{'b': 0x02, 'i': 0x1D, 'u': 0x1F, 's': 0x1E}

// bbCanonicalOrder is the fixed order styles re-emit and auto-close in.
const bbCanonicalOrder = "bius"

// TranslateBBCode is the stateless one-shot form for complete responses
// (final replies, tool-loop text, legacy completions). warn receives one
// message per dropped malformed tag; nil is silent. styles reports
// whether the style-toggle half is enabled: with it off, toggle tags ride
// the unknown-token path — verbatim, no warn — while colors translate.
func TranslateBBCode(text string, warn func(string), styles bool) string {
	tr := newBBCodeTranslator(warn, styles)
	return tr.Process(text) + tr.Flush()
}

// bbcodeTranslator is the streaming form: Process feeds deltas and returns
// every decidable byte, holding back only a trailing possibly-incomplete
// tag (`[`, `[name`, `[name=value` without its `]`) and — lazily — the
// one-character guard decision after a color emission. Flush resolves the
// tail (an unterminated tag candidate releases verbatim — it never became
// a tag), auto-closes open styles, and spends the instance (callers build
// a fresh one per API round-trip; continuations never Process again).
type bbcodeTranslator struct {
	colorStack             []string // two-digit codes, innermost last
	bold, ital, under, str bool
	pending                string // held tag candidate
	justColor              bool   // a \x03 emission awaits its guard decision
	pendingNL              bool   // a \n awaits style re-emission before visible text
	// styles gates the style-toggle half ([b] [i] [u] [s]): when false,
	// toggle tags are prose jurisdiction and ride the unknown-token
	// path (verbatim, no warn) exactly like [table] citations. Colors
	// are never gated here — only the caller's bbcodeEnabled does that.
	// With styles off no toggle can ever open, so Flush's bius
	// auto-close loop and emitRun's toggle re-emits naturally no-op.
	styles bool
	warn   func(string)
}

func newBBCodeTranslator(warn func(string), styles bool) *bbcodeTranslator {
	return &bbcodeTranslator{warn: warn, styles: styles}
}

func (t *bbcodeTranslator) warnf(format string, args ...any) {
	if t.warn != nil {
		t.warn("bbcode: " + fmt.Sprintf(format, args...))
	}
}

// bbEffectiveColor returns the nearest REAL color in the stack, skipping
// transparent placeholders ("" entries) — the color visually active right
// now. "" means nothing is colored.
func (t *bbcodeTranslator) bbEffectiveColor() string {
	for i := len(t.colorStack) - 1; i >= 0; i-- {
		if t.colorStack[i] != "" {
			return t.colorStack[i]
		}
	}
	return ""
}

func (t *bbcodeTranslator) anyOpen() bool {
	return len(t.colorStack) > 0 || t.bold || t.ital || t.under || t.str
}

func (t *bbcodeTranslator) toggleOpen(name byte) bool {
	switch name {
	case 'b':
		return t.bold
	case 'i':
		return t.ital
	case 'u':
		return t.under
	case 's':
		return t.str
	}
	return false
}

func (t *bbcodeTranslator) setToggle(name byte, v bool) {
	switch name {
	case 'b':
		t.bold = v
	case 'i':
		t.ital = v
	case 'u':
		t.under = v
	case 's':
		t.str = v
	}
}

// Process translates delta plus any held bytes, returning what is
// decidable now. Empty in does not mean end of stream (wire-noise chunks
// carry content:""), so it returns "" for "".
func (t *bbcodeTranslator) Process(delta string) string {
	if delta == "" {
		return ""
	}
	t.pending += delta
	var out strings.Builder
	for t.pending != "" {
		if t.pending[0] != '[' {
			idx := strings.IndexByte(t.pending, '[')
			if idx < 0 {
				t.emitRun(&out, t.pending)
				t.pending = ""
				break
			}
			t.emitRun(&out, t.pending[:idx])
			t.pending = t.pending[idx:]
			continue
		}
		tag, ok := parseBBTag(t.pending)
		if !ok {
			break // incomplete tag candidate — hold
		}
		t.applyTag(&out, tag)
		t.pending = t.pending[tag.consumed:]
	}
	return out.String()
}

// Flush resolves the held tail and auto-closes open styles. Auto-close
// emits one bare \x03 regardless of color-nesting depth (a re-emit chain
// ending in a reset is a no-op sandwich) plus the toggles in canonical
// order. The instance is spent afterwards.
func (t *bbcodeTranslator) Flush() string {
	var out strings.Builder
	if t.pending != "" {
		// Never completed its ']': it never became a tag — release
		// verbatim (visible, not eaten; the model left broken markup).
		t.emitRun(&out, t.pending)
		t.pending = ""
	}
	if t.pendingNL {
		t.pendingNL = false
		// Nothing visible ever followed the last newline: auto-close
		// codes would land on an empty line — a codes-only PRIVMSG
		// (a blank bot message). The instance is spent and IRC
		// clients reset formatting at every message boundary anyway,
		// so the codes are pure artifact — clear state, emit nothing.
		t.bold, t.ital, t.under, t.str = false, false, false, false
		t.colorStack = nil
		t.justColor = false
		return out.String()
	}
	t.pendingNL = false
	for i := 0; i < len(bbCanonicalOrder); i++ {
		name := bbCanonicalOrder[i]
		if t.toggleOpen(name) {
			out.WriteByte(bbToggleCode[name])
			t.setToggle(name, false)
		}
	}
	if t.bbEffectiveColor() != "" {
		out.WriteByte(0x03) // a real color is still open; placeholders need no reset
	}
	t.colorStack = nil
	t.justColor = false
	return out.String()
}

// emitRun writes a text run, resolving the lazy newline/guard state at
// each VISIBLE character: newlines arm the re-emit flag (styles open) but
// never trigger it themselves — an empty line must not produce a
// codes-only PRIVMSG — and code emissions between a \x03 and the next
// visible char clear color adjacency naturally (a guard is only needed
// when text directly follows the \x03).
func (t *bbcodeTranslator) emitRun(out *strings.Builder, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\n' {
			out.WriteByte('\n')
			if t.anyOpen() {
				t.pendingNL = true
			}
			t.justColor = false
			continue
		}
		if t.pendingNL {
			t.pendingNL = false
			if eff := t.bbEffectiveColor(); eff != "" {
				out.WriteString("\x03" + eff)
				t.justColor = true
			}
			for j := 0; j < len(bbCanonicalOrder); j++ {
				name := bbCanonicalOrder[j]
				if t.toggleOpen(name) {
					out.WriteByte(bbToggleCode[name])
					t.justColor = false
				}
			}
		}
		if t.justColor && (c >= '0' && c <= '9' || c == ',') {
			out.WriteString("\x02\x02")
		}
		t.justColor = false
		out.WriteByte(c)
	}
}

// bbTag is one parsed tag token.
type bbTag struct {
	consumed int
	closing  bool
	name     string // lowercased
	value    string // raw (only for opens; closes with values are invalid)
	// kind
	known    bool // name is in the vocabulary
	complete bool // had its ']' — always true when parseBBTag returns ok
}

// parseBBTag decides whether the front of s is a tag. ok=false means the
// candidate is incomplete (hold). A candidate that stops matching tag
// shape returns ok with known=false and consumed=1 — "not a tag, the '['
// is text" — so the caller re-scans from the following bytes. Unknown
// NAMES return the full token consumed (verbatim passthrough).
func parseBBTag(s string) (bbTag, bool) {
	i := 1
	closing := false
	if i < len(s) && s[i] == '/' {
		closing = true
		i++
	}
	nameStart := i
	for i < len(s) && isASCIILetter(s[i]) {
		i++
	}
	name := strings.ToLower(s[nameStart:i])
	if name == "" {
		if i >= len(s) {
			return bbTag{}, false // "[", "[/" — a name may still arrive
		}
		// "[]", "[1]", "[=x]" … — not tag-shaped; the '[' is text.
		return bbTag{consumed: 1}, true
	}
	value := ""
	if i < len(s) && s[i] == '=' {
		if closing {
			// [/color=red] — consume the value so the token closes
			// whole; the value is redundant, the intent is a close.
			// Known-ness uses the SAME expression as the plain-close
			// branch below: an unknown name ([/underline=x]) is
			// verbatim text, never a close whose first letter matches
			// a real toggle.
			vs := i + 1
			j := vs
			for j < len(s) && (isASCIILetter(s[j]) || isASCIIDigit(s[j]) || s[j] == ' ') {
				j++
			}
			if j == len(s) {
				// Hold unbounded: a mid-value cap would break chunk
				// invariance (one-shot classifies the complete tag;
				// byte-streaming would leak it verbatim). The hold
				// is bounded by the stream itself. See the name-growth
				// branch for why THAT cap is safe and this one is not.
				return bbTag{}, false
			}
			if s[j] != ']' {
				return bbTag{consumed: 1}, true
			}
			return bbTag{consumed: j + 1, closing: true, name: name,
				known: name == "color" || len(name) == 1 && bbToggleCode[name[0]] != 0, complete: true}, true
		}
		i++
		vs := i
		for i < len(s) && (isASCIILetter(s[i]) || isASCIIDigit(s[i]) || s[i] == ' ') {
			i++
		}
		if i == len(s) {
			// Hold unbounded — a mid-value cap breaks chunk invariance
			// (re-review finding A). The stream bounds the hold.
			return bbTag{}, false // value still growing
		}
		value = s[vs:i]
	}
	if i < len(s) && s[i] == ']' {
		return bbTag{consumed: i + 1, closing: closing, name: name, value: value,
			known: name == "color" || bbToggleCode[name[0]] != 0 && len(name) == 1, complete: true}, true
	}
	if i == len(s) {
		if len(s) > bbMaxTagLen {
			// SAFE here, unlike the value-growth branches above: any
			// 65+ char name is unknown, and unknown tokens are verbatim
			// in BOTH modes — releasing at the cap yields the same bytes
			// one-shot would. Value caps cannot make that claim (the
			// complete tag classifies as known), which is why the value
			// holds are unbounded.
			return bbTag{consumed: 1}, true // runaway partial — not a tag
		}
		return bbTag{}, false // name still growing
	}
	// Disqualifying char before ']': not a tag.
	return bbTag{consumed: 1}, true
}

func isASCIILetter(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func isASCIIDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// applyTag applies one parsed token to the state, emitting codes. Codes
// are DEFERRED (state-only) while a newline is pending: a tag on its own
// line after a trailing newline would otherwise put its codes on an empty
// line — a codes-only PRIVMSG. The re-emit at the next visible character
// reconstructs the exact post-tag state; if nothing visible ever follows,
// the codes are pure artifact (clients reset formatting per message).
func (t *bbcodeTranslator) applyTag(out *strings.Builder, tag bbTag) {
	raw := "<tag>"
	if tag.consumed <= len(t.pending) {
		raw = t.pending[:tag.consumed]
	}
	if !tag.known || (!t.styles && tag.name != "color") {
		// Unknown name — or a style-toggle tag while toggles are off
		// (bbcode_styles = false: bracket-ambiguous with prose, the
		// owner-ruled color-only default): prose/markdown jurisdiction,
		// verbatim.
		t.emitRun(out, raw)
		return
	}
	if tag.name == "color" {
		if tag.closing {
			// Any [/color…] form is close-intent (a close-with-value
			// pairs with its open just the same); dropping it would
			// strand the open color until auto-close.
			if len(t.colorStack) == 0 {
				t.warnf("stray close %q with no open color stripped", raw)
				return
			}
			popped := t.colorStack[len(t.colorStack)-1]
			t.colorStack = t.colorStack[:len(t.colorStack)-1]
			if popped == "" {
				// Transparent placeholder: no code was ever emitted
				// for its span, so the visual state is unchanged —
				// popping it emits nothing.
				return
			}
			if !t.pendingNL {
				out.WriteByte(0x03)
				if eff := t.bbEffectiveColor(); eff != "" {
					out.WriteString(eff)
				}
				t.justColor = true
			}
			return
		}
		code, ok := bbColorValue(tag.value)
		if !ok {
			// Track the span as a transparent placeholder so its
			// eventual [/color] pairs with IT instead of popping some
			// innocent enclosing color (an untracked invalid open
			// desyncs every later close). Warn once, here.
			t.colorStack = append(t.colorStack, "")
			t.warnf("invalid tag %q: unknown color, span tracked transparent", raw)
			return
		}
		t.colorStack = append(t.colorStack, code)
		if !t.pendingNL {
			out.WriteString("\x03" + code)
			t.justColor = true
		}
		return
	}
	// b/i/u/s
	c := tag.name[0]
	if tag.closing {
		if !t.toggleOpen(c) {
			t.warnf("stray close %q with no open tag stripped", raw)
			return
		}
		t.setToggle(c, false)
		if !t.pendingNL {
			out.WriteByte(bbToggleCode[c])
		}
		t.justColor = false
		return
	}
	if tag.value != "" {
		t.warnf("invalid tag %q stripped (value on valueless tag)", raw)
		return
	}
	t.setToggle(c, true)
	if !t.pendingNL {
		out.WriteByte(bbToggleCode[c])
	}
	t.justColor = false
}

// bbColorValue resolves a color tag value: a name/alias from the table or
// a numeric 0-15 (models ape mIRC numbers; inside a tag the value is
// explicit, so numbers are deterministic — no guessing). Two-digit result
// by construction.
func bbColorValue(v string) (string, bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "", false
	}
	if code, ok := bbColorNames[v]; ok {
		return code, true
	}
	if len(v) <= 2 && isASCIIDigit(v[0]) && (len(v) == 1 || isASCIIDigit(v[1])) {
		if n, err := strconv.Atoi(v); err == nil && n <= 15 {
			return bbPad2(n), true
		}
	}
	return "", false
}

func bbPad2(v int) string {
	if v < 10 {
		return "0" + strconv.Itoa(v)
	}
	return strconv.Itoa(v)
}
