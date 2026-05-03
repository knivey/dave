package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			data, dataURI, err := convertImage(tt.imgData, tt.mimeType, tt.format, tt.quality, tt.maxW, tt.maxH)
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

		msg, err := buildImageMessage(text, urls, 5, "jpg", 75, 1024, 1024)
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

		msg, err := buildImageMessage(text, urls, 5, "jpg", 75, 1024, 1024)
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

		msg, err := buildImageMessage(text, urls, 5, "jpg", 75, 1024, 1024)
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
