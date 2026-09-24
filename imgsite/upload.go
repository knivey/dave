package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const apiKeyHeader = "X-API-Key"

// Public image ids: 7 random base62 chars. ~3.5 trillion space, so
// collisions are a non-event, but every candidate is still checked against
// the DB with a bounded retry loop.
const (
	imageIDLen      = 7
	imageIDAttempts = 5
)

const imageIDAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// UploadMeta is the optional `meta` multipart field sent by img-mcp.
// Unknown JSON fields are ignored (plain Unmarshal behavior) so dave can
// grow the payload without a coordinated deploy. LLMGenerated is a *bool
// so "field absent" (nil) is distinguishable from an explicit false — the
// merge cross-check only fires when meta actually carried the field.
type UploadMeta struct {
	JobID          string `json:"job_id"`
	OriginalPrompt string `json:"original_prompt"`
	EnhancedPrompt string `json:"enhanced_prompt"`
	NegativePrompt string `json:"negative_prompt"`
	Reasoning      string `json:"reasoning"`
	LLMGenerated   *bool  `json:"llm_generated"`
	WorkflowName   string `json:"workflow_name"`
	Network        string `json:"network"`
	Channel        string `json:"channel"`
	Nick           string `json:"nick"`
}

type uploadResponse struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Page     string `json:"page"`
	Filename string `json:"filename"`
}

// checkAPIKey does a constant-time comparison of the presented key against
// the configured one. An empty configured key denies everything: with no
// key configured there must be no way to upload.
func checkAPIKey(expected, provided string) bool {
	if expected == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

// rateLimiter is a simple in-memory token bucket refilled at
// rate_per_minute/60 with capacity rate_per_minute (one full minute of
// budget for bursts — the only client is dave). The bucket is swapped
// whole when the rate is hot-reloaded.
type rateLimiter struct {
	mu  sync.Mutex
	lim *rate.Limiter
}

func newRateLimiter(perMinute int) *rateLimiter {
	return &rateLimiter{lim: rate.NewLimiter(rate.Limit(float64(perMinute)/60.0), perMinute)}
}

func (rl *rateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.lim.Allow()
}

func (rl *rateLimiter) setPerMinute(perMinute int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.lim = rate.NewLimiter(rate.Limit(float64(perMinute)/60.0), perMinute)
}

// handleUpload implements POST /updo. Milestone-1 sequence: auth, rate
// limit, size guard, filename sanitization, sha256 while buffering, dedupe
// write, id generation, INSERT, 201 JSON with the ready-to-paste URL.
// EXIF parsing, thumbnail enqueueing, and the SSE event are later
// milestones; width/height stay NULL until then.
func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	cfg := a.getConfig()

	// 1. Auth (constant-time), rate limit, size guard.
	if !checkAPIKey(cfg.Auth.APIKey, r.Header.Get(apiKeyHeader)) {
		loggerUpload.Warn("upload rejected: bad or missing api key", "remote_addr", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !a.limiter.allow() {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	maxBytes := cfg.Upload.MaxBytes
	if maxBytes < 1 {
		maxBytes = defaultUploadMaxBytes
	}
	if r.ContentLength > maxBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)

	if err := r.ParseMultipartForm(8 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad multipart form: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer r.MultipartForm.RemoveAll()

	// 2. File field + filename sanitization.
	file, hdr, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing or invalid file field", http.StatusBadRequest)
		return
	}
	defer file.Close()
	filename := sanitizeUploadFilename(hdr.Filename)

	// 3. Buffer + sha256 (MaxBytesReader still guards the read).
	data, err := io.ReadAll(file)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "reading file: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(data) == 0 {
		http.Error(w, "empty file", http.StatusBadRequest)
		return
	}

	// Optional meta JSON (unknown fields ignored).
	var meta UploadMeta
	if mv := r.FormValue("meta"); mv != "" {
		if err := json.Unmarshal([]byte(mv), &meta); err != nil {
			http.Error(w, "invalid meta JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	sum := sha256.Sum256(data)
	hashHex := hex.EncodeToString(sum[:])

	// MIME from magic bytes, never the client's claim. Hardened to an
	// image/* allowlist: anything else (e.g. HTML-ish bytes) is stored as
	// application/octet-stream so this origin can never serve text/html.
	// Together with X-Content-Type-Options: nosniff, a non-image upload
	// cannot become a content-sniffing script in a browser, and the orig
	// route still serves the bytes faithfully for download.
	mimeType := http.DetectContentType(data)
	if !isAllowedImageMIME(mimeType) {
		mimeType = "application/octet-stream"
	}

	// EXIF extraction: synchronous and sub-millisecond (a few-KB chunk
	// scan, no decode). Any failure degrades to meta alone — never fails
	// the upload.
	apiPayload, _, _ := extractEmbeddedWorkflows(data)
	var md extractedMetadata
	exifOK := false
	if apiPayload != "" {
		md, exifOK = ExtractMetadata(apiPayload)
		if !exifOK {
			loggerUpload.Warn("embedded workflow present but unparseable; proceeding on meta alone", "sha256", hashHex)
		}
	}
	merged := mergeUploadMetadata(meta, md, exifOK)

	// 4. Dedupe write: the store skips the write when the hash already
	// has a file; a fresh row is still inserted below either way.
	if _, err := a.store.WriteOriginal(hashHex, data); err != nil {
		loggerUpload.Error("store write failed", "sha256", hashHex, "error", err)
		http.Error(w, "storage failure", http.StatusInternalServerError)
		return
	}

	// 5. Fresh id for the new row (dedupe shares the FILE, not the row).
	id, err := generateImageID(func(id string) (bool, error) {
		return dbImageIDExists(a.db, id)
	})
	if err != nil {
		loggerUpload.Error("id generation failed", "error", err)
		http.Error(w, "could not allocate id", http.StatusInternalServerError)
		return
	}

	img := dbImage{
		ID:          id,
		SHA256:      hashHex,
		Filename:    filename,
		MimeType:    mimeType,
		SizeBytes:   int64(len(data)),
		CreatedAt:   time.Now().UTC().Format(dbTimeFormat),
		ThumbStatus: thumbStatusPending,
	}
	applyMergedMetadata(&img, merged)
	if err := dbInsertImage(a.db, &img); err != nil {
		loggerUpload.Error("db insert failed", "id", id, "error", err)
		http.Error(w, "storage failure", http.StatusInternalServerError)
		return
	}

	// SSE image-new goes out only after the INSERT has committed: a
	// subscriber acting on this event (prepend card, fetch neighbors)
	// must already see the row. Nil-hub-safe (tests without the hub).
	a.publishImageNew(&img)

	// Hand off to the background thumbnailer (nil-safe / non-blocking).
	a.enqueueThumb(img.ID)

	// 6. 201 with the verbatim direct link.
	base := strings.TrimRight(cfg.Server.BaseURL, "/")
	if base == "" {
		base = deriveBaseURL(r)
	}
	resp := uploadResponse{
		ID:       id,
		URL:      base + "/" + id + "/orig/" + url.PathEscape(filename),
		Page:     base + "/" + id,
		Filename: filename,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		loggerUpload.Error("writing upload response", "id", id, "error", err)
		return
	}

	loggerUpload.Info("upload complete",
		"id", id,
		"sha256", hashHex[:12],
		"size", len(data),
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// mergedUploadMetadata is the post-merge metadata destined for the row.
type mergedUploadMetadata struct {
	OriginalPrompt string
	EnhancedPrompt string
	NegativePrompt string
	Reasoning      string
	JobID          string
	LLMGenerated   bool

	// Provenance: upload meta wins (EXIF never carries these).
	Network      string
	Channel      string
	Nick         string
	WorkflowName string

	// Graph-derived: EXIF is the source of truth when available.
	Seed         *int64
	Steps        *int
	Cfg          *float64
	Denoise      *float64
	Sampler      string
	Scheduler    string
	ModelUnet    string
	ModelClip    string
	ModelVae     string
	LorasJSON    string
	Width        *int
	Height       *int
	WorkflowJSON string

	MetaSource string
}

// mergeUploadMetadata implements the plan's merge policy:
//
//   - EXIF (the embedded workflow) is the source of truth for
//     graph-derived fields — seed, sampler, models, dims, enhanced and
//     negative prompts, workflow JSON — because it is what the image
//     actually executed with.
//   - Upload meta wins for provenance (network/channel/nick/workflow_name)
//     and is the fallback for prompt fields when EXIF parsing failed.
//   - original_prompt, reasoning, llm_generated, job_id are cross-checked
//     between both sides; a mismatch logs a WARN and prefers EXIF.
//   - meta_source records which side(s) contributed: "exif", "upload",
//     "upload+exif", or "" (neither — byte-only upload).
func mergeUploadMetadata(meta UploadMeta, md extractedMetadata, exifOK bool) mergedUploadMetadata {
	var m mergedUploadMetadata

	// Provenance always comes from meta.
	m.Network = meta.Network
	m.Channel = meta.Channel
	m.Nick = meta.Nick
	m.WorkflowName = meta.WorkflowName

	metaContributed := uploadMetaPresent(meta)

	if exifOK {
		m.OriginalPrompt = md.OriginalPrompt
		m.Reasoning = md.Reasoning
		m.JobID = md.JobID
		m.LLMGenerated = md.LLMGenerated
		m.EnhancedPrompt = md.EnhancedPrompt
		m.NegativePrompt = md.NegativePrompt
		m.Seed, m.Steps, m.Cfg, m.Denoise = md.Seed, md.Steps, md.Cfg, md.Denoise
		m.Sampler, m.Scheduler = md.Sampler, md.Scheduler
		m.ModelUnet, m.ModelClip, m.ModelVae = md.ModelUnet, md.ModelClip, md.ModelVae
		m.LorasJSON = md.LorasJSON
		m.Width, m.Height = md.Width, md.Height
		m.WorkflowJSON = md.WorkflowJSON

		// Cross-check quartet: WARN + prefer EXIF on mismatch. Only fields
		// meta actually carried are compared (strings by non-empty,
		// llm_generated by non-nil pointer).
		if meta.OriginalPrompt != "" && meta.OriginalPrompt != md.OriginalPrompt {
			loggerUpload.Warn("meta/exif mismatch: original_prompt differs; preferring exif",
				"meta", meta.OriginalPrompt, "exif", md.OriginalPrompt)
		}
		if meta.JobID != "" && meta.JobID != md.JobID {
			loggerUpload.Warn("meta/exif mismatch: job_id differs; preferring exif",
				"meta", meta.JobID, "exif", md.JobID)
		}
		if meta.Reasoning != "" && meta.Reasoning != md.Reasoning {
			loggerUpload.Warn("meta/exif mismatch: reasoning differs; preferring exif")
		}
		if meta.LLMGenerated != nil && *meta.LLMGenerated != md.LLMGenerated {
			loggerUpload.Warn("meta/exif mismatch: llm_generated differs; preferring exif",
				"meta", *meta.LLMGenerated, "exif", md.LLMGenerated)
		}

		if metaContributed {
			m.MetaSource = metaSourceUploadAndEXIF
		} else {
			m.MetaSource = metaSourceEXIF
		}
		return m
	}

	// No usable EXIF: meta is the fallback for prompt fields; graph-derived
	// fields stay blank until a later milestone could re-extract.
	m.OriginalPrompt = meta.OriginalPrompt
	m.EnhancedPrompt = meta.EnhancedPrompt
	m.NegativePrompt = meta.NegativePrompt
	m.Reasoning = meta.Reasoning
	m.JobID = meta.JobID
	m.LLMGenerated = meta.LLMGenerated != nil && *meta.LLMGenerated
	if metaContributed {
		m.MetaSource = metaSourceUpload
	}
	return m
}

// applyMergedMetadata writes a merge result into a row's metadata columns.
// Shared by the upload INSERT and the admin re-extract so both paths map
// identical fields with identical NULL semantics.
func applyMergedMetadata(img *dbImage, merged mergedUploadMetadata) {
	img.OriginalPrompt = merged.OriginalPrompt
	img.EnhancedPrompt = merged.EnhancedPrompt
	img.NegativePrompt = merged.NegativePrompt
	img.Reasoning = merged.Reasoning
	img.JobID = nullStr(merged.JobID)
	img.LLMGenerated = merged.LLMGenerated
	img.Network = nullStr(merged.Network)
	img.Channel = nullStr(merged.Channel)
	img.Nick = nullStr(merged.Nick)
	img.WorkflowName = nullStr(merged.WorkflowName)
	img.Seed = merged.Seed
	img.Steps = merged.Steps
	img.Cfg = merged.Cfg
	img.Denoise = merged.Denoise
	img.Sampler = nullStr(merged.Sampler)
	img.Scheduler = nullStr(merged.Scheduler)
	img.ModelUnet = nullStr(merged.ModelUnet)
	img.ModelClip = nullStr(merged.ModelClip)
	img.ModelVae = nullStr(merged.ModelVae)
	img.Loras = nullStr(merged.LorasJSON)
	img.WorkflowJSON = merged.WorkflowJSON
	img.Width = merged.Width
	img.Height = merged.Height
	img.MetaSource = merged.MetaSource
}

// rowToUploadMeta rebuilds the meta side of the merge from a row's stored
// values. Used by re-extraction: the stored prompt/quartet fields (which
// originally came from EXIF at upload time) are fed back as "meta" so the
// cross-check compares fresh-EXIF vs stored-EXIF — a mismatch means the
// extraction rules changed the answer, and the fresh EXIF side wins.
// Provenance columns (which came from the original upload meta) round-trip
// untouched. LLMGenerated is always non-nil here: the stored value is the
// best known answer. Side effect: the non-nil pointer makes the merge see
// "meta contributed", so every re-extracted row's meta_source normalizes
// to "upload+exif" — even rows originally uploaded EXIF-only ("exif").
// meta_source is informational only (nothing reads it at runtime), and
// distinguishing heal-generations from upload-generations isn't worth the
// plumbing; documented here so the normalization is a choice, not a bug.
func rowToUploadMeta(img *dbImage) UploadMeta {
	llm := img.LLMGenerated
	return UploadMeta{
		JobID:          ptrValue(img.JobID),
		OriginalPrompt: img.OriginalPrompt,
		EnhancedPrompt: img.EnhancedPrompt,
		NegativePrompt: img.NegativePrompt,
		Reasoning:      img.Reasoning,
		LLMGenerated:   &llm,
		WorkflowName:   ptrValue(img.WorkflowName),
		Network:        ptrValue(img.Network),
		Channel:        ptrValue(img.Channel),
		Nick:           ptrValue(img.Nick),
	}
}

// uploadMetaPresent reports whether the form meta carried any information
// at all — the gate for the "upload" component of meta_source.
func uploadMetaPresent(meta UploadMeta) bool {
	return meta.JobID != "" ||
		meta.OriginalPrompt != "" ||
		meta.EnhancedPrompt != "" ||
		meta.NegativePrompt != "" ||
		meta.Reasoning != "" ||
		meta.LLMGenerated != nil ||
		meta.WorkflowName != "" ||
		meta.Network != "" ||
		meta.Channel != "" ||
		meta.Nick != ""
}

// deriveBaseURL builds the external base URL from the request when
// server.base_url is unset: scheme from TLS state or X-Forwarded-Proto
// (behind a proxy), host from the Host header.
func deriveBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		if i := strings.IndexByte(proto, ','); i >= 0 {
			proto = proto[:i]
		}
		proto = strings.TrimSpace(proto)
		if proto != "" {
			scheme = proto
		}
	}
	return scheme + "://" + r.Host
}

// generateImageID returns a random base62 id that the exists callback
// reports as unused, retrying bounded times. The exists indirection is the
// test seam for forcing collision paths.
func generateImageID(exists func(string) (bool, error)) (string, error) {
	for attempt := 0; attempt < imageIDAttempts; attempt++ {
		id, err := randomImageID()
		if err != nil {
			return "", err
		}
		taken, err := exists(id)
		if err != nil {
			return "", err
		}
		if !taken {
			return id, nil
		}
	}
	return "", fmt.Errorf("no unused image id after %d attempts", imageIDAttempts)
}

// randomImageID draws imageIDLen crypto/rand bytes and maps them onto the
// base62 alphabet. Byte%len(alphabet) is slightly biased (256 % 62 != 0);
// irrelevant here — ids are collision-checked, not secret.
func randomImageID() (string, error) {
	buf := make([]byte, imageIDLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	out := make([]byte, imageIDLen)
	for i, b := range buf {
		out[i] = imageIDAlphabet[int(b)%len(imageIDAlphabet)]
	}
	return string(out), nil
}

// allowedImageMIMEs is the image sniff allowlist applied after
// http.DetectContentType. Deliberately limited to the formats the
// thumbnailer can decode (png/jpeg/webp) — dave only sends webp/png,
// and a gif/bmp that passed the sniff would sail through upload only
// to fail thumb generation later. Rejecting at upload time (octet-
// stream degradation) beats a silently thumb-less gallery card.
var allowedImageMIMEs = map[string]bool{
	"image/webp": true,
	"image/png":  true,
	"image/jpeg": true,
}

func isAllowedImageMIME(mimeType string) bool {
	return allowedImageMIMEs[mimeType]
}

// sanitizeUploadFilename keeps the /orig/ URL valid and derivable: the URL
// path is <base>/<id>/orig/<filename>, so directory components would break
// it, and an empty name needs a default echoed back in the response.
// Anything that could corrupt the multipart part header (control
// characters), smuggle a path past filepath.Base (Windows separators —
// Base does not split them on Linux), traverse paths (".."), or render
// oddly in the URL (non-printable-ASCII) falls back to "image.png";
// filenames come from ComfyUI or LLM tool calls and are not trusted.
// Ported from img-mcp's sanitizeUploadFilename — keep the rules in sync.
func sanitizeUploadFilename(filename string) string {
	filename = filepath.Base(filename)
	if filename == "" || filename == "." || filename == ".." ||
		strings.ContainsAny(filename, "\x00\r\n\\/") ||
		strings.IndexFunc(filename, func(r rune) bool { return r < 0x20 || r > 0x7e }) >= 0 {
		return "image.png"
	}
	return filename
}
