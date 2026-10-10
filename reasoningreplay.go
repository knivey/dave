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
// the provider's summarized form (messages.reasoning_summary). Replay
// sends ONLY the real content — a summary is not the thinking. Chat
// Completions: reasoning_content on eligible assistant rows (never the
// summary). Responses API: a reasoning input item carrying content parts
// ONLY, with no summary key (reasoningReplayInputItem). The stored summary
// is display/log-only and is NEVER serialized, replayed, or token-counted
// on any path; with reasoningsummary unset AND no content captured,
// Responses replay stays a structural no-op.
const (
	ReasoningReplayOff       = "off"
	ReasoningReplayToolCalls = "tool_calls"
	ReasoningReplayAlways    = "always"
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

// replayReasoningRow is the single eligibility predicate — whether row m's
// stored reasoning goes on the wire in mode replay. Both serializers (add
// reasoning_content / reasoning items) and the token-accounting filter
// (strip what won't be serialized) call this, so the two can never drift.
//
// Requires: assistant role, non-empty ReasoningContent (the REAL thinking
// text). A summary-only row — ReasoningSummary set, ReasoningContent empty
// — is NEVER eligible in any mode: the summary is display/log-only and
// never rides the wire. Then: "always" takes any such row; "tool_calls"
// takes tool-call rows at ANY position — the current turn's agentic loop
// AND past tool-call turns (the owner's cross-turn thought-context choice;
// rendering of past-turn rows is the server's preserve_thinking gate, the
// admin's chat_template_kwargs — dave only decides what is on the wire);
// "off" and unknown raw modes replay nothing except that unknown modes are
// mapped to tool_calls semantics, mirroring the resolver so a hand-built-
// AIConfig call can never widen what validation rejected.
func replayReasoningRow(m ChatMessage, replay string) bool {
	if m.Role != RoleAssistant || m.ReasoningContent == "" {
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

// replayReasoningFilter pre-strips ReasoningContent from rows that mode
// replay would NOT put on the wire: the token accounting passes its
// messages through this so countMessageTokens sees exactly the payload
// that will be serialized ("neutral count of the payload it sends" holds
// in both directions of the knob). ReasoningSummary is irrelevant here —
// it never rides the wire in any mode, so it is never counted. Copy-on-
// write: the original slice and rows are never mutated (compaction callers
// share rows with the live history), and an input that needs no stripping
// is returned unchanged.
func replayReasoningFilter(messages []ChatMessage, replay string) []ChatMessage {
	if replay == ReasoningReplayAlways {
		return messages
	}
	filtered := messages
	changed := false
	for i := range messages {
		if replayReasoningRow(messages[i], replay) || messages[i].ReasoningContent == "" {
			continue
		}
		if !changed {
			filtered = make([]ChatMessage, len(messages))
			copy(filtered, messages)
			changed = true
		}
		filtered[i].ReasoningContent = ""
	}
	return filtered
}
