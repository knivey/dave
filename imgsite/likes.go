package main

// likes.go — anonymous image likes: the DB layer (this file) and, in
// later milestones of the same feature, the cookie identity + POST
// toggle handler. Design summary (full version in docs/image-site.md
// "Likes"):
//
//   - Identity is a cookie token minted on the visitor's FIRST like
//     and never before — browsers that never like carry no cookie.
//   - One row in likes = one like; the (image_id, token) PK is the
//     entire dedupe mechanism.
//   - Toggling is INSERT ... ON CONFLICT DO NOTHING first: one row
//     affected means the toggle LIKED; zero rows means this token had
//     already liked, so the toggle UN-likes via DELETE.
//   - Likes are global per image (not per logical site): the count is
//     a property of the row, and both hosts render the same number.
//     Host-scoped cookies mean a visitor using BOTH the default and
//     the safe host holds two tokens and could like twice — accepted,
//     this is a casual anonymous feature, not a boundary.

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// dbToggleLike flips token's like on id and returns the post-toggle
// state: whether the caller now likes the image and its new count.
//
// The insert-first order makes the toggle idempotent under the DB's
// single connection: two racing toggles from the same token both fail
// the INSERT, both run the DELETE, and the second DELETE affects zero
// rows — the reported state (unliked, count as-of-then) is coherent
// either way.
func dbToggleLike(db *sqlx.DB, id, token string) (liked bool, count int, err error) {
	now := formatDBTimeNow()
	res, err := db.Exec(
		`INSERT INTO likes (image_id, token, created_at) VALUES (?, ?, ?)
		 ON CONFLICT (image_id, token) DO NOTHING`,
		id, token, now)
	if err != nil {
		return false, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, err
	}
	liked = n > 0
	if !liked {
		if _, err := db.Exec(`DELETE FROM likes WHERE image_id = ? AND token = ?`, id, token); err != nil {
			return false, 0, err
		}
	}
	if err := db.Get(&count, `SELECT COUNT(*) FROM likes WHERE image_id = ?`, id); err != nil {
		return false, 0, err
	}
	return liked, count, nil
}

// dbGetLikeState returns an image's like count and whether token has
// already liked it, in one round trip. Unknown image ids surface as
// sql.ErrNoRows (the FROM images anchor refuses to fabricate a state
// for a row that does not exist); production callers always look the
// row up first, so this is a defensive tripwire, not a code path.
func dbGetLikeState(db *sqlx.DB, id, token string) (count int, liked bool, err error) {
	var state struct {
		Count int  `db:"cnt"`
		Liked bool `db:"liked"`
	}
	err = db.Get(&state, `
		SELECT (SELECT COUNT(*) FROM likes l WHERE l.image_id = i.id) AS cnt,
		       EXISTS(SELECT 1 FROM likes l WHERE l.image_id = i.id AND l.token = ?) AS liked
		FROM images i WHERE i.id = ?`, token, id)
	if err != nil {
		return 0, false, err
	}
	return state.Count, state.Liked, nil
}

// hydrateLikeCounts batch-fills LikeCount on the given rows with one
// grouped query. Rows whose ids have no likes keep the zero value —
// absence in the GROUP BY result means zero, not "unknown". Callers
// pass page-sized slices (tens of rows), far under SQLite's parameter
// limit, so no chunking exists.
func hydrateLikeCounts(db *sqlx.DB, imgs []*dbImage) error {
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
		`SELECT image_id, COUNT(*) FROM likes WHERE image_id IN (`+strings.Join(placeholders, ",")+`)
		 GROUP BY image_id`,
		args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := make(map[string]int, len(imgs))
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return err
		}
		counts[id] = n
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, img := range imgs {
		img.LikeCount = counts[img.ID]
	}
	return nil
}

// formatDBTimeNow is the shared "now" stamp for like rows (kept as a
// helper so future writers cannot drift from dbTimeFormat).
func formatDBTimeNow() string {
	return time.Now().UTC().Format(dbTimeFormat)
}

// publishImageLiked fans out image-liked for a toggle that just
// committed (the handler calls this only after dbToggleLike returned,
// honoring the hub's commit-before-publish ordering). The count is
// absolute, so a client that missed earlier toggles still converges.
// Per-site visibility is computed once from the row — the same
// safeSiteCtx(siteCanSee) folding publishImageNew uses — and rides the
// ring entry for replay filtering.
func (a *App) publishImageLiked(img *dbImage, count int) {
	if a.events == nil {
		return
	}
	a.events.publishVisible(eventImageLiked, imageLikedEvent{
		ID:    img.ID,
		Count: count,
	}, siteCanSee(safeSiteCtx(a.getConfig().SafeSite), img))
}

// Like identity cookie + rate constants. Deliberately not config:
// protocol/robustness internals (the SSE hub tuning constants set the
// precedent), not operator policy. The rate is a per-PROCESS bucket —
// enough for any human, cheap insurance against scripted like-stuffing
// from one client.
const (
	likeCookieName = "imgsite_liker"
	// likeTokenBytes is the entropy of the anonymous liker id (hex
	// encoded to likeTokenLen chars on the wire).
	likeTokenBytes = 16
	// likesPerMinute bounds toggle POSTs per process.
	likesPerMinute = 60
	// likeCookieMaxAge ≈ 10 years: the token has no server-side
	// expiry and rotating it only loses the visitor's "already liked"
	// memory.
	likeCookieMaxAge = 10 * 365 * 24 * 60 * 60
)

// validLikeToken reports whether v is a token this server could have
// minted: exactly 32 lowercase hex chars. Anything else is ignored as
// identity (the POST path mints a fresh one) — never trusted.
func validLikeToken(v string) bool {
	if len(v) != 2*likeTokenBytes {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// likeTokenFromRequest resolves the caller's liker identity: the
// cookie's value when it is a well-formed token, else a freshly minted
// one (minted=true tells the caller to Set-Cookie it on the response).
// The cookie is minted ONLY on the first like POST — page GETs never
// set cookies, so visitors who never like stay cookieless.
func likeTokenFromRequest(r *http.Request) (token string, minted bool) {
	if c, err := r.Cookie(likeCookieName); err == nil && validLikeToken(c.Value) {
		return c.Value, false
	}
	b := make([]byte, likeTokenBytes)
	if _, err := crand.Read(b); err != nil {
		// crypto/rand failing is an environment-level catastrophe;
		// refuse to fall back to a weaker source.
		panic("imgsite: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b), true
}

// likeRateLimiter is the toggle endpoint's per-IP guard: one token
// bucket per client IP, keyed exactly like the SSE hub's connection
// cap (clientIP — X-Forwarded-For's first value when the operator's
// reverse proxy supplies it, else RemoteAddr's host part). Per-IP, NOT
// process-wide: a shared bucket would let one scripted client starve
// every other visitor's toggles — a remote off-switch for the feature
// — while per-IP only bounds each client's own stuffing. The trust
// boundary is the same documented deployment assumption the SSE cap
// carries (proxies append the real client to XFF; a directly-exposed
// server lets clients forge the header and dodge the cap — a cheap
// runaway guard, not authentication).
//
// Memory is bounded opportunistically: once the bucket map passes
// likeBucketsPruneSize, IPs idle longer than likeBucketsTTL are
// dropped on the next allow() call. At gallery traffic this never
// fires; it exists so a hostile IP spray cannot grow the map forever.
// The mutex is only held for map bookkeeping (each bucket's own
// rateLimiter has its own lock), and likes are low-rate, so contention
// is nil.
type likeRateLimiter struct {
	mu        sync.Mutex
	perMinute int
	buckets   map[string]*rateLimiter
	lastSeen  map[string]time.Time
}

const (
	likeBucketsPruneSize = 1024
	likeBucketsTTL       = 10 * time.Minute
)

func newLikeRateLimiter(perMinute int) *likeRateLimiter {
	return &likeRateLimiter{
		perMinute: perMinute,
		buckets:   make(map[string]*rateLimiter),
		lastSeen:  make(map[string]time.Time),
	}
}

func (l *likeRateLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if len(l.buckets) >= likeBucketsPruneSize {
		for k, ts := range l.lastSeen {
			if now.Sub(ts) > likeBucketsTTL {
				delete(l.buckets, k)
				delete(l.lastSeen, k)
			}
		}
	}
	rl := l.buckets[ip]
	if rl == nil {
		rl = newRateLimiter(l.perMinute)
		l.buckets[ip] = rl
	}
	l.lastSeen[ip] = now
	return rl.allow()
}

// likeResponse is the JSON shape the JS enhancement consumes.
type likeResponse struct {
	Liked bool `json:"liked"`
	Count int  `json:"count"`
}

// handleLikeToggle implements POST /{id}/like — the like toggle for
// both the JS enhancement (Accept: application/json → 200 + JSON) and
// the no-JS form path (303 back to the details page, which re-renders
// the button's new state server-side). Visibility rules mirror the
// details page exactly: unknown/malformed id 404, hidden 410, and an
// id invisible on the requesting site's host 404s with no existence
// hint (same order as handleImagePage: hidden check first, so a hidden
// row discriminates 410 on both hosts).
func (a *App) handleLikeToggle(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validImageID(id) {
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
	liked, count, err := dbToggleLike(a.db, id, token)
	if err != nil {
		logger.Error("like toggle failed", "id", id, "error", err)
		http.Error(w, "storage failure", http.StatusInternalServerError)
		return
	}

	// Publish only after the toggle committed (the hub's ordering
	// rule: a subscriber acting on the event must see the new count on
	// re-query). Nil-hub-safe.
	a.publishImageLiked(img, count)

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
		if err := json.NewEncoder(w).Encode(likeResponse{Liked: liked, Count: count}); err != nil {
			logger.Error("writing like response", "id", id, "error", err)
		}
		return
	}
	http.Redirect(w, r, "/"+id, http.StatusSeeOther)
}
