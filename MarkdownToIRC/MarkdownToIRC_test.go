package markdowntoirc

import (
	"regexp"
	"strings"
	"testing"
)

var colorRETest = regexp.MustCompile(`\x03(\d{2})?(,\d{2})?`)

func humanize(s string) string {
	s = strings.ReplaceAll(s, "\x02", "[BOLD]")
	s = strings.ReplaceAll(s, "\x1D", "[ITALIC]")
	s = strings.ReplaceAll(s, "\x1F", "[UNDERLINE]")
	s = colorRETest.ReplaceAllString(s, "[COLOR${1}${2}]")
	s = strings.ReplaceAll(s, "\u2022", "[BULLET]")
	return s
}

type mdTest struct {
	name       string
	input      string
	contain    []string
	notContain []string
}

func runTests(t *testing.T, tests []mdTest) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MarkdownToIRC(tt.input)
			for _, want := range tt.contain {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\ngot:  %s\nwant: %s", humanize(want), humanize(got), humanize(want))
				}
			}
			for _, notwant := range tt.notContain {
				if strings.Contains(got, notwant) {
					t.Errorf("output unexpectedly contains %q\ngot:  %s", humanize(notwant), humanize(got))
				}
			}
		})
	}
}

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
			contain:    []string{"\n\x02Hello"},
			notContain: []string{"\x02\n", "\x02\x02"},
		},
		{
			name:    "HeadingFollowedByParagraph",
			input:   "# Title\n\nBody text",
			contain: []string{"\n\x02Title", "Body text"},
		},
		{
			name:    "MultipleHeadings",
			input:   "# One\n\n## Two",
			contain: []string{"\n\x02One", "\n\x02Two"},
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
			name:    "CodeBlockWithLang",
			input:   "```python\nprint(\"hi\")\n```",
			contain: []string{"\x030,90"},
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
			contain:    []string{"\u2022 outer", "\u2022 inner:", "\x030,90code\x03"},
			notContain: []string{"\n\n"},
		},
		{
			name:       "CodeBlockAfterListItemParagraph",
			input:      "1. **Title:**\n   - text:\n     ```\n     sudo ip route add\n     ```",
			contain:    []string{"1. \x02Title:\x02", "\u2022 text:", "\x030,90sudo ip route add\x03"},
			notContain: []string{"\n\n"},
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
			contain: []string{"\x0309> line one", "\x0309> line two"},
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
			contain: []string{"line1\nline2"},
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
			contain: []string{"\n\x02Title"},
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
			contain: []string{"\x0309> line one", "\x0309> line two"},
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
			contain:    []string{"\u2022 item", "   > quoted"},
			notContain: []string{"\x0309"},
		},
		{
			name:       "ListItemWithMultiLineQuote",
			input:      "- item\n  > line one\n  > line two",
			contain:    []string{"\u2022 item", "   > line one", "   > line two"},
			notContain: []string{"\x0309"},
		},
		{
			name:       "NestedListItemWithQuote",
			input:      "- outer\n  - inner\n    > quoted",
			contain:    []string{"\u2022 outer", "   \u2022 inner", "      > quoted"},
			notContain: []string{"\x0309"},
		},
		{
			name:       "QuoteInsideNestedList",
			input:      "- a\n  - b\n    > quote",
			contain:    []string{"\u2022 a", "   \u2022 b", "      > quote"},
			notContain: []string{"\x0309"},
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
		{
			name:    "QuoteWithCodeBlockAndLang",
			input:   "> ```go\n> fmt.Println(\"hi\")\n> ```",
			contain: []string{"\x0309>  \x030,90"},
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
				"\x0309>    > nested quote",
			},
		},
		{
			name:  "ListWithQuoteAndCode",
			input: "- text\n  > `code`",
			contain: []string{
				"\u2022 text",
				"   > \x030,90code\x03",
			},
		},
		{
			name:  "DeepListWithQuote",
			input: "- l1\n  - l2\n    - l3\n      > quoted",
			contain: []string{
				"\u2022 l1",
				"   \u2022 l2",
				"      \u2022 l3",
				"         > quoted",
			},
		},
		{
			name:  "QuoteWithListAndCode",
			input: "> - item\n>   `code`",
			contain: []string{
				"\x0309>  \u2022 item\x030,90code\x03",
			},
		},
	})
}
