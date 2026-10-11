package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRetryableCallErr(t *testing.T) {
	assert.True(t, retryableCallErr(&httpError{status: 500}), "500 is transient")
	assert.True(t, retryableCallErr(&httpError{status: 503}), "503 is transient")
	assert.False(t, retryableCallErr(&httpError{status: 404}), "4xx is the request's own fault")
	assert.False(t, retryableCallErr(&httpError{status: 400}))
	assert.True(t, retryableCallErr(fmt.Errorf(`Post "http://x": EOF`)), "transport-level errors retry")
	assert.True(t, retryableCallErr(fmt.Errorf("read tcp 127.0.0.1:1->127.0.0.1:2: connection reset by peer")))
	assert.False(t, retryableCallErr(context.Canceled), "deliberate cancellation never retries")
	assert.False(t, retryableCallErr(fmt.Errorf("wrapped: %w", context.Canceled)))
	assert.False(t, retryableCallErr(timeoutError{}), "a bare net.Error client timeout never retries")
	assert.False(t, retryableCallErr(&url.Error{Op: "Post", URL: "http://x", Err: timeoutError{}}),
		"the *url.Error client.Do wraps around a timeout never retries")
	assert.False(t, retryableCallErr(context.DeadlineExceeded), "context deadlines (Timeout()=true) never retry")
	assert.True(t, retryableCallErr(&url.Error{Op: "Post", URL: "http://x", Err: fmt.Errorf("EOF")}),
		"a non-timeout *url.Error still retries — the exclusion is Timeout(), not the wrapper")
}

func TestRetryBackoff(t *testing.T) {
	base := 2 * time.Second
	for attempt, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
		assert.Equalf(t, want, retryBackoff(base, attempt), "attempt %d", attempt)
	}
	assert.Equal(t, time.Millisecond, retryBackoff(time.Millisecond, 0))
	assert.Equal(t, 2*time.Millisecond, retryBackoff(time.Millisecond, 1))
	assert.Equal(t, maxRetryBackoff, retryBackoff(base, 100), "the curve caps instead of overflowing")
	assert.Equal(t, maxRetryBackoff, retryBackoff(time.Hour, 60))
	assert.Equal(t, time.Duration(0), retryBackoff(0, 3), "zero base sleeps nothing")
}

// retryTestConfig is the shared cfg for the live-responder retry tests:
// real HTTP against an httptest server, tiny backoff so tests stay fast.
func retryTestConfig(baseURL string) runConfig {
	return runConfig{
		baseURL:      baseURL,
		model:        "test-model",
		tools:        advertisedTools,
		seed:         1,
		deliveryRole: "system",
		retries:      3,
		retryWait:    time.Millisecond,
	}
}

// TestSessionRetriesTransient5xx pins the recovery path end-to-end through
// the REAL live responder: a server that 500s twice then succeeds leaves
// the session intact — one logical request, the identical body re-sent
// every attempt, one choices-less error record per failed attempt, and the
// success logged as the normal response that follows them.
func TestSessionRetriesTransient5xx(t *testing.T) {
	var hits atomic.Int32
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "proxy error: Failed to read connection")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, cannedResp("but of course", ""))
	}))
	defer srv.Close()

	s, logPath := newTestSessionResponder(t, newLiveResponder(retryTestConfig(srv.URL)), 1, "testv", "imageflow")
	s.cfg.retries, s.cfg.retryWait = 3, time.Millisecond
	require.NoError(t, s.userTurn("hi"))

	assert.Equal(t, 1, s.reqCount, "a retried request is still ONE logical request")
	assert.Equal(t, 1, s.respCount, "only the successful exchange logs a counted response")
	assert.Equal(t, 2, s.retryCount, "two failed attempts were retried")
	require.Equal(t, int32(3), hits.Load(), "2 failures + 1 success")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 3)
	assert.Equal(t, bodies[0], bodies[1], "the retry must re-send the identical request body")
	assert.Equal(t, bodies[0], bodies[2], "the successful attempt carries the same body too")

	lines := mustReadLines(t, logPath)
	require.Len(t, lines, 4, "request, error record, error record, response")
	var e apiLogEntry
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &e))
	assert.Equal(t, "request", e.Type, "the request is logged exactly once, unannotated")
	for _, ln := range lines[1:3] {
		require.NoError(t, json.Unmarshal([]byte(ln), &e))
		assert.Equal(t, "response", e.Type, "failed attempts reuse the response record type")
		assert.NotContains(t, string(e.Body), "choices", "error records are choices-less (the analysis scripts skip them)")
		assert.Contains(t, string(e.Body), "proxy error", "the 500 body rides the error record")
	}
	require.NoError(t, json.Unmarshal([]byte(lines[3]), &e))
	assert.Equal(t, "response", e.Type)
	assert.Contains(t, string(e.Body), "but of course", "the successful attempt's response follows normally")
}

// TestSessionRetriesExhaustedAborts pins the final-failure path: a server
// that never recovers gets exactly retries+1 attempts, the last failure's
// body is logged the way the pre-retry failure path always logged it, and
// the session still aborts with the HTTP error.
func TestSessionRetriesExhaustedAborts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "proxy error: still broken")
	}))
	defer srv.Close()

	s, logPath := newTestSessionResponder(t, newLiveResponder(retryTestConfig(srv.URL)), 1, "testv", "imageflow")
	s.cfg.retries, s.cfg.retryWait = 3, time.Millisecond
	err := s.userTurn("hi")
	require.ErrorContains(t, err, "HTTP 500")
	assert.Equal(t, 1, s.reqCount)
	assert.Equal(t, 3, s.retryCount, "every retry was spent")
	assert.Equal(t, 1, s.respCount, "the final failure's body still logs as a response")
	require.Equal(t, int32(4), hits.Load(), "retries+1 total attempts")

	lines := mustReadLines(t, logPath)
	require.Len(t, lines, 5, "request + 3 error records + the final failure's response record")
	var e apiLogEntry
	for _, ln := range lines[1:4] {
		require.NoError(t, json.Unmarshal([]byte(ln), &e))
		assert.Equal(t, "response", e.Type)
		assert.NotContains(t, string(e.Body), "choices")
	}
	require.NoError(t, json.Unmarshal([]byte(lines[4]), &e))
	assert.Equal(t, "response", e.Type, "the final failure keeps the existing log shape")
	assert.Contains(t, string(e.Body), "still broken")
}

// TestSessionDoesNotRetry4xx pins the classification: a 400 is the
// request's own fault — one attempt, no error records, immediate failure.
func TestSessionDoesNotRetry4xx(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"bad tool schema"}}`)
	}))
	defer srv.Close()

	s, logPath := newTestSessionResponder(t, newLiveResponder(retryTestConfig(srv.URL)), 1, "testv", "imageflow")
	s.cfg.retries, s.cfg.retryWait = 3, time.Millisecond
	err := s.userTurn("hi")
	require.ErrorContains(t, err, "HTTP 400")
	assert.Equal(t, 0, s.retryCount, "4xx is never retried")
	require.Equal(t, int32(1), hits.Load(), "exactly one attempt")

	lines := mustReadLines(t, logPath)
	require.Len(t, lines, 2, "request + the failure's response record, nothing else")
	assert.Equal(t, 1, s.respCount)
}

// TestSessionRetriesTransportEOF pins the router's other failure face: a
// connection killed mid-flight (hijacked and closed before any response)
// is a transport-level error — retried like a 500, with a synthesized
// choices-less error record because no body ever arrived.
func TestSessionRetriesTransportEOF(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("httptest server does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijacking: %v", err)
				return
			}
			_ = conn.Close() // the wire just... dies
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, cannedResp("recovered", ""))
	}))
	defer srv.Close()

	s, logPath := newTestSessionResponder(t, newLiveResponder(retryTestConfig(srv.URL)), 1, "testv", "imageflow")
	s.cfg.retries, s.cfg.retryWait = 3, time.Millisecond
	require.NoError(t, s.userTurn("hi"), "a transport failure recovers on retry")

	assert.Equal(t, 1, s.reqCount)
	assert.Equal(t, 1, s.respCount)
	assert.Equal(t, 1, s.retryCount)
	require.Equal(t, int32(2), hits.Load())

	lines := mustReadLines(t, logPath)
	require.Len(t, lines, 3, "request, synthesized error record, response")
	var e apiLogEntry
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &e))
	assert.Equal(t, "response", e.Type)
	assert.NotContains(t, string(e.Body), "choices")
	assert.Contains(t, string(e.Body), `"error"`, "a bodyless transport failure gets a synthesized error object")
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &e))
	assert.Contains(t, string(e.Body), "recovered")
}

// timeoutError is a fabricated net.Error with Timeout()=true — the error
// family http.Client.Timeout surfaces: bare from a transport, or wrapped
// in the *url.Error that client.Do returns (url.Error satisfies net.Error
// itself and delegates Timeout to the cause, so both shapes must hit the
// exclusion).
type timeoutError struct{}

func (timeoutError) Error() string {
	return "net/http: request canceled (Client.Timeout exceeded while awaiting headers)"
}
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// TestSessionDoesNotRetryClientTimeout pins the controller ruling: a
// timed-out request performs exactly ONE attempt — the retry budget exists
// for the router's instant drops, not for requests that already burned
// their full client time budget. The session fails through the normal
// bodyless-transport path (request record only; error records exist solely
// for attempts that get retried). No sleeps: nothing retries.
func TestSessionDoesNotRetryClientTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"bare net.Error", timeoutError{}},
		{"url.Error-wrapped", &url.Error{Op: "Post", URL: "http://127.0.0.1:1/v1/chat/completions", Err: timeoutError{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			respond := func([]byte) ([]byte, error) {
				calls++
				return nil, tc.err
			}
			s, logPath := newTestSessionResponder(t, respond, 1, "testv", "imageflow")
			s.cfg.retries, s.cfg.retryWait = 3, time.Millisecond

			err := s.userTurn("hi")
			require.ErrorContains(t, err, "Client.Timeout exceeded")
			assert.Equal(t, 1, calls, "a client timeout performs exactly one attempt, no retries")
			assert.Equal(t, 0, s.retryCount)
			assert.Equal(t, 0, s.respCount, "a bodyless failure logs no response")
			assert.Equal(t, 1, s.reqCount)
			lines := mustReadLines(t, logPath)
			assert.Len(t, lines, 1, "only the request record — no retry error records")
		})
	}
}

// TestRunOneRetriesColumn pins the CSV wiring end-to-end: the retries
// column counts the session's retry re-sends, requests stays the logical
// count, and the full row renders through the index with the new columns
// in order.
func TestRunOneRetriesColumn(t *testing.T) {
	responses := []string{ // minimal no-tool imageflow shape
		cannedResp("i shall simply describe it...", ""),
		cannedResp("as delivered", ""),
		cannedResp("hmm, no tool again", ""),
		cannedResp("very well", ""),
	}
	var hits, ok atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, "proxy error")
			return
		}
		n := int(ok.Add(1))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responses[n-1])
	}))
	defer srv.Close()

	cfg := retryTestConfig(srv.URL)
	outdir := t.TempDir()
	row, err := runOne(loadedVariant{name: "testv", text: "SYS PROMPT"}, scenarioImageflow, 1, cfg, newLiveResponder(cfg), outdir)
	require.NoError(t, err)
	require.NotNil(t, row)

	assert.Equal(t, 4, row.Requests, "logical request count is unaffected by retries")
	assert.Equal(t, 4, row.Responses)
	assert.Equal(t, 2, row.Retries, "both dropped requests were retried")
	require.Equal(t, int32(6), hits.Load(), "4 logical requests + 2 retries")
	require.Equal(t, int32(4), ok.Load(), "every logical request eventually succeeded")

	lines := mustReadLines(t, filepath.Join(outdir, "testv", "imageflow-system", "run-01.jsonl"))
	require.Len(t, lines, 10, "4 requests + 2 error records + 4 responses")
	var e apiLogEntry
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &e))
	assert.Equal(t, "response", e.Type, "the first error record sits right after its request")
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &e))
	assert.Equal(t, "response", e.Type)
	require.NoError(t, json.Unmarshal([]byte(lines[3]), &e))
	assert.Equal(t, "response", e.Type, "the recovered attempt's response closes the first exchange")
	require.NoError(t, json.Unmarshal([]byte(lines[4]), &e))
	assert.Equal(t, "request", e.Type, "the second logical request follows the recovered exchange")

	var buf bytes.Buffer
	idx := newRunIndex(&buf)
	require.NoError(t, idx.add(*row))
	require.NoError(t, idx.close())
	assert.Equal(t,
		"variant,tool_variant,temperature,retries,scenario,delivery_role,run,requests,responses,reasoning_chars_total,fabricated,leak_suspects,fabricated_urls,linkless_finals,silent_turns,jobs,registers\n"+
			"testv,default,default,2,imageflow,system,01,4,4,0,false,0,0,2,0,0,0\n",
		buf.String(), "the row renders with temperature, retries, and the detector columns in order; both linkless presentation slots (no-tool responses after each delivery, no URLs cited) trip")
}
