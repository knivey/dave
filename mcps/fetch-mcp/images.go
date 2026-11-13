package main

import (
	"bytes"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// maxHarvestedImages bounds the images field: filmstrip-heavy pages carry
// dozens of thumbnail URLs and the field is a pointer for the model ("these
// are the images on this page"), not an exhaustive inventory. Emission is
// CLASS-RANKED (see harvestImageURLs below); within a class, document order
// is preserved, so content images that appear before filmstrips still make
// the cut.
const maxHarvestedImages = 20

// imageMetaNames are the exact meta names/properties whose content is an
// image URL. Deliberately an exact set rather than an "og:image*" prefix
// match: og:image:width / og:image:height / og:image:alt carry numbers and
// text, not URLs.
var imageMetaNames = map[string]bool{
	"og:image":            true,
	"og:image:url":        true,
	"og:image:secure_url": true,
	"twitter:image":       true,
	"twitter:image:src":   true,
}

// anchorImageExtensions gates which <a href> values are harvested: only
// hrefs that look like image files. Anchors that merely WRAP an <img> are not
// enough — on gallery sites the filmstrip pattern is
// <a href="/thumb-page"><img src="/thumb.webp"></a> where the href is a PAGE,
// and harvesting extension-less wrapping hrefs unconditionally would flood
// the images field with page URLs and evict real images from the cap. The
// cost of the gate: extension-less originals behind lightbox anchors (e.g.
// "/65D/orig") are missed — but the wrapped <img src> is still harvested, so
// a directly viewable image URL always lands in the field. dave's
// image_detect.go carries a twin list (`imageFileExtensions`) behind an
// identical gate for view_image's page resolution — keep both in sync.
var anchorImageExtensions = []string{
	".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".bmp", ".svg",
}

// dataURIInMarkdown matches markdown image/link syntax whose destination is a
// data: URI — the shape html-to-markdown produces for lazy-load placeholder
// <img src="data:image/webp;base64,..."> tags. These embed tens of KB of
// base64 into the markdown and are useless to the model.
var dataURIInMarkdown = regexp.MustCompile(`(?i)(!?)\[([^\]]*)]\(data:[^)]*\)`)

// harvestImageURLs walks the RAW page HTML (not the readability output —
// readability's text-density scoring drops image-only containers entirely,
// which is how the direct image URL vanished from fetch results on
// image-centric pages) and returns candidate image URLs resolved against
// pageURL (or the first <base href>, which per spec precedes all
// content), deduped, data:/non-http schemes skipped, capped at
// maxHarvestedImages. Sources: <img src/data-src/srcset/data-srcset>,
// <picture> <source srcset>, <a href> with an image extension, og:image /
// twitter:image meta, and <link rel=preload as=image> / <link rel=image_src>.
//
// Emission is IDENTITY-ranked (sessions 3179 + the og:image correction):
// the field's FIRST entry is what the model treats as "the image behind
// the page", and an extension'd anchor signals A file link, not THE
// page's file link (zkpq filmstrips wrap every NEIGHBOR thumb in one).
// Order: (1) a SINGLE unique file link (anchor/preload/image_src — the
// "open file" details-page pattern, counted after dedupe so imgsite's
// twice-appearing orig anchor still qualifies); (2) displayed
// <img>/<source> candidates by declared size desc (width/height attrs,
// srcset width descriptors), document order for ties/unknowns, data-*
// before plain attributes; (3) a BATCH of distinct file links — a gallery
// filmstrip, neighbors not "the" image — after the displayed images; (4)
// og:image/twitter:image metas LAST (preview-sized by web convention).
func harvestImageURLs(pageURL *url.URL, body []byte) []string {
	seen := make(map[string]bool)
	var files []string
	var displayedScored []scoredURL
	var metas []string
	base := pageURL

	addTo := func(bucket *[]string, raw string) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(strings.ToLower(raw), "data:") {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		resolved := base.ResolveReference(u)
		if resolved.Scheme != "http" && resolved.Scheme != "https" {
			return
		}
		s := resolved.String()
		if seen[s] {
			return
		}
		seen[s] = true
		*bucket = append(*bucket, s)
	}
	addScored := func(raw string, declared int64) {
		raw = strings.TrimSpace(raw)
		if raw == "" || strings.HasPrefix(strings.ToLower(raw), "data:") {
			return
		}
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		resolved := base.ResolveReference(u)
		if resolved.Scheme != "http" && resolved.Scheme != "https" {
			return
		}
		s := resolved.String()
		if seen[s] {
			return
		}
		seen[s] = true
		displayedScored = append(displayedScored, scoredURL{url: s, declared: declared})
	}

	z := html.NewTokenizer(bytes.NewReader(body))
	// Deliberate full-tokenize: the old cap break froze document order,
	// which is incompatible with ranking late-in-document candidates.
	// The walk is bounded by the fetch's maxBodySize; the cap truncates
	// AFTER the ranked concat.
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		nameB, _ := z.TagName()
		switch string(nameB) {
		case "img":
			attrs := tokenAttrs(z)
			// srcset descriptors first: when a URL appears both as src
			// and inside srcset, the descriptor-carrying occurrence must
			// win the dedupe or the candidate loses its declared size.
			// data-* before plain attributes otherwise (lazy-load
			// convention: the real image lives in data-src).
			score := imgDeclaredScore(attrs)
			for _, pair := range srcsetScored(attrs["data-srcset"]) {
				addScored(pair.url, pair.declared)
			}
			for _, pair := range srcsetScored(attrs["srcset"]) {
				addScored(pair.url, pair.declared)
			}
			addScored(attrs["data-src"], score)
			addScored(attrs["src"], score)
		case "source":
			// <picture> alternatives. HTML requires <picture> to contain an
			// <img> fallback, so this only adds sizes/encodings the fallback
			// would not carry (e.g. avif/webp variants).
			attrs := tokenAttrs(z)
			for _, pair := range srcsetScored(attrs["srcset"]) {
				addScored(pair.url, pair.declared)
			}
			for _, pair := range srcsetScored(attrs["data-srcset"]) {
				addScored(pair.url, pair.declared)
			}
		case "a":
			if href := tokenAttrs(z)["href"]; looksLikeImageURL(href) {
				addTo(&files, href)
			}
		case "base":
			// Pages with <base href> resolve relative URLs against it, not
			// the page URL. Per spec <base> precedes content, so remembering
			// the first one switches the resolution base for everything that
			// follows. Relative href values are invalid and ignored.
			if href := tokenAttrs(z)["href"]; href != "" {
				if u, err := url.Parse(strings.TrimSpace(href)); err == nil &&
					(u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
					base = u
				}
			}
		case "meta":
			attrs := tokenAttrs(z)
			key := attrs["property"]
			if key == "" {
				key = attrs["name"]
			}
			if imageMetaNames[key] {
				addTo(&metas, attrs["content"])
			}
		case "link":
			attrs := tokenAttrs(z)
			relTokens := map[string]bool{}
			for _, tok := range strings.Fields(strings.ToLower(attrs["rel"])) {
				relTokens[tok] = true
			}
			// "as" is ASCII case-insensitive per spec, like rel.
			if relTokens["preload"] && strings.EqualFold(attrs["as"], "image") {
				for _, u := range srcsetURLs(attrs["imagesrcset"]) {
					addTo(&files, u)
				}
				addTo(&files, attrs["imagesrc"])
			}
			if relTokens["image_src"] {
				addTo(&files, attrs["href"])
			}
		}
	}

	// Displayed images: declared size desc, document order stable.
	sort.SliceStable(displayedScored, func(i, j int) bool {
		return displayedScored[i].declared > displayedScored[j].declared
	})
	var displayed []string
	for _, c := range displayedScored {
		displayed = append(displayed, c.url)
	}

	var out []string
	if len(files) == 1 {
		// The open-file pattern: exactly one file link on the page.
		out = append(out, files[0])
		out = append(out, displayed...)
	} else {
		// No file links, or a gallery filmstrip: displayed images first.
		out = append(out, displayed...)
	}
	if len(files) > 1 {
		out = append(out, files...)
	}
	out = append(out, metas...)
	if len(out) > maxHarvestedImages {
		out = out[:maxHarvestedImages]
	}
	return out
}

// tokenAttrs drains the current tag's attributes into a map. Tokenizer keys
// are already lowercased.
func tokenAttrs(z *html.Tokenizer) map[string]string {
	attrs := make(map[string]string)
	for {
		key, val, more := z.TagAttr()
		attrs[string(key)] = string(val)
		if !more {
			return attrs
		}
	}
}

// scoredURL is a harvested image URL with the size the markup DECLARED
// for it (pixel area from width/height attrs, else the max srcset width
// descriptor; 0 = undeclared). fetch-mcp ranks by markup only — the
// image-header probe is dave's view_image selection mechanism.
type scoredURL struct {
	url      string
	declared int64
}

// imgDeclaredScore derives a pixel-area score from an <img> tag's
// width/height attributes (both required for area; a lone dim scores its
// value). Bare integers only — units and percentages do not count.
func imgDeclaredScore(attrs map[string]string) int64 {
	w, wok := atoiStrict(attrs["width"])
	h, hok := atoiStrict(attrs["height"])
	switch {
	case wok && hok:
		return w * h
	case wok:
		return w
	case hok:
		return h
	}
	return 0
}

func atoiStrict(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// srcsetScored splits a srcset into (url, descriptor-score) pairs; the
// pixel-width descriptor doubles as the declared size. The data:-URI
// continuation guard mirrors srcsetURLs below.
func srcsetScored(s string) []scoredURL {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []scoredURL
	prevWasData := false
	for _, candidate := range strings.Split(s, ",") {
		raw := strings.TrimSpace(candidate)
		if raw == "" {
			continue
		}
		fields := strings.Fields(raw)
		if prevWasData && (!freshSrcsetURL(raw) || !looksLikeImageURL(fields[0])) {
			continue
		}
		prevWasData = strings.HasPrefix(strings.ToLower(raw), "data:")
		var score int64
		if len(fields) > 1 && strings.HasSuffix(fields[1], "w") {
			if n, ok := atoiStrict(strings.TrimSuffix(fields[1], "w")); ok {
				score = n
			}
		}
		out = append(out, scoredURL{url: fields[0], declared: score})
	}
	return out
}

// srcsetURLs splits a srcset attribute into its URL candidates, dropping the
// width/pixel-density descriptors. Splitting on commas is what the HTML spec
// does too (commas terminate srcset URLs), which means a data: URI inside a
// srcset is broken input: its base64 payload lands in the NEXT candidate.
// Candidates after a data: candidate must clear two gates to count as fresh
// URLs — start like one (/, //, or scheme-prefixed) AND carry an image
// extension — because base64 payloads often start with "/" themselves (every
// base64 JPEG begins "/9j/", the encoding of the FF D8 FF magic bytes), and
// harvesting such fragments as resolved page URLs would hand the model
// guaranteed-404 garbage.
func srcsetURLs(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var urls []string
	prevWasData := false
	for _, candidate := range strings.Split(s, ",") {
		raw := strings.TrimSpace(candidate)
		if raw == "" {
			continue
		}
		fields := strings.Fields(raw)
		if prevWasData && (!freshSrcsetURL(raw) || !looksLikeImageURL(fields[0])) {
			// Continuation chunk of the broken data: URI — stay sticky until
			// a candidate clears both gates.
			continue
		}
		prevWasData = strings.HasPrefix(strings.ToLower(raw), "data:")
		urls = append(urls, fields[0])
	}
	return urls
}

// freshSrcsetURL reports whether a srcset candidate starts like a standalone
// URL rather than a data-URI continuation.
func freshSrcsetURL(u string) bool {
	return strings.HasPrefix(u, "/") ||
		strings.Contains(u, "://") ||
		strings.HasPrefix(u, "//")
}

func looksLikeImageURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	path := strings.ToLower(u.Path)
	for _, ext := range anchorImageExtensions {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// elideDataURIs replaces markdown image/link destinations that are data:
// URIs with a short stub so lazy-load placeholder images cannot inline tens
// of KB of base64 into the converted markdown.
func elideDataURIs(markdown string) string {
	return dataURIInMarkdown.ReplaceAllString(markdown, `${1}[${2}](data URI elided)`)
}
