package markdowntoirc

// Structure-aware fuzz/survey harness for the streaming markdown renderer.
//
// Two oracles:
//
//	P1 (chunk invariance): StreamingRenderer output must be identical no
//	   matter how the same document is split into deltas. IRC-visible output
//	   is normalized (empty lines dropped, outer newlines trimmed) since
//	   sendIRC skips empty lines anyway.
//	P2 (parity): the incremental output must equal MarkdownToIRCStream()
//	   rendered over the whole document — the intended streaming-mode
//	   semantics (fixed code padding, no tables, no highlighting). Any
//	   divergence means the hand-rolled line state machine in Process()
//	   disagrees with the real markdown parser about structure — the class
//	   of nested-construct bugs that got streaming markdown disabled.
//
// Test entry points:
//
//	TestStreamingChunkInvariance — always on; seeded, fast. P1 must ALWAYS
//	  pass, for ANY input, not just well-formed markdown.
//	TestStreamingSurvey — env-gated (DAVE_STREAM_SURVEY=1). Generates many
//	  documents, finds P1/P2 violations, SHRINKS each to a minimal
//	  reproducer, dedupes, and RECORDS fixtures to
//	  testdata/streaming/known_bugs/ so the markdown that breaks rendering
//	  is never lost again.
//	TestStreamingKnownBugs — always on. Loads recorded fixtures and pins
//	  them as known failures (xfail): each fixture must still diverge. When
//	  the renderer is fixed these flip to passing and the test tells you to
//	  promote them into regression fixtures (testdata/streaming/).

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	mrand "math/rand/v2"
)

// ---------------------------------------------------------------------------
// deterministic seeding
// ---------------------------------------------------------------------------

func envSeed(name string) (uint64, bool) {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

func fuzzRand() (*mrand.Rand, uint64) {
	if seed, ok := envSeed("DAVE_STREAM_SEED"); ok {
		return mrand.New(mrand.NewPCG(seed, seed^0x5eed)), seed
	}
	seed := mrand.Uint64()
	return mrand.New(mrand.NewPCG(seed, seed^0x5eed)), seed
}

// ---------------------------------------------------------------------------
// document generator — structure-aware, nesting-heavy by design
// ---------------------------------------------------------------------------

type docGen struct {
	r *mrand.Rand
}

var genWords = []string{
	"alpha", "beta", "gamma", "delta", "foo", "bar", "baz", "qux",
	"the", "quick", "brown", "fox", "jumps", "over", "lazy", "dog",
	"lorem", "ipsum", "dolor", "sit", "amet", "value", "item", "note",
}

var genFenceInfo = []string{"", "", "", "go", "python", "json", "text", "sh", "js", "rust", "toml"}
var genBullets = []string{"-", "*", "+"}
var genURLs = []string{"https://example.com", "https://example.com/a?b=c", "http://foo.bar/baz"}

func (g *docGen) pick(sl []string) string { return sl[g.r.IntN(len(sl))] }
func (g *docGen) chance(p float64) bool   { return g.r.Float64() < p }
func (g *docGen) intn(n int) int          { return g.r.IntN(n) }
func (g *docGen) words(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = g.pick(genWords)
	}
	return strings.Join(parts, " ")
}

// inline text with random emphasis / code spans / links / images
func (g *docGen) inline() string {
	n := 1 + g.intn(4)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		switch g.intn(12) {
		case 0:
			fmt.Fprintf(&sb, "*%s*", g.words(1+g.intn(2)))
		case 1:
			fmt.Fprintf(&sb, "**%s**", g.words(1+g.intn(2)))
		case 2:
			fmt.Fprintf(&sb, "***%s***", g.words(1))
		case 3:
			fmt.Fprintf(&sb, "`%s`", g.words(1))
		case 4:
			fmt.Fprintf(&sb, "`%s %s`", g.words(1), g.words(1))
		case 5:
			fmt.Fprintf(&sb, "[%s](%s)", g.words(1), g.pick(genURLs))
		case 6:
			fmt.Fprintf(&sb, "![img](%s)", g.pick(genURLs))
		case 7:
			fmt.Fprintf(&sb, "<%s>", g.pick(genURLs))
		case 8:
			sb.WriteString("<br>")
		case 9:
			fmt.Fprintf(&sb, "%s_%s", g.words(1), g.words(1)) // underscore stress
		default:
			sb.WriteString(g.words(1 + g.intn(2)))
		}
		if i < n-1 {
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

// block returns the LINES of a single block, unprefixed.
func (g *docGen) block(depth int) []string {
	kinds := []string{"para", "para", "para", "heading", "setext", "fence", "fence",
		"indentedCode", "list", "list", "list", "quote", "quote", "table", "hr", "html", "taskList"}
	// containers get more nesting weight when already deep enough to matter
	if depth < 3 {
		kinds = append(kinds, "list", "quote")
	}
	switch g.pick(kinds) {
	case "para":
		return g.para()
	case "heading":
		return []string{strings.Repeat("#", 1+g.intn(6)) + " " + g.words(1+g.intn(4))}
	case "setext":
		if g.chance(0.5) {
			return []string{g.words(2 + g.intn(3)), strings.Repeat("=", 3+g.intn(4))}
		}
		return []string{g.words(2 + g.intn(3)), strings.Repeat("-", 3+g.intn(4))}
	case "fence":
		return g.fence()
	case "indentedCode":
		lines := []string{"    " + g.words(1+g.intn(3))}
		for i := 0; i < g.intn(3); i++ {
			if g.chance(0.2) {
				lines = append(lines, "    ")
			} else {
				lines = append(lines, "    "+g.words(1+g.intn(3)))
			}
		}
		return lines
	case "list":
		return g.list(depth)
	case "quote":
		return g.quote(depth)
	case "table":
		return g.table()
	case "hr":
		return []string{g.pick([]string{"---", "***", "___", "- - -", "********"})}
	case "html":
		switch g.intn(3) {
		case 0:
			return []string{"<div>", g.words(2), "</div>"}
		case 1:
			return []string{"<p>" + g.words(2) + "</p>"}
		default:
			return []string{"<br>", g.words(2)}
		}
	case "taskList":
		n := 1 + g.intn(3)
		var lines []string
		for i := 0; i < n; i++ {
			mark := "[ ]"
			if g.chance(0.5) {
				mark = "[x]"
			}
			lines = append(lines, g.pick(genBullets)+" "+mark+" "+g.words(1+g.intn(2)))
		}
		return lines
	}
	return g.para()
}

func (g *docGen) para() []string {
	n := 1 + g.intn(3)
	var lines []string
	for i := 0; i < n; i++ {
		txt := g.inline()
		if i < n-1 && g.chance(0.2) {
			txt += "  " // hard break
		}
		lines = append(lines, txt)
	}
	return lines
}

// fence generates fenced code with the shapes that stress the state machine:
// info strings, ~~~ fences, longer closing fences, indented fences, blank
// lines inside, unclosed fences.
func (g *docGen) fence() []string {
	tick := g.pick([]string{"```", "```", "```", "~~~"})
	info := g.pick(genFenceInfo)
	indent := g.pick([]string{"", "", "", "  ", "   "})
	open := indent + tick + info
	if g.chance(0.15) {
		open += " extra=args"
	}
	var lines []string
	lines = append(lines, open)
	for i := 0; i < 1+g.intn(4); i++ {
		switch {
		case g.chance(0.15): // blank line inside code
			lines = append(lines, "")
		case g.chance(0.15): // indented code content
			lines = append(lines, "  "+g.words(1+g.intn(2)))
		default:
			lines = append(lines, g.words(1+g.intn(3)))
		}
	}
	if g.chance(0.85) {
		closeTick := tick
		if g.chance(0.2) && strings.HasPrefix(tick, "`") {
			closeTick = "````" // longer close is legal
		}
		lines = append(lines, indent+closeTick)
	} // else: unclosed fence
	return lines
}

func (g *docGen) list(depth int) []string {
	ordered := g.chance(0.4)
	loose := g.chance(0.3)
	n := 1 + g.intn(4)
	var lines []string
	for i := 0; i < n; i++ {
		var marker string
		if ordered {
			marker = fmt.Sprintf("%d.", i+1)
			if g.chance(0.2) {
				marker = fmt.Sprintf("%d)", i+1)
			}
		} else {
			marker = g.pick(genBullets)
		}
		lines = append(lines, marker+" "+g.inline())
		markerLineIdx := len(lines) - 1
		// continuation paragraph (loose item)
		if g.chance(0.25) {
			lines = append(lines, strings.Repeat(" ", len(marker)+1)+g.words(2+g.intn(2)))
		}
		// nested block inside the item (2-4 spaces — 4 flips to code: bug food)
		if depth < 2 && g.chance(0.4) {
			ind := strings.Repeat(" ", 2+g.intn(3))
			var child []string
			switch g.intn(3) {
			case 0:
				child = g.list(depth + 1)
			case 1:
				child = g.fence()
				if g.chance(0.3) && len(child) > 1 {
					// marker-line fence: opener ON the marker line
					// ("- ```py") — the shape whose closer was once
					// misread as a new top-level opener. Splice at the
					// recorded marker line so a preceding loose-item
					// continuation doesn't redirect the rewrite.
					lines[markerLineIdx] = marker + " " + child[0]
					child = child[1:]
				}
			default:
				child = g.para()
			}
			for _, cl := range child {
				lines = append(lines, ind+cl)
			}
		}
		if loose && i < n-1 {
			lines = append(lines, "")
		}
	}
	return lines
}

func (g *docGen) quote(depth int) []string {
	var inner []string
	if depth < 2 && g.chance(0.5) {
		switch g.intn(3) {
		case 0:
			inner = g.quote(depth + 1)
		case 1:
			inner = g.list(depth + 1)
		default:
			inner = g.fence()
		}
	} else {
		inner = g.para()
	}
	var lines []string
	for _, l := range inner {
		if l == "" {
			lines = append(lines, ">")
		} else {
			lines = append(lines, "> "+l)
		}
	}
	if g.chance(0.2) {
		lines = append(lines, g.words(2)) // lazy continuation
	}
	return lines
}

func (g *docGen) table() []string {
	cols := 2 + g.intn(3)
	rows := 1 + g.intn(3)
	var lines []string
	cellRow := func() string {
		cells := make([]string, cols)
		for i := range cells {
			cells[i] = g.words(1)
		}
		return "| " + strings.Join(cells, " | ") + " |"
	}
	lines = append(lines, cellRow())
	aligns := make([]string, cols)
	for i := range aligns {
		aligns[i] = g.pick([]string{"---", ":--", "--:", ":-:"})
	}
	lines = append(lines, "| "+strings.Join(aligns, " | ")+" |")
	for i := 0; i < rows; i++ {
		lines = append(lines, cellRow())
	}
	return lines
}

// doc composes blocks with random (sometimes zero!) blank-line separators —
// adjacency is bug food (fences/lists interrupting paragraphs, setext...).
func (g *docGen) doc() string {
	n := 2 + g.intn(5)
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, strings.Join(g.block(0), "\n"))
	}
	var sb strings.Builder
	for i, p := range parts {
		if i > 0 {
			sb.WriteString(g.pick([]string{"\n\n", "\n\n", "\n\n", "\n", "\n\n\n"}))
		}
		sb.WriteString(p)
	}
	if g.chance(0.1) {
		sb.WriteString("\n")
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// chunking strategies + runner
// ---------------------------------------------------------------------------

type chunkStrategy struct {
	name   string
	deltas []string
}

func chunkStrategies(r *mrand.Rand, doc string) []chunkStrategy {
	var out []chunkStrategy
	out = append(out, chunkStrategy{name: "oneShot", deltas: []string{doc}})
	for _, n := range []int{1, 3, 5, 13} {
		var ds []string
		for i := 0; i < len(doc); i += n {
			e := min(i+n, len(doc))
			ds = append(ds, doc[i:e])
		}
		if len(ds) == 0 {
			ds = []string{doc}
		}
		out = append(out, chunkStrategy{name: fmt.Sprintf("bytes(%d)", n), deltas: ds})
	}
	// line-ish chunks (realistic SSE)
	{
		var ds []string
		rest := doc
		for len(rest) > 0 {
			// cut shortly after a random newline
			limit := 1 + r.IntN(min(len(rest), 80))
			cut := strings.IndexByte(rest[:limit], '\n')
			if cut >= 0 && cut+1 < len(rest) {
				cut = cut + 1
			} else {
				cut = limit
			}
			ds = append(ds, rest[:cut])
			rest = rest[cut:]
		}
		out = append(out, chunkStrategy{name: "lineish", deltas: ds})
	}
	// word-ish chunks (realistic token deltas)
	{
		var ds []string
		rest := doc
		for len(rest) > 0 {
			cut := strings.IndexByte(rest, ' ')
			if cut < 0 || cut > 24 {
				cut = min(len(rest), 1+r.IntN(24))
			} else {
				cut++ // include the space
			}
			ds = append(ds, rest[:cut])
			rest = rest[cut:]
		}
		out = append(out, chunkStrategy{name: "wordish", deltas: ds})
	}
	return out
}

// streamCanonical runs the StreamingRenderer over the deltas and returns the
// IRC-visible canonical form: entries joined with "\n", empty entries dropped
// (sendIRC skips them), outer newlines trimmed.
func streamCanonical(deltas []string) string {
	s := NewStreamingRenderer()
	var out []string
	for _, d := range deltas {
		out = append(out, s.Process(d)...)
	}
	out = append(out, s.Process("")...)
	return normalizeIRC(strings.Join(out, "\n"))
}

func normalizeIRC(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Trim(strings.Join(kept, "\n"), "\n")
}

// referenceStream is the whole-document streaming-mode render — what the
// incremental path is supposed to converge to.
func referenceStream(doc string) string {
	return normalizeIRC(MarkdownToIRCStream(doc))
}

// ---------------------------------------------------------------------------
// oracles
// ---------------------------------------------------------------------------

func p1Violated(doc string, r *mrand.Rand) (bool, string) {
	strats := chunkStrategies(r, doc)
	first := streamCanonical(strats[0].deltas)
	for _, s := range strats[1:] {
		if got := streamCanonical(s.deltas); got != first {
			return true, fmt.Sprintf("strategy %q: %q != %q (%q)", s.name, preview(got), preview(first), preview(doc))
		}
	}
	return false, ""
}

func p2Violated(doc string) (bool, string) {
	got := streamCanonical([]string{doc})
	want := referenceStream(doc)
	if got != want {
		return true, fmt.Sprintf("got:  %q\nwant: %q\ndoc:  %q", preview(got), preview(want), preview(doc))
	}
	return false, ""
}

func preview(s string) string {
	const max = 220
	r := []rune(s)
	if len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// shrinker — greedy line deletion + line truncation to a fixpoint
// ---------------------------------------------------------------------------

func shrinkDoc(doc string, violated func(string) bool) string {
	cur := doc
	for round := 0; round < 200; round++ {
		lines := strings.Split(cur, "\n")
		improved := false
		// drop each line
		for i := range lines {
			cand := append(append([]string{}, lines[:i]...), lines[i+1:]...)
			joined := strings.Join(cand, "\n")
			if joined != cur && violated(joined) {
				cur = joined
				improved = true
				break
			}
		}
		if improved {
			continue
		}
		// truncate long lines
		for i, ln := range lines {
			r := []rune(ln)
			if len(r) <= 16 {
				continue
			}
			cand := append(append([]string{}, lines[:i]...), string(r[:12])+"x")
			cand = append(cand, lines[i+1:]...)
			joined := strings.Join(cand, "\n")
			if joined != cur && violated(joined) {
				cur = joined
				improved = true
				break
			}
		}
		if improved {
			continue
		}
		// collapse runs of blank lines to one
		var collapsed []string
		for i, ln := range lines {
			if ln == "" && i > 0 && collapsed[len(collapsed)-1] == "" {
				continue
			}
			collapsed = append(collapsed, ln)
		}
		if joined := strings.Join(collapsed, "\n"); joined != cur && violated(joined) {
			cur = joined
			continue
		}
		break
	}
	return cur
}

// ---------------------------------------------------------------------------
// bug classification tags for the survey summary
// ---------------------------------------------------------------------------

var tagPatterns = []struct {
	tag string
	re  *regexp.Regexp
}{
	{"fence-info", regexp.MustCompile(`(?m)^ {0,3}(?:` + "`" + `{3,}|~{3,})\s*\w`)},
	{"fence-tilde", regexp.MustCompile(`~{3,}`)},
	{"fence-longer", regexp.MustCompile("`{4,}")},
	{"fence-indented", regexp.MustCompile(`(?m)^ {1,3}(?:` + "`" + `{3,}|~{3,})`)},
	{"blank-in-fence", nil}, // handled by blankInsideFence
	{"fence-in-container", regexp.MustCompile(`(?m)^(?:>|\s{2,}\S|[-*+] |\d+[.)] ).*(?:` + "`" + `{3,}|~{3,})`)},
	{"indented-code", regexp.MustCompile(`(?m)^ {4,}\S`)},
	{"list", regexp.MustCompile(`(?m)^\s*(?:[-*+] |\d+[.)] )`)},
	{"quote", regexp.MustCompile(`(?m)^>`)},
	{"table", regexp.MustCompile(`(?:\|--|---\||:-)`)},
	{"setext", regexp.MustCompile(`(?m)^[=-]+$`)},
	{"html", regexp.MustCompile(`<`)},
}

func bugTags(doc string) string {
	var tags []string
	for _, tp := range tagPatterns {
		if tp.re != nil && tp.re.MatchString(doc) {
			tags = append(tags, tp.tag)
		}
	}
	if strings.Contains(doc, "```") && blankInsideFence(doc) {
		tags = append(tags, "blank-in-fence")
	}
	if len(tags) == 0 {
		tags = append(tags, "plain")
	}
	return strings.Join(tags, ",")
}

// blankInsideFence: a truly blank line between fence lines
func blankInsideFence(doc string) bool {
	lines := strings.Split(doc, "\n")
	inFence := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "```" || t == "````" {
			inFence = !inFence
			continue
		}
		if inFence && l == "" {
			return true
		}
	}
	return false
}

// canonicalShape reduces word content to a single token so two documents with
// identical STRUCTURE but different words dedupe to the same survey signature.
func canonicalShape(doc string) string {
	return shapeWords.ReplaceAllString(doc, "w")
}

var shapeWords = regexp.MustCompile(`[a-zA-Z0-9]+`)

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

// TestStreamingChunkInvariance: P1 must hold for ANY input regardless of
// chunking. Always-on. A fixed-seed pass gives every run the same corpus
// (deterministic CI); the random pass keeps exploring.
func TestStreamingChunkInvariance(t *testing.T) {
	r2 := mrand.New(mrand.NewPCG(1, 1))
	g2 := &docGen{r: r2}
	for i := 0; i < 30; i++ {
		if bad, detail := p1Violated(g2.doc(), r2); bad {
			t.Errorf("chunk invariance violated (fixed-seed corpus, iter=%d): %s", i, detail)
			return
		}
	}

	r, seed := fuzzRand()
	iters := 150
	if n, ok := envSeed("DAVE_STREAM_FUZZ_ITERS"); ok {
		if v := int(n); v > 0 {
			iters = v
		}
	}
	g := &docGen{r: r}
	for i := 0; i < iters; i++ {
		doc := g.doc()
		if bad, detail := p1Violated(doc, r); bad {
			t.Errorf("chunk invariance violated (seed=%d iter=%d): %s", seed, i, detail)
			return
		}
	}
}

// TestStreamingSurvey: env-gated bug hunt. Finds P1/P2 violations, shrinks
// them to minimal reproducers, records distinct ones as fixtures under
// testdata/streaming/known_bugs/.
func TestStreamingSurvey(t *testing.T) {
	if os.Getenv("DAVE_STREAM_SURVEY") == "" {
		t.Skip("set DAVE_STREAM_SURVEY=1 to run the streaming bug survey")
	}
	iters := 4000
	if v := os.Getenv("DAVE_STREAM_SURVEY_ITERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			iters = n
		}
	}
	maxBugs := 40
	if v := os.Getenv("DAVE_STREAM_SURVEY_MAXBUGS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxBugs = n
		}
	}
	r, seed := fuzzRand()
	g := &docGen{r: r}

	type bug struct {
		doc, detail, prop string
	}
	found := map[string]bug{}
	var order []string

	record := func(doc, prop, detail string) {
		mini := shrinkDoc(doc, func(d string) bool {
			switch prop {
			case "P1":
				bad, _ := p1Violated(d, r)
				return bad
			default:
				bad, _ := p2Violated(d)
				return bad
			}
		})
		hh := fnv.New64a()
		hh.Write([]byte(prop + "\x00" + canonicalShape(mini)))
		h := fmt.Sprintf("%x", hh.Sum64())
		if _, dup := found[h]; dup {
			return
		}
		if len(found) >= maxBugs {
			return // corpus cap reached; still counted in the totals
		}
		// re-verify the shrunken form actually violates
		ok := false
		if prop == "P1" {
			ok, _ = p1Violated(mini, r)
		} else {
			ok, _ = p2Violated(mini)
		}
		if !ok {
			return
		}
		var d string
		if prop == "P2" {
			_, d = p2Violated(mini)
		} else {
			_, d = p1Violated(mini, r)
		}
		found[h] = bug{doc: mini, detail: d, prop: prop}
		order = append(order, h)
	}

	p1Fails, p2Fails := 0, 0
	for i := 0; i < iters; i++ {
		doc := g.doc()
		if bad, detail := p2Violated(doc); bad {
			p2Fails++
			record(doc, "P2", detail)
		} else if bad, detail := p1Violated(doc, r); bad {
			p1Fails++
			record(doc, "P1", detail)
		}
	}

	t.Logf("survey: %d docs, %d P2 (parity) failures, %d P1 (invariance) failures, %d distinct recorded (seed=%d)",
		iters, p2Fails, p1Fails, len(found), seed)

	dir := filepath.Join("testdata", "streaming", "known_bugs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	require.Less(t, len(found), 500, "absurd bug count; generator or oracle is broken")
	for i, h := range order {
		b := found[h]
		name := fmt.Sprintf("%03d-%s", i+1, h[:8])
		path := filepath.Join(dir, name+".md")
		if err := os.WriteFile(path, []byte(b.doc), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		info := fmt.Sprintf("property: %s\ntags: %s\nseed: %d\n\n%s\n", b.prop, bugTags(b.doc), seed, b.detail)
		if err := os.WriteFile(filepath.Join(dir, name+".info.txt"), []byte(info), 0o644); err != nil {
			t.Fatalf("write fixture info: %v", err)
		}
		t.Logf("recorded %s [%s] %q", name, bugTags(b.doc), preview(b.doc))
	}
	if len(found) > 0 {
		t.Errorf("survey found %d distinct streaming markdown bugs; fixtures recorded in %s", len(found), dir)
	}
}

// TestStreamingKnownBugs: pins every recorded fixture as a known failure.
// Each fixture must still diverge from the streaming-mode reference. When a
// renderer fix lands and fixtures start passing, run the test once with
// DAVE_STREAM_PROMOTE=1: passing fixtures are converted into golden
// regression pairs (testdata/streaming/bug-*.md + .irc, picked up by
// TestStreamingFromTestData) and removed from the known_bugs pool. Any
// fixture still failing stays pinned until it renders correctly.
func TestStreamingKnownBugs(t *testing.T) {
	dir := filepath.Join("testdata", "streaming", "known_bugs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("no known_bugs fixture dir: %v", err)
	}
	promote := os.Getenv("DAVE_STREAM_PROMOTE") != ""
	checked, promoted := 0, 0
	var brokeThrough []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		doc, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read fixture: %v", err)
		}
		checked++
		bad, detail := p2Violated(string(doc))
		if !bad {
			if b2, d2 := p1Violated(string(doc), mrand.New(mrand.NewPCG(1, 1))); b2 {
				t.Errorf("fixture %s: P1 now violated but P2 passes — re-survey (detail: %s)", e.Name(), d2)
				continue
			}
			if !promote {
				t.Errorf("fixture %s now RENDERS CORRECTLY — the bug is fixed: re-run with DAVE_STREAM_PROMOTE=1 to promote it to a golden regression fixture", e.Name())
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".md")
			goldenMd := filepath.Join("testdata", "streaming", "bug-"+name+".md")
			goldenIrc := filepath.Join("testdata", "streaming", "bug-"+name+".irc")
			if err := os.WriteFile(goldenMd, doc, 0o644); err != nil {
				t.Fatalf("promote md: %v", err)
			}
			if err := os.WriteFile(goldenIrc, []byte(humanize(streamingRender(string(doc)))), 0o644); err != nil {
				t.Fatalf("promote irc: %v", err)
			}
			// keep the provenance (property/tags/seed) alongside the golden
			if info, err := os.ReadFile(filepath.Join(dir, name+".info.txt")); err == nil {
				_ = os.WriteFile(filepath.Join("testdata", "streaming", "bug-"+name+".info.txt"), info, 0o644)
			}
			_ = os.Remove(filepath.Join(dir, e.Name()))
			_ = os.Remove(filepath.Join(dir, name+".info.txt"))
			promoted++
			t.Logf("promoted %s to golden regression fixture (bug-%s.md/.irc)", e.Name(), name)
			continue
		}
		brokeThrough = append(brokeThrough, e.Name())
		t.Logf("known bug pinned: %s [%s]", e.Name(), bugTags(string(doc)))
		_ = detail
	}
	if checked == 0 {
		t.Skip("no known_bugs fixtures recorded yet")
	}
	if promoted > 0 {
		remaining, _ := os.ReadDir(dir)
		onlyNonMd := true
		for _, f := range remaining {
			if strings.HasSuffix(f.Name(), ".md") {
				onlyNonMd = false
			}
		}
		if onlyNonMd {
			_ = os.RemoveAll(dir)
		}
		t.Logf("promoted %d/%d fixtures to golden regression pairs", promoted, checked)
	}
}
