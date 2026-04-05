package markdowntoirc

import (
	"regexp"
	"strings"
)

var codeFenceRE = regexp.MustCompile("`+")

type StreamingMD struct {
	inCode    bool
	codeDelim string
}

func (s *StreamingMD) ParseLine(line string) string {

	if s.inCode {
		if s.codeDelim != "" && strings.Contains(line, s.codeDelim) {
			s.inCode = false
			s.codeDelim = ""
		}
		return line
	}

	if m := codeFenceRE.FindString(line); m != "" {
		s.inCode = true
		s.codeDelim = m
	}

	return line
}
