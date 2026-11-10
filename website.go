package main

import (
	"context"
	"fmt"

	"github.com/lrstanley/girc"
)

// websiteMessageLink builds the absolute session-page anchor URL for the
// session's latest assistant message. ok=false when the website is
// unconfigured, the session is unknown (or ephemeral, sessionID 0), or it
// has no slug (pre-migration legacy row) — callers fall back to the
// pastebin path.
//
// The anchor targets the LATEST assistant row: the final assistant message
// is persisted before the send (turn.Add → sendFinalText →
// sendWithPastebin) and the session turn gate serializes per-session
// writers, so at link-build time that row IS the reply being sent. When no
// assistant row exists yet, the session page itself is linked — still a
// valid destination.
func websiteMessageLink(sessionID int64) (string, bool) {
	base := websiteURL()
	if base == "" || sessionID == 0 {
		return "", false
	}
	sess, err := getDBSessionByID(sessionID)
	if err != nil || sess == nil || !validSlug(sess.Slug) {
		return "", false
	}
	mid, ok := latestAssistantMessageID(sessionID)
	if !ok {
		// No assistant row (yet): link the session page itself.
		return fmt.Sprintf("%s/s/%s", base, sess.Slug), true
	}
	return fmt.Sprintf("%s/s/%s#m%d", base, sess.Slug, mid), true
}

// userSlugByID looks up a user's website slug. ok=false when the row is
// missing or its slug is not well-formed (legacy pre-migration rows).
func userSlugByID(userID int64) (string, bool) {
	var slug string
	err := theDB.Model(&User{}).Where("id = ?", userID).Pluck("slug", &slug).Error
	if err != nil || !validSlug(slug) {
		return "", false
	}
	return slug, true
}

// linkCommand replies with the caller's session-site URLs: the active
// session page and the user page. Plain channel messages via notice
// templates (dave "notices" are templated channel messages, not IRC
// NOTICEs).
func linkCommand(network Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
	base := websiteURL()
	if base == "" {
		n := getNotices()
		sendOrDone(ctx, output, errorNotice(n.DB.NoWebsiteURL, map[string]string{}))
		return
	}
	if !dbGuard(output, ctx) {
		return
	}
	userID := resolveIRCUserWithID(network, c, e)
	if userID == 0 {
		n := getNotices()
		sendOrDone(ctx, output, errorNotice(n.DB.NoWebsiteURL, map[string]string{}))
		return
	}
	userSlug, ok := userSlugByID(userID)
	n := getNotices()
	userURL := ""
	if ok {
		userURL = fmt.Sprintf("%s/u/%s", base, userSlug)
	}
	casemapping := getCasemapping(network.Name)
	channel := normalizeIRC(e.Params[0], casemapping)
	sess, err := sessionMgr.GetActiveSession(network.Name, channel, userID)
	if err != nil || sess == nil || !validSlug(sess.Slug) {
		if userURL == "" {
			sendOrDone(ctx, output, n.Sessions.None)
			return
		}
		sendOrDone(ctx, output, expandNotice(n.Website.LinkNoSession, map[string]string{"user_url": userURL}))
		return
	}
	if userURL == "" {
		// Session exists but the user row somehow lacks a slug: link the
		// session alone rather than failing.
		sendOrDone(ctx, output, expandNotice(n.Website.LinkNoSession, map[string]string{
			"user_url": fmt.Sprintf("%s/s/%s", base, sess.Slug)}))
		return
	}
	sendOrDone(ctx, output, expandNotice(n.Website.Link, map[string]string{
		"session_url": fmt.Sprintf("%s/s/%s", base, sess.Slug),
		"user_url":    userURL,
	}))
}
