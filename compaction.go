package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/lrstanley/girc"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"gorm.io/gorm"
)

// CompactionConfig controls automatic and manual session compaction.
//
// When the prompt token count of the most recent turn exceeds AutoThreshold
// times the effective context window for the session's model, an automatic
// compaction is triggered (if AutoEnabled). Manual compaction (via the
// `^compact$` IRC command or the TUI `/compact` command) ignores these
// thresholds and runs whenever invoked, subject only to MinTurns.
//
// FUTURE DIRECTION (Option A — logical_seq):
//
//	The current implementation re-inserts preserved tail rows on each
//	compaction so that ORDER BY id ASC produces the correct logical order
//	for the live message stream. This duplicates content on disk for hot
//	sessions that compact repeatedly. We mitigate the user-visible impact
//	by tagging tail-copies with SourceCompactionID and marking them
//	superseded=true on re-archival, but disk storage still grows linearly
//	with compaction count (bounded only by MaxAgeDays session cleanup).
//
//	A cleaner long-term approach is to add a Message.LogicalSeq column,
//	query live history via ORDER BY logical_seq, and on compaction insert
//	the fresh-system + summary rows with seq values lower than the
//	existing live tail's lowest seq — never copying tail rows. This
//	eliminates duplication entirely. It requires migrating every
//	ORDER BY id on messages to ORDER BY logical_seq and backfilling the
//	column for existing rows. Not implemented in this change; consider
//	when the codebase grows other ordering needs or when production data
//	shows pathological growth.
type CompactionConfig struct {
	Enabled       bool    `toml:"enabled"`
	AutoEnabled   bool    `toml:"auto_enabled"`
	AutoThreshold float64 `toml:"auto_threshold"`
	// ContextWindow is the fallback token limit used when the session's
	// service does not set context_window. When both the service window
	// and this fallback are 0, auto-compaction is disabled for sessions
	// on services without a window.
	ContextWindow int `toml:"context_window"`
	// MinTurns: the minimum number of turns (user → assistant pairs) the
	// session must contain before any compaction will run. Below this, the
	// compactor refuses to act because there's not enough material to
	// summarize meaningfully.
	MinTurns int `toml:"min_turns"`
	// TargetFraction is the post-compaction live-context target as a
	// fraction of the effective context window (the service's
	// context_window, else this section's context_window fallback). When
	// the 2/3-rule preserved tail would exceed the budget
	// (window × fraction), the cut advances — archiving more turns and
	// shrinking the tail — until the projection fits or only one tail
	// turn remains. ApplyDefaults maps <= 0 to 0.4 (0 is the TOML zero
	// value and means unset); the feature is DISABLED by setting it to
	// >= 1.0, which passes through ApplyDefaults untouched and is
	// treated as "no budget constraint". Inert when no context window is
	// resolvable (the 2/3 rule alone governs).
	TargetFraction float64 `toml:"target_fraction"`
	// MaxSummaryTokens caps the summarizer call's max_tokens, bounding
	// summary size so a verbose summarizer cannot eat the context budget
	// it is supposed to free. 0 = inherit the command's maxtokens/none.
	MaxSummaryTokens int `toml:"max_summary_tokens"`
	// PromptTemplate optionally overrides the built-in summarizer prompt.
	PromptTemplate string `toml:"prompt_template"`
}

func (c *CompactionConfig) ApplyDefaults() {
	if c.AutoThreshold <= 0 {
		c.AutoThreshold = 0.7
	}
	if c.MinTurns <= 0 {
		c.MinTurns = 6
	}
	if c.TargetFraction <= 0 {
		// 0 means unset (TOML zero value). Disable is expressed as
		// >= 1.0, which must pass through untouched — see the field
		// doc above.
		c.TargetFraction = 0.4
	}
}

const defaultCompactionPrompt = `You are summarizing the early portion of an ongoing IRC conversation between a user and an AI assistant.
Produce a concise but information-dense summary of what has happened so far. The summary will replace the original messages in the conversation history, so preserve everything the assistant or user might need to continue coherently:

- Names, nicknames, channels, networks mentioned.
- Key facts, decisions, conclusions, and unresolved questions.
- User preferences, instructions, or constraints stated by the user.
- Any code, identifiers, paths, URLs, numbers, or short literal strings that were discussed.
- Tools that were called and their outcomes (success/failure + key result data).
- Image content references (use abstract descriptions like "image of a sunset"; the actual images are removed).
- Open threads or topics that were paused mid-conversation.

If the transcript begins with a prior conversation summary, treat it as established context and carry its facts forward, merging them into the updated summary.

Do NOT invent details. If something was unclear, say so. Output only the summary text — no preamble, no headers, no markdown formatting.`

// Sentinels surfaced to user-facing layers via notice templates.
var (
	ErrCompactionDisabled    = errors.New("compaction disabled")
	ErrCompactionTooShort    = errors.New("not enough history to compact")
	ErrCompactionNoActive    = errors.New("no active session")
	ErrCompactionInProgress  = errors.New("compaction already in progress")
	ErrCompactionEmptyResult = errors.New("summarizer returned empty content")
	// ErrCompactionSessionChanged is returned by the optimistic guard at
	// the top of the compaction transaction when rows appeared after the
	// snapshot was taken (callSummarizer takes seconds and is uncoordinated
	// with AddMessage; the per-session compactionMu only serializes
	// compaction-vs-compaction). Aborting converts what used to be a
	// silent live-ordering corruption into a clean retry for the caller.
	ErrCompactionSessionChanged = errors.New("session changed during compaction")
	// ErrCompactionNoSystemRow is a defensive refusal when the live history
	// does not start with a system row — e.g. a legacy session that was
	// already scrambled by the mid-flight-insert race before the guard
	// above existed. Every downstream step assumes dbMsgs[0] is the system
	// prompt row.
	ErrCompactionNoSystemRow = errors.New("live history does not start with a system message")
	// ErrCompactionNothingNew refuses a degenerate repeat compaction: the
	// 2/3 cut landed entirely on tail-copies from the prior compaction, so
	// there is zero genuinely-new material to summarize. Re-summarizing
	// already-summarized tail content would burn a summarizer call and
	// degrade the rolling summary's quality for no benefit — pure churn.
	// Detected AFTER the partition loop and BEFORE the summarizer call so
	// no API cost is wasted.
	ErrCompactionNothingNew = errors.New("no new messages to compact since the last compaction")
)

// compactionMu serializes compactions per session ID.
var compactionMu sync.Map // key: int64 sessionID → *sync.Mutex

func getCompactionLock(sessionID int64) *sync.Mutex {
	v, _ := compactionMu.LoadOrStore(sessionID, &sync.Mutex{})
	return v.(*sync.Mutex)
}

// CompactionResult is returned to callers (IRC/TUI) for user-facing notices.
type CompactionResult struct {
	CompactionID     int64
	ArchivedCount    int
	FirstArchivedID  int64
	LastArchivedID   int64
	SummaryMessageID int64
	PromptTokens     int
	CompletionTokens int
	DurationMs       int
	// Observability fields, used only for notices and log lines:
	//
	//   SupersededCount    — prior-compaction tail-copies marked superseded
	//                        (content already covered by an earlier summary).
	//   PriorSummaryCount  — prior summary rows folded into this summarizer
	//                        call (the rolling-summary chain inputs).
	//   LiveMessages       — row count of live history after the compaction.
	//   LiveTokensEst      — token estimate over exactly those live rows:
	//                        countMessageTokens (real tokenizer when the
	//                        model's encoding is known, o200k_base
	//                        approximation otherwise) when a model is
	//                        configured, estimateTokens runes/4 over
	//                        Content otherwise (MultiContent / tool-call
	//                        payloads excluded on that fallback path) —
	//                        display-only, never a decision input.
	//   ContextBefore      — the pre-compaction context size shown in
	//                        notices as "was ~X": the last chat turn's REAL
	//                        prompt_tokens when a usage row exists, else
	//                        perMsg × live-message count estimate.
	//
	// The goal these serve: the owner wants session tokens kept low and
	// wants to SEE whether compaction achieves that — which requires the
	// summary's own token count and the post-compaction live-history size,
	// neither of which is visible anywhere else on IRC.
	SupersededCount   int
	PriorSummaryCount int
	LiveMessages      int
	LiveTokensEst     int
	ContextBefore     int
}

// estimateTokens is a crude chars/4 heuristic for token counts
// (English-centric: roughly four characters per token). It is used ONLY
// for user-facing estimates and log lines — never for decisions
// (thresholds, truncation, and compaction triggers all use real usage
// numbers returned by the API).
func estimateTokens(s string) int { return utf8.RuneCountInString(s) / 4 }

// summarySizeWarnRatio is the completion/prompt ratio at which the
// summarizer's output is considered suspiciously large. A good summary is
// far smaller than its input; a ratio above 0.5 means the summarizer is
// echoing the transcript rather than condensing it — worth an admin's
// attention (log tripwire only, no behavior change).
const summarySizeWarnRatio = 0.5

// summarySizeRatio returns completionTokens / promptTokens, and 0 when
// promptTokens <= 0 (an unknown prompt size says nothing about summary
// quality, so it must not look like a tiny summary).
func summarySizeRatio(promptTokens, completionTokens int) float64 {
	if promptTokens <= 0 {
		return 0
	}
	return float64(completionTokens) / float64(promptTokens)
}

// tokensPerMessage estimates the model's token cost of one live-history
// message, derived from the API's OWN usage accounting: the last recorded
// turn's prompt_tokens divided by the number of live messages that prompt
// contained (capped by MaxHistory truncation — GetMessages sends
// min(live, maxHistory+1) rows). This captures chat framing, tool-call
// JSON, and image tokens for the actual model.
//
// BASIS CASCADE (the returned basis string documents which path was
// taken for logging):
//
//   - "usage" (PRIMARY): real API numbers for THIS provider beat any
//     tokenizer — they already include the provider's own framing,
//     tool-JSON and image accounting, inflated or not. A tokenizer
//     estimate can only replace the provider's number with ours; it
//     cannot improve on it as a measure of what that provider charges.
//
//   - "tokenizer:<enc>" / "tokenizer:<enc>,approx": no usage rows yet
//     (fresh sessions, manual compact before any turn) and a model is
//     configured — countMessageTokens over the payload projection the
//     next request would send (the live rows truncated by the same
//     min(live, maxHistory+1) TruncateHistory rule the usage path
//     mirrors), averaged over the rows it contains. "approx" marks
//     models whose real tokenizer is unknown (o200k_base stand-in).
//
//   - "chars/4": no usage rows AND no model — the old estimateTokens
//     (runes/4) fallback over the live rows' Content, still reachable
//     because tokensPerMessage must never fail a compaction.
//
// The DECISION math (budget projection) is unchanged by which basis
// wins: every path yields perMsg with the same 1.0 floor semantics.
func tokensPerMessage(lastUsage *TurnUsage, liveMsgs []Message, maxHistory int, model string) (perMsg float64, basis string) {
	if len(liveMsgs) == 0 {
		// Nothing to average over; the floor below is the only sane
		// answer so budget math can't divide to infinity.
		return 1.0, "chars/4"
	}
	// effective/rowsForProjection mirror TruncateHistory: the next
	// request (and the one that produced lastUsage) carried at most
	// maxHistory+1 rows — msgs[0] plus the newest maxHistory. maxHistory
	// == 0 is treated as "no cap" here: production configs always have a
	// positive MaxHistory (AIConfig.ApplyDefaults backfills the service
	// default), and a literal 0 would mean TruncateHistory sends a
	// single message — an estimate-quality corner we deliberately don't
	// model.
	rowsForProjection := liveMsgs
	effective := len(liveMsgs)
	if maxHistory > 0 && effective > maxHistory+1 {
		effective = maxHistory + 1
		rowsForProjection = append([]Message{liveMsgs[0]}, liveMsgs[len(liveMsgs)-maxHistory:]...)
	}
	if lastUsage != nil && lastUsage.PromptTokens > 0 {
		// The last turn's prompt contained at most maxHistory+1 rows
		// (TruncateHistory), so dividing by more rows than that would
		// understate the per-message cost.
		perMsg = float64(lastUsage.PromptTokens) / float64(effective)
		basis = "usage"
	} else if model != "" {
		tc := countMessageTokens(model, messagesToChat(rowsForProjection))
		perMsg = float64(tc.Tokens) / float64(len(rowsForProjection))
		if tc.Encoding == "chars/4-estimate" {
			// Encoder init failed (corrupt embedded data — unreachable
			// with the vendored files); the structured chars/4 estimate
			// from countMessageTokens is still a per-message average.
			basis = "chars/4-estimate"
		} else if tc.Exact {
			basis = "tokenizer:" + tc.Encoding
		} else {
			basis = "tokenizer:" + tc.Encoding + ",approx"
		}
	} else {
		total := 0
		for i := range liveMsgs {
			total += estimateTokens(liveMsgs[i].Content)
		}
		perMsg = float64(total) / float64(len(liveMsgs))
		basis = "chars/4"
	}
	if perMsg < 1.0 {
		perMsg = 1.0
	}
	return perMsg, basis
}

// effectiveContextWindow resolves the token window to budget against, in
// ONE readConfig snapshot (a concurrent /reload can never splice a service
// window from one config generation onto a compaction fallback from
// another): the session's service context_window ("service") wins when
// set > 0, else the global [compaction] context_window fallback
// ("compaction"), else 0 ("none").
func effectiveContextWindow(cfg AIConfig) (window int, source string) {
	readConfig(func() {
		if w := config.Services[cfg.Service].ContextWindow; w > 0 {
			window, source = w, "service"
		} else if w := config.Compaction.ContextWindow; w > 0 {
			window, source = w, "compaction"
		} else {
			source = "none"
		}
	})
	return window, source
}

// defaultSummaryTokensAssumed is the summary-size guess used by
// token-aware tail sizing when [compaction] max_summary_tokens is not
// configured: a mid-range estimate of a concise summary's size. When
// max_summary_tokens IS configured the cap is assumed to fill — the
// worst case for budgeting. The completion log records both the assumed
// and actual summary sizes so drift is one diff away.
const defaultSummaryTokensAssumed = 512

// projectPostCompactionTokens estimates the live-context size immediately
// after a compaction cutting at `cut`: one system message plus the
// preserved tail rows at perMsg each, plus the summary at its assumed
// size. This is the exact formula advanceCutForTokenBudget budgets
// against, reused by the trigger/completion logs so all three numbers
// can be compared directly.
func projectPostCompactionTokens(messages []ChatMessage, turns []messageTurn, cut int, perMsg float64, summaryTokensAssumed int) float64 {
	tailMessages := 0
	if cut >= 0 && cut < len(turns) {
		tailMessages = len(messages) - turns[cut].end
		if tailMessages < 0 {
			tailMessages = 0
		}
	}
	return float64(summaryTokensAssumed) + perMsg*float64(1+tailMessages)
}

// advanceCutForTokenBudget walks the cut forward (archiving more turns,
// shrinking the preserved tail) while the projected post-compaction
// context exceeds the budget. Projection = summaryTokensAssumed +
// perMsg*(1 /*system*/ + tailMessages). It never retreats below the 2/3
// cut, never leaves fewer than one tail turn, and only lands on
// boundaries whose tail starts with RoleUser (same invariant as
// pickCompactionCutTurn — reuse the same check). Returns the final cut.
//
// A budget <= 0 (no window resolvable, or TargetFraction disabled) is a
// no-op: the 2/3 cut is returned unchanged. When the budget is
// unreachable even at the smallest legal tail (one turn), the cut stops
// there anyway — estimate quality must never break the structural
// invariants; the trigger log's projection_tokens makes the miss visible.
func advanceCutForTokenBudget(messages []ChatMessage, turns []messageTurn, cut int, budgetTokens float64, perMsg float64, summaryTokensAssumed int) int {
	if budgetTokens <= 0 || perMsg <= 0 || cut < 1 {
		return cut
	}
	for cut < len(turns)-1 &&
		projectPostCompactionTokens(messages, turns, cut, perMsg, summaryTokensAssumed) > budgetTokens {
		// Advance to the next boundary whose tail starts with RoleUser,
		// skipping non-user boundaries (async-injected RoleSystem rows
		// etc.). Bounded by len(turns)-1: at least one tail turn must
		// survive. If no further valid boundary exists, keep the
		// current cut — same refusal shape pickCompactionCutTurn uses.
		next := -1
		for cand := cut + 1; cand < len(turns)-1; cand++ {
			boundary := turns[cand].end
			if boundary < len(messages) && messages[boundary].Role == RoleUser {
				next = cand
				break
			}
		}
		if next < 0 {
			return cut
		}
		cut = next
	}
	return cut
}

// pickCompactionCutTurn returns the index into `turns` such that the inclusive
// range turns[1..cut] should be archived. We always start at turn 1 because
// turn 0 contains the system prompt (msg id 0) which must never be split.
//
// The 2/3 rule: pick the smallest cut such that the archived turns cover at
// least 2/3 of the *non-system* messages, while leaving at least one turn in
// the preserved tail.
//
// CRITICAL INVARIANT: the preserved tail (messages[turns[cut].end:]) MUST
// begin with a RoleUser message. Some providers reject a message chain that
// jumps `system → assistant` with no intervening user turn (xAI/Grok and
// some Anthropic-compatible proxies are known to do this). buildTurns starts
// a new turn whenever it sees a RoleUser message, so the message at
// turns[cut].end is normally RoleUser by construction. However, if a session
// has had async-result injection or other oddities that placed a RoleSystem
// or RoleAssistant message at a turn boundary, we walk forward looking for a
// cut whose tail starts with RoleUser. If none exists before the last turn,
// we refuse the compaction.
//
// Returns -1 if there's not enough material or no safe cut exists.
func pickCompactionCutTurn(messages []ChatMessage, turns []messageTurn) int {
	if len(turns) < 3 {
		// Need: turn 0 (system) + at least one to archive + at least one tail.
		return -1
	}
	totalNonSystem := 0
	for i := 1; i < len(turns); i++ {
		totalNonSystem += turns[i].end - turns[i].start
	}
	if totalNonSystem < 2 {
		return -1
	}
	// totalNonSystem >= 2 is guaranteed by the guard above, so
	// (totalNonSystem*2)/3 >= 4/3 truncates to at least 1 — no clamp to 1
	// is needed (or possible to hit) here.
	target := (totalNonSystem * 2) / 3
	covered := 0
	for cut := 1; cut < len(turns)-1; cut++ {
		covered += turns[cut].end - turns[cut].start
		if covered < target {
			continue
		}
		// Tail starts at turns[cut].end. Verify it's a RoleUser message;
		// if not, advance until we find one or run out of turns.
		tailStart := turns[cut].end
		if tailStart < len(messages) && messages[tailStart].Role == RoleUser {
			return cut
		}
		// Advance cut forward looking for a tail boundary that starts
		// with a user message. The forward search is bounded by
		// len(turns)-1 (we must leave at least one turn in the tail).
		for next := cut + 1; next < len(turns)-1; next++ {
			boundary := turns[next].end
			if boundary < len(messages) && messages[boundary].Role == RoleUser {
				return next
			}
		}
		// No safe cut exists.
		return -1
	}
	return -1
}

// stripImagesForSummary returns a copy of messages with all image_url parts
// replaced by text placeholders. The summarizer call should never receive
// raw image data — the originals are preserved on the archived rows for the
// future history viewer.
func stripImagesForSummary(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, len(messages))
	for i, m := range messages {
		out[i] = m
		if len(m.MultiContent) == 0 {
			continue
		}
		newParts := make([]MessagePart, 0, len(m.MultiContent))
		hasText := false
		for _, p := range m.MultiContent {
			if p.Type == PartTypeImageURL {
				newParts = append(newParts, MessagePart{Type: PartTypeText, Text: "[image]"})
			} else {
				if p.Text != "" {
					hasText = true
				}
				newParts = append(newParts, p)
			}
		}
		if !hasText && out[i].Content == "" {
			// Belt-and-suspenders: never send a totally-empty user message.
			newParts = append(newParts, MessagePart{Type: PartTypeText, Text: "[image only]"})
		}
		out[i].MultiContent = newParts
	}
	return out
}

// renderFreshSystemPrompt re-renders the session's chat-command system
// prompt template with the current SystemPromptData. If the template is
// nil or rendering fails, falls back to fallback.
//
// client may be nil — in that case ChanNicks is left empty.
func renderFreshSystemPrompt(cfg AIConfig, network Network, client *girc.Client, channel, userNick, fallback string) string {
	if cfg.SystemTmpl == nil {
		if cfg.System != "" {
			return cfg.System
		}
		return fallback
	}
	data := buildSystemPromptData(network, client, channel, userNick)
	var buf strings.Builder
	if err := cfg.SystemTmpl.Execute(&buf, data); err != nil {
		return fallback
	}
	return buf.String()
}

// callSummarizer makes a one-shot non-streaming chat-completion call to the
// session's own service/model. It bypasses the session bookkeeping in
// chatRunner (no addContext, no storeUsage) and returns the raw summary text
// plus usage and elapsed time.
//
// Reasoning, tools, and streaming are all disabled. Responses API is never
// used here — the session-state implications are too tangled for a transient
// helper, and Chat Completions is universally supported.
//
// maxSummaryTokens, when > 0, overrides cfg.MaxTokens for this call — the
// [compaction] max_summary_tokens cap bounding summary size.
func callSummarizer(ctx context.Context, cfg AIConfig, summarizerSys string, archived []ChatMessage, sessionID int64, maxSummaryTokens int) (string, *Usage, int, error) {
	var svc Service
	var svcOK bool
	readConfig(func() { svc, svcOK = config.Services[cfg.Service] })
	if !svcOK {
		// A zero-value Service would hand the SDK an empty BaseURL, which
		// fails downstream as an opaque upstream 401 (or a call to the
		// SDK's default endpoint with no key). Fail here instead, naming
		// the missing service — usually a chats.toml/services.toml
		// mismatch or a /reload that dropped the service.
		return "", nil, 0, fmt.Errorf("summarizer service %q not found in config", cfg.Service)
	}

	transport := newDaveTransport(nil, nil)
	transport.setAPILogger(apiLogger, sessionID)
	transport.setCaptureBody(true)
	httpClient := &http.Client{Transport: transport}
	openaiClient := openai.NewClient(
		option.WithAPIKey(svc.Key),
		option.WithBaseURL(svc.BaseURL),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(2),
	)

	// Build messages: a single system instruction asking for a summary,
	// followed by the archived conversation as context. Each archived
	// message is rendered as plain user-role text with a role tag so the
	// summarizer doesn't mistake it for a real conversation it's part of.
	var b strings.Builder
	b.WriteString("Conversation transcript to summarize:\n\n")
	for _, m := range archived {
		switch m.Role {
		case RoleSystem:
			b.WriteString("[system] ")
		case RoleUser:
			b.WriteString("[user] ")
		case RoleAssistant:
			b.WriteString("[assistant] ")
		case RoleTool:
			b.WriteString("[tool] ")
		default:
			b.WriteString("[" + m.Role + "] ")
		}
		if len(m.MultiContent) > 0 {
			parts := make([]string, 0, len(m.MultiContent))
			for _, p := range m.MultiContent {
				if p.Type == PartTypeImageURL {
					parts = append(parts, "[image]")
				} else if p.Text != "" {
					parts = append(parts, p.Text)
				}
			}
			b.WriteString(strings.Join(parts, " "))
		} else {
			b.WriteString(m.Content)
		}
		if len(m.ToolCalls) > 0 {
			b.WriteString(" {tool_calls:")
			for _, tc := range m.ToolCalls {
				b.WriteString(" ")
				b.WriteString(tc.Function.Name)
			}
			b.WriteString("}")
		}
		b.WriteString("\n")
	}

	summarizerCfg := cfg
	summarizerCfg.Streaming = false
	summarizerCfg.ResponsesAPI = false
	summarizerCfg.ReasoningEffort = ""
	// Defensive symmetry with the effort clear above: the summarizer forces
	// Chat Completions (which never reads ReasoningSummary), but if it ever
	// flips back to the Responses API, summary tokens would silently ride
	// along on compaction calls (billed as output tokens).
	summarizerCfg.ReasoningSummary = ""
	summarizerCfg.MCPs = nil
	// Cap the summarizer's output so a verbose summary cannot eat the
	// context budget it is supposed to free. 0 = inherit the command's
	// maxtokens/none (buildChatCompletionParams already treats 0 as
	// "send no max_tokens").
	if maxSummaryTokens > 0 {
		summarizerCfg.MaxTokens = maxSummaryTokens
	}

	msgs := []ChatMessage{
		{Role: RoleSystem, Content: summarizerSys},
		{Role: RoleUser, Content: b.String()},
	}

	apiCtx, cancel := context.WithTimeout(ctx, summarizerCfg.Timeout)
	defer cancel()
	params := buildChatCompletionParams(summarizerCfg, msgs, nil, apiIdentity{})
	start := time.Now()
	resp, err := openaiClient.Chat.Completions.New(apiCtx, params)
	dur := int(time.Since(start) / time.Millisecond)
	if err != nil {
		return "", nil, dur, err
	}
	text, _, _, usage := parseChatCompletionResponse(*resp)
	return strings.TrimSpace(text), usage, dur, nil
}

// CompactSessionInputs encapsulates the non-config arguments for compaction.
// We use a struct to keep the SessionManager method signature reasonable.
type CompactSessionInputs struct {
	SessionID int64
	Network   Network
	Channel   string
	UserNick  string
	Client    *girc.Client // may be nil (e.g. background trigger when client unavailable)
	Trigger   string       // "manual" or "auto"
}

// CompactSession performs a single compaction event on the given session.
// See docs/queue-and-sessions.md and the design notes in todo.md (Phase 3 →
// Session compacting). Algorithm:
//
//  1. Acquire the per-session compaction lock.
//  2. Load all live messages (loadDBSessionMessages already filters archived).
//  3. Pick a turn-aligned cut point covering the first ~2/3 of non-system
//     messages, never splitting tool-call turns.
//  4. Build a transient summarizer call (Chat Completions, no tools, images
//     stripped) using the session's own service/model.
//  5. In a transaction:
//     - Insert a freshly-rendered system prompt as a new RoleSystem message.
//     - Insert the summary as a tagged RoleSystem message.
//     - Mark the original system message + all archived turn messages as
//     archived = true with compaction_id = new row.
//     - Reset Session.ResponseID to nil so any Responses API chain restarts.
func (sm *SessionManager) CompactSession(ctx context.Context, inputs CompactSessionInputs, cfg AIConfig) (*CompactionResult, error) {
	logger := newLogger("compaction")

	mu := getCompactionLock(inputs.SessionID)
	if !mu.TryLock() {
		return nil, ErrCompactionInProgress
	}
	defer mu.Unlock()

	session, err := sm.GetSession(inputs.SessionID)
	if err != nil || session == nil {
		return nil, fmt.Errorf("loading session: %w", err)
	}

	dbMsgs, err := loadDBSessionMessages(inputs.SessionID)
	if err != nil {
		return nil, fmt.Errorf("loading messages: %w", err)
	}
	if len(dbMsgs) == 0 {
		return nil, ErrCompactionTooShort
	}

	// Defensive state check (cheap insurance against sessions that were
	// already scrambled by the mid-flight-insert race — see the
	// session-changed guard inside the transaction below): everything
	// downstream assumes dbMsgs[0] is the system prompt row —
	// renderFreshSystemPrompt's fallback content, pickCompactionCutTurn's
	// turn-0 skip, and the partition loop that starts at i=1. If it isn't,
	// refuse rather than archive the wrong rows.
	if dbMsgs[0].Role != RoleSystem {
		logger.Warn("refusing compaction: live history does not start with a system message",
			"session", inputs.SessionID, "first_role", dbMsgs[0].Role)
		return nil, ErrCompactionNoSystemRow
	}

	chatMsgs := make([]ChatMessage, len(dbMsgs))
	for i, dm := range dbMsgs {
		chatMsgs[i] = messageFromDB(dm)
	}

	turns := buildTurns(chatMsgs)

	// Single config snapshot for the whole compaction: MinTurns below and
	// PromptTemplate further down both read this one copy, so a concurrent
	// /reload can never mix two config generations into one event (e.g.
	// MinTurns from before the reload gating a prompt template from after).
	var ccfg CompactionConfig
	readConfig(func() { ccfg = config.Compaction })
	if len(turns)-1 < ccfg.MinTurns {
		return nil, ErrCompactionTooShort
	}

	cut := pickCompactionCutTurn(chatMsgs, turns)
	if cut < 1 {
		return nil, ErrCompactionTooShort
	}

	// --- Token-aware tail sizing (advances the cut only) ---
	//
	// DESIGN NOTE — why the summary size is ASSUMED, not actual: the
	// archived range must be final BEFORE the summarizer call (archiving
	// unsummarized turns after the fact would silently lose data — the
	// summary is built from exactly the rows marked archived). So the
	// budget projection uses an assumed summary size (the configured
	// max_summary_tokens cap, or a mid-range default), and the
	// completion log records assumed-vs-actual so drift is visible.
	//
	// Invariant safety: whatever the estimate quality, the advanced cut
	// still only lands on RoleUser-starting boundaries and still leaves
	// at least one tail turn (advanceCutForTokenBudget enforces both),
	// and everything downstream (partition loop, NothingNew refusal,
	// session-changed guard, prior-summary folding, supersession)
	// computes from the FINAL cut.
	//
	// The last-turn usage row is fetched here (once, reused for logging
	// and notices): ShouldAutoCompact already fetched its own copy on the
	// auto path, but manual compaction has no such prefetch, and the row
	// may have changed in between regardless.
	var lastUsage *TurnUsage
	if theDB != nil {
		if tu, err := getLastTurnUsageForSession(inputs.SessionID); err == nil {
			lastUsage = tu
		}
	}
	perMsg, estBasis := tokensPerMessage(lastUsage, dbMsgs, cfg.MaxHistory, cfg.Model)
	// Dave's own neutral token count of the payload the NEXT request
	// would send: the live rows truncated by the same min(live,
	// maxHistory+1) TruncateHistory rule a real turn applies (GetMessages),
	// converted to ChatMessages. This is the number to diff against the
	// provider-reported last_prompt_tokens below — a persistent gap means
	// the provider's accounting (or template) differs from the payload,
	// not that the payload grew. Unconditional on model: an empty/unknown
	// model still counts via the o200k_base approximation (Exact=false).
	ourCount := countMessageTokens(cfg.Model, TruncateHistory(chatMsgs, cfg.MaxHistory))

	// The window snapshot is taken by effectiveContextWindow's own
	// readConfig (shared with ShouldAutoCompact); a /reload racing this
	// point could pair a new window with the event's earlier ccfg
	// snapshot. That is benign for the same reason estimate quality is
	// benign: the budget only ever ADVANCES a cut whose invariants hold
	// regardless.
	window, windowSource := effectiveContextWindow(cfg)
	// Budget disabled when no window is resolvable OR TargetFraction is
	// the >= 1.0 "no constraint" sentinel OR the config was built
	// without ApplyDefaults (zero fraction) — the 2/3 rule alone governs.
	budgetTokens := 0.0
	if window > 0 && ccfg.TargetFraction > 0 && ccfg.TargetFraction < 1.0 {
		budgetTokens = float64(window) * ccfg.TargetFraction
	}
	summaryTokensAssumed := defaultSummaryTokensAssumed
	if ccfg.MaxSummaryTokens > 0 {
		summaryTokensAssumed = ccfg.MaxSummaryTokens
	}
	baseCut := cut
	cut = advanceCutForTokenBudget(chatMsgs, turns, cut, budgetTokens, perMsg, summaryTokensAssumed)
	projectionTokens := int(projectPostCompactionTokens(chatMsgs, turns, cut, perMsg, summaryTokensAssumed))

	// Pre-compaction context size for notices ("was ~X") and the
	// completion log: the last turn's REAL prompt tokens when a usage
	// row exists, else the chars/4 estimate over the live rows.
	contextBefore := int(perMsg * float64(len(dbMsgs)))
	contextBeforeBasis := estBasis
	if lastUsage != nil && lastUsage.PromptTokens > 0 {
		contextBefore = lastUsage.PromptTokens
		contextBeforeBasis = "usage"
	}

	trigger := inputs.Trigger
	if trigger == "" {
		trigger = "manual"
	}

	firstIdx := turns[1].start
	lastIdx := turns[cut].end - 1
	if firstIdx < 1 || lastIdx < firstIdx || lastIdx >= len(dbMsgs) {
		return nil, ErrCompactionTooShort
	}
	firstArchivedID := dbMsgs[firstIdx].ID
	lastArchivedID := dbMsgs[lastIdx].ID
	archivedCount := lastIdx - firstIdx + 1

	originalSystemID := dbMsgs[0].ID
	preservedTail := dbMsgs[lastIdx+1:]
	// Highest live row id at snapshot time. The transaction rechecks this
	// before mutating anything (see the session-changed guard inside the
	// tx closure) to abort if AddMessage inserted rows mid-flight.
	snapshotMaxLiveID := dbMsgs[len(dbMsgs)-1].ID

	// Partition every non-system row into one of four buckets:
	//
	//   supersedeIDs:  rows whose SourceCompactionID is non-nil — they
	//                  were inserted as tail-copies by a prior compaction
	//                  and represent content already covered by an earlier
	//                  summary. Marking them archived would inflate the
	//                  user-visible archived count and clutter the history
	//                  viewer with the same content appearing multiple
	//                  times. Mark them superseded=true so they vanish
	//                  from every user-facing surface.
	//
	//                  These can appear in EITHER the archived range OR
	//                  the preserved tail (they're rows like any other
	//                  in dbMsgs); we treat them uniformly by id.
	//
	//   priorSummaryIDs: untagged rows BEFORE firstIdx. In a healthy
	//                  post-compaction session this bucket contains
	//                  exactly the prior compaction's summary row
	//                  (index 1): live history is [freshSys, summary,
	//                  tail-copies...], firstIdx = turns[1].start = 2,
	//                  and the summary row at index 1 is neither in the
	//                  archived slice nor in the preserved tail. They are
	//                  prior summary material — fed to the summarizer so
	//                  the rolling-summary chain ACCUMULATES, and
	//                  archived (not superseded) so the history viewer
	//                  keeps them as real archived records.
	//
	//                  BUG HISTORY: this loop's old comment claimed
	//                  tailRegularIDs covered "(lastIdx..end]" — but the
	//                  loop starts at i=1, so untagged rows before
	//                  firstIdx (the prior summary!) used to fall into
	//                  tailRegularIDs: archived as fresh material, never
	//                  fed to the summarizer, never re-inserted as live.
	//                  Every compaction after the first silently
	//                  destroyed the accumulated summary.
	//
	//   archivedRangeRegularIDs: rows in [firstIdx..lastIdx] without
	//                  SourceCompactionID set — fresh material that
	//                  this compaction is summarizing. Archive normally.
	//
	//   tailRegularIDs: rows in (lastIdx..end] without SourceCompactionID
	//                  set — fresh material we're keeping as live history.
	//                  Archive (so we can re-insert fresh copies) and
	//                  re-tag the new copies as tail-copies of THIS
	//                  compaction.
	//
	// archivedNonSupersededCount is what we report to the user as the
	// "real" archived count for this event; superseded rows are deliberately
	// excluded from CompactionResult.ArchivedCount. Prior summary rows are
	// excluded too, for the same reason as superseded rows: the count
	// reports genuinely-new material archived by THIS event.
	var supersedeIDs, archivedRangeRegularIDs, tailRegularIDs, priorSummaryIDs []int64
	var priorSummaryMsgs []ChatMessage
	for i := 1; i < len(dbMsgs); i++ {
		m := dbMsgs[i]
		if m.SourceCompactionID != nil {
			supersedeIDs = append(supersedeIDs, m.ID)
			continue
		}
		if i < firstIdx {
			priorSummaryIDs = append(priorSummaryIDs, m.ID)
			priorSummaryMsgs = append(priorSummaryMsgs, chatMsgs[i])
			continue
		}
		if i <= lastIdx {
			archivedRangeRegularIDs = append(archivedRangeRegularIDs, m.ID)
		} else {
			tailRegularIDs = append(tailRegularIDs, m.ID)
		}
	}
	archivedNonSupersededCount := len(archivedRangeRegularIDs)

	// Degenerate-case refusal: when the 2/3 cut lands entirely on
	// tail-copies from the prior compaction, archivedNonSupersededCount
	// is 0 and the event is pure churn — it would re-summarize
	// already-summarized tail content, burning a summarizer call and
	// degrading the rolling summary's quality for zero benefit. Checked
	// here (after the partition, before the summarizer) so no API cost
	// is wasted. The first compaction on a session can never hit this:
	// no rows carry SourceCompactionID yet, so the archived range always
	// contains at least one regular row.
	if archivedNonSupersededCount == 0 {
		logger.Info("refusing compaction: no new messages since the last compaction",
			"session", inputs.SessionID,
			"superseded", len(supersedeIDs),
			"prior_summaries", len(priorSummaryIDs))
		return nil, ErrCompactionNothingNew
	}

	// Full-information trigger log — everything the sizing decision had
	// available, emitted before the summarizer call spends anything. This
	// is the "why did it compact like THIS" record: trigger, scale
	// (live messages/turns), both cuts, the budget math and its inputs
	// (window source, per-message estimate basis, assumed summary size),
	// dave's own neutral token count of the next-request payload
	// (our_token_count/our_encoding/our_exact/image_parts — diff against
	// last_prompt_tokens to attribute provider accounting divergence),
	// and the post-compaction projection evaluated at the FINAL cut (the
	// value the advancement loop optimized against; compare it with
	// budget_tokens for the achieved margin, and with the completion
	// log's actual summary_tokens for assumed-vs-actual drift).
	triggerLogKV := []interface{}{
		"trigger", trigger,
		"session", inputs.SessionID,
		"network", inputs.Network.Name,
		"channel", inputs.Channel,
		"nick", inputs.UserNick,
		"service", cfg.Service,
		"model", cfg.Model,
		"live_messages", len(dbMsgs),
		"live_turns", len(turns) - 1,
		"min_turns", ccfg.MinTurns,
		"base_cut", baseCut,
		"final_cut", cut,
		"tail_turns", len(turns) - 1 - cut,
		"archived_range_first", firstArchivedID,
		"archived_range_last", lastArchivedID,
		"context_window", window,
		"window_source", windowSource,
		"target_fraction", ccfg.TargetFraction,
		"budget_tokens", int(budgetTokens),
		"tokens_per_message", perMsg,
		"estimate_basis", estBasis,
		"our_token_count", ourCount.Tokens,
		"our_encoding", ourCount.Encoding,
		"our_exact", ourCount.Exact,
		"image_parts", ourCount.ImageParts,
		"summary_tokens_assumed", summaryTokensAssumed,
		"projection_tokens", projectionTokens,
		"max_summary_tokens", ccfg.MaxSummaryTokens,
	}
	if lastUsage != nil {
		// Last chat turn's own usage — the real number that tripped (or
		// would trip) the auto threshold. Omitted cleanly when the
		// session has no usage rows yet.
		triggerLogKV = append(triggerLogKV,
			"last_prompt_tokens", lastUsage.PromptTokens,
			"last_completion_tokens", lastUsage.CompletionTokens,
			"last_cached_tokens", lastUsage.CachedTokens,
			"last_reasoning_tokens", lastUsage.ReasoningTokens)
	}
	logger.Info("compaction triggered", triggerLogKV...)

	// Summarizer input: prior summary material FIRST, then the newly
	// archived range. The summary rows are RoleSystem rows whose content
	// already begins with "[CONVERSATION SUMMARY — covers N earlier
	// messages (#X–#Y)]", and callSummarizer's transcript builder renders
	// RoleSystem rows with a "[system] " role tag, so the transcript is
	// self-describing; the default prompt tells the model to carry a
	// leading prior summary forward and merge it. Built as an explicit
	// new slice to avoid aliasing surprises with chatMsgs sub-slices.
	summarizerInput := make([]ChatMessage, 0, len(priorSummaryMsgs)+int(lastIdx-firstIdx+1))
	summarizerInput = append(summarizerInput, priorSummaryMsgs...)
	summarizerInput = append(summarizerInput, chatMsgs[firstIdx:lastIdx+1]...)
	archivedSlice := stripImagesForSummary(summarizerInput)

	// Reuses the single ccfg snapshot from the top of CompactSession
	// (see the comment there) rather than re-reading config.Compaction.
	prompt := defaultCompactionPrompt
	if ccfg.PromptTemplate != "" {
		prompt = ccfg.PromptTemplate
	}

	summary, usage, durationMs, err := callSummarizer(ctx, cfg, prompt, archivedSlice, inputs.SessionID, ccfg.MaxSummaryTokens)
	if err != nil {
		return nil, fmt.Errorf("summarizer call: %w", err)
	}
	if summary == "" {
		return nil, ErrCompactionEmptyResult
	}

	freshSystem := renderFreshSystemPrompt(cfg, inputs.Network, inputs.Client, inputs.Channel, inputs.UserNick, dbMsgs[0].Content)

	summaryHeader := fmt.Sprintf(
		"[CONVERSATION SUMMARY — covers %d earlier messages (#%d–#%d)]\n\n",
		archivedCount, firstArchivedID, lastArchivedID,
	)
	summaryMessage := summaryHeader + summary

	pTok, cTok := 0, 0
	if usage != nil {
		pTok = int(usage.PromptTokens)
		cTok = int(usage.CompletionTokens)
	}

	// Log tripwire only — no behavior change. See summarySizeWarnRatio.
	if ratio := summarySizeRatio(pTok, cTok); ratio >= summarySizeWarnRatio {
		logger.Warn("compaction summary unusually large",
			"session", inputs.SessionID,
			"prompt_tokens", pTok,
			"completion_tokens", cTok,
			"ratio", ratio)
	}

	// Live-history observability, computed from the same locals the
	// transaction below inserts: the fresh system row + the summary row +
	// the re-inserted preserved-tail copies. This mirrors EXACTLY what the
	// transaction makes live — the session-changed guard inside the tx
	// aborts if any other row arrived mid-flight, so on success there is
	// nothing else in live history. Prior-summary rows are excluded
	// because the tx archives them (they are folded into the new summary
	// instead of surviving as live rows).
	//
	// Token basis: countMessageTokens over those rows when a model is
	// configured (real tokenizer, or the o200k_base approximation for
	// unknown models — see tokencount.go), because the owner compares
	// these numbers against provider-reported usage; estimateTokens
	// (runes/4 over Content only) remains the no-model fallback.
	postLiveChat := make([]ChatMessage, 0, 2+len(preservedTail))
	postLiveChat = append(postLiveChat,
		ChatMessage{Role: RoleSystem, Content: freshSystem},
		ChatMessage{Role: RoleSystem, Content: summaryMessage},
	)
	for i := range preservedTail {
		postLiveChat = append(postLiveChat, messageFromDB(preservedTail[i]))
	}
	// Computed once, used twice: as the completion log's our_token_count
	// (dave's neutral count of the post-compaction payload — pairs with
	// the trigger log's pre-compaction count to bracket the event) and,
	// when a model is configured, as LiveTokensEst.
	postLiveCount := countMessageTokens(cfg.Model, postLiveChat)
	var liveTokensEst int
	if cfg.Model != "" {
		liveTokensEst = postLiveCount.Tokens
	} else {
		liveTokensEst = estimateTokens(freshSystem) + estimateTokens(summaryMessage)
		for i := range preservedTail {
			liveTokensEst += estimateTokens(preservedTail[i].Content)
		}
	}
	liveMessages := 2 + len(preservedTail)

	var result CompactionResult
	err = sm.db.Transaction(func(tx *gorm.DB) error {
		// DESIGN NOTE — optimistic session-changed guard. This must be the
		// FIRST statement in the closure (before the compaction row Create)
		// so nothing is mutated when we abort. Between the snapshot above
		// and this transaction, seconds may have elapsed inside
		// callSummarizer with zero coordination with AddMessage (the
		// per-session compactionMu only serializes compaction-vs-
		// compaction; maybeAutoCompact fires exactly when the user's next
		// message is most likely to arrive). A row inserted mid-flight
		// gets an autoincrement id above the snapshot's max but BELOW the
		// fresh system/summary/tail-copy rows this transaction is about to
		// insert — scrambling the live ORDER BY id ASC stream into
		// [user, assistant, system, summary, tail...]. From there,
		// TruncateHistory silently drops the system prompt and the next
		// compaction's dbMsgs[0]-is-system assumption breaks. Compaction-
		// vs-compaction is already excluded by the TryLock, so only
		// concurrent AddMessage traffic can trip this guard — and once new
		// messages exist the summarizer result is stale anyway. Aborting
		// converts the silent corruption into a clean retry.
		var maxLiveID int64
		if err := tx.Model(&Message{}).
			Select("COALESCE(MAX(id), 0)").
			Where("session_id = ? AND archived = ?", inputs.SessionID, false).
			Scan(&maxLiveID).Error; err != nil {
			return fmt.Errorf("session-changed guard: %w", err)
		}
		if maxLiveID > snapshotMaxLiveID {
			return ErrCompactionSessionChanged
		}

		// Archive ordering matters: archive originals first so the only
		// non-archived rows for this session at the moment of new-row
		// insertion are the tail rows we are about to re-insert. Then
		// the new rows (fresh system + summary + tail copies) end up
		// ordered correctly by autoincrement id ASC.

		// 1. Create the compaction row up front (we'll fix
		//    SummaryMessageID after we insert the summary).
		comp := Compaction{
			SessionID:        inputs.SessionID,
			SummaryMessageID: 0,
			FirstArchivedID:  firstArchivedID,
			LastArchivedID:   lastArchivedID,
			ArchivedCount:    archivedCount,
			Service:          cfg.Service,
			Model:            cfg.Model,
			PromptTokens:     pTok,
			CompletionTokens: cTok,
			DurationMs:       durationMs,
			Trigger:          trigger,
		}
		if err := tx.Create(&comp).Error; err != nil {
			return fmt.Errorf("insert compaction: %w", err)
		}

		// 2. Archive original system message + the contiguous archived
		//    range. Within that range, rows tagged with SourceCompactionID
		//    (tail-copies inserted by a prior compaction) are marked
		//    superseded=true rather than counted as fresh archived
		//    material; their content is already covered by an earlier
		//    summary. Same for any tail-copies that happen to fall in
		//    the preserved tail. The genuinely-new rows in the preserved
		//    tail are archived normally so we can re-insert them as
		//    fresh tail-copies of THIS compaction.
		if err := archiveMessageByID(tx, originalSystemID, comp.ID); err != nil {
			return fmt.Errorf("archive original system: %w", err)
		}
		if len(archivedRangeRegularIDs) > 0 {
			if err := tx.Model(&Message{}).
				Where("id IN ?", archivedRangeRegularIDs).
				Updates(map[string]interface{}{"archived": true, "compaction_id": comp.ID}).Error; err != nil {
				return fmt.Errorf("archive range (regular): %w", err)
			}
		}
		// Prior summary rows archive like any other real archived material
		// (same UPDATE shape as above; superseded stays false): unlike
		// tail-copy ghosts they are the ONLY record of what earlier
		// compactions folded away, so the history viewer must keep showing
		// them. Their content lives on in the new summary produced by this
		// compaction (fed through the summarizer above).
		if len(priorSummaryIDs) > 0 {
			if err := tx.Model(&Message{}).
				Where("id IN ?", priorSummaryIDs).
				Updates(map[string]interface{}{"archived": true, "compaction_id": comp.ID}).Error; err != nil {
				return fmt.Errorf("archive prior summaries: %w", err)
			}
		}
		if len(tailRegularIDs) > 0 {
			if err := tx.Model(&Message{}).
				Where("id IN ?", tailRegularIDs).
				Updates(map[string]interface{}{"archived": true, "compaction_id": comp.ID}).Error; err != nil {
				return fmt.Errorf("archive tail (regular): %w", err)
			}
		}
		if err := markMessagesSupersededByIDs(tx, supersedeIDs, comp.ID); err != nil {
			return fmt.Errorf("supersede prior tail copies: %w", err)
		}

		// 3. Insert fresh system row first so it gets the smallest new id.
		freshSysRow := Message{
			SessionID: inputs.SessionID,
			Role:      RoleSystem,
			Content:   freshSystem,
		}
		if err := tx.Create(&freshSysRow).Error; err != nil {
			return fmt.Errorf("insert fresh system: %w", err)
		}

		// 4. Insert the summary RoleSystem row.
		summaryRow := Message{
			SessionID: inputs.SessionID,
			Role:      RoleSystem,
			Content:   summaryMessage,
		}
		if err := tx.Create(&summaryRow).Error; err != nil {
			return fmt.Errorf("insert summary: %w", err)
		}

		// 5. Re-insert preserved tail messages as fresh rows so they end
		//    up with ids strictly greater than the summary row, making
		//    the live history (ORDER BY id ASC, archived=false) come
		//    out in the correct logical order:
		//        [fresh system] → [summary] → [preserved tail]
		//    Each tail copy is tagged with SourceCompactionID = comp.ID
		//    so the NEXT compaction can identify it as a duplicate of
		//    already-summarized content and supersede it instead of
		//    archiving it as fresh material. See the partitioning logic
		//    above and the design note in AGENTS.md.
		compIDForTag := comp.ID
		for _, orig := range preservedTail {
			newRow := Message{
				SessionID:          inputs.SessionID,
				Role:               orig.Role,
				Content:            orig.Content,
				ToolCalls:          orig.ToolCalls,
				ToolCallID:         orig.ToolCallID,
				ReasoningContent:   orig.ReasoningContent,
				MultiContent:       orig.MultiContent,
				IsAsyncResult:      orig.IsAsyncResult,
				SourceCompactionID: &compIDForTag,
			}
			if err := tx.Create(&newRow).Error; err != nil {
				return fmt.Errorf("re-insert tail: %w", err)
			}
		}

		// 6. Now patch the compaction row with the summary message id.
		if err := tx.Model(&Compaction{}).Where("id = ?", comp.ID).
			Update("summary_message_id", summaryRow.ID).Error; err != nil {
			return fmt.Errorf("update compaction summary id: %w", err)
		}

		// 7. Reset Responses API chain — see comments at responses.go and
		//    aiCmds.go's recovery path: previous_response_id refers to a
		//    server-side history that no longer matches our compacted
		//    local history, so we must drop it and resend full history on
		//    the next turn. response_model goes with it (the pair is
		//    always written/cleared together).
		if err := tx.Model(&Session{}).Where("id = ?", inputs.SessionID).
			Updates(map[string]interface{}{"response_id": nil, "response_model": nil}).Error; err != nil {
			return fmt.Errorf("reset response_id: %w", err)
		}

		result = CompactionResult{
			CompactionID: comp.ID,
			// ArchivedCount reports the user-meaningful archived count:
			// genuinely-new rows that this compaction archived. We
			// deliberately exclude superseded tail-copies from a prior
			// compaction so the user-facing notice doesn't claim to
			// have summarized content that was already summarized.
			ArchivedCount:    archivedNonSupersededCount,
			FirstArchivedID:  firstArchivedID,
			LastArchivedID:   lastArchivedID,
			SummaryMessageID: summaryRow.ID,
			PromptTokens:     pTok,
			CompletionTokens: cTok,
			DurationMs:       durationMs,
			// Observability mirrors (see the computation above and the
			// field docs on CompactionResult).
			SupersededCount:   len(supersedeIDs),
			PriorSummaryCount: len(priorSummaryIDs),
			LiveMessages:      liveMessages,
			LiveTokensEst:     liveTokensEst,
			ContextBefore:     contextBefore,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Completion log — assumed-vs-actual drift is one diff away:
	// summary_tokens_assumed (what the cut advancement budgeted against)
	// vs summary_tokens (what the summarizer actually produced), and
	// projection_tokens (the pre-call estimate) vs live_tokens_est (the
	// post-call reality over the inserted rows). our_token_count is
	// dave's own neutral count of exactly the live rows the transaction
	// inserted (same basis as the trigger log's pre-compaction
	// our_token_count — the pair brackets the compaction's effect).
	completionLogKV := []interface{}{
		"session", inputs.SessionID,
		"compaction", result.CompactionID,
		"archived", result.ArchivedCount,
		"prompt_tokens", result.PromptTokens,
		"completion_tokens", result.CompletionTokens,
		"duration_ms", result.DurationMs,
		"summary_tokens", result.CompletionTokens,
		"summary_tokens_assumed", summaryTokensAssumed,
		"projection_tokens", projectionTokens,
		"our_token_count", postLiveCount.Tokens,
		"superseded", result.SupersededCount,
		"prior_summaries", result.PriorSummaryCount,
		"live_messages", result.LiveMessages,
		"live_tokens_est", result.LiveTokensEst,
		"context_before_est", contextBefore,
		"context_before_basis", contextBeforeBasis,
		"trigger", trigger,
	}
	if budgetTokens > 0 {
		completionLogKV = append(completionLogKV,
			"budget_tokens", int(budgetTokens),
			"target_met", float64(result.LiveTokensEst) <= budgetTokens)
	}
	logger.Info("compacted session", completionLogKV...)
	return &result, nil
}

// ShouldAutoCompact decides whether an automatic compaction is warranted
// after the most recent turn. Returns false when the feature is disabled,
// the most recent usage is unknown, or no context-window value is available.
//
// Context-window cascade (service-first): effectiveContextWindow resolves
// the session's service ([services.<name>] context_window) first, then the
// global [compaction] context_window fallback. When neither is set,
// auto-compaction stays off — one global number is wrong for every model
// but one when services mix context sizes (e.g. 8k local next to 200k
// cloud). The flags snapshot and the window snapshot are separate
// readConfig calls (the window resolver is shared with CompactSession's
// token-aware sizing); flags short-circuit FIRST so a disabled feature
// never depends on the window lookup at all.
func (sm *SessionManager) ShouldAutoCompact(sessionID int64, cfg AIConfig) bool {
	var ccfg CompactionConfig
	readConfig(func() { ccfg = config.Compaction })
	if !ccfg.Enabled || !ccfg.AutoEnabled {
		return false
	}
	last, err := getLastTurnUsageForSession(sessionID)
	if err != nil || last == nil {
		return false
	}
	if last.PromptTokens <= 0 {
		return false
	}
	contextWindow, _ := effectiveContextWindow(cfg)
	if contextWindow <= 0 {
		return false
	}
	threshold := float64(contextWindow) * ccfg.AutoThreshold
	return float64(last.PromptTokens) >= threshold
}

// compactionNoticeVars builds the placeholder map for a compaction notice.
//
// Number provenance — two DIFFERENT API calls are represented and must not
// be conflated:
//
//   - {count}, {tokens_in}, {tokens_out}, {summary_tokens}, {superseded},
//     {prior_summaries}, {live_messages}, {live_tokens_est},
//     {context_before}, {context_after}, {duration}: all come from THIS
//     compaction — its own summarizer call's usage and its
//     CompactionResult. {tokens_out} and {summary_tokens} are the same
//     number (the alias exists because "summary_tokens" reads clearer in
//     templates). {context_after} aliases {live_tokens_est} (post-
//     compaction live-history estimate); {context_before} is the
//     pre-compaction size carried on CompactionResult.ContextBefore —
//     the last chat turn's REAL prompt tokens when a usage row exists,
//     else a chars/4 estimate over the live rows.
//
//   - {prompt}, {completion}, {total}, {cached}, {reasoning}: the LAST
//     recorded TurnUsage for the session — i.e. the most recent CHAT API
//     call's usage (the one that tripped auto-compaction), which is a
//     different call entirely from the summarizer. TurnUsage rows are
//     written per API call, so on multi-call tool-loop turns {total} is
//     only the final call of that turn. These placeholders are kept
//     filled for backward compatibility with custom templates; the
//     DEFAULT templates no longer use them.
//
// Every placeholder always has a numeric value ("0" fallbacks) — they can
// never render empty.
func compactionNoticeVars(res *CompactionResult, sessionID int64) map[string]string {
	vars := map[string]string{
		"count":           fmt.Sprintf("%d", res.ArchivedCount),
		"tokens_in":       fmt.Sprintf("%d", res.PromptTokens),
		"tokens_out":      fmt.Sprintf("%d", res.CompletionTokens),
		"summary_tokens":  fmt.Sprintf("%d", res.CompletionTokens),
		"superseded":      fmt.Sprintf("%d", res.SupersededCount),
		"prior_summaries": fmt.Sprintf("%d", res.PriorSummaryCount),
		"live_messages":   fmt.Sprintf("%d", res.LiveMessages),
		"live_tokens_est": fmt.Sprintf("%d", res.LiveTokensEst),
		"context_before":  fmt.Sprintf("%d", res.ContextBefore),
		"context_after":   fmt.Sprintf("%d", res.LiveTokensEst),
		"duration":        fmt.Sprintf("%d", res.DurationMs),
		"prompt":          "0",
		"completion":      "0",
		"total":           "0",
		"cached":          "0",
		"reasoning":       "0",
	}
	if theDB != nil {
		if tu, err := getLastTurnUsageForSession(sessionID); err == nil && tu != nil {
			vars["prompt"] = fmt.Sprintf("%d", tu.PromptTokens)
			vars["completion"] = fmt.Sprintf("%d", tu.CompletionTokens)
			vars["total"] = fmt.Sprintf("%d", tu.PromptTokens+tu.CompletionTokens)
			vars["cached"] = fmt.Sprintf("%d", tu.CachedTokens)
			vars["reasoning"] = fmt.Sprintf("%d", tu.ReasoningTokens)
		}
	}
	return vars
}

// maybeAutoCompact is invoked at the end of a successful chat() turn. If the
// most recent turn's prompt token count crossed the configured threshold,
// it spawns a goroutine that compacts the session in the background using
// the same chat command's config. Successful compactions emit a notice via
// the IRC client; failures are logged only (we do not spam the channel).
//
// Runs in its own goroutine so it never delays the user's reply. Best-effort
// — if the bot disconnects, the session is closed, or another compaction is
// already running, we silently skip.
func maybeAutoCompact(runner *chatRunner, cfg AIConfig, network Network, c *girc.Client, channel, userNick string) {
	if runner == nil || runner.sessionID == 0 {
		return
	}
	if !sessionMgr.ShouldAutoCompact(runner.sessionID, cfg) {
		return
	}
	sessionID := runner.sessionID
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		res, err := sessionMgr.CompactSession(ctx, CompactSessionInputs{
			SessionID: sessionID,
			Network:   network,
			Channel:   channel,
			UserNick:  userNick,
			Client:    c,
			Trigger:   "auto",
		}, cfg)
		logger := newLogger("compaction.auto")
		if err != nil {
			if errors.Is(err, ErrCompactionSessionChanged) {
				// Expected under load: the user sent another message while
				// the summarizer was in flight and the transaction guard
				// aborted. Nothing is wrong — INFO, not WARN. The next
				// threshold crossing (or a manual `compact`) retries.
				logger.Info("auto-compaction aborted: session changed during compaction",
					"session", sessionID)
				return
			}
			// ErrCompactionNoSystemRow lands here on purpose: it signals an
			// already-scrambled session, which IS unexpected and deserves a
			// WARN in the admin log.
			//
			// ErrCompactionNothingNew is also expected and silent: the
			// threshold tripped on the chat turn's prompt tokens (which
			// include the summary + tail), but no genuinely-new material
			// exists to compact yet. The next crossing retries.
			if !errors.Is(err, ErrCompactionInProgress) && !errors.Is(err, ErrCompactionTooShort) && !errors.Is(err, ErrCompactionNothingNew) {
				logger.Warn("auto-compaction failed", "session", sessionID, "error", err)
			}
			return
		}
		if c == nil || !c.IsConnected() {
			return
		}
		n := getNotices()
		msg := expandNotice(n.Compaction.AutoNotice, compactionNoticeVars(res, sessionID))
		if msg != "" {
			c.Cmd.Message(channel, msg)
		}
	}()
}
