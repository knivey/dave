package main

// likes.go — anonymous image likes AND dislikes: the DB layer (this
// file) and the cookie identity + POST toggle handlers. Design
// summary (full version in docs/image-site.md "Likes"):
//
//   - Identity is a cookie token minted on the visitor's FIRST vote
//     POST and never before — browsers that never vote carry no
//     cookie.
//   - One row in likes = one vote; the (image_id, token) PK is the
//     entire dedupe mechanism. The vote column says WHICH stance the
//     token holds (voteLike / voteDislike), so like and dislike are
//     mutually exclusive for free: a token cannot hold two rows.
//   - Toggling is one guarded upsert: INSERT ... ON CONFLICT DO
//     UPDATE SET vote = excluded.vote WHERE likes.vote <> excluded.vote.
//     One row affected means the token now holds this stance (fresh
//     insert, or a SWITCH from the opposite one); zero rows means it
//     already held it, so the toggle retracts to neutral via DELETE.
//   - Votes are global per image (not per logical site): the counts
//     are properties of the row, and both hosts render the same
//     numbers. Host-scoped cookies mean a visitor using BOTH the
//     default and the safe host holds two tokens and could like twice
//     — accepted, this is a casual anonymous feature, not a boundary.

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

// Vote values: the stance a token's single row expresses. Zero is
// never stored — it is the "no row" state dbGetVoteState reports for
// a token that has not voted (COALESCE in SQL, myVote in Go).
const (
	voteLike    = 1
	voteDislike = -1
)

// dbToggleVote flips token's stance on id toward vote and returns the
// post-toggle state: whether the caller now holds that stance (false
// = retracted to neutral) plus the image's fresh like and dislike
// counts.
//
// The guarded-upsert form keeps the whole decision inside one
// statement, so the semantics survive racing toggles from the same
// token under SQLite's single-writer serialization: a fresh vote
// inserts (1 row), a switch from the opposite stance updates (1 row),
// and a repeat of the held stance affects 0 rows and falls through to
// the DELETE — the reported state (retracted, counts as-of-then) is
// coherent either way. created_at rides the DO UPDATE so the stamp
// always says when the CURRENT stance landed, not when the token
// first voted at all.
func dbToggleVote(db *sqlx.DB, id, token string, vote int) (held bool, likeCount, dislikeCount int, err error) {
	now := formatDBTimeNow()
	res, err := db.Exec(
		`INSERT INTO likes (image_id, token, created_at, vote) VALUES (?, ?, ?, ?)
		 ON CONFLICT (image_id, token) DO UPDATE SET vote = excluded.vote, created_at = excluded.created_at
		 WHERE likes.vote <> excluded.vote`,
		id, token, now, vote)
	if err != nil {
		return false, 0, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, 0, 0, err
	}
	held = n > 0
	if !held {
		if _, err := db.Exec(`DELETE FROM likes WHERE image_id = ? AND token = ?`, id, token); err != nil {
			return false, 0, 0, err
		}
	}
	if likeCount, dislikeCount, err = dbVoteCounts(db, id); err != nil {
		return false, 0, 0, err
	}
	return held, likeCount, dislikeCount, nil
}

// dbVoteCounts returns an image's like and dislike counts in one
// round trip. Like dbGetVoteState, the FROM images anchor refuses to
// fabricate counts for a row that does not exist.
func dbVoteCounts(db *sqlx.DB, id string) (likeCount, dislikeCount int, err error) {
	var c struct {
		Likes    int `db:"likes"`
		Dislikes int `db:"dislikes"`
	}
	err = db.Get(&c, `
		SELECT (SELECT COUNT(*) FROM likes l WHERE l.image_id = i.id AND l.vote = 1) AS likes,
		       (SELECT COUNT(*) FROM likes l WHERE l.image_id = i.id AND l.vote = -1) AS dislikes
		FROM images i WHERE i.id = ?`, id)
	if err != nil {
		return 0, 0, err
	}
	return c.Likes, c.Dislikes, nil
}

// dbGetVoteState returns an image's like count, dislike count, and
// the token's own stance (voteLike / voteDislike / 0 = not voted), in
// one round trip. Unknown image ids surface as sql.ErrNoRows (the
// FROM images anchor refuses to fabricate a state for a row that does
// not exist); production callers always look the row up first, so
// this is a defensive tripwire, not a code path.
func dbGetVoteState(db *sqlx.DB, id, token string) (likeCount, dislikeCount, myVote int, err error) {
	var state struct {
		Likes    int `db:"likes"`
		Dislikes int `db:"dislikes"`
		Mine     int `db:"mine"`
	}
	err = db.Get(&state, `
		SELECT (SELECT COUNT(*) FROM likes l WHERE l.image_id = i.id AND l.vote = 1) AS likes,
		       (SELECT COUNT(*) FROM likes l WHERE l.image_id = i.id AND l.vote = -1) AS dislikes,
		       COALESCE((SELECT l.vote FROM likes l WHERE l.image_id = i.id AND l.token = ?), 0) AS mine
		FROM images i WHERE i.id = ?`, token, id)
	if err != nil {
		return 0, 0, 0, err
	}
	return state.Likes, state.Dislikes, state.Mine, nil
}

// hydrateVoteCounts batch-fills LikeCount and DislikeCount on the
// given rows with one grouped query (keyed by image AND vote, so both
// tallies come back from a single pass). Rows whose ids have no votes
// keep the zero values — absence in the GROUP BY result means zero,
// not "unknown". Callers pass page-sized slices (tens of rows), far
// under SQLite's parameter limit, so no chunking exists.
func hydrateVoteCounts(db *sqlx.DB, imgs []*dbImage) error {
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
		`SELECT image_id, vote, COUNT(*) FROM likes WHERE image_id IN (`+strings.Join(placeholders, ",")+`)
		 GROUP BY image_id, vote`,
		args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	type voteTally struct{ likes, dislikes int }
	counts := make(map[string]voteTally, len(imgs))
	for rows.Next() {
		var id string
		var vote, n int
		if err := rows.Scan(&id, &vote, &n); err != nil {
			return err
		}
		t := counts[id]
		// The column CHECK pins the domain to ±1; the explicit
		// voteDislike compare (over a blind else) keeps an
		// out-of-domain value from silently inflating the dislike
		// tally if that CHECK is ever widened.
		if vote == voteLike {
			t.likes = n
		} else if vote == voteDislike {
			t.dislikes = n
		}
		counts[id] = t
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, img := range imgs {
		img.LikeCount = counts[img.ID].likes
		img.DislikeCount = counts[img.ID].dislikes
	}
	return nil
}

// formatDBTimeNow is the shared "now" stamp for like rows (kept as a
// helper so future writers cannot drift from dbTimeFormat).
func formatDBTimeNow() string {
	return time.Now().UTC().Format(dbTimeFormat)
}

// publishImageLiked fans out image-liked for a toggle that just
// committed (the handler calls this only after dbToggleVote returned,
// honoring the hub's commit-before-publish ordering). Both counts are
// absolute, so a client that missed earlier toggles still converges.
// Per-site visibility is computed once from the row — the same
// safeSiteCtx(siteCanSee) folding publishImageNew uses — and rides the
// ring entry for replay filtering.
func (a *App) publishImageLiked(img *dbImage, likes, dislikes int) {
	if a.events == nil {
		return
	}
	a.events.publishVisible(eventImageLiked, imageLikedEvent{
		ID:       img.ID,
		Likes:    likes,
		Dislikes: dislikes,
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

// likeResponse is the JSON shape the JS enhancement consumes. Both
// stance booleans describe the CALLER's post-toggle state (exactly one
// can be true — like and dislike are mutually exclusive per token;
// both false = retracted to neutral), and both counts are the image's
// absolute post-toggle tallies.
type likeResponse struct {
	Liked    bool `json:"liked"`
	Disliked bool `json:"disliked"`
	Likes    int  `json:"likes"`
	Dislikes int  `json:"dislikes"`
}

// handleLikeToggle implements POST /{id}/like; handleDislikeToggle is
// the same handler bound to POST /{id}/dislike (voteHandler(-1)). The
// vote toggle serves both the JS enhancement (Accept:
// application/json → 200 + JSON) and the no-JS form path (303 back to
// the details page, which re-renders the buttons' new state
// server-side). Visibility rules mirror the details page exactly:
// unknown/malformed id 404, hidden 410, and an id invisible on the
// requesting site's host 404s with no existence hint (same order as
// handleImagePage: hidden check first, so a hidden row discriminates
// 410 on both hosts).
func (a *App) handleLikeToggle(w http.ResponseWriter, r *http.Request) {
	a.handleVoteToggle(voteLike)(w, r)
}

// handleDislikeToggle implements POST /{id}/dislike — see
// handleLikeToggle for the shared contract.
func (a *App) handleDislikeToggle(w http.ResponseWriter, r *http.Request) {
	a.handleVoteToggle(voteDislike)(w, r)
}

// handleVoteToggle is the parameterized vote toggle both route
// handlers delegate to.
func (a *App) handleVoteToggle(vote int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		held, likes, dislikes, err := dbToggleVote(a.db, id, token, vote)
		if err != nil {
			logger.Error("vote toggle failed", "id", id, "vote", vote, "error", err)
			http.Error(w, "storage failure", http.StatusInternalServerError)
			return
		}

		// Publish only after the toggle committed (the hub's ordering
		// rule: a subscriber acting on the event must see the new
		// counts on re-query). Nil-hub-safe.
		a.publishImageLiked(img, likes, dislikes)

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
			resp := likeResponse{Liked: vote == voteLike && held, Disliked: vote == voteDislike && held,
				Likes: likes, Dislikes: dislikes}
			if err := json.NewEncoder(w).Encode(resp); err != nil {
				logger.Error("writing vote response", "id", id, "error", err)
			}
			return
		}
		http.Redirect(w, r, "/"+id, http.StatusSeeOther)
	}
}
