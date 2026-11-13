package main

import (
	"net/url"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

func TestHarvestImageURLs(t *testing.T) {
	base := mustURL(t, "https://img.example.com/AAA1/_DSC1234")

	tests := []struct {
		name string
		html string
		want []string
	}{
		{
			name: "img src relative resolved absolute",
			html: `<html><body><img src="/photos/1.jpg"></body></html>`,
			want: []string{"https://img.example.com/photos/1.jpg"},
		},
		{
			name: "img src already absolute kept",
			html: `<img src="https://cdn.other.net/pic.webp">`,
			want: []string{"https://cdn.other.net/pic.webp"},
		},
		{
			name: "protocol relative resolved",
			html: `<img src="//static.example.com/i.png">`,
			want: []string{"https://static.example.com/i.png"},
		},
		{
			name: "srcset entries harvested with descriptors stripped",
			html: `<img src="/a.jpg" srcset="/a.jpg 2294w, /b.jpg 1200w">`,
			want: []string{"https://img.example.com/a.jpg", "https://img.example.com/b.jpg"},
		},
		{
			name: "data-src lazy attribute harvested",
			html: `<img data-src="/lazy/1.png" src="/placeholder.gif">`,
			want: []string{"https://img.example.com/lazy/1.png", "https://img.example.com/placeholder.gif"},
		},
		{
			// Class-ranked since the Nov 2026 og:image correction: anchors
			// (files) before displayed imgs, document order within a class.
			name: "anchor with image extension harvested with and without wrapped img",
			html: `<a href="/AAA1/orig.jpg"><img src="/AAA1/full/256.jpg"></a><a href="/files/original.jpeg">download</a>`,
			want: []string{
				"https://img.example.com/AAA1/orig.jpg",
				"https://img.example.com/files/original.jpeg",
				"https://img.example.com/AAA1/full/256.jpg",
			},
		},
		{
			// Extension-less wrapping hrefs (gallery filmstrips use
			// <a href="/page"><img src="/thumb.webp"></a>) are NOT harvested:
			// they are indistinguishable from page links, and harvesting them
			// would flood the cap with page URLs. The wrapped img src is the
			// directly viewable image and always lands.
			name: "extension-less wrapping anchor href not harvested",
			html: `<a href="/thumb1"><img src="/thumb1/small.webp"></a>`,
			want: []string{"https://img.example.com/thumb1/small.webp"},
		},
		{
			name: "anchor without image semantics not harvested",
			html: `<a href="/pages/1">text link</a><a href="/pages/2"><span>styled</span></a>`,
			want: nil,
		},
		{
			name: "og:image variants",
			html: `<meta property="og:image" content="https://cdn.example.com/cover1.jpg">` +
				`<meta property="og:image:secure_url" content="https://cdn.example.com/cover2.jpg">` +
				`<meta name="twitter:image" content="https://cdn.example.com/card.jpg">`,
			want: []string{
				"https://cdn.example.com/cover1.jpg",
				"https://cdn.example.com/cover2.jpg",
				"https://cdn.example.com/card.jpg",
			},
		},
		{
			name: "link preload as image",
			html: `<link rel="preload" as="image" imagesrcset="/next/full/256.jpg 2294w" imagesrc="/next/full/128.jpg">` +
				`<link rel="image_src" href="/legacy/cover.png">` +
				`<link rel="preload" as="font" href="/fonts/x.woff2">`,
			want: []string{
				"https://img.example.com/next/full/256.jpg",
				"https://img.example.com/next/full/128.jpg",
				"https://img.example.com/legacy/cover.png",
			},
		},
		{
			name: "data URIs skipped everywhere",
			html: `<img src="data:image/png;base64,AAA">` +
				`<img srcset="data:image/webp;base64,BBB 1x, /real.jpg 2x">` +
				`<a href="data:text/html;base64,CCC"><img src="/wrapped.jpg"></a>`,
			want: []string{"https://img.example.com/real.jpg", "https://img.example.com/wrapped.jpg"},
		},
		{
			// Base64 JPEG payloads start with "/9j/" (the encoding of the
			// FF D8 FF magic bytes) — a leading slash, so the fresh-URL gate
			// alone would leak them as page-relative garbage. Post-data
			// candidates must also carry an image extension.
			name: "jpeg base64 payload fragment after data srcset candidate",
			html: `<img srcset="data:image/jpeg;base64,/9j/4AAQSkZJRgABAQ 1x, /after.jpg 2x">`,
			want: []string{"https://img.example.com/after.jpg"},
		},
		{
			name: "data-srcset harvested",
			html: `<img data-srcset="/lazy/a.webp 1x, /lazy/b.webp 2x">`,
			want: []string{"https://img.example.com/lazy/a.webp", "https://img.example.com/lazy/b.webp"},
		},
		{
			name: "picture source srcset harvested",
			html: `<picture><source srcset="/avif/photo.avif" type="image/avif"><img src="/photo.jpg"></picture>`,
			want: []string{"https://img.example.com/avif/photo.avif", "https://img.example.com/photo.jpg"},
		},
		{
			name: "base href overrides resolution base",
			html: `<base href="https://cdn.other.net/g/"><img src="photo.jpg">`,
			want: []string{"https://cdn.other.net/g/photo.jpg"},
		},
		{
			name: "relative base href ignored",
			html: `<base href="/root/"><img src="photo.jpg">`,
			want: []string{"https://img.example.com/AAA1/photo.jpg"},
		},
		{
			name: "link as attribute case insensitive",
			html: `<link rel="preload" as="Image" imagesrc="/loaded.jpg">`,
			want: []string{"https://img.example.com/loaded.jpg"},
		},
		{
			// The exact-name set is deliberate: og:image:width/height/alt
			// carry numbers and text, not URLs.
			name: "og image dimension and alt meta not harvested",
			html: `<meta property="og:image" content="/cover.jpg">` +
				`<meta property="og:image:width" content="1200">` +
				`<meta property="og:image:height" content="800">` +
				`<meta property="og:image:alt" content="a description">`,
			want: []string{"https://img.example.com/cover.jpg"},
		},
		{
			name: "non http schemes skipped",
			html: `<img src="javascript:alert(1)"><a href="mailto:x@y.z"><img src="/mailwrap.jpg"></a>`,
			want: []string{"https://img.example.com/mailwrap.jpg"},
		},
		{
			name: "empty src skipped",
			html: `<img><img src=""><img src="   ">`,
			want: nil,
		},
		{
			name: "dedupe preserves first seen order",
			html: `<img src="/b.jpg"><img src="/a.jpg"><img src="/b.jpg" srcset="/a.jpg 1x"><img src="/a.jpg">`,
			want: []string{"https://img.example.com/b.jpg", "https://img.example.com/a.jpg"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := harvestImageURLs(base, []byte(tt.html))
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestHarvestImageURLsCap(t *testing.T) {
	base := mustURL(t, "https://example.com/")
	html := `<html><body>`
	for i := 0; i < maxHarvestedImages+10; i++ {
		html += `<img src="/thumb/` + string(rune('a'+i%26)) + string(rune('0'+i/26)) + `.webp">`
	}
	html += `</body></html>`

	got := harvestImageURLs(base, []byte(html))
	assert.Len(t, got, maxHarvestedImages)
}

func TestHarvestImageURLsPhotoPageFixture(t *testing.T) {
	body, err := os.ReadFile("testdata/photo_page.html")
	require.NoError(t, err)
	base := mustURL(t, "https://img.example.com/AAA1/_DSC1234")

	got := harvestImageURLs(base, body)
	assert.Equal(t, []string{
		"https://img.example.com/prev/full/256.jpg",
		"https://img.example.com/next/full/256.jpg",
		"https://img.example.com/AAA1/full/256.jpg",
		"https://img.example.com/thumb1/small.webp",
	}, got)
}

func TestElideDataURIs(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "image with alt",
			in:   `look ![a pic](data:image/webp;base64,UklGRhIA) here`,
			want: `look ![a pic](data URI elided) here`,
		},
		{
			name: "image empty alt",
			in:   `![](data:image/png;base64,iVBORw0KGgo=)`,
			want: `![](data URI elided)`,
		},
		{
			name: "link form",
			in:   `[click](data:text/html;base64,PGh0bWw+)`,
			want: `[click](data URI elided)`,
		},
		{
			name: "multiple occurrences",
			in:   `![](data:image/gif;base64,R0lGOD)![](data:image/jpeg;base64,/9j/4)`,
			want: `![](data URI elided)![](data URI elided)`,
		},
		{
			name: "normal markdown untouched",
			in:   `![real](https://example.com/a.png) [link](/page) and data: mentioned in prose`,
			want: `![real](https://example.com/a.png) [link](/page) and data: mentioned in prose`,
		},
		{
			name: "multiline base64 blob",
			in:   "![](data:image/webp;base64,UklGR\nhIA\nAABXRUJQ)",
			want: "![](data URI elided)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, elideDataURIs(tt.in))
		})
	}
}

// TestHarvestImageURLsClassRanking pins the owner correction (Nov 2026):
// og:image is preview-sized BY WEB CONVENTITION (production-confirmed
// against imgsite: og:image = display thumb), and the images field's
// FIRST entry is what the model treats as "the image behind the page".
// Full-size FILE links (extension-gated anchors, preload/image_src
// links) outrank displayed <img>s; social-preview metas are the LAST
// resort — even though they sit in <head> and win every document-order
// race. Within a class, document order is preserved.
func TestHarvestImageURLsClassRanking(t *testing.T) {
	base := mustURL(t, "https://img.example.com/AAA1/_DSC1234")
	html := `<html><head>` +
		`<meta property="og:image" content="/thumb.jpg">` +
		`<link rel="image_src" href="/legacy-full.png">` +
		`</head><body>` +
		`<img src="/display.webp">` +
		`<a href="/AAA1/orig.jpg">open file</a>` +
		`</body></html>`
	want := []string{
		"https://img.example.com/legacy-full.png", // link: a real file resource
		"https://img.example.com/AAA1/orig.jpg",   // anchor: the file itself
		"https://img.example.com/display.webp",    // displayed image
		"https://img.example.com/thumb.jpg",       // social preview, last resort
	}
	got := harvestImageURLs(base, []byte(html))
	assert.Equal(t, want, got)
}
