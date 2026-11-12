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
// same styled spans IRC users saw — the site's translator is a verbatim
// port of dave's emission-layer one.
func TestBBCodeAssistantContent(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "Peak shrew. [color=red]RED[/color] and [b]bold[/b] plain")

	v := buildMessageView(m, s.Slug, viewOpts{BBCode: true})
	html := string(v.ContentHTML)
	assert.Contains(t, html, `irc-fg-4`, "red maps to IRC 4 → styled span")
	assert.Contains(t, html, ">RED</span>")
	assert.Contains(t, html, "[b]bold[/b]", "styles off: toggles ride the unknown-token path verbatim")
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

// TestBBCodeStylesGate: with styles on, toggles translate; off, they
// stay verbatim — mirroring dave's bbcode_styles knob.
func TestBBCodeStylesGate(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "[b]bold[/b]")

	on := buildMessageView(m, s.Slug, viewOpts{BBCode: true, BBCodeStyles: true})
	assert.Contains(t, string(on.ContentHTML), "irc-bold")
	assert.NotContains(t, string(on.ContentHTML), "[b]")

	off := buildMessageView(m, s.Slug, viewOpts{BBCode: true})
	assert.Contains(t, string(off.ContentHTML), "[b]bold[/b]")
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

// TestBBCodeInlineCodeStripped: BBCode inside an inline code span must
// not leak the translated IRC control codes (the color digits would
// render as text). The transformer strips codes in code spans.
func TestBBCodeInlineCodeStripped(t *testing.T) {
	db := newTestDB(t)
	s := mkSession(t, db, "aaaaaaaa")
	m := mkMessage(t, db, s.ID, "assistant", "write `[color=red]warn[/color]` for red")

	v := buildMessageView(m, s.Slug, viewOpts{BBCode: true})
	html := string(v.ContentHTML)
	assert.NotContains(t, html, "\x03", "no control bytes reach the browser")
	assert.Contains(t, html, "warn")
	assert.NotContains(t, html, "04warn", "color digits must not leak into code text")
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
	assert.Contains(t, body, ">Peak</span>")
	assert.Contains(t, body, "shrew.")
	assert.False(t, strings.Contains(body, "[color=red]"), "the tag itself must not render")
}
