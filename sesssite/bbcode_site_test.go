package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBBCodeAssistantContent: with bbcode on, assistant rows show the
// same styled spans IRC users saw — both sides share the
// markdowntoirc parser; this package renders its nodes as HTML spans.
func TestBBCodeAssistantContent(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "Peak shrew. [color=red]RED[/color] and [b]bold[/b] plain")

	v := buildMessageView(m, s.Slug, viewOpts{BBCode: true})
	html := string(v.ContentHTML)
	assert.Contains(t, html, `irc-fg-4`, "red maps to IRC 4 → styled span")
	assert.Contains(t, html, ">RED</span>")
	assert.Contains(t, html, "[b]bold[/b]", "toggles are unknown tokens — verbatim (owner ruling 2026-11-13)")
	assert.NotContains(t, html, "[color=red]")
}

// TestBBCodeUserRowsNotTranslated: user rows keep literal brackets —
// dave translates only model output at IRC emission.
func TestBBCodeUserRowsNotTranslated(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "user", "I typed [color=red]this[/color] and [b]that[/b] literally")

	v := buildMessageView(m, s.Slug, viewOpts{BBCode: true})
	assert.Contains(t, string(v.ContentHTML), "[color=red]")
	assert.Contains(t, string(v.ContentHTML), "[b]")
}

// TestBBCodeDisabledVerbatim: explicit bbcode = false renders tags
// literally everywhere.
func TestBBCodeDisabledVerbatim(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "[color=red]RED[/color]")

	v := buildMessageView(m, s.Slug, viewOpts{})
	assert.Contains(t, string(v.ContentHTML), "[color=red]")
}

// TestBBCodeInlineCodeLiteral: BBCode inside an inline code span stays
// literal — the shared parser never runs inside code spans (backtick
// content is consumed raw), so no tags, no codes, no spans.
func TestBBCodeInlineCodeLiteral(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "write `[color=red]warn[/color]` for red")

	v := buildMessageView(m, s.Slug, viewOpts{BBCode: true})
	html := string(v.ContentHTML)
	assert.Contains(t, html, "[color=red]warn[/color]", "tags literal inside the code span")
	assert.NotContains(t, html, "\x03", "no control bytes reach the browser")
	assert.NotContains(t, html, "irc-fg-", "no styled span inside the code span")
}

// TestBBCodeReasoningTranslated: reasoning is model output — translated
// with the same opts.
func TestBBCodeReasoningTranslated(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "answer", func(msg *Message) {
		r := "think [color=red]urgent[/color]"
		msg.ReasoningContent = &r
	})

	v := buildMessageView(m, s.Slug, viewOpts{BBCode: true})
	assert.Contains(t, string(v.ReasoningHTML), "irc-fg-4")
}

// TestBBCodePageRendering: end-to-end through the handler with the
// config default (bbcode on) — the production complaint shape.
func TestBBCodePageRendering(t *testing.T) {
	db := newTestDB(t)
	srv := NewServer(db, &Config{})
	ts := srv.mux()
	s := mkSession(t, db, "aaaaaaaa")
	mkMessage(t, db, s.ID, "assistant",
		"Now that's a shrew with presence. [color=red]Peak shrew.[/color]")

	req, err := http.NewRequest("GET", "/s/"+s.Slug, nil)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	ts.ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, `irc-fg-4`, "red maps to a styled span")
	// One contiguous span — the shared parser owns the whole tag range
	// (the old translator+IRC-transformer pipeline split it into two
	// adjacent spans, one per underlying Text node).
	assert.Contains(t, body, ">Peak shrew.</span>")
	assert.Contains(t, body, "shrew.")
	assert.False(t, strings.Contains(body, "[color=red]"), "the tag itself must not render")
}
