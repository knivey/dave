package main

// Tripwire detectors — run-index.csv columns beyond the prefix-based
// `fabricated` flag. These are deliberately CHEAP, MECHANICAL heuristics
// with known false-positive classes: their job is to flag runs for deeper
// human/pilot inspection, never to verdict (owner ruling after the
// qualitative log review). Semantic shapes — silent false completions,
// in-character monologue — stay with the README's pilot-read protocol.
//
// Detection happens here at logging/aggregation time, over exactly the
// session data the JSONL writer already sees: parsed responses plus the
// request history (for the delivered-URL set). Nothing in this file
// touches the wire, the scenarios, the determinism streams, or the
// existing columns.

import (
	"encoding/json"
	"regexp"
	"strings"
)

// leakSuspectRe is the observed leak shape: a LINE starting with a
// reasoning label + colon ("thought: I will use the enhancement tool...").
// Line-anchored only — "My thought:" mid-sentence is in-character
// monologue, not a leak, and must not match.
var leakSuspectRe = regexp.MustCompile(`(?im)^\s*(thought|thinking|reasoning)\s*:`)

// templateLeakPatterns is the gemma4 special-token family whose literals
// must NEVER appear in visible response content: the template renders
// them, the model emits them as raw text only when the channel structure
// leaked (run-02 shipped a bare `<channel|>` stub; run-05 leaked a long
// `<|tool_call>call:enhance_and_generate_async{prompt:<|"|>…` span —
// mojibake on IRC). Every pattern is prefix-anchored by its own shape
// (`<|…` or the stub forms `<x|>`): ordinary angle-bracket text like
// `<div>` and the words "channel"/"turn" in prose cannot match, and IRC
// color codes (0x03 etc.) are a DIFFERENT class that never collides with
// any of these literals. `<|think` is deliberately open-ended (any
// thinking-token variant); the rest are exact.
var templateLeakPatterns = []string{
	"<|tool_call",
	"<|tool_response",
	"<|channel>",
	"<channel|>",
	"<|turn>",
	"<turn|>",
	"<|think",
	"<|image|>",
}

// containsTemplateLeak reports whether content carries any template-token
// literal. Content-only by design: reasoning_content legitimately holds
// channel-shaped thinking (`<|channel>thought` is the model doing its job
// internally), so only pr.content is ever passed here.
func containsTemplateLeak(content string) bool {
	for _, p := range templateLeakPatterns {
		if strings.Contains(content, p) {
			return true
		}
	}
	return false
}

// imageSiteRe matches image-site URLs with a word-char slug: the harness's
// fabricated host plus whatever the model conjures (6-char base62 from the
// rng, an 8-hex job id used as a slug, ...). Word chars stop the match
// before trailing punctuation so markdown links `[x](url)` and bare urls
// both yield the bare URL string.
var imageSiteRe = regexp.MustCompile(`https://i\.shrews\.xyz/[0-9A-Za-z]+`)

// deliveryMarkerPrefix is the stable prefix of the prod async-delivery
// marker (jobManager.go's "[System: Background task completed — tool: ..."
// injection) that identifies a delivery message.
const deliveryMarkerPrefix = "[System: Background task completed"

// registerJobTool is the advertised name of the register tool, counted by
// the `registers` column (the log review's register-omission finding:
// 136/137 jobs were never registered — the raw count lets analysis derive
// omissions per run).
const registerJobTool = "register_background_job"

// registerCallJobID extracts the job_id argument from a
// register_background_job call's arguments JSON. Malformed JSON or a
// missing/empty job_id yields "" — a register citing no id rescues
// nothing (the orphan-jobs detector ignores it, like the server would).
func registerCallJobID(args string) string {
	var v struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return ""
	}
	return v.JobID
}

// isDeliveryMsg reports whether a history row is an async-result delivery
// injection: wire role system/user/developer (dave's injection_role axis)
// carrying the delivery marker. Assistant rows carrying marker-shaped
// content are FORGERIES, never deliveries, and tool rows are tool results
// — neither may seed the delivered-URL set.
func isDeliveryMsg(m wireMsg) bool {
	switch m.Role {
	case "system", "user", "developer":
		return strings.Contains(m.Content, deliveryMarkerPrefix)
	}
	return false
}

// citesUndeliveredImageURL reports whether content cites at least one
// image-site URL that is not in the delivered set. Before the first
// delivery the set is empty, so every citation counts; a response counts
// once even when it cites several bad URLs (caller increments per
// response, not per URL).
func citesUndeliveredImageURL(content string, delivered map[string]bool) bool {
	for _, u := range imageSiteRe.FindAllString(content, -1) {
		if !delivered[u] {
			return true
		}
	}
	return false
}

// observeResponse runs every per-response tripwire over one parsed
// response, immediately after it is accounted for the JSONL:
//
//   - leak_suspects: content matches the line-anchored reasoning-label
//     regex (false positives: in-character monologue).
//   - template_leaks: content contains a gemma4 template-token literal
//     (`<|tool_call`, `<channel|>`, ...). False positives: a reply
//     deliberately quoting template syntax.
//   - fabricated_urls: content cites an image-site URL absent from the
//     delivered set AT THAT POINT (false positives: stale-but-real links
//     from before a reset are indistinguishable from inventions at this
//     layer).
//   - silent_turns: no tool calls and blank content — on the real bot
//     these are turns where the channel sees nothing.
//   - linkless_finals: the presentation slot — the FIRST no-tool response
//     after a delivery — cites no image-site URL at all (the temp-0.7
//     review's lost-link failure). Tool-call responses (queue_status
//     detours) do not consume the slot.
//   - jobs / registers: raw per-call counts from the response's tool_calls;
//     register calls also fold their job_id argument into the registered
//     set for the session-end orphan_jobs evaluation.
func (s *session) observeResponse(pr parsedResp) {
	if leakSuspectRe.MatchString(pr.content) {
		s.leakSuspects++
	}
	if containsTemplateLeak(pr.content) {
		s.templateLeaks++
	}
	if citesUndeliveredImageURL(pr.content, s.deliveredURLs) {
		s.fabricatedURLs++
	}
	if len(pr.toolCalls) == 0 {
		if strings.TrimSpace(pr.content) == "" {
			s.silentTurns++
		}
		if s.presentationPending {
			s.presentationPending = false
			if !imageSiteRe.MatchString(pr.content) {
				s.linklessFinals++
			}
		}
	}
	for _, tc := range pr.toolCalls {
		switch {
		case isImageJobTool(tc.Function.Name):
			s.jobs++
		case tc.Function.Name == registerJobTool:
			s.registers++
			if id := registerCallJobID(tc.Function.Arguments); id != "" {
				s.registeredJobIDs[id] = true
			}
		}
	}
}

// orphanJobCount is the session-end evaluation behind the `orphan_jobs`
// column: how many fabricated submissions were never cited by ANY
// register_background_job call's arguments. In prod an unregistered job
// means the async monitor never attaches — the image silently never
// arrives (run-05: job 6bc8bcba submitted, never registered, while the
// call counts read almost-perfect). Duplicate registers collapse via the
// set, and a register citing an id that was never submitted rescues
// nothing (a different anomaly, not this column). Evaluated ONCE at
// session end precisely because the registered set only ever grows: a
// register in a later turn rescues an earlier submission, and no register
// can arrive after the final response.
func (s *session) orphanJobCount() int {
	orphans := 0
	for _, id := range s.jobSubmissions {
		if !s.registeredJobIDs[id] {
			orphans++
		}
	}
	return orphans
}

// foldDeliveries folds history rows that are new since the last request
// into the detector state, so the delivered set always describes exactly
// what the request that produced the current response carried:
//
//   - a delivery message REPLACES the delivered-URL set (each delivery
//     describes the CURRENT image — a citation of an earlier delivery's
//     URL after a newer one is stale and must trip fabricated_urls, the
//     observed "reuses the FIRST image's live URL" shape) and arms the
//     presentation slot for linkless_finals;
//   - URLs inside assistant content — forged markers included — never
//     enter the set (isDeliveryMsg's role gate).
//
// Called at the top of session.request(), before the body is marshaled:
// the rows scanned are exactly the rows about to be sent.
func (s *session) foldDeliveries() {
	if s.scannedMsgs > len(s.msgs) {
		s.scannedMsgs = len(s.msgs)
	}
	for _, m := range s.msgs[s.scannedMsgs:] {
		if !isDeliveryMsg(m) {
			continue
		}
		clear(s.deliveredURLs)
		s.presentationPending = true
		for _, u := range imageSiteRe.FindAllString(m.Content, -1) {
			s.deliveredURLs[u] = true
		}
	}
	s.scannedMsgs = len(s.msgs)
}
