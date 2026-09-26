package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// Safety verdicts. "safe"/"unsafe"/"unknown" are the values persisted to
// jobs.safety and imgsite's images.safety column; the empty string marks a
// job that never underwent classification at all (skip_networks) so
// persistence can tell "nothing to write" from "write unknown".
const (
	safetyVerdictSafe    = "safe"
	safetyVerdictUnsafe  = "unsafe"
	safetyVerdictUnknown = "unknown"
	// safetyVerdictUnvetted is the not-classified marker (see above); it
	// intentionally equals the jobs.safety column's empty default.
	safetyVerdictUnvetted = ""
)

// defaultSafetyVetEnhancement is the reserved [enhancement.*] entry name
// that performs the strict second-pass judgment ([safety] vet_enhancement).
const defaultSafetyVetEnhancement = "safety-vet"

// vetVerdictSchema is the structured-output schema the vet model must
// satisfy. Strict mode requires every property to be listed in "required".
var vetVerdictSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"safe":   map[string]any{"type": "boolean"},
		"reason": map[string]any{"type": "string"},
	},
	"required":             []string{"safe", "reason"},
	"additionalProperties": false,
}

// vetVerdictResponse is the JSON the vet prompt (owner config) instructs
// the model to return.
type vetVerdictResponse struct {
	Safe   bool   `json:"safe"`
	Reason string `json:"reason"`
}

// vetMissingWarnSink is invoked exactly when the missing-config WARN is
// emitted. Nil in production; a test seam so the WARN-once behavior stays
// observable (logxi has no per-test capture).
var vetMissingWarnSink func(vetName string)

var (
	vetMissingWarnMu sync.Mutex
	// vetMissingWarned tracks which vet enhancement names have already
	// produced their one-per-process WARN. Keyed by name because
	// [safety] vet_enhancement is configurable; a config that is added via
	// SIGHUP starts warning under its own name.
	vetMissingWarned = map[string]bool{}
)

// warnVetConfigMissing emits the missing-vet-config WARN once per process
// per enhancement name: every job on an unconfigured deployment would
// otherwise repeat it.
func warnVetConfigMissing(vetName string) {
	vetMissingWarnMu.Lock()
	first := !vetMissingWarned[vetName]
	vetMissingWarned[vetName] = true
	sink := vetMissingWarnSink
	vetMissingWarnMu.Unlock()

	if !first {
		return
	}
	if sink != nil {
		sink(vetName)
	}
	loggerQueue.Warn("safety vet enhancement config missing; verdicts degrade to unknown until it is configured",
		"vet_enhancement", vetName,
		"hint", "add an [enhancement."+defaultSafetyVetEnhancement+"] section (see example.toml) or point [safety] vet_enhancement at an existing one",
	)
}

// safetyNetworkSkipped reports whether the job's network is exempt from
// classification ([safety] skip_networks, case-insensitive). Skipped jobs
// are not vetted at all — their visibility comes from imgsite's
// allowed_networks rule instead.
func safetyNetworkSkipped(cfg Config, network string) bool {
	if network == "" {
		return false
	}
	for _, skip := range cfg.Safety.SkipNetworks {
		if strings.EqualFold(skip, network) {
			return true
		}
	}
	return false
}

// buildVetInput assembles the vet call's user message: both prompts,
// labeled. The judging policy lives in the vet enhancement's system prompt
// (owner config), not here. For direct-tool jobs the enhanced prompt equals
// the original — both sections carry the same text, which is honest: that
// IS the prompt the image runs with.
func buildVetInput(originalPrompt, enhancedPrompt string) string {
	var b strings.Builder
	b.WriteString("Original prompt:\n")
	b.WriteString(originalPrompt)
	b.WriteString("\n\nEnhanced prompt:\n")
	b.WriteString(enhancedPrompt)
	return b.String()
}

// runSafetyVet performs the strict second-pass safety judgment via the
// reserved enhancement config (default "safety-vet"), reusing the entire
// enhancement machinery — both API paths, reasoning_effort, timeouts,
// SIGHUP hot-reload. Failures NEVER fail the job: every error path WARNs
// here and returns verdict "unknown" alongside the error.
func runSafetyVet(ctx context.Context, cfg Config, originalPrompt, enhancedPrompt string) (string, error) {
	vetName := cfg.Safety.VetEnhancement
	if vetName == "" {
		vetName = defaultSafetyVetEnhancement
	}

	if _, ok := cfg.Enhancements[vetName]; !ok {
		warnVetConfigMissing(vetName)
		return safetyVerdictUnknown, fmt.Errorf("safety vet enhancement %q not configured", vetName)
	}

	vetInput := buildVetInput(originalPrompt, enhancedPrompt)
	content, _, err := callEnhancementLLM(ctx, cfg, vetName, vetInput, "", "safety_verdict", vetVerdictSchema)
	if err != nil {
		// Context cancellation (job cancelled / shutdown) lands here too;
		// unknown is correct — the verdict never resolved.
		loggerQueue.Warn("safety vet call failed; verdict unknown", "vet_enhancement", vetName, "error", err)
		return safetyVerdictUnknown, fmt.Errorf("safety vet call: %w", err)
	}

	var verdict vetVerdictResponse
	if err := json.Unmarshal([]byte(content), &verdict); err != nil {
		loggerQueue.Warn("safety vet returned an unparseable verdict; verdict unknown",
			"vet_enhancement", vetName, "error", err, "content", content)
		return safetyVerdictUnknown, fmt.Errorf("parsing safety vet verdict: %w", err)
	}

	outcome := safetyVerdictUnsafe
	if verdict.Safe {
		outcome = safetyVerdictSafe
	}
	loggerQueue.Info("safety vet verdict",
		"vet_enhancement", vetName,
		"verdict", outcome,
		"reason", verdict.Reason,
	)
	return outcome, nil
}

// safetyVetFuture is an in-flight (or already-resolved) safety verdict.
// The verdict field is written exactly once by the vet goroutine before
// done is closed; wait() reads it after observing the close, so the
// happens-before edge is the channel close — no further locking needed.
type safetyVetFuture struct {
	done    chan struct{}
	verdict string
}

// wait blocks until the verdict resolves and returns it. A nil future
// (vetting skipped: skip_networks) waits for nothing and yields the
// unvetted marker.
func (f *safetyVetFuture) wait() string {
	if f == nil {
		return safetyVerdictUnvetted
	}
	<-f.done
	return f.verdict
}

// resolvedSafetyVet returns an already-complete future — used by the nsfw
// first-pass short-circuit, which needs no call.
func resolvedSafetyVet(verdict string) *safetyVetFuture {
	f := &safetyVetFuture{verdict: verdict, done: make(chan struct{})}
	close(f.done)
	return f
}

// startSafetyVet launches the classification for one job:
//
//   - skip_networks jobs skip vetting ENTIRELY (nil future → unvetted);
//   - an nsfw first-pass flag short-circuits to unsafe with NO vet call
//     (the call-saving);
//   - otherwise the strict vet runs on the shared enhancement machinery in
//     a goroutine of its own, so the LLM latency overlaps generation.
//
// The future's verdict is awaited by upload time (processJob), never before
// submit — generation must not block on the vet. If the job fails or is
// cancelled first, nobody waits; the goroutine still terminates because its
// context is the job's.
func startSafetyVet(ctx context.Context, cfg Config, network string, nsfwFirstPass bool, originalPrompt, enhancedPrompt string) *safetyVetFuture {
	if safetyNetworkSkipped(cfg, network) {
		loggerQueue.Info("safety: network in skip_networks, classification skipped", "network", network)
		return nil
	}

	if nsfwFirstPass {
		loggerQueue.Info("safety: enhancement first pass flagged nsfw; verdict unsafe without a vet call")
		return resolvedSafetyVet(safetyVerdictUnsafe)
	}

	f := &safetyVetFuture{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		// Failure semantics (WARN + "unknown") are handled inside
		// runSafetyVet; the verdict alone is the contract here.
		f.verdict, _ = runSafetyVet(ctx, cfg, originalPrompt, enhancedPrompt)
	}()
	return f
}
