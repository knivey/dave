package main

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// llama.cpp stream-session support (Nov 2026, prod incidents 2026-11-04/
// 05/06 — sessions 2890, 2908, 3002, 3007 ×2). THE ROOT CAUSE of those
// deaths turned out to be client-side and is fixed in ssecomments.go:
// llama.cpp's SSE keepalive comment (":\n\n", sent after every 30s of
// stream silence) hit an openai-go ssestream bug that aborts the whole
// stream on the comment's data-less blank-line dispatch — a cold-cache
// 21k-token prefill is ~30.5s of exactly such silence, so the FIRST big
// request after every model load died (later turns ride the KV cache and
// prefill in ~1s). Investigation detour worth remembering: the llama.cpp
// child/router logs ("http client error: Failed to read connection",
// task cancels) were all DOWNSTREAM of the SDK aborting — the wire was
// healthy, verified by relay capture: keepalive comment at T+30.0s,
// client FIN 3ms later.
//
// What this file adds on top is DEFENSE IN DEPTH, still valuable with the
// root cause fixed: llama.cpp's X-Conversation-Id header detaches the
// generation from the HTTP socket (server_res_spipe::should_stop returns
// only the session cancel flag), so if dave ever DOES lose a live stream
// — genuine network drop, restart, ^stop — the generation survives
// server-side in a resumable ring (GET /v1/stream, 4MiB / 300s TTL) and
// only an explicit DELETE stops it. Without the release DELETE, an
// abandoned detached generation would burn GPU to max_tokens with no
// reader; with it, ^stop/shutdown/error paths actually stop server work.
// The DELETE also clears the router's conv_models map entry, which
// otherwise accumulates one entry per conv id until router restart.
//
// Scope is deliberately streaming-only: a non-streaming turn cannot keep
// reading the live wire, so header-detaching it would let an abandoned
// generation burn the GPU with no reader. dave has no reattach today;
// if it ever gains one, byte offsets must be counted on the FILTERED
// body (see ssecomments.go).

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
