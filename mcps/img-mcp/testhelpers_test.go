package main

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	initLogger(os.TempDir())
	os.Exit(m.Run())
}

func setupTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := initDB(t.TempDir() + "/test.db")
	require.NoError(t, err, "initDB")
	t.Cleanup(func() { db.Close() })
	return db
}

func testConfig(comfyURL string) Config {
	return Config{
		Comfy: ComfyServiceConfig{
			BaseURL:         comfyURL,
			Timeout:         60,
			DefaultWorkflow: "test",
		},
		Queue: QueueConfig{
			MaxWorkers: 1,
			MaxDepth:   10,
			ResultTTL:  1 * time.Hour,
		},
		Workflows: map[string]WorkflowConfig{
			"test": {
				ClientID:   "test-client",
				OutputNode: "output-node",
				PromptNode: "prompt-node",
				Timeout:    60,
			},
		},
	}
}

func setupTestQueue(t *testing.T, cfg Config) (*JobQueue, func()) {
	t.Helper()
	db := setupTestDB(t)
	q := NewJobQueue(cfg, db)
	return q, func() { q.Stop() }
}

func submitTestJob(t *testing.T, q *JobQueue) *Job {
	t.Helper()
	job, err := q.Submit(JobTypeGenerate, "test", JobInput{
		Prompt: "a cat",
	})
	require.NoError(t, err, "Submit")
	return job
}

func waitForJobDone(t *testing.T, job *Job, timeout time.Duration) {
	t.Helper()
	select {
	case <-job.done:
		return
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for job %s to be done", job.ID)
	}
}

func assertJobStatus(t *testing.T, q *JobQueue, jobID string, expected JobStatus) {
	t.Helper()
	job, ok := q.Get(jobID)
	if !ok {
		t.Fatalf("job %s not found", jobID)
	}
	assert.Equal(t, expected, job.Status, "job status")
}

type mockInterruptServer struct {
	server      *httptest.Server
	mu          sync.Mutex
	interrupts  []map[string]string
	queueDelete [][]string
	promptDelay time.Duration
}

func newMockInterruptServer(t *testing.T) *mockInterruptServer {
	t.Helper()
	m := &mockInterruptServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/interrupt", m.handleInterrupt)
	mux.HandleFunc("/queue", m.handleQueue)
	m.server = httptest.NewServer(mux)
	t.Cleanup(func() { m.server.Close() })
	return m
}

func (m *mockInterruptServer) handleQueue(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Delete []string `json:"delete"`
	}
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	m.mu.Lock()
	m.queueDelete = append(m.queueDelete, req.Delete)
	m.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (m *mockInterruptServer) getQueueDeletes() [][]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]string{}, m.queueDelete...)
}

func (m *mockInterruptServer) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req map[string]string
	json.Unmarshal(body, &req)

	m.mu.Lock()
	m.interrupts = append(m.interrupts, req)
	m.mu.Unlock()

	w.WriteHeader(http.StatusOK)
}

func (m *mockInterruptServer) getInterrupts() []map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]map[string]string{}, m.interrupts...)
}

func (m *mockInterruptServer) URL() string {
	return m.server.URL
}

func mustWriteWorkflow(t *testing.T, dir string) string {
	t.Helper()
	workflow := map[string]ComfyNode{
		"prompt-node": {
			Inputs: map[string]interface{}{"text": ""},
			Class:  "CLIPTextEncode",
		},
		"output-node": {
			Inputs: map[string]interface{}{"images": []string{"1"}},
			Class:  "SaveImage",
		},
	}
	data, err := json.Marshal(workflow)
	require.NoError(t, err)
	path := dir + "/test_workflow.json"
	require.NoError(t, os.WriteFile(path, data, 0644))
	return path
}

func dbJobStatus(t *testing.T, db *sqlx.DB, jobID string) string {
	t.Helper()
	var status string
	err := db.Get(&status, "SELECT status FROM jobs WHERE job_id = ?", jobID)
	require.NoError(t, err, "querying job status")
	return status
}

func dbJobSafety(t *testing.T, db *sqlx.DB, jobID string) string {
	t.Helper()
	var safety string
	err := db.Get(&safety, "SELECT safety FROM jobs WHERE job_id = ?", jobID)
	require.NoError(t, err, "querying job safety")
	return safety
}

// ─── Synthetic webp/TIFF builders (recovery EXIF fixtures) ────────────────
//
// Adapted from imgsite's extract_test.go builders: the same
// production-verified structure ComfyUI's webp writer emits (one IFD0
// ASCII Model-tag entry holding "prompt:"+API-workflow-JSON+NUL inside the
// RIFF EXIF chunk) and img-mcp's embeddedWorkflowJSON reads back. Task 11's
// EXIF rewrite tests build on these too.

// fixtureExifTagModel is the EXIF Model tag (0x0110) — where production
// files carry the workflow payload. Fixture-only on purpose: the production
// extractor is tag-agnostic (it scans every ASCII entry), so the number
// documents the production shape rather than filtering anything; it lives
// next to the builder that encodes it instead of in exif.go.
const fixtureExifTagModel = 0x0110

// buildTestTIFF encodes a one-entry IFD0 (Model tag, ASCII) little-endian
// TIFF, mirroring the verified production structure.
func buildTestTIFF(payload string) []byte {
	bo := binary.ByteOrder(binary.LittleEndian)
	header := make([]byte, 8)
	copy(header[0:2], []byte("II"))
	bo.PutUint16(header[2:4], 42)
	bo.PutUint32(header[4:8], 8) // IFD0 immediately follows the header

	entry := make([]byte, 12)
	bo.PutUint16(entry[0:2], fixtureExifTagModel)
	bo.PutUint16(entry[2:4], tiffTypeASCII)
	bo.PutUint32(entry[4:8], uint32(len(payload)))
	bo.PutUint32(entry[8:12], 8+2+12+4) // value follows header+count+entry+next

	ifd := make([]byte, 18)
	bo.PutUint16(ifd[0:2], 1) // one entry
	copy(ifd[2:14], entry)
	// next-IFD offset stays 0

	out := append(header, ifd...)
	out = append(out, payload...)
	return out
}

// buildTestWebP assembles a minimal RIFF/webp (VP8X + EXIF chunk), applying
// the odd-size pad byte per the container spec.
func buildTestWebP(exif []byte) []byte {
	body := []byte("WEBP")
	body = append(body, []byte("VP8X")...)
	body = append(body, 10, 0, 0, 0)
	body = append(body, make([]byte, 10)...)
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], uint32(len(exif)))
	body = append(body, []byte("EXIF")...)
	body = append(body, sz[:]...)
	body = append(body, exif...)
	if len(exif)%2 == 1 {
		body = append(body, 0)
	}
	var total [4]byte
	binary.LittleEndian.PutUint32(total[:], uint32(len(body)))
	out := append([]byte("RIFF"), total[:]...)
	return append(out, body...)
}

// exifWebPFromWorkflow wraps an arbitrary API workflow in the production EXIF
// shape: a "prompt:"-prefixed workflow JSON in the TIFF Model tag inside a
// webp EXIF chunk.
func exifWebPFromWorkflow(t *testing.T, workflow ComfyWorkflow) []byte {
	t.Helper()
	wfJSON, err := json.Marshal(workflow)
	require.NoError(t, err, "marshaling fixture workflow")
	return buildTestWebP(buildTestTIFF("prompt:" + string(wfJSON) + "\x00"))
}

// exifWebPWithPrompt builds a completed-image fixture: a webp whose EXIF
// embeds an API workflow whose prompt node carries promptText — the only
// place the enhanced prompt survives a crash on the recovery path.
func exifWebPWithPrompt(t *testing.T, promptText string) []byte {
	t.Helper()
	workflow := ComfyWorkflow{
		"prompt-node": {Inputs: map[string]interface{}{"text": promptText}, Class: "CLIPTextEncode"},
		"output-node": {Inputs: map[string]interface{}{"images": []string{"1"}}, Class: "SaveImage"},
	}
	return exifWebPFromWorkflow(t, workflow)
}

// exifWebPWithNote builds a completed-image fixture whose embedded workflow
// also carries a dave_original_prompt note node (text = noteJSON) — the shape
// the EXIF note rewrite operates on. promptText lands in the prompt node so
// tests can assert the enhanced prompt survives the surgery.
func exifWebPWithNote(t *testing.T, noteJSON, promptText string) []byte {
	t.Helper()
	workflow := ComfyWorkflow{
		"prompt-node": {Inputs: map[string]interface{}{"text": promptText}, Class: "CLIPTextEncode"},
		"output-node": {Inputs: map[string]interface{}{"images": []string{"1"}}, Class: "SaveImage"},
		davePromptNoteNodeID: {
			Inputs: map[string]interface{}{"text": noteJSON},
			Class:  "CLIPTextEncode",
			Meta:   &comfyNodeMeta{Title: davePromptNoteTitle},
		},
	}
	return exifWebPFromWorkflow(t, workflow)
}
