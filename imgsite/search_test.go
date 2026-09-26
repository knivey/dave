package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertSearchRow inserts a row with explicit prompt columns for search
// fixtures (insertImage's default prompt is "prompt for <id>").
func insertSearchRow(t *testing.T, app *App, id, createdAt, orig, enh string, mutators ...func(*dbImage)) {
	t.Helper()
	muts := append([]func(*dbImage){func(img *dbImage) {
		img.OriginalPrompt = orig
		img.EnhancedPrompt = enh
	}}, mutators...)
	insertImage(t, app, id, createdAt, muts...)
}

// runSearchFor runs a search with the test-default search settings.
func runSearchFor(t *testing.T, app *App, q string, cur searchCursor, limit int) searchResult {
	t.Helper()
	res, err := runSearch(app.db, q, cur, limit, 2, snippetTokenWindow(160), siteCtx{})
	require.NoError(t, err, "query %q", q)
	return res
}

// runSearchForSite is runSearchFor with an explicit site context.
func runSearchForSite(t *testing.T, app *App, q string, cur searchCursor, limit int, sc siteCtx) searchResult {
	t.Helper()
	res, err := runSearch(app.db, q, cur, limit, 2, snippetTokenWindow(160), sc)
	require.NoError(t, err, "query %q", q)
	return res
}

func hitIDs(hits []searchHit) []string {
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.img.ID
	}
	return ids
}

// ---------------------------------------------------------------------------
// Query building
// ---------------------------------------------------------------------------

func TestBuildFTSQuery(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		prefixMin int
		wantExpr  string
		wantToks  []string
		wantOK    bool
	}{
		{"SimpleToken", "shrew", 2, `"shrew"*`, []string{"shrew"}, true},
		{"PrefixFragment", "shre", 2, `"shre"*`, []string{"shre"}, true},
		{"ExplicitPhrase", `"walkin down"`, 2, `"walkin down"`, []string{"walkin down"}, true},
		{"MultiTokenImplicitAND", "shrew cat", 2, `"shrew"* "cat"*`, []string{"shrew", "cat"}, true},
		{"OperatorKeywordDropped", "shrew AND cat", 2, `"shrew"* "cat"*`, []string{"shrew", "cat"}, true},
		{"AllOperatorKeywordsDropped", "NOT OR AND", 2, "", nil, false},
		{"NEARParenStripped", "NEAR(", 2, "", nil, false},
		{"StarStripped", "shrew*", 2, `"shrew"*`, []string{"shrew"}, true},
		{"CaretStripped", "^shrew", 2, `"shrew"*`, []string{"shrew"}, true},
		{"ColumnFilterTypedByUser", "{original_prompt}: shrew", 2, `"original_prompt"* "shrew"*`, []string{"original_prompt", "shrew"}, true},
		{"UnbalancedQuoteDropped", `shrew "unclosed`, 2, `"shrew"* "unclosed"*`, []string{"shrew", "unclosed"}, true},
		{"EmptyPhraseDropped", `"" shrew`, 2, `"shrew"*`, []string{"shrew"}, true},
		{"ParensStripped", "(shrew)", 2, `"shrew"*`, []string{"shrew"}, true},
		{"Empty", "", 2, "", nil, false},
		{"WhitespaceOnly", "   ", 2, "", nil, false},
		{"OperatorSoupOnly", "()*^{}:", 2, "", nil, false},
		{"PrefixMinDisablesStar", "ab cat", 3, `"ab" "cat"*`, []string{"ab", "cat"}, true},
		{"PrefixMinOneCharsStarred", "ab cat", 2, `"ab"* "cat"*`, []string{"ab", "cat"}, true},
		{"SingleCharExactAtDefault", "a", 2, `"a"`, []string{"a"}, true},
		{"UTF8Tokens", "héllo wörld", 2, `"héllo"* "wörld"*`, []string{"héllo", "wörld"}, true},
		{"HyphenKept", "shrew-cat", 2, `"shrew-cat"*`, []string{"shrew-cat"}, true},
		{"MixedTokensAndPhrase", `shrew "comin in hot" cat`, 2, `"shrew"* "comin in hot" "cat"*`, []string{"shrew", "comin in hot", "cat"}, true},
		{"LowercaseAndKept", "this and that", 2, `"this"* "and"* "that"*`, []string{"this", "and", "that"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, tokens, ok := buildFTSQuery(tt.input, tt.prefixMin)
			if !tt.wantOK {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.Equal(t, tt.wantExpr, expr)
			assert.Equal(t, tt.wantToks, tokens)
		})
	}
}

func TestFTSTier1ExprWrapsParens(t *testing.T) {
	assert.Equal(t, `{original_prompt}: ("shrew"* "cat"*)`, ftsTier1Expr(`"shrew"* "cat"*`))
}

func TestEscapeLike(t *testing.T) {
	tests := map[string]string{
		"plain":      "plain",
		"100%":       `100\%`,
		"under_go":   `under\_go`,
		`back\slash`: `back\\slash`,
		"mixed_%_":   `mixed\_\%\_`,
	}
	for in, want := range tests {
		assert.Equal(t, want, escapeLike(in), "input %q", in)
	}
}

func TestTruncateSearchQuery(t *testing.T) {
	assert.Equal(t, "short", truncateSearchQuery("short"))
	long := strings.Repeat("ü", 600) // 2 bytes per rune, 1200 bytes
	got := truncateSearchQuery(long)
	assert.LessOrEqual(t, len(got), maxSearchQueryBytes, "byte-bounded")
	assert.True(t, strings.HasSuffix(got, "ü"), "no partial rune")
	assert.Equal(t, strings.Repeat("ü", maxSearchQueryBytes/2), got)
}

// ---------------------------------------------------------------------------
// Cursor + snippet helpers
// ---------------------------------------------------------------------------

func TestSearchCursorRoundTrip(t *testing.T) {
	c := searchCursor{tier1: 48, tier2: 0, tier3: 12}
	s := formatSearchCursor(c)
	assert.Equal(t, "1:48|2:0|3:12", s)
	got, ok := parseSearchCursor(s)
	require.True(t, ok)
	assert.Equal(t, c, got)

	got, ok = parseSearchCursor("")
	require.True(t, ok)
	assert.Equal(t, searchCursor{}, got, "empty cursor = first page")

	for _, bad := range []string{
		"1:48", "2:0|1:48", "1:-1|2:0|3:0", "1:x|2:0|3:0", "1:0|2:y|3:0", "1:0|2:0|3:z",
		"1:0|2:0", "1:0|2:0|3:0|4:0", "garbage", "1:|2:|3:", "0:1|2:3|3:0",
		"1:99999999999|2:0|3:0", "3:0|1:0|2:0",
	} {
		_, ok := parseSearchCursor(bad)
		assert.False(t, ok, "cursor %q", bad)
	}
}

func TestSplitSnippet(t *testing.T) {
	t.Run("Plain", func(t *testing.T) {
		assert.Nil(t, splitSnippet(""))
		assert.Equal(t, []snippetPart{{Text: "no hits here"}}, splitSnippet("no hits here"))
	})
	t.Run("LeadingHit", func(t *testing.T) {
		assert.Equal(t,
			[]snippetPart{{Text: "shrew", Hit: true}, {Text: " walkin down"}},
			splitSnippet("\x01shrew\x02 walkin down"))
	})
	t.Run("MidHit", func(t *testing.T) {
		assert.Equal(t,
			[]snippetPart{{Text: "a "}, {Text: "shrew", Hit: true}, {Text: " hat"}},
			splitSnippet("a \x01shrew\x02 hat"))
	})
	t.Run("AdjacentHits", func(t *testing.T) {
		assert.Equal(t,
			[]snippetPart{{Text: "shrew", Hit: true}, {Text: "shrew", Hit: true}, {Text: " rest"}},
			splitSnippet("\x01shrew\x02\x01shrew\x02 rest"))
	})
	t.Run("UnterminatedMarker", func(t *testing.T) {
		assert.Equal(t,
			[]snippetPart{{Text: "tail ", Hit: false}, {Text: "shrew", Hit: true}},
			splitSnippet("tail \x01shrew"))
	})
}

func TestSnippetTokenWindow(t *testing.T) {
	assert.Equal(t, 26, snippetTokenWindow(160), "160 chars / ~6 chars per token")
	assert.Equal(t, 8, snippetTokenWindow(10), "floor")
	assert.Equal(t, 26, snippetTokenWindow(0), "invalid config falls back to the default clamp first")
	assert.Equal(t, 26, snippetTokenWindow(-5), "negative config")
	assert.Equal(t, 64, snippetTokenWindow(10000), "ceiling")
	assert.Less(t, snippetTokenWindow(320), snippetTokenWindow(1000), "larger snippet_chars yields larger windows")
}

// ---------------------------------------------------------------------------
// Ranking: tier separation
// ---------------------------------------------------------------------------

func TestSearchTierSeparation(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "orig001", "2026-09-24 01:00:00", "shrew comin in hot", "")
	insertSearchRow(t, app, "enh0001", "2026-09-24 02:00:00", "a cat naps", "the shrew dances")
	insertSearchRow(t, app, "mis0001", "2026-09-24 03:00:00", "totally unrelated", "nothing here")

	res := runSearchFor(t, app, "shrew", searchCursor{}, 48)

	require.Len(t, res.Hits, 2)
	assert.Equal(t, "orig001", res.Hits[0].img.ID, "original-prompt match first")
	assert.Equal(t, 1, res.Hits[0].tier)
	assert.Equal(t, "enh0001", res.Hits[1].img.ID, "enhanced-only match second")
	assert.Equal(t, 2, res.Hits[1].tier)
	assert.False(t, res.HasMore)
}

// TestSearchEnhancedSpamStaysTier1 is the exact case that broke blended
// bm25(8,1) during the plan's verification: a row whose original
// prompt says the term once but whose enhanced prompt repeats it ten
// times outranked the genuine original hit. With structural tiering the
// spam row simply IS a tier-1 row (its original matched) and can never
// be outranked by any tier-2 row.
func TestSearchEnhancedSpamStaysTier1(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "spam0001", "2026-09-24 01:00:00", "shrew once",
		strings.Repeat("shrew ", 10))
	insertSearchRow(t, app, "real0001", "2026-09-24 02:00:00", "shrew comin in hot", "")
	insertSearchRow(t, app, "only0001", "2026-09-24 03:00:00", "a cat",
		strings.Repeat("shrew ", 10))

	res := runSearchFor(t, app, "shrew", searchCursor{}, 48)

	require.Len(t, res.Hits, 3)
	byID := map[string]int{}
	for i, h := range res.Hits {
		byID[h.img.ID] = i
	}
	spam, real, only := byID["spam0001"], byID["real0001"], byID["only0001"]
	assert.Equal(t, 1, res.Hits[spam].tier, "one original mention = tier 1 despite enhanced spam")
	assert.Equal(t, 1, res.Hits[real].tier)
	assert.Equal(t, 2, res.Hits[only].tier, "enhanced-only spam = tier 2")
	assert.Less(t, spam, only, "tier-1 spam row ranks above every tier-2 row")
	assert.Less(t, real, only)
}

// TestSearchMultiTermNoTier1Leak is the behavioral pin for the paren
// wrapping in ftsTier1Expr (TestFTSTier1ExprWrapsParens pins only the
// string). FTS5's implicit AND spans columns, so a row whose ORIGINAL
// prompt matches term 1 and whose ENHANCED prompt matches term 2 DOES
// match the overall query — the leak scenario is that it must not land
// in TIER 1: tier 1 means every term matched in the original column.
// The two-tier contract places it in tier 2 (overall matches minus
// tier 1), below every tier-1 row, and a row matching neither term
// must be absent entirely.
func TestSearchMultiTermNoTier1Leak(t *testing.T) {
	app := newTestApp(t, testConfig())
	// Original matches "shrew" only; enhanced matches "cat" only.
	insertSearchRow(t, app, "leak0001", "2026-09-24 01:00:00", "shrew once", "a cat naps")
	// Original matches BOTH terms — the genuine tier-1 row.
	insertSearchRow(t, app, "both0001", "2026-09-24 02:00:00", "shrew and cat together", "")
	// Matches neither term in either column.
	insertSearchRow(t, app, "mis0001", "2026-09-24 03:00:00", "totally unrelated", "nothing here")

	res := runSearchFor(t, app, "shrew cat", searchCursor{}, 48)

	pos := map[string]int{}
	byID := map[string]searchHit{}
	for i, h := range res.Hits {
		pos[h.img.ID] = i
		byID[h.img.ID] = h
	}

	both, ok := byID["both0001"]
	require.True(t, ok, "original matching both terms is a tier-1 hit")
	assert.Equal(t, 1, both.tier)

	leak, ok := byID["leak0001"]
	require.True(t, ok, "split original/enhanced match still matches the overall query")
	assert.Equal(t, 2, leak.tier, "must not leak into tier 1: 'cat' is not in the original column")
	assert.Less(t, pos["both0001"], pos["leak0001"], "tier-1 row ranks above every tier-2 row")

	_, ok = byID["mis0001"]
	assert.False(t, ok, "row matching neither term must be absent entirely")
}

// ---------------------------------------------------------------------------
// Prefix, porter, phrase, multi-token
// ---------------------------------------------------------------------------

func TestSearchPrefixPorterPhraseMulti(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "ppp0001", "2026-09-24 01:00:00", "shrew comin in hot", "")
	insertSearchRow(t, app, "ppp0002", "2026-09-24 02:00:00", "comin out hot", "")
	insertSearchRow(t, app, "ppp0003", "2026-09-24 03:00:00", "hot shrew on a bench", "")

	t.Run("Prefix", func(t *testing.T) {
		res := runSearchFor(t, app, "shre", searchCursor{}, 48)
		assert.ElementsMatch(t, []string{"ppp0001", "ppp0003"}, hitIDs(res.Hits))
	})
	t.Run("PorterStemming", func(t *testing.T) {
		res := runSearchFor(t, app, "shrews", searchCursor{}, 48)
		assert.ElementsMatch(t, []string{"ppp0001", "ppp0003"}, hitIDs(res.Hits), `"shrews" stems to "shrew"`)
	})
	t.Run("Phrase", func(t *testing.T) {
		res := runSearchFor(t, app, `"comin in hot"`, searchCursor{}, 48)
		assert.Equal(t, []string{"ppp0001"}, hitIDs(res.Hits), "adjacency required; 'comin out hot' excluded")
	})
	t.Run("MultiTokenAND", func(t *testing.T) {
		res := runSearchFor(t, app, "shrew hot", searchCursor{}, 48)
		assert.ElementsMatch(t, []string{"ppp0001", "ppp0003"}, hitIDs(res.Hits), "both tokens required, any order")
	})
	t.Run("MultiTokenMissOne", func(t *testing.T) {
		res := runSearchFor(t, app, "shrew zebra", searchCursor{}, 48)
		assert.Empty(t, res.Hits, "no row has both terms in one prompt column — no FTS match, no substring match")
	})
	t.Run("PhrasePlusToken", func(t *testing.T) {
		res := runSearchFor(t, app, `shrew "comin in"`, searchCursor{}, 48)
		assert.Equal(t, []string{"ppp0001"}, hitIDs(res.Hits))
	})
}

// TestSearchOperatorInputsSafe pins that operator-shaped user input
// never produces an SQL error and still gives sensible results — the
// sanitizing tokenizer quotes everything (see buildFTSQuery's DESIGN
// NOTE; an unbalanced quote passed through raw is a real "unterminated
// string" SQL error against this driver).
func TestSearchOperatorInputsSafe(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertSearchRow(t, app, "opq0001", "2026-09-24 01:00:00", "shrew and cat together", "")

	for _, q := range []string{
		"shrew AND cat",
		`shrew "unclosed`,
		"NEAR(",
		"(",
		"^shrew",
		"{original_prompt}: shrew",
		"shrew*",
	} {
		t.Run(q, func(t *testing.T) {
			runSearchFor(t, app, q, searchCursor{}, 48) // asserts NoError
			status, body := getPage(t, ts.URL, "/search?q="+url.QueryEscape(q))
			require.Equal(t, http.StatusOK, status)
			assert.Contains(t, body, `data-page="gallery"`)
		})
	}

	// Sensible results: the AND query finds the row containing both words.
	res := runSearchFor(t, app, "shrew AND cat", searchCursor{}, 48)
	assert.Equal(t, []string{"opq0001"}, hitIDs(res.Hits))
	// Operator soup finds nothing but stays error-free.
	res = runSearchFor(t, app, "NEAR(", searchCursor{}, 48)
	assert.Empty(t, res.Hits)
}

// ---------------------------------------------------------------------------
// Tier 3: always-on LIKE substring scan
// ---------------------------------------------------------------------------

// TestSearchSuffixMatchRanksTier3 is the owner's motivating case:
// searching "shrew" must find "cowshrew" — a token-final fragment FTS5
// prefix terms can never match (no suffix operator exists). It also
// pins strict tier ordering and the id-level dedupe: every row here is
// LIKE-visible, but the two that tokenize keep their FTS ranks and the
// suffix-only row appends after both as tier 3.
func TestSearchSuffixMatchRanksTier3(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "sfx0001", "2026-09-24 01:00:00", "shrew parade", "")          // tier 1 (also LIKE-visible)
	insertSearchRow(t, app, "sfx0002", "2026-09-24 02:00:00", "plain", "the shrew dances") // tier 2 (also LIKE-visible)
	insertSearchRow(t, app, "sfx0003", "2026-09-24 03:00:00", "a cowshrew grazes", "")     // tier 3 only (fused token)
	insertSearchRow(t, app, "sfx0004", "2026-09-24 04:00:00", "unrelated", "")             // no match in any tier

	res := runSearchFor(t, app, "shrew", searchCursor{}, 48)

	assert.Equal(t, []string{"sfx0001", "sfx0002", "sfx0003"}, hitIDs(res.Hits),
		"tier 1 > tier 2 > tier 3 strictly; FTS-and-LIKE rows appear once at their FTS rank")
	assert.Equal(t, []int{1, 2, 3}, []int{res.Hits[0].tier, res.Hits[1].tier, res.Hits[2].tier})
	assert.False(t, res.HasMore)
}

// TestSearchLIKETier3 pins the substring tier's own behavior: mid-word
// fragments FTS cannot tokenize, the original-before-enhanced internal
// ordering, the per-column multi-token AND, wildcard escaping, and
// pagination through an all-tier-3 result set.
func TestSearchLIKETier3(t *testing.T) {
	app := newTestApp(t, testConfig())
	// Mid-word substring: no token STARTS with "omcomi", so FTS
	// (prefix + full tokens) cannot see it; LIKE %omcomi% can.
	insertSearchRow(t, app, "lik0001", "2026-09-24 01:00:00", "wander randomcomimg alone", "")
	insertSearchRow(t, app, "lik0002", "2026-09-24 02:00:00", "unrelated stuff", "something incomcomimg here")
	insertSearchRow(t, app, "lik0003", "2026-09-24 03:00:00", "shrew visible", "")

	t.Run("MidWordSubstring", func(t *testing.T) {
		res := runSearchFor(t, app, "omcomi", searchCursor{}, 48)
		require.Len(t, res.Hits, 2)
		assert.Equal(t, "lik0001", res.Hits[0].img.ID)
		assert.Equal(t, 3, res.Hits[0].tier, "original substring match = tier 3")
		assert.False(t, res.Hits[0].previewEnhanced, "original substring previews the original column")
		assert.Equal(t, "lik0002", res.Hits[1].img.ID)
		assert.Equal(t, 3, res.Hits[1].tier, "enhanced substring match = tier 3 too, after original matches")
		assert.True(t, res.Hits[1].previewEnhanced, "enhanced-only substring previews the enhanced column")
		assert.Empty(t, res.Hits[0].snippet, "tier-3 hits carry no snippet; plain preview is used")
	})
	t.Run("RanksBelowFTSMatches", func(t *testing.T) {
		// "shre" prefix-matches lik0003's token (tier 1) while the
		// substring rows stay tier 3 below it — lik0003 is itself
		// LIKE-visible and must not be duplicated by the scan.
		res := runSearchFor(t, app, "shre", searchCursor{}, 48)
		assert.Equal(t, []string{"lik0003"}, hitIDs(res.Hits))
		assert.Equal(t, 1, res.Hits[0].tier)
	})
	t.Run("MultiTokenAND", func(t *testing.T) {
		// Both tokens are mid-word fragments ("wander"→"ander",
		// "alone"→"lone"): FTS prefix terms miss both; tier 3 ANDs the
		// substrings within one column.
		res := runSearchFor(t, app, "ander lone", searchCursor{}, 48)
		require.Len(t, res.Hits, 1)
		assert.Equal(t, "lik0001", hitIDs(res.Hits)[0], "both substrings required in one column")
		assert.Equal(t, 3, res.Hits[0].tier)
	})
	t.Run("NoMatchAnywhere", func(t *testing.T) {
		res := runSearchFor(t, app, "zzzqqqxyzw", searchCursor{}, 48)
		assert.Empty(t, res.Hits, "a term matching nothing in any tier still returns nothing")
		assert.False(t, res.HasMore)
	})
	t.Run("Tier3PaginatesAcrossPages", func(t *testing.T) {
		// Continuation pages of an all-tier-3 result set keep walking
		// the substring scan via the tier-3 cursor offset.
		res := runSearchFor(t, app, "omcomi", searchCursor{}, 1)
		require.Len(t, res.Hits, 1)
		assert.Equal(t, "lik0001", res.Hits[0].img.ID)
		require.True(t, res.HasMore)
		assert.Equal(t, searchCursor{tier3: 1}, res.Next)
		res2 := runSearchFor(t, app, "omcomi", res.Next, 1)
		assert.Equal(t, []string{"lik0002"}, hitIDs(res2.Hits))
		assert.False(t, res2.HasMore)
	})
	t.Run("WildcardsLiteral", func(t *testing.T) {
		insertSearchRow(t, app, "lik0004", "2026-09-24 04:00:00", "underscore_test thing", "")
		// "line_t" is no FTS token sequence (unicode61 splits on _),
		// so this lands in tier 3; the _ must be escaped to a literal
		// instead of matching any character.
		res := runSearchFor(t, app, "erscore_t", searchCursor{}, 48)
		require.Len(t, res.Hits, 1)
		assert.Equal(t, "lik0004", res.Hits[0].img.ID)
		assert.Equal(t, 3, res.Hits[0].tier)
	})
	t.Run("ViewPreviewsMatchedColumn", func(t *testing.T) {
		// Snippet-less tier-3 cards preview the column that matched:
		// original for original-substring rows, enhanced for the
		// enhanced-only row (plain clamp, no <mark> pieces).
		cfg := testConfig()
		res := runSearchFor(t, app, "omcomi", searchCursor{}, 48)
		view := buildSearchView(cfg, "omcomi", res)
		require.Len(t, view.Cards, 2)
		assert.Equal(t, clampSnippet("wander randomcomimg alone", cfg.Search.SnippetChars), view.Cards[0].PromptSnippet)
		assert.Nil(t, view.Cards[0].SnippetParts)
		assert.Equal(t, clampSnippet("something incomcomimg here", cfg.Search.SnippetChars), view.Cards[1].PromptSnippet)
	})
}

// ---------------------------------------------------------------------------
// Tier 3 trigram acceleration
// ---------------------------------------------------------------------------

// TestBundledSQLiteTrigramSupport pins the driver capability the
// accelerator is built on: the bundled modernc.org/sqlite must compile
// in the FTS5 trigram tokenizer with substring-phrase semantics. If a
// future driver upgrade ever drops or changes it, this fails before any
// subtle search regression can.
func TestBundledSQLiteTrigramSupport(t *testing.T) {
	db := setupTestDB(t)
	var version string
	require.NoError(t, db.Get(&version, `SELECT sqlite_version()`))
	t.Logf("bundled SQLite %s (modernc.org/sqlite)", version)

	require.NoError(t, dbInsertImage(db, &dbImage{
		ID: "tri1001", SHA256: strings.Repeat("07", 32), Filename: "x.png",
		MimeType: "image/png", SizeBytes: 1, CreatedAt: "2026-09-24 00:00:00",
		ThumbStatus: thumbStatusPending, OriginalPrompt: "a CowShrew grazes 100%",
		MetaSource: metaSourceUpload,
	}))

	steps := []struct {
		match string
		want  int
		why   string
	}{
		{`"shrew"`, 1, "quoted phrase = substring match, mid-token"},
		{`"SHREW"`, 1, "default trigram options fold case"},
		{`"wshre"`, 1, "fragment entirely inside the fused token, crossing its intra-word case boundary (Cow|Shrew)"},
		{`"rew gr"`, 1, "phrase spanning the SPACE between tokens (…rew|grazes): trigram indexes spaces as ordinary code points"},
		{`"100%"`, 1, "% is a literal inside a quoted FTS5 string"},
		{`"graz" "cowsh"`, 1, "consecutive quoted terms are implicit AND"},
		{`"ab"`, 0, "sub-3-char token finds nothing (trigram floor), error-free"},
		{`"nope"`, 0, "absent substring matches nothing"},
	}
	var n int
	for _, s := range steps {
		require.NoError(t, db.Get(&n, `SELECT COUNT(*) FROM images_substring_fts WHERE images_substring_fts MATCH ?`, s.match), s.why)
		assert.Equal(t, s.want, n, "%s: MATCH %s", s.why, s.match)
	}
}

// TestTrigramFilterExpr pins the accelerator's gating and expression
// shape: every token quoted (FTS5-syntax-injection-proof, wildcards
// literal), AND-joined, and the whole accelerator disabled when any
// token is shorter than the trigram floor.
func TestTrigramFilterExpr(t *testing.T) {
	expr, ok := trigramFilterExpr([]string{"shrew", "graz"})
	assert.True(t, ok)
	assert.Equal(t, `"shrew" "graz"`, expr)

	for _, toks := range [][]string{{"ab"}, {"shrew", "ab"}, {"a", "b", "c"}} {
		_, ok := trigramFilterExpr(toks)
		assert.False(t, ok, "tokens %v: any short token disables the accelerator", toks)
	}

	// Rune count, not bytes: multibyte tokens at/above 3 code points.
	expr, ok = trigramFilterExpr([]string{"örld"})
	assert.True(t, ok)
	assert.Equal(t, `"örld"`, expr)

	// Wildcard-bearing tokens stay literal via quoting.
	expr, ok = trigramFilterExpr([]string{"100%", "under_score"})
	assert.True(t, ok)
	assert.Equal(t, `"100%" "under_score"`, expr)
}

// TestTier3TrigramPrefilterPlan is the proof the accelerator exists for:
// EXPLAIN QUERY PLAN of the EXACT production tier-3 query must consult
// images_substring_fts through its index and must not degenerate into a
// scan of the images table. Assertions are phrasing-tolerant (table
// name + per-line scan check) so SQLite plan-text drift doesn't break
// the pin.
func TestTier3TrigramPrefilterPlan(t *testing.T) {
	db := setupTestDB(t)

	query, args := buildLikeTierQuery([]string{"shrew"}, `"shrew"*`, 49, 0, siteCtx{})
	require.Contains(t, query, substringFTSTable, "all-≥3-char tokens take the accelerated shape")

	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	var plan []string
	for rows.Next() {
		var id, parent, notused, detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, plan, "EQP returned no plan rows")

	joined := strings.Join(plan, " | ")
	assert.Contains(t, joined, substringFTSTable+" VIRTUAL TABLE INDEX",
		"the trigram side table must be consulted through its index: %s", joined)
	for _, line := range plan {
		assert.NotEqual(t, "SCAN i", line, "accelerated query must not scan images: %s", joined)
		assert.NotEqual(t, "SCAN images", line, "accelerated query must not scan images: %s", joined)
	}

	// The <3-char fallback keeps today's plain scan shape.
	query, args = buildLikeTierQuery([]string{"ab"}, `"ab"`, 49, 0, siteCtx{})
	assert.NotContains(t, query, substringFTSTable, "short-token query takes the plain scan")
}

// TestTier3TrigramPrefilterPlanSafeSite proves the safe-site conjunct
// does not defeat the accelerator: the EXACT production tier-3 query
// with the visibility fragment appended must still consult
// images_substring_fts through its index and still drive images by
// rowid instead of scanning. The site predicate is an outer AND by
// design (intersect after retrieval), so the plan shape is unchanged.
func TestTier3TrigramPrefilterPlanSafeSite(t *testing.T) {
	db := setupTestDB(t)

	query, args := buildLikeTierQuery([]string{"shrew"}, `"shrew"*`, 49, 0, matrixSafeSiteCtx())
	require.Contains(t, query, substringFTSTable)
	require.Contains(t, query, "LOWER(i.network)")

	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	var plan []string
	for rows.Next() {
		var id, parent, notused, detail string
		require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
		plan = append(plan, detail)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, plan, "EQP returned no plan rows")

	joined := strings.Join(plan, " | ")
	assert.Contains(t, joined, substringFTSTable+" VIRTUAL TABLE INDEX",
		"the trigram side table must still be consulted through its index: %s", joined)
	for _, line := range plan {
		assert.NotEqual(t, "SCAN i", line, "accelerated query must not scan images: %s", joined)
		assert.NotEqual(t, "SCAN images", line, "accelerated query must not scan images: %s", joined)
	}
}

// TestTier3ShortTokenFallsBackToScan pins the fallback path end to end:
// a query with ANY token under 3 runes cannot use the trigram prefilter
// (its windows cannot index a 1–2-char string) and must still return
// correct rows through the plain LIKE scan.
func TestTier3ShortTokenFallsBackToScan(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "sht0001", "2026-09-24 01:00:00", "grabbing the railing", "")
	insertSearchRow(t, app, "sht0002", "2026-09-24 02:00:00", "shrew grabbing together", "")
	insertSearchRow(t, app, "sht0003", "2026-09-24 03:00:00", "unrelated", "")

	res := runSearchFor(t, app, "ab", searchCursor{}, 48)
	assert.Equal(t, []string{"sht0002", "sht0001"}, hitIDs(res.Hits),
		"2-char token still substring-matches via the scan path, recency within tier 3")
	for _, h := range res.Hits {
		assert.Equal(t, 3, h.tier)
	}

	// One short token poisons the whole prefilter; the scan still ANDs
	// both tokens per column.
	res = runSearchFor(t, app, "ab shrew", searchCursor{}, 48)
	assert.Equal(t, []string{"sht0002"}, hitIDs(res.Hits),
		"mixed short+long query falls back to the scan and requires every token in one column")
}

// TestTier3TrigramCaseFolding pins mixed-case substring matching through
// the accelerated path: "CowShrew" is found by q=shrew (and q=SHREW).
// "cowshrew" never tokenizes to a shrew-prefixed term, so the row can
// only surface via tier 3 — with the accelerator on, that means the
// trigram's case folding and LIKE's ASCII folding must agree.
func TestTier3TrigramCaseFolding(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "cse0001", "2026-09-24 01:00:00", "a CowShrew grazes", "")

	for _, q := range []string{"shrew", "SHREW", "Shrew"} {
		res := runSearchFor(t, app, q, searchCursor{}, 48)
		require.Len(t, res.Hits, 1, "query %q", q)
		assert.Equal(t, "cse0001", res.Hits[0].img.ID, "query %q", q)
		assert.Equal(t, 3, res.Hits[0].tier, "query %q: fused token is invisible to FTS, substring tier only", q)
	}
}

// TestTier3MultibyteSubstring pins the accelerator's superset property
// on multibyte text: a ≥3-rune non-ASCII fragment rides the trigram
// prefilter (3 code points = one full window) and must still surface the
// row — LIKE accepts it, so the trigram index cannot be allowed to miss
// it. This is the regression class a driver change would silently
// introduce (dropped tier-3 rows), so it gets its own end-to-end pin.
func TestTier3MultibyteSubstring(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "uni0001", "2026-09-24 01:00:00", "höllo wörld", "")

	// "örl" is a 3-rune mid-token fragment: FTS prefix terms can't see
	// it (the token starts with "w"), so the row can only surface via
	// the accelerated tier-3 path.
	res := runSearchFor(t, app, "örl", searchCursor{}, 48)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "uni0001", res.Hits[0].img.ID)
	assert.Equal(t, 3, res.Hits[0].tier)
}

// TestTier3QuotedPhraseSubstring pins the quoted-phrase token through
// the accelerated tier-3 path end to end. A user phrase like "n com"
// arrives from buildFTSQuery as ONE token containing a space; the FTS
// tiers read it as an adjacency phrase (token "n" immediately followed
// by token "com" — nothing matches), while the trigram prefilter and
// the LIKE conjuncts both read it as a literal substring spanning the
// space inside "walkin comin". The LIKE conjuncts stay authoritative:
// a row with the same letters but no space is excluded even though a
// sloppier matcher could confuse the two.
func TestTier3QuotedPhraseSubstring(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "qph0001", "2026-09-24 01:00:00", "walkin comin down the street", "")
	insertSearchRow(t, app, "qph0002", "2026-09-24 02:00:00", "ncompany tight", "")

	res := runSearchFor(t, app, `"n com"`, searchCursor{}, 48)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "qph0001", res.Hits[0].img.ID, "space-spanning phrase matches as a substring")
	assert.Equal(t, 3, res.Hits[0].tier, "no FTS tier sees tokens 'n'/'com' — substring tier only")
	assert.False(t, res.Hits[0].previewEnhanced, "original-column substring previews the original column")
}

// ---------------------------------------------------------------------------
// hidden=0 filtering
// ---------------------------------------------------------------------------

func TestSearchHiddenFiltered(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertSearchRow(t, app, "vis0001", "2026-09-24 01:00:00", "secret shrew public", "")
	insertSearchRow(t, app, "hid0001", "2026-09-24 02:00:00", "secret shrew hidden", "", func(img *dbImage) {
		img.Hidden = true
	})
	insertSearchRow(t, app, "hid0002", "2026-09-24 03:00:00", "innocent text", "secret shrew enhanced", func(img *dbImage) {
		img.Hidden = true
	})
	// Tier-3 coverage too: hidden rows must not surface through the
	// always-on substring scan.
	insertSearchRow(t, app, "hid0003", "2026-09-24 04:00:00", "xrandomcomimgx", "", func(img *dbImage) {
		img.Hidden = true
	})

	t.Run("FTS", func(t *testing.T) {
		res := runSearchFor(t, app, "shrew", searchCursor{}, 48)
		assert.Equal(t, []string{"vis0001"}, hitIDs(res.Hits))
	})
	t.Run("LIKE", func(t *testing.T) {
		res := runSearchFor(t, app, "omcomi", searchCursor{}, 48)
		assert.Empty(t, res.Hits)
	})
	t.Run("HTTP", func(t *testing.T) {
		_, body := getPage(t, ts.URL, "/search?q=shrew")
		assert.Contains(t, body, "vis0001")
		assert.NotContains(t, body, "hid0001")
		assert.NotContains(t, body, "hid0002")
	})
}

// ---------------------------------------------------------------------------
// Snippets
// ---------------------------------------------------------------------------

func TestSearchSnippetColumnPerTier(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertSearchRow(t, app, "sni0001", "2026-09-24 01:00:00",
		"<script>alert(1)</script> shrew on parade", "")
	insertSearchRow(t, app, "sni0002", "2026-09-24 02:00:00",
		"a cat", "the shrew dances tonight")

	res := runSearchFor(t, app, "shrew", searchCursor{}, 48)
	require.Len(t, res.Hits, 2)

	var snippetText func(h searchHit) string
	snippetText = func(h searchHit) string {
		var b strings.Builder
		for _, p := range splitSnippet(h.snippet) {
			b.WriteString(p.Text)
		}
		return b.String()
	}

	t.Run("Tier1SnippetsOriginal", func(t *testing.T) {
		h := res.Hits[0]
		require.Equal(t, "sni0001", h.img.ID)
		parts := splitSnippet(h.snippet)
		require.NotEmpty(t, parts)
		assert.Equal(t, snippetText(h), "<script>alert(1)</script> shrew on parade",
			"snippet text comes from the original prompt")
		assert.Contains(t, parts, snippetPart{Text: "shrew", Hit: true}, "the match is the marked piece")
	})
	t.Run("Tier2SnippetsEnhanced", func(t *testing.T) {
		h := res.Hits[1]
		require.Equal(t, "sni0002", h.img.ID)
		assert.Equal(t, "the shrew dances tonight", snippetText(h),
			"snippet text comes from the enhanced prompt")
		assert.Contains(t, splitSnippet(h.snippet), snippetPart{Text: "shrew", Hit: true})
	})
}

// TestSearchSnippetEscapedInHTML pins the full rendering path: snippet
// pieces ride through html/template auto-escaping — the injected tag
// arrives entity-escaped, and the only literal markup is the template's
// own <mark> elements.
func TestSearchSnippetEscapedInHTML(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertSearchRow(t, app, "esc0001", "2026-09-24 01:00:00",
		"<script>alert(1)</script> shrew <img src=x onerror=alert(2)>", "")

	_, body := getPage(t, ts.URL, "/search?q=shrew")

	assert.Contains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;", "untrusted snippet text escaped")
	assert.Contains(t, body, "&lt;img src=x onerror=alert(2)&gt;")
	assert.Contains(t, body, "<mark>shrew</mark>", "highlight markup is the template's own literal")
	assert.NotContains(t, body, "<script>alert(1)")
	assert.NotContains(t, body, "<img src=x onerror")
	_, frag := getPage(t, ts.URL, "/search-fragment?q=shrew")
	assert.Contains(t, frag, "&lt;script&gt;alert(1)&lt;/script&gt;")
	assert.NotContains(t, frag, "<script>alert(1)")
}

func TestSearchSnippetCharsConfigHonored(t *testing.T) {
	// snippet_chars drives snippet()'s token window (FTS counts
	// tokens); a bigger config must produce a longer rendered snippet
	// around the same match.
	app := newTestApp(t, testConfig())
	prompt := strings.TrimSuffix(strings.Repeat("wordy filler token ", 40), " ")
	prompt = "START " + prompt + " shrew END " + prompt
	insertSearchRow(t, app, "cfg0001", "2026-09-24 01:00:00", prompt, "")

	snipLen := func(chars int) int {
		res, err := runSearch(app.db, "shrew", searchCursor{}, 10, 2, snippetTokenWindow(chars), siteCtx{})
		require.NoError(t, err)
		require.Len(t, res.Hits, 1)
		n := 0
		for _, p := range splitSnippet(res.Hits[0].snippet) {
			n += len(p.Text)
		}
		return n
	}
	small, big := snipLen(48), snipLen(1024)
	assert.Less(t, small, big, "larger search.snippet_chars yields a wider snippet window")
}

// ---------------------------------------------------------------------------
// Pagination across the tier boundary
// ---------------------------------------------------------------------------

func TestSearchPaginationAcrossTierBoundary(t *testing.T) {
	app := newTestApp(t, testConfig())
	// Identical prompts within each tier -> identical bm25 scores ->
	// the rowid tiebreak gives a deterministic order (insertion order).
	var wantTier1, wantTier2 []string
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("pag000%d", i)
		insertSearchRow(t, app, id, fmt.Sprintf("2026-09-24 %02d:00:00", i), "zebra crossing", "")
		wantTier1 = append(wantTier1, id)
	}
	for i := 7; i <= 10; i++ {
		id := fmt.Sprintf("pag000%d", i)
		insertSearchRow(t, app, id, fmt.Sprintf("2026-09-24 %02d:00:00", i), "plain landscape", "zebra crossing")
		wantTier2 = append(wantTier2, id)
	}

	var got []string
	cur := searchCursor{}
	for page := 1; ; page++ {
		res := runSearchFor(t, app, "zebra", cur, 4)
		require.NotEmpty(t, res.Hits, "page %d unexpectedly empty", page)
		got = append(got, hitIDs(res.Hits)...)
		if !res.HasMore {
			break
		}
		cur = res.Next
		require.Less(t, page, 5, "pagination did not terminate")
	}

	want := append(append([]string{}, wantTier1...), wantTier2...)
	assert.Equal(t, want, got, "tier 1 drains fully, tier 2 follows — no dupes, no gaps, stable order")

	// Page-by-page shape: page 1 is pure tier 1 (tier 2 untouched in
	// the cursor); page 2 straddles the boundary; page 3 finishes
	// tier 2.
	res1 := runSearchFor(t, app, "zebra", searchCursor{}, 4)
	require.Len(t, res1.Hits, 4)
	for _, h := range res1.Hits {
		assert.Equal(t, 1, h.tier)
	}
	assert.Equal(t, searchCursor{tier1: 4, tier2: 0, tier3: 0}, res1.Next, "tier 2 offset untouched while tier 1 fills pages")
	assert.True(t, res1.HasMore)

	res2 := runSearchFor(t, app, "zebra", res1.Next, 4)
	require.Len(t, res2.Hits, 4)
	assert.Equal(t, []int{1, 1, 2, 2}, []int{res2.Hits[0].tier, res2.Hits[1].tier, res2.Hits[2].tier, res2.Hits[3].tier},
		"boundary page stitches remaining tier 1 then tier 2")
	assert.Equal(t, searchCursor{tier1: 6, tier2: 2, tier3: 0}, res2.Next)
	assert.True(t, res2.HasMore)

	res3 := runSearchFor(t, app, "zebra", res2.Next, 4)
	require.Len(t, res3.Hits, 2)
	for _, h := range res3.Hits {
		assert.Equal(t, 2, h.tier)
	}
	assert.False(t, res3.HasMore)
	assert.Equal(t, searchCursor{tier1: 6, tier2: 4, tier3: 0}, res3.Next, "both tiers fully consumed")
}

// TestSearchPaginationAcrossThreeTiers walks a result set spanning all
// three tiers with a page size that straddles BOTH boundaries, pinning
// no-dupes/no-gaps pagination through tier 3 and the independent
// per-tier cursor offsets.
func TestSearchPaginationAcrossThreeTiers(t *testing.T) {
	app := newTestApp(t, testConfig())
	// Identical prompts within each tier -> deterministic order
	// (tier 1/2: identical bm25 scores, rowid tiebreak = insertion
	// order; tier 3: recency).
	var wantTier1, wantTier2, wantTier3 []string
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("tri000%d", i)
		insertSearchRow(t, app, id, fmt.Sprintf("2026-09-24 %02d:00:00", i), "zebra crossing", "")
		wantTier1 = append(wantTier1, id)
	}
	for i := 4; i <= 6; i++ {
		id := fmt.Sprintf("tri000%d", i)
		insertSearchRow(t, app, id, fmt.Sprintf("2026-09-24 %02d:00:00", i), "plain landscape", "zebra crossing")
		wantTier2 = append(wantTier2, id)
	}
	// Suffix-only rows: "cowzebra" never tokenizes to a zebra-prefixed
	// term, so these are tier 3; recency order is newest first.
	for i := 7; i <= 9; i++ {
		id := fmt.Sprintf("tri000%d", i)
		insertSearchRow(t, app, id, fmt.Sprintf("2026-09-24 %02d:00:00", i), "a cowzebra grazes", "")
		wantTier3 = append(wantTier3, id)
	}
	want := append(append(append([]string{}, wantTier1...), wantTier2...),
		[]string{"tri0009", "tri0008", "tri0007"}...)

	var got []string
	cur := searchCursor{}
	for page := 1; ; page++ {
		res := runSearchFor(t, app, "zebra", cur, 4)
		require.NotEmpty(t, res.Hits, "page %d unexpectedly empty", page)
		got = append(got, hitIDs(res.Hits)...)
		if !res.HasMore {
			break
		}
		cur = res.Next
		require.Less(t, page, 5, "pagination did not terminate")
	}

	assert.Equal(t, want, got, "tier 1 drains, tier 2 follows, tier 3 finishes — no dupes, no gaps, stable order")

	// Page-by-page shape: page 1 drains tier 1 and dips into tier 2;
	// page 2 finishes tier 2 and dips into tier 3; page 3 finishes
	// tier 3.
	res1 := runSearchFor(t, app, "zebra", searchCursor{}, 4)
	require.Len(t, res1.Hits, 4)
	assert.Equal(t, []int{1, 1, 1, 2}, []int{res1.Hits[0].tier, res1.Hits[1].tier, res1.Hits[2].tier, res1.Hits[3].tier})
	assert.Equal(t, searchCursor{tier1: 3, tier2: 1, tier3: 0}, res1.Next)
	assert.True(t, res1.HasMore)

	res2 := runSearchFor(t, app, "zebra", res1.Next, 4)
	require.Len(t, res2.Hits, 4)
	assert.Equal(t, []int{2, 2, 3, 3}, []int{res2.Hits[0].tier, res2.Hits[1].tier, res2.Hits[2].tier, res2.Hits[3].tier},
		"boundary page stitches remaining tier 2 then tier 3")
	assert.Equal(t, searchCursor{tier1: 3, tier2: 3, tier3: 2}, res2.Next)
	assert.True(t, res2.HasMore)

	res3 := runSearchFor(t, app, "zebra", res2.Next, 4)
	require.Len(t, res3.Hits, 1)
	assert.Equal(t, 3, res3.Hits[0].tier)
	assert.False(t, res3.HasMore)
	assert.Equal(t, searchCursor{tier1: 3, tier2: 3, tier3: 3}, res3.Next, "all three tiers fully consumed")
}

// ---------------------------------------------------------------------------
// HTTP surface
// ---------------------------------------------------------------------------

func TestSearchRoutes(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertSearchRow(t, app, "htt0001", "2026-09-24 01:00:00", "shrew comin in hot", "")
	insertSearchRow(t, app, "htt0002", "2026-09-24 02:00:00", "a cat", "the shrew dances")

	t.Run("FullPageRenders", func(t *testing.T) {
		resp := fetchPath(t, ts, "/search?q=shrew")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"))
		assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
		body := readBody(t, resp)
		assert.Contains(t, body, "<!DOCTYPE html>")
		assert.Contains(t, body, `data-page="gallery"`, "results page is the gallery in search mode (JS modules)")
		assert.Contains(t, body, `<form id="search-form" action="/search" method="get" role="search">`, "no-JS form present")
		assert.Contains(t, body, `value="shrew"`, "query prefilled")
		assert.Contains(t, body, "search: shrew", "title carries the query")
		assert.Contains(t, body, `<article class="card" data-id="htt0001"`)
		assert.Contains(t, body, "<mark>shrew</mark>", "highlighted snippet")
		assert.Less(t, strings.Index(body, "htt0001"), strings.Index(body, "htt0002"), "tier order on the page")
	})
	t.Run("EmptyQueryPage", func(t *testing.T) {
		resp := fetchPath(t, ts, "/search")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := readBody(t, resp)
		assert.Contains(t, body, `action="/search"`)
		assert.NotContains(t, body, `<article class="card"`)
		assert.Contains(t, body, "nothing searched yet", "empty query shows the nothing-searched note, not a bare grid")
		assert.NotContains(t, body, "no results for", "empty query is not a no-results state")
	})
	t.Run("NoResultsNote", func(t *testing.T) {
		resp := fetchPath(t, ts, "/search?q="+url.QueryEscape("nothing matches this"))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Contains(t, readBody(t, resp), "no results for nothing matches this")
	})
	t.Run("FragmentIsCardsOnly", func(t *testing.T) {
		resp := fetchPath(t, ts, "/search-fragment?q=shrew")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "no-cache", resp.Header.Get("Cache-Control"))
		body := readBody(t, resp)
		assert.Contains(t, body, `<article class="card"`)
		assert.NotContains(t, body, "<!DOCTYPE", "fragment carries no page chrome")
		assert.NotContains(t, body, `<form`)
	})
	t.Run("FragmentPaginates", func(t *testing.T) {
		for i := 1; i <= 3; i++ {
			insertSearchRow(t, app, fmt.Sprintf("htp000%d", i), fmt.Sprintf("2026-09-24 %02d:30:00", i), "zebra parade", "")
		}
		resp := fetchPath(t, ts, "/search-fragment?q=zebra&after="+url.QueryEscape("1:2|2:0|3:0"))
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body := readBody(t, resp)
		assert.Contains(t, body, `data-id="htp0003"`, "offset cursor consumed 2 tier-1 rows")
		assert.NotContains(t, body, `data-id="htp0001"`)
		assert.NotContains(t, body, `data-id="htp0002"`)
	})
	t.Run("ExactLiteralBeatsIDDispatcher", func(t *testing.T) {
		// "search" / "search-fragment" are not valid image ids (wrong
		// length); if the exact-literal patterns were missing, the
		// "GET /" catch-all's id dispatcher would 404 them.
		for _, p := range []string{"/search", "/search?q=x"} {
			resp := fetchPath(t, ts, p)
			assert.Equal(t, http.StatusOK, resp.StatusCode, p)
		}
	})
	t.Run("GalleryPageCarriesNoJSForm", func(t *testing.T) {
		_, body := getPage(t, ts.URL, "/")
		assert.Contains(t, body, `action="/search" method="get"`)
		assert.Contains(t, body, `name="q"`)
	})
	t.Run("MalformedCursorIs400", func(t *testing.T) {
		for _, c := range []string{"garbage", "1:0", "1:0|2:0", "3:0|1:0|2:0"} {
			resp := fetchPath(t, ts, "/search?q=x&after="+url.QueryEscape(c))
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "cursor %q", c)
			resp = fetchPath(t, ts, "/search-fragment?q=x&after="+url.QueryEscape(c))
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "cursor %q", c)
		}
	})
	t.Run("PostIs405", func(t *testing.T) {
		req, err := http.NewRequest("POST", ts.URL+"/search", strings.NewReader(""))
		require.NoError(t, err)
		resp := doReq(t, ts, req)
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})
}

// injectSearchFailure points both search DB seams at an always-failing
// function.
func injectSearchFailure(t *testing.T) {
	t.Helper()
	failFTS := func(db *sqlx.DB, tier int, expr, tier1Expr string, offset, limit, snippetTokens int, sc siteCtx) ([]searchHit, error) {
		return nil, fmt.Errorf("injected fts failure")
	}
	failLIKE := func(db *sqlx.DB, tokens []string, ftsExpr string, offset, limit int, sc siteCtx) ([]searchHit, error) {
		return nil, fmt.Errorf("injected like failure")
	}
	origFTS, origLIKE := dbSearchFTSTierFn, dbSearchLIKETierFn
	dbSearchFTSTierFn = failFTS
	dbSearchLIKETierFn = failLIKE
	t.Cleanup(func() {
		dbSearchFTSTierFn = origFTS
		dbSearchLIKETierFn = origLIKE
	})
}

func TestSearchDBErrorReturns500(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	insertSearchRow(t, app, "err0001", "2026-09-24 01:00:00", "shrew", "")
	injectSearchFailure(t)

	for _, p := range []string{"/search?q=shrew", "/search-fragment?q=shrew"} {
		resp := fetchPath(t, ts, p)
		assert.Equal(t, http.StatusInternalServerError, resp.StatusCode, p)
	}
}

// ---------------------------------------------------------------------------
// Safe-site visibility (task 3): the predicate rides every tier
// ---------------------------------------------------------------------------

// matrixSafeSiteCtx is the matrix rows' site context: safe site, libera the
// only allowed network (matches safeSiteTestConfig).
func matrixSafeSiteCtx() siteCtx {
	return siteCtx{Safe: true, networks: []string{"libera"}}
}

// TestSearchSiteMatrix runs the six-row visibility matrix through
// runSearch and the HTTP search surfaces on both hosts. The rows are
// placed one per tier role (see seedSiteMatrix): tier 1 keeps its
// visible row and drops the invisible one, tier 2 does the same, tier 3
// (trigram-accelerated here — "gribble" is 7 runes) keeps its
// safe-visible row. Default site unchanged.
func TestSearchSiteMatrix(t *testing.T) {
	app := newTestApp(t, safeSiteTestConfig())
	ts := newTestServer(t, app)
	seedSiteMatrix(t, app)

	t.Run("SafeSiteRunSearch", func(t *testing.T) {
		res := runSearchForSite(t, app, "gribble", searchCursor{}, 48, matrixSafeSiteCtx())
		assert.Equal(t, []string{"sit0001", "sit0004", "sit0005"}, hitIDs(res.Hits),
			"tier order preserved; exactly the safe-visible rows survive in every tier")
		assert.Equal(t, []int{1, 2, 3}, []int{res.Hits[0].tier, res.Hits[1].tier, res.Hits[2].tier},
			"one surviving row per tier keeps each tier's structural position")
	})
	t.Run("DefaultSiteRunSearch", func(t *testing.T) {
		res := runSearchForSite(t, app, "gribble", searchCursor{}, 48, siteCtx{})
		assert.ElementsMatch(t, []string{"sit0001", "sit0002", "sit0003", "sit0004", "sit0005"}, hitIDs(res.Hits))
		tiers := map[string]int{}
		for _, h := range res.Hits {
			tiers[h.img.ID] = h.tier
		}
		assert.Equal(t, 1, tiers["sit0001"])
		assert.Equal(t, 1, tiers["sit0002"], "default host still sees the efnet-unknown tier-1 row")
		assert.Equal(t, 2, tiers["sit0003"])
		assert.Equal(t, 2, tiers["sit0004"])
		assert.Equal(t, 3, tiers["sit0005"])
	})
	t.Run("SafeSitePaginationSkipsInvisible", func(t *testing.T) {
		// Small pages prove the OFFSET stitch composes with the filter:
		// draining the filtered tiers never surfaces an invisible row.
		var got []string
		cur := searchCursor{}
		for page := 1; ; page++ {
			res := runSearchForSite(t, app, "gribble", cur, 1, matrixSafeSiteCtx())
			require.Less(t, page, 6, "pagination did not terminate")
			got = append(got, hitIDs(res.Hits)...)
			if !res.HasMore {
				break
			}
			cur = res.Next
		}
		assert.Equal(t, []string{"sit0001", "sit0004", "sit0005"}, got)
	})
	t.Run("SafeHostHTTPPage", func(t *testing.T) {
		status, body := getPageHost(t, ts, "safe.example.com", "/search?q=gribble")
		require.Equal(t, http.StatusOK, status)
		assertIDsInOrder(t, body, "sit0001", "sit0004", "sit0005")
		for _, invisible := range []string{"sit0002", "sit0003", "sit0006"} {
			assert.NotContains(t, body, invisible)
		}
	})
	t.Run("SafeHostHTTPFragment", func(t *testing.T) {
		status, body := getPageHost(t, ts, "safe.example.com", "/search-fragment?q=gribble")
		require.Equal(t, http.StatusOK, status)
		assertIDsInOrder(t, body, "sit0001", "sit0004", "sit0005")
		for _, invisible := range []string{"sit0002", "sit0003", "sit0006"} {
			assert.NotContains(t, body, invisible)
		}
	})
	t.Run("DefaultHostHTTPPage", func(t *testing.T) {
		status, body := getPage(t, ts.URL, "/search?q=gribble")
		require.Equal(t, http.StatusOK, status)
		for _, id := range []string{"sit0001", "sit0002", "sit0003", "sit0004", "sit0005"} {
			assert.Contains(t, body, id, "default host sees id %s", id)
		}
		assert.NotContains(t, body, "sit0006", "hidden stays out of search on the default host")
	})
}

// TestSearchTier3SiteFilteredBothShapes proves tier 3's site filter on
// BOTH query shapes: the trigram-accelerated shape (every token ≥3
// runes) and the short-token fallback plain scan. In each shape an
// invisible efnet-unknown row and a visible libera row both substring-
// match; only the visible one may surface on the safe site. This is
// the regression class the "outer AND on the images row" rule exists
// to prevent — the filter must ride the outer query, never the MATCH
// expressions or the prefilter subquery.
func TestSearchTier3SiteFilteredBothShapes(t *testing.T) {
	app := newTestApp(t, safeSiteTestConfig())
	ts := newTestServer(t, app)
	safe := matrixSafeSiteCtx()

	t.Run("TrigramAccelerated", func(t *testing.T) {
		// "gribble" (7 runes) engages the accelerator; both rows match
		// only via substring ("cowgribble" is invisible to FTS prefix
		// terms).
		insertImage(t, app, "vis0001", "2026-09-26 01:00:00", func(img *dbImage) {
			img.Network = ptrStr("libera") // explicit: the row's safe-host visibility hinges on the allowed origin
			img.OriginalPrompt = "a cowgribble grazes"
		})
		insertImage(t, app, "inv0001", "2026-09-26 02:00:00", func(img *dbImage) {
			img.Network = ptrStr("efnet")
			img.OriginalPrompt = "another cowgribble lurks"
		})

		res := runSearchForSite(t, app, "gribble", searchCursor{}, 48, safe)
		assert.Equal(t, []string{"vis0001"}, hitIDs(res.Hits), "accelerated tier 3 drops the invisible row")

		_, body := getPageHost(t, ts, "safe.example.com", "/search-fragment?q=gribble")
		assert.Contains(t, body, "vis0001")
		assert.NotContains(t, body, "inv0001")

		res = runSearchForSite(t, app, "gribble", searchCursor{}, 48, siteCtx{})
		assert.ElementsMatch(t, []string{"vis0001", "inv0001"}, hitIDs(res.Hits), "default host unchanged")
	})
	t.Run("ShortTokenFallbackScan", func(t *testing.T) {
		// "qq" is 2 runes: trigramFilterExpr declines and tier 3 runs
		// the plain LIKE scan. Both rows carry "qq" mid-word (xqqx /
		// yqqy — no token starts with qq, so FTS tiers see nothing).
		insertImage(t, app, "vis0002", "2026-09-26 03:00:00", func(img *dbImage) {
			img.Network = ptrStr("libera") // explicit: the row's safe-host visibility hinges on the allowed origin
			img.OriginalPrompt = "an xqqx fragment"
		})
		insertImage(t, app, "inv0002", "2026-09-26 04:00:00", func(img *dbImage) {
			img.Network = ptrStr("efnet")
			img.OriginalPrompt = "a yqqy fragment"
		})

		res := runSearchForSite(t, app, "qq", searchCursor{}, 48, safe)
		assert.Equal(t, []string{"vis0002"}, hitIDs(res.Hits), "fallback scan drops the invisible row")

		res = runSearchForSite(t, app, "qq", searchCursor{}, 48, siteCtx{})
		assert.ElementsMatch(t, []string{"vis0002", "inv0002"}, hitIDs(res.Hits), "default host unchanged")
	})
}

// TestBuildLikeTierQuerySiteFilterShape pins the SQL SHAPE of the safe
// site's tier-3 conjunct: the visibility fragment is an outer AND
// appended after every existing conjunct (site args bind after the
// trigram expression), and neither the FTS NOT IN subquery nor the
// trigram prefilter subquery is touched — preserving the
// trigram-superset argument (the predicate intersects AFTER
// retrieval).
func TestBuildLikeTierQuerySiteFilterShape(t *testing.T) {
	t.Run("DefaultSiteQueryUnchanged", func(t *testing.T) {
		query, args := buildLikeTierQuery([]string{"gribble"}, `"gribble"*`, 49, 0, siteCtx{})
		assert.NotContains(t, query, "LOWER(i.network)")
		assert.NotContains(t, query, "i.safety")
		assert.Len(t, args, 7, "orig×2, enh, ftsExpr, trigramExpr, limit, offset — no site args")
	})
	t.Run("SafeSiteFragmentAppendedAfterTrigram", func(t *testing.T) {
		query, args := buildLikeTierQuery([]string{"gribble"}, `"gribble"*`, 49, 0, matrixSafeSiteCtx())

		assert.Contains(t, query, substringFTSTable, "accelerated shape intact")
		assert.Contains(t, query, " AND (LOWER(i.network) IN (?) OR i.safety = 'safe')",
			"site predicate is one outer AND conjunct")
		assert.Equal(t, 1, strings.Count(query, "LOWER(i.network)"), "exactly one site conjunct")
		assert.Equal(t, 1, strings.Count(query, "images_fts MATCH"),
			"the images_fts NOT IN subquery is untouched")
		assert.Contains(t, query,
			"SELECT rowid FROM "+substringFTSTable+" WHERE "+substringFTSTable+" MATCH ?",
			"trigram prefilter subquery text is verbatim-untouched")
		assert.Less(t, strings.Index(query, substringFTSTable), strings.Index(query, "LOWER(i.network)"),
			"site conjunct sits after the trigram prefilter, never inside it")
		assert.Less(t, strings.Index(query, "LOWER(i.network)"), strings.Index(query, "ORDER BY"),
			"site conjunct is a WHERE-level conjunct, not trailing junk after ORDER BY")
		assert.Equal(t, 8, len(args), "one extra arg per allowed network")

		// Arg order must match placeholder order: orig (CASE), orig
		// (WHERE), enh (WHERE), ftsExpr, trigramExpr, THEN site args,
		// then limit/offset.
		assert.Equal(t, "%gribble%", args[0])
		assert.Equal(t, "%gribble%", args[1])
		assert.Equal(t, "%gribble%", args[2])
		assert.Equal(t, `"gribble"*`, args[3])
		assert.Equal(t, `"gribble"`, args[4])
		assert.Equal(t, "libera", args[5])
		assert.Equal(t, 49, args[6])
		assert.Equal(t, 0, args[7])
	})
}

// TestBuildFTSTierQuerySiteFilterShape pins the SQL SHAPE of the FTS
// tiers' site handling — the tier-1/2 twin of
// TestBuildLikeTierQuerySiteFilterShape (the default-host identity
// was previously pinned only for tier 3). Default site: the query
// text carries NO site predicate at all (no LOWER(i.network), no
// i.safety) and no site args bind, so default-host identity cannot
// silently drift. Safe site: the fragment is one outer AND conjunct
// after hidden=0 and before ORDER BY, the MATCH expressions and tier
// 2's NOT IN subquery stay untouched, and site args bind after the
// MATCH placeholders and before limit/offset.
func TestBuildFTSTierQuerySiteFilterShape(t *testing.T) {
	expr := `"gribble"*`
	tier1Expr := ftsTier1Expr(expr)

	t.Run("DefaultSiteTier1QueryUnchanged", func(t *testing.T) {
		query, args := buildFTSTierQuery(1, expr, tier1Expr, 0, 49, 27, siteCtx{})

		assert.NotContains(t, query, "LOWER(i.network)")
		assert.NotContains(t, query, "i.safety")
		assert.Equal(t, 1, strings.Count(query, "images_fts MATCH ?"),
			"tier 1 keeps exactly its own MATCH, no subquery")
		assert.Len(t, args, 7, "4 snippet params + tier1Expr + limit + offset — no site args")
	})
	t.Run("DefaultSiteTier2QueryUnchanged", func(t *testing.T) {
		query, args := buildFTSTierQuery(2, expr, tier1Expr, 0, 49, 27, siteCtx{})

		assert.NotContains(t, query, "LOWER(i.network)")
		assert.NotContains(t, query, "i.safety")
		assert.Equal(t, 2, strings.Count(query, "images_fts MATCH ?"),
			"tier 2 keeps its own MATCH plus the NOT IN subquery's")
		assert.Len(t, args, 8, "4 snippet params + expr + tier1Expr + limit + offset — no site args")
	})
	t.Run("SafeSiteTier1FragmentAppendedAfterHidden", func(t *testing.T) {
		query, args := buildFTSTierQuery(1, expr, tier1Expr, 0, 49, 27, matrixSafeSiteCtx())

		assert.Contains(t, query, " AND (LOWER(i.network) IN (?) OR i.safety = 'safe')",
			"site predicate is one outer AND conjunct")
		assert.Equal(t, 1, strings.Count(query, "LOWER(i.network)"), "exactly one site conjunct")
		assert.Less(t, strings.Index(query, "i.hidden = 0"), strings.Index(query, "LOWER(i.network)"),
			"site conjunct sits after the hidden filter, never inside the MATCH")
		assert.Less(t, strings.Index(query, "LOWER(i.network)"), strings.Index(query, "ORDER BY"),
			"site conjunct is a WHERE-level conjunct, not trailing junk after ORDER BY")
		assert.Equal(t, 1, strings.Count(query, "images_fts MATCH ?"))
		assert.Equal(t, []any{snippetStartMarker, snippetEndMarker, snippetEllipsis, 27, tier1Expr, "libera", 49, 0}, args,
			"snippet params, tier1 MATCH, THEN the site arg, then limit/offset")
	})
	t.Run("SafeSiteTier2SubqueryUntouched", func(t *testing.T) {
		query, args := buildFTSTierQuery(2, expr, tier1Expr, 0, 49, 27, matrixSafeSiteCtx())

		assert.Contains(t, query, " AND (LOWER(i.network) IN (?) OR i.safety = 'safe')")
		assert.Equal(t, 1, strings.Count(query, "LOWER(i.network)"))
		assert.Contains(t, query,
			"SELECT rowid FROM images_fts WHERE images_fts MATCH ?",
			"tier-2 NOT IN subquery text is verbatim-untouched")
		assert.Equal(t, 2, strings.Count(query, "images_fts MATCH ?"))
		assert.Less(t, strings.Index(query, "i.hidden = 0"), strings.Index(query, "LOWER(i.network)"))
		assert.Less(t, strings.Index(query, "LOWER(i.network)"), strings.Index(query, "ORDER BY"))
		assert.Equal(t, []any{snippetStartMarker, snippetEndMarker, snippetEllipsis, 27, expr, tier1Expr, "libera", 49, 0}, args,
			"snippet params, both MATCH exprs, THEN the site arg, then limit/offset")
	})
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}
