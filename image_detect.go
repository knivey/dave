package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chai2010/webp"
	"golang.org/x/image/bmp"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	"golang.org/x/net/html"
)

const maxImageSize = 10 * 1024 * 1024 // 10MB
const defaultImageFormat = "jpg"
const defaultImageQuality = 75
const defaultMaxImageWidth = 1024
const defaultMaxImageHeight = 1024

// defaultMaxImagePixels is the default for the root-level maximagepixels
// config option: it caps the total pixel count (width * height * frameCount)
// an image may claim or decode at. Decode and resize cost is linear in
// megapixels, so this bounds CPU/memory regardless of file size on disk — a
// few-KB "decompression bomb" whose header claims huge dimensions is rejected
// from the header alone, before any pixel data is touched.
const defaultMaxImagePixels = 50_000_000

// errImageTooManyPixels is returned by convertImage when an image exceeds
// maxPixels. Callers treat it as a graceful skip (info-level log), not a hard
// error.
var errImageTooManyPixels = errors.New("image exceeds pixel cap")

// DESIGN NOTE: decompression-bomb defense.
//
// Decode and resize cost is linear in TOTAL megapixels (width * height *
// frameCount), not in file size — a few-KB image whose header claims huge
// dimensions can stall the resize for minutes or OOM the process. The defense
// is layered:
//
//  1. headerDimensions parses the claimed dimensions from the header only
//     (image.DecodeConfig / webp.DecodeConfig / bmp.DecodeConfig — no pixel
//     data touched) and convertImage rejects anything over maxPixels BEFORE
//     decoding. For GIFs the frame count is included because animated images
//     decode every frame.
//  2. GIFs decode only their first frame (gif.Decode, not gif.DecodeAll) —
//     the pipeline only ever used frame 0 anyway. Animated WebP/PNG are not a
//     concern here: the decoders in use (chai2010/libwebp still-image decode,
//     Go png ignoring APNG extension chunks) decode a single frame.
//  3. After decode, convertImage re-checks the ACTUAL dimensions against the
//     cap — crafted files can disagree with their headers.
//
// The skip is returned as errImageTooManyPixels, which callers treat as a
// graceful, info-logged skip rather than a hard error.
//
// headerDimensions fails open (ok=false) when a header can't be parsed: a
// broken header will fail the real decode anyway. BMP must NOT fail open
// despite being byte-capped: x/image/bmp allocates the full image from the
// header's claimed dimensions before reading any pixel data, and sub-8bpp
// files decode to 1 byte/pixel on the heap — so a 62-byte header can demand
// an arbitrarily large allocation regardless of file size.
func headerDimensions(imgData []byte, mimeType string) (width, height, frames int, ok bool) {
	switch {
	case mimeType == "image/gif":
		cfg, err := gif.DecodeConfig(bytes.NewReader(imgData))
		if err != nil {
			return 0, 0, 0, false
		}
		n, err := countGIFFrames(bytes.NewReader(imgData))
		if err != nil || n < 1 {
			// Malformed block structure: the decode path will surface the
			// error (or decode frame 1 only). Clamp so the cap math can't be
			// bypassed with a zero multiplier.
			n = 1
		}
		return cfg.Width, cfg.Height, n, true
	case mimeType == "image/webp":
		// chai2010's webp.DecodeConfig does a single r.Read of the header —
		// safe here because we always hand it a bytes.Reader over the full
		// body. Do not pass it a streaming reader.
		cfg, err := webp.DecodeConfig(bytes.NewReader(imgData))
		if err != nil {
			return 0, 0, 0, false
		}
		return cfg.Width, cfg.Height, 1, true
	case mimeType == "image/bmp":
		cfg, err := bmp.DecodeConfig(bytes.NewReader(imgData))
		if err != nil {
			return 0, 0, 0, false
		}
		return cfg.Width, cfg.Height, 1, true
	default:
		// jpeg, png, and any format registered via image.RegisterFormat
		// (chai2010/webp registers itself, so mixed sniffed types work too).
		cfg, _, err := image.DecodeConfig(bytes.NewReader(imgData))
		if err != nil {
			return 0, 0, 0, false
		}
		return cfg.Width, cfg.Height, 1, true
	}
}

// exceedsPixelCap reports whether width*height*frames exceeds cap.
// The per-frame product is compared first so the frame multiplication can
// never overflow int64 (once w*h <= cap, multiplying by a frame count
// bounded by the GIF format's 16-bit fields stays well in range).
func exceedsPixelCap(width, height, frames int, cap int64) bool {
	if width <= 0 || height <= 0 {
		return false
	}
	px := int64(width) * int64(height)
	if px > cap {
		return true
	}
	if frames <= 1 {
		return false
	}
	return px*int64(frames) > cap
}

// countGIFFrames walks a GIF's block structure and counts image descriptors
// without decompressing any pixel data. Layout per the GIF89a spec:
// header (6) + logical screen descriptor (7) + optional global color table,
// then blocks introduced by 0x21 (extension), 0x2C (image descriptor — one
// per frame), or 0x3B (trailer).
func countGIFFrames(r io.Reader) (int, error) {
	br := bufio.NewReader(r)
	header := make([]byte, 6)
	if _, err := io.ReadFull(br, header); err != nil {
		return 0, fmt.Errorf("reading GIF header: %w", err)
	}
	if string(header) != "GIF87a" && string(header) != "GIF89a" {
		return 0, fmt.Errorf("invalid GIF signature %q", header)
	}

	lsd := make([]byte, 7)
	if _, err := io.ReadFull(br, lsd); err != nil {
		return 0, fmt.Errorf("reading logical screen descriptor: %w", err)
	}
	if lsd[4]&0x80 != 0 {
		gctSize := int64(3) << ((lsd[4] & 0x07) + 1)
		if _, err := io.CopyN(io.Discard, br, gctSize); err != nil {
			return 0, fmt.Errorf("skipping global color table: %w", err)
		}
	}

	frames := 0
	intro := make([]byte, 1)
	for {
		if _, err := io.ReadFull(br, intro); err != nil {
			return 0, fmt.Errorf("reading block introducer: %w", err)
		}
		switch intro[0] {
		case 0x3B: // trailer
			return frames, nil
		case 0x21: // extension: label byte followed by sub-blocks
			if _, err := io.ReadFull(br, intro); err != nil {
				return 0, fmt.Errorf("reading extension label: %w", err)
			}
			if err := skipGIFSubBlocks(br); err != nil {
				return 0, fmt.Errorf("skipping extension sub-blocks: %w", err)
			}
		case 0x2C: // image descriptor: one frame
			frames++
			desc := make([]byte, 9)
			if _, err := io.ReadFull(br, desc); err != nil {
				return 0, fmt.Errorf("reading image descriptor: %w", err)
			}
			// desc = left(2) + top(2) + width(2) + height(2) + packed(1)
			if desc[8]&0x80 != 0 {
				lctSize := int64(3) << ((desc[8] & 0x07) + 1)
				if _, err := io.CopyN(io.Discard, br, lctSize); err != nil {
					return 0, fmt.Errorf("skipping local color table: %w", err)
				}
			}
			// LZW minimum code size byte, then the image's sub-blocks.
			if _, err := io.ReadFull(br, intro); err != nil {
				return 0, fmt.Errorf("reading LZW min code size: %w", err)
			}
			if err := skipGIFSubBlocks(br); err != nil {
				return 0, fmt.Errorf("skipping image data sub-blocks: %w", err)
			}
		default:
			return 0, fmt.Errorf("invalid block introducer 0x%02X", intro[0])
		}
	}
}

func skipGIFSubBlocks(br *bufio.Reader) error {
	size := make([]byte, 1)
	for {
		if _, err := io.ReadFull(br, size); err != nil {
			return err
		}
		if size[0] == 0 {
			return nil
		}
		if _, err := io.CopyN(io.Discard, br, int64(size[0])); err != nil {
			return err
		}
	}
}

func formatSize(b int) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

var urlRegex = regexp.MustCompile(`(?i)https?://\S+`)

func detectImageURLs(text string) (originalText string, urls []string) {
	matches := urlRegex.FindAllString(text, -1)
	seen := make(map[string]bool)
	for _, u := range matches {
		u = strings.TrimRight(u, ".,;:)")
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	return text, urls
}

func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsUnspecified() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 169 && ip4[1] == 254:
			return true
		case ip4[0] == 0:
			return true
		case ip4[0] == 127:
			return true
		}
	}
	return false
}

var ssrfSafeTransport = &http.Transport{
	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("invalid address: %w", err)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve host: %w", err)
		}
		for _, ipAddr := range ips {
			if isBlockedIP(ipAddr.IP) {
				return nil, fmt.Errorf("blocked IP address: %s", ipAddr.IP)
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(host, port))
	},
}

var imageHTTPClient = &http.Client{
	Timeout:   30 * time.Second,
	Transport: ssrfSafeTransport,
}

// imageHTTPClientTrusted skips ONLY the SSRF private-IP dial block, for
// hosts the admin explicitly listed in trusted_image_hosts (imgsite
// typically resolves privately). Every other download guarantee stays:
// scheme, 30s timeout, size cap, content-type check — and redirects are
// NOT followed blind: the guarded client re-enters its checked dial on
// every hop, so the trusted one must too — each redirect target's host
// is re-checked against trusted_image_hosts (view_image URLs are
// model-chosen, i.e. prompt-injectable via fetched pages; a bare client
// would follow up to 10 hops to ANY host, private ranges included,
// widening the bypass beyond what the admin listed).
var imageHTTPClientTrusted = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !trustedImageHost(req.URL.Hostname()) {
			return fmt.Errorf("redirect to untrusted host %q blocked", req.URL.Host)
		}
		return nil
	},
}

// trustedImageHost reports whether host exactly (case-insensitively) matches
// a trusted_image_hosts entry. No wildcards, no suffix matching — a wildcard
// bypass of an SSRF guard is a footgun (load-time validation enforces the
// same for the config file).
func trustedImageHost(host string) bool {
	if host == "" {
		return false
	}
	var hosts []string
	readConfig(func() { hosts = config.TrustedImageHosts })
	for _, h := range hosts {
		if strings.EqualFold(h, host) {
			return true
		}
	}
	return false
}

// fetchURLBounded is the shared fetch core of downloadImage and view_image's
// page resolution: trusted-host client selection, status check, and a
// bounded body read. It does NOT check the content type — callers decide
// what they accept.
func fetchURLBounded(url string, maxBytes int64) ([]byte, string, error) {
	if logger != nil {
		logger.Debug("fetching url", "url", url)
	}
	client := imageHTTPClient
	if u, perr := neturl.Parse(url); perr == nil && trustedImageHost(u.Hostname()) {
		client = imageHTTPClientTrusted
		if logger != nil {
			logger.Debug("trusted image host, skipping SSRF private-IP guard", "host", u.Hostname())
		}
	}

	resp, err := client.Get(url)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image URL returned status %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	contentLength := resp.ContentLength
	if contentLength > maxBytes {
		return nil, "", fmt.Errorf("image too large (%d bytes, max %d)", contentLength, maxBytes)
	}

	// Always bound the body read: a missing/lying Content-Length (chunked
	// transfer) must not turn into an unbounded read that buffers until the
	// client timeout. Reading maxBytes+1 lets the size check below
	// distinguish "at the cap" from "over the cap".
	bodyReader := io.LimitReader(resp.Body, maxBytes+1)

	data, err := io.ReadAll(bodyReader)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read image body: %w", err)
	}

	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("image too large (%d bytes, max %d)", len(data), maxBytes)
	}

	if logger != nil {
		logger.Debug("url fetched", "url", url, "size", formatSize(len(data)), "content_type", contentType)
	}
	return data, contentType, nil
}

func downloadImage(url string) ([]byte, string, error) {
	if logger != nil {
		logger.Debug("downloading image", "url", url)
	}
	data, contentType, err := fetchURLBounded(url, int64(maxImageSize))
	if err != nil {
		return nil, "", err
	}
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", fmt.Errorf("URL did not return an image (content-type: %s)", contentType)
	}
	return data, contentType, nil
}

// imageFileExtensions gates which <a href> values count as image links:
// anchors that merely WRAP a thumbnail (gallery filmstrips) are page
// links, not images — harvesting extension-less hrefs would flood the
// candidate list with page URLs (same gate fetch-mcp's harvest uses).
var imageFileExtensions = []string{
	".jpg", ".jpeg", ".png", ".gif", ".webp", ".avif", ".bmp", ".svg",
}

// looksLikeImageFileURL reports whether raw looks like a direct image
// file link: the URL's path (query and fragment ignored) ends in a known
// image extension, case-insensitively.
func looksLikeImageFileURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return false
	}
	path := strings.ToLower(u.Path)
	for _, ext := range imageFileExtensions {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

// pageImageCandidateCap bounds how many harvested candidates view_image
// will try when resolving an HTML page to its image.
const pageImageCandidateCap = 5

// imageHeaderProbeBytes bounds the ranged GET used to read a candidate's
// image header for dimension decoding: every common raster format's
// dimensions live well within the first 4KiB.
const imageHeaderProbeBytes = 4096

// probeImageConfig decodes pixel dimensions from the HEAD BYTES of an
// image (what a bounded ranged GET returns) across the common web image
// formats — the owner directive is ALL of them, not just what production
// has served so far:
//   - JPEG / PNG / GIF: stdlib image.DecodeConfig
//   - WebP / BMP / TIFF: x/image + chai2010/webp, which self-register
//     into the global image registry on import
//   - AVIF / HEIC / HEIF: hand-rolled ISOBMFF walk to the ispe box
//     (no Go decoder needed — the container states its own dimensions)
//   - ICO: directory entry bytes (0 means 256)
//   - SVG / JPEG XL / anything else: unknown (0) — vectors have no
//     intrinsic pixels and JXL has no decoder in the module graph
func probeImageConfig(data []byte) (w, h int, ok bool) {
	if len(data) > 0 {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil && cfg.Width > 0 && cfg.Height > 0 {
			return cfg.Width, cfg.Height, true
		}
		if pw, ph, pok := parseISOBMFFDims(data); pok {
			return pw, ph, true
		}
		if pw, ph, pok := parseICODims(data); pok {
			return pw, ph, true
		}
	}
	return 0, 0, false
}

// parseISOBMFFDims walks an ISO base media file format container (AVIF,
// HEIC, HEIF) looking for an ispe box (image spatial extents: 4-byte
// version/flags + big-endian width + height). Boxes are length-prefixed;
// meta/hdlr carry full boxes (4 payload bytes before children), iprp and
// ipco are plain boxes — the walk recurses into both shapes and skips
// what it does not recognize rather than failing.
func parseISOBMFFDims(data []byte) (w, h int, ok bool) {
	// ftyp must lead the file (brand table after version/flags position).
	if len(data) < 12 || string(data[4:8]) != "ftyp" {
		return 0, 0, false
	}
	var walk func(b []byte, depth int) (int, int, bool)
	walk = func(b []byte, depth int) (int, int, bool) {
		if depth > 6 {
			return 0, 0, false
		}
		for off := 0; off+8 <= len(b); {
			size := int(uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3]))
			typ := string(b[off+4 : off+8])
			header := 8
			if size == 1 {
				// 64-bit sizes exceed any probe buffer; skip rest.
				return 0, 0, false
			}
			if size < header || off+size > len(b) {
				// Truncated or oversized: recurse into what we have.
				size = len(b) - off
			}
			payload := b[off+header : off+size]
			switch typ {
			case "ispe":
				if len(payload) >= 12 {
					pw := int(uint32(payload[4])<<24 | uint32(payload[5])<<16 | uint32(payload[6])<<8 | uint32(payload[7]))
					ph := int(uint32(payload[8])<<24 | uint32(payload[9])<<16 | uint32(payload[10])<<8 | uint32(payload[11]))
					if pw > 0 && ph > 0 {
						return pw, ph, true
					}
				}
			case "meta", "iprp", "ipco", "ipma", "moov", "trak", "mdia", "minf", "stbl":
				// meta is a FullBox (4 flag bytes); iprp/ipco are plain.
				start := 0
				if typ == "meta" {
					start = 4
				}
				if start < len(payload) {
					if w, h, ok := walk(payload[start:], depth+1); ok {
						return w, h, true
					}
				}
			}
			off += size
		}
		return 0, 0, false
	}
	return walk(data, 0)
}

// parseICODims reads the first directory entry of an ICO file: byte 6 is
// width, byte 7 height, 0 meaning 256.
func parseICODims(data []byte) (w, h int, ok bool) {
	if len(data) < 8 || data[0] != 0 || data[1] != 0 || data[2] != 1 || data[3] != 0 || data[4] == 0 {
		return 0, 0, false
	}
	pw, ph := int(data[6]), int(data[7])
	if pw == 0 {
		pw = 256
	}
	if ph == 0 {
		ph = 256
	}
	return pw, ph, true
}

// fetchImageHeaderBytes performs the bounded ranged GET: the first
// imageHeaderProbeBytes of the URL through the same SSRF-guarded client
// selection every other image fetch uses. Servers that ignore Range
// simply have their stream truncated at the cap.
func fetchImageHeaderBytes(url string) ([]byte, error) {
	client := imageHTTPClient
	if u, perr := neturl.Parse(url); perr == nil && trustedImageHost(u.Hostname()) {
		client = imageHTTPClientTrusted
	}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", imageHeaderProbeBytes-1))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("probe returned status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, imageHeaderProbeBytes))
}

// rankCandidatesByProbedDims reorders candidates by the pixel area
// decoded from each one's image header bytes (bounded ranged GETs) when
// the markup declared nothing — session 3179's follow-up: markup
// heuristics are blind on pages that omit width/height, and the header
// bytes carry the real signal. Decoding failures keep relative order
// after the knowns (stable sort); the whole thing is best-effort — probe
// errors never remove a candidate, they just leave it unranked.
func rankCandidatesByProbedDims(cands []string) []string {
	type scored struct {
		url  string
		area int64
	}
	scoredCands := make([]scored, len(cands))
	for i, u := range cands {
		scoredCands[i] = scored{url: u}
		data, err := fetchImageHeaderBytes(u)
		if err != nil {
			continue
		}
		if w, h, ok := probeImageConfig(data); ok {
			scoredCands[i].area = int64(w) * int64(h)
		}
	}
	sort.SliceStable(scoredCands, func(i, j int) bool {
		return scoredCands[i].area > scoredCands[j].area
	})
	out := make([]string, len(scoredCands))
	for i, c := range scoredCands {
		out[i] = c.url
	}
	return out
}

// pageImageCand is one harvest candidate carrying the size the markup
// DECLARED for it (pixel area when width/height attrs are present, else
// the max srcset width descriptor; 0 = undeclared). Declared sizes rank
// the displayed-image class; when nothing declares, the caller probes
// real image headers instead (rankCandidatesByProbedDims).
type pageImageCand struct {
	url      string
	declared int64
}

// harvestPageImageURLs extracts candidate image URLs from an HTML page,
// ranked by IDENTITY (session 3179: extension'd filmstrip anchors — every
// neighbor thumb wrapped in <a href="/NNN/full/256.jpg"> — outranked the
// page's own viewer image and the model edited the wrong photo; an
// extension'd anchor signals A file link, not THE page's file link).
// Emission order: (1) a SINGLE unique extension'd anchor — the "open
// file" details-page pattern; (2) displayed <img> candidates by declared
// size desc (width/height attrs, srcset width descriptors), document
// order for ties/unknowns, data-src before src within an img; (3) a BATCH
// of distinct anchors — a gallery filmstrip, neighbors not "the" image —
// demoted below the page's own imgs; (4) og:image, then twitter:image,
// LAST (EXACT property/name matches only — og:image:width and friends
// carry non-URLs; og is also preview-sized by web convention). Relative
// URLs resolve against the page URL; data: and non-http(s) candidates
// are skipped; deduped, capped. anyDeclared reports whether any
// displayed candidate carried a declared size — false tells the caller
// the markup ranking is blind and the image-header probe should run.
func harvestPageImageURLs(body []byte, baseURL string) (out []string, anyDeclared bool) {
	base, err := neturl.Parse(baseURL)
	if err != nil || !base.IsAbs() {
		return nil, false
	}

	var anchors []string
	var imgs []pageImageCand
	var og, tw []string
	tk := html.NewTokenizer(bytes.NewReader(body))
	for {
		tt := tk.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		tagBytes, _ := tk.TagName()
		tag := string(tagBytes)
		if tag != "meta" && tag != "img" && tag != "a" {
			continue
		}
		var attrs map[string]string
		for {
			k, v, more := tk.TagAttr()
			if attrs == nil {
				attrs = make(map[string]string, 8)
			}
			attrs[strings.ToLower(string(k))] = string(v)
			if !more {
				break
			}
		}
		switch tag {
		case "a":
			if looksLikeImageFileURL(attrs["href"]) {
				anchors = append(anchors, attrs["href"])
			}
		case "meta":
			if attrs["property"] == "og:image" && attrs["content"] != "" {
				og = append(og, attrs["content"])
			}
			if attrs["name"] == "twitter:image" && attrs["content"] != "" {
				tw = append(tw, attrs["content"])
			}
		case "img":
			// srcset descriptors first: when a URL appears both as src
			// and inside srcset, the descriptor-carrying occurrence must
			// win the dedupe or the candidate loses its declared size.
			// data-* before plain attributes otherwise (lazy-load
			// convention: the real image lives in data-src).
			score := imgDeclaredScore(attrs)
			for _, pair := range srcsetCandidates(attrs["data-srcset"]) {
				imgs = append(imgs, pair)
			}
			for _, pair := range srcsetCandidates(attrs["srcset"]) {
				imgs = append(imgs, pair)
			}
			if attrs["data-src"] != "" {
				imgs = append(imgs, pageImageCand{url: attrs["data-src"], declared: score})
			}
			if attrs["src"] != "" {
				imgs = append(imgs, pageImageCand{url: attrs["src"], declared: score})
			}
		}
	}

	resolve := func(raw string) (string, bool) {
		u, perr := neturl.Parse(raw)
		if perr != nil {
			return "", false
		}
		// Relative URLs (empty scheme) resolve against the page; only
		// ABSOLUTE non-http(s) schemes (data:, javascript:, ftp:…) are
		// rejected outright.
		if u.Scheme != "" && u.Scheme != "http" && u.Scheme != "https" {
			return "", false
		}
		abs := base.ResolveReference(u)
		return abs.String(), true
	}

	// Displayed imgs: declared size desc, document order stable.
	sort.SliceStable(imgs, func(i, j int) bool {
		return imgs[i].declared > imgs[j].declared
	})
	for _, c := range imgs {
		if c.declared > 0 {
			anyDeclared = true
		}
	}

	// Anchor count is over UNIQUE resolved URLs (imgsite's orig anchor
	// appears twice — download + open file — but is ONE file).
	seen := make(map[string]bool)
	add := func(raw string) {
		if len(out) >= pageImageCandidateCap {
			return
		}
		if resolved, ok := resolve(raw); ok && !seen[resolved] {
			seen[resolved] = true
			out = append(out, resolved)
		}
	}

	var anchorUniques []string
	for _, a := range anchors {
		if r, ok := resolve(a); ok && !containsStr(anchorUniques, r) {
			anchorUniques = append(anchorUniques, r)
		}
	}

	switch len(anchorUniques) {
	case 1:
		// The open-file pattern: the page links exactly one image file.
		add(anchorUniques[0])
		for _, c := range imgs {
			add(c.url)
		}
	default:
		// No anchors, or a gallery filmstrip (many distinct files =
		// neighbors): the page's own displayed images come first.
		for _, c := range imgs {
			add(c.url)
		}
		for _, a := range anchorUniques {
			add(a)
		}
	}
	for _, m := range og {
		add(m)
	}
	for _, m := range tw {
		add(m)
	}
	return out, anyDeclared
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// imgDeclaredScore derives a pixel-area score from an <img> tag's
// width/height attributes (both required for area; a lone dim scores its
// value — better than blind). Attributes can carry units or percentages;
// only bare integers count.
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

// srcsetCandidates splits a srcset attribute into (url, descriptor-score)
// pairs; the pixel-width descriptor doubles as the declared-size score.
func srcsetCandidates(s string) []pageImageCand {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []pageImageCand
	for _, cand := range strings.Split(s, ",") {
		fields := strings.Fields(strings.TrimSpace(cand))
		if len(fields) == 0 {
			continue
		}
		var score int64
		if len(fields) > 1 && strings.HasSuffix(fields[1], "w") {
			if n, ok := atoiStrict(strings.TrimSuffix(fields[1], "w")); ok {
				score = n
			}
		}
		out = append(out, pageImageCand{url: fields[0], declared: score})
	}
	return out
}

// convertToFetched is the shared download→convert dressing: bytes of any
// image content-type become a fetchedImage (Encoded + full DataURI + MIME
// per requested format).
func convertToFetched(imgData []byte, mimeType, format string, quality, maxW, maxH, maxPixels int) (*fetchedImage, error) {
	encoded, dataURI, w, h, err := convertImage(imgData, mimeType, format, quality, maxW, maxH, maxPixels)
	if err != nil {
		return nil, err
	}
	mime := "image/jpeg"
	if format == "webp" {
		mime = "image/webp"
	}
	return &fetchedImage{
		Encoded: encoded,
		DataURI: dataURI + base64.StdEncoding.EncodeToString(encoded),
		MIME:    mime,
		Width:   w,
		Height:  h,
	}, nil
}

// fetchViewImageSource resolves what view_image should attach: a direct
// image URL passes through; an HTML page is resolved to its primary image
// (harvestPageImageURLs order) with the FIRST candidate that actually
// fetches AND converts as an image winning — no recursion (a candidate
// that is itself HTML does not chase further), and a candidate that
// fetches as an image but fails conversion (corrupt/bomb) falls through
// to the next candidate, each being independently bomb-checked. The FIRST
// fetch keeps the full maxImageSize budget: it is usually a direct image,
// and a page-sized bound here would silently reject every direct image
// between the two caps (the review-caught regression). A page therefore
// reads up to maxImageSize bytes too — bounded, just less tightly than a
// dedicated page cap would be; harvest on a truncated page simply sees
// fewer candidates. Returns the converted image, the RESOLVED source URL
// (what the label and ref register — never the page), and the originating
// page URL when extraction happened ("" for direct images).
func fetchViewImageSource(url, format string, quality, maxW, maxH, maxPixels int) (*fetchedImage, string, string, error) {
	data, contentType, err := fetchURLBounded(url, int64(maxImageSize))
	if err != nil {
		return nil, "", "", err
	}
	if strings.HasPrefix(contentType, "image/") {
		fi, cerr := convertToFetched(data, contentType, format, quality, maxW, maxH, maxPixels)
		if cerr != nil {
			return nil, "", "", cerr
		}
		return fi, url, "", nil
	}
	if !strings.HasPrefix(contentType, "text/html") {
		return nil, "", "", fmt.Errorf("URL did not return an image (content-type: %s)", contentType)
	}
	var lastCandErr error
	cands, anyDeclared := harvestPageImageURLs(data, url)
	if !anyDeclared && len(cands) > 1 {
		// Markup ranking is blind (no width/height/srcset descriptors
		// anywhere): probe the candidates' image header bytes and rank by
		// real pixel area — a filmstrip's neighbor files must not win just
		// because the page omits dimensions.
		cands = rankCandidatesByProbedDims(cands)
	}
	for _, cand := range cands {
		b, ct, ferr := fetchURLBounded(cand, int64(maxImageSize))
		if ferr != nil {
			lastCandErr = ferr
			continue
		}
		if !strings.HasPrefix(ct, "image/") {
			continue
		}
		fi, cerr := convertToFetched(b, ct, format, quality, maxW, maxH, maxPixels)
		if cerr != nil {
			// Corrupt/bomb candidate: try the next one; remember why for
			// the error tail below.
			lastCandErr = cerr
			continue
		}
		return fi, cand, url, nil
	}
	if logger != nil && lastCandErr != nil {
		logger.Debug("page image candidates all failed", "url", url, "last_error", lastCandErr.Error())
	}
	return nil, "", "", fmt.Errorf(
		"URL was an HTML page (content-type: %s) and no viewable image could be extracted from it — use fetch to read the page and locate a direct image URL",
		contentType)
}

func convertImage(imgData []byte, mimeType, format string, quality, maxW, maxH, maxPixels int) (out []byte, dataURI string, width int, height int, err error) {
	if maxPixels <= 0 {
		maxPixels = defaultMaxImagePixels
	}

	// Pre-decode guard: reject images whose headers claim more than maxPixels
	// total (dimensions x frame count) before touching any pixel data.
	if w, h, frames, ok := headerDimensions(imgData, mimeType); ok {
		if exceedsPixelCap(w, h, frames, int64(maxPixels)) {
			return nil, "", 0, 0, fmt.Errorf("%w: header claims %dx%d x %d frame(s)", errImageTooManyPixels, w, h, frames)
		}
	}

	var img image.Image

	reader := bytes.NewReader(imgData)
	switch {
	case mimeType == "image/jpeg" || strings.HasSuffix(mimeType, ".jpg"):
		img, err = jpeg.Decode(reader)
	case mimeType == "image/png":
		img, err = png.Decode(reader)
	case mimeType == "image/gif":
		// gif.Decode (first frame only), NOT gif.DecodeAll: decoding every
		// frame multiplies decode cost by frame count, and only frame 0 has
		// ever been used here.
		img, err = gif.Decode(reader)
	case mimeType == "image/bmp":
		img, err = decodeBMP(reader)
	case mimeType == "image/webp":
		img, err = webp.Decode(reader)
	default:
		img, err = decodeAny(reader)
	}

	if err != nil {
		return nil, "", 0, 0, fmt.Errorf("failed to decode image: %w", err)
	}

	bounds := img.Bounds()
	width = bounds.Dx()
	height = bounds.Dy()

	// Defense-in-depth: headers can disagree with decoded dimensions in
	// crafted files, so re-check the real dimensions before resizing.
	if exceedsPixelCap(width, height, 1, int64(maxPixels)) {
		return nil, "", 0, 0, fmt.Errorf("%w: decoded image is %dx%d", errImageTooManyPixels, width, height)
	}

	if maxW > 0 || maxH > 0 {
		newW := width
		newH := height
		scale := 1.0

		if maxW > 0 && width > maxW {
			scale = float64(maxW) / float64(width)
		}
		if maxH > 0 && height > maxH {
			hScale := float64(maxH) / float64(height)
			if hScale < scale {
				scale = hScale
			}
		}

		if scale < 1.0 {
			newW = int(float64(width) * scale)
			newH = int(float64(height) * scale)
			if logger != nil {
				logger.Debug("scaling image", "original", fmt.Sprintf("%dx%d", width, height), "scaled", fmt.Sprintf("%dx%d", newW, newH))
			}

			dst := image.NewNRGBA(image.Rect(0, 0, newW, newH))
			draw.NearestNeighbor.Scale(dst, dst.Rect, img, bounds, draw.Over, nil)
			img = dst
			bounds = img.Bounds()
		}
	}

	if format == "" {
		format = defaultImageFormat
	}
	if quality == 0 {
		quality = defaultImageQuality
	}

	var buf bytes.Buffer

	switch format {
	case "webp":
		err = webp.Encode(&buf, img, &webp.Options{Quality: float32(quality)})
		if err != nil {
			return nil, "", 0, 0, fmt.Errorf("failed to encode webp: %w", err)
		}
		dataURI = "data:image/webp;base64,"
	case "jpg", "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
		if err != nil {
			return nil, "", 0, 0, fmt.Errorf("failed to encode jpeg: %w", err)
		}
		dataURI = "data:image/jpeg;base64,"
	default:
		return nil, "", 0, 0, fmt.Errorf("unsupported image format: %s", format)
	}

	encodedData := buf.Bytes()
	if logger != nil {
		logger.Debug("image converted", "format", format, "original_type", mimeType, "original_size", formatSize(len(imgData)), "new_size", formatSize(len(encodedData)))
	}
	return encodedData, dataURI, bounds.Dx(), bounds.Dy(), nil
}

// fetchedImage is the product of the shared intake pipeline: downloaded
// (SSRF-guarded unless trusted-hosted), bomb-checked, resized, re-encoded,
// base64-URI'd. Both intake paths consume it: user pastes
// (buildImageMessage) and view_image.
type fetchedImage struct {
	Encoded []byte // converted bytes — what the ref store persists
	DataURI string // full data:<mime>;base64,… URI for the model
	MIME    string // encoded mime type
	Width   int
	Height  int
}

func fetchAndConvertImage(url, format string, quality, maxW, maxH, maxPixels int) (*fetchedImage, error) {
	imgData, mimeType, err := downloadImage(url)
	if err != nil {
		return nil, err
	}
	return convertToFetched(imgData, mimeType, format, quality, maxW, maxH, maxPixels)
}

func decodeBMP(r io.Reader) (image.Image, error) {
	return bmp.Decode(r)
}

func decodeAny(r io.Reader) (image.Image, error) {
	img, _, err := image.Decode(r)
	return img, err
}

func countContextImages(messages []ChatMessage) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.MultiContent {
			if part.Type == PartTypeImageURL {
				count++
			}
		}
	}
	return count
}

func sanitizeMessages(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, len(messages))
	for i, msg := range messages {
		msgCopy := msg
		if len(msgCopy.MultiContent) > 0 {
			msgCopy.MultiContent = make([]MessagePart, len(msg.MultiContent))
			for j, part := range msg.MultiContent {
				partCopy := part
				if partCopy.Type == PartTypeImageURL && partCopy.ImageURL != nil {
					url := partCopy.ImageURL.URL
					if idx := strings.Index(url, ","); idx != -1 {
						mimeType := url[5:idx]
						partCopy.ImageURL = &ImageURL{
							URL:    "data:" + mimeType + ",...[truncated]",
							Detail: partCopy.ImageURL.Detail,
						}
					}
				}
				msgCopy.MultiContent[j] = partCopy
			}
		}
		out[i] = msgCopy
	}
	return out
}

func stripSuccessfulURLs(text string, successfulURLs []string) string {
	for _, url := range successfulURLs {
		for _, raw := range urlRegex.FindAllString(text, -1) {
			if strings.TrimRight(raw, ".,;:)") == url {
				text = strings.Replace(text, raw, "", 1)
				break
			}
		}
	}
	return strings.Join(strings.Fields(text), " ")
}

func buildImageMessage(text string, imageUrls []string, maxImages int, format string, quality, maxW, maxH, maxPixels int, sessionID int64) (ChatMessage, error) {
	if len(imageUrls) > maxImages {
		imageUrls = imageUrls[:maxImages]
	}

	var parts []MessagePart
	var successfulURLs []string

	// Each label part PRECEDES its image part — the adjacent text block
	// pairing: the model visually maps the ref in the label to the image
	// that follows it, so a future edit tool can resolve "usr_xxxx" back
	// to the exact bytes in context.
	for _, url := range imageUrls {
		fi, err := fetchAndConvertImage(url, format, quality, maxW, maxH, maxPixels)
		if err != nil {
			if errors.Is(err, errImageTooManyPixels) {
				// Decompression-bomb skip: observable but non-fatal.
				if logger != nil {
					logger.Info("skipping image: too many pixels", "url", url, "reason", err.Error())
				}
				continue
			}
			if logger != nil {
				logger.Warn("skipping image URL", "url", url, "error", err.Error())
			}
			continue
		}

		ref, refErr := registerImageRef(sessionID, "user", url, fi.MIME, fi.Width, fi.Height, fi.Encoded)
		if refErr != nil {
			if logger != nil {
				logger.Warn("image ref registration failed (image still attached)", "url", url, "error", refErr.Error())
			}
		}
		if ref == "" {
			ref = "unregistered"
		}
		n := len(successfulURLs) + 1
		parts = append(parts,
			MessagePart{Type: PartTypeText, Text: fmt.Sprintf("[image %d — ref: %s — %s]", n, ref, url)},
			MessagePart{
				Type: PartTypeImageURL,
				ImageURL: &ImageURL{
					URL:    fi.DataURI,
					Detail: ImageDetailAuto,
				},
			},
		)
		successfulURLs = append(successfulURLs, url)
	}

	text = stripSuccessfulURLs(text, successfulURLs)

	if text != "" {
		parts = append([]MessagePart{{
			Type: PartTypeText,
			Text: text,
		}}, parts...)
	}

	if len(parts) == 0 {
		return ChatMessage{}, fmt.Errorf("no valid images or text found")
	}

	return ChatMessage{
		Role:         RoleUser,
		MultiContent: parts,
	}, nil
}

const viewImageToolName = "view_image"

// rootMaxImagePixels reads the root maximagepixels knob (0 -> default).
func rootMaxImagePixels() int {
	var v int
	readConfig(func() { v = config.MaxImagePixels })
	if v <= 0 {
		return defaultMaxImagePixels
	}
	return v
}

// handleViewImage implements the view_image builtin: pull an image URL into
// the conversation as vision input. The image rides a labeled RoleUser
// message queued after the tool result (flushed by executeToolCalls after
// ALL results of the round) — the ONLY way images enter context besides
// user pastes. Generated-image URLs are NEVER auto-injected (owner
// directive: context economy — the model views them explicitly through
// this tool); every failure is tool-result text the model can act on.
func (cr *chatRunner) handleViewImage(turn *turnContext, call ToolCall) bool {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		turn.Add(toolResultMsg(call.ID, "error: failed to parse arguments: "+err.Error()))
		return false
	}
	if turn.ephemeral {
		// Ephemeral (generator) turns never advertise view_image; a
		// hallucinated call errors like the generator tools' !ephemeral
		// guards do in reverse.
		turn.Add(toolResultMsg(call.ID, "error: view_image is not available on this command"))
		return false
	}
	if args.URL == "" {
		turn.Add(toolResultMsg(call.ID, "error: url is required"))
		return false
	}
	u, err := neturl.Parse(args.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		turn.Add(toolResultMsg(call.ID, "error: url must be an http(s) URL"))
		return false
	}
	// Budget check against the live turn. Tool results of this round are
	// no concern — each call turn.Add's its own result inside the
	// executeToolCalls loop, so countContextImages sees them. The ONLY
	// lagging state is pendingInjections: view_image calls earlier in
	// this same round have queued their image but it is not in
	// turn.Messages() until the round's flush — without counting them,
	// N parallel calls each check the same stale snapshot and exceed the
	// cap by N-1 (persisted into the session until compaction).
	remaining := cr.cfg.MaxContextImages - countContextImages(turn.Messages()) - len(cr.pendingInjections)
	if remaining <= 0 {
		turn.Add(toolResultMsg(call.ID, fmt.Sprintf(
			"error: context image budget reached (max %d images in the conversation). Tell the user to start a new session.",
			cr.cfg.MaxContextImages)))
		return false
	}
	fi, srcURL, fromPage, err := fetchViewImageSource(args.URL, cr.cfg.ImageFormat, cr.cfg.ImageQuality, cr.cfg.MaxImageWidth, cr.cfg.MaxImageHeight, rootMaxImagePixels())
	if err != nil {
		turn.Add(toolResultMsg(call.ID, "error: "+err.Error()))
		return false
	}
	// The ref and the label carry the RESOLVED image URL — never the page.
	// For page extractions the model must be able to see, permanently in
	// context, WHAT it is looking at: the page's preview image, not the
	// page's content (owner ruling: no silent resolution — that would
	// confirm the bias that every link is an image).
	ref, refErr := registerImageRef(cr.sessionID, "view", srcURL, fi.MIME, fi.Width, fi.Height, fi.Encoded)
	if refErr != nil {
		cr.logger.Warn("image ref registration failed (image still attached)", "url", srcURL, "error", refErr.Error())
	}
	if ref == "" {
		ref = "unregistered"
	}
	if fromPage != "" {
		turn.Add(toolResultMsg(call.ID, fmt.Sprintf(
			"URL was an HTML page, not an image. Extracted the page's primary image and attached it in the next message — ref: %s. Source: %s (%dx%d). Requested page: %s. NOTE: the attached image may be a preview or thumbnail rather than the full-size original — use fetch for the page's text.",
			ref, srcURL, fi.Width, fi.Height, fromPage)))
	} else {
		turn.Add(toolResultMsg(call.ID, fmt.Sprintf(
			"Image loaded and attached in the next message — ref: %s. Source: %s (%dx%d).",
			ref, srcURL, fi.Width, fi.Height)))
	}
	cr.pendingInjections = append(cr.pendingInjections, ChatMessage{
		Role: RoleUser,
		MultiContent: []MessagePart{
			// Label PRECEDES the image — the ref-pairing contract: the model
			// visually maps ref -> image (old image-editing plan).
			{Type: PartTypeText, Text: fmt.Sprintf("[image — ref: %s — source: %s]", ref, srcURL)},
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: fi.DataURI, Detail: ImageDetailAuto}},
		},
	})
	return false
}
