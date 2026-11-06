package main

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// maxHarvestedImages bounds the images field: filmstrip-heavy pages carry
// dozens of thumbnail URLs and the field is a pointer for the model ("these
// are the images on this page"), not an exhaustive inventory. Document order
// is preserved, so content images that appear before filmstrips always make
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
// a directly viewable image URL always lands in the field.
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
// image-centric pages) and returns candidate image URLs in document order:
// resolved against pageURL (or the first <base href>, which per spec precedes
// all content), deduped, data:/non-http schemes skipped, capped at
// maxHarvestedImages. Sources: <img src/data-src/srcset/data-srcset>,
// <picture> <source srcset>, <a href> with an image extension, og:image /
// twitter:image meta, and <link rel=preload as=image> / <link rel=image_src>.
func harvestImageURLs(pageURL *url.URL, body []byte) []string {
	seen := make(map[string]bool)
	var out []string
	capped := false
	base := pageURL

	add := func(raw string) {
		if capped {
			return
		}
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
		out = append(out, s)
		if len(out) >= maxHarvestedImages {
			capped = true
		}
	}

	z := html.NewTokenizer(bytes.NewReader(body))
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
			// data-* first: lazy-load convention puts the real image in
			// data-src and a 1x placeholder in src, and document order
			// decides what survives the cap.
			add(attrs["data-src"])
			for _, u := range srcsetURLs(attrs["data-srcset"]) {
				add(u)
			}
			add(attrs["src"])
			for _, u := range srcsetURLs(attrs["srcset"]) {
				add(u)
			}
		case "source":
			// <picture> alternatives. HTML requires <picture> to contain an
			// <img> fallback, so this only adds sizes/encodings the fallback
			// would not carry (e.g. avif/webp variants).
			attrs := tokenAttrs(z)
			for _, u := range srcsetURLs(attrs["srcset"]) {
				add(u)
			}
			for _, u := range srcsetURLs(attrs["data-srcset"]) {
				add(u)
			}
		case "a":
			if href := tokenAttrs(z)["href"]; looksLikeImageURL(href) {
				add(href)
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
				add(attrs["content"])
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
					add(u)
				}
				add(attrs["imagesrc"])
			}
			if relTokens["image_src"] {
				add(attrs["href"])
			}
		}
		if capped {
			break
		}
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
