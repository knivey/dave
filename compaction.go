package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
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
	// ContextWindow is the fallback token limit when the session's service
	// does not specify one. 0 disables auto-compaction in that case.
	ContextWindow int `toml:"context_window"`
	// MinTurns: the minimum number of turns (user → assistant pairs) the
	// session must contain before any compaction will run. Below this, the
	// compactor refuses to act because there's not enough material to
	// summarize meaningfully.
	MinTurns int `toml:"min_turns"`
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

Do NOT invent details. If something was unclear, say so. Output only the summary text — no preamble, no headers, no markdown formatting.`

// Sentinels surfaced to user-facing layers via notice templates.
var (
	ErrCompactionDisabled    = errors.New("compaction disabled")
	ErrCompactionTooShort    = errors.New("not enough history to compact")
	ErrCompactionNoActive    = errors.New("no active session")
	ErrCompactionInProgress  = errors.New("compaction already in progress")
	ErrCompactionEmptyResult = errors.New("summarizer returned empty content")
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
	target := (totalNonSystem * 2) / 3
	if target < 1 {
		target = 1
	}
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
	var templateVars map[string]string
	readConfig(func() {
		templateVars = make(map[string]string, len(config.TemplateVars))
		for k, v := range config.TemplateVars {
			templateVars[k] = v
		}
	})
	data := SystemPromptData{
		Nick:    userNick,
		BotNick: network.Nick,
		Channel: channel,
		Network: network.Name,
		Date:    time.Now().Format("2006-01-02"),
		Vars:    templateVars,
	}
	if client != nil {
		data.BotNick = client.GetNick()
		ch := client.LookupChannel(channel)
		var nicks []string
		if ch != nil {
			for _, u := range ch.Users(client) {
				nicks = append(nicks, u.Nick)
			}
			sort.Strings(nicks)
		}
		data.ChanNicks = `["` + strings.Join(nicks, `","`) + `"]`
	}
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
func callSummarizer(ctx context.Context, cfg AIConfig, summarizerSys string, archived []ChatMessage) (string, *Usage, int, error) {
	var svc Service
	readConfig(func() { svc = config.Services[cfg.Service] })

	transport := newDaveTransport(nil, nil)
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
	summarizerCfg.MCPs = nil

	msgs := []ChatMessage{
		{Role: RoleSystem, Content: summarizerSys},
		{Role: RoleUser, Content: b.String()},
	}

	apiCtx, cancel := context.WithTimeout(ctx, summarizerCfg.Timeout)
	defer cancel()
	params := buildChatCompletionParams(summarizerCfg, msgs, nil, "")
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
	logger := logxi.New("compaction")
	logger.SetLevel(logxi.LevelAll)

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

	chatMsgs := make([]ChatMessage, len(dbMsgs))
	for i, dm := range dbMsgs {
		chatMsgs[i] = messageFromDB(dm)
	}

	turns := buildTurns(chatMsgs)

	var ccfg CompactionConfig
	readConfig(func() { ccfg = config.Compaction })
	if len(turns)-1 < ccfg.MinTurns {
		return nil, ErrCompactionTooShort
	}

	cut := pickCompactionCutTurn(chatMsgs, turns)
	if cut < 1 {
		return nil, ErrCompactionTooShort
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
	archivedSlice := stripImagesForSummary(chatMsgs[firstIdx : lastIdx+1])
	preservedTail := dbMsgs[lastIdx+1:]

	// Partition every non-system row into one of three buckets:
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
	// excluded from CompactionResult.ArchivedCount.
	var supersedeIDs, archivedRangeRegularIDs, tailRegularIDs []int64
	for i := 1; i < len(dbMsgs); i++ {
		m := dbMsgs[i]
		inArchivedRange := i >= firstIdx && i <= lastIdx
		if m.SourceCompactionID != nil {
			supersedeIDs = append(supersedeIDs, m.ID)
			continue
		}
		if inArchivedRange {
			archivedRangeRegularIDs = append(archivedRangeRegularIDs, m.ID)
		} else {
			tailRegularIDs = append(tailRegularIDs, m.ID)
		}
	}
	archivedNonSupersededCount := len(archivedRangeRegularIDs)

	prompt := defaultCompactionPrompt
	var compactionCfg CompactionConfig
	readConfig(func() { compactionCfg = config.Compaction })
	if compactionCfg.PromptTemplate != "" {
		prompt = compactionCfg.PromptTemplate
	}

	summary, usage, durationMs, err := callSummarizer(ctx, cfg, prompt, archivedSlice)
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

	trigger := inputs.Trigger
	if trigger == "" {
		trigger = "manual"
	}

	var result CompactionResult
	err = sm.db.Transaction(func(tx *gorm.DB) error {
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
			SessionID:  inputs.SessionID,
			Role:       RoleSystem,
			Content:    freshSystem,
			SettingsID: session.SettingsID,
		}
		if err := tx.Create(&freshSysRow).Error; err != nil {
			return fmt.Errorf("insert fresh system: %w", err)
		}

		// 4. Insert the summary RoleSystem row.
		summaryRow := Message{
			SessionID:  inputs.SessionID,
			Role:       RoleSystem,
			Content:    summaryMessage,
			SettingsID: session.SettingsID,
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
				SettingsID:         orig.SettingsID,
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
		//    the next turn.
		if err := tx.Model(&Session{}).Where("id = ?", inputs.SessionID).
			Update("response_id", nil).Error; err != nil {
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
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	logger.Info("compacted session",
		"session", inputs.SessionID,
		"compaction", result.CompactionID,
		"archived", result.ArchivedCount,
		"prompt_tokens", result.PromptTokens,
		"completion_tokens", result.CompletionTokens,
		"duration_ms", result.DurationMs,
		"trigger", trigger,
	)
	return &result, nil
}

// ShouldAutoCompact decides whether an automatic compaction is warranted
// after the most recent turn. Returns false when the feature is disabled,
// the most recent usage is unknown, or no context-window value is available.
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
	contextWindow := ccfg.ContextWindow
	if contextWindow <= 0 {
		return false
	}
	threshold := float64(contextWindow) * ccfg.AutoThreshold
	return float64(last.PromptTokens) >= threshold
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
		logger := logxi.New("compaction.auto")
		logger.SetLevel(logxi.LevelAll)
		if err != nil {
			if !errors.Is(err, ErrCompactionInProgress) && !errors.Is(err, ErrCompactionTooShort) {
				logger.Warn("auto-compaction failed", "session", sessionID, "error", err)
			}
			return
		}
		if c == nil || !c.IsConnected() {
			return
		}
		n := getNotices()
		msg := expandNotice(n.Compaction.AutoNotice, map[string]string{
			"count":      fmt.Sprintf("%d", res.ArchivedCount),
			"tokens_in":  fmt.Sprintf("%d", res.PromptTokens),
			"tokens_out": fmt.Sprintf("%d", res.CompletionTokens),
			"duration":   fmt.Sprintf("%d", res.DurationMs),
		})
		if msg != "" {
			c.Cmd.Message(channel, msg)
		}
	}()
}
