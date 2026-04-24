package main

import (
	"bytes"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gogpt "github.com/sashabaranov/go-openai"
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
			if got != tt.want {
				t.Errorf("formatSize(%d) = %q, want %q", tt.b, got, tt.want)
			}
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
			if clean != tt.wantClean {
				t.Errorf("detectImageURLs() clean = %q, want %q", clean, tt.wantClean)
			}
			if len(urls) != len(tt.wantUrls) {
				t.Errorf("detectImageURLs() urls count = %d, want %d\ngot: %v\nwant: %v",
					len(urls), len(tt.wantUrls), urls, tt.wantUrls)
				return
			}
			for i, u := range urls {
				if u != tt.wantUrls[i] {
					t.Errorf("detectImageURLs() urls[%d] = %q, want %q", i, u, tt.wantUrls[i])
				}
			}
		})
	}
}

func TestSanitizeMessages(t *testing.T) {
	tests := []struct {
		name      string
		messages  []gogpt.ChatCompletionMessage
		wantClean []gogpt.ChatCompletionMessage
	}{
		{
			name:      "empty messages",
			messages:  []gogpt.ChatCompletionMessage{},
			wantClean: []gogpt.ChatCompletionMessage{},
		},
		{
			name: "message without multi content",
			messages: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleUser, Content: "hello"},
			},
			wantClean: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleUser, Content: "hello"},
			},
		},
		{
			name: "message with text part only",
			messages: []gogpt.ChatCompletionMessage{
				{
					Role:         gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{{Type: gogpt.ChatMessagePartTypeText, Text: "hello"}},
				},
			},
			wantClean: []gogpt.ChatCompletionMessage{
				{
					Role:         gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{{Type: gogpt.ChatMessagePartTypeText, Text: "hello"}},
				},
			},
		},
		{
			name: "message with image URL without base64",
			messages: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
							URL:    "https://example.com/image.jpg",
							Detail: gogpt.ImageURLDetailAuto,
						}},
					},
				},
			},
			wantClean: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
							URL:    "https://example.com/image.jpg",
							Detail: gogpt.ImageURLDetailAuto,
						}},
					},
				},
			},
		},
		{
			name: "message with base64 data URL truncated",
			messages: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
							URL:    "data:image/jpeg;base64,/9j/4AAQSkZJRg==",
							Detail: gogpt.ImageURLDetailAuto,
						}},
					},
				},
			},
			wantClean: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
							URL:    "data:image/jpeg;base64,...[truncated]",
							Detail: gogpt.ImageURLDetailAuto,
						}},
					},
				},
			},
		},
		{
			name: "message with webp base64 data URL",
			messages: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
							URL:    "data:image/webp;base64,abcdef",
							Detail: gogpt.ImageURLDetailLow,
						}},
					},
				},
			},
			wantClean: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
							URL:    "data:image/webp;base64,...[truncated]",
							Detail: gogpt.ImageURLDetailLow,
						}},
					},
				},
			},
		},
		{
			name: "multiple messages with mixed content",
			messages: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleUser, Content: "hello"},
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeText, Text: "what is this"},
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
							URL: "data:image/png;base64,abc123",
						}},
					},
				},
			},
			wantClean: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleUser, Content: "hello"},
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeText, Text: "what is this"},
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: &gogpt.ChatMessageImageURL{
							URL: "data:image/png;base64,...[truncated]",
						}},
					},
				},
			},
		},
		{
			name: "nil ImageURL pointer preserved",
			messages: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: nil},
					},
				},
			},
			wantClean: []gogpt.ChatCompletionMessage{
				{
					Role: gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL, ImageURL: nil},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeMessages(tt.messages)
			if len(got) != len(tt.wantClean) {
				t.Errorf("sanitizeMessages() returned %d messages, want %d", len(got), len(tt.wantClean))
				return
			}
			for i := range got {
				if len(got[i].MultiContent) != len(tt.wantClean[i].MultiContent) {
					t.Errorf("sanitizeMessages()[%d] MultiContent count = %d, want %d",
						i, len(got[i].MultiContent), len(tt.wantClean[i].MultiContent))
					continue
				}
				for j := range got[i].MultiContent {
					gotPart := got[i].MultiContent[j]
					wantPart := tt.wantClean[i].MultiContent[j]
					if gotPart.Type != wantPart.Type {
						t.Errorf("sanitizeMessages()[%d].MultiContent[%d].Type = %v, want %v",
							i, j, gotPart.Type, wantPart.Type)
					}
					if gotPart.Text != wantPart.Text {
						t.Errorf("sanitizeMessages()[%d].MultiContent[%d].Text = %q, want %q",
							i, j, gotPart.Text, wantPart.Text)
					}
					if gotPart.ImageURL == nil && wantPart.ImageURL == nil {
						continue
					}
					if gotPart.ImageURL == nil || wantPart.ImageURL == nil {
						t.Errorf("sanitizeMessages()[%d].MultiContent[%d].ImageURL mismatch", i, j)
						continue
					}
					if gotPart.ImageURL.URL != wantPart.ImageURL.URL {
						t.Errorf("sanitizeMessages()[%d].MultiContent[%d].ImageURL.URL = %q, want %q",
							i, j, gotPart.ImageURL.URL, wantPart.ImageURL.URL)
					}
					if gotPart.ImageURL.Detail != wantPart.ImageURL.Detail {
						t.Errorf("sanitizeMessages()[%d].MultiContent[%d].ImageURL.Detail = %v, want %v",
							i, j, gotPart.ImageURL.Detail, wantPart.ImageURL.Detail)
					}
				}
			}
		})
	}
}

func TestCountContextImages(t *testing.T) {
	tests := []struct {
		name     string
		messages []gogpt.ChatCompletionMessage
		want     int
	}{
		{
			name:     "empty messages",
			messages: []gogpt.ChatCompletionMessage{},
			want:     0,
		},
		{
			name: "message without multi content",
			messages: []gogpt.ChatCompletionMessage{
				{Role: gogpt.ChatMessageRoleUser, Content: "hello"},
			},
			want: 0,
		},
		{
			name: "message with text only",
			messages: []gogpt.ChatCompletionMessage{
				{
					Role:         gogpt.ChatMessageRoleUser,
					MultiContent: []gogpt.ChatMessagePart{{Type: gogpt.ChatMessagePartTypeText}},
				},
			},
			want: 0,
		},
		{
			name: "message with one image",
			messages: []gogpt.ChatCompletionMessage{
				{
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL},
					},
				},
			},
			want: 1,
		},
		{
			name: "message with multiple images",
			messages: []gogpt.ChatCompletionMessage{
				{
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeText},
						{Type: gogpt.ChatMessagePartTypeImageURL},
						{Type: gogpt.ChatMessagePartTypeImageURL},
					},
				},
			},
			want: 2,
		},
		{
			name: "multiple messages with images",
			messages: []gogpt.ChatCompletionMessage{
				{
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL},
					},
				},
				{
					MultiContent: []gogpt.ChatMessagePart{
						{Type: gogpt.ChatMessagePartTypeImageURL},
						{Type: gogpt.ChatMessagePartTypeImageURL},
					},
				},
			},
			want: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countContextImages(tt.messages)
			if got != tt.want {
				t.Errorf("countContextImages() = %d, want %d", got, tt.want)
			}
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
			if err != nil {
				t.Fatalf("convertImage() error = %v", err)
			}
			if tt.wantContain != "" && !containsStr(dataURI, tt.wantContain) {
				t.Errorf("dataURI = %q, want containing %q", dataURI, tt.wantContain)
			}
			if len(data) == 0 {
				t.Error("encoded data is empty")
			}
		})
	}
}

func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || func() bool {
		for i := 0; i <= len(s)-len(substr); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	}())
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
			if got != tt.want {
				t.Errorf("stripSuccessfulURLs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func createTestJPEG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 75}); err != nil {
		t.Fatalf("createTestJPEG: %v", err)
	}
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
		if err != nil {
			t.Fatalf("buildImageMessage() error = %v", err)
		}

		if len(msg.MultiContent) < 2 {
			t.Fatalf("expected at least 2 parts, got %d", len(msg.MultiContent))
		}

		var textPart string
		var imageCount int
		for _, part := range msg.MultiContent {
			if part.Type == gogpt.ChatMessagePartTypeText {
				textPart = part.Text
			}
			if part.Type == gogpt.ChatMessagePartTypeImageURL {
				imageCount++
			}
		}

		if imageCount != 1 {
			t.Errorf("expected 1 image part, got %d", imageCount)
		}

		if !containsStr(textPart, htmlServer.URL+"/page.html") {
			t.Errorf("non-image URL missing from text: %q", textPart)
		}
		if containsStr(textPart, imgServer.URL+"/pic.jpg") {
			t.Errorf("image URL should have been stripped from text: %q", textPart)
		}
	})

	t.Run("all non-image URLs preserved", func(t *testing.T) {
		text := "visit " + htmlServer.URL + "/a and " + htmlServer.URL + "/b"
		urls := []string{htmlServer.URL + "/a", htmlServer.URL + "/b"}

		msg, err := buildImageMessage(text, urls, 5, "jpg", 75, 1024, 1024)
		if err != nil {
			t.Fatalf("buildImageMessage() error = %v", err)
		}

		var textPart string
		var imageCount int
		for _, part := range msg.MultiContent {
			if part.Type == gogpt.ChatMessagePartTypeText {
				textPart = part.Text
			}
			if part.Type == gogpt.ChatMessagePartTypeImageURL {
				imageCount++
			}
		}

		if imageCount != 0 {
			t.Errorf("expected 0 image parts, got %d", imageCount)
		}

		if !containsStr(textPart, htmlServer.URL+"/a") {
			t.Errorf("first non-image URL missing from text: %q", textPart)
		}
		if !containsStr(textPart, htmlServer.URL+"/b") {
			t.Errorf("second non-image URL missing from text: %q", textPart)
		}
	})

	t.Run("successful image URL stripped from text", func(t *testing.T) {
		text := "see " + imgServer.URL + "/pic.jpg now"
		urls := []string{imgServer.URL + "/pic.jpg"}

		msg, err := buildImageMessage(text, urls, 5, "jpg", 75, 1024, 1024)
		if err != nil {
			t.Fatalf("buildImageMessage() error = %v", err)
		}

		var textPart string
		for _, part := range msg.MultiContent {
			if part.Type == gogpt.ChatMessagePartTypeText {
				textPart = part.Text
			}
		}

		if containsStr(textPart, imgServer.URL) {
			t.Errorf("image URL should have been stripped from text: %q", textPart)
		}
		if !containsStr(textPart, "see") || !containsStr(textPart, "now") {
			t.Errorf("surrounding text should be preserved: %q", textPart)
		}
	})
}
