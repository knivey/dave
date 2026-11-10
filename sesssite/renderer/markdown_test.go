package renderer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRender_PlainText(t *testing.T) {
	result, err := Render("hello")
	assert.NoError(t, err)
	assert.Contains(t, result, "<p>hello")
}

func TestRender_Headers(t *testing.T) {
	tests := []struct {
		input string
		tag   string
	}{
		{"# Title", "<h1"},
		{"## Title", "<h2"},
		{"### Title", "<h3"},
		{"#### Title", "<h4"},
		{"##### Title", "<h5"},
		{"###### Title", "<h6"},
	}
	for _, tt := range tests {
		result, err := Render(tt.input)
		assert.NoError(t, err)
		assert.Contains(t, result, tt.tag)
	}
}

func TestRender_BoldItalic(t *testing.T) {
	result, err := Render("**bold** _italic_")
	assert.NoError(t, err)
	assert.Contains(t, result, "<strong>bold</strong>")
	assert.Contains(t, result, "<em>italic</em>")
}

func TestRender_CodeBlock(t *testing.T) {
	input := "```go\nfmt.Println(\"hello\")\n```"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "Println")
	assert.Contains(t, result, "<pre")
	assert.Contains(t, result, "<code")
}

func TestRender_CodeBlockDracula(t *testing.T) {
	input := "```python\nprint(\"hello\")\n```"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "#282a36")
	assert.Contains(t, result, "#f8f8f2")
}

func TestRender_Linkify(t *testing.T) {
	result, err := Render("visit http://example.com for more")
	assert.NoError(t, err)
	assert.Contains(t, result, `<a href="http://example.com"`)
}

func TestRender_Table(t *testing.T) {
	input := "| A | B |\n|---|---|\n| 1 | 2 |"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "<table")
	assert.Contains(t, result, "<th")
	assert.Contains(t, result, "<td")
}

func TestRender_TaskList(t *testing.T) {
	input := "- [x] done\n- [ ] todo"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "checkbox")
}

func TestRender_Empty(t *testing.T) {
	result, err := Render("")
	assert.NoError(t, err)
	assert.Equal(t, "", strings.TrimSpace(result))
}

func TestRender_SpecialChars(t *testing.T) {
	result, err := Render("<script>alert('xss')</script>")
	assert.NoError(t, err)
	assert.NotContains(t, result, "<script>")
}

func TestRender_CodeInline(t *testing.T) {
	result, err := Render("use `code` here")
	assert.NoError(t, err)
	assert.Contains(t, result, "<code>code</code>")
}

func TestRender_Blockquote(t *testing.T) {
	result, err := Render("> quote text")
	assert.NoError(t, err)
	assert.Contains(t, result, "<blockquote")
}

func TestRender_OrderedList(t *testing.T) {
	result, err := Render("1. first\n2. second\n3. third")
	assert.NoError(t, err)
	assert.Contains(t, result, "<ol")
	assert.Contains(t, result, "<li")
}

func TestRender_UnorderedList(t *testing.T) {
	result, err := Render("- first\n- second\n- third")
	assert.NoError(t, err)
	assert.Contains(t, result, "<ul")
	assert.Contains(t, result, "<li")
}

func TestRender_Strikethrough(t *testing.T) {
	result, err := Render("~~deleted~~")
	assert.NoError(t, err)
	assert.Contains(t, result, "<del>deleted</del>")
}

func TestRender_Image(t *testing.T) {
	result, err := Render("![alt text](http://example.com/img.png)")
	assert.NoError(t, err)
	assert.Contains(t, result, "<img")
	assert.Contains(t, result, `src="http://example.com/img.png"`)
	assert.Contains(t, result, `alt="alt text"`)
}

func TestRender_ExplicitLink(t *testing.T) {
	result, err := Render("[click here](http://example.com)")
	assert.NoError(t, err)
	assert.Contains(t, result, `<a href="http://example.com"`)
	assert.Contains(t, result, "click here")
}

func TestRender_HardWraps(t *testing.T) {
	result, err := Render("line one\nline two")
	assert.NoError(t, err)
	assert.Contains(t, result, "<br />")
}

func TestRender_CodeBlockNoLanguage(t *testing.T) {
	input := "```\nsome code\n```"
	result, err := Render(input)
	assert.NoError(t, err)
	assert.Contains(t, result, "<pre")
	assert.Contains(t, result, "<code")
}
