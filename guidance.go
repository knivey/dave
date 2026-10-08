package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// Unified guidance injection — Knob 1 (payload role) and Knob 2 (trailing
// user turn) resolvers plus the shared message builder used by every site
// that injects mid-conversation guidance (async background results,
// empty-response retry corrections). Design:
// docs/superpowers/specs/2026-10-08-guidance-injection-role-design.md
//
// The two knobs are deliberately NOT collapsed into one: the payload keeps
// its correct semantic role (system by default) even when the provider
// cannot end a request on it — that constraint is satisfied by APPENDING a
// short user turn (two rows), never by changing the payload's role. Role
// escalation was considered and rejected during design review.

// Async result delivery modes (spec addendum): "message" routes the
// notification through the guidance mechanism; "tool" delivers it as a
// synthetic assistant tool-call + tool-result round-trip.
const (
	asyncDeliveryMessage = "message"
	asyncDeliveryTool    = "tool"
)

// asyncJobStatusTool is the function name used for the synthetic round-trip
// in tool delivery mode. Providers do not require history tool calls to
// have been advertised; reusing wait_for_job would contradict the "do not
// wait" instruction in register_background_job's description.
const asyncJobStatusTool = "job_status"

// guidanceRole resolves Knob 1: the wire role of a guidance payload row.
// Cascade happens in ApplyDefaults (command > service > "system"); this
// only defends against hand-built configs that skipped it — an unknown
// role falls back to system rather than sending garbage to the API.
func guidanceRole(cfg AIConfig) string {
	switch cfg.InjectionRole {
	case RoleSystem, RoleDeveloper, RoleUser:
		return cfg.InjectionRole
	}
	return RoleSystem
}

// needsUserSuffix resolves Knob 2: whether a guidance row must be followed
// by a user turn. An explicit config value wins in both directions
// (needsusersuffix = false suppresses even the anthropic/ auto-detect);
// nil cascades to the model-based default (anthropic/ models need a user
// turn to answer).
func needsUserSuffix(cfg AIConfig) bool {
	if cfg.NeedsUserSuffix != nil {
		return *cfg.NeedsUserSuffix
	}
	return modelNeedsUserSuffix(cfg.Model)
}

// asyncResultDelivery resolves the async delivery mode (command > service >
// "message", materialized in ApplyDefaults; unknown values fall back to
// "message" defensively).
func asyncResultDelivery(cfg AIConfig) string {
	switch cfg.AsyncResultDelivery {
	case asyncDeliveryMessage, asyncDeliveryTool:
		return cfg.AsyncResultDelivery
	}
	return asyncDeliveryMessage
}

// asyncResultRole is the value exposed to system templates via
// {{.AsyncResultRole}} and to the register_background_job description: the
// wire role background results arrive under — "tool" in tool delivery
// mode, else the guidance Knob 1 role.
func asyncResultRole(cfg AIConfig) string {
	if asyncResultDelivery(cfg) == asyncDeliveryTool {
		return asyncDeliveryTool
	}
	return guidanceRole(cfg)
}

// guidanceMessages builds the message rows for one guidance injection:
// the payload row under the resolved role, plus — only when the row would
// be the LAST message of the request (trailing), the provider needs an
// answerable turn (Knob 2), and the payload is not already a user row —
// the site-supplied user suffix. suffixText is per-site because the
// wording references what precedes it ("above background task result" vs
// the correction's instruction).
func guidanceMessages(cfg AIConfig, content, suffixText string, trailing bool) []ChatMessage {
	role := guidanceRole(cfg)
	msgs := []ChatMessage{{Role: role, Content: content}}
	if trailing && role != RoleUser && needsUserSuffix(cfg) {
		msgs = append(msgs, ChatMessage{Role: RoleUser, Content: suffixText})
	}
	return msgs
}

// asyncToolRoundTrip builds the synthetic tool round-trip for tool
// delivery mode: an assistant row carrying a job_status tool call for the
// job, plus the matching RoleTool result row with the (marker-prefixed)
// payload. The pair must be persisted atomically and the session's
// response chain cleared — see injectAsyncResultFromDB.
func asyncToolRoundTrip(job PendingJob, content string) ([]ChatMessage, error) {
	callID, err := newToolCallIDFn()
	if err != nil {
		return nil, err
	}
	args := fmt.Appendf(nil, `{"job_id":%q}`, job.JobID)
	return []ChatMessage{
		{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{{
				ID:   callID,
				Type: "function",
				Function: FunctionCall{
					Name:      asyncJobStatusTool,
					Arguments: string(args),
				},
			}},
		},
		{
			Role:       RoleTool,
			ToolCallID: callID,
			Content:    content,
		},
	}, nil
}

// newToolCallID mints an OpenAI-style tool call id (call_ + 24 hex chars).
// Providers validate pairing, not provenance; both ends of the pair are
// ours. Var indirection (the connectMCPServerImpl pattern) lets tests
// force the failure path.
var newToolCallIDFn = newToolCallID

func newToolCallID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "call_" + hex.EncodeToString(b[:]), nil
}

// messagesEndWith reports whether msgs ends with exactly seq (role and
// content equality). Used to collapse back-to-back identical guidance
// injections within a turn: one nudge is enough, and repeated identical
// trailing rows would only dilute the actual prompt.
func messagesEndWith(msgs, seq []ChatMessage) bool {
	if len(seq) == 0 || len(msgs) < len(seq) {
		return false
	}
	tail := msgs[len(msgs)-len(seq):]
	for i := range seq {
		if tail[i].Role != seq[i].Role || tail[i].Content != seq[i].Content {
			return false
		}
	}
	return true
}
