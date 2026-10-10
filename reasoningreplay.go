package main

// Reasoning history replay ("reasoningreplay") — attaches stored assistant
// reasoning back onto the wire so thinking models keep their chain of thought
// across tool loops and, on "always", across turns.
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
//   - "tool_calls": replay reasoning ONLY on assistant rows that carry
//     tool calls and sit after the last user row — the
//     current turn's agentic loop. Historical turns are
//     stripped before the next user turn, per the gemma rule.
//   - "always":     every assistant row with reasoning, historical
//     included — for models/proxies that want the full
//     reasoning history (the gemma template will still strip
//     pre-user-turn thoughts itself unless the admin passes
//     preserve_thinking = true).
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

// lastUserRoleIndex reports the position of the last user row — the same
// scan the gemma4 template runs to find its `last_user_idx` reasoning-gate
// boundary. Rows after it belong to the current turn (the tool loop's own
// iterations); everything before it is closed history. -1 when no user row
// exists. Developer/system guidance rows do not reset the boundary: the
// template scans the user role only, and the wire behavior must match.
func lastUserRoleIndex(messages []ChatMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == RoleUser {
			return i
		}
	}
	return -1
}

// replayReasoningRow is the single eligibility predicate — whether row i's
// stored reasoning goes on the wire in mode replay. Both serializers (add
// reasoning_content / reasoning items) and the token-accounting filter
// (strip what won't be serialized) call this, so the two can never drift.
//
// Requires: assistant role, non-empty reasoning. Then: "always" takes any
// such row; "tool_calls" takes tool-call rows of the CURRENT turn
// (historical tool-call rows stay stripped — the gemma rule is per-turn);
// "off" and unknown raw modes replay nothing except that unknown modes are
// mapped to tool_calls semantics, mirroring the resolver so a
// hand-built-AIConfig call can never widen what validation rejected.
func replayReasoningRow(messages []ChatMessage, i int, replay string) bool {
	m := messages[i]
	if m.Role != RoleAssistant || m.ReasoningContent == "" {
		return false
	}
	switch replay {
	case ReasoningReplayAlways:
		return true
	case ReasoningReplayToolCalls /* defensive default */, "":
		return len(m.ToolCalls) > 0 && i > lastUserRoleIndex(messages)
	case ReasoningReplayOff:
		return false
	}
	// Unknown raw value: same semantics as tool_calls (resolver's fallback).
	return len(m.ToolCalls) > 0 && i > lastUserRoleIndex(messages)
}

// replayReasoningFilter pre-strips ReasoningContent from rows that mode
// replay would NOT put on the wire: the token accounting passes its
// messages through this so countMessageTokens sees exactly the payload
// that will be serialized ("neutral count of the payload it sends" holds
// in both directions of the knob). Copy-on-write: the original slice and
// rows are never mutated (compaction callers share rows with the live
// history), and an input that needs no stripping is returned unchanged.
func replayReasoningFilter(messages []ChatMessage, replay string) []ChatMessage {
	if replay == ReasoningReplayAlways {
		return messages
	}
	filtered := messages
	changed := false
	for i := range messages {
		if replayReasoningRow(messages, i, replay) || messages[i].ReasoningContent == "" {
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
