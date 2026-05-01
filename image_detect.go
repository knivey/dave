package main

import (
	"bytes"
	"context"
	"encoding/base64"
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

const maxImageSize = 5 * 1024 * 1024 // 5MB
const defaultImageFormat = "jpg"
const defaultImageQuality = 75
const defaultMaxImageWidth = 1024
const defaultMaxImageHeight = 1024

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

	var bodyReader io.Reader = resp.Body
	if contentLength > 0 {
		bodyReader = io.LimitReader(resp.Body, maxImageSize+1)
	}

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

func convertImage(imgData []byte, mimeType, format string, quality, maxW, maxH int) ([]byte, string, error) {
	var img image.Image
	var err error

	reader := bytes.NewReader(imgData)
	switch {
	case mimeType == "image/jpeg" || strings.HasSuffix(mimeType, ".jpg"):
		img, err = jpeg.Decode(reader)
	case mimeType == "image/png":
		img, err = png.Decode(reader)
	case mimeType == "image/gif":
		var g *gif.GIF
		g, err = gif.DecodeAll(reader)
		if err == nil && len(g.Image) > 0 {
			img = g.Image[0]
		}
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

func buildImageMessage(text string, imageUrls []string, maxImages int, format string, quality, maxW, maxH int) (ChatMessage, error) {
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

		imgData, dataURI, err := convertImage(imgData, mimeType, format, quality, maxW, maxH)
		if err != nil {
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
