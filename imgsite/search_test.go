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
	res, err := runSearch(app.db, q, cur, limit, 2, snippetTokenWindow(160))
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
		res, err := runSearch(app.db, "shrew", searchCursor{}, 10, 2, snippetTokenWindow(chars))
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
	failFTS := func(db *sqlx.DB, tier int, expr, tier1Expr string, offset, limit, snippetTokens int) ([]searchHit, error) {
		return nil, fmt.Errorf("injected fts failure")
	}
	failLIKE := func(db *sqlx.DB, tokens []string, ftsExpr string, offset, limit int) ([]searchHit, error) {
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

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(b)
}
