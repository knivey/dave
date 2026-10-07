package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	logxi "github.com/mgutz/logxi/v1"
)

// Model-load detection for llama-server's built-in router (and llama-swap).
//
// DESIGN NOTE: detection is a pre-request probe of the service's OpenAI-compat
// model list, NOT a response-time heuristic. The llama-server router blocks a
// request (streaming or not) while it spawns the child process and loads the
// model — but a non-streaming request ALSO blocks for the whole generation on
// an already-loaded model, so "request is slow" cannot distinguish the two.
// What does distinguish them is asking the server:
//
//   - llama-server router mode: GET /v1/models (the {baseurl}/models path)
//     lists EVERY discovered model with a per-model status object — loaded,
//     loading, unloaded, sleeping, failed. The entry for the model we are
//     about to request tells us whether the request will have to wait.
//   - llama-swap: /v1/models is proxied to the active upstream only, so the
//     list contains just the currently-loaded model. Absence of our model
//     from a status-less list means a swap is coming.
//   - plain single-model llama-server: /v1/models lists the served model with
//     no status field; presence is treated as loaded.
//
// The probe is one small GET per turn on an opt-in feature (enabled by
// default only for type = "llama" services), bounded by modelLoadProbeTimeout,
// and fails open: any error, non-200, or unparseable body means "no notice",
// and the turn proceeds exactly as before.

// modelLoadProbeTimeout bounds the pre-turn GET {baseurl}/models request.
// Local-router round trips are ~1ms; this only caps a wedged endpoint so it
// cannot eat a meaningful slice of the turn.
const modelLoadProbeTimeout = 2 * time.Second

// modelLoadProbeMaxBytes caps how much of the /models response we read. The
// router merges each loaded child's info into its entries, so the list can be
// larger than a bare id list; 4 MiB is far beyond any real list while still
// bounding a misbehaving endpoint.
const modelLoadProbeMaxBytes = 4 << 20

type modelListEntry struct {
	ID      string          `json:"id"`
	Aliases []string        `json:"aliases"`
	Status  json.RawMessage `json:"status"` // router: {"value":"loaded"} or "loaded"; others: absent
}

type modelListResponse struct {
	Data []modelListEntry `json:"data"`
}

// parseModelStatus extracts the status string from either shape the
// llama-server router has used: an object {"value":"loading",...} or a bare
// string. Empty means "no usable status". Normalized to lowercase without
// surrounding whitespace so "Loaded" or "loaded " still match the loaded
// state rather than producing a spurious notice.
func parseModelStatus(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Value != "" {
		return strings.ToLower(strings.TrimSpace(obj.Value))
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.ToLower(strings.TrimSpace(s))
	}
	return ""
}

func entryMatchesModel(e modelListEntry, model string) bool {
	if e.ID == model {
		return true
	}
	for _, a := range e.Aliases {
		if a == model {
			return true
		}
	}
	return false
}

// evaluateModelLoad decides whether a request for `model` against this model
// list will have to wait for a model load/swap.
//
// decided=false means "this list cannot tell us" (model unknown to a
// status-aware router, or the probe never got a usable list) — callers must
// not send a notice in that case: an unknown model fails fast on its own, and
// claiming "loading" would be wrong.
func evaluateModelLoad(entries []modelListEntry, model string) (needsLoad, decided bool) {
	// A list is "status-aware" only if some entry carries a PARSEABLE
	// status: keying on raw field presence would let a `"status": null`
	// entry flip llama-swap-shaped lists into router semantics.
	statusAware := false
	for _, e := range entries {
		if parseModelStatus(e.Status) != "" {
			statusAware = true
			break
		}
	}
	// First matching entry wins. The router emits one entry per model, so
	// duplicates/alias collisions (an admin hand-crafting a list) resolve in
	// list order, same as the router's own lookup.
	for _, e := range entries {
		if !entryMatchesModel(e, model) {
			continue
		}
		status := parseModelStatus(e.Status)
		if status == "" {
			// Listed without a status (plain llama-server /v1/models, or a
			// router entry mid-transition): the server advertises it can
			// serve this model; treat presence as loaded.
			return false, true
		}
		// Anything not "loaded" (unloaded, loading, sleeping, failed) means
		// the upcoming request waits while the router brings it up.
		return status != "loaded", true
	}
	// Model not listed. On a status-aware list (llama-server router) that
	// means the server does not know the model — the request will error on
	// its own, so no notice. On a status-less list (llama-swap proxying the
	// active upstream, or an idle server) absence means not loaded: the
	// request will trigger a swap.
	if statusAware {
		return false, false
	}
	return true, true
}

// probeModelLoad fetches {baseurl}/models and reports whether `model` needs
// loading. Errors fail open: (false, false) = "don't know, don't notify".
func probeModelLoad(ctx context.Context, client *http.Client, baseURL, apiKey, model string) (needsLoad, decided bool) {
	if client == nil {
		client = http.DefaultClient
	}
	probeCtx, cancel := context.WithTimeout(ctx, modelLoadProbeTimeout)
	defer cancel()

	url := strings.TrimSuffix(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return false, false
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, false
	}

	var list modelListResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, modelLoadProbeMaxBytes)).Decode(&list); err != nil {
		return false, false
	}
	return evaluateModelLoad(list.Data, model)
}

// sendModelLoadNotice probes the service's model list and sends the [llm]
// model_load notice through `send` when the model needs loading. Shared by
// the chat-runner path (maybeNotifyModelLoad) and the legacy completion()
// path so the two cannot drift.
func sendModelLoadNotice(ctx context.Context, client *http.Client, logger logxi.Logger, baseURL, apiKey, model, nick string, send func(string)) {
	needsLoad, decided := probeModelLoad(ctx, client, baseURL, apiKey, model)
	if !decided || !needsLoad {
		return
	}
	if logger != nil {
		logger.Info("model not loaded on server, notifying", "model", model)
	}
	send(expandNotice(getNotices().LLM.ModelLoad, map[string]string{
		"nick":  nick,
		"model": model,
	}))
}

// maybeNotifyModelLoad runs the pre-turn model-load probe and sends the
// notice when the service reports our model is not loaded yet. No-op unless
// the command resolved load_notice = true.
func (cr *chatRunner) maybeNotifyModelLoad() {
	if cr.cfg.LoadNotice == nil || !*cr.cfg.LoadNotice {
		return
	}
	// Defensive nil guard: hand-built runners (tests) may lack ctx; a nil
	// ctx would panic inside context.WithTimeout before sendIRC ever ran.
	ctx := cr.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	sendModelLoadNotice(ctx, cr.httpClient, cr.logger, cr.baseURL, cr.apiKey, cr.cfg.Model, cr.nick, cr.sendIRC)
}
