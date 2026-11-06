package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// llama.cpp stream-session support (Nov 2026, prod incident 2026-11-06
// session 3002): a llama.cpp child server binds a streaming generation to
// its HTTP socket — between prefill batches it polls the socket with
// select()+MSG_PEEK, and any transient readable-wakeup on the router↔child
// loopback connection is read as "client gone": the child cancels the task,
// the router's proxy logs "http client error: Failed to read connection",
// and dave receives a plain-text "proxy error: ..." line inside its already
// committed 200 text/event-stream response, which the openai-go SSE decoder
// turns into "unexpected end of JSON input". Three incidents over three
// days (sessions 2890, 2908, 3002), all ~21k-token streaming turns on
// type = "llama" services, dying mid-prefill at 14.8-40.6s — a race, not a
// timer (dave's own timeouts were 4m and never fired).
//
// llama.cpp's opt-out is the X-Conversation-Id header (server-stream.cpp):
// with it, server_res_spipe::should_stop() returns only the session's
// cancel flag — the generation detaches from the socket entirely and only
// an explicit DELETE /v1/stream stops it. This is what the webui uses,
// which is why webui long prefills never die. Verified live against the
// prod router (b12156): the same 21k-token request that died headerless
// ran 42.5s clean with the header.
//
// The detachment cuts both ways: once the header is sent, dave dropping or
// cancelling its end no longer stops the server-side generation. Every
// runTurnStream exit therefore releases the session with
// DELETE /v1/stream?conv_id=<id> — ^stop, queue cancel, shutdown, stream
// errors, idle timeouts, and normal completion alike. The DELETE also
// clears the router's conv_models map entry, which otherwise accumulates
// one entry per conv id until the router restarts. The release runs on a
// DETACHED context with a short timeout precisely because the runner ctx
// is cancelled on the paths where it matters most.
//
// Scope is deliberately streaming-only: a non-streaming turn cannot keep
// reading the live wire, so header-detaching it would let an abandoned
// generation burn the GPU to max_tokens with no reader — the DELETE would
// arrive only after dave already gave up. Streaming turns hold the wire
// for their whole life, so a live dave implies a live reader.

const (
	// llamaConversationIDHeader is the llama.cpp resumable-stream opt-in.
	llamaConversationIDHeader = "X-Conversation-Id"
	// llamaStreamReleaseTimeout bounds the session-release DELETE. Detached
	// from the runner ctx on purpose — that ctx is often already cancelled
	// when the release fires (user stop, shutdown).
	llamaStreamReleaseTimeout = 3 * time.Second
)

// llamaStreamSession reports whether this turn's streaming requests should
// carry the llama.cpp conversation header (and therefore release the
// server-side session on exit). Single predicate for both sides so the
// header and the DELETE can never drift apart.
func (cr *chatRunner) llamaStreamSession() bool {
	return cr.llamaService && cr.convID != ""
}

// releaseLlamaStream stops the server-side generation and evicts the stream
// session for this conversation id. Idempotent (204 on a live session, and
// a completed session simply evicts); failures are logged, never fatal —
// a stuck release must not take the turn down with it.
func (cr *chatRunner) releaseLlamaStream() {
	ctx, cancel := context.WithTimeout(context.Background(), llamaStreamReleaseTimeout)
	defer cancel()

	endpoint := strings.TrimRight(cr.baseURL, "/") + "/stream?conv_id=" + url.QueryEscape(cr.convID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		cr.logger.Warn("llama stream release: bad request", "error", err.Error())
		return
	}
	// llama-server enforces --api-key on /v1/stream like every other
	// route — an unauthenticated release would 401 (silently, WARN-only)
	// on key-protected deployments and strand the detached generation.
	if cr.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+cr.apiKey)
	}
	resp, err := cr.httpClient.Do(req)
	if err != nil {
		cr.logger.Warn("llama stream release failed", "error", err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	cr.logger.Debug("llama stream session released", "status", resp.StatusCode)
}
