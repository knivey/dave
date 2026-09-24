package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"sync"

	"github.com/jmoiron/sqlx"
	"golang.org/x/image/draw"
	// webp decode (VP8 lossy + VP8L lossless) via the registered
	// image.Image decoder; png/jpeg above cover the fallbacks.
	_ "golang.org/x/image/webp"
)

// thumbQueueCap bounds the in-process job channel. Uploads enqueue
// non-blockingly; a full queue drops the job (the row stays pending and
// the startup re-scan picks it up after a restart), so a stuck worker can
// never stall the upload path.
const thumbQueueCap = 256

// decodeConfigFn is the header-probe seam: image.DecodeConfig on the
// registered decoders. Tests swap it to exercise the max_dimension guard
// without allocating a real 20000px image.
var decodeConfigFn = func(data []byte) (image.Config, string, error) {
	return image.DecodeConfig(bytes.NewReader(data))
}

// resizeToWidthFn is the resize seam of the pipeline. Tests swap it to
// inject panics from inside the pipeline (after decode) and assert the
// per-job recover in runJob keeps the worker alive. Installed before
// Start and restored after Stop, so the swap itself never races the
// pool.
var resizeToWidthFn = resizeToWidth

// thumbWorker is the background thumbnailer: a fixed-size pool draining
// an id channel, plus a startup re-scan of pending rows so a crash
// between INSERT and thumbnail never strands placeholders forever.
type thumbWorker struct {
	cfgFn func() Config // read per job: widths/quality/max_dimension hot-reload
	db    *sqlx.DB
	store *Store

	jobs   chan string
	wg     sync.WaitGroup
	cancel context.CancelFunc

	// onThumbReady is the SSE publish seam: main wires it to
	// App.publishThumbReady, which fans out the thumb-ready event —
	// and process only invokes it after runJob returned nil, i.e.
	// after the DB flip to ready committed (subscribers' shimmer→thumb
	// swap queries the row immediately). A no-op by default so tests
	// that don't exercise SSE run the pipeline unchanged.
	onThumbReady func(id string)
}

func newThumbWorker(cfgFn func() Config, db *sqlx.DB, store *Store) *thumbWorker {
	return &thumbWorker{
		cfgFn:        cfgFn,
		db:           db,
		store:        store,
		jobs:         make(chan string, thumbQueueCap),
		onThumbReady: func(string) {},
	}
}

// Start launches the worker pool (sized once at startup —
// thumbnails.workers is not reloadable) and re-enqueues every row still
// pending.
func (tw *thumbWorker) Start(workers int) {
	if workers < 1 {
		workers = 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	tw.cancel = cancel
	for i := 0; i < workers; i++ {
		tw.wg.Add(1)
		go func() {
			defer tw.wg.Done()
			for {
				select {
				case <-ctx.Done():
					// Graceful shutdown: finish the current job, drop the
					// queued ones (their rows stay pending for the next
					// startup re-scan).
					return
				case id := <-tw.jobs:
					tw.process(id)
				}
			}
		}()
	}
	tw.rescanPending()
}

// Stop cancels the pool and waits for in-flight jobs to finish.
func (tw *thumbWorker) Stop() {
	if tw.cancel != nil {
		tw.cancel()
	}
	tw.wg.Wait()
}

// Enqueue hands an image id to the pool without ever blocking. It
// reports whether the job was accepted; callers decide how to report a
// drop (the row stays pending either way and the next startup re-scan
// picks it up, so a stuck worker can never stall the upload path).
func (tw *thumbWorker) Enqueue(id string) bool {
	select {
	case tw.jobs <- id:
		return true
	default:
		return false
	}
}

// rescanPending re-enqueues rows left pending by a previous run, in the
// query's deterministic order (created_at DESC, id DESC — enqueue order
// must stay stable so worker pickup is reproducible). A full queue
// drops jobs; drops are reported as ONE aggregated WARN rather than a
// WARN per id, since a re-scan racing a stuck worker can drop hundreds
// of rows and the per-id spam adds nothing operable.
func (tw *thumbWorker) rescanPending() {
	ids, err := dbGetPendingThumbIDs(tw.db)
	if err != nil {
		loggerThumbs.Error("startup pending re-scan failed", "error", err)
		return
	}
	dropped := 0
	for _, id := range ids {
		if !tw.Enqueue(id) {
			dropped++
		}
	}
	if dropped > 0 {
		loggerThumbs.Warn("startup re-scan dropped jobs; rows stay pending for the next restart",
			"dropped", dropped, "total", len(ids))
	}
	if enqueued := len(ids) - dropped; enqueued > 0 {
		loggerThumbs.Info("startup re-scan enqueued pending thumbnails", "count", enqueued)
	}
}

// process runs one id through the pipeline and records the outcome.
// Failures are terminal (thumb_status='failed') — never retried in a
// loop; a future re-extract/admin action can reset the row to pending.
func (tw *thumbWorker) process(id string) {
	err := tw.runJob(id)
	if err != nil {
		loggerThumbs.Warn("thumbnail generation failed", "id", id, "error", err)
		if uerr := dbUpdateThumbStatus(tw.db, id, thumbStatusFailed); uerr != nil {
			loggerThumbs.Error("marking thumbnail failed", "id", id, "error", uerr)
		}
		return
	}
	tw.onThumbReady(id)
}

// runJob is the recover boundary of the worker pool. decodeImage
// already converts DECODER panics into ordinary errors, but a panic
// anywhere else in the pipeline (CatmullRom.Scale on a hostile image,
// jpeg.Encode, any future step) would otherwise crash the whole
// process: one bad image must never take the worker — let alone the
// site — down. The panic is flattened into an error here so process
// marks the row failed and the loop moves on to the next job.
func (tw *thumbWorker) runJob(id string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("pipeline panic: %v", p)
		}
	}()
	return processThumbJob(tw.db, tw.store, tw.cfgFn(), id)
}

// processThumbJob generates both derivatives for one image and flips the
// row to ready. It is a standalone function (not a method) so tests can
// drive the pipeline synchronously without a running pool.
func processThumbJob(db *sqlx.DB, store *Store, cfg Config, id string) error {
	img, err := dbGetImageByID(db, id)
	if err != nil {
		return fmt.Errorf("loading row: %w", err)
	}
	// Dedupe: only pending rows are processed, so a startup re-scan
	// racing an upload's enqueue is a harmless skip.
	if img.ThumbStatus != thumbStatusPending {
		return nil
	}

	orig, err := os.ReadFile(store.OriginalPath(img.SHA256))
	if err != nil {
		return fmt.Errorf("reading original: %w", err)
	}

	src, err := decodeImage(orig, cfg.Thumbnails.MaxDimension)
	if err != nil {
		return err
	}
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()

	quality := cfg.Thumbnails.JPEGQuality
	for _, target := range []int{cfg.Thumbnails.SmallWidth, cfg.Thumbnails.DisplayWidth} {
		if target < 1 {
			continue
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, resizeToWidthFn(src, target), &jpeg.Options{Quality: quality}); err != nil {
			return fmt.Errorf("encoding %dw derivative: %w", target, err)
		}
		if err := store.WriteThumb(img.SHA256, target, buf.Bytes()); err != nil {
			return fmt.Errorf("storing %dw derivative: %w", target, err)
		}
	}

	// width/height are the decoded-bounds fallback: only backfilled when
	// the graph had no latent node (M2 extraction leaves them NULL).
	if err := dbUpdateThumbReady(db, id, width, height); err != nil {
		return fmt.Errorf("marking ready: %w", err)
	}
	loggerThumbs.Info("thumbnails ready", "id", id, "width", width, "height", height)
	return nil
}

// decodeImage probes the header first (cheap) so absurd dimensions are
// refused before a full decode allocates gigabytes, then decodes. The
// recover turns decoder panics on adversarial input into ordinary
// errors with a precise message; the worker-level recover in runJob is
// the actual never-take-the-worker-down guarantee and covers the rest
// of the pipeline (resize, encode) as well.
func decodeImage(data []byte, maxDimension int) (src image.Image, err error) {
	conf, _, err := decodeConfigFn(data)
	if err != nil {
		return nil, fmt.Errorf("probing header: %w", err)
	}
	if conf.Width <= 0 || conf.Height <= 0 {
		return nil, fmt.Errorf("invalid dimensions %dx%d", conf.Width, conf.Height)
	}
	if conf.Width > maxDimension || conf.Height > maxDimension {
		return nil, fmt.Errorf("dimensions %dx%d exceed max_dimension %d", conf.Width, conf.Height, maxDimension)
	}
	defer func() {
		if p := recover(); p != nil {
			src = nil
			err = fmt.Errorf("decoder panic: %v", p)
		}
	}()
	src, _, err = image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decoding: %w", err)
	}
	return src, nil
}

// resizeToWidth downscales with CatmullRom to the target width,
// preserving aspect. Never upscales: smaller originals pass through at
// natural size (the derivative is then just a JPEG re-encode).
//
// DESIGN NOTE (alpha flattening): JPEG has no alpha channel and the
// stdlib encoder drops it, which composites any transparency onto
// black. That is deliberately left as-is: every context the site
// renders an image in is itself near-black (`.card img` and `.viewer`
// both sit on #0d0d0f = rgb(13,13,15)), so a flattened thumbnail is
// visually identical to the original composited by the browser in
// place — worst-case channel delta is 15/255 (blue; 13/255 red/green),
// and production inputs never carry alpha anyway — ComfyUI's lossy VP8
// webp decodes to opaque *image.YCbCr (verified against the testdata
// fixtures). Alpha only reaches this pipeline through accepted but
// unused upload types (transparent PNG), where flatten-to-black is the
// faithful rendering.
func resizeToWidth(src image.Image, targetWidth int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || w <= targetWidth {
		return src
	}
	newH := (h*targetWidth + w/2) / w
	if newH < 1 {
		newH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, targetWidth, newH))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}
