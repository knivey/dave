package main

import "fmt"

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
