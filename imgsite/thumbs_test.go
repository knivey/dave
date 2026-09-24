package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// insertImageWithFile writes data to the content-addressed store and
// inserts a pending row pointing at it — the state an upload leaves
// behind.
func insertImageWithFile(t *testing.T, app *App, id string, data []byte, mutators ...func(*dbImage)) string {
	t.Helper()
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	_, err := app.store.WriteOriginal(hash, data)
	require.NoError(t, err)
	img := dbImage{
		ID:          id,
		SHA256:      hash,
		Filename:    id + ".webp",
		MimeType:    "application/octet-stream",
		SizeBytes:   int64(len(data)),
		CreatedAt:   "2026-09-24 03:12:00",
		ThumbStatus: thumbStatusPending,
	}
	for _, m := range mutators {
		m(&img)
	}
	require.NoError(t, dbInsertImage(app.db, &img))
	return hash
}

func mustThumbDims(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	require.NoError(t, err, "thumbnail must be a decodable image: %s", path)
	return cfg.Width, cfg.Height
}

// mustDecodeDims decodes image dimensions from in-memory bytes.
func mustDecodeDims(t *testing.T, data []byte) (int, int) {
	t.Helper()
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err, "bytes must be a decodable image")
	return cfg.Width, cfg.Height
}

// smallPNG encodes a solid-color PNG — a real image (magic-sniffable,
// decodable) small enough to keep worker-pool tests fast under -race
// while still exercising the real decode -> resize -> encode pipeline.
// The heavyweight 1920x1080 VP8 decode is covered by
// TestProcessThumbJobRealWebP.
func smallPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, w, h))))
	return buf.Bytes()
}

func TestProcessThumbJobRealWebP(t *testing.T) {
	app := newTestApp(t, testConfig())
	cfg := testConfig()
	data := loadFixture(t, "enhanced.webp") // genuine VP8 lossy webp, 1920x1080
	hash := insertImageWithFile(t, app, "aaaa001", data)

	require.NoError(t, processThumbJob(app.db, app.store, cfg, "aaaa001"))

	img, err := dbGetImageByID(app.db, "aaaa001")
	require.NoError(t, err)
	assert.Equal(t, thumbStatusReady, img.ThumbStatus)
	require.NotNil(t, img.Width, "decoded bounds backfill for NULL dims")
	assert.Equal(t, 1920, *img.Width)
	require.NotNil(t, img.Height)
	assert.Equal(t, 1080, *img.Height)

	smallPath := app.store.ThumbPath(hash, cfg.Thumbnails.SmallWidth)
	displayPath := app.store.ThumbPath(hash, cfg.Thumbnails.DisplayWidth)
	for _, path := range []string{smallPath, displayPath} {
		raw, err := os.ReadFile(path)
		require.NoError(t, err, "derivative exists: %s", path)
		assert.True(t, bytes.HasPrefix(raw, []byte{0xFF, 0xD8}), "JPEG magic bytes: %s", path)
	}

	w, h := mustThumbDims(t, smallPath)
	assert.Equal(t, 480, w, "small derivative width")
	assert.Equal(t, 270, h, "small derivative keeps aspect (1920x1080 -> 480x270)")

	w, h = mustThumbDims(t, displayPath)
	assert.Equal(t, 1280, w, "display derivative width")
	assert.Equal(t, 720, h, "display derivative keeps aspect")
}

func TestProcessThumbJobKeepsExistingDims(t *testing.T) {
	app := newTestApp(t, testConfig())
	data := loadFixture(t, "plain.webp")
	existingW, existingH := 100, 50
	insertImageWithFile(t, app, "bbbb002", data, func(img *dbImage) {
		img.Width, img.Height = &existingW, &existingH
	})

	require.NoError(t, processThumbJob(app.db, app.store, testConfig(), "bbbb002"))

	img, err := dbGetImageByID(app.db, "bbbb002")
	require.NoError(t, err)
	require.NotNil(t, img.Width)
	assert.Equal(t, 100, *img.Width, "extraction-owned dims are never overwritten by decode bounds")
	require.NotNil(t, img.Height)
	assert.Equal(t, 50, *img.Height)
	assert.Equal(t, thumbStatusReady, img.ThumbStatus)
}

func TestProcessThumbJobNeverUpscales(t *testing.T) {
	app := newTestApp(t, testConfig())
	// A 300x200 PNG — smaller than both target widths.
	small := image.NewRGBA(image.Rect(0, 0, 300, 200))
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, small))
	hash := insertImageWithFile(t, app, "cccc003", buf.Bytes())

	require.NoError(t, processThumbJob(app.db, app.store, testConfig(), "cccc003"))

	for _, target := range []int{testConfig().Thumbnails.SmallWidth, testConfig().Thumbnails.DisplayWidth} {
		w, h := mustThumbDims(t, app.store.ThumbPath(hash, target))
		assert.Equal(t, 300, w, "no upscale for %dw target", target)
		assert.Equal(t, 200, h)
	}
}

func TestThumbProcessCorruptInputFails(t *testing.T) {
	app := newTestApp(t, testConfig())
	cfg := testConfig()
	insertImageWithFile(t, app, "dddd004", []byte("definitely not an image"))

	// The pipeline function errors...
	err := processThumbJob(app.db, app.store, cfg, "dddd004")
	require.Error(t, err)

	// ...and the worker wrapper records the terminal failure without
	// looping.
	tw := newThumbWorker(func() Config { return cfg }, app.db, app.store)
	tw.process("dddd004")

	img, err := dbGetImageByID(app.db, "dddd004")
	require.NoError(t, err)
	assert.Equal(t, thumbStatusFailed, img.ThumbStatus)
	entries, err := os.ReadDir(filepath.Join(app.store.thumbsPath, "dd"))
	if err == nil {
		assert.Empty(t, entries, "no derivatives written for a failed image")
	}
}

func TestThumbProcessOversizedGuard(t *testing.T) {
	app := newTestApp(t, testConfig())
	cfg := testConfig()
	data := loadFixture(t, "plain.webp") // small real image...
	hash := insertImageWithFile(t, app, "eeee005", data)

	// ...but the header probe lies about enormous dimensions; the guard
	// must refuse before any full-decode allocation.
	orig := decodeConfigFn
	decodeConfigFn = func([]byte) (image.Config, string, error) {
		return image.Config{Width: 20000, Height: 100}, "webp", nil
	}
	t.Cleanup(func() { decodeConfigFn = orig })

	err := processThumbJob(app.db, app.store, cfg, "eeee005")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max_dimension")

	tw := newThumbWorker(func() Config { return cfg }, app.db, app.store)
	tw.process("eeee005")
	img, err := dbGetImageByID(app.db, "eeee005")
	require.NoError(t, err)
	assert.Equal(t, thumbStatusFailed, img.ThumbStatus)
	_, err = os.Stat(app.store.ThumbPath(hash, cfg.Thumbnails.SmallWidth))
	assert.True(t, os.IsNotExist(err), "no derivative allocated for oversized dims")
}

func TestThumbWorkerSkipsNonPending(t *testing.T) {
	app := newTestApp(t, testConfig())
	cfg := testConfig()
	data := loadFixture(t, "plain.webp")
	hash := insertImageWithFile(t, app, "ffff006", data, func(img *dbImage) {
		img.ThumbStatus = thumbStatusReady // e.g. racing startup re-scan
	})

	tw := newThumbWorker(func() Config { return cfg }, app.db, app.store)
	tw.process("ffff006")

	img, err := dbGetImageByID(app.db, "ffff006")
	require.NoError(t, err)
	assert.Equal(t, thumbStatusReady, img.ThumbStatus)
	_, err = os.Stat(app.store.ThumbPath(hash, cfg.Thumbnails.SmallWidth))
	assert.True(t, os.IsNotExist(err), "non-pending rows are not processed")
}

func TestThumbWorkerRescanPending(t *testing.T) {
	app := newTestApp(t, testConfig())
	// Two rows left pending by a "crashed" previous run. Small real
	// PNGs keep each pipeline pass fast under -race (a 1920x1080 VP8
	// decode + two CatmullRom resizes can take ~17s there — that path
	// is covered separately by TestProcessThumbJobRealWebP).
	insertImageWithFile(t, app, "aaaa00A", smallPNG(t, 640, 400))
	insertImageWithFile(t, app, "bbbb00B", smallPNG(t, 640, 400))

	tw := newThumbWorker(func() Config { return testConfig() }, app.db, app.store)
	tw.Start(1)
	defer tw.Stop()

	// Order-agnostic: the re-scan enqueues created_at DESC, id DESC, but
	// per-row completion order is an implementation detail — poll BOTH
	// ids to ready. 60s budget: -race on a contended host slows even a
	// small real decode dramatically.
	require.Eventually(t, func() bool {
		for _, id := range []string{"aaaa00A", "bbbb00B"} {
			img, err := dbGetImageByID(app.db, id)
			if err != nil || img.ThumbStatus != thumbStatusReady {
				return false
			}
		}
		return true
	}, 60*time.Second, 50*time.Millisecond, "both pending rows re-scanned and processed")
}

func TestThumbWorkerStartStopIsIdempotent(t *testing.T) {
	app := newTestApp(t, testConfig())
	tw := newThumbWorker(func() Config { return testConfig() }, app.db, app.store)
	tw.Start(2)
	tw.Stop()
	tw.Stop() // second Stop must not hang or panic
}

// TestThumbWorkerSurvivesPanicInPipeline pins the per-job recover: a
// panic AFTER decode (resize/encode territory — decodeImage's own
// recover does not cover it) must mark the row failed and leave the
// worker goroutine alive to process the next job. The seam counts
// calls and panics only on the first, so the swap happens once before
// Start and never races the pool.
func TestThumbWorkerSurvivesPanicInPipeline(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertImageWithFile(t, app, "aaaa00F", smallPNG(t, 640, 400))

	orig := resizeToWidthFn
	var calls atomic.Int32
	resizeToWidthFn = func(src image.Image, targetWidth int) image.Image {
		if calls.Add(1) == 1 {
			panic("injected scaler panic")
		}
		return resizeToWidth(src, targetWidth)
	}
	t.Cleanup(func() { resizeToWidthFn = orig })

	tw := newThumbWorker(func() Config { return testConfig() }, app.db, app.store)
	tw.Start(1)
	defer tw.Stop()

	require.Eventually(t, func() bool {
		img, err := dbGetImageByID(app.db, "aaaa00F")
		return err == nil && img.ThumbStatus == thumbStatusFailed
	}, 60*time.Second, 50*time.Millisecond, "panicking job is marked failed, not a crash")

	// The worker is still alive: a second job whose resize calls (count
	// >= 2) go through to the real scaler completes normally.
	insertImageWithFile(t, app, "bbbb00G", smallPNG(t, 640, 400))
	require.True(t, tw.Enqueue("bbbb00G"), "worker pool must still accept jobs")
	require.Eventually(t, func() bool {
		img, err := dbGetImageByID(app.db, "bbbb00G")
		return err == nil && img.ThumbStatus == thumbStatusReady
	}, 60*time.Second, 50*time.Millisecond, "next job processes after the panic")
}

// TestRescanFullQueueDropsStayPending pins the aggregated-drop path: a
// full queue makes rescanPending drop every job (one WARN with a count,
// not a WARN per id), return promptly, and leave the rows pending.
func TestRescanFullQueueDropsStayPending(t *testing.T) {
	app := newTestApp(t, testConfig())
	insertImageWithFile(t, app, "aaaa00H", smallPNG(t, 320, 200))

	tw := newThumbWorker(func() Config { return testConfig() }, app.db, app.store)
	// Fill the queue with no worker running.
	for i := 0; i < thumbQueueCap; i++ {
		require.True(t, tw.Enqueue(fmt.Sprintf("filler%03d", i)), "queue accepts up to cap")
	}
	require.False(t, tw.Enqueue("overflow"), "full queue refuses non-blockingly")

	done := make(chan struct{})
	go func() { tw.rescanPending(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rescanPending must not block on a full queue")
	}

	img, err := dbGetImageByID(app.db, "aaaa00H")
	require.NoError(t, err)
	assert.Equal(t, thumbStatusPending, img.ThumbStatus, "dropped re-scan rows stay pending for the next restart")
}

// TestUploadThenThumbRouteServesJPEG is the end-to-end M3 path: upload via
// /updo, drive the pipeline synchronously (the test hook — the real pool
// runs it in the background), then fetch the thumbnail route.
func TestUploadThenThumbRouteServesJPEG(t *testing.T) {
	app := newTestApp(t, testConfig())
	ts := newTestServer(t, app)
	data := loadFixture(t, "enhanced.webp")

	resp := doUpload(t, ts, testAPIKey, uploadParts{hasFile: true, filename: "shrew.webp", data: data})
	require.Equal(t, 201, resp.StatusCode)
	ur := decodeUploadResponse(t, resp)

	img, err := dbGetImageByID(app.db, ur.ID)
	require.NoError(t, err)
	require.Equal(t, thumbStatusPending, img.ThumbStatus, "upload answers before any thumbnail work")

	require.NoError(t, processThumbJob(app.db, app.store, testConfig(), ur.ID))

	thumb := fetchPath(t, ts, "/"+ur.ID+"/t/small")
	require.Equal(t, 200, thumb.StatusCode)
	assert.Equal(t, "image/jpeg", thumb.Header.Get("Content-Type"))
	assert.Equal(t, "public, max-age=31536000, immutable", thumb.Header.Get("Cache-Control"))
	body, err := io.ReadAll(thumb.Body)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(body, []byte{0xFF, 0xD8}), "served bytes are a real JPEG")
}

// fetchPath GETs a path on the test server and returns the response with
// the body still open (caller reads or discards; closed via cleanup).
func fetchPath(t *testing.T, ts *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := ts.Client().Get(ts.URL + path)
	require.NoError(t, err)
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}
