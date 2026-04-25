package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

type daveTransport struct {
	base      http.RoundTripper
	extraBody map[string]any

	extraHeaders map[string]string

	mu           sync.Mutex
	captureBody  bool
	capturedBody []byte

	apiLogger *APILogger
	ctxKey    string
	isStream  bool
}

func newDaveTransport(extraBody map[string]any, extraHeaders map[string]string) *daveTransport {
	if extraBody == nil {
		extraBody = make(map[string]any)
	}
	return &daveTransport{
		base:         http.DefaultTransport,
		extraBody:    extraBody,
		extraHeaders: extraHeaders,
	}
}

func (t *daveTransport) setAPILogger(logger *APILogger, ctxKey string) {
	t.apiLogger = logger
	t.ctxKey = ctxKey
}

func (t *daveTransport) setExtraHeaders(headers map[string]string) {
	t.extraHeaders = headers
}

func (t *daveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var finalBody []byte

	if req.Method == http.MethodPost && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}

		var reqMap map[string]json.RawMessage
		if err := json.Unmarshal(body, &reqMap); err != nil {
			return nil, err
		}

		changed := false
		for k, v := range t.extraBody {
			valBytes, err := json.Marshal(v)
			if err != nil {
				continue
			}
			reqMap[k] = valBytes
			changed = true
		}

		if changed {
			newBody, err := json.Marshal(reqMap)
			if err != nil {
				return nil, err
			}
			finalBody = newBody
		} else {
			finalBody = body
		}
		req.Body = io.NopCloser(bytes.NewReader(finalBody))
		req.ContentLength = int64(len(finalBody))
	}

	if len(finalBody) > 0 && t.apiLogger != nil {
		t.apiLogger.LogRequest(t.ctxKey, finalBody)
	}

	for k, v := range t.extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	ct := resp.Header.Get("Content-Type")
	t.isStream = ct == "text/event-stream"

	t.mu.Lock()
	shouldCapture := t.captureBody
	t.mu.Unlock()

	if shouldCapture && !t.isStream {
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil {
			t.mu.Lock()
			t.capturedBody = body
			t.mu.Unlock()
			resp.Body = io.NopCloser(bytes.NewReader(body))

			if t.apiLogger != nil {
				t.apiLogger.LogResponse(t.ctxKey, body)
			}
		}
	}

	return resp, nil
}

func (t *daveTransport) getCapturedBody() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.capturedBody
}

func (t *daveTransport) setCaptureBody(capture bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.captureBody = capture
}
