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

	mu           sync.Mutex
	captureBody  bool
	capturedBody []byte
}

func newDaveTransport(extraBody map[string]any) *daveTransport {
	if extraBody == nil {
		extraBody = make(map[string]any)
	}
	return &daveTransport{
		base:      http.DefaultTransport,
		extraBody: extraBody,
	}
}

func (t *daveTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && req.Body != nil && len(t.extraBody) > 0 {
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
			req.Body = io.NopCloser(bytes.NewReader(newBody))
			req.ContentLength = int64(len(newBody))
		} else {
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
		}
	}

	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return resp, err
	}

	t.mu.Lock()
	shouldCapture := t.captureBody
	t.mu.Unlock()

	if shouldCapture {
		ct := resp.Header.Get("Content-Type")
		if ct != "text/event-stream" {
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil {
				t.mu.Lock()
				t.capturedBody = body
				t.mu.Unlock()
				resp.Body = io.NopCloser(bytes.NewReader(body))
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
