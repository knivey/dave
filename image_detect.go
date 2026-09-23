package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/chai2010/webp"
	"golang.org/x/image/bmp"
	"golang.org/x/image/draw"
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

func downloadImage(url string) ([]byte, string, error) {
	if logger != nil {
		logger.Debug("downloading image", "url", url)
	}
	client := imageHTTPClient

	resp, err := client.Get(url)
	if err != nil {
		return nil, "", fmt.Errorf("failed to fetch image: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("image URL returned status %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "image/") {
		return nil, "", fmt.Errorf("URL did not return an image (content-type: %s)", contentType)
	}

	contentLength := resp.ContentLength
	if contentLength > maxImageSize {
		return nil, "", fmt.Errorf("image too large (%d bytes, max %d)", contentLength, maxImageSize)
	}

	// Always bound the body read: a missing/lying Content-Length (chunked
	// transfer) must not turn into an unbounded read that buffers until the
	// client timeout. Reading maxImageSize+1 lets the size check below
	// distinguish "at the cap" from "over the cap".
	bodyReader := io.LimitReader(resp.Body, maxImageSize+1)

	data, err := io.ReadAll(bodyReader)
	if err != nil {
		return nil, "", fmt.Errorf("failed to read image body: %w", err)
	}

	if len(data) > maxImageSize {
		return nil, "", fmt.Errorf("image too large (%d bytes, max %d)", len(data), maxImageSize)
	}

	if logger != nil {
		logger.Debug("image downloaded", "url", url, "size", formatSize(len(data)), "content_type", contentType)
	}
	return data, contentType, nil
}

func convertImage(imgData []byte, mimeType, format string, quality, maxW, maxH, maxPixels int) ([]byte, string, error) {
	if maxPixels <= 0 {
		maxPixels = defaultMaxImagePixels
	}

	// Pre-decode guard: reject images whose headers claim more than maxPixels
	// total (dimensions x frame count) before touching any pixel data.
	if w, h, frames, ok := headerDimensions(imgData, mimeType); ok {
		if exceedsPixelCap(w, h, frames, int64(maxPixels)) {
			return nil, "", fmt.Errorf("%w: header claims %dx%d x %d frame(s)", errImageTooManyPixels, w, h, frames)
		}
	}

	var img image.Image
	var err error

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
		return nil, "", fmt.Errorf("failed to decode image: %w", err)
	}

	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	// Defense-in-depth: headers can disagree with decoded dimensions in
	// crafted files, so re-check the real dimensions before resizing.
	if exceedsPixelCap(width, height, 1, int64(maxPixels)) {
		return nil, "", fmt.Errorf("%w: decoded image is %dx%d", errImageTooManyPixels, width, height)
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
	var dataURI string

	switch format {
	case "webp":
		err = webp.Encode(&buf, img, &webp.Options{Quality: float32(quality)})
		if err != nil {
			return nil, "", fmt.Errorf("failed to encode webp: %w", err)
		}
		dataURI = "data:image/webp;base64,"
	case "jpg", "jpeg":
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality})
		if err != nil {
			return nil, "", fmt.Errorf("failed to encode jpeg: %w", err)
		}
		dataURI = "data:image/jpeg;base64,"
	default:
		return nil, "", fmt.Errorf("unsupported image format: %s", format)
	}

	encodedData := buf.Bytes()
	if logger != nil {
		logger.Debug("image converted", "format", format, "original_type", mimeType, "original_size", formatSize(len(imgData)), "new_size", formatSize(len(encodedData)))
	}
	return encodedData, dataURI, nil
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

func buildImageMessage(text string, imageUrls []string, maxImages int, format string, quality, maxW, maxH, maxPixels int) (ChatMessage, error) {
	if len(imageUrls) > maxImages {
		imageUrls = imageUrls[:maxImages]
	}

	var parts []MessagePart
	var successfulURLs []string

	for _, url := range imageUrls {
		imgData, mimeType, err := downloadImage(url)
		if err != nil {
			if logger != nil {
				logger.Warn("skipping image URL", "url", url, "error", err.Error())
			}
			continue
		}

		imgData, dataURI, err := convertImage(imgData, mimeType, format, quality, maxW, maxH, maxPixels)
		if err != nil {
			if errors.Is(err, errImageTooManyPixels) {
				// Decompression-bomb skip: observable but non-fatal.
				if logger != nil {
					logger.Info("skipping image: too many pixels", "url", url, "reason", err.Error())
				}
				continue
			}
			if logger != nil {
				logger.Warn("failed to convert image", "url", url, "error", err.Error())
			}
			continue
		}

		b64 := base64.StdEncoding.EncodeToString(imgData)
		dataURI = dataURI + b64

		parts = append(parts, MessagePart{
			Type: PartTypeImageURL,
			ImageURL: &ImageURL{
				URL:    dataURI,
				Detail: ImageDetailAuto,
			},
		})
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
