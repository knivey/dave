package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lrstanley/girc"
)

// fetchChannelLogFn is the injection point for tests (same pattern as
// connectMCPServerImpl).
var fetchChannelLogFn = fetchChannelLog

const queryChannelLogsToolName = "query_channel_logs"

const respondToolName = "respond"

// generatorLogQuery is the per-run log-retrieval context a log-fed
// generator carries on its runner: the defaulted spec plus the raw and
// normalized channel names the SQL match needs (setChannel keeps only
// the normalized one) and the generator name for logs.
type generatorLogQuery struct {
	spec       LogQuerySpec
	channelRaw string
	channel    string
	name       string
}

// generatorLogToolDef builds the per-run tool definition. The default
// window is embedded in the description so the model knows what it gets
// when it omits the argument. DESIGN NOTE: this is NOT a static
// builtinTools entry — the definition is per-run (default window) and
// the handler needs the runner's generator context.
func generatorLogToolDef(lq *generatorLogQuery) Tool {
	return Tool{
		Type: "function",
		Function: &FunctionDefinition{
			Name: queryChannelLogsToolName,
			Description: fmt.Sprintf(
				"Retrieve the transcript of this IRC channel's recent activity. Returns timestamped lines (newest last) with a header giving the covered time range and line/token counts; a keep-newest token budget is applied and any truncation is disclosed in the result. Call this before summarizing or referencing channel history. window: how far back to look — a duration string of <number><unit> groups with units s, m, h, d (e.g. \"90m\", \"12h\", \"7d\", \"1d12h\"). Default: %s.",
				lq.spec.Window),
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"window": map[string]any{
						"type":        "string",
						"description": "How far back to retrieve (e.g. \"12h\", \"7d\"). Defaults to the configured window.",
					},
				},
				"required": []string{},
			},
		},
	}
}

type generatorToolEntry struct {
	handler func(cr *chatRunner, turn *turnContext, call ToolCall)
}

// generatorTools are the ephemeral-turn-only tools generators offer
// (query_channel_logs for log-fed commands; respond for respond_tool
// generators). Deliberately separate from the global builtinTools map:
// definitions are per-run and handlers need the runner's generator
// context. Both honor disabled_builtin_tools and hidden_tools exactly
// like builtins.
var generatorTools = map[string]generatorToolEntry{
	queryChannelLogsToolName: {handler: handleGeneratorLogQuery},
	respondToolName:          {handler: handleGeneratorRespond},
}

// handleGeneratorLogQuery executes query_channel_logs: resolves the
// window (configured default when omitted), fetches via the injection
// seam, and returns the transcript as the tool result — every failure
// mode is tool-result content the model can act on (self-correct or
// relay), never a user-facing notice.
func handleGeneratorLogQuery(cr *chatRunner, turn *turnContext, call ToolCall) {
	if cr.logQuery == nil || !cr.ephemeral {
		// Offered only on ephemeral log-fed turns; a hallucinated call
		// from any other turn gets an error result, not a panic.
		turn.Add(toolResultMsg(call.ID, "error: query_channel_logs is not available on this command"))
		return
	}
	var args struct {
		Window string `json:"window"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		turn.Add(toolResultMsg(call.ID, "error: failed to parse tool arguments: "+err.Error()))
		return
	}
	spec := cr.logQuery.spec
	// Defense in depth: production specs are defaulted at config load
	// and again by generator(); hand-built contexts (tests) may not be.
	applyLogQueryDefaults(&spec)
	if args.Window != "" {
		// The token IS the duration string (same grammar as the config
		// field); windowDuration below rejects anything else with a
		// clear message.
		spec.Window = args.Window
	}
	window, werr := spec.windowDuration()
	if werr != nil {
		turn.Add(toolResultMsg(call.ID, "error: "+werr.Error()))
		return
	}
	lw, err := fetchChannelLogFn(spec, cr.network.Name, cr.logQuery.channelRaw, cr.logQuery.channel, cr.cfg.Model, time.Now())
	if err != nil {
		if errors.Is(err, errLogWindowTooLarge) {
			turn.Add(toolResultMsg(call.ID, fmt.Sprintf(
				"error: that window exceeds the row cap (%d rows) — request a narrower window", logQueryRowCap)))
			return
		}
		cr.logger.Error("generator log query failed", "trigger", cr.logQuery.name, "error", err)
		turn.Add(toolResultMsg(call.ID, "error: "+err.Error()))
		return
	}
	// Coverage carries the kept rows' actual time range; BOTH endpoints
	// must be set or it stays empty (a hand-built result with only one
	// would render "14:32 to 00:00") — same rule the v1 notice used.
	coverage := ""
	if !lw.FirstKept.IsZero() && !lw.LastKept.IsZero() {
		coverage = fmt.Sprintf(", covering %s to %s", lw.FirstKept.Format("15:04"), lw.LastKept.Format("15:04"))
	}
	cr.logger.Info("generator log query",
		"trigger", cr.logQuery.name, "window", spec.Window, "coverage", coverage,
		"files", len(lw.Files), "lines", lw.TotalLines, "dropped", lw.DroppedLines,
		"tokens", lw.Tokens, "budget", spec.MaxTokens, "truncated", lw.Truncated)
	if lw.TotalLines == 0 {
		turn.Add(toolResultMsg(call.ID, fmt.Sprintf("No channel activity found in the last %s.", formatDuration(window))))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Channel activity for %s on %s, last %s (%d lines, %d tokens%s):\n",
		cr.logQuery.channel, cr.network.Name, formatDuration(window), lw.TotalLines, lw.Tokens, coverage)
	b.WriteString(strings.Join(lw.Lines, "\n"))
	turn.Add(toolResultMsg(call.ID, b.String()))
}

// generatorRespondToolDef builds the respond tool definition. Its text
// argument is the model's complete final answer for the user.
func generatorRespondToolDef() Tool {
	return Tool{
		Type: "function",
		Function: &FunctionDefinition{
			Name:        respondToolName,
			Description: "Deliver your complete final answer to the user and end your turn. Use this once your task is finished; the text is rendered and sent verbatim. Do not call any other tool after this.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{
						"type":        "string",
						"description": "Your complete final answer for the user.",
					},
				},
				"required": []string{"text"},
			},
		},
	}
}

// handleGeneratorRespond delivers the model's final answer and ends
// the turn: the text goes through the normal render/pastebin plumbing,
// and the responded flag tells every runTurn variant to return right
// after executeToolCalls — no second API round-trip (a respond
// iteration carries a tool call, so it never hits the empty-response
// machinery by the existing definition: tool-call branches reset
// emptyRetries). When respond rides alongside other tool calls in one
// iteration, calls execute in order and the rest still run (results
// logged; the turn is ephemeral anyway) — the flag simply ends the
// loop after the batch.
func handleGeneratorRespond(cr *chatRunner, turn *turnContext, call ToolCall) {
	if !cr.ephemeral {
		// Offered only on ephemeral turns; a hallucinated call from a
		// chat turn gets an error result.
		turn.Add(toolResultMsg(call.ID, "error: respond is not available on this command"))
		return
	}
	var args struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		turn.Add(toolResultMsg(call.ID, "error: failed to parse tool arguments: "+err.Error()))
		return
	}
	text := strings.TrimSpace(args.Text)
	if text == "" {
		turn.Add(toolResultMsg(call.ID, "error: respond requires a non-empty \"text\" argument — provide your complete final answer"))
		return
	}
	cr.sendRendered(text)
	cr.responded = true
	cr.logger.Info("generator respond delivered", "chars", len(text))
	turn.Add(toolResultMsg(call.ID, "delivered"))
}

// generator is the executor for one-shot generator commands (spec
// docs/superpowers/specs/2026-10-09-generators-tool-driven-design.md):
// chat() minus persistence. It renders the system prompt, builds one
// user message from the verbatim args, and runs the UNCHANGED runTurn
// agentic loop on an ephemeral turn — streaming, tools, empty-response
// retry all apply. Log-fed generators expose the query_channel_logs
// tool (the model chooses its windows; [name.log] configures defaults
// and caps). No session row is created; usage rows are attributed with
// SessionID 0; the run's API traffic logs to its own api-log file
// under a unique negative id.
func generator(network Network, c *girc.Client, e girc.Event, cfg GeneratorConfig, ctx context.Context, output chan<- string, resolvedUser *User, args ...string) {
	runner := newChatRunnerFn(network, c, cfg.AIConfig, ctx, output).(*chatRunner)
	nick := e.Source.Name
	channelRaw := e.Params[0]
	channel := normalizeIRC(channelRaw, getCasemapping(network.Name))
	if resolvedUser == nil {
		resolvedUser = resolvedUserFromCtx(ctx)
	}
	if resolvedUser == nil {
		var err error
		resolvedUser, err = resolveIRCUser(network, c, e)
		if err != nil {
			runner.logger.Error("failed to resolve user in generator()", "error", err)
		}
		proceed, _ := handleResolveResult(c, e, resolvedUser, err)
		if !proceed {
			return
		}
	}
	if resolvedUser == nil {
		runner.logger.Warn("generator() got nil resolved user, dropping message", "nick", nick)
		return
	}
	if resolvedUser.Flagged {
		runner.logger.Warn("generator() proceeding with flagged user",
			"user_id", resolvedUser.ID, "nick", nick, "reason", resolvedUser.FlaggedReason)
	}
	runner.userID = resolvedUser.ID
	runner.hostmask = e.Source.Name + "!" + e.Source.Ident + "@" + e.Source.Host
	runner.ephemeral = true
	runner.respondTool = cfg.RespondTool
	if cfg.Log != nil {
		spec := *cfg.Log
		// Production specs are defaulted at config-load time; hand-built
		// or partial specs (tests, future callers) re-default here so an
		// empty window never becomes the tool's advertised default.
		applyLogQueryDefaults(&spec)
		runner.logQuery = &generatorLogQuery{
			spec:       spec,
			channelRaw: channelRaw,
			channel:    channel,
			name:       cfg.Name,
		}
	}
	// One api-log session per run: a unique negative id (never a DB
	// session id; unique across concurrent runs and restarts) opened
	// with the run's real identity. sessionID stays 0 — load-bearing
	// for usage attribution (SessionID-0 turn_usage rows) and the
	// apiLogger's legacy no-session guard.
	runner.apiLogSessionID = nextEphemeralAPILogID()
	apiLogger.RestoreSession(runner.apiLogSessionID, network.Name, channel, resolvedUser.ID)
	// setChannel syncs the transport (apiLogger under apiLogID()).
	runner.setChannel(channel, nick, resolvedUser.ID)

	userText := ""
	if len(args) > 0 {
		userText = args[0]
	}
	if userText == "" {
		// Bare invocation (optional-args dispatch) — the model defaults
		// the window via the tool; the prompt is the instruction.
		userText = cfg.Prompt
	}
	if userText == "" {
		userText = "Summarize the following channel activity."
	}

	systemContent := cfg.System
	if cfg.SystemTmpl != nil {
		data := buildSystemPromptData(network, c, channel, nick)
		data.AsyncResultRole = asyncResultRole(cfg.AIConfig)
		var buf strings.Builder
		if err := cfg.SystemTmpl.Execute(&buf, data); err != nil {
			runner.logger.Error("system prompt template execution error:", err)
		} else {
			systemContent = buf.String()
		}
	}

	turn := newEphemeralTurnContext([]ChatMessage{
		{Role: RoleSystem, Content: systemContent},
		{Role: RoleUser, Content: userText},
	})
	runner.runTurn(turn)
	runner.logger.Debug("generator finished", "api_log", apiLogger.GetSessionFilePath(runner.apiLogID()))
}
