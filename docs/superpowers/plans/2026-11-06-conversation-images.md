# Conversation Images Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the model a `view_image` builtin tool that pulls any image URL into the conversation as vision input, with ref-ID pairing (the seam for a future img-mcp edit workflow) and a binary-content guard in fetch-mcp.

**Architecture:** One injection mechanism — a labeled `RoleUser` message with `MultiContent [ref-text, image]`, appended through the existing `turnContext.Add` persistence seam after all tool results of a round. A new `image_refs` store (DB row + file on disk) mints `usr_` (pastes) and `web_` (view_image) refs that survive compaction. Generated images are NEVER auto-injected (owner directive). The Responses chained tool-loop input builder must learn to carry the injected user message (it currently takes only the trailing tool results and would silently drop it).

**Tech Stack:** Go 1.25, GORM (AutoMigrate), TOML config, testify, httptest. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-11-06-conversation-images-design.md` — the plan argues from the spec; read both.

## Global Constraints

- Go 1.25.0 module `github.com/knivey/dave`. Build: `go build -o dave .` etc. per AGENTS.md.
- Every `logxi.New()` logger MUST call `SetLevel(logxi.LevelAll)`.
- TOML struct tags are snake_case. New config fields must be added to the config file's reference section + commented example block (Config Documentation Convention in AGENTS.md).
- Tests use `github.com/stretchr/testify` (`assert`/`require`), table-driven with `t.Run` where natural. Run `go test ./...` after every task; `go fmt ./...` and `go vet ./...` before each commit.
- Never `git add -f`. Commit per task with messages like `feat: ...` / `fix: ...` / `test: ...`.
- Failure philosophy for the tool handler: every failure is tool-result text the model can act on, never a user-facing notice.
- Root-package tests: `setupTestDB(t)` (testHelpers_test.go) sets the GORM global; `setupNoticesDefaults(t)` initializes notices; runner literals copy `makeResponsesRunner` (aiCmds_test.go:439).
- Image test helpers that already exist: `createTestJPEG(t)` (image_detect_test.go:660), `patchPNGDimensions` (:761), and the httptest image-server pattern in `TestBuildImageMessageNonImageURLPreserved` (:668).

## Review Focus

Five input classes the spec implies but individual tasks might not exercise — each pinned below in the task that owns the code:

1. **Multiple tool calls in one assistant message including `view_image`** — the injection must land after ALL tool results of the round, not between them (providers require tool results to follow the `tool_calls` message). Pinned by `TestExecuteToolCallsFlushesInjectionAfterAllResults` (Task 5).
2. **Chained Responses turn (`previous_response_id`) with a `view_image` call** — the delta-only input must include the injected image message; the old builder (`turn.LastN(numToolCalls)`) silently drops it. Pinned by `TestRunTurnResponsesViewImageChainDeliversImage` (Task 6).
3. **Loopback/LAN imgsite URL** — the SSRF guard blocks private IPs by default; `trusted_image_hosts` must unblock exactly the listed hosts and nothing else. Pinned by both directions of `TestDownloadImageTrustedHost` (Task 3).
4. **Context image budget counts viewed images too** — repeated `view_image` calls must stop at `maxcontextimages`, counting pasted + viewed images across the turn's history. Pinned by `TestHandleViewImageBudgetRefusal` (Task 5).
5. **Corrupt/bomb image via `view_image`** — decode/pixel-cap failures surface as graceful tool-result errors, not panics or user notices. Pinned by `TestHandleViewImageConvertFailure` (Task 5).

---

### Task 1: Config — `trusted_image_hosts` + `images_dir`

**Files:**
- Modify: `config.go` (root Config struct ~line 38, ApplyDefaults ~line 750, `loadConfigDir` root validation)
- Modify: `config/config.toml` (reference section + commented example)
- Test: `config_test.go`

**Interfaces:**
- Consumes: existing `Config` struct, `loadConfigDir`, `createTestConfigDir` helper (see `TestLoadConfigDirNetworkEnabledDefault` for its exact calling convention).
- Produces: `config.TrustedImageHosts []string` (`toml:"trusted_image_hosts"`), `config.ImagesDir string` (`toml:"images_dir"`, default `"data/images"` applied in ApplyDefaults). Load rejects wildcard entries.

- [ ] **Step 1: Write the failing tests**

Add to `config_test.go` (mirror `createTestConfigDir` usage from an existing load test):

```go
func TestLoadConfigDirTrustedImageHosts(t *testing.T) {
	dir := createTestConfigDir(t, `
trusted_image_hosts = ["img.example.com", "IMG.other.host"]
`)
	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"img.example.com", "IMG.other.host"}, cfg.TrustedImageHosts,
		"hosts load verbatim (case preserved); matching is case-insensitive at use site")
}

func TestLoadConfigDirRejectsWildcardTrustedImageHost(t *testing.T) {
	for _, host := range []string{"*.example.com", "img.*.com", "?"} {
		t.Run(host, func(t *testing.T) {
			dir := createTestConfigDir(t, "trusted_image_hosts = [\""+host+"\"]\n")
			_, err := loadConfigDir(dir)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "trusted_image_hosts")
			assert.Contains(t, err.Error(), "wildcard")
		})
	}
}

func TestLoadConfigDirImagesDirDefault(t *testing.T) {
	dir := createTestConfigDir(t, "")
	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)
	assert.Equal(t, "data/images", cfg.ImagesDir)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestLoadConfigDirTrustedImageHosts|TestLoadConfigDirRejectsWildcard|TestLoadConfigDirImagesDirDefault' .`
Expected: FAIL — unknown field ignored (first test gets nil slice), no wildcard rejection, empty ImagesDir.

- [ ] **Step 3: Implement**

In `config.go`, add to the root `Config` struct right after `MaxImagePixels` (~line 38):

```go
	TrustedImageHosts []string `toml:"trusted_image_hosts"`
	ImagesDir         string   `toml:"images_dir"`
```

In `ApplyDefaults` next to the existing `MaxImagePixels` default (~line 750):

```go
	if config.ImagesDir == "" {
		config.ImagesDir = "data/images"
	}
```

In `loadConfigDir`, where the root config is validated (the same region that parses `maximagesize`), add:

```go
	for _, h := range config.TrustedImageHosts {
		if strings.ContainsAny(h, "*?") {
			return fmt.Errorf("trusted_image_hosts: wildcards are not supported (got %q) — list exact hostnames; a wildcarded SSRF-guard bypass is a footgun", h)
		}
		if strings.TrimSpace(h) == "" {
			return fmt.Errorf("trusted_image_hosts: empty hostname entry")
		}
	}
```

In `config/config.toml`, add to the reference section (and the commented example block) per the config docs convention:

```toml
# trusted_image_hosts (string list, default []) — exact hostnames (no wildcards,
#   case-insensitive) whose image downloads bypass the SSRF private-IP block.
#   Needed when imgsite resolves privately (LAN/localhost) so dave can fetch its
#   own gallery for view_image. Scheme, size and content-type checks still apply.
# images_dir (string, default "data/images") — where conversation-image ref files
#   are stored (view_image + user pastes).
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestLoadConfigDirTrustedImageHosts|TestLoadConfigDirRejectsWildcard|TestLoadConfigDirImagesDirDefault' .`
Expected: PASS. Then `go test ./...` for the root package — config tests are load-bearing for everything.

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go config/config.toml
git commit -m "feat: trusted_image_hosts + images_dir config"
```

---

### Task 2: Image ref store

**Files:**
- Create: `imageref.go`
- Create: `imageref_test.go`
- Modify: `db.go` (AutoMigrate list, ~line 294)
- Modify: `contextLifecycle.go` (`CleanupContexts`, after the pending-jobs pass ~line 67)
- Modify: `tools/migrate-sqlite-to-pgsql/main.go` (mirror struct + tables list ~line 198)

**Interfaces:**
- Consumes: `theDB` (*gorm.DB global), `readConfig` (config RLock helper), `config.ImagesDir` (Task 1), `setupTestDB(t)`.
- Produces:
  - `type ImageRef struct` (GORM model, table `image_refs`)
  - `registerImageRef(sessionID int64, origin, sourceURL, mime string, width, height int, data []byte) (ref string, err error)` — best-effort: returns the minted ref even when storing fails (callers label the ref regardless and WARN)
  - `resolveImageRef(ref string) (*ImageRef, error)` — unused by production; the seam the future edit workflow resolves through
  - `cleanupImageRefsByAge(maxAgeDays int) (int64, error)`, `sweepOrphanImageFiles(dir string, grace time.Duration) (int, error)`

- [ ] **Step 1: Write the failing tests**

Create `imageref_test.go`:

```go
package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupImageRefTest(t *testing.T) {
	t.Helper()
	setupTestDB(t)
	dir := t.TempDir()
	config.ImagesDir = dir
	t.Cleanup(func() { config.ImagesDir = "" })
}

func TestRegisterImageRefRoundTrip(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(42, "view", "https://img.example.com/abc.png", "image/png", 640, 480, []byte("pngbytes"))
	require.NoError(t, err)
	assert.Regexp(t, `^web_[0-9a-f]{12}$`, ref, "view origin mints web_ refs")

	row, err := resolveImageRef(ref)
	require.NoError(t, err)
	assert.Equal(t, int64(42), row.SessionID)
	assert.Equal(t, "view", row.Origin)
	assert.Equal(t, "https://img.example.com/abc.png", row.SourceURL)
	assert.Equal(t, 640, row.Width)
	assert.Equal(t, 480, row.Height)

	data, err := os.ReadFile(row.FilePath)
	require.NoError(t, err)
	assert.Equal(t, []byte("pngbytes"), data, "file stores the encoded bytes")
	assert.Equal(t, ".png", filepath.Ext(row.FilePath))
}

func TestRegisterImageRefUserOriginPrefix(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(1, "user", "https://x/y.jpg", "image/jpeg", 10, 10, []byte("j"))
	require.NoError(t, err)
	assert.Regexp(t, `^usr_[0-9a-f]{12}$`, ref)
}

func TestRegisterImageRefRefsAreUnique(t *testing.T) {
	setupImageRefTest(t)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		ref, err := registerImageRef(1, "view", "u", "image/png", 1, 1, []byte("b"))
		require.NoError(t, err)
		require.False(t, seen[ref], "ref %s minted twice", ref)
		seen[ref] = true
	}
}

func TestResolveImageRefUnknown(t *testing.T) {
	setupImageRefTest(t)
	_, err := resolveImageRef("usr_deadbeefcafe")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown image ref")
}

func TestCleanupImageRefsByAge(t *testing.T) {
	setupImageRefTest(t)
	oldRef, err := registerImageRef(1, "view", "u", "image/png", 1, 1, []byte("old"))
	require.NoError(t, err)
	newRef, err := registerImageRef(1, "view", "u", "image/png", 1, 1, []byte("new"))
	require.NoError(t, err)

	cutoff := time.Now().Add(-48 * time.Hour)
	require.NoError(t, theDB.Model(&ImageRef{}).Where("ref = ?", oldRef).Update("created_at", cutoff).Error)

	n, err := cleanupImageRefsByAge(1)
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	_, err = resolveImageRef(oldRef)
	require.Error(t, err, "old ref deleted")
	row, err := resolveImageRef(newRef)
	require.NoError(t, err)
	assert.NoError(t, os.Stat(row.FilePath), "kept ref's file still exists")
}

func TestSweepOrphanImageFiles(t *testing.T) {
	setupImageRefTest(t)
	ref, err := registerImageRef(1, "view", "u", "image/png", 1, 1, []byte("kept"))
	require.NoError(t, err)
	var keptRow ImageRef
	require.NoError(t, theDB.Where("ref = ?", ref).First(&keptRow).Error)

	orphan := filepath.Join(config.ImagesDir, "web_orphanedfile.png")
	require.NoError(t, os.WriteFile(orphan, []byte("x"), 0o644))
	past := time.Now().Add(-48 * time.Hour)
	require.NoError(t, os.Chtimes(orphan, past, past))

	n, err := sweepOrphanImageFiles(config.ImagesDir, 24*time.Hour)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.NoFileExists(t, orphan)
	assert.FileExists(t, keptRow.FilePath, "registered file untouched")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestRegisterImageRef|TestResolveImageRef|TestCleanupImageRefs|TestSweepOrphan' .`
Expected: FAIL — `undefined: registerImageRef` (etc.).

- [ ] **Step 3: Implement `imageref.go`**

```go
package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gorm.io/gorm"
)

var loggerImgRef = newLogger("imageref")
```

(`newLogger` in main.go:32 wraps `logxi.New` + `SetLevel(logxi.LevelAll)` — the package idiom, same as `loggerTC` in turncontext.go; use it instead of hand-rolling.)

```go
// ImageRef maps a conversation image's ref ID to its bytes on disk. Refs are
// minted wherever an image enters the model's context (user pastes: usr_,
// view_image: web_) and are the seam a future img-mcp edit workflow resolves
// through: the model passes the ref it sees in the image's label text;
// dave expands usr_/web_ refs to bytes; img-mcp will resolve its own gen_
// refs. The store must outlive live history because compaction strips image
// parts to [image] placeholders — the ref label text survives, the data URI
// does not.
type ImageRef struct {
	ID        uint64    `gorm:"primaryKey"`
	Ref       string    `gorm:"uniqueIndex;size:64"`
	SessionID int64     `gorm:"index"`
	Origin    string    `gorm:"size:16"` // "user" (paste) | "view" (view_image)
	SourceURL string    `gorm:"type:text"`
	MIME      string    `gorm:"size:64"`
	FilePath  string    `gorm:"type:text"`
	Width     int
	Height    int
	CreatedAt time.Time
}

// imageRefRandLen: 6 random bytes -> 12 hex chars. 48 bits of randomness with
// a unique index; registerImageRef still retries once on collision so the
// contract is mechanical rather than probabilistic.
const imageRefRandLen = 6

func mintImageRef(prefix string) (string, error) {
	b := make([]byte, imageRefRandLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("minting image ref: %w", err)
	}
	return prefix + "_" + hex.EncodeToString(b), nil
}

func imageRefPrefix(origin string) string {
	switch origin {
	case "user":
		return "usr"
	case "view":
		return "web"
	default:
		return "img"
	}
}

func imageExtForMIME(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "image/bmp":
		return "bmp"
	default:
		return "bin"
	}
}

// registerImageRef stores data at <images_dir>/<ref>.<ext> and inserts the
// row. BEST-EFFORT CONTRACT: callers label the ref in the conversation
// REGARDLESS of the returned error — seeing the image is the point, and an
// unresolvable ref degrades to a readable error in the future edit tool,
// never a broken turn. When minting succeeded, the ref is returned non-empty
// even alongside an error so callers can keep the label shape stable.
func registerImageRef(sessionID int64, origin, sourceURL, mime string, width, height int, data []byte) (string, error) {
	prefix := imageRefPrefix(origin)
	ref, err := mintImageRef(prefix)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return ref, errors.New("image ref store: no image data")
	}

	var dir string
	readConfig(func() { dir = config.ImagesDir })
	if dir == "" {
		dir = "data/images"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		loggerImgRef.Warn("image ref store: mkdir failed (ref stays labeled, unresolvable)", "ref", ref, "error", err.Error())
		return ref, fmt.Errorf("image ref store: %w", err)
	}

	writeFile := func(r string) (string, error) {
		p := filepath.Join(dir, r+"."+imageExtForMIME(mime))
		return p, os.WriteFile(p, data, 0o644)
	}

	row := ImageRef{SessionID: sessionID, Origin: origin, SourceURL: sourceURL, MIME: mime, Width: width, Height: height, CreatedAt: time.Now()}

	if theDB == nil {
		path, werr := writeFile(ref)
		if werr != nil {
			loggerImgRef.Warn("image ref store: write failed (ref stays labeled, unresolvable)", "ref", ref, "error", werr.Error())
			return ref, fmt.Errorf("image ref store: %w", werr)
		}
		row.FilePath = path
		loggerImgRef.Warn("image ref store: no database (ref stays labeled, unresolvable)", "ref", ref)
		return ref, errors.New("image ref store: database not initialized")
	}

	path, werr := writeFile(ref)
	if werr != nil {
		loggerImgRef.Warn("image ref store: write failed (ref stays labeled, unresolvable)", "ref", ref, "error", werr.Error())
		return ref, fmt.Errorf("image ref store: %w", werr)
	}
	row.Ref, row.FilePath = ref, path
	if err := theDB.Create(&row).Error; err != nil {
		// One collision retry (see imageRefRandLen note), then give up —
		// the orphaned file is removed so the sweep never sees it.
		os.Remove(path)
		if ref2, e2 := mintImageRef(prefix); e2 == nil {
			if path2, w2 := writeFile(ref2); w2 == nil {
				row.Ref, row.FilePath = ref2, path2
				if e3 := theDB.Create(&row).Error; e3 == nil {
					loggerImgRef.Debug("image ref registered (retry)", "ref", ref2, "origin", origin)
					return ref2, nil
				}
				os.Remove(path2)
				ref = ref2
				err = e3
			}
		}
		loggerImgRef.Warn("image ref store: insert failed (ref stays labeled, unresolvable)", "ref", ref, "error", err.Error())
		return ref, fmt.Errorf("image ref store: %w", err)
	}
	loggerImgRef.Debug("image ref registered", "ref", ref, "origin", origin, "session", sessionID)
	return ref, nil
}

// resolveImageRef looks a ref up. Deliberately unused by production today —
// it is the seam the img-mcp edit workflow's tool-call interception resolves
// usr_/web_ refs through; these tests keep it honest so it cannot rot.
func resolveImageRef(ref string) (*ImageRef, error) {
	if theDB == nil {
		return nil, errors.New("image ref store: database not initialized")
	}
	var row ImageRef
	if err := theDB.Where("ref = ?", ref).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("unknown image ref %q", ref)
		}
		return nil, err
	}
	return &row, nil
}

// cleanupImageRefsByAge removes ref rows (and their files) older than
// maxAgeDays — refs for long-gone sessions are dead weight. Rides the same
// boot/periodic pass and age knob as session cleanup (CleanupContexts).
func cleanupImageRefsByAge(maxAgeDays int) (int64, error) {
	if theDB == nil {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -maxAgeDays)
	var rows []ImageRef
	if err := theDB.Where("created_at < ?", cutoff).Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	for _, row := range rows {
		if row.FilePath != "" {
			if err := os.Remove(row.FilePath); err != nil && !os.IsNotExist(err) {
				loggerImgRef.Warn("image ref cleanup: unlink failed", "ref", row.Ref, "error", err.Error())
			}
		}
	}
	res := theDB.Where("created_at < ?", cutoff).Delete(&ImageRef{})
	return res.RowsAffected, res.Error
}

// sweepOrphanImageFiles deletes files in dir with no matching row and a
// modtime older than grace — covers crashes between file write and row
// insert.
func sweepOrphanImageFiles(dir string, grace time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	known := map[string]bool{}
	if theDB != nil {
		var rows []ImageRef
		if err := theDB.Find(&rows).Error; err != nil {
			return 0, err
		}
		for _, r := range rows {
			if r.FilePath != "" {
				known[filepath.Base(r.FilePath)] = true
			}
		}
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || known[e.Name()] {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || time.Since(info.ModTime()) < grace {
			continue
		}
		if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr == nil {
			removed++
		}
	}
	return removed, nil
}
```

In `db.go`, extend the AutoMigrate list (~line 294):

```go
	if err := db.AutoMigrate(
		&Session{}, &Message{}, &PendingJob{}, &TurnUsage{}, &SessionSetting{},
		&User{}, &UserKnownHost{}, &NickChange{}, &Ban{}, &Compaction{}, &ImageRef{},
	); err != nil {
```

In `contextLifecycle.go` `CleanupContexts`, after the pending-jobs pass (mirror its log-and-continue comment style):

```go
	// Image-ref GC rides the same pass and knob: refs older than MaxAgeDays
	// belong to long-gone sessions; the images also live in archived message
	// rows until those go away. Orphan files (crash between write and insert)
	// get a grace period sweep in the same pass.
	if refAffected, err := cleanupImageRefsByAge(config.Database.MaxAgeDays); err != nil {
		loggerCS.Error("Failed to cleanup image refs", "error", err)
	} else if refAffected > 0 {
		loggerCS.Info("Cleaned up old image refs", "count", refAffected)
	}
	if n, err := sweepOrphanImageFiles(config.ImagesDir, 24*time.Hour); err != nil {
		loggerCS.Error("Failed to sweep orphan image files", "error", err)
	} else if n > 0 {
		loggerCS.Info("Swept orphan image files", "count", n)
	}
```

(Add `"time"` to contextLifecycle.go imports if missing.)

In `tools/migrate-sqlite-to-pgsql/main.go`: mirror the model like the existing `Message` mirror (~line 57), and add `"image_refs"` to the tables list (~line 198):

```go
// ImageRef mirrors the main ImageRef struct (imageref.go) — the migration
// tool copies rows 1:1 between dialects.
type ImageRef struct {
	ID        uint64    `gorm:"primaryKey"`
	Ref       string    `gorm:"uniqueIndex;size:64"`
	SessionID int64     `gorm:"index"`
	Origin    string    `gorm:"size:16"`
	SourceURL string    `gorm:"type:text"`
	MIME      string    `gorm:"size:64"`
	FilePath  string    `gorm:"type:text"`
	Width     int
	Height    int
	CreatedAt time.Time
}
```

and `tables := []string{"sessions", "session_settings", "messages", "pending_jobs", "turn_usage", "image_refs"}` plus a `migrateTable[ImageRef](srcDB, dstDB, 500, "image_refs")` call alongside the messages one.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestRegisterImageRef|TestResolveImageRef|TestCleanupImageRefs|TestSweepOrphan' .`
Expected: PASS. Also `go build ./...` and `go build -o /tmp/opencode/migrate-check ./tools/migrate-sqlite-to-pgsql` to confirm the mirror compiles.

- [ ] **Step 5: Commit**

```bash
git add imageref.go imageref_test.go db.go contextLifecycle.go tools/migrate-sqlite-to-pgsql/main.go
git commit -m "feat: image ref store (mint/register/resolve/cleanup)"
```

---

### Task 3: Trusted-host download + shared `fetchAndConvertImage`

**Files:**
- Modify: `image_detect.go` (trusted client + host check ~line 289; `downloadImage` ~line 294; `convertImage` signature ~line 341; `buildImageMessage` refactor ~line 512)
- Test: `image_detect_test.go`

**Interfaces:**
- Consumes: `config.TrustedImageHosts` (Task 1), existing `downloadImage`/`convertImage`.
- Produces:
  - `trustedImageHost(host string) bool`
  - `fetchedImage struct { Encoded []byte; DataURI string; MIME string; Width, Height int }`
  - `fetchAndConvertImage(url, format string, quality, maxW, maxH, maxPixels int) (*fetchedImage, error)`
  - CHANGED: `convertImage(imgData []byte, mimeType, format string, quality, maxW, maxH, maxPixels int) (out []byte, dataURI string, width, height int, err error)` — now also returns post-resize dimensions (update its direct test callers).
  - `downloadImage(url)` behavior change: hosts in `trusted_image_hosts` use a plain (non-SSRF-guarded) client; everything else unchanged.

- [ ] **Step 1: Write the failing tests**

Add to `image_detect_test.go` (reuse `createTestJPEG` and the httptest server pattern from `TestBuildImageMessageNonImageURLPreserved`):

```go
func newTestImageServer(t *testing.T) *httptest.Server {
	t.Helper()
	jpegBytes := createTestJPEG(t)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpegBytes)
	}))
}

func TestDownloadImageTrustedHost(t *testing.T) {
	srv := newTestImageServer(t)
	defer srv.Close()
	host := strings.SplitN(strings.TrimPrefix(srv.URL, "http://"), ":", 2)[0] // 127.0.0.1

	// Default: loopback is SSRF-blocked.
	_, _, err := downloadImage(srv.URL + "/pic.jpg")
	require.Error(t, err, "loopback must be blocked by the SSRF guard by default")
	assert.Contains(t, err.Error(), "blocked IP address")

	// Trusted: exact host entry unblocks ONLY the private-IP dial check.
	config.TrustedImageHosts = []string{host}
	t.Cleanup(func() { config.TrustedImageHosts = nil })
	data, mime, err := downloadImage(srv.URL + "/pic.jpg")
	require.NoError(t, err)
	assert.Equal(t, "image/jpeg", mime)
	assert.NotEmpty(t, data)

	// A DIFFERENT host is still blocked even while the list is populated.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(createTestJPEG(t))
	}))
	defer bad.Close()
	// Force a non-matching hostname comparison via the matcher directly:
	assert.False(t, trustedImageHost("definitely-not-listed.example"),
		"only exact (case-insensitive) host entries match")
}

func TestTrustedImageHostCaseInsensitive(t *testing.T) {
	config.TrustedImageHosts = []string{"IMG.Example.com"}
	t.Cleanup(func() { config.TrustedImageHosts = nil })
	assert.True(t, trustedImageHost("img.example.com"))
	assert.False(t, trustedImageHost("sub.img.example.com"), "no suffix matching")
}

func TestFetchAndConvertImage(t *testing.T) {
	srv := newTestImageServer(t)
	defer srv.Close()
	config.TrustedImageHosts = []string{"127.0.0.1"}
	t.Cleanup(func() { config.TrustedImageHosts = nil })

	fi, err := fetchAndConvertImage(srv.URL+"/pic.jpg", "jpg", 75, 1024, 1024, 0)
	require.NoError(t, err)
	assert.Contains(t, fi.DataURI, "data:image/jpeg;base64,")
	assert.NotEmpty(t, fi.Encoded)
	assert.Equal(t, "image/jpeg", fi.MIME)
	assert.Greater(t, fi.Width, 0)
	assert.Greater(t, fi.Height, 0)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestDownloadImageTrustedHost|TestTrustedImageHostCaseInsensitive|TestFetchAndConvertImage' .`
Expected: FAIL — `undefined: trustedImageHost` / `undefined: fetchAndConvertImage`; the first subtest's second half fails because downloads are blocked.

- [ ] **Step 3: Implement**

In `image_detect.go`:

After `imageHTTPClient` (~line 292), add:

```go
// imageHTTPClientTrusted skips ONLY the SSRF private-IP dial block, for
// hosts the admin explicitly listed in trusted_image_hosts (imgsite
// typically resolves privately). Every other download guarantee stays:
// scheme, redirects, 30s timeout, size cap, content-type check.
var imageHTTPClientTrusted = &http.Client{Timeout: 30 * time.Second}

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
```

In `downloadImage`, replace `client := imageHTTPClient` with:

```go
	client := imageHTTPClient
	if u, perr := url.Parse(url); perr == nil && trustedImageHost(u.Hostname()) {
		client = imageHTTPClientTrusted
		if logger != nil {
			logger.Debug("trusted image host, skipping SSRF private-IP guard", "host", u.Hostname())
		}
	}
```

(Add `"net/url"` to imports.)

Change `convertImage`'s signature and final return — post-resize bounds are the dimensions the model actually sees, which the ref store records:

```go
func convertImage(imgData []byte, mimeType, format string, quality, maxW, maxH, maxPixels int) (out []byte, dataURI string, width int, height int, err error) {
```

At the top of the resize block, `width`/`height` are already locals (`bounds.Dx()`/`bounds.Dy()`); after the resize branch and before encoding they reflect the final image (the resize reassigns `img` and `bounds`). Change every `return nil, "", fmt.Errorf(...)` in this function to `return nil, "", 0, 0, fmt.Errorf(...)` and the final return to:

```go
	return encodedData, dataURI, bounds.Dx(), bounds.Dy(), nil
```

Add `fetchAndConvertImage` after `convertImage`:

```go
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
```

Refactor `buildImageMessage`'s loop to use it (behavior identical, including the skip-on-error logging and the `errImageTooManyPixels` non-fatal branch):

```go
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

		parts = append(parts, MessagePart{
			Type: PartTypeImageURL,
			ImageURL: &ImageURL{
				URL:    fi.DataURI,
				Detail: ImageDetailAuto,
			},
		})
		successfulURLs = append(successfulURLs, url)
	}
```

Update every direct `convertImage` caller in `image_detect_test.go` (e.g. `TestConvertImage`, `TestConvertImageRejectsPixelBombPNG`, `TestConvertImageConfigurablePixelCap`) to the 5-return form — usually just `_, _, _, _, err := convertImage(...)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestDownloadImageTrustedHost|TestTrustedImageHostCaseInsensitive|TestFetchAndConvertImage|TestConvertImage|TestBuildImageMessage' .`
Expected: PASS, and the whole package still compiles/passes: `go test .`.

- [ ] **Step 5: Commit**

```bash
git add image_detect.go image_detect_test.go
git commit -m "feat: trusted-host image download bypass + shared fetchAndConvertImage"
```

---

### Task 4: Paste ref pairing in `buildImageMessage`

**Files:**
- Modify: `image_detect.go` (`buildImageMessage` ~line 512)
- Modify: `aiCmds.go` (`chat()` call site, the `buildImageMessage(` call inside `if cfg.DetectImages`, ~line 2220)
- Test: `image_detect_test.go`

**Interfaces:**
- Consumes: `registerImageRef` (Task 2), `fetchAndConvertImage` (Task 3).
- Produces: CHANGED `buildImageMessage(text string, imageUrls []string, maxImages int, format string, quality, maxW, maxH, maxPixels int, sessionID int64) (ChatMessage, error)` — pairs each image part with a preceding label text part `[image N — ref: usr_xxxx — <url>]` and registers each image (origin `"user"`).

- [ ] **Step 1: Write the failing test**

Add to `image_detect_test.go`:

```go
func TestBuildImageMessageRefPairing(t *testing.T) {
	setupTestDB(t)
	config.ImagesDir = t.TempDir()
	t.Cleanup(func() { config.ImagesDir = "" })
	config.TrustedImageHosts = []string{"127.0.0.1"}
	t.Cleanup(func() { config.TrustedImageHosts = nil })

	srv := newTestImageServer(t)
	defer srv.Close()

	msg, err := buildImageMessage("look at this " + srv.URL + "/a.jpg " + srv.URL + "/b.jpg",
		[]string{srv.URL + "/a.jpg", srv.URL + "/b.jpg"}, 5, "jpg", 75, 1024, 1024, 0, 7)
	require.NoError(t, err)
	require.Len(t, msg.MultiContent, 5, "leading text part + (label, image) x2")

	assert.Equal(t, PartTypeText, msg.MultiContent[0].Type)
	assert.Contains(t, msg.MultiContent[0].Text, "look at this")

	for i, pair := range []int{1, 3} {
		label := msg.MultiContent[pair]
		img := msg.MultiContent[pair+1]
		assert.Equal(t, PartTypeText, label.Type)
		assert.Regexp(t, fmt.Sprintf(`^\[image %d — ref: usr_[0-9a-f]{12} —`, i+1), label.Text,
			"label precedes its image, 1-indexed, carries a usr_ ref and the URL")
		assert.Contains(t, label.Text, srv.URL)
		assert.Equal(t, PartTypeImageURL, img.Type)
		assert.Contains(t, img.ImageURL.URL, "data:image/jpeg;base64,")
	}

	// Both refs registered with the session id.
	var count int64
	require.NoError(t, theDB.Model(&ImageRef{}).Where("session_id = ? AND origin = ?", 7, "user").Count(&count).Error)
	assert.Equal(t, int64(2), count)
}
```

(Add `"fmt"` to the test imports if not present.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -run TestBuildImageMessageRefPairing .`
Expected: FAIL — `too many arguments in call to buildImageMessage`.

- [ ] **Step 3: Implement**

Change the signature and the success branch of the loop in `buildImageMessage`:

```go
func buildImageMessage(text string, imageUrls []string, maxImages int, format string, quality, maxW, maxH, maxPixels int, sessionID int64) (ChatMessage, error) {
```

```go
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
```

(Design note to keep as a comment above the loop: the label part PRECEDES its image part — the old image-editing plan's "adjacent text block" pairing, so the model visually maps ref → image.)

Update the `chat()` call site in `aiCmds.go` (~line 2220) to pass `session.ID`:

```go
			userMsg, err = buildImageMessage(cleanText, imageUrls, cfg.MaxImages, cfg.ImageFormat, cfg.ImageQuality, cfg.MaxImageWidth, cfg.MaxImageHeight, maxImagePixels, session.ID)
```

Update every other `buildImageMessage` caller in `image_detect_test.go` (`TestBuildImageMessageNonImageURLPreserved`, `TestBuildImageMessageSkipsPixelBombNonFatal`, and any others — find with `grep -n buildImageMessage *_test.go`) to append a session id (0 is fine for tests that don't assert refs).

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run TestBuildImageMessage . && go test .`
Expected: PASS across the package.

- [ ] **Step 5: Commit**

```bash
git add image_detect.go image_detect_test.go aiCmds.go
git commit -m "feat: user pastes pair images with usr_ ref labels"
```

---

### Task 5: `view_image` builtin tool

**Files:**
- Modify: `image_detect.go` (handler — image pipeline home)
- Modify: `aiCmds.go` (`chatRunner` struct field ~line 220; `builtinTools` map ~line 2336; `getBuiltinToolDefs` gate ~line 2466; `executeToolCalls` flush before `return registeredJob` ~line 1538)
- Test: `image_detect_test.go` (handler family)

**Interfaces:**
- Consumes: `fetchAndConvertImage`, `registerImageRef`, `countContextImages`, `toolResultMsg`, `turnContext`, `readConfig`, `config.MaxImagePixels`.
- Produces:
  - `const viewImageToolName = "view_image"`
  - `(cr *chatRunner) handleViewImage(turn *turnContext, call ToolCall) bool`
  - `rootMaxImagePixels() int`
  - `chatRunner.pendingInjections []ChatMessage` — flushed by `executeToolCalls` after the tool loop
  - `view_image` def in `builtinTools`, gated on `cfg.DetectImages` in `getBuiltinToolDefs`

- [ ] **Step 1: Write the failing tests**

Add to `image_detect_test.go` (runner literal mirrors `makeResponsesRunner` in aiCmds_test.go:439 — copy the field set from there):

```go
func newImageViewTestRunner(t *testing.T, cfg AIConfig, sid int64) *chatRunner {
	t.Helper()
	setupNoticesDefaults(t)
	logger := logxi.New("test")
	logger.SetLevel(logxi.LevelAll)
	return &chatRunner{
		cfg:       cfg,
		network:   Network{Name: "testnet"},
		channel:   "#101",
		nick:      "shrew",
		userID:    ensureTestUser(t, "testnet", "shrew"),
		sessionID: sid,
		logger:    logger,
		ctx:       context.Background(),
		outputCh:  make(chan string, 100),
	}
}

func TestExecuteToolCallsViewImageInjectsLabeledMessage(t *testing.T) {
	setupTestDB(t)
	config.ImagesDir = t.TempDir()
	t.Cleanup(func() { config.ImagesDir = "" })
	config.TrustedImageHosts = []string{"127.0.0.1"}
	t.Cleanup(func() { config.TrustedImageHosts = nil })

	srv := newTestImageServer(t)
	defer srv.Close()

	cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 5, ImageFormat: "jpg", ImageQuality: 75}, 9)
	turn := newTurnContext(9, nil)
	args, _ := json.Marshal(map[string]string{"url": srv.URL + "/pic.jpg"})
	cr.executeToolCalls(turn, []ToolCall{{
		ID: "call_1", Type: "function",
		Function: FunctionCall{Name: viewImageToolName, Arguments: string(args)},
	}})

	msgs := turn.Messages()
	require.GreaterOrEqual(t, len(msgs), 2)
	toolMsg := msgs[len(msgs)-2]
	injected := msgs[len(msgs)-1]
	assert.Equal(t, RoleTool, toolMsg.Role)
	assert.Contains(t, toolMsg.Content, "Image loaded")
	assert.Contains(t, toolMsg.Content, "ref: web_")

	assert.Equal(t, RoleUser, injected.Role)
	require.Len(t, injected.MultiContent, 2)
	assert.Equal(t, PartTypeText, injected.MultiContent[0].Type)
	assert.Regexp(t, `^\[image — ref: web_[0-9a-f]{12} —`, injected.MultiContent[0].Text)
	assert.Contains(t, injected.MultiContent[0].Text, srv.URL)
	assert.Equal(t, PartTypeImageURL, injected.MultiContent[1].Type)
	assert.Contains(t, injected.MultiContent[1].ImageURL.URL, "data:image/jpeg;base64,")

	// Persisted (non-ephemeral turn) + registered in the store.
	var refCount int64
	require.NoError(t, theDB.Model(&ImageRef{}).Where("session_id = ? AND origin = ?", 9, "view").Count(&refCount).Error)
	assert.Equal(t, int64(1), refCount)
	dbMsgs, err := sessionMgr.GetMessages(9)
	require.NoError(t, err)
	assert.Equal(t, RoleUser, dbMsgs[len(dbMsgs)-1].Role, "injection persisted to the session")
}

func TestExecuteToolCallsFlushesInjectionAfterAllResults(t *testing.T) {
	setupTestDB(t)
	config.ImagesDir = t.TempDir()
	t.Cleanup(func() { config.ImagesDir = "" })
	config.TrustedImageHosts = []string{"127.0.0.1"}
	t.Cleanup(func() { config.TrustedImageHosts = nil })

	srv := newTestImageServer(t)
	defer srv.Close()

	cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 5, ImageFormat: "jpg", ImageQuality: 75}, 9)
	turn := newTurnContext(9, nil)
	turn.Add(ChatMessage{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "call_1"}, {ID: "call_2"}}})

	viewArgs, _ := json.Marshal(map[string]string{"url": srv.URL + "/pic.jpg"})
	cr.executeToolCalls(turn, []ToolCall{
		{ID: "call_1", Type: "function", Function: FunctionCall{Name: viewImageToolName, Arguments: string(viewArgs)}},
		{ID: "call_2", Type: "function", Function: FunctionCall{Name: "unknown_tool_zz", Arguments: "{}"}},
	})

	msgs := turn.Messages()
	require.GreaterOrEqual(t, len(msgs), 3)
	assert.Equal(t, RoleTool, msgs[len(msgs)-3].Role, "first tool result")
	assert.Equal(t, RoleTool, msgs[len(msgs)-2].Role, "second tool result — injection must not sit between tool results")
	assert.Equal(t, RoleUser, msgs[len(msgs)-1].Role, "injection flushed after ALL results")
}

func TestHandleViewImageBudgetRefusal(t *testing.T) {
	setupTestDB(t)
	cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 2, ImageFormat: "jpg"}, 9)
	turn := newTurnContext(9, []ChatMessage{{Role: RoleSystem, Content: "s"}})
	turn.Add(ChatMessage{Role: RoleUser, MultiContent: []MessagePart{
		{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/jpeg;base64,AAA"}},
		{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/jpeg;base64,BBB"}},
	}})
	args, _ := json.Marshal(map[string]string{"url": "https://img.example.com/x.jpg"})
	cr.executeToolCalls(turn, []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: viewImageToolName, Arguments: string(args)}}})

	last := turn.Messages()[len(turn.Messages())-1]
	assert.Equal(t, RoleTool, last.Role, "over budget: tool result refusal, no injection")
	assert.Contains(t, last.Content, "budget reached")
	assert.Empty(t, cr.pendingInjections)
}

func TestHandleViewImageEphemeralRejected(t *testing.T) {
	setupTestDB(t)
	cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 5}, 0)
	turn := newEphemeralTurnContext(nil)
	args, _ := json.Marshal(map[string]string{"url": "https://img.example.com/x.jpg"})
	cr.executeToolCalls(turn, []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: viewImageToolName, Arguments: string(args)}}})
	last := turn.Messages()[len(turn.Messages())-1]
	assert.Equal(t, RoleTool, last.Role)
	assert.Contains(t, last.Content, "not available")
}

func TestHandleViewImageConvertFailure(t *testing.T) {
	setupTestDB(t)
	// Serves image content-type with garbage bytes: download passes, decode fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("this is not a png"))
	}))
	defer srv.Close()
	config.TrustedImageHosts = []string{"127.0.0.1"}
	t.Cleanup(func() { config.TrustedImageHosts = nil })

	cr := newImageViewTestRunner(t, AIConfig{DetectImages: true, MaxContextImages: 5, ImageFormat: "jpg"}, 9)
	turn := newTurnContext(9, nil)
	args, _ := json.Marshal(map[string]string{"url": srv.URL + "/bad.png"})
	cr.executeToolCalls(turn, []ToolCall{{ID: "c1", Type: "function", Function: FunctionCall{Name: viewImageToolName, Arguments: string(args)}}})
	last := turn.Messages()[len(turn.Messages())-1]
	assert.Equal(t, RoleTool, last.Role, "convert failure surfaces as tool result, never a panic")
	assert.Contains(t, last.Content, "error:")
	assert.Empty(t, cr.pendingInjections)
}

func TestGetBuiltinToolDefsViewImageGatedOnDetectImages(t *testing.T) {
	with := getBuiltinToolDefs(AIConfig{DetectImages: true}, nil)
	found := false
	for _, tl := range with {
		if tl.Function != nil && tl.Function.Name == viewImageToolName {
			found = true
		}
	}
	assert.True(t, found, "offered on vision commands")

	without := getBuiltinToolDefs(AIConfig{DetectImages: false}, nil)
	for _, tl := range without {
		if tl.Function != nil {
			assert.NotEqual(t, viewImageToolName, tl.Function.Name, "not offered on non-vision commands")
		}
	}

	disabled := getBuiltinToolDefs(AIConfig{DetectImages: true}, []string{viewImageToolName})
	for _, tl := range disabled {
		if tl.Function != nil {
			assert.NotEqual(t, viewImageToolName, tl.Function.Name, "disabled_builtin_tools opts out")
		}
	}
}
```

NOTE: `getBuiltinToolDefs` returns `[]Tool` (no error) and reads the global `config` (bans) — initialize the global config the way other tests that touch it do (`setupNoticesDefaults(t)` plus setting `config` under the test's control) if it panics on a zero config.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestExecuteToolCallsViewImage|TestExecuteToolCallsFlushes|TestHandleViewImage|TestGetBuiltinToolDefsViewImage' .`
Expected: FAIL — `undefined: viewImageToolName` and no `pendingInjections` field.

- [ ] **Step 3: Implement**

In `aiCmds.go`, add to the `chatRunner` struct (~line 220, near `deliveryPending`):

```go
	// pendingInjections collects tool-loop messages (view_image's labeled
	// image message) that must land AFTER all tool results of the round —
	// a tool result after an injected user message breaks providers'
	// tool_calls ordering. Flushed by executeToolCalls when the loop ends.
	pendingInjections []ChatMessage
```

At the end of `executeToolCalls`, just before `return registeredJob` (~line 1538):

```go
	// Tool-loop image injections (view_image) flush AFTER all tool results
	// of the round — never between them (see pendingInjections).
	for _, msg := range cr.pendingInjections {
		turn.Add(msg)
	}
	cr.pendingInjections = nil
```

Add the `builtinTools` entry (map at ~line 2336):

```go
	viewImageToolName: {
		handler: func(cr *chatRunner, turn *turnContext, call ToolCall) bool {
			cr.handleViewImage(turn, call)
			return false
		},
		def: Tool{
			Type: "function",
			Function: &FunctionDefinition{
				Name:        viewImageToolName,
				Description: "Download an image from a URL and view it. Use for images found in fetched pages or image links (including generated image pages). Each call views one image; the image is attached in the message right after the tool result, labeled with a reference ID. Subject to the context image budget.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]any{
							"type":        "string",
							"description": "The http(s) URL of the image to view",
						},
					},
					"required": []string{"url"},
				},
			},
		},
	},
```

In `getBuiltinToolDefs`' assembly loop (~line 2466), after the disabled check and before the per-tool description rewrites:

```go
		if t.Function.Name == viewImageToolName && !cfg.DetectImages {
			// view_image presupposes a vision-capable command; detectimages
			// is the admin's existing declaration of that. Offering it on a
			// non-vision command would attach images the model cannot see.
			continue
		}
```

In `image_detect.go`, add the handler (with a design comment noting the owner directive: generated images are never auto-injected; the model views them explicitly through this tool):

```go
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
// directive: context economy); the model views them explicitly here.
// Every failure is tool-result text the model can act on.
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
	u, err := url.Parse(args.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		turn.Add(toolResultMsg(call.ID, "error: url must be an http(s) URL"))
		return false
	}
	remaining := cr.cfg.MaxContextImages - countContextImages(turn.Messages())
	if remaining <= 0 {
		turn.Add(toolResultMsg(call.ID, fmt.Sprintf(
			"error: context image budget reached (max %d images in the conversation). Tell the user to start a new session.",
			cr.cfg.MaxContextImages)))
		return false
	}
	fi, err := fetchAndConvertImage(args.URL, cr.cfg.ImageFormat, cr.cfg.ImageQuality, cr.cfg.MaxImageWidth, cr.cfg.MaxImageHeight, rootMaxImagePixels())
	if err != nil {
		turn.Add(toolResultMsg(call.ID, "error: "+err.Error()))
		return false
	}
	ref, refErr := registerImageRef(cr.sessionID, "view", args.URL, fi.MIME, fi.Width, fi.Height, fi.Encoded)
	if refErr != nil {
		cr.logger.Warn("image ref registration failed (image still attached)", "url", args.URL, "error", refErr.Error())
	}
	if ref == "" {
		ref = "unregistered"
	}
	turn.Add(toolResultMsg(call.ID, fmt.Sprintf(
		"Image loaded and attached in the next message — ref: %s. Source: %s (%dx%d).",
		ref, args.URL, fi.Width, fi.Height)))
	cr.pendingInjections = append(cr.pendingInjections, ChatMessage{
		Role: RoleUser,
		MultiContent: []MessagePart{
			// Label PRECEDES the image — the ref-pairing contract: the model
			// visually maps ref -> image (old image-editing plan).
			{Type: PartTypeText, Text: fmt.Sprintf("[image — ref: %s — source: %s]", ref, args.URL)},
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: fi.DataURI, Detail: ImageDetailAuto}},
		},
	})
	return false
}
```

(Add `"encoding/json"`, `"net/url"`, `"fmt"` to image_detect.go imports as needed.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestExecuteToolCallsViewImage|TestExecuteToolCallsFlushes|TestHandleViewImage|TestGetBuiltinToolDefsViewImage' . && go test .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add image_detect.go image_detect_test.go aiCmds.go
git commit -m "feat: view_image builtin — model-initiated image injection with refs"
```

---

### Task 6: Responses chained tool-loop input carries injections

**Files:**
- Modify: `responses.go` (extract `userMsgToResponseInputItem` from `messagesToResponseInputItems` ~lines 31-63; replace `toolResultMsgsToInputItems` ~line 195)
- Modify: `turncontext.go` (add `MessagesFrom`)
- Modify: `aiCmds.go` (two call sites: streaming ~lines 941-957, non-streaming ~lines 1905-1916)
- Test: `responses_test.go`, `aiCmds_test.go`

**Interfaces:**
- Consumes: existing `messagesToResponseInputItems` user branch, `turn.LastN` (still used elsewhere — do not remove).
- Produces:
  - `(tc *turnContext) MessagesFrom(idx int) []ChatMessage`
  - `userMsgToResponseInputItem(msg ChatMessage) responses.ResponseInputItemUnionParam`
  - `toolLoopMsgsToInputItems(messages []ChatMessage) []responses.ResponseInputItemUnionParam` (replaces `toolResultMsgsToInputItems`; handles `RoleTool` AND `RoleUser`, skips everything else)

- [ ] **Step 1: Write the failing tests**

In `responses_test.go`:

```go
func TestToolLoopMsgsToInputItemsCarriesInjectedUser(t *testing.T) {
	msgs := []ChatMessage{
		{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "c1"}}}, // must be skipped
		{Role: RoleTool, ToolCallID: "c1", Content: "Image loaded"},
		{Role: RoleUser, MultiContent: []MessagePart{
			{Type: PartTypeText, Text: "[image — ref: web_abc — source: u]"},
			{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/jpeg;base64,AAA", Detail: ImageDetailAuto}},
		}},
	}
	input := toolLoopMsgsToInputItems(msgs)
	require.Len(t, input, 2)
	s := string(input[1].RawJSON()) // marshal via the SDK's raw representation, or json.Marshal the union — use whichever the existing responses_test.go wire-shape tests use (see the input_image assertion pattern ~line 326)
	assert.Contains(t, s, "input_image", "injected user message must ride the chained delta input")
	assert.Contains(t, s, "data:image/jpeg;base64,AAA")
}
```

NOTE: mirror however `responses_test.go` already asserts wire shapes (there is an existing test asserting `{"type": "input_image", "image_url": ...}` around line 326 — copy its marshaling approach exactly).

In `turncontext` area (or an existing turncontext test file):

```go
func TestTurnContextMessagesFrom(t *testing.T) {
	tc := newTurnContext(1, []ChatMessage{{Role: RoleSystem, Content: "s"}})
	tc.Add(ChatMessage{Role: RoleUser, Content: "a"})
	tc.Add(ChatMessage{Role: RoleAssistant, Content: "b"})
	assert.Len(t, tc.MessagesFrom(1), 2)
	assert.Len(t, tc.MessagesFrom(0), 3)
	assert.Nil(t, tc.MessagesFrom(3))
	assert.Len(t, tc.MessagesFrom(-5), 3, "negative index clamps to 0")
}
```

In `aiCmds_test.go` — the money test, modeled directly on `TestRunTurnResponsesToolLoopChainsFreshID` (line 1033; copy its setup helpers: `setupTestDB`, `setupNoticesDefaults`, `setupSessionWithResponseID`, `makeResponsesAPIToolCallResponse`, `makeResponsesRunner`, `parseRecordedRequestBody`):

```go
// TestRunTurnResponsesViewImageChainDeliversImage pins the Review Focus #2
// hazard: on a previous_response_id chain the tool-loop follow-up sends
// delta-only input; the old builder (turn.LastN(numToolCalls) filtered to
// tool roles) SILENTLY DROPPED the flushed view_image injection.
func TestRunTurnResponsesViewImageChainDeliversImage(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)
	config.ImagesDir = t.TempDir()
	t.Cleanup(func() { config.ImagesDir = "" })

	// Image server: loopback + trusted so downloadImage passes the SSRF guard.
	imgBytes := createTestJPEG(t)
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(imgBytes)
	}))
	defer imgSrv.Close()
	config.TrustedImageHosts = []string{"127.0.0.1"}
	t.Cleanup(func() { config.TrustedImageHosts = nil })

	sid := setupSessionWithResponseID(t, "")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "look"}))

	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, string(raw))
		mu.Unlock()
		var payload map[string]any
		if n == 0 {
			viewArgs, _ := json.Marshal(map[string]string{"url": imgSrv.URL + "/pic.jpg"})
			payload = makeResponsesAPIToolCallResponse("resp-1", "view_image", string(viewArgs))
		} else {
			payload = makeResponsesAPIResponse("resp-2", "done")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	}))
	defer server.Close()

	cfg := AIConfig{
		Model: "test-model", ResponsesAPI: true, PreviousResponseID: true,
		Timeout: 10 * time.Second, StreamTimeout: 10 * time.Second,
	}
	runner := makeResponsesRunner(t, server.URL, sid, cfg)
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	runner.runTurn(newTurnContext(sid, messages))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 2, "tool round-trip + follow-up")
	prevID1, input1 := parseRecordedRequestBody(t, bodies[1])
	assert.Equal(t, "resp-1", prevID1, "follow-up must chain")
	joined := ""
	for _, item := range input1 {
		joined += string(item)
	}
	assert.Contains(t, joined, "function_call_output", "tool result rides the delta input")
	assert.Contains(t, joined, "input_image", "view_image injection MUST ride the chained delta input")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestToolLoopMsgsToInputItems|TestTurnContextMessagesFrom|TestRunTurnResponsesViewImageChainDeliversImage' .`
Expected: FAIL — `undefined: toolLoopMsgsToInputItems`, `undefined: MessagesFrom`; the runTurn test fails on the missing builder (compile).

- [ ] **Step 3: Implement**

In `turncontext.go`:

```go
// MessagesFrom returns messages[idx:] (clamped). The chained Responses
// tool-loop delta input is "everything after the assistant tool-call
// message", which includes tool results AND flushed image injections —
// LastN(numToolCalls) cannot express that.
func (tc *turnContext) MessagesFrom(idx int) []ChatMessage {
	if idx < 0 {
		idx = 0
	}
	if idx >= len(tc.messages) {
		return nil
	}
	return tc.messages[idx:]
}
```

In `responses.go`: extract the existing `RoleUser` branch of `messagesToResponseInputItems` VERBATIM (same types, same shapes — the wire shape is pinned by existing tests around line 326) into:

```go
// userMsgToResponseInputItem converts a user message (multimodal or plain)
// to its Responses input item. Extracted from messagesToResponseInputItems
// so the tool-loop delta builder carries injected image messages with
// byte-identical shapes to the full-history serializer.
func userMsgToResponseInputItem(msg ChatMessage) responses.ResponseInputItemUnionParam {
	// ... the existing RoleUser branch body, returning the item instead of
	// appending to the local slice ...
}
```

(the full-history serializer then calls it in its `RoleUser` case).

Replace `toolResultMsgsToInputItems` with:

```go
// toolLoopMsgsToInputItems builds the delta-only input for a chained
// Responses tool round-trip: everything after the assistant tool-call
// message. Tool results become function_call_output; queued image
// injections (RoleUser multimodal) convert like any user message — the old
// tool-results-only builder silently dropped them (spec §3). Assistant rows
// are skipped: the chained server already knows the assistant message that
// produced the calls.
func toolLoopMsgsToInputItems(messages []ChatMessage) []responses.ResponseInputItemUnionParam {
	input := make([]responses.ResponseInputItemUnionParam, 0, len(messages))
	for _, msg := range messages {
		switch msg.Role {
		case RoleTool:
			input = append(input, responses.ResponseInputItemParamOfFunctionCallOutput(
				msg.ToolCallID, msg.Content,
			))
		case RoleUser:
			input = append(input, userMsgToResponseInputItem(msg))
		}
	}
	return input
}
```

Update both call sites in `aiCmds.go`:

Streaming site (~line 936): capture the index BEFORE the assistant message is added:

```go
	preToolIdx := len(turn.Messages())
	assistantMsg.ToolCalls = toolCalls
	turn.Add(assistantMsg)
```

then replace the chain branch (~line 955):

```go
	if chain {
		newInput = toolLoopMsgsToInputItems(turn.MessagesFrom(preToolIdx))
	} else {
```

(remove the now-unused `numToolCalls` at ~line 941 if nothing else uses it).

Non-streaming site (~line 1905): capture before `handleToolCallResponse` (it adds the assistant message, tool results, and the flushed injections):

```go
			preToolIdx := len(turn.Messages())
			cr.handleToolCallResponse(turn, text, toolCalls, parts)
```

then replace the chain branch (~line 1913):

```go
		if chainActive(cr.cfg, currentResponseID) {
			input = toolLoopMsgsToInputItems(turn.MessagesFrom(preToolIdx))
			// Input is now delta-only (tool results + injections): the next
			// request must chain currentResponseID or the provider loses all
			// context.
			usePrevID = true
		} else {
```

(remove the now-unused `numToolCalls` if nothing else uses it).

If any test referenced `toolResultMsgsToInputItems` by name, update it to `toolLoopMsgsToInputItems`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -run 'TestToolLoopMsgsToInputItems|TestTurnContextMessagesFrom|TestRunTurnResponsesViewImageChainDeliversImage' .`
Expected: PASS. Then the full Responses regression family: `go test -run 'TestRunTurnResponses|TestMessagesToResponse' .` — the existing chain tests (`TestRunTurnResponsesToolLoopChainsFreshID` asserts the follow-up input is EXACTLY the tool result when no injection exists; the toolLoop builder still produces just that) must stay green.

- [ ] **Step 5: Commit**

```bash
git add responses.go turncontext.go aiCmds.go responses_test.go aiCmds_test.go turncontext_test.go
git commit -m "fix: chained Responses tool-loop input carries injected image messages"
```

---

### Task 7: fetch-mcp binary-content guard

**Files:**
- Modify: `mcps/fetch-mcp/fetcher.go` (`Fetch` non-HTML branch ~line 139; new helpers)
- Test: `mcps/fetch-mcp/fetcher_test.go`

**Interfaces:**
- Consumes: existing `Fetcher.Fetch`, `isHTML`, test conventions in `fetcher_test.go` (plain `httptest` + `testify`; construct `NewFetcher` the way existing fetcher tests do — copy their `Config` literal).
- Produces: `mediaType(contentType string) string`, `binaryContentStub(mt string, size int) (string, bool)`, `humanSize(b int) string`; image responses return a stub naming `view_image`, other binaries a generic stub, textual types keep the raw passthrough; stubs are never markdown-cached.

- [ ] **Step 1: Write the failing tests**

Add to `mcps/fetch-mcp/fetcher_test.go` (mirror the Fetcher construction from an existing test in that file):

```go
func TestFetchBinaryImageStub(t *testing.T) {
	pngBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(pngBytes)
	}))
	defer srv.Close()

	f := newTestFetcher(t)
	res, err := f.Fetch(context.Background(), srv.URL, 0, 20000)
	require.NoError(t, err)
	assert.Contains(t, res.Markdown, "[binary image content (image/png,")
	assert.Contains(t, res.Markdown, "use the view_image tool")
	assert.NotContains(t, res.Markdown, "\x89PNG", "no mangled binary in the model context")
}

func TestFetchBinaryAudioStub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.Write([]byte("ID3 fakemp3 bytes"))
	}))
	defer srv.Close()

	f := newTestFetcher(t)
	res, err := f.Fetch(context.Background(), srv.URL, 0, 20000)
	require.NoError(t, err)
	assert.Contains(t, res.Markdown, "[binary content (audio/mpeg,")
}

func TestFetchTextPassthroughUnchanged(t *testing.T) {
	for _, ct := range []string{"text/plain", "application/json", "application/xml; charset=utf-8", "application/geo+json"} {
		t.Run(ct, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", ct)
				w.Write([]byte(`{"a":1}`))
			}))
			defer srv.Close()
			f := newTestFetcher(t)
			res, err := f.Fetch(context.Background(), srv.URL, 0, 20000)
			require.NoError(t, err)
			assert.Equal(t, `{"a":1}`, res.Markdown)
		})
	}
}
```

Add the shared constructor in the same file:

```go
// newTestFetcher builds a Fetcher the way existing fetcher tests do; no
// markdown cache (stubs and raw content must never be cached).
func newTestFetcher(t *testing.T) *Fetcher {
	t.Helper()
	f, err := NewFetcher(testFetcherConfig(), nil)
	require.NoError(t, err)
	return f
}
```

If existing tests in the file already build a Fetcher/Config differently (a `testFetcherConfig` may not exist), copy THEIR literal — do not invent a new convention.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -run 'TestFetchBinary|TestFetchTextPassthrough' ./mcps/fetch-mcp`
Expected: image/audio tests FAIL (mangled bytes come back); note whether `NewFetcher(cfg, nil)` panics on a nil cache — if it does, pass an empty cache constructed the way existing tests do, and adjust `Fetch` if needed so the stub path never touches the cache either way.

- [ ] **Step 3: Implement**

In `mcps/fetch-mcp/fetcher.go`, add helpers:

```go
// mediaType strips parameters: "text/html; charset=utf-8" -> "text/html".
func mediaType(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i != -1 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

// textualContentTypes are the non-HTML content types returned as-is.
var textualContentTypes = map[string]bool{
	"application/json": true, "application/xml": true,
	"application/javascript": true, "application/ecmascript": true,
	"application/yaml": true, "application/x-yaml": true,
	"application/toml": true, "application/rss+xml": true,
	"application/atom+xml": true, "application/x-www-form-urlencoded": true,
}

// binaryContentStub classifies a non-HTML response: images get a stub that
// points the model at dave's view_image tool; other binaries get a generic
// stub; textual types return ok=false (raw passthrough). Previously ALL
// non-HTML bodies were passed through string(bodyBytes) — fetching an image
// URL returned mangled binary into the model's context.
func binaryContentStub(mt string, size int) (string, bool) {
	if strings.HasPrefix(mt, "image/") {
		return fmt.Sprintf("[binary image content (%s, %s) — use the view_image tool to view it]", mt, humanSize(size)), true
	}
	if strings.HasPrefix(mt, "text/") || textualContentTypes[mt] ||
		strings.HasSuffix(mt, "+xml") || strings.HasSuffix(mt, "+json") {
		return "", false
	}
	return fmt.Sprintf("[binary content (%s, %s) — not fetchable as text]", mt, humanSize(size)), true
}

func humanSize(b int) string {
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
```

In `Fetch`, guard the non-HTML branch (currently `if !isHTML(contentType, bodyBytes) {` ~line 140) — the stub returns BEFORE any markdown caching (the raw branch never cached anyway, and the stub must not enter the cache either):

```go
	contentType := resp.Header.Get("Content-Type")
	if !isHTML(contentType, bodyBytes) {
		if stub, isBinary := binaryContentStub(mediaType(contentType), len(bodyBytes)); isBinary {
			return &FetchResult{
				Markdown:     stub,
				CacheStatus:  cacheStatus,
				IsRawContent: true,
				StartIndex:   startIndex,
			}, nil
		}
		content := string(bodyBytes)
		// ... existing pagination + return unchanged ...
	}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./mcps/fetch-mcp`
Expected: PASS (new tests + all existing fetch/cache/tools tests).

- [ ] **Step 5: Commit**

```bash
git add mcps/fetch-mcp/fetcher.go mcps/fetch-mcp/fetcher_test.go
git commit -m "feat: fetch-mcp binary-content stubs (images point at view_image)"
```

---

### Task 8: Chat Completions e2e + docs + full verification

**Files:**
- Test: `aiCmds_test.go`
- Modify: `config/chats.toml` (builtin tools reference — near the `disabled_builtin_tools`/`hidden_tools` entries)
- Modify: `AGENTS.md` (feature bullet)
- No production code expected — this task verifies and documents.

**Interfaces:**
- Consumes: everything from Tasks 1-7.
- Produces: `TestRunTurnViewImageSeesImageChatPath`; docs.

- [ ] **Step 1: Write the failing-passing e2e (verification test — it should PASS with Tasks 1-6 done; if it fails, the Chat Completions wiring is broken somewhere)**

In `aiCmds_test.go`:

```go
// TestRunTurnViewImageSeesImageChatPath: end-to-end on the Chat Completions
// path — round 1 returns a view_image tool call, round 2's REQUEST must
// contain the injected labeled image message after the tool result.
func TestRunTurnViewImageSeesImageChatPath(t *testing.T) {
	setupTestDB(t)
	setupNoticesDefaults(t)
	config.ImagesDir = t.TempDir()
	t.Cleanup(func() { config.ImagesDir = "" })

	imgBytes := createTestJPEG(t)
	imgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(imgBytes)
	}))
	defer imgSrv.Close()
	config.TrustedImageHosts = []string{"127.0.0.1"}
	t.Cleanup(func() { config.TrustedImageHosts = nil })

	sid := setupSessionWithResponseID(t, "")
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
	require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "what is this"}))

	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(bodies)
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if n == 0 {
			viewArgs, _ := json.Marshal(map[string]string{"url": imgSrv.URL + "/pic.jpg"})
			fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"view_image","arguments":%s}}]}}]}`, viewArgs)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"it is a test jpeg"}}]}`)
	}))
	defer server.Close()

	cfg := AIConfig{Model: "test-model", Timeout: 10 * time.Second, StreamTimeout: 10 * time.Second}
	runner := makeResponsesRunner(t, server.URL, sid, cfg) // runner builder works for either API — it just wires the client
	messages, err := sessionMgr.GetMessages(sid)
	require.NoError(t, err)
	runner.runTurn(newTurnContext(sid, messages))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 2)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(bodies[1]), &req))
	var sawTool, sawImage bool
	for _, m := range req.Messages {
		if m.Role == "tool" {
			sawTool = true
		}
		if m.Role == "user" {
			for _, p := range m.Content {
				if p.Type == "image_url" {
					sawImage = true
				}
			}
		}
	}
	assert.True(t, sawTool, "round-2 request carries the tool result")
	assert.True(t, sawImage, "round-2 request carries the injected image")
}
```

Run: `go test -run TestRunTurnViewImageSeesImageChatPath .` — Expected: PASS. If it FAILS, stop and diagnose (systematic-debugging) before touching anything else; the wiring bug is in Tasks 3-6.

- [ ] **Step 2: Update docs**

`config/chats.toml` — in the reference section where builtin tools are documented (near `disabled_builtin_tools` / `hidden_tools`), add:

```toml
# Builtin tool: view_image — offered only when detectimages = true (the
#   command is vision-capable). Lets the model pull any image URL into the
#   conversation (fetched pages, generated image pages). Each viewed image is
#   budgeted by maxcontextimages and labeled with a ref id (web_...) usable by
#   future image-editing tools. Not hidden by default (tool notices show it);
#   disable per command/service via disabled_builtin_tools = ["view_image"].
```

`AGENTS.md` — in the Architecture section (near the image-detection/config bullets), add:

```markdown
- **Conversation images** (`image_detect.go` + `imageref.go`, Nov 2026; spec `docs/superpowers/specs/2026-11-06-conversation-images-design.md`): the model can pull images into context itself via the `view_image` builtin (gated on `detectimages` + the MCP-tools-exist gate; visible tool notice; `disabled_builtin_tools` opts out). The handler reuses the whole hardened pipeline (SSRF-guarded download — with a `trusted_image_hosts` root-config bypass of the private-IP block so privately-resolving imgsite works — bomb defenses, resize, re-encode), enforces `maxcontextimages` against the live turn, and queues a labeled `RoleUser` message `[image — ref: web_xxxx — source: url] + image part` that `executeToolCalls` flushes AFTER all tool results of the round (an injection between tool results breaks providers' tool_calls ordering). Every image the model sees — pastes (`usr_`) and views (`web_`) — is paired with a ref label and registered in the `image_refs` store (table + file under `images_dir`), which survives compaction (compaction strips image parts; labels survive) and is the seam a future img-mcp edit workflow resolves refs through (`resolveImageRef`; deliberately unused until then, test-pinned). Registration is best-effort: store failure WARNs but the image still attaches. Generated images are NEVER auto-injected (owner directive — context economy); the model views them explicitly via their imgsite URL. The chained Responses tool-loop delta input is built by `toolLoopMsgsToInputItems(turn.MessagesFrom(preToolIdx))` — the old `LastN(numToolCalls)` builder silently dropped injected user messages. fetch-mcp returns a binary stub for non-text content (images: "use the view_image tool") instead of mangled bytes. Guarded by `TestExecuteToolCallsViewImageInjectsLabeledMessage` + `TestExecuteToolCallsFlushesInjectionAfterAllResults` + `TestHandleViewImageBudgetRefusal`/`_EphemeralRejected`/`_ConvertFailure` + `TestGetBuiltinToolDefsViewImageGatedOnDetectImages` + `TestBuildImageMessageRefPairing` + `TestRegisterImageRef*`/`TestCleanupImageRefsByAge`/`TestSweepOrphanImageFiles` + `TestDownloadImageTrustedHost` + `TestToolLoopMsgsToInputItemsCarriesInjectedUser` + `TestRunTurnResponsesViewImageChainDeliversImage` + `TestRunTurnViewImageSeesImageChatPath`.
```

- [ ] **Step 3: Full verification**

```bash
go fmt ./...
go vet ./...
go test ./...
go build -o dave .
go build -o mcps/img-mcp/img-mcp ./mcps/img-mcp
go build -o mcps/yt-mcp/yt-mcp ./mcps/yt-mcp
go build -o imgsite/imgsite ./imgsite
go build -o tools/migrate-sqlite-to-pgsql/migrate-sqlite-to-pgsql ./tools/migrate-sqlite-to-pgsql
go build -o tools/prompt-eval/prompt-eval ./tools/prompt-eval
```

Expected: everything green. Then delete any build artifacts you created inside the tree if they are gitignored (they should be — never `git add -f` them).

- [ ] **Step 4: Commit**

```bash
git add aiCmds_test.go config/chats.toml AGENTS.md
git commit -m "test+docs: conversation images e2e and reference docs"
```

---

## Self-Review notes (already applied)

- Spec coverage: view_image (Task 5), injection shape/ordering (5), Responses delta fix (6), ref store + cleanup + migration mirror (2), paste pairing (4), trusted hosts (1+3), fetch-mcp guard (7), config docs (1+8), AGENTS.md (8), e2e both API paths (6+8). Future-work items (edit workflow, video) are intentionally NOT tasks.
- The spec's "hook into the session MaxAgeDays sweep" is realized as `cleanupImageRefsByAge` riding `CleanupContexts` with the same knob — dave's session cleanup only soft-completes sessions (no auto hard-delete), so age-based ref GC is the honest analogue; noted here so a reviewer doesn't look for a session-delete hook.
- Type consistency: `fetchAndConvertImage` (Task 3) is what Tasks 4 and 5 consume; `registerImageRef`'s `(ref, err)` best-effort contract is identical at both call sites; `toolLoopMsgsToInputItems`/`MessagesFrom` names match between Task 6's definition and call-site edits.
