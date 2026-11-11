package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/knivey/dave/sesssite/renderer"
)

// templatesFS holds every template under sesssite/templates. The embed
// lives here (Task 7) because rows.html is parsed at package init; Task
// 8's page templates share the same FS.
//
//go:embed templates
var templatesFS embed.FS

var rowsTpl = template.Must(template.New("rows.html").
	Funcs(template.FuncMap{"safeImageURL": safeImageURL}).
	ParseFS(templatesFS, "templates/rows.html"))

type ToolCallView struct {
	ID       string
	Name     string
	ArgsJSON string
}

type UsageView struct {
	Model, Service, FinishReason, APIPath string
	PromptTokens, CompletionTokens        int
	CachedTokens, ReasoningTokens         int
	DurationMs                            int
}

type MessageView struct {
	ID               int64
	Role             string
	Time             time.Time
	Content          string
	ContentHTML      template.HTML
	ToolCalls        []ToolCallView
	ToolCallID       string
	HasReasoning     bool
	ReasoningContent string
	ReasoningSummary string
	ImageParts       []string
	Archived         bool
	RawURL           string
	Usage            *UsageView
	IsSystem         bool
}

// CompactionView is a Compaction row plus the rendered summary message
// content, filled in by renderRowsHTML when the summary system message
// is among the page's views.
type CompactionView struct {
	Compaction
	SummaryHTML template.HTML
}

// messagePart mirrors the stored multicontent JSON array shape.
type messagePart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

// toolCallJSON mirrors the stored tool_calls JSON array shape.
type toolCallJSON struct {
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// buildMessageView converts one stored row into a render-ready view.
// User/assistant content renders as markdown (IRC codes styled by the
// ported transformer); tool results render escaped in <pre> via the
// template (no markdown); system rows are template-collapsed.
//
// Content carries the RAW stored content alongside ContentHTML: the
// template's tool-result branch interpolates it escaped (html/template
// does the escaping) — tool output is model/tool text, never markdown.
func buildMessageView(m Message, sessionSlug string) MessageView {
	v := MessageView{
		ID: m.ID, Role: m.Role, Time: m.CreatedAt,
		Content:  m.Content,
		Archived: m.Archived,
		RawURL:   "/s/" + sessionSlug + "/m/" + fmtInt(m.ID) + "/raw",
		IsSystem: m.Role == "system",
	}
	if m.ReasoningContent != nil && *m.ReasoningContent != "" {
		v.HasReasoning = true
		v.ReasoningContent = *m.ReasoningContent
		if m.ReasoningSummary != nil {
			v.ReasoningSummary = *m.ReasoningSummary
		}
	} else if m.ReasoningSummary != nil && *m.ReasoningSummary != "" {
		v.HasReasoning = true
		v.ReasoningSummary = *m.ReasoningSummary
	}
	if m.ToolCallID != nil {
		v.ToolCallID = *m.ToolCallID
	}
	if m.ToolCalls != nil {
		var calls []toolCallJSON
		if json.Unmarshal([]byte(*m.ToolCalls), &calls) == nil {
			for _, c := range calls {
				v.ToolCalls = append(v.ToolCalls, ToolCallView{ID: c.ID, Name: c.Function.Name, ArgsJSON: prettyJSON(c.Function.Arguments)})
			}
		}
	}
	var textParts []string
	if m.MultiContent != nil {
		var parts []messagePart
		if json.Unmarshal([]byte(*m.MultiContent), &parts) == nil {
			for _, p := range parts {
				switch {
				case p.Type == "image_url" && p.ImageURL != nil:
					v.ImageParts = append(v.ImageParts, p.ImageURL.URL)
				case p.Type == "text" && p.Text != "":
					textParts = append(textParts, p.Text)
				}
			}
		}
	}
	content := m.Content
	if content == "" && len(textParts) > 0 {
		content = strings.Join(textParts, "\n\n")
	}
	if content != "" && (m.Role == "user" || m.Role == "assistant") {
		if html, err := renderer.Render(content); err == nil {
			v.ContentHTML = template.HTML(html)
		} else {
			// Degradation: escaped plain text — content is never lost.
			v.ContentHTML = template.HTML(template.HTMLEscapeString(content))
		}
	}
	return v
}

func buildMessageViews(msgs []Message, sessionSlug string) []MessageView {
	views := make([]MessageView, len(msgs))
	for i, m := range msgs {
		views[i] = buildMessageView(m, sessionSlug)
	}
	return views
}

// assignUsage joins turn_usage rows to assistant messages by time window:
// rows in (previous assistant message, this assistant message] belong to
// this message. Multiple rows (tool-loop iterations) sum; the LAST row's
// model/service/finish/API path win (they describe the final call).
// Display-only, approximate by construction (usage rows carry no message
// id) — the spec labels it as such. usage must be ordered by created_at
// (sessionUsageRows guarantees it: created_at ASC, id ASC).
func assignUsage(views []MessageView, msgs []Message, usage []TurnUsage) {
	ui := 0
	prev := time.Time{}
	for i := range views {
		if msgs[i].Role != "assistant" {
			continue
		}
		uv := &UsageView{}
		seen := false
		for ui < len(usage) && !usage[ui].CreatedAt.After(msgs[i].CreatedAt) {
			if usage[ui].CreatedAt.After(prev) {
				uv.PromptTokens += usage[ui].PromptTokens
				uv.CompletionTokens += usage[ui].CompletionTokens
				uv.CachedTokens += usage[ui].CachedTokens
				uv.ReasoningTokens += usage[ui].ReasoningTokens
				uv.DurationMs += usage[ui].DurationMs
				uv.Model, uv.Service = usage[ui].Model, usage[ui].Service
				uv.FinishReason, uv.APIPath = usage[ui].FinishReason, usage[ui].APIPath
				seen = true
			}
			ui++
		}
		if seen {
			views[i].Usage = uv
		}
		prev = msgs[i].CreatedAt
	}
}

// rowsData is the template data for rows.html. CompBefore maps a view
// index to the compaction dividers rendered immediately BEFORE that row.
// Comps exists as the template's cheap "any compactions at all" guard.
type rowsData struct {
	Views      []MessageView
	Comps      []CompactionView
	CompBefore map[int][]CompactionView
}

// renderRowsHTML executes rows.html over the views with compaction
// dividers interleaved. A divider for compaction C renders immediately
// BEFORE the first message with ID > C.LastArchivedID — the
// post-compaction boundary (the summary row itself is a system message
// inside the archived range and renders on its own). When the boundary
// message is not on this page (older/newer chunk), the divider renders
// before the first row that follows the archived range, or not at all
// when the whole page precedes the boundary.
//
// SummaryHTML renders the summary system message's content via
// renderer.Render when that message is among the page's views (lookup
// by SummaryMessageID); else empty.
func renderRowsHTML(views []MessageView, comps []CompactionView) (template.HTML, error) {
	fillCompactionSummaries(comps, views)
	compBefore := make(map[int][]CompactionView)
	for _, c := range comps {
		for i := range views {
			if views[i].ID > c.LastArchivedID {
				compBefore[i] = append(compBefore[i], c)
				break
			}
		}
	}
	var buf bytes.Buffer
	if err := rowsTpl.Execute(&buf, rowsData{Views: views, Comps: comps, CompBefore: compBefore}); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// fillCompactionSummaries fills each compaction's SummaryHTML from the
// summary system message when it is among the views (lookup by
// SummaryMessageID); missing rows leave the previous value untouched.
// Extracted from renderRowsHTML so hide_system_prompts callers can fill
// BEFORE dropping system rows — renderRowsHTML's internal call then
// finds no matching row and the pre-filled summary survives in the
// divider.
func fillCompactionSummaries(comps []CompactionView, views []MessageView) {
	for k := range comps {
		for i := range views {
			if views[i].ID == comps[k].SummaryMessageID {
				if html, err := renderer.Render(views[i].Content); err == nil {
					comps[k].SummaryHTML = template.HTML(html)
				} else {
					comps[k].SummaryHTML = template.HTML(template.HTMLEscapeString(views[i].Content))
				}
				break
			}
		}
	}
}

// dropSystemViews removes system rows for the hide_system_prompts
// option. Call AFTER fillCompactionSummaries (summaries are system rows
// whose content rides the divider) and AFTER assignUsage (the usage
// join walks views and msgs index-parallel). Guidance injections are
// system rows too and hide with the prompts by design — dave stores no
// kind column distinguishing prompt from guidance.
func dropSystemViews(views []MessageView) []MessageView {
	out := make([]MessageView, 0, len(views))
	for _, v := range views {
		if !v.IsSystem {
			out = append(out, v)
		}
	}
	return out
}

// safeImageURL allowlists conversation-image src schemes and returns the
// URL as template.URL so html/template's URL filter trusts it. Without
// this, the filter rewrites EVERY data: URI to "#ZgotmplZ" — and stored
// multicontent image parts are base64 data URIs in production (spec:
// "<img> for both URL and data-URI forms"). This is not blanket trust:
// only http:, https:, and data:image/ prefixes pass (case-insensitive);
// everything else — javascript:, vbscript:, data:text/html, relative
// paths — returns "" (an empty src renders a broken-image glyph, never
// executable content). Attribute escaping still applies to the value, so
// it cannot break out of the src attribute.
func safeImageURL(s string) template.URL {
	l := strings.ToLower(s)
	if strings.HasPrefix(l, "http:") || strings.HasPrefix(l, "https:") || strings.HasPrefix(l, "data:image/") {
		return template.URL(s)
	}
	return ""
}

// prettyJSON indents JSON for display; non-JSON input returns verbatim.
func prettyJSON(s string) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
		return s
	}
	return buf.String()
}

func fmtInt(i int64) string { return strconv.FormatInt(i, 10) }
