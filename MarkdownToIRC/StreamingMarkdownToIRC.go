package markdowntoirc

import (
	"regexp"
	"strings"
)

// super basic conversions for streaming since
// doing this without gomarkdown/markdown to just keep it simple hopefully
// i dont expect this to be very complete or good

// track the conversion state, mainly so we know if we're in a code block and shouldnt render anything else
type StreamingMD struct {
	inCode    bool
	codeDelim string
}

func (s *StreamingMD) ParseLine(line string) string {

	if s.inCode {
		// look for the exact delim string to end
		if s.codeDelim != "" && strings.Contains(line, s.codeDelim) {
			s.inCode = false
			s.codeDelim = ""
		}
		return line
	}

	// find one-or-more backticks and store the actual delimiter string
	re := regexp.MustCompile("`+")
	if m := re.FindString(line); m != "" {
		s.inCode = true
		s.codeDelim = m
	}

	return line
}
