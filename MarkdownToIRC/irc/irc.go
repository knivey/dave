package irc

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	ircBold      = '\x02'
	ircItalic    = '\x1D'
	ircUnderline = '\x1F'
	ircColor     = '\x03'
	ircReset     = '\x0F'
	defaultColor = byte(99)
)

type IRCColor struct {
	FG byte
	BG byte
}

type IRCFormat struct {
	Bold      bool
	Italic    bool
	Underline bool
	Color     *IRCColor
}

func (f IRCFormat) Equal(other IRCFormat) bool {
	if f.Bold != other.Bold || f.Italic != other.Italic || f.Underline != other.Underline {
		return false
	}
	if f.Color == nil && other.Color == nil {
		return true
	}
	if f.Color == nil || other.Color == nil {
		return false
	}
	return f.Color.FG == other.Color.FG && f.Color.BG == other.Color.BG
}

func (f IRCFormat) IsDefault() bool {
	return !f.Bold && !f.Italic && !f.Underline && f.Color == nil
}

type IRCSegment struct {
	Text  string
	Style IRCFormat
}

func ParseIRC(text string) []IRCSegment {
	var segments []IRCSegment
	style := IRCFormat{}
	var buf strings.Builder
	runes := []rune(text)
	i := 0

	for i < len(runes) {
		r := runes[i]
		switch r {
		case ircReset:
			if buf.Len() > 0 {
				segments = append(segments, IRCSegment{Text: buf.String(), Style: style})
				buf.Reset()
			}
			style = IRCFormat{}
		case ircBold:
			if buf.Len() > 0 {
				segments = append(segments, IRCSegment{Text: buf.String(), Style: style})
				buf.Reset()
			}
			style.Bold = !style.Bold
		case ircItalic:
			if buf.Len() > 0 {
				segments = append(segments, IRCSegment{Text: buf.String(), Style: style})
				buf.Reset()
			}
			style.Italic = !style.Italic
		case ircUnderline:
			if buf.Len() > 0 {
				segments = append(segments, IRCSegment{Text: buf.String(), Style: style})
				buf.Reset()
			}
			style.Underline = !style.Underline
		case ircColor:
			if buf.Len() > 0 {
				segments = append(segments, IRCSegment{Text: buf.String(), Style: style})
				buf.Reset()
			}
			style, i = parseColor(runes, i, style)
		default:
			buf.WriteRune(r)
		}
		i++
	}

	if buf.Len() > 0 {
		segments = append(segments, IRCSegment{Text: buf.String(), Style: style})
	}

	if len(segments) == 0 {
		segments = append(segments, IRCSegment{Text: "", Style: IRCFormat{}})
	}

	return segments
}

func parseColor(runes []rune, pos int, style IRCFormat) (IRCFormat, int) {
	end, col := parseColorCode(runes, pos)
	style.Color = col
	return style, end - 1
}

func isDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

func parseColorCode(runes []rune, pos int) (int, *IRCColor) {
	i := pos
	if i >= len(runes) || runes[i] != ircColor {
		return i, nil
	}
	i++
	if i >= len(runes) || !isDigit(runes[i]) {
		return i, nil
	}
	fg := byte(runes[i] - '0')
	i++
	if i < len(runes) && isDigit(runes[i]) {
		fg = fg*10 + byte(runes[i]-'0')
		i++
	}
	bg := defaultColor
	if i < len(runes) && runes[i] == ',' {
		commaI := i
		i++
		if i < len(runes) && isDigit(runes[i]) {
			bg = byte(runes[i] - '0')
			i++
			if i < len(runes) && isDigit(runes[i]) {
				bg = bg*10 + byte(runes[i]-'0')
				i++
			}
		} else {
			i = commaI
			bg = defaultColor
		}
	}
	return i, &IRCColor{FG: fg, BG: bg}
}

func CloseCodes(style IRCFormat) string {
	var b strings.Builder
	if style.Color != nil {
		b.WriteRune(ircColor)
	}
	if style.Underline {
		b.WriteRune(ircUnderline)
	}
	if style.Italic {
		b.WriteRune(ircItalic)
	}
	if style.Bold {
		b.WriteRune(ircBold)
	}
	return b.String()
}

func OpenCodes(style IRCFormat) string {
	var b strings.Builder
	if style.Bold {
		b.WriteRune(ircBold)
	}
	if style.Italic {
		b.WriteRune(ircItalic)
	}
	if style.Underline {
		b.WriteRune(ircUnderline)
	}
	if style.Color != nil {
		if style.Color.BG != defaultColor {
			fmt.Fprintf(&b, "%c%d,%d", ircColor, style.Color.FG, style.Color.BG)
		} else {
			fmt.Fprintf(&b, "%c%d", ircColor, style.Color.FG)
		}
	}
	return b.String()
}

func StyleChange(from, to IRCFormat) string {
	var b strings.Builder
	if !from.Equal(to) {
		if !from.IsDefault() {
			b.WriteString(CloseCodes(from))
		}
		if !to.IsDefault() {
			b.WriteString(OpenCodes(to))
		}
	}
	return b.String()
}

func StripCodes(text string) string {
	var b strings.Builder
	runes := []rune(text)
	i := 0
	for i < len(runes) {
		r := runes[i]
		switch r {
		case ircBold, ircItalic, ircUnderline, ircReset:
		case ircColor:
			i, _ = parseColorCode(runes, i)
			continue
		default:
			b.WriteRune(r)
		}
		i++
	}
	return b.String()
}

func SplitAt(text string, plainPos int) (string, string) {
	var before strings.Builder
	var after strings.Builder
	currentPlain := 0
	runes := []rune(text)
	i := 0
	split := false
	activeStyle := IRCFormat{}

	for i < len(runes) {
		r := runes[i]
		switch r {
		case ircBold:
			activeStyle.Bold = !activeStyle.Bold
			if !split {
				before.WriteRune(r)
			} else {
				after.WriteRune(r)
			}
			i++
		case ircItalic:
			activeStyle.Italic = !activeStyle.Italic
			if !split {
				before.WriteRune(r)
			} else {
				after.WriteRune(r)
			}
			i++
		case ircUnderline:
			activeStyle.Underline = !activeStyle.Underline
			if !split {
				before.WriteRune(r)
			} else {
				after.WriteRune(r)
			}
			i++
		case ircReset:
			activeStyle = IRCFormat{}
			if !split {
				before.WriteRune(r)
			} else {
				after.WriteRune(r)
			}
			i++
		case ircColor:
			colorStart := i
			var newColor *IRCColor
			i, newColor = parseColorCode(runes, i)
			activeStyle.Color = newColor

			codeStr := string(runes[colorStart:i])
			if !split {
				before.WriteString(codeStr)
			} else {
				after.WriteString(codeStr)
			}
		default:
			if !split {
				before.WriteRune(r)
				currentPlain++
				if currentPlain == plainPos {
					split = true
					before.WriteString(CloseCodes(activeStyle))
					after.WriteString(OpenCodes(activeStyle))
				}
			} else {
				after.WriteRune(r)
			}
			i++
		}
	}

	return before.String(), after.String()
}

func WordWrap(text string, maxWidth int) []string {
	if text == "" {
		return []string{""}
	}

	stripped := StripCodes(text)
	runes := []rune(stripped)
	if len(runes) <= maxWidth {
		return []string{text}
	}

	cutPos := FindWordBreak(stripped, maxWidth)
	if cutPos == 0 {
		cutPos = maxWidth
	}

	before, after := SplitAt(text, cutPos)

	afterStripped := StripCodes(after)
	if len(afterStripped) > 0 && afterStripped[0] == ' ' {
		_, after = SplitAt(text, cutPos+1)
	}

	var lines []string
	lines = append(lines, before)
	lines = append(lines, WordWrap(after, maxWidth)...)

	return lines
}

func FindWordBreak(plain string, maxLen int) int {
	runes := []rune(plain)
	if len(runes) <= maxLen {
		return len(runes)
	}

	for i := maxLen; i > maxLen/2; i-- {
		if runes[i] == ' ' {
			return i
		}
	}
	return maxLen
}

const minWordBreak = 15

func byteSplitAt(text string, plainPos int) (string, string) {
	var before strings.Builder
	var after strings.Builder
	currentPlain := 0
	runes := []rune(text)
	i := 0
	split := false
	activeStyle := IRCFormat{}

	for i < len(runes) {
		r := runes[i]
		switch r {
		case ircBold:
			activeStyle.Bold = !activeStyle.Bold
			if !split {
				before.WriteRune(r)
			} else {
				after.WriteRune(r)
			}
			i++
		case ircItalic:
			activeStyle.Italic = !activeStyle.Italic
			if !split {
				before.WriteRune(r)
			} else {
				after.WriteRune(r)
			}
			i++
		case ircUnderline:
			activeStyle.Underline = !activeStyle.Underline
			if !split {
				before.WriteRune(r)
			} else {
				after.WriteRune(r)
			}
			i++
		case ircReset:
			activeStyle = IRCFormat{}
			if !split {
				before.WriteRune(r)
			} else {
				after.WriteRune(r)
			}
			i++
		case ircColor:
			colorStart := i
			var newColor *IRCColor
			i, newColor = parseColorCode(runes, i)
			activeStyle.Color = newColor

			codeStr := string(runes[colorStart:i])
			if !split {
				before.WriteString(codeStr)
			} else {
				after.WriteString(codeStr)
			}
		default:
			if !split {
				before.WriteRune(r)
				currentPlain++
				if currentPlain == plainPos {
					split = true
					after.WriteString(OpenCodes(activeStyle))
				}
			} else {
				after.WriteRune(r)
			}
			i++
		}
	}

	return before.String(), after.String()
}

func findByteBreakPos(text string, maxBytes int) int {
	runes := []rune(text)
	byteCount := 0
	plainCount := 0
	result := 0
	i := 0

	for i < len(runes) {
		r := runes[i]
		switch r {
		case ircBold, ircItalic, ircUnderline, ircReset:
			byteCount++
			i++
		case ircColor:
			colorStart := i
			i, _ = parseColorCode(runes, i)
			byteCount += len(string(runes[colorStart:i]))
		default:
			byteCount += utf8.RuneLen(r)
			plainCount++
			if byteCount <= maxBytes {
				result = plainCount
			} else {
				return result
			}
			i++
		}
	}
	return result
}

func ByteWrap(text string, maxBytes int) []string {
	if text == "" {
		return []string{""}
	}
	if len(text) <= maxBytes {
		return []string{text}
	}

	breakPos := findByteBreakPos(text, maxBytes)
	if breakPos == 0 {
		breakPos = 1
	}

	stripped := StripCodes(text)
	cutPos := FindByteBreak(stripped, breakPos, minWordBreak)
	if cutPos == 0 {
		cutPos = breakPos
	}

	before, after := byteSplitAt(text, cutPos)

	afterStripped := StripCodes(after)
	if len(afterStripped) > 0 && afterStripped[0] == ' ' {
		_, after = byteSplitAt(text, cutPos+1)
	}

	var lines []string
	lines = append(lines, before)
	lines = append(lines, ByteWrap(after, maxBytes)...)

	return lines
}

func FindByteBreak(plain string, pos int, minBack int) int {
	runes := []rune(plain)
	if pos >= len(runes) {
		pos = len(runes) - 1
	}
	if pos < 1 {
		return 0
	}
	start := pos - minBack
	if start < 0 {
		start = 0
	}
	for i := pos; i > start; i-- {
		if runes[i] == ' ' {
			return i
		}
	}
	return 0
}
