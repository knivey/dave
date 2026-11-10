package main

import (
	"html/template"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildMessageViewMarkdown(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "**bold** and `code`")

	v := buildMessageView(m, s.Slug)
	assert.Contains(t, string(v.ContentHTML), "<strong>bold</strong>")
	assert.Contains(t, string(v.ContentHTML), "<code>code</code>")
	assert.Equal(t, "/s/aaaaaaaa/m/"+strconv.FormatInt(m.ID, 10)+"/raw", v.RawURL)
}

func TestBuildMessageViewIRCCodes(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "user", "\x02bold text\x02 plain")

	v := buildMessageView(m, s.Slug)
	assert.Contains(t, string(v.ContentHTML), `class="irc-bold"`)
	assert.NotContains(t, string(v.ContentHTML), "\x02")
}

func TestBuildMessageViewToolCalls(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "", func(msg *Message) {
		tc := `[{"id":"call_1","function":{"name":"generate_image","arguments":"{\"prompt\":\"a shrew\"}"}}]`
		msg.ToolCalls = &tc
	})
	v := buildMessageView(m, s.Slug)
	require.Len(t, v.ToolCalls, 1)
	assert.Equal(t, "generate_image", v.ToolCalls[0].Name)
	assert.Contains(t, v.ToolCalls[0].ArgsJSON, "prompt")
}

func TestBuildMessageViewReasoningCollapsed(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "answer", func(msg *Message) {
		r := "chain of thought"
		msg.ReasoningContent = &r
		sum := "short"
		msg.ReasoningSummary = &sum
	})
	v := buildMessageView(m, s.Slug)
	assert.True(t, v.HasReasoning)
	assert.Equal(t, "chain of thought", v.ReasoningContent)
}

func TestBuildMessageViewImages(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "user", "", func(msg *Message) {
		mc := `[{"type":"image_url","image_url":{"url":"https://img.example.com/x.png"}},{"type":"text","text":"look"}]`
		msg.MultiContent = &mc
	})
	v := buildMessageView(m, s.Slug)
	require.Len(t, v.ImageParts, 1)
	assert.Equal(t, "https://img.example.com/x.png", v.ImageParts[0])
	// Multicontent text is rendered too — content is empty, parts carry it.
	assert.Contains(t, string(v.ContentHTML), "look")
}

func TestSafeImageURL(t *testing.T) {
	// Unit: scheme allowlist — http/https and data:image/ pass verbatim
	// (case-insensitive), everything else blanks (javascript:, other
	// data: payloads, relative, empty).
	assert.Equal(t, template.URL("https://img.example.com/x.png"), safeImageURL("https://img.example.com/x.png"))
	assert.Equal(t, template.URL("http://img.example.com/x.png"), safeImageURL("http://img.example.com/x.png"))
	assert.Equal(t, template.URL("HTTPS://IMG.EXAMPLE.COM/X.PNG"), safeImageURL("HTTPS://IMG.EXAMPLE.COM/X.PNG"))
	assert.Equal(t, template.URL("data:image/png;base64,iVBORw0KGgo="), safeImageURL("data:image/png;base64,iVBORw0KGgo="))
	assert.Equal(t, template.URL("DATA:image/webp;base64,xxx"), safeImageURL("DATA:image/webp;base64,xxx"))
	assert.Equal(t, template.URL(""), safeImageURL("javascript:alert(1)"))
	assert.Equal(t, template.URL(""), safeImageURL("JaVaScRiPt:alert(1)"))
	assert.Equal(t, template.URL(""), safeImageURL("vbscript:msgbox(1)"))
	assert.Equal(t, template.URL(""), safeImageURL("data:text/html;base64,PHNjcmlwdD4="))
	assert.Equal(t, template.URL(""), safeImageURL("/relative.png"))
	assert.Equal(t, template.URL(""), safeImageURL(""))

	// End to end: through the template, https renders normally, the
	// data-URI image survives intact (html/template's URL filter alone
	// would rewrite it to #ZgotmplZ), and the javascript: part blanks
	// without its payload appearing anywhere in the output.
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	mc := `[{"type":"image_url","image_url":{"url":"https://img.example.com/x.png"}},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}},` +
		`{"type":"image_url","image_url":{"url":"javascript:alert(1)"}}]`
	m := mkMessage(t, db, s.ID, "user", "pics", func(msg *Message) { msg.MultiContent = &mc })
	views := buildMessageViews([]Message{m}, s.Slug)
	require.Len(t, views[0].ImageParts, 3)
	html, err := renderRowsHTML(views, nil)
	require.NoError(t, err)
	out := string(html)
	assert.Contains(t, out, `src="https://img.example.com/x.png"`)
	assert.Contains(t, out, `src="data:image/png;base64,iVBORw0KGgo="`)
	assert.Contains(t, out, `src=""`) // javascript: part blanked
	assert.NotContains(t, out, "ZgotmplZ")
	assert.NotContains(t, out, "javascript:")
}

func TestAssignUsageWindow(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	base := time.Now()
	u1 := mkMessage(t, db, s.ID, "user", "q1", func(m *Message) { m.CreatedAt = base })
	a1 := mkMessage(t, db, s.ID, "assistant", "r1", func(m *Message) { m.CreatedAt = base.Add(2 * time.Second) })
	u2 := mkMessage(t, db, s.ID, "user", "q2", func(m *Message) { m.CreatedAt = base.Add(3 * time.Second) })
	a2 := mkMessage(t, db, s.ID, "assistant", "r2", func(m *Message) { m.CreatedAt = base.Add(5 * time.Second) })
	msgs := []Message{u1, a1, u2, a2}

	usage := []TurnUsage{
		{SessionID: s.ID, Model: "m1", PromptTokens: 10, CompletionTokens: 5, CreatedAt: base.Add(1 * time.Second)},
		{SessionID: s.ID, Model: "m2", PromptTokens: 20, CompletionTokens: 8, CreatedAt: base.Add(4 * time.Second)},
		{SessionID: s.ID, Model: "m2", PromptTokens: 30, CompletionTokens: 9, CreatedAt: base.Add(4 * time.Second)},
	}
	views := buildMessageViews(msgs, s.Slug)
	assignUsage(views, msgs, usage)
	require.NotNil(t, views[1].Usage)
	assert.Equal(t, "m1", views[1].Usage.Model)
	require.NotNil(t, views[3].Usage)
	assert.Equal(t, "m2", views[3].Usage.Model)
	assert.Equal(t, 50, views[3].Usage.PromptTokens) // summed across the window's rows
	assert.Nil(t, views[0].Usage)
}

func TestRenderRowsCompactionDivider(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m1 := mkMessage(t, db, s.ID, "user", "old", func(m *Message) { m.CreatedAt = time.Now().Add(-time.Hour); m.Archived = true })
	m2 := mkMessage(t, db, s.ID, "user", "new")
	comp := Compaction{SessionID: s.ID, SummaryMessageID: m1.ID, FirstArchivedID: m1.ID,
		LastArchivedID: m1.ID, ArchivedCount: 1, Trigger: "auto", Model: "m", CreatedAt: time.Now()}
	require.NoError(t, db.Create(&comp).Error)

	views := buildMessageViews([]Message{m1, m2}, s.Slug)
	comps := []CompactionView{{Compaction: comp}}
	html, err := renderRowsHTML(views, comps)
	require.NoError(t, err)
	assert.Contains(t, string(html), "compaction-divider")
	assert.Contains(t, string(html), "archived") // archived tag on m1
}
