package main

// reactions.go — emoji reactions (owner request, Oct 2026), the third
// anonymous-feedback channel alongside likes/dislikes. Same identity
// (the imgsite_liker cookie token, shared mint-and-rate-limit
// machinery), same visibility contract as the details page, same
// commit-before-publish SSE ordering — but a DIFFERENT cardinality:
// a token may hold many different reactions on one image (Discord/
// Slack model), one row per (image_id, token, emoji). The composite
// PK is the entire dedupe story, per emoji exactly the insert-first
// toggle pattern the likes table used before dislikes:
//
//   - INSERT ... ON CONFLICT (image_id, token, emoji) DO NOTHING:
//     1 row affected means the toggle REACTED; zero rows means this
//     token already had that reaction, so the toggle REMOVES it via
//     DELETE.
//   - Which emojis exist is CONFIG ([reactions], hot-reloadable —
//     see config.go). Rows key on the stable ASCII name; the glyph is
//     rendered from the live config, so curation never migrates data.
//     Rows for removed names stay dormant and revive if the name
//     returns. `reactions.enabled = false` hides every surface and
//     404s the endpoints without touching stored rows.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/jmoiron/sqlx"
)

// cardReactionBadges is how many reaction badges a gallery card shows
// (top N by count, configured order breaking ties). Owner choice
// (Oct 2026): cards carry the signal without turning the meta line
// into a second prompt.
const cardReactionBadges = 2

// dbToggleReaction flips token's reaction of emoji on id and returns
// whether the caller now holds that reaction plus the image's full
// post-toggle reaction tally (emoji name → count; absent = zero —
// never stored as explicit zeros).
//
// The insert-first order makes the toggle idempotent under the DB's
// single connection: two racing toggles from the same token both fail
// the INSERT, both run the DELETE, and the second DELETE affects zero
// rows — the reported state (un-reacted, tally as-of-then) is
// coherent either way. Exactly the pre-dislikes dbToggleLike shape,
// scoped to one emoji.
func dbToggleReaction(db *sqlx.DB, id, token, emoji string) (reacted bool, tally map[string]int, err error) {
	res, err := db.Exec(
		`INSERT INTO reactions (image_id, token, emoji, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (image_id, token, emoji) DO NOTHING`,
		id, token, emoji, formatDBTimeNow())
	if err != nil {
		return false, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	reacted = n > 0
	if !reacted {
		if _, err := db.Exec(`DELETE FROM reactions WHERE image_id = ? AND token = ? AND emoji = ?`, id, token, emoji); err != nil {
			return false, nil, err
		}
	}
	tally, err = dbReactionTally(db, id)
	if err != nil {
		return false, nil, err
	}
	return reacted, tally, nil
}

// dbReactionTally returns one image's full reaction tally (emoji →
// count). Unlike dbVoteCounts there is no FROM images anchor: a
// GROUP BY over zero rows and over a nonexistent image are
// indistinguishable, so the anchor would add ceremony without a
// tripwire — the handler's lookupImage gate is the real boundary.
func dbReactionTally(db *sqlx.DB, id string) (map[string]int, error) {
	rows, err := db.Query(
		`SELECT emoji, COUNT(*) FROM reactions WHERE image_id = ? GROUP BY emoji`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tally := make(map[string]int)
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return nil, err
		}
		tally[name] = n
	}
	return tally, rows.Err()
}

// dbGetReactionState returns an image's full tally and the subset of
// emojis token itself holds, in one round trip. Unknown image ids
// yield empty maps, not errors (see dbReactionTally's anchor note).
func dbGetReactionState(db *sqlx.DB, id, token string) (tally map[string]int, mine map[string]bool, err error) {
	rows, err := db.Query(
		`SELECT emoji, COUNT(*),
		        MAX(CASE WHEN token = ? THEN 1 ELSE 0 END)
		 FROM reactions WHERE image_id = ? GROUP BY emoji`, token, id)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	tally = make(map[string]int)
	mine = make(map[string]bool)
	for rows.Next() {
		var name string
		var n, isMine int
		if err := rows.Scan(&name, &n, &isMine); err != nil {
			return nil, nil, err
		}
		tally[name] = n
		if isMine == 1 {
			mine[name] = true
		}
	}
	return tally, mine, rows.Err()
}

// hydrateReactionCounts batch-fills Reactions on the given rows with
// one grouped query (image AND emoji, one pass for every image's
// whole tally). Rows whose ids have no reactions keep a nil map —
// nil means "no reactions", and every consumer treats nil and empty
// identically. Callers pass page-sized slices, far under SQLite's
// parameter limit.
func hydrateReactionCounts(db *sqlx.DB, imgs []*dbImage) error {
	if len(imgs) == 0 {
		return nil
	}
	placeholders := make([]string, len(imgs))
	args := make([]any, len(imgs))
	for i, img := range imgs {
		placeholders[i] = "?"
		args[i] = img.ID
	}
	rows, err := db.Query(
		`SELECT image_id, emoji, COUNT(*) FROM reactions WHERE image_id IN (`+strings.Join(placeholders, ",")+`)
		 GROUP BY image_id, emoji`,
		args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type key struct{ id, emoji string }
	counts := make(map[key]int)
	for rows.Next() {
		var k key
		var n int
		if err := rows.Scan(&k.id, &k.emoji, &n); err != nil {
			return err
		}
		counts[k] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, img := range imgs {
		var tally map[string]int
		for k, n := range counts {
			if k.id == img.ID {
				if tally == nil {
					tally = make(map[string]int)
				}
				tally[k.emoji] = n
			}
		}
		img.Reactions = tally
	}
	return nil
}

// topCardReactions picks the card-badge slice from a hydrated tally:
// configured emojis only, top N by count, configured order breaking
// ties. Dormant rows (names no longer configured) are invisible by
// construction — the badge can only show a glyph the config supplies.
// reactionsEnabled is re-checked defensively: normalize already nils
// the list when disabled, so this only matters for configs that
// bypassed loadConfig (struct literals in tests, future callers).
func topCardReactions(cfg Config, tally map[string]int) []reactionBadge {
	if !cfg.Reactions.reactionsEnabled() {
		return nil
	}
	var badges []reactionBadge
	for _, e := range cfg.Reactions.Emojis {
		if n := tally[e.Name]; n > 0 {
			badges = append(badges, reactionBadge{Name: e.Name, Glyph: e.Glyph, Count: n})
		}
	}
	// Stable sort by count desc; configured order survives ties.
	sort.SliceStable(badges, func(i, j int) bool { return badges[i].Count > badges[j].Count })
	if len(badges) > cardReactionBadges {
		badges = badges[:cardReactionBadges]
	}
	return badges
}

// publishImageReacted fans out image-reacted for a toggle that just
// committed (the handler calls this only after dbToggleReaction
// returned, honoring the hub's commit-before-publish ordering). The
// tally is the FULL absolute map (all stored emoji names, including
// ones the current config no longer lists — the client filters
// through its embedded glyph map), so a subscriber that missed
// earlier events still converges. Per-site visibility computed once
// from the row, same as publishImageLiked.
func (a *App) publishImageReacted(img *dbImage, tally map[string]int) {
	if a.events == nil {
		return
	}
	a.events.publishVisible(eventImageReacted, imageReactedEvent{
		ID:        img.ID,
		Reactions: tally,
	}, siteCanSee(safeSiteCtx(a.getConfig().SafeSite), img))
}

// reactionResponse is the JSON shape the JS enhancement consumes:
// the toggled emoji, whether the caller now holds it, and the full
// absolute tally so the client can re-render the whole row.
type reactionResponse struct {
	Emoji     string         `json:"emoji"`
	Reacted   bool           `json:"reacted"`
	Reactions map[string]int `json:"reactions"`
}

// handleReactionToggle implements POST /{id}/react/{emoji} — the
// per-emoji toggle for both the JS enhancement (Accept:
// application/json → 200 + JSON) and the no-JS form path (303 back
// to the details page). Shares the vote endpoints' contract exactly:
// identity cookie, per-IP bucket, visibility gates, response
// negotiation. An emoji name outside the live configured set 404s —
// the set is public in the page markup, so this is correctness (no
// toggling invisible things), not secrecy. When the feature is
// disabled the endpoint 404s for every emoji, keeping the route
// shape stable across config flips.
func (a *App) handleReactionToggle(w http.ResponseWriter, r *http.Request) {
	cfg := a.getConfig()
	if !cfg.Reactions.reactionsEnabled() {
		http.NotFound(w, r)
		return
	}
	id := r.PathValue("id")
	if !validImageID(id) {
		http.NotFound(w, r)
		return
	}
	emoji := r.PathValue("emoji")
	if _, ok := cfg.Reactions.reactionByName(emoji); !ok {
		http.NotFound(w, r)
		return
	}
	sc := a.resolveSite(r)
	img, ok := a.lookupImage(w, r, id)
	if !ok {
		return
	}
	if img.Hidden {
		http.Error(w, "gone", http.StatusGone)
		return
	}
	if !siteCanSee(sc, img) {
		http.NotFound(w, r)
		return
	}
	if !a.likeLimiter.allow(clientIP(r)) {
		http.Error(w, "too many like requests", http.StatusTooManyRequests)
		return
	}

	token, minted := likeTokenFromRequest(r)
	reacted, tally, err := dbToggleReaction(a.db, id, token, emoji)
	if err != nil {
		logger.Error("reaction toggle failed", "id", id, "emoji", emoji, "error", err)
		http.Error(w, "storage failure", http.StatusInternalServerError)
		return
	}

	// Publish only after the toggle committed. Nil-hub-safe.
	a.publishImageReacted(img, tally)

	if minted {
		// Set-Cookie must precede http.Redirect/Encode, both of which
		// write the response immediately.
		http.SetCookie(w, &http.Cookie{
			Name:     likeCookieName,
			Value:    token,
			Path:     "/",
			MaxAge:   likeCookieMaxAge,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		w.Header().Set("Content-Type", "application/json")
		// A toggle response must never be replayed from any cache.
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(reactionResponse{Emoji: emoji, Reacted: reacted, Reactions: tally}); err != nil {
			logger.Error("writing reaction response", "id", id, "emoji", emoji, "error", err)
		}
		return
	}
	http.Redirect(w, r, "/"+id, http.StatusSeeOther)
}
