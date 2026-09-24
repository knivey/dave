package main

// Milestone 5: FTS5 prompt search with strictly two-tier ranking,
// snippet highlighting, and a LIKE substring fallback.
//
// Verified FTS5 semantics (probed empirically against
// modernc.org/sqlite v1.49.1 — the driver this repo ships; see
// docs/image-site.md "Search"):
//   - The bare column filter `{original_prompt}: "a"* "b"*` binds to
//     ONLY the term immediately following it; the parenthesized form
//     `{original_prompt}: ("a"* "b"*)` filters every term inside the
//     parens. Tier 1 therefore always wraps the match expression in
//     parens — without them a row whose original matched token 1 and
//     whose enhanced matched token 2 would leak into tier 1.
//   - Consecutive separately-quoted terms are implicit AND (starred or
//     not); only a quoted string with internal spaces is an adjacency
//     phrase. Multi-token user input is therefore AND, quoted phrases
//     from the user stay phrases.
//   - bm25()/snippet() must reference the table by its UN-aliased name
//     (external-content FTS exposes only its own columns; JOIN back to
//     images on rowid).
//   - NOT is a binary operator in FTS5 query syntax, so tier-2
//     exclusion is a `rowid NOT IN (subquery)` in SQL, never a
//     prefixed NOT inside the MATCH string.
//   - An empty MATCH string is a syntax error — buildFTSQuery's
//     ok=false path must run before any SQL does.

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
)

// ---------------------------------------------------------------------------
// Query building
// ---------------------------------------------------------------------------

// maxSearchQueryBytes caps the accepted query before tokenizing so a
// hostile or pasted-megabyte ?q= cannot build a pathological MATCH
// expression. Truncation is rune-aligned to keep the echoed query
// valid UTF-8 for the templates.
const maxSearchQueryBytes = 512

// ftsOperatorChars are stripped from every bare token. Belt and braces:
// inside a quoted FTS5 string every byte except '"' is already literal,
// so quoting alone prevents injection — but stripping keeps tokens that
// were pure operator soup ("NEAR(", "^x") from becoming nonsense terms,
// and dropping the bytes means they can never re-enter an unquoted
// context no matter how the joining code evolves.
const ftsOperatorChars = `"()*+:^{}~,`

// ftsKeywords are FTS5's boolean/proximity operators. They only have
// operator meaning when bare and uppercase, which our quoting would
// neutralize anyway — but a bare AND typed by a user means "and these
// terms", which our implicit AND already provides, so the token is
// dropped instead of searched as a literal word. (Verified: passing
// raw `shrew AND cat` through makes FTS5 treat AND as an operator —
// exactly the "user input becomes query syntax" failure this file
// exists to prevent.)
var ftsKeywords = map[string]bool{"AND": true, "OR": true, "NOT": true, "NEAR": true}

func isQuerySpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	}
	return false
}

func stripFTSOperators(tok string) string {
	if !strings.ContainsAny(tok, ftsOperatorChars) {
		return tok
	}
	var b strings.Builder
	b.Grow(len(tok))
	for i := 0; i < len(tok); i++ {
		if strings.IndexByte(ftsOperatorChars, tok[i]) < 0 {
			b.WriteByte(tok[i])
		}
	}
	return b.String()
}

// buildFTSQuery converts raw user input into a sanitized FTS5 MATCH
// expression plus the plain token texts (used by the LIKE fallback).
//
// DESIGN NOTE — why every token is quoted: user input must never inject
// FTS5 query syntax. Bare AND/OR/NOT/NEAR would silently change
// semantics, and unbalanced quotes or operator characters make SQLite
// return an SQL error ("unterminated string", syntax errors near NEAR( —
// both reproduced against the driver while probing). Every bare token is
// therefore emitted as a quoted prefix term `"tok"*` (as-you-type
// matching; the table's prefix='2 3 4' index makes these fast) and every
// explicit user phrase passes through as one quoted string. Tokens
// shorter than prefixMin lose the star: 1-char prefix queries scan the
// whole index for very little recall (this is what search.prefix_min
// configures). After sanitizing, an expression with zero terms yields
// ok=false and callers run no SQL at all (an empty MATCH string is
// itself an FTS5 syntax error).
func buildFTSQuery(input string, prefixMin int) (expr string, tokens []string, ok bool) {
	if prefixMin < 1 {
		prefixMin = 1
	}
	var terms []string
	i := 0
	for i < len(input) {
		c := input[i]
		switch {
		case isQuerySpace(c):
			i++
		case c == '"':
			// Explicit phrase: pass the quoted content through verbatim
			// (a multi-token quoted string is an adjacency phrase in
			// FTS5). The scan guarantees the content itself contains no
			// quote character.
			if end := strings.IndexByte(input[i+1:], '"'); end >= 0 {
				phrase := input[i+1 : i+1+end]
				if phrase != "" {
					terms = append(terms, `"`+phrase+`"`)
					tokens = append(tokens, phrase)
				}
				i += end + 2
			} else {
				// Unbalanced quote: drop it and keep scanning; the rest
				// is treated as ordinary bare tokens.
				i++
			}
		default:
			j := i
			for j < len(input) && input[j] != '"' && !isQuerySpace(input[j]) {
				j++
			}
			tok := stripFTSOperators(input[i:j])
			if tok != "" && !ftsKeywords[tok] {
				if utf8.RuneCountInString(tok) >= prefixMin {
					terms = append(terms, `"`+tok+`"*`)
				} else {
					terms = append(terms, `"`+tok+`"`)
				}
				tokens = append(tokens, tok)
			}
			i = j
		}
	}
	if len(terms) == 0 {
		return "", nil, false
	}
	return strings.Join(terms, " "), tokens, true
}

// ftsTier1Expr wraps a sanitized match expression in the
// original-prompt column filter. The parens are load-bearing (see the
// file-header verification notes).
func ftsTier1Expr(expr string) string {
	return `{original_prompt}: (` + expr + `)`
}

// escapeLike makes token a literal LIKE pattern fragment (wildcards %
// and _ escaped) for use with ESCAPE '\'.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
// Search cursor
// ---------------------------------------------------------------------------

// searchCursor is the search pagination position: rows already consumed
// from each tier. DESIGN NOTE — offsets, not keyset: search results are
// RANKED (bm25 within tier), so a keyset cursor would have to encode
// the rank float of the last row; float text round-trips are not
// guaranteed to be exact and a half-digit of drift would duplicate or
// skip rows at page boundaries. Offsets within each tier are stable
// enough for a point-in-time query: unlike the gallery (where live
// prepends shift pages constantly and keyset is load-bearing), search
// pages only drift when a NEW upload matches the query mid-scroll, at
// worst repeating one row at a tier boundary. The two offsets are
// stitched (tier 1 drains first, tier 2 fills the remainder), so a
// single page can straddle the tier boundary without dupes or gaps.
type searchCursor struct {
	tier1 int
	tier2 int
}

// maxSearchOffset bounds the accepted offsets: a cursor can only ever
// advance by page size per request, so anything beyond this is garbage.
const maxSearchOffset = 1 << 20

func formatSearchCursor(c searchCursor) string {
	return fmt.Sprintf("1:%d|2:%d", c.tier1, c.tier2)
}

func parseSearchCursor(s string) (searchCursor, bool) {
	if s == "" {
		return searchCursor{}, true
	}
	parts := strings.Split(s, "|")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "1:") || !strings.HasPrefix(parts[1], "2:") {
		return searchCursor{}, false
	}
	n1, err := strconv.Atoi(parts[0][2:])
	if err != nil || n1 < 0 || n1 > maxSearchOffset {
		return searchCursor{}, false
	}
	n2, err := strconv.Atoi(parts[1][2:])
	if err != nil || n2 < 0 || n2 > maxSearchOffset {
		return searchCursor{}, false
	}
	return searchCursor{n1, n2}, true
}

// ---------------------------------------------------------------------------
// Snippets
// ---------------------------------------------------------------------------

// Snippet markers are the SOH/STX control bytes. Rationale: FTS5's
// snippet() interleaves arbitrary marker strings into untrusted prompt
// text, and the result must be split back into plain/highlighted
// pieces before rendering. Any printable marker ("[", "<b>", …) can
// also appear in user prompt text and would be indistinguishable from
// a real marker; control bytes are stripped as separators by the
// unicode61 tokenizer (so they never occur INSIDE a highlighted token)
// and do not survive into prompts except through deliberate binary
// injection — where the worst case is a cosmetically misplaced
// highlight, never a security issue, because every piece is
// HTML-escaped by html/template on the way out (see the cards partial:
// pieces render as {{.Text}} inside literal <mark> tags; no
// template.HTML anywhere).
const (
	snippetStartMarker = "\x01"
	snippetEndMarker   = "\x02"
	snippetEllipsis    = "…"
)

// snippetPart is one template-safe piece of a highlighted snippet.
// Text is raw (untrusted) — html/template escapes it at render time;
// Hit only decides whether the piece sits inside a literal <mark>.
type snippetPart struct {
	Text string
	Hit  bool
}

// splitSnippet splits snippet()'s marker-interleaved output into
// parts. Ambiguity note: a literal \x01/\x02 inside prompt text can
// only cause a highlight boundary to sit in the wrong place — every
// piece is still escaped before rendering, so there is no injection
// path regardless.
func splitSnippet(raw string) []snippetPart {
	if raw == "" {
		return nil
	}
	var parts []snippetPart
	var b strings.Builder
	hit := false
	flush := func() {
		if b.Len() > 0 {
			parts = append(parts, snippetPart{Text: b.String(), Hit: hit})
			b.Reset()
		}
	}
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case snippetStartMarker[0]:
			flush()
			hit = true
		case snippetEndMarker[0]:
			flush()
			hit = false
		default:
			b.WriteByte(raw[i])
		}
	}
	flush()
	return parts
}

// snippetTokenWindow converts search.snippet_chars into snippet()'s
// token-count argument. FTS5 snippets are measured in tokens, not
// characters; prompt tokens average ~6 chars including the following
// space (measured against the production prompts in
// docs/image-site.md), so chars/6 lands the rendered snippet near the
// configured clamp. Clamped to a sane band so config extremes can't
// ask for degenerate windows.
func snippetTokenWindow(snippetChars int) int {
	if snippetChars <= 0 {
		snippetChars = defaultSnippetChars
	}
	n := snippetChars / 6
	if n < 8 {
		n = 8
	}
	if n > 64 {
		n = 64
	}
	return n
}

// ---------------------------------------------------------------------------
// DB queries
// ---------------------------------------------------------------------------

// FTS column indexes inside images_fts (the virtual table's own
// columns, in CREATE order). Hardcoded into the tier SQL because
// snippet()'s column argument is positional.
const (
	ftsColOriginal = 0
	ftsColEnhanced = 1
)

// searchHit is one ranked result row.
type searchHit struct {
	img  dbImage
	tier int // 1 = matched in original_prompt, 2 = enhanced-only
	// snippet is snippet()'s raw marker-interleaved output; empty for
	// LIKE-fallback hits (they render the plain clamped prompt).
	snippet string
}

// searchRow scans a JOIN row: every images column plus the computed
// snippet. sqlx flattens the embedded dbImage's db-tagged fields.
type searchRow struct {
	dbImage
	SearchSnippet string `db:"search_snippet"`
}

// Seams for handler-level failure injection (repo convention — see
// dbGetImageByIDFn).
var (
	dbSearchFTSTierFn  = dbSearchFTSTier
	dbSearchLIKETierFn = dbSearchLIKETier
)

// bm25 weights: original prompt 8, enhanced 1. Tiering is structural,
// so these only order rows WITHIN a tier — but they still express the
// same preference (an original-column hit outranks the same hit in the
// enhanced column when both rows land in one tier via a multi-term
// query).
//
// The rowid tiebreak after bm25 makes the total order deterministic —
// required for stable OFFSET pagination.
const searchBM25Order = `ORDER BY bm25(images_fts, 8.0, 1.0), images_fts.rowid`

// dbSearchFTSTier runs one tier of the two-tier FTS search.
//
//	tier 1: rows whose ORIGINAL prompt column matched
//	tier 2: rows that matched anywhere EXCEPT tier 1 — via
//	        rowid NOT IN (tier-1 subquery), because NOT is a binary
//	        operator in FTS5 query syntax and cannot prefix a column
//	        filter inside the MATCH string
//
// Both tiers join back to images (external-content FTS exposes only
// its own columns) for the hidden=0 filter and the card columns, and
// both request a snippet from the column that defines their tier.
func dbSearchFTSTier(db *sqlx.DB, tier int, expr, tier1Expr string, offset, limit, snippetTokens int) ([]searchHit, error) {
	var rows []searchRow
	var err error
	if tier == 1 {
		err = db.Select(&rows, `
			SELECT i.*, snippet(images_fts, `+strconv.Itoa(ftsColOriginal)+`, ?, ?, ?, ?) AS search_snippet
			FROM images_fts JOIN images i ON i.rowid = images_fts.rowid
			WHERE images_fts MATCH ? AND i.hidden = 0
			`+searchBM25Order+`
			LIMIT ? OFFSET ?`,
			snippetStartMarker, snippetEndMarker, snippetEllipsis, snippetTokens,
			tier1Expr, limit, offset)
	} else {
		err = db.Select(&rows, `
			SELECT i.*, snippet(images_fts, `+strconv.Itoa(ftsColEnhanced)+`, ?, ?, ?, ?) AS search_snippet
			FROM images_fts JOIN images i ON i.rowid = images_fts.rowid
			WHERE images_fts MATCH ?
			  AND images_fts.rowid NOT IN (
			    SELECT rowid FROM images_fts WHERE images_fts MATCH ?
			  )
			  AND i.hidden = 0
			`+searchBM25Order+`
			LIMIT ? OFFSET ?`,
			snippetStartMarker, snippetEndMarker, snippetEllipsis, snippetTokens,
			expr, tier1Expr, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	hits := make([]searchHit, 0, len(rows))
	for i := range rows {
		hits = append(hits, searchHit{img: rows[i].dbImage, tier: tier, snippet: rows[i].SearchSnippet})
	}
	return hits, nil
}

// dbSearchLIKETier is the two-tier substring fallback for queries FTS
// cannot see (mid-word fragments like "omcomi" inside "randomcomimg").
// Original-column matches are tier 1; enhanced-only matches tier 2,
// excluding tier-1 rows by id. Ordered by recency, not relevance —
// there is no rank to sort by when the match isn't tokenized.
func dbSearchLIKETier(db *sqlx.DB, tokens []string, tier, offset, limit int) ([]searchHit, error) {
	origPatterns := make([]string, 0, len(tokens))
	enhPatterns := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		origPatterns = append(origPatterns, "%"+escapeLike(tok)+"%")
		enhPatterns = append(enhPatterns, "%"+escapeLike(tok)+"%")
	}
	origAnd := strings.Join(repeatPredicate("original_prompt LIKE ? ESCAPE '\\'", len(tokens)), " AND ")
	enhAnd := strings.Join(repeatPredicate("enhanced_prompt LIKE ? ESCAPE '\\'", len(tokens)), " AND ")

	var query string
	var args []interface{}
	if tier == 1 {
		query = `SELECT * FROM images
			WHERE hidden = 0 AND (` + origAnd + `)
			ORDER BY created_at DESC, id DESC
			LIMIT ? OFFSET ?`
		args = append(toAnySlice(origPatterns), limit, offset)
	} else {
		query = `SELECT * FROM images
			WHERE hidden = 0 AND (` + enhAnd + `)
			  AND id NOT IN (
			    SELECT id FROM images WHERE hidden = 0 AND (` + origAnd + `)
			  )
			ORDER BY created_at DESC, id DESC
			LIMIT ? OFFSET ?`
		args = append(toAnySlice(enhPatterns), toAnySlice(origPatterns)...)
		args = append(args, limit, offset)
	}

	var imgs []dbImage
	if err := db.Select(&imgs, query, args...); err != nil {
		return nil, err
	}
	hits := make([]searchHit, 0, len(imgs))
	for i := range imgs {
		hits = append(hits, searchHit{img: imgs[i], tier: tier})
	}
	return hits, nil
}

func repeatPredicate(pred string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = pred
	}
	return out
}

func toAnySlice(ss []string) []interface{} {
	out := make([]interface{}, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// ---------------------------------------------------------------------------
// Orchestration
// ---------------------------------------------------------------------------

// searchResult is one stitched results page.
type searchResult struct {
	Hits     []searchHit
	HasMore  bool
	Next     searchCursor
	UsedLIKE bool
}

// runSearch executes the two-tier search for one page.
//
// Page assembly (offset-stitch): each tier is fetched with limit+1 rows
// starting at its cursor offset; tier 1 fills the page first and tier 2
// fills only the remainder (never consuming tier 2 while tier 1 could
// still fill the page), so the two offsets advance independently and a
// page can straddle the tier boundary with no duplicates or gaps.
//
// The LIKE fallback runs whenever the FTS query produces zero rows —
// including mid-pagination, because a query with zero FTS matches
// stays that way only while nothing new matches: a mid-scroll upload
// whose terms DO tokenize flips the next page from LIKE back to FTS
// mode, where the carried tier offsets index a different (ranked, not
// recency-ordered) result set — a possible duplicate or skip at that
// boundary. Same offset-drift class as documented on searchCursor.
func runSearch(db *sqlx.DB, q string, cur searchCursor, limit, prefixMin, snippetTokens int) (searchResult, error) {
	res := searchResult{Next: cur}

	q = truncateSearchQuery(q)
	expr, tokens, ok := buildFTSQuery(q, prefixMin)
	if !ok {
		// Nothing searchable after sanitizing (empty or operator-only
		// input). No SQL: an empty MATCH string is an FTS5 syntax
		// error, and there is no token to LIKE either.
		return res, nil
	}
	tier1Expr := ftsTier1Expr(expr)

	t1, err := dbSearchFTSTierFn(db, 1, expr, tier1Expr, cur.tier1, limit+1, snippetTokens)
	if err != nil {
		return res, err
	}
	t2, err := dbSearchFTSTierFn(db, 2, expr, tier1Expr, cur.tier2, limit+1, snippetTokens)
	if err != nil {
		return res, err
	}
	if len(t1) == 0 && len(t2) == 0 {
		l1, err := dbSearchLIKETierFn(db, tokens, 1, cur.tier1, limit+1)
		if err != nil {
			return res, err
		}
		l2, err := dbSearchLIKETierFn(db, tokens, 2, cur.tier2, limit+1)
		if err != nil {
			return res, err
		}
		return stitchSearchPage(l1, l2, cur, limit, true), nil
	}
	return stitchSearchPage(t1, t2, cur, limit, false), nil
}

func stitchSearchPage(t1, t2 []searchHit, cur searchCursor, limit int, usedLIKE bool) searchResult {
	take1 := min(len(t1), limit)
	remain := limit - take1
	take2 := min(len(t2), remain)

	hits := make([]searchHit, 0, take1+take2)
	hits = append(hits, t1[:take1]...)
	hits = append(hits, t2[:take2]...)

	return searchResult{
		Hits:     hits,
		HasMore:  len(t1) > take1 || len(t2) > take2,
		Next:     searchCursor{tier1: cur.tier1 + take1, tier2: cur.tier2 + take2},
		UsedLIKE: usedLIKE,
	}
}

// truncateSearchQuery caps the query at maxSearchQueryBytes of UTF-8,
// cut on a rune boundary so the echoed query stays valid text for the
// templates.
func truncateSearchQuery(q string) string {
	if len(q) <= maxSearchQueryBytes {
		return q
	}
	var b strings.Builder
	for _, r := range q {
		if b.Len()+utf8.RuneLen(r) > maxSearchQueryBytes {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// HTTP handlers
// ---------------------------------------------------------------------------

// searchQueryFromRequest pulls and truncates ?q=.
func searchQueryFromRequest(r *http.Request) string {
	return truncateSearchQuery(r.URL.Query().Get("q"))
}

// searchCursorFromRequest parses ?after= into a search cursor.
func searchCursorFromRequest(r *http.Request) (searchCursor, bool) {
	return parseSearchCursor(r.URL.Query().Get("after"))
}

// handleSearchPage renders GET /search?q=… — the shareable,
// no-JS-complete full results page (same card grid as the gallery,
// with highlighted snippets under matched prompts).
//
// Registered as an exact literal on the mux, so it always wins over
// the "GET /" catch-all whose id dispatcher would 404 "search" (6
// chars, not a valid image id) — same precedence reasoning as /gallery.
func (a *App) handleSearchPage(w http.ResponseWriter, r *http.Request) {
	// SSE cursor capture — BEFORE runSearch's DB reads, same
	// overlap-safe/gap-unsafe ordering as handleGalleryPage. On this
	// page the replayed arrivals buffer behind the "+N new" pill (the
	// filter is active from boot), which is exactly how a live arrival
	// during a search already presents.
	lastEvent, hasHub := a.renderEventCursor()
	cur, ok := searchCursorFromRequest(r)
	if !ok {
		http.Error(w, "malformed cursor", http.StatusBadRequest)
		return
	}
	cfg := a.getConfig()
	q := searchQueryFromRequest(r)
	res, err := runSearch(a.db, q, cur, galleryPageSize, cfg.Search.PrefixMin, snippetTokenWindow(cfg.Search.SnippetChars))
	if err != nil {
		logger.Error("search query failed", "q", q, "error", err)
		http.Error(w, "lookup failure", http.StatusInternalServerError)
		return
	}

	view := buildSearchView(cfg, q, res)
	if hasHub {
		view.LastEvent = &lastEvent
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := searchTemplates.Execute(w, view); err != nil {
		logger.Error("rendering search page", "q", q, "error", err)
	}
}

// handleSearchFragment renders GET /search-fragment?q=…&after=… — the
// next results page as the same "cards" partial the gallery fragment
// uses, consumed by search.js's first swap and gallery.js's infinite
// scroll (which fetch-URLs through the provider search.js installs).
func (a *App) handleSearchFragment(w http.ResponseWriter, r *http.Request) {
	cur, ok := searchCursorFromRequest(r)
	if !ok {
		http.Error(w, "malformed cursor", http.StatusBadRequest)
		return
	}
	cfg := a.getConfig()
	q := searchQueryFromRequest(r)
	res, err := runSearch(a.db, q, cur, galleryPageSize, cfg.Search.PrefixMin, snippetTokenWindow(cfg.Search.SnippetChars))
	if err != nil {
		logger.Error("search fragment query failed", "q", q, "error", err)
		http.Error(w, "lookup failure", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := searchTemplates.ExecuteTemplate(w, "cards", buildSearchView(cfg, q, res)); err != nil {
		logger.Error("rendering search fragment", "q", q, "error", err)
	}
}

// buildSearchView assembles the galleryView for a results page.
// Snippet-bearing hits render the highlighted snippet instead of the
// plain clamped prompt; LIKE hits (no snippet) fall back to the plain
// clamp of the column their tier matched.
func buildSearchView(cfg Config, q string, res searchResult) galleryView {
	v := galleryView{
		Title:         searchPageTitle(cfg, q),
		SiteTitle:     cfg.Site.Title,
		Description:   cfg.Site.Description,
		Query:         q,
		HasMore:       res.HasMore,
		OGTitle:       searchPageTitle(cfg, q),
		OGDescription: cfg.Site.Description,
	}
	if res.HasMore {
		v.NextCursor = formatSearchCursor(res.Next)
	}
	for i := range res.Hits {
		hit := &res.Hits[i]
		card := galleryCardFromImage(cfg, &hit.img)
		if hit.tier == 2 {
			// Tier-2 rows matched via the enhanced prompt; the plain
			// preview fallback (used only when there is no snippet,
			// i.e. LIKE-fallback hits) should preview that column.
			card.PromptSnippet = clampSnippet(hit.img.EnhancedPrompt, cfg.Search.SnippetChars)
		}
		if parts := splitSnippet(hit.snippet); parts != nil {
			card.SnippetParts = parts
		}
		v.Cards = append(v.Cards, card)
	}
	v.NoResults = strings.TrimSpace(q) != "" && len(v.Cards) == 0
	// Empty query: runSearch returns zero hits without touching SQL, so
	// a bare /search would render an empty grid — say nothing was
	// searched instead (mirrors NoResults' shape one branch over).
	v.NothingSearched = strings.TrimSpace(q) == "" && len(v.Cards) == 0
	return v
}

func searchPageTitle(cfg Config, q string) string {
	if strings.TrimSpace(q) == "" {
		return cfg.Site.Title
	}
	return "search: " + q + " — " + cfg.Site.Title
}
