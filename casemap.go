package main

import "strings"

// normalizeIRC applies IRC casemapping rules to a string (nick or channel).
// Three casemapping types per IRC spec (ISUPPORT CASEMAPPING token):
//   - "ascii":           A-Z → a-z only
//   - "strict-rfc1459":  A-Z → a-z, [ → {, ] → }, \ → |
//   - "rfc1459":         A-Z → a-z, [ → {, ] → }, \ → |, ~ → ^
//
// Unknown or empty casemapping defaults to "rfc1459".
func normalizeIRC(s, casemapping string) string {
	switch strings.ToLower(casemapping) {
	case "ascii":
		return toASCII(s)
	case "strict-rfc1459":
		return toStrictRFC1459(s)
	default:
		return toRFC1459(s)
	}
}

func toASCII(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			b.WriteByte(c + 32)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

func toStrictRFC1459(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			b.WriteByte(c + 32)
		} else {
			switch c {
			case '[':
				b.WriteByte('{')
			case ']':
				b.WriteByte('}')
			case '\\':
				b.WriteByte('|')
			default:
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}

func toRFC1459(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			b.WriteByte(c + 32)
		} else {
			switch c {
			case '[':
				b.WriteByte('{')
			case ']':
				b.WriteByte('}')
			case '\\':
				b.WriteByte('|')
			case '~':
				b.WriteByte('^')
			default:
				b.WriteByte(c)
			}
		}
	}
	return b.String()
}
