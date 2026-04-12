package markdowntoirc

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/knivey/dave/MarkdownToIRC/irc"
)

func TestInlineFormatting(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "InlineBold",
			input:   "**bold text**",
			contain: []string{"\x02bold text\x02"},
		},
		{
			name:    "InlineItalic",
			input:   "*italic text*",
			contain: []string{"\x1Ditalic text\x1D"},
		},
		{
			name:    "InlineBoldAndItalic",
			input:   "**bold and *italic* together**",
			contain: []string{"\x02bold and \x1Ditalic\x1D together\x02"},
		},
		{
			name:    "InlineCode",
			input:   "`code`",
			contain: []string{"\x030,90code\x03"},
		},
		{
			name:    "InlineCodeWithText",
			input:   "use `code` here",
			contain: []string{"use \x030,90code\x03 here"},
		},
	})
}

func TestHeadings(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:       "Heading",
			input:      "# Hello",
			contain:    []string{"\x02Hello"},
			notContain: []string{"\x02\n", "\x02\x02"},
		},
		{
			name:    "HeadingFollowedByParagraph",
			input:   "# Title\n\nBody text",
			contain: []string{"\x02Title", "Body text"},
		},
		{
			name:    "MultipleHeadings",
			input:   "# One\n\n## Two",
			contain: []string{"\x02One", "\x02Two"},
		},
		{
			name:       "HeadingBoldClosed",
			input:      "# Title\n\nBody text",
			contain:    []string{"\x02Title\x02"},
			notContain: []string{"\x02Body"},
		},
	})
}

func TestParagraphs(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "SingleParagraph",
			input:   "Hello world",
			contain: []string{"Hello world"},
		},
		{
			name:    "MultipleParagraphs",
			input:   "Para one\n\nPara two",
			contain: []string{"Para one", "Para two"},
		},
		{
			name:       "ParagraphInListItem",
			input:      "- item text",
			contain:    []string{"item text"},
			notContain: []string{"\n\n"},
		},
	})
}

func TestCodeBlocks(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:       "CodeBlockNoLang",
			input:      "```\ncode\n```",
			contain:    []string{"\x030,90code\x03"},
			notContain: []string{"\n\n"},
		},
		{
			name:    "CodeBlockEmptyLines",
			input:   "```\nfoo\n\nbar\n```",
			contain: []string{"foo", "bar"},
		},
		{
			name:       "CodeBlockTrailingNewline",
			input:      "```\ncode\n```",
			notContain: []string{"\n\n"},
		},
		{
			name:       "CodeBlockMultipleTrailingNewlines",
			input:      "```\ncode\n\n\n```",
			notContain: []string{"\n\n"},
		},
		{
			name:       "CodeBlockLeadingNewline",
			input:      "```\n\ncode\n```",
			notContain: []string{"\n\n"},
		},
	})

	runTestsStripIRC(t, []struct {
		name         string
		input        string
		lines        []string
		checkBgColor bool
	}{
		{
			name:         "CodeBlockWithLang",
			input:        "```python\nprint(\"hi\")\n```",
			lines:        []string{" print(\"hi\") "},
			checkBgColor: true,
		},
	})
}

func TestLists(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "UnorderedList",
			input:   "- item one\n- item two",
			contain: []string{"\u2022 item one", "\u2022 item two"},
		},
		{
			name:    "OrderedList",
			input:   "1. first\n2. second",
			contain: []string{"1. first", "2. second"},
		},
		{
			name:    "OrderedListSplitByCodeBlock",
			input:   "1. Simple echo example\n```bash\necho hello\n```\n\n2. Echo with variable\n```bash\necho bye\n```",
			contain: []string{"1. Simple echo example", "2. Echo with variable"},
		},
		{
			name:    "NestedUnorderedList",
			input:   "- outer\n  - inner",
			contain: []string{"\u2022 outer", "   \u2022 inner"},
		},
		{
			name:    "NestedOrderedList",
			input:   "1. outer\n   1. inner",
			contain: []string{"1. outer"},
		},
		{
			name:    "MixedNestedList",
			input:   "- outer\n  1. inner",
			contain: []string{"\u2022 outer", "   1. inner"},
		},
		{
			name:    "TaskList",
			input:   "- [x] done\n- [ ] todo",
			contain: []string{"\u2022 [x] done", "\u2022 [ ] todo"},
		},
		{
			name:    "ListItemWithInlineCode",
			input:   "- use `code` here",
			contain: []string{"\u2022 use \x030,90code\x03 here"},
		},
	})
}

func TestCodeBlocksInLists(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:       "CodeBlockInListItem",
			input:      "- text:\n  ```\n  code\n  ```",
			contain:    []string{"\u2022 text:", "\x030,90code\x03"},
			notContain: []string{"\n\n"},
		},
		{
			name:       "CodeBlockInNestedListItem",
			input:      "- outer\n  - inner:\n    ```\n    code\n    ```",
			contain:    []string{"\u2022 outer", "   \u2022 inner:", "\x030,90code\x03"},
			notContain: []string{"\n\n"},
		},
		{
			name:       "CodeBlockAfterListItemParagraph",
			input:      "1. **Title:**\n   - text:\n     ```\n     sudo ip route add\n     ```",
			contain:    []string{"1. \x02Title:\x02", "\u2022 text:", "\x030,90sudo ip route add\x03"},
			notContain: []string{"\n\n"},
		},
	})

	runTestsStripIRC(t, []struct {
		name         string
		input        string
		lines        []string
		checkBgColor bool
	}{
		{
			name:  "CodeBlockWithBlankLine",
			input: "```python\n\ndef hello():\n    pass\n```\n\ntext",
			lines: []string{
				" def hello(): ",
				"     pass     ",
				"text",
			},
			checkBgColor: true,
		},
	})
}

func TestBlockQuotes(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "BlockQuote",
			input:   "> quoted text",
			contain: []string{"\x0309> quoted text"},
		},
		{
			name:    "BlockQuoteMultiLine",
			input:   "> line one\n> line two",
			contain: []string{"\x0309> line one line two"},
		},
		{
			name:    "NestedBlockQuote",
			input:   "> outer\n>> inner",
			contain: []string{"\x0309> outer", "\x0309>\x0309> inner"},
		},
		{
			name:    "BlockQuoteWithList",
			input:   "> - item",
			contain: []string{"\x0309>  \u2022 item"},
		},
		{
			name:    "BlockQuoteWithCode",
			input:   "> `code`",
			contain: []string{"\x0309> \x030,90code\x03"},
		},
	})
}

func TestBreaks(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "Hardbreak",
			input:   "line1  \nline2",
			contain: []string{"line1\nline2"},
		},
		{
			name:    "Softbreak",
			input:   "line1\nline2",
			contain: []string{"line1 line2"},
		},
	})
}

func TestEdgeCases(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "EmptyString",
			input:   "",
			contain: []string{""},
		},
		{
			name:    "WhitespaceOnly",
			input:   "   \n\n  ",
			contain: []string{""},
		},
		{
			name:       "OnlyCodeBlock",
			input:      "```\ncode\n```",
			contain:    []string{"\x030,90code\x03"},
			notContain: []string{"\n\n"},
		},
		{
			name:    "OnlyHeading",
			input:   "# Title",
			contain: []string{"\x02Title"},
		},
		{
			name:    "OnlyInlineCode",
			input:   "`code`",
			contain: []string{"\x030,90code\x03"},
		},
		{
			name:    "MixedFormatting",
			input:   "**bold** and `code` and *italic*",
			contain: []string{"\x02bold\x02", "\x030,90code\x03", "\x1Ditalic\x1D"},
		},
	})
}

func TestNoExtraEmptyLines(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"HeadingThenParagraph", "# Title\n\nBody"},
		{"CodeBlockThenParagraph", "```\ncode\n```\n\nBody"},
		{"ListThenParagraph", "- item\n\nBody"},
		{"MultipleCodeBlocks", "```\na\n```\n\n```\nb\n```"},
		{"NestedListItems", "- outer\n  - inner\n  - inner2"},
		{"CodeBlockInListItem", "- text:\n  ```\n  code\n  ```"},
		{"BlockQuote", "> quote\n\nBody"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToIRC(tt.input)
			if strings.Contains(got, "\n\n") {
				t.Errorf("output contains consecutive newlines\ngot: %s", humanize(got))
			}
		})
	}
}

func TestListIndentationLevels(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "TwoLevelList",
			input:   "- a\n  - b",
			contain: []string{"\u2022 a", "   \u2022 b"},
		},
		{
			name:    "ThreeLevelList",
			input:   "- a\n  - b\n    - c",
			contain: []string{"\u2022 a", "   \u2022 b", "      \u2022 c"},
		},
		{
			name:    "FourLevelList",
			input:   "- a\n  - b\n    - c\n      - d",
			contain: []string{"\u2022 a", "   \u2022 b", "      \u2022 c", "         \u2022 d"},
		},
		{
			name:    "IndentDecrease",
			input:   "- a\n  - b\n- c",
			contain: []string{"\u2022 a", "   \u2022 b", "\u2022 c"},
		},
		{
			name:    "IndentIncreaseAndDecrease",
			input:   "- a\n  - b\n    - c\n  - d\n- e",
			contain: []string{"\u2022 a", "   \u2022 b", "      \u2022 c", "   \u2022 d", "\u2022 e"},
		},
		{
			name:  "DeepNestedList",
			input: "- l1\n  - l2\n    - l3\n      - l4\n        - l5",
			contain: []string{
				"\u2022 l1",
				"   \u2022 l2",
				"      \u2022 l3",
				"         \u2022 l4",
				"         \u2022 l5",
			},
		},
	})
}

func TestQuoteRendering(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "SingleQuote",
			input:   "> quoted",
			contain: []string{"\x0309> quoted"},
		},
		{
			name:    "QuoteMultiLine",
			input:   "> line one\n> line two",
			contain: []string{"\x0309> line one line two"},
		},
		{
			name:    "DoubleNestedQuote",
			input:   "> outer\n>> inner",
			contain: []string{"\x0309> outer", "\x0309>\x0309> inner"},
		},
		{
			name:  "TripleNestedQuote",
			input: "> outer\n>> inner\n>>> deepest",
			contain: []string{
				"\x0309> outer",
				"\x0309>\x0309> inner",
				"\x0309>\x0309>\x0309> deepest",
			},
		},
		{
			name:    "QuoteWithBold",
			input:   "> **bold** text",
			contain: []string{"\x0309> \x02bold\x02 text"},
		},
		{
			name:    "QuoteWithItalic",
			input:   "> *italic* text",
			contain: []string{"\x0309> \x1Ditalic\x1D text"},
		},
		{
			name:    "QuoteWithInlineCode",
			input:   "> use `code` here",
			contain: []string{"\x0309> use \x030,90code\x03 here"},
		},
		{
			name:    "QuoteWithHeading",
			input:   "> ### heading",
			contain: []string{"\x0309> \x02heading"},
		},
	})
}

func TestQuoteInsideLists(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:       "ListItemWithQuote",
			input:      "- item\n  > quoted",
			contain:    []string{"\u2022 item", "\x0309> quoted"},
			notContain: []string{},
		},
		{
			name:       "ListItemWithMultiLineQuote",
			input:      "- item\n  > line one\n  > line two",
			contain:    []string{"\u2022 item", "\x0309> line one line two"},
			notContain: []string{},
		},
		{
			name:       "NestedListItemWithQuote",
			input:      "- outer\n  - inner\n    > quoted",
			contain:    []string{"\u2022 outer", "   \u2022 inner", "\x0309> quoted"},
			notContain: []string{},
		},
		{
			name:       "QuoteInsideNestedList",
			input:      "- a\n  - b\n    > quote",
			contain:    []string{"\u2022 a", "   \u2022 b", "\x0309> quote"},
			notContain: []string{},
		},
		{
			name:    "ListItemWithQuoteSeparated",
			input:   "- item\n\n  > quoted",
			contain: []string{"\u2022 item", "\x0309> quoted"},
		},
		{
			name:    "NestedListItemWithQuoteSeparated",
			input:   "- outer\n  - inner\n\n    > quoted",
			contain: []string{"\u2022 outer", "   \u2022 inner", "\x0309> quoted"},
		},
	})
}

func TestListInsideQuotes(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "QuoteWithUnorderedList",
			input:   "> - item one\n> - item two",
			contain: []string{"\x0309>  \u2022 item one", "\x0309>  \u2022 item two"},
		},
		{
			name:    "QuoteWithOrderedList",
			input:   "> 1. first\n> 2. second",
			contain: []string{"\x0309> 1. first", "\x0309> 2. second"},
		},
		{
			name:    "QuoteWithNestedList",
			input:   "> - outer\n>   - inner",
			contain: []string{"\x0309>  \u2022 outer", "\x0309>     \u2022 inner"},
		},
	})
}

func TestQuoteWithCodeBlocks(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:       "QuoteWithCodeBlock",
			input:      "> ```\n> code\n> ```",
			contain:    []string{"\x0309>  \x030,90code\x03"},
			notContain: []string{"\n\n"},
		},
	})

	runTestsStripIRC(t, []struct {
		name         string
		input        string
		lines        []string
		checkBgColor bool
	}{
		{
			name:         "QuoteWithCodeBlockAndLang",
			input:        "> ```go\n> fmt.Println(\"hi\")\n> ```",
			lines:        []string{">  fmt.Println(\"hi\") "},
			checkBgColor: true,
		},
	})
}

func TestComplexNesting(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:  "QuoteInsideListInsideQuote",
			input: "> - item\n>   > nested quote",
			contain: []string{
				"\x0309>  \u2022 item",
				"\x0309>    \x0309> nested quote",
			},
		},
		{
			name:  "ListWithQuoteAndCode",
			input: "- text\n  > `code`",
			contain: []string{
				"\u2022 text",
				"   \x0309> \x030,90code\x03",
			},
		},
		{
			name:  "DeepListWithQuote",
			input: "- l1\n  - l2\n    - l3\n      > quoted",
			contain: []string{
				"\u2022 l1",
				"   \u2022 l2",
				"      \u2022 l3",
				"         \x0309> quoted",
			},
		},
		{
			name:  "QuoteWithListAndCode",
			input: "> - item\n>   `code`",
			contain: []string{
				"\x0309>  \u2022 item \x030,90code\x03",
			},
		},
	})
}

func TestCodeBlockFallback(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "PlainCodeBlockNoLang",
			input: "```\nplain code\n```",
		},
		{
			name:  "PlainCodeBlockWithTabs",
			input: "```\n\tindented\n```",
		},
		{
			name:  "PlainCodeBlockMultiLine",
			input: "```\nline1\nline2\nline3\n```",
		},
		{
			name:  "PlainCodeBlockUnevenLines",
			input: "```\nshort\nthis is a much longer line\n```",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToIRC(tt.input)
			if strings.Contains(got, "\n\n") {
				t.Errorf("output contains consecutive newlines\ngot: %s", humanize(got))
			}
			if !strings.Contains(got, "\x030,90") {
				t.Errorf("output missing code colour code, got: %s", humanize(got))
			}
		})
	}
}

func TestHorizontalRule(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "SimpleHR",
			input:   "---",
			contain: []string{strings.Repeat("-", 40)},
		},
		{
			name:    "HRAsterisks",
			input:   "***",
			contain: []string{strings.Repeat("-", 40)},
		},
		{
			name:    "HRUnderscores",
			input:   "___",
			contain: []string{strings.Repeat("-", 40)},
		},
		{
			name:    "HRBetweenParagraphs",
			input:   "before\n\n---\n\nafter",
			contain: []string{"before", "after", strings.Repeat("-", 40)},
		},
		{
			name:       "HRNoConsecutiveNewlines",
			input:      "text\n\n---\n\nmore text",
			notContain: []string{"\n\n"},
		},
	})
}

func TestLinks(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "SimpleLink",
			input:   "[click here](https://example.com)",
			contain: []string{"click here", "(https://example.com)"},
		},
		{
			name:    "LinkWithBoldText",
			input:   "[**bold link**](https://example.com)",
			contain: []string{"\x02bold link\x02", "(https://example.com)"},
		},
		{
			name:    "LinkWithItalicText",
			input:   "[*italic link*](https://example.com)",
			contain: []string{"\x1Ditalic link\x1D", "(https://example.com)"},
		},
		{
			name:    "LinkWithCodeText",
			input:   "[`code link`](https://example.com)",
			contain: []string{"\x030,90code link\x03", "(https://example.com)"},
		},
		{
			name:    "MultipleLinks",
			input:   "[one](https://one.com) and [two](https://two.com)",
			contain: []string{"one", "(https://one.com)", "two", "(https://two.com)"},
		},
	})
}

func TestImages(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:    "SimpleImage",
			input:   "![alt text](https://example.com/img.png)",
			contain: []string{"[image: alt text](https://example.com/img.png)"},
		},
		{
			name:    "ImageWithBoldAlt",
			input:   "![**bold alt**](https://example.com/img.png)",
			contain: []string{"[image: \x02bold alt\x02](https://example.com/img.png)"},
		},
		{
			name:    "MultipleImages",
			input:   "![first](a.png) and ![second](b.png)",
			contain: []string{"[image: first](a.png)", "[image: second](b.png)"},
		},
	})
}

func TestMixedNewFeatures(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:  "LinkAndHR",
			input: "[docs](https://docs.example.com)\n\n---\n\nSee above",
			contain: []string{
				"docs",
				"(https://docs.example.com)",
				strings.Repeat("-", 40),
				"See above",
			},
			notContain: []string{"\n\n"},
		},
		{
			name:  "ImageAndHR",
			input: "![screenshot](shot.png)\n\n---\n\ncaption",
			contain: []string{
				"[image: screenshot](shot.png)",
				strings.Repeat("-", 40),
				"caption",
			},
			notContain: []string{"\n\n"},
		},
		{
			name:  "LinkInList",
			input: "- [read more](https://example.com)\n- [docs](https://docs.example.com)",
			contain: []string{
				"\u2022",
				"read more",
				"(https://example.com)",
				"docs",
				"(https://docs.example.com)",
			},
		},
		{
			name:  "LinkInQuote",
			input: "> check [this](https://example.com)",
			contain: []string{
				"\x0309>",
				"this",
				"(https://example.com)",
			},
		},
		{
			name:  "HRAfterHeading",
			input: "# Title\n\n---\n\nbody",
			contain: []string{
				"\x02Title\x02",
				strings.Repeat("-", 40),
				"body",
			},
			notContain: []string{"\n\n"},
		},
	})
}

func TestCodeBlockWidthCapping(t *testing.T) {
	longLine := strings.Repeat("a", 120)
	shortLine := "short"

	codeRE := regexp.MustCompile(`\x03\d{1,2}(,\d{1,2})?|\x03|\x02|\x1D|\x1F`)
	stripAll := func(s string) string {
		return codeRE.ReplaceAllLiteralString(s, "")
	}

	t.Run("PlainCodeBlockAllLinesUnder80", func(t *testing.T) {
		input := "```\nfoo\nbarbaz\nqux\n```"
		got := MarkdownToIRC(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, stripAll(l))
			}
		}
		if len(codeLines) < 2 {
			t.Fatalf("expected multiple code lines, got %d", len(codeLines))
		}
		firstLen := utf8.RuneCountInString(codeLines[0])
		secondLen := utf8.RuneCountInString(codeLines[1])
		if firstLen != secondLen {
			t.Errorf("expected all lines padded to same length, got %d vs %d", firstLen, secondLen)
		}
	})

	t.Run("PlainCodeBlockMixedLengths", func(t *testing.T) {
		input := "```\n" + shortLine + "\n" + longLine + "\n```"
		got := MarkdownToIRC(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, l)
			}
		}
		if len(codeLines) != 2 {
			t.Fatalf("expected 2 code lines, got %d", len(codeLines))
		}
		shortClean := stripAll(codeLines[0])
		longClean := stripAll(codeLines[1])
		shortLen := utf8.RuneCountInString(shortClean)
		longLen := utf8.RuneCountInString(longClean)
		expectedPad := maxCodeBlockPadWidth + 2
		if shortLen != expectedPad {
			t.Errorf("short line padded to %d chars, want %d (80 + borders), got: %q", shortLen, expectedPad, shortClean)
		}
		if longLen <= shortLen {
			t.Errorf("long line should exceed padded width, got length %d vs short %d", longLen, shortLen)
		}
	})

	t.Run("PlainCodeBlockAllLinesOver80", func(t *testing.T) {
		line1 := strings.Repeat("x", 90)
		line2 := strings.Repeat("y", 100)
		input := "```\n" + line1 + "\n" + line2 + "\n```"
		got := MarkdownToIRC(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, stripAll(l))
			}
		}
		if len(codeLines) != 2 {
			t.Fatalf("expected 2 code lines, got %d", len(codeLines))
		}
		len1 := utf8.RuneCountInString(codeLines[0])
		len2 := utf8.RuneCountInString(codeLines[1])
		if len1 == len2 {
			t.Errorf("expected different lengths when all lines exceed 80, got both %d", len1)
		}
	})
}

func TestHighlightedCodeBlockWidthCapping(t *testing.T) {
	longLine := strings.Repeat("x", 120)
	shortLine := "x"

	stripAll := func(s string) string {
		return irc.StripCodes(s)
	}

	t.Run("MixedLengthsWithLanguage", func(t *testing.T) {
		input := "```go\n" + shortLine + "\n" + longLine + "\n```"
		got := MarkdownToIRC(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, l)
			}
		}
		if len(codeLines) != 2 {
			t.Fatalf("expected 2 code lines, got %d", len(codeLines))
		}
		shortClean := stripAll(codeLines[0])
		longClean := stripAll(codeLines[1])
		shortLen := utf8.RuneCountInString(shortClean)
		longLen := utf8.RuneCountInString(longClean)
		expectedPad := maxCodeBlockPadWidth + 2
		if shortLen != expectedPad {
			t.Errorf("short line padded to %d chars, want %d (80 + borders), got: %q", shortLen, expectedPad, shortClean)
		}
		if longLen <= shortLen {
			t.Errorf("long line should exceed padded width, got length %d vs short %d", longLen, shortLen)
		}
	})

	t.Run("AllLinesOver80WithLanguage", func(t *testing.T) {
		line1 := strings.Repeat("a", 90)
		line2 := strings.Repeat("b", 100)
		input := "```python\n" + line1 + "\n" + line2 + "\n```"
		got := MarkdownToIRC(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, stripAll(l))
			}
		}
		if len(codeLines) != 2 {
			t.Fatalf("expected 2 code lines, got %d", len(codeLines))
		}
		len1 := utf8.RuneCountInString(codeLines[0])
		len2 := utf8.RuneCountInString(codeLines[1])
		if len1 == len2 {
			t.Errorf("expected different lengths when all lines exceed 80, got both %d", len1)
		}
	})

	t.Run("AllLinesUnder80WithLanguage", func(t *testing.T) {
		input := "```go\nfoo\nbar\n```"
		got := MarkdownToIRC(input)
		lines := strings.Split(got, "\n")
		var codeLines []string
		for _, l := range lines {
			if strings.Contains(l, "\x030,90") {
				codeLines = append(codeLines, stripAll(l))
			}
		}
		if len(codeLines) < 2 {
			t.Fatalf("expected multiple code lines, got %d", len(codeLines))
		}
		firstLen := utf8.RuneCountInString(codeLines[0])
		secondLen := utf8.RuneCountInString(codeLines[1])
		if firstLen != secondLen {
			t.Errorf("expected all lines padded to same length, got %d vs %d", firstLen, secondLen)
		}
	})
}

func TestTables(t *testing.T) {
	runTests(t, []mdTest{
		{
			name: "SimpleTable",
			input: `| A | B |
|---|---|
| 1 | 2 |`,
			contain: []string{"┌───┬───┐", "│ A │ B │", "├───┼───┤", "│ 1 │ 2 │", "└───┴───┘"},
		},
		{
			name: "TableWithAlignment",
			input: `| Left | Center | Right |
|:-----|:------:|------:|
| a    |   b    |     c |`,
			contain: []string{"┌──────┬────────┬───────┐", "│ Left │ Center │ Right │", "├──────┼────────┼───────┤", "│ a    │   b    │     c │", "└──────┴────────┴───────┘"},
		},
		{
			name: "TableWithBold",
			input: `| **Name** | **Value** |
|----------|-----------|
| foo      | bar       |`,
			contain: []string{"┌──────┬───────┐", "│ \x02Name\x02 │ \x02Value\x02 │", "├──────┼───────┤", "│ foo  │ bar   │", "└──────┴───────┘"},
		},
		{
			name:    "TableWithInlineCode",
			input:   "| Cmd | Desc |\n|-----|------|\n| `ls` | List files |",
			contain: []string{"┌─────┬────────────┐", "│ Cmd │ Desc       │", "├─────┼────────────┤", "│ \x030,90ls\x03  │ List files │", "└─────┴────────────┘"},
		},
		{
			name: "TableWithLink",
			input: `| Site | URL |
|------|-----|
| Example | https://example.com |`,
			contain: []string{"Example", "https://example.com"},
		},
		{
			name: "TableSingleColumn",
			input: `| Item |
|------|
| one  |
| two  |`,
			contain: []string{"┌──────┐", "│ Item │", "├──────┤", "│ one  │", "│ two  │", "└──────┘"},
		},
		{
			name: "TableMultipleRows",
			input: `| A | B | C |
|---|---|---|
| 1 | 2 | 3 |
| 4 | 5 | 6 |
| 7 | 8 | 9 |`,
			contain: []string{"┌───┬───┬───┐", "│ A │ B │ C │", "├───┼───┼───┤", "│ 1 │ 2 │ 3 │", "│ 4 │ 5 │ 6 │", "│ 7 │ 8 │ 9 │", "└───┴───┴───┘"},
		},
		{
			name: "TableWithItalic",
			input: `| *Key* | *Val* |
|-------|-------|
| x     | y     |`,
			contain: []string{"│ \x1DKey\x1D │ \x1DVal\x1D │"},
		},
		{
			name:    "TableWithEscapedPipe",
			input:   "| A | B |\n|---|---|\n| Value \\| separated | other |",
			contain: []string{"┌───────────────────┬───────┐", "│ A                 │ B     │", "├───────────────────┼───────┤", "│ Value | separated │ other │", "└───────────────────┴───────┘"},
		},
		{
			name:    "TableWithLineWrap",
			input:   "| Short | Long |\n|-------|------|\n| ok    | this is a very long cell that should wrap |",
			contain: []string{"┌───────┬───────────────────────────────────────────┐", "│ Short │ Long                                      │", "├───────┼───────────────────────────────────────────┤", "│ ok    │ this is a very long cell that should wrap │", "└───────┴───────────────────────────────────────────┘"},
		},
		{
			name:    "TableWithBR",
			input:   "| A | B |\n|---|---|\n| line1<br>line2 | test |",
			contain: []string{"┌───────┬──────┐", "│ A     │ B    │", "├───────┼──────┤", "│ line1 │ test │", "│ line2 │      │", "└───────┴──────┘"},
		},
		{
			name:    "TableWithBRSlash",
			input:   "| A | B |\n|---|---|\n| line1<br/>line2 | test |",
			contain: []string{"┌───────┬──────┐", "│ A     │ B    │", "├───────┼──────┤", "│ line1 │ test │", "│ line2 │      │", "└───────┴──────┘"},
		},
		{
			name:    "TableWithBRAndWrap",
			input:   "| A | B |\n|---|---|\n| test | this is<br>a very long cell text that should wrap properly |",
			contain: []string{"┌──────┬─────────────────────────────────────────────────┐", "│ A    │ B                                               │", "├──────┼─────────────────────────────────────────────────┤", "│ test │ this is                                         │", "│      │ a very long cell text that should wrap properly │", "└──────┴─────────────────────────────────────────────────┘"},
		},
	})
}

func TestTableInContext(t *testing.T) {
	runTests(t, []mdTest{
		{
			name:       "TableAfterParagraph",
			input:      "Here is a table:\n\n| A | B |\n|---|---|\n| 1 | 2 |",
			contain:    []string{"Here is a table:", "┌───┬───┐", "│ A │ B │", "│ 1 │ 2 │"},
			notContain: []string{"\n\n"},
		},
		{
			name:       "TableBeforeParagraph",
			input:      "| A | B |\n|---|---|\n| 1 | 2 |\n\nDone.",
			contain:    []string{"┌───┬───┐", "│ A │ B │", "│ 1 │ 2 │", "Done."},
			notContain: []string{"\n\n"},
		},
		{
			name:       "TableAfterHeading",
			input:      "## Data\n\n| X |\n|---|\n| y |",
			contain:    []string{"\x02Data\x02", "┌───┐", "│ X │", "│ y │"},
			notContain: []string{"\n\n"},
		},
	})
}

func TestTableEdgeCases(t *testing.T) {
	codeRE := regexp.MustCompile(`\x03\d{1,2}(,\d{1,2})?|\x03|\x02|\x1D|\x1F`)
	stripAll := func(s string) string {
		return codeRE.ReplaceAllLiteralString(s, "")
	}

	t.Run("TableWithLongContent", func(t *testing.T) {
		longCell := strings.Repeat("x", 60)
		input := "| A | B |\n|---|---|\n| short | " + longCell + " |"
		got := MarkdownToIRC(input)
		lines := strings.Split(got, "\n")
		for _, line := range lines {
			if strings.HasPrefix(stripAll(line), "│") {
				lineLen := utf8.RuneCountInString(stripAll(line))
				if lineLen > 100 {
					t.Errorf("table line too long: %d chars", lineLen)
				}
			}
		}
	})

	t.Run("TableWithMixedFormatting", func(t *testing.T) {
		input := "| **Bold** | *Italic* | `Code` |\n|----------|----------|--------|\n| val1     | val2     | val3   |"
		got := MarkdownToIRC(input)
		if !strings.Contains(got, "\x02Bold\x02") {
			t.Errorf("missing bold in table cell, got: %s", humanize(got))
		}
		if !strings.Contains(got, "\x1DItalic\x1D") {
			t.Errorf("missing italic in table cell, got: %s", humanize(got))
		}
		if !strings.Contains(got, "\x030,90Code\x03") {
			t.Errorf("missing inline code in table cell, got: %s", humanize(got))
		}
	})

	t.Run("TableBorderAlignment", func(t *testing.T) {
		input := "| L | C | R |\n|:--|:--:|--:|\n| a | b | c |"
		got := MarkdownToIRC(input)
		lines := strings.Split(got, "\n")
		var dataLine string
		for _, line := range lines {
			clean := stripAll(line)
			if strings.Contains(clean, "│ a ") {
				dataLine = clean
				break
			}
		}
		if dataLine == "" {
			t.Fatalf("could not find data line in: %s", humanize(got))
		}
		if !strings.HasPrefix(dataLine, "│ a ") {
			t.Errorf("left alignment failed, got: %q", dataLine)
		}
		if !strings.Contains(dataLine, " b ") {
			t.Errorf("center alignment failed, got: %q", dataLine)
		}
		if !strings.Contains(dataLine, " c │") {
			t.Errorf("right alignment failed, got: %q", dataLine)
		}
	})

	t.Run("TableWrapNoFormatBleeding", func(t *testing.T) {
		input := "| A | B |\n|---|---|\n| **this is a very long bold cell that should wrap** | *short* |"
		got := MarkdownToIRC(input)
		lines := strings.Split(got, "\n")
		for _, line := range lines {
			if line == "" {
				continue
			}
			stripped := stripAll(line)
			if !strings.HasPrefix(stripped, "│") || strings.HasPrefix(stripped, "┌") || strings.HasPrefix(stripped, "├") || strings.HasPrefix(stripped, "└") {
				continue
			}
			boldCount := strings.Count(line, "\x02")
			italicCount := strings.Count(line, "\x1D")
			colorCount := strings.Count(line, "\x03")
			if boldCount%2 != 0 {
				t.Errorf("unbalanced bold on line: %q", stripped)
			}
			if italicCount%2 != 0 {
				t.Errorf("unbalanced italic on line: %q", stripped)
			}
			if colorCount%2 != 0 {
				t.Errorf("unbalanced color on line: %q", stripped)
			}
		}
	})
}

func TestTableStrictRendering(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "MixedFormattingAndWrap",
			input:    "| **Bold** | *Italic* | `Code` |\n|----------|----------|--------|\n| short | this is a long cell with **bold** that will wrap |",
			expected: "┌───────┬──────────────────────────────────────────────┬──────┐\n│ \x02Bold\x02  │ \x1DItalic\x1D                                       │ \x030,90Code\x03 │\n├───────┼──────────────────────────────────────────────┼──────┤\n│ short │ this is a long cell with \x02bold\x02 that will wrap │      │\n└───────┴──────────────────────────────────────────────┴──────┘",
		},
		{
			name:     "AlignedWithCodes",
			input:    "| Left | Center | Right |\n|:-----|:------:|------:|\n| **bold left** | `center` | *italic right* |",
			expected: "┌───────────┬────────┬──────────────┐\n│ Left      │ Center │        Right │\n├───────────┼────────┼──────────────┤\n│ \x02bold left\x02 │ \x030,90center\x03 │ \x1Ditalic right\x1D │\n└───────────┴────────┴──────────────┘",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToIRC(tt.input)
			if got != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, got)
			}
		})
	}
}
