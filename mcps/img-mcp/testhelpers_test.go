package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func setupTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	db, err := initDB(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("initDB: %v", err)
	}
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
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
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
	if job.Status != expected {
		t.Errorf("expected status %s, got %s", expected, job.Status)
	}
}

type mockInterruptServer struct {
	server      *httptest.Server
	mu          sync.Mutex
	interrupts  []map[string]string
	promptDelay time.Duration
}

func newMockInterruptServer(t *testing.T) *mockInterruptServer {
	t.Helper()
	m := &mockInterruptServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/interrupt", m.handleInterrupt)
	m.server = httptest.NewServer(mux)
	t.Cleanup(func() { m.server.Close() })
	return m
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
	if err != nil {
		t.Fatal(err)
	}
	path := dir + "/test_workflow.json"
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func dbJobStatus(t *testing.T, db *sqlx.DB, jobID string) string {
	t.Helper()
	var status string
	err := db.Get(&status, "SELECT status FROM jobs WHERE job_id = ?", jobID)
	if err != nil {
		t.Fatalf("querying job status: %v", err)
	}
	return status
}
