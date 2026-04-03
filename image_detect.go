package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/chai2010/webp"
	gogpt "github.com/sashabaranov/go-openai"
	"golang.org/x/image/bmp"
)

const maxImageSize = 5 * 1024 * 1024 // 5MB
const webpQuality = 75.0

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

var imageURLRegex = regexp.MustCompile(`(?i)https?://\S+\.(?:jpe?g|png|gif|webp|bmp)(?:\?\S*)?`)

func detectImageURLs(text string) (cleanText string, urls []string) {
	matches := imageURLRegex.FindAllString(text, -1)
	seen := make(map[string]bool)
	for _, u := range matches {
		u = strings.TrimRight(u, ".,;:)")
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	cleanText = imageURLRegex.ReplaceAllString(text, "")
	cleanText = strings.Join(strings.Fields(cleanText), " ")
	return cleanText, urls
}

func downloadImage(url string) ([]byte, string, error) {
	logger.Debug("downloading image", "url", url)
	client := &http.Client{
		Timeout: 30 * time.Second,
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

	logger.Debug("image downloaded", "url", url, "size", formatSize(len(data)), "content_type", contentType)
	return data, contentType, nil
}

func convertToWebP(imgData []byte, mimeType string) ([]byte, error) {
	if mimeType == "image/webp" {
		_, err := webp.Decode(bytes.NewReader(imgData))
		if err == nil {
			logger.Debug("image already webp, skipping conversion", "size", formatSize(len(imgData)))
			return imgData, nil
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
		return nil, fmt.Errorf("failed to decode image: %w", err)
	}

	var buf bytes.Buffer
	err = webp.Encode(&buf, img, &webp.Options{Quality: webpQuality})
	if err != nil {
		return nil, fmt.Errorf("failed to encode webp: %w", err)
	}

	webpData := buf.Bytes()
	logger.Debug("image converted to webp", "original_type", mimeType, "original_size", formatSize(len(imgData)), "webp_size", formatSize(len(webpData)))
	return webpData, nil
}

func decodeBMP(r io.Reader) (image.Image, error) {
	return bmp.Decode(r)
}

func decodeAny(r io.Reader) (image.Image, error) {
	img, _, err := image.Decode(r)
	return img, err
}

func countContextImages(messages []gogpt.ChatCompletionMessage) int {
	count := 0
	for _, msg := range messages {
		for _, part := range msg.MultiContent {
			if part.Type == gogpt.ChatMessagePartTypeImageURL {
				count++
			}
		}
	}
	return count
}

func sanitizeMessages(messages []gogpt.ChatCompletionMessage) []gogpt.ChatCompletionMessage {
	out := make([]gogpt.ChatCompletionMessage, len(messages))
	for i, msg := range messages {
		msgCopy := msg
		if len(msgCopy.MultiContent) > 0 {
			msgCopy.MultiContent = make([]gogpt.ChatMessagePart, len(msg.MultiContent))
			for j, part := range msg.MultiContent {
				partCopy := part
				if partCopy.Type == gogpt.ChatMessagePartTypeImageURL && partCopy.ImageURL != nil {
					url := partCopy.ImageURL.URL
					if idx := strings.Index(url, ","); idx != -1 {
						mimeType := url[5:idx]
						partCopy.ImageURL = &gogpt.ChatMessageImageURL{
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

func buildImageMessage(text string, imageUrls []string, maxImages int) (gogpt.ChatCompletionMessage, error) {
	if len(imageUrls) > maxImages {
		imageUrls = imageUrls[:maxImages]
	}

	var parts []gogpt.ChatMessagePart

	if text != "" {
		parts = append(parts, gogpt.ChatMessagePart{
			Type: gogpt.ChatMessagePartTypeText,
			Text: text,
		})
	}

	for _, url := range imageUrls {
		imgData, mimeType, err := downloadImage(url)
		if err != nil {
			logger.Warn("skipping image URL", "url", url, "error", err.Error())
			continue
		}

		webpData, err := convertToWebP(imgData, mimeType)
		if err != nil {
			logger.Warn("failed to convert image to webp", "url", url, "error", err.Error())
			continue
		}

		b64 := base64.StdEncoding.EncodeToString(webpData)
		dataURI := "data:image/webp;base64," + b64

		parts = append(parts, gogpt.ChatMessagePart{
			Type: gogpt.ChatMessagePartTypeImageURL,
			ImageURL: &gogpt.ChatMessageImageURL{
				URL:    dataURI,
				Detail: gogpt.ImageURLDetailAuto,
			},
		})
	}

	if len(parts) == 0 {
		return gogpt.ChatCompletionMessage{}, fmt.Errorf("no valid images or text found")
	}

	return gogpt.ChatCompletionMessage{
		Role:         gogpt.ChatMessageRoleUser,
		MultiContent: parts,
	}, nil
}
