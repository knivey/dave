package main

// Reasoning history replay ("reasoningreplay") — attaches stored assistant
// reasoning back onto the wire so thinking models keep their chain of thought
// on tool-call rows — within the current turn's tool loop and across past
// tool-call turns — with "always" extending to every reasoning row.
//
// Semantics are pinned to the gemma4 chat template contract (the prompt-
// formatting model card's "Managing Thought Context Between Turns" rules,
// verified against the google/gemma-4-26B-A4B-it chat_template.jinja):
//
//   - In a multi-turn conversation, thoughts from previous model turns are
//     stripped from history — the historical output should be only the
//     final response.
//   - EXCEPTION: within a single model turn that involves tool calls,
//     thoughts must NOT be removed between the function calls.
//
// The gemma4 template implements exactly this gate itself:
//
//	thinking_gate = (index > last_user_idx) or (preserve_thinking and tool_calls)
//
// and renders an assistant message's `reasoning`/`reasoning_content` field
// as a `<|channel>thought` block when the gate passes, stripping inline
// thought tags from content strings server-side. llama.cpp parses
// `reasoning_content` from INPUT assistant messages and re-emits it into
// the template context (common/chat.cpp to_json_oaicompat), so the wire
// field dave sends is what the template sees. OpenRouter documents
// `reasoning_content` as an exact alias of its `reasoning` round-trip
// field ("Preserving Reasoning" — 1: message.reasoning string), and
// DeepSeek's API is native reasoning_content. One field covers all three
// replay targets; policy beyond that (template gating, preserve_thinking
// kwarg, enable_thinking) stays with the server/template, controlled by
// the admin's chat_template_kwargs — dave only decides what is on the wire.
//
// The `replay_reasoning` config field (AIConfig + Service, cascade
// command > service > "tool_calls" in ApplyDefaults; invalid values
// rejected at load by validateReasoningReplay) selects the mode:
//
//   - "off":        never replay (the pre-2026-10 behavior).
//   - "tool_calls": replay reasoning on EVERY assistant row that carries
//     tool calls, current turn AND past turns — the owner's
//     cross-turn thought-context choice. The template's
//     preserve_thinking arm gates exactly these rows
//     server-side: with the template default (false) dave
//     still SENDS past-turn rows but the server does not
//     render them (stripped at prompt assembly); the admin
//     enables preserve_thinking = true via
//     chat_template_kwargs to render cross-turn thoughts.
//     Final-answer rows of past turns (no tool calls on the
//     row) stay stripped — per the gemma rule "the historical
//     output should be only the final response" (the response
//     content is kept; only its thought block is not).
//   - "always":     every assistant row with reasoning, plain non-tool
//     rows included — note gemma templates can NEVER render
//     historical plain rows regardless of kwargs (the gate's
//     preserve arm requires tool_calls on the row), so on
//     gemma "always" sends wire bytes the template will not
//     render; it exists for non-gemma providers/templates.
//
// Replay fidelity split (aiMessages.go reasoningParts): capture stores the
// model's REAL thinking text (messages.reasoning_content) separately from
// the provider's summarized form (messages.reasoning_summary), and — since
// the reasoningreplayformat knob — ALSO the exact structure the provider
// emitted (messages.reasoning_details, the envelope; captured always).
// Replay form is the format knob's choice: "text" sends ONLY the real
// content — Chat Completions `reasoning_content` on eligible assistant
// rows (never the summary); Responses API a reasoning input item carrying
// content parts only, with no summary key (reasoningReplayInputItem).
// "details" sends the envelope's VERBATIM items — Chat Completions
// `reasoning_details` on the assistant row (OpenRouter's preserving-
// reasoning contract: the sequence must match the original output, which
// is why capture is verbatim and replay is unmodified); Responses API the
// provider's own reasoning items echoed back (ids, encrypted_content,
// summary, content). Exact replay only happens on the API dialect the
// payload was born on; a cross-dialect envelope or a missing one falls
// back to the text form per-row. The stored summary is display/log-only
// and is NEVER serialized, replayed, or token-counted on any path; with
// reasoningsummary unset AND no content captured, the text form is a
// structural no-op. OpenAI-strict reasoning-item validation remains a
// deliberate non-goal (dave persists the echoed shape, not OpenAI's full
// item taxonomy; OpenAI services chain server-side and never replay).
const (
	ReasoningReplayOff       = "off"
	ReasoningReplayToolCalls = "tool_calls"
	ReasoningReplayAlways    = "always"
)

// Replay wire forms (reasoningreplayformat). "text" is the default and is
// byte-identical to the pre-knob behavior. "details" is the opt-in exact
// dialect: OpenRouter's preserving-reasoning contract ("pass the full
// reasoning_details block … the sequence must match the outputs generated
// by the model during the original request") — the stored envelope's
// verbatim items ride the wire, with per-row fallback to the text form
// when a row captured nothing (or captured the OTHER API dialect — exact
// replay only happens on the path the payload was born on).
const (
	ReasoningReplayFormatText    = "text"
	ReasoningReplayFormatDetails = "details"
)

// reasoningReplay resolves the replay mode defensively: valid values pass
// through, hand-built configs that skipped ApplyDefaults/load validation
// fall back to the default mode rather than leaking a garbage string into
// the serializers. Mirrors guidanceRole/asyncResultDelivery.
func reasoningReplay(cfg AIConfig) string {
	switch cfg.ReasoningReplay {
	case ReasoningReplayOff, ReasoningReplayToolCalls, ReasoningReplayAlways:
		return cfg.ReasoningReplay
	}
	return ReasoningReplayToolCalls
}

// reasoningReplayFormat resolves the replay wire form defensively: valid
// values pass through, hand-built configs that skipped ApplyDefaults/load
// validation fall back to "text" (the behavior-compatible default) rather
// than leaking a garbage string into the serializers. Mirrors
// reasoningReplay above.
func reasoningReplayFormat(cfg AIConfig) string {
	switch cfg.ReasoningReplayFormat {
	case ReasoningReplayFormatText, ReasoningReplayFormatDetails:
		return cfg.ReasoningReplayFormat
	}
	return ReasoningReplayFormatText
}

// replayReasoningRow is the single eligibility predicate — whether row m's
// stored reasoning goes on the wire in mode replay and format format. Both
// serializers (attach reasoning_content / reasoning items), the token-
// accounting filter (strip what won't be serialized), and the eligibility
// tests call this, so they can never drift.
//
// Requires: assistant role, and replayable material for the format —
// non-empty ReasoningContent (the REAL thinking text) in "text" mode; in
// "details" mode additionally a parseable exact-replay envelope (the
// eligibility WIDENING: an encrypted-only row — summary/encrypted details,
// no real text, e.g. Claude via OpenRouter — finally has replayable
// material). A summary-only row — ReasoningSummary set, nothing else — is
// NEVER eligible in any mode/format: the summary is display/log-only and
// never rides the wire. Then the mode gate: "always" takes any eligible
// row; "tool_calls" takes tool-call rows at ANY position — the current
// turn's agentic loop AND past tool-call turns (the owner's cross-turn
// thought-context choice; rendering of past-turn rows is the server's
// preserve_thinking gate, the admin's chat_template_kwargs — dave only
// decides what is on the wire); "off" and unknown raw modes replay nothing
// except that unknown modes are mapped to tool_calls semantics, mirroring
// the resolver so a hand-built AIConfig call can never widen what
// validation rejected. Unknown raw formats fall back to "text" semantics
// (reasoningReplayFormat's default).
func replayReasoningRow(m ChatMessage, replay, format string) bool {
	if m.Role != RoleAssistant {
		return false
	}
	hasText := m.ReasoningContent != ""
	hasDetails := false
	if format == ReasoningReplayFormatDetails {
		_, _, hasDetails = parseReasoningDetailsEnvelope(m.ReasoningDetails)
	}
	if !hasText && !hasDetails {
		return false
	}
	switch replay {
	case ReasoningReplayAlways:
		return true
	case ReasoningReplayToolCalls /* defensive default */, "":
		return len(m.ToolCalls) > 0
	case ReasoningReplayOff:
		return false
	}
	// Unknown raw value: same semantics as tool_calls (resolver's fallback).
	return len(m.ToolCalls) > 0
}

// replayReasoningFilter pre-strips what mode+format would NOT put on the
// wire: the token accounting passes its messages through this so
// countMessageTokens sees exactly the payload that will be serialized (the
// "neutral count of the payload it sends" contract holds in every
// direction of both knobs). ReasoningSummary is irrelevant here — it never
// rides the wire in any mode, so it is never counted.
//
// Format handling mirrors the serializers' choice per row: in "details"
// format a row with a parseable envelope counts the ENVELOPE (its Content
// is stripped — the serializers put the exact items on the wire INSTEAD of
// the text, never both), a row without one counts the text fallback. The
// filter is path-agnostic (it runs before the request knows which API
// dialect will carry it), so a cross-dialect envelope — one the
// path-specific serializer would skip in favor of text — still counts as
// details here: a slight over-estimate in the rare service-switch case,
// typically in the conservative direction for compaction budgeting (the
// direction is not guaranteed: a cross-dialect row whose text fallback
// outsizes its envelope can under-shoot).
// Copy-on-write: the original slice and rows are never mutated (compaction
// callers share rows with the live history), and an input that needs no
// stripping is returned unchanged.
func replayReasoningFilter(messages []ChatMessage, replay, format string) []ChatMessage {
	// No fast path: the pre-details "always returns unchanged" shortcut
	// silently kept envelopes in the counted payload once details capture
	// landed (text format never rides them on any path — code review
	// finding 1). The loop below is the single source of truth for every
	// mode+format combination; "always" is simply the mode where every
	// eligible row keeps its (single) wire slot.
	filtered := messages
	changed := false
	for i := range messages {
		eligible := replayReasoningRow(messages[i], replay, format)
		var stripContent, stripDetails bool
		if !eligible {
			stripContent = messages[i].ReasoningContent != ""
			stripDetails = messages[i].ReasoningDetails != ""
		} else if format == ReasoningReplayFormatDetails {
			if _, _, ok := parseReasoningDetailsEnvelope(messages[i].ReasoningDetails); ok {
				stripContent = messages[i].ReasoningContent != ""
			} else {
				stripDetails = messages[i].ReasoningDetails != ""
			}
		} else {
			stripDetails = messages[i].ReasoningDetails != ""
		}
		if !stripContent && !stripDetails {
			continue
		}
		if !changed {
			filtered = make([]ChatMessage, len(messages))
			copy(filtered, messages)
			changed = true
		}
		if stripContent {
			filtered[i].ReasoningContent = ""
		}
		if stripDetails {
			filtered[i].ReasoningDetails = ""
		}
	}
	return filtered
}
