package main

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/color/palette"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/image/bmp"
)

func TestFormatSize(t *testing.T) {
	tests := []struct {
		name string
		b    int
		want string
	}{
		{name: "zero bytes", b: 0, want: "0 B"},
		{name: "one byte", b: 1, want: "1 B"},
		{name: "half KB", b: 512, want: "512 B"},
		{name: "one KB", b: 1024, want: "1.0 KiB"},
		{name: "1.5 KB", b: 1536, want: "1.5 KiB"},
		{name: "10 KB", b: 10 * 1024, want: "10.0 KiB"},
		{name: "100 KB", b: 100 * 1024, want: "100.0 KiB"},
		{name: "one MB", b: 1024 * 1024, want: "1.0 MiB"},
		{name: "5 MB", b: 5 * 1024 * 1024, want: "5.0 MiB"},
		{name: "100 MB", b: 100 * 1024 * 1024, want: "100.0 MiB"},
		{name: "one GB", b: 1024 * 1024 * 1024, want: "1.0 GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatSize(tt.b)
			assert.Equal(t, tt.want, got, "formatSize(%d)", tt.b)
		})
	}
}

func TestDetectImageURLs(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantUrls  []string
		wantClean string
	}{
		{
			name:      "no URLs",
			text:      "hello world",
			wantUrls:  nil,
			wantClean: "hello world",
		},
		{
			name:      "single jpg URL",
			text:      "check out https://example.com/image.jpg",
			wantUrls:  []string{"https://example.com/image.jpg"},
			wantClean: "check out https://example.com/image.jpg",
		},
		{
			name:      "single png URL",
			text:      "image: http://site.org/photo.png",
			wantUrls:  []string{"http://site.org/photo.png"},
			wantClean: "image: http://site.org/photo.png",
		},
		{
			name:      "gif URL",
			text:      "fun https://anim.com/anim.gif?q=1",
			wantUrls:  []string{"https://anim.com/anim.gif?q=1"},
			wantClean: "fun https://anim.com/anim.gif?q=1",
		},
		{
			name:      "webp URL",
			text:      "see https://img.webp",
			wantUrls:  []string{"https://img.webp"},
			wantClean: "see https://img.webp",
		},
		{
			name:      "bmp URL",
			text:      "bmp https://x.com/pic.bmp",
			wantUrls:  []string{"https://x.com/pic.bmp"},
			wantClean: "bmp https://x.com/pic.bmp",
		},
		{
			name:      "URL with trailing punctuation",
			text:      "look at https://a.com/pic.jpg, it's cool!",
			wantUrls:  []string{"https://a.com/pic.jpg"},
			wantClean: "look at https://a.com/pic.jpg, it's cool!",
		},
		{
			name:      "URL with trailing semicolon",
			text:      "check https://a.com/img.png; and this",
			wantUrls:  []string{"https://a.com/img.png"},
			wantClean: "check https://a.com/img.png; and this",
		},
		{
			name:      "URL with trailing parenthesis",
			text:      "(see https://a.com/img.jpg)",
			wantUrls:  []string{"https://a.com/img.jpg"},
			wantClean: "(see https://a.com/img.jpg)",
		},
		{
			name:      "URL with colon",
			text:      "https://a.com/pic.png: extra",
			wantUrls:  []string{"https://a.com/pic.png"},
			wantClean: "https://a.com/pic.png: extra",
		},
		{
			name:      "multiple URLs",
			text:      "images https://a.com/1.jpg and https://b.com/2.png here",
			wantUrls:  []string{"https://a.com/1.jpg", "https://b.com/2.png"},
			wantClean: "images https://a.com/1.jpg and https://b.com/2.png here",
		},
		{
			name:      "duplicate URLs deduplicated",
			text:      "same https://a.com/pic.jpg and again https://a.com/pic.jpg",
			wantUrls:  []string{"https://a.com/pic.jpg"},
			wantClean: "same https://a.com/pic.jpg and again https://a.com/pic.jpg",
		},
		{
			name:      "case insensitive http",
			text:      "HTTP://example.com/pic.jpg",
			wantUrls:  []string{"HTTP://example.com/pic.jpg"},
			wantClean: "HTTP://example.com/pic.jpg",
		},
		{
			name:      "jpeg extension",
			text:      "see https://a.com/pic.jpeg",
			wantUrls:  []string{"https://a.com/pic.jpeg"},
			wantClean: "see https://a.com/pic.jpeg",
		},
		{
			name:      "URL with query params",
			text:      "see https://a.com/pic.jpg?w=100&h=200",
			wantUrls:  []string{"https://a.com/pic.jpg?w=100&h=200"},
			wantClean: "see https://a.com/pic.jpg?w=100&h=200",
		},
		{
			name:      "empty string",
			text:      "",
			wantUrls:  nil,
			wantClean: "",
		},
		{
			name:      "only URL",
			text:      "https://a.com/pic.jpg",
			wantUrls:  []string{"https://a.com/pic.jpg"},
			wantClean: "https://a.com/pic.jpg",
		},
		{
			name:      "jpeg capitalized",
			text:      "see https://a.com/pic.JPEG",
			wantUrls:  []string{"https://a.com/pic.JPEG"},
			wantClean: "see https://a.com/pic.JPEG",
		},
		{
			name:      "non-image URL detected",
			text:      "visit https://example.com/page.html",
			wantUrls:  []string{"https://example.com/page.html"},
			wantClean: "visit https://example.com/page.html",
		},
		{
			name:      "text file URL detected",
			text:      "read https://example.com/file.txt",
			wantUrls:  []string{"https://example.com/file.txt"},
			wantClean: "read https://example.com/file.txt",
		},
		{
			name:      "multiple spaces preserved",
			text:      "see   https://a.com/pic.jpg   now",
			wantUrls:  []string{"https://a.com/pic.jpg"},
			wantClean: "see   https://a.com/pic.jpg   now",
		},
		{
			name:      "real world twitter image URL with query params",
			text:      "check this https://pbs.twimg.com/media/HF4OV9JWsAAAAbX?format=jpg&name=900x900",
			wantUrls:  []string{"https://pbs.twimg.com/media/HF4OV9JWsAAAAbX?format=jpg&name=900x900"},
			wantClean: "check this https://pbs.twimg.com/media/HF4OV9JWsAAAAbX?format=jpg&name=900x900",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clean, urls := detectImageURLs(tt.text)
			assert.Equal(t, tt.wantClean, clean, "detectImageURLs() clean")
			if assert.Len(t, urls, len(tt.wantUrls), "detectImageURLs() urls count") {
				for i, u := range urls {
					assert.Equal(t, tt.wantUrls[i], u, "detectImageURLs() urls[%d]", i)
				}
			}
		})
	}
}

func TestSanitizeMessages(t *testing.T) {
	tests := []struct {
		name      string
		messages  []ChatMessage
		wantClean []ChatMessage
	}{
		{
			name:      "empty messages",
			messages:  []ChatMessage{},
			wantClean: []ChatMessage{},
		},
		{
			name: "message without multi content",
			messages: []ChatMessage{
				{Role: RoleUser, Content: "hello"},
			},
			wantClean: []ChatMessage{
				{Role: RoleUser, Content: "hello"},
			},
		},
		{
			name: "message with text part only",
			messages: []ChatMessage{
				{
					Role:         RoleUser,
					MultiContent: []MessagePart{{Type: PartTypeText, Text: "hello"}},
				},
			},
			wantClean: []ChatMessage{
				{
					Role:         RoleUser,
					MultiContent: []MessagePart{{Type: PartTypeText, Text: "hello"}},
				},
			},
		},
		{
			name: "message with image URL without base64",
			messages: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL:    "https://example.com/image.jpg",
							Detail: ImageDetailAuto,
						}},
					},
				},
			},
			wantClean: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL:    "https://example.com/image.jpg",
							Detail: ImageDetailAuto,
						}},
					},
				},
			},
		},
		{
			name: "message with base64 data URL truncated",
			messages: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL:    "data:image/jpeg;base64,/9j/4AAQSkZJRg==",
							Detail: ImageDetailAuto,
						}},
					},
				},
			},
			wantClean: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL:    "data:image/jpeg;base64,...[truncated]",
							Detail: ImageDetailAuto,
						}},
					},
				},
			},
		},
		{
			name: "message with webp base64 data URL",
			messages: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL:    "data:image/webp;base64,abcdef",
							Detail: ImageDetailLow,
						}},
					},
				},
			},
			wantClean: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL:    "data:image/webp;base64,...[truncated]",
							Detail: ImageDetailLow,
						}},
					},
				},
			},
		},
		{
			name: "multiple messages with mixed content",
			messages: []ChatMessage{
				{Role: RoleUser, Content: "hello"},
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeText, Text: "what is this"},
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL: "data:image/png;base64,abc123",
						}},
					},
				},
			},
			wantClean: []ChatMessage{
				{Role: RoleUser, Content: "hello"},
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeText, Text: "what is this"},
						{Type: PartTypeImageURL, ImageURL: &ImageURL{
							URL: "data:image/png;base64,...[truncated]",
						}},
					},
				},
			},
		},
		{
			name: "nil ImageURL pointer preserved",
			messages: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: nil},
					},
				},
			},
			wantClean: []ChatMessage{
				{
					Role: RoleUser,
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL, ImageURL: nil},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeMessages(tt.messages)
			if !assert.Len(t, got, len(tt.wantClean), "sanitizeMessages() message count") {
				return
			}
			for i := range got {
				if !assert.Len(t, got[i].MultiContent, len(tt.wantClean[i].MultiContent), "sanitizeMessages()[%d] MultiContent count", i) {
					continue
				}
				for j := range got[i].MultiContent {
					gotPart := got[i].MultiContent[j]
					wantPart := tt.wantClean[i].MultiContent[j]
					assert.Equal(t, wantPart.Type, gotPart.Type, "sanitizeMessages()[%d].MultiContent[%d].Type", i, j)
					assert.Equal(t, wantPart.Text, gotPart.Text, "sanitizeMessages()[%d].MultiContent[%d].Text", i, j)
					if gotPart.ImageURL == nil && wantPart.ImageURL == nil {
						continue
					}
					if assert.NotNil(t, gotPart.ImageURL, "sanitizeMessages()[%d].MultiContent[%d].ImageURL", i, j) &&
						assert.NotNil(t, wantPart.ImageURL, "sanitizeMessages()[%d].MultiContent[%d].ImageURL", i, j) {
						assert.Equal(t, wantPart.ImageURL.URL, gotPart.ImageURL.URL, "sanitizeMessages()[%d].MultiContent[%d].ImageURL.URL", i, j)
						assert.Equal(t, wantPart.ImageURL.Detail, gotPart.ImageURL.Detail, "sanitizeMessages()[%d].MultiContent[%d].ImageURL.Detail", i, j)
					}
				}
			}
		})
	}
}

func TestCountContextImages(t *testing.T) {
	tests := []struct {
		name     string
		messages []ChatMessage
		want     int
	}{
		{
			name:     "empty messages",
			messages: []ChatMessage{},
			want:     0,
		},
		{
			name: "message without multi content",
			messages: []ChatMessage{
				{Role: RoleUser, Content: "hello"},
			},
			want: 0,
		},
		{
			name: "message with text only",
			messages: []ChatMessage{
				{
					Role:         RoleUser,
					MultiContent: []MessagePart{{Type: PartTypeText}},
				},
			},
			want: 0,
		},
		{
			name: "message with one image",
			messages: []ChatMessage{
				{
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL},
					},
				},
			},
			want: 1,
		},
		{
			name: "message with multiple images",
			messages: []ChatMessage{
				{
					MultiContent: []MessagePart{
						{Type: PartTypeText},
						{Type: PartTypeImageURL},
						{Type: PartTypeImageURL},
					},
				},
			},
			want: 2,
		},
		{
			name: "multiple messages with images",
			messages: []ChatMessage{
				{
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL},
					},
				},
				{
					MultiContent: []MessagePart{
						{Type: PartTypeImageURL},
						{Type: PartTypeImageURL},
					},
				},
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countContextImages(tt.messages)
			assert.Equal(t, tt.want, got, "countContextImages()")
		})
	}
}

func TestConvertImage(t *testing.T) {
	createTestImage := func(width, height int) []byte {
		img := image.NewRGBA(image.Rect(0, 0, width, height))
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				img.Pix[(y*width+x)*4+0] = 255
				img.Pix[(y*width+x)*4+1] = 0
				img.Pix[(y*width+x)*4+2] = 0
				img.Pix[(y*width+x)*4+3] = 255
			}
		}
		var buf bytes.Buffer
		jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90})
		return buf.Bytes()
	}

	tests := []struct {
		name        string
		imgData     []byte
		mimeType    string
		format      string
		quality     int
		maxW        int
		maxH        int
		wantContain string
	}{
		{
			name:        "encode jpg format",
			imgData:     createTestImage(100, 100),
			mimeType:    "image/jpeg",
			format:      "jpg",
			quality:     75,
			wantContain: "data:image/jpeg;base64,",
		},
		{
			name:        "encode webp format",
			imgData:     createTestImage(100, 100),
			mimeType:    "image/jpeg",
			format:      "webp",
			quality:     75,
			wantContain: "data:image/webp;base64,",
		},
		{
			name:     "scale down 2000x2000 to 1024x1024",
			imgData:  createTestImage(2000, 2000),
			mimeType: "image/jpeg",
			format:   "jpg",
			quality:  75,
			maxW:     1024,
			maxH:     1024,
		},
		{
			name:     "scale 2000x1000 to 1024x512",
			imgData:  createTestImage(2000, 1000),
			mimeType: "image/jpeg",
			format:   "jpg",
			quality:  75,
			maxW:     1024,
			maxH:     1024,
		},
		{
			name:     "no scale small image",
			imgData:  createTestImage(500, 500),
			mimeType: "image/jpeg",
			format:   "jpg",
			quality:  75,
			maxW:     1024,
			maxH:     1024,
		},
		{
			name:     "scale to 1024x768",
			imgData:  createTestImage(2000, 1500),
			mimeType: "image/jpeg",
			format:   "jpg",
			quality:  75,
			maxW:     1024,
			maxH:     768,
		},
		{
			name:     "quality affects output",
			imgData:  createTestImage(100, 100),
			mimeType: "image/jpeg",
			format:   "jpg",
			quality:  50,
		},
		{
			name:        "jpeg format alias",
			imgData:     createTestImage(100, 100),
			mimeType:    "image/jpeg",
			format:      "jpeg",
			quality:     75,
			wantContain: "data:image/jpeg;base64,",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, dataURI, err := convertImage(tt.imgData, tt.mimeType, tt.format, tt.quality, tt.maxW, tt.maxH, 0)
			require.NoError(t, err, "convertImage() error")
			if tt.wantContain != "" {
				assert.Contains(t, dataURI, tt.wantContain, "dataURI")
			}
			assert.NotEmpty(t, data, "encoded data")
		})
	}
}

func TestStripSuccessfulURLs(t *testing.T) {
	tests := []struct {
		name           string
		text           string
		successfulURLs []string
		want           string
	}{
		{
			name:           "no URLs to strip",
			text:           "hello world",
			successfulURLs: nil,
			want:           "hello world",
		},
		{
			name:           "strip single URL",
			text:           "see https://a.com/pic.jpg now",
			successfulURLs: []string{"https://a.com/pic.jpg"},
			want:           "see now",
		},
		{
			name:           "strip URL with trailing comma",
			text:           "look at https://a.com/pic.jpg, it's cool",
			successfulURLs: []string{"https://a.com/pic.jpg"},
			want:           "look at it's cool",
		},
		{
			name:           "strip URL with trailing semicolon",
			text:           "check https://a.com/img.png; and this",
			successfulURLs: []string{"https://a.com/img.png"},
			want:           "check and this",
		},
		{
			name:           "strip URL with trailing paren",
			text:           "(see https://a.com/img.jpg)",
			successfulURLs: []string{"https://a.com/img.jpg"},
			want:           "(see",
		},
		{
			name:           "strip multiple URLs",
			text:           "images https://a.com/1.jpg and https://b.com/2.png here",
			successfulURLs: []string{"https://a.com/1.jpg", "https://b.com/2.png"},
			want:           "images and here",
		},
		{
			name:           "strip one leave one",
			text:           "see https://a.com/page.html and https://b.com/pic.jpg here",
			successfulURLs: []string{"https://b.com/pic.jpg"},
			want:           "see https://a.com/page.html and here",
		},
		{
			name:           "URL not in text is no-op",
			text:           "no urls here",
			successfulURLs: []string{"https://ghost.com/img.jpg"},
			want:           "no urls here",
		},
		{
			name:           "URL with colon stripped",
			text:           "https://a.com/pic.png: extra",
			successfulURLs: []string{"https://a.com/pic.png"},
			want:           "extra",
		},
		{
			name:           "only URL stripped",
			text:           "https://a.com/pic.jpg",
			successfulURLs: []string{"https://a.com/pic.jpg"},
			want:           "",
		},
		{
			name:           "multiple spaces collapsed after strip",
			text:           "see   https://a.com/pic.jpg   now",
			successfulURLs: []string{"https://a.com/pic.jpg"},
			want:           "see now",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripSuccessfulURLs(tt.text, tt.successfulURLs)
			assert.Equal(t, tt.want, got, "stripSuccessfulURLs()")
		})
	}
}

func createTestJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75}), "createTestJPEG")
	return buf.Bytes()
}

func TestBuildImageMessageNonImageURLPreserved(t *testing.T) {
	origClient := imageHTTPClient
	imageHTTPClient = &http.Client{Timeout: 30 * time.Second}
	defer func() { imageHTTPClient = origClient }()

	jpegData := createTestJPEG(t)

	imgServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpegData)
	}))
	defer imgServer.Close()

	htmlServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html>not an image</html>"))
	}))
	defer htmlServer.Close()

	t.Run("non-image URL preserved in text", func(t *testing.T) {
		text := "check " + htmlServer.URL + "/page.html and " + imgServer.URL + "/pic.jpg here"
		urls := []string{htmlServer.URL + "/page.html", imgServer.URL + "/pic.jpg"}

		msg, err := buildImageMessage(text, urls, 5, "jpg", 75, 1024, 1024, 0)
		require.NoError(t, err, "buildImageMessage() error")

		require.GreaterOrEqual(t, len(msg.MultiContent), 2, "expected at least 2 parts")

		var textPart string
		var imageCount int
		for _, part := range msg.MultiContent {
			if part.Type == PartTypeText {
				textPart = part.Text
			}
			if part.Type == PartTypeImageURL {
				imageCount++
			}
		}

		assert.Equal(t, 1, imageCount, "image part count")

		assert.Contains(t, textPart, htmlServer.URL+"/page.html", "non-image URL should be in text")
		assert.NotContains(t, textPart, imgServer.URL+"/pic.jpg", "image URL should have been stripped from text")
	})

	t.Run("all non-image URLs preserved", func(t *testing.T) {
		text := "visit " + htmlServer.URL + "/a and " + htmlServer.URL + "/b"
		urls := []string{htmlServer.URL + "/a", htmlServer.URL + "/b"}

		msg, err := buildImageMessage(text, urls, 5, "jpg", 75, 1024, 1024, 0)
		require.NoError(t, err, "buildImageMessage() error")

		var textPart string
		var imageCount int
		for _, part := range msg.MultiContent {
			if part.Type == PartTypeText {
				textPart = part.Text
			}
			if part.Type == PartTypeImageURL {
				imageCount++
			}
		}

		assert.Equal(t, 0, imageCount, "image part count")

		assert.Contains(t, textPart, htmlServer.URL+"/a", "first non-image URL should be in text")
		assert.Contains(t, textPart, htmlServer.URL+"/b", "second non-image URL should be in text")
	})

	t.Run("successful image URL stripped from text", func(t *testing.T) {
		text := "see " + imgServer.URL + "/pic.jpg now"
		urls := []string{imgServer.URL + "/pic.jpg"}

		msg, err := buildImageMessage(text, urls, 5, "jpg", 75, 1024, 1024, 0)
		require.NoError(t, err, "buildImageMessage() error")

		var textPart string
		for _, part := range msg.MultiContent {
			if part.Type == PartTypeText {
				textPart = part.Text
			}
		}

		assert.NotContains(t, textPart, imgServer.URL, "image URL should have been stripped from text")
		assert.Contains(t, textPart, "see", "surrounding text 'see' should be preserved")
		assert.Contains(t, textPart, "now", "surrounding text 'now' should be preserved")
	})
}

// patchPNGDimensions rewrites the IHDR width/height fields of a PNG and
// recomputes the chunk CRC so the header still parses. Width and height are
// big-endian uint32 at byte offsets 16/20; the CRC covers bytes 12..28 and is
// stored big-endian at 29..32.
func patchPNGDimensions(t *testing.T, pngData []byte, width, height uint32) []byte {
	t.Helper()
	out := make([]byte, len(pngData))
	copy(out, pngData)
	require.GreaterOrEqual(t, len(out), 33, "PNG must contain a full IHDR chunk")
	require.Equal(t, "IHDR", string(out[12:16]), "first chunk must be IHDR")
	binary.BigEndian.PutUint32(out[16:20], width)
	binary.BigEndian.PutUint32(out[20:24], height)
	binary.BigEndian.PutUint32(out[29:33], crc32.ChecksumIEEE(out[12:29]))
	return out
}

func TestConvertImageRejectsPixelBombPNG(t *testing.T) {
	var buf bytes.Buffer
	base := image.NewRGBA(image.Rect(0, 0, 4, 4))
	require.NoError(t, png.Encode(&buf, base), "encode base png")
	bomb := patchPNGDimensions(t, buf.Bytes(), 60000, 60000)

	// Sanity: the patched header must still parse and report the lie.
	cfg, err := png.DecodeConfig(bytes.NewReader(bomb))
	require.NoError(t, err, "patched IHDR must parse")
	require.Equal(t, 60000, cfg.Width, "claimed width")
	require.Equal(t, 60000, cfg.Height, "claimed height")

	start := time.Now()
	data, _, err := convertImage(bomb, "image/png", "jpg", 75, 1024, 1024, 0)
	elapsed := time.Since(start)
	t.Logf("pixel bomb skip took %s", elapsed)

	require.Error(t, err, "convertImage must reject the pixel bomb")
	require.ErrorIs(t, err, errImageTooManyPixels, "error must be the pixel-cap sentinel")
	assert.Empty(t, data, "no output data on skip")
	assert.Less(t, elapsed, time.Second, "skip path must return without decoding pixel data")
}

func createMultiFrameGIF(t *testing.T, frames, width, height int) []byte {
	t.Helper()
	imgs := make([]*image.Paletted, frames)
	for i := range imgs {
		p := image.NewPaletted(image.Rect(0, 0, width, height), palette.Plan9)
		c1 := uint8(i % 2)
		c2 := uint8(1 - i%2)
		for y := 0; y < height; y++ {
			for x := 0; x < width; x++ {
				if (x/8+y/8)%2 == 0 {
					p.SetColorIndex(x, y, c1)
				} else {
					p.SetColorIndex(x, y, c2)
				}
			}
		}
		imgs[i] = p
	}
	var buf bytes.Buffer
	g := &gif.GIF{
		Image:  imgs,
		Delay:  make([]int, frames),
		Config: image.Config{ColorModel: color.Palette(palette.Plan9), Width: width, Height: height},
	}
	require.NoError(t, gif.EncodeAll(&buf, g), "encode multi-frame gif")
	return buf.Bytes()
}

func TestConvertImageRejectsMultiFrameGIF(t *testing.T) {
	// 10 frames of 1600x1600 = 25.6M total pixels, each frame under the cap.
	anim := createMultiFrameGIF(t, 10, 1600, 1600)

	start := time.Now()
	data, _, err := convertImage(anim, "image/gif", "jpg", 75, 1024, 1024, 25_000_000)
	elapsed := time.Since(start)
	t.Logf("animated gif skip took %s", elapsed)

	require.Error(t, err, "convertImage must reject the frame-multiplied pixel bomb")
	require.ErrorIs(t, err, errImageTooManyPixels, "error must be the pixel-cap sentinel")
	assert.Empty(t, data, "no output data on skip")
	assert.Less(t, elapsed, 2*time.Second, "skip path must return without decoding frames")
}

func TestConvertImageAllowsSingleFrameGIFUnderCap(t *testing.T) {
	anim := createMultiFrameGIF(t, 1, 1600, 1600)

	data, dataURI, err := convertImage(anim, "image/gif", "jpg", 75, 1024, 1024, 0)
	require.NoError(t, err, "single-frame GIF under the cap must convert")
	assert.Contains(t, dataURI, "data:image/jpeg;base64,", "dataURI")
	assert.NotEmpty(t, data, "encoded data")
}

func TestConvertImageAllowsLargeImageUnderCap(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 4000, 4000))
	for y := 0; y < 4000; y++ {
		for x := 0; x < 4000; x++ {
			img.Pix[y*4000+x] = uint8((x + y) % 256)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img), "encode 4000x4000 png")

	data, dataURI, err := convertImage(buf.Bytes(), "image/png", "jpg", 75, 1024, 1024, 0)
	require.NoError(t, err, "16MP image is under the 50MP default cap and must convert")

	assert.Contains(t, dataURI, "data:image/jpeg;base64,", "dataURI")
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	require.NoError(t, err, "result must be a valid jpeg")
	assert.Equal(t, 1024, decoded.Bounds().Dx(), "resized width")
	assert.Equal(t, 1024, decoded.Bounds().Dy(), "resized height")
}

func TestConvertImageConfigurablePixelCap(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 4000, 4000))
	for y := 0; y < 4000; y++ {
		for x := 0; x < 4000; x++ {
			img.Pix[y*4000+x] = uint8((x + y) % 256)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img), "encode 4000x4000 png")

	_, _, err := convertImage(buf.Bytes(), "image/png", "jpg", 75, 1024, 1024, 10_000_000)
	require.ErrorIs(t, err, errImageTooManyPixels, "16MP must exceed an explicit 10MP cap")

	data, _, err := convertImage(buf.Bytes(), "image/png", "jpg", 75, 1024, 1024, 20_000_000)
	require.NoError(t, err, "16MP must pass an explicit 20MP cap")
	assert.NotEmpty(t, data, "encoded data")
}

func TestConvertImageDefaultPixelCapIs50MP(t *testing.T) {
	// 0 resolves to the default cap: 25.6MP passes, a 64MP claim is rejected.
	anim := createMultiFrameGIF(t, 10, 1600, 1600) // 25.6MP total
	data, _, err := convertImage(anim, "image/gif", "jpg", 75, 1024, 1024, 0)
	require.NoError(t, err, "25.6MP must be under the 50MP default cap")
	assert.NotEmpty(t, data, "encoded data")

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))), "encode base png")
	bomb := patchPNGDimensions(t, buf.Bytes(), 8000, 8000) // claims 64MP

	start := time.Now()
	_, _, err = convertImage(bomb, "image/png", "jpg", 75, 1024, 1024, 0)
	elapsed := time.Since(start)
	t.Logf("64MP claim vs default cap took %s", elapsed)

	require.ErrorIs(t, err, errImageTooManyPixels, "64MP must exceed the 50MP default cap")
	assert.Less(t, elapsed, time.Second, "skip path must return without decoding pixel data")
}

func TestCountGIFFrames(t *testing.T) {
	tests := []struct {
		name   string
		frames int
		width  int
		height int
		want   int
	}{
		{name: "single frame", frames: 1, width: 8, height: 8, want: 1},
		{name: "three frames", frames: 3, width: 8, height: 8, want: 3},
		{name: "ten frames", frames: 10, width: 4, height: 4, want: 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := createMultiFrameGIF(t, tt.frames, tt.width, tt.height)
			got, err := countGIFFrames(bytes.NewReader(data))
			require.NoError(t, err, "countGIFFrames() error")
			assert.Equal(t, tt.want, got, "countGIFFrames()")
		})
	}
}

func TestBuildImageMessageSkipsPixelBombNonFatal(t *testing.T) {
	origClient := imageHTTPClient
	imageHTTPClient = &http.Client{Timeout: 30 * time.Second}
	defer func() { imageHTTPClient = origClient }()

	var buf bytes.Buffer
	base := image.NewRGBA(image.Rect(0, 0, 4, 4))
	require.NoError(t, png.Encode(&buf, base), "encode base png")
	bomb := patchPNGDimensions(t, buf.Bytes(), 60000, 60000)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(bomb)
	}))
	defer server.Close()

	text := "see " + server.URL + "/bomb.png now"
	msg, err := buildImageMessage(text, []string{server.URL + "/bomb.png"}, 5, "jpg", 75, 1024, 1024, 0)
	require.NoError(t, err, "pixel bomb skip must be non-fatal")

	var imageCount int
	var textPart string
	for _, part := range msg.MultiContent {
		if part.Type == PartTypeImageURL {
			imageCount++
		}
		if part.Type == PartTypeText {
			textPart = part.Text
		}
	}
	assert.Equal(t, 0, imageCount, "bomb must not become an image part")
	assert.Contains(t, textPart, server.URL, "skipped URL stays in text")
	assert.Contains(t, textPart, "see", "surrounding text preserved")
	assert.Contains(t, textPart, "now", "surrounding text preserved")
}

func TestDownloadImageChunkedBodyBounded(t *testing.T) {
	origClient := imageHTTPClient
	imageHTTPClient = &http.Client{Timeout: 5 * time.Second}
	defer func() { imageHTTPClient = origClient }()

	chunk := make([]byte, 1024*1024)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		// No Content-Length set: Go uses chunked transfer encoding.
		for {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	start := time.Now()
	_, _, err := downloadImage(server.URL)
	elapsed := time.Since(start)
	t.Logf("chunked oversize download rejected in %s", elapsed)

	require.Error(t, err, "oversized chunked body must be rejected")
	assert.Contains(t, err.Error(), "too large", "rejection must come from the size cap, not the timeout: %v", err)
	assert.Less(t, elapsed, 4*time.Second, "bounded read must not run into the client timeout")
}

// craftBMPHeader builds a 62-byte 1bpp BMP: file header (14) +
// BITMAPINFOHEADER (40) + default 2-entry palette (8), with NO pixel data.
// bmp.DecodeConfig parses it and reports the claimed dimensions; bmp.Decode
// would allocate the full image from those dimensions before hitting EOF.
func craftBMPHeader(width, height int) []byte {
	offset := 14 + 40 + 2*4 // file header + DIB + palette
	buf := make([]byte, offset)
	copy(buf[0:2], "BM")
	binary.LittleEndian.PutUint32(buf[2:6], uint32(offset)) // file size (not validated)
	binary.LittleEndian.PutUint32(buf[10:14], uint32(offset))
	binary.LittleEndian.PutUint32(buf[14:18], 40) // BITMAPINFOHEADER
	binary.LittleEndian.PutUint32(buf[18:22], uint32(width))
	binary.LittleEndian.PutUint32(buf[22:26], uint32(height))
	binary.LittleEndian.PutUint16(buf[26:28], 1) // planes
	binary.LittleEndian.PutUint16(buf[28:30], 1) // bits per pixel
	// compression [30:34] = 0 (BI_RGB), colorUsed [46:50] = 0 (defaults to
	// 2 for 1bpp), palette entries left as zero (black). All other fields zero.
	return buf
}

func TestConvertImageRejectsPixelBombBMP(t *testing.T) {
	bomb := craftBMPHeader(6000, 6000)

	// Sanity: the crafted header must parse and report the claim.
	cfg, err := bmp.DecodeConfig(bytes.NewReader(bomb))
	require.NoError(t, err, "crafted BMP header must parse")
	require.Equal(t, 6000, cfg.Width, "claimed width")
	require.Equal(t, 6000, cfg.Height, "claimed height")

	start := time.Now()
	data, _, err := convertImage(bomb, "image/bmp", "jpg", 75, 1024, 1024, 25_000_000)
	elapsed := time.Since(start)
	t.Logf("bmp bomb skip took %s", elapsed)

	require.Error(t, err, "convertImage must reject the BMP pixel bomb")
	require.ErrorIs(t, err, errImageTooManyPixels, "error must be the pixel-cap sentinel")
	assert.Empty(t, data, "no output data on skip")
	assert.Less(t, elapsed, time.Second, "skip path must return without decoding pixel data")
}

func TestCountGIFFramesMalformed(t *testing.T) {
	// "GIF89a" + logical screen descriptor (w=1, h=1, packed=0: no GCT).
	validPrefix := []byte{'G', 'I', 'F', '8', '9', 'a', 1, 0, 1, 0, 0, 0, 0}

	tests := []struct {
		name  string
		input []byte
	}{
		{
			name:  "bad signature",
			input: append([]byte("GIF88a"), validPrefix[6:]...),
		},
		{
			name:  "truncated after logical screen descriptor",
			input: validPrefix,
		},
		{
			name:  "invalid block introducer",
			input: append(append([]byte{}, validPrefix...), 0xFF),
		},
		{
			name:  "truncated inside image descriptor",
			input: append(append([]byte{}, validPrefix...), 0x2C, 0x00, 0x00, 0x00),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := countGIFFrames(bytes.NewReader(tt.input))
			require.Error(t, err, "malformed GIF must produce an error, not a frame count")
		})
	}
}

func TestHeaderDimensionsClampsMalformedGIFFrames(t *testing.T) {
	// Valid header (so gif.DecodeConfig succeeds) followed by an invalid
	// block introducer (so the frame walker fails): the frame count must
	// clamp to 1, not zero — a zero multiplier would bypass the cap.
	input := append([]byte{'G', 'I', 'F', '8', '9', 'a', 1, 0, 1, 0, 0, 0, 0}, 0xFF)

	w, h, frames, ok := headerDimensions(input, "image/gif")
	require.True(t, ok, "valid GIF header must still pre-check")
	assert.Equal(t, 1, w, "width")
	assert.Equal(t, 1, h, "height")
	assert.Equal(t, 1, frames, "frame count must clamp to at least 1")
}
