package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	markdowntoirc "github.com/knivey/dave/MarkdownToIRC"
	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/responses"
)

var responseChainMu sync.Map

// defaultCompletionMaxTokens bounds a legacy completion when no config value
// resolves. llama.cpp treats an absent (or effectively unset) max_tokens as
// "generate until EOS or the full context window" — a base model asked to
// continue prose may ramble for many minutes, long past any proxy read
// timeout (observed in production: the llama-server multi-model router runs
// with timeout=10 and returns 500 "proxy error: Failed to read connection"
// for every generation that outlives 10 seconds).
const defaultCompletionMaxTokens = 500

// completionMaxTokens resolves the token cap for the legacy completions API.
//
// Precedence: the command's maxcompletiontokens beats the service's maxtokens
// (services.toml documents maxtokens as the legacy knob, "prefer
// maxcompletiontokens") — the cascade in ApplyDefaults only fills zeros, so
// without this a command declaring maxcompletiontokens=200 against a service
// default of maxtokens=500 would silently generate 500. A config where both
// resolve to zero still gets the bounded default above so the request is
// never unbounded.
func completionMaxTokens(cfg AIConfig) int64 {
	if cfg.MaxCompletionTokens > 0 {
		return int64(cfg.MaxCompletionTokens)
	}
	if cfg.MaxTokens > 0 {
		return int64(cfg.MaxTokens)
	}
	return defaultCompletionMaxTokens
}

func completion(network Network, c *girc.Client, e girc.Event, cfg AIConfig, ctx context.Context, output chan<- string, args ...string) {
	var svcKey, svcBaseURL string
	readConfig(func() {
		svcKey = config.Services[cfg.Service].Key
		svcBaseURL = config.Services[cfg.Service].BaseURL
	})

	aiClient := openai.NewClient(
		option.WithAPIKey(svcKey),
		option.WithBaseURL(svcBaseURL),
	)

	logger := newLogger(network.Name + ".completion." + cfg.Name)

	// cfg.Timeout cascades command > service > 60s in ApplyDefaults, so it is
	// always non-zero here; honoring it (instead of a hardcoded 60s) lets a
	// slow local endpoint be given a longer budget per-command.
	apiCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	// Model-load probe for legacy completions (llama-server router /
	// llama-swap): same pre-request check the chat path does in runTurn.
	if cfg.LoadNotice != nil && *cfg.LoadNotice {
		sendModelLoadNotice(ctx, nil, logger, svcBaseURL, svcKey, cfg.Model, e.Source.Name, func(msg string) {
			sendToOutput(msg, output, ctx)
		})
	}

	resp, err := aiClient.Completions.New(apiCtx, openai.CompletionNewParams{
		Model: openai.CompletionNewParamsModel(cfg.Model),
		Prompt: openai.CompletionNewParamsPromptUnion{
			OfString: openai.String(args[0]),
		},
		MaxTokens:   openai.Int(completionMaxTokens(cfg)),
		Temperature: openai.Float(float64(cfg.Temperature)),
	})
	if err != nil {
		select {
		case output <- errorMsg(err.Error()):
		case <-ctx.Done():
		}
		logger.Error(err.Error())
		return
	}

	if len(resp.Choices) == 0 {
		logger.Warn("API returned no choices")
		return
	}

	logger.Info(resp.Choices[0].Text)
	sendToOutput(resp.Choices[0].Text, output, ctx)
}

type Timings struct {
	CacheN              int     `json:"cache_n"`
	PromptN             int     `json:"prompt_n"`
	PromptMs            float64 `json:"prompt_ms"`
	PromptPerTokenMs    float64 `json:"prompt_per_token_ms"`
	PromptPerSecond     float64 `json:"prompt_per_second"`
	PredictedN          int     `json:"predicted_n"`
	PredictedMs         float64 `json:"predicted_ms"`
	PredictedPerTokenMs float64 `json:"predicted_per_token_ms"`
	PredictedPerSecond  float64 `json:"predicted_per_second"`
}

func extractTimings(raw []byte) *Timings {
	var wrapper struct {
		Timings *Timings `json:"timings"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil || wrapper.Timings == nil {
		return nil
	}
	return wrapper.Timings
}

func logTimings(logger logxi.Logger, timings *Timings) {
	if timings == nil {
		return
	}
	fields := []interface{}{
		"prompt", timings.PromptN,
		"cache", timings.CacheN,
		"completion", timings.PredictedN,
		"prompt_speed", fmt.Sprintf("%.1ftok/s", timings.PromptPerSecond),
		"completion_speed", fmt.Sprintf("%.1ftok/s", timings.PredictedPerSecond),
		"prompt_time", fmt.Sprintf("%.0fms", timings.PromptMs),
		"completion_time", fmt.Sprintf("%.0fms", timings.PredictedMs),
	}
	if timings.PredictedPerTokenMs > 0 {
		fields = append(fields, "completion_per_token", fmt.Sprintf("%.1fms", timings.PredictedPerTokenMs))
	}
	if timings.PromptPerTokenMs > 0 {
		fields = append(fields, "prompt_per_token", fmt.Sprintf("%.1fms", timings.PromptPerTokenMs))
	}
	logger.Info("timings", fields...)
}

func logUsage(logger logxi.Logger, usage *Usage) {
	if usage == nil {
		logger.Debug("no usage reported")
		return
	}
	fields := []interface{}{
		"prompt", usage.PromptTokens,
		"completion", usage.CompletionTokens,
		"total", usage.TotalTokens,
	}
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens > 0 {
		fields = append(fields, "reasoning", usage.CompletionTokensDetails.ReasoningTokens)
	}
	if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens > 0 {
		fields = append(fields, "cached", usage.PromptTokensDetails.CachedTokens)
	}
	logger.Info("token usage", fields...)
}

func (cr *chatRunner) storeUsage(usage *Usage, apiPath string, durationMs int) {
	logUsage(cr.logger, usage)
	if theDB == nil || usage == nil || (cr.sessionID == 0 && !cr.ephemeral) {
		return
	}
	if cr.sessionID == 0 {
		// Reaching here means cr.ephemeral (the guard above kept the legacy
		// session-0 skip for non-ephemeral runners). Ephemeral (generator)
		// runs have no session row, but their usage is still worth
		// attributing: the row is written with SessionID 0 — the column is
		// not-null but FK-less, and the denormalized Model/Service/
		// ReasoningEffort columns give generator calls the same cost
		// attribution as chat turns (spec §Ephemeral Turn Machinery).
		// insertDBTurnUsage defensively skips session 0 (legacy callers
		// could never legitimately have one), so this path builds the row
		// directly — a mirror of insertDBTurnUsage's field mapping (db.go);
		// keep the two in sync.
		var cachedTokens, reasoningTokens int
		if usage.PromptTokensDetails != nil {
			cachedTokens = int(usage.PromptTokensDetails.CachedTokens)
		}
		if usage.CompletionTokensDetails != nil {
			reasoningTokens = int(usage.CompletionTokensDetails.ReasoningTokens)
		}
		row := TurnUsage{
			SessionID:        0,
			Model:            cr.cfg.Model,
			Service:          cr.cfg.Service,
			ReasoningEffort:  cr.cfg.ReasoningEffort,
			PromptTokens:     int(usage.PromptTokens),
			CompletionTokens: int(usage.CompletionTokens),
			CachedTokens:     cachedTokens,
			ReasoningTokens:  reasoningTokens,
			FinishReason:     usage.FinishReason,
			APIPath:          apiPath,
			DurationMs:       durationMs,
		}
		if err := theDB.Create(&row).Error; err != nil {
			cr.logger.Error("Failed to store ephemeral turn usage", "error", err)
		}
		return
	}
	if err := insertDBTurnUsage(cr.sessionID, cr.cfg, usage, usage.FinishReason, apiPath, durationMs); err != nil {
		cr.logger.Error("Failed to store turn usage", "error", err)
	}
}

type chatRunner struct {
	openaiClient *openai.Client
	transport    *daveTransport
	httpClient   *http.Client
	baseURL      string
	apiKey       string
	cfg          AIConfig
	network      Network
	client       *girc.Client
	channel      string
	nick         string
	userID       int64
	hostmask     string
	logger       logxi.Logger
	ctx          context.Context
	outputCh     chan<- string
	sessionID    int64
	convID       string
	// ephemeral marks a generator run: no session row backs this runner.
	// Gates: response-id persistence (handleResponseIDSave), builtin tool
	// offering (getTools), and the session-0 usage-row exception
	// (storeUsage). Everything else in runTurn is untouched.
	ephemeral bool
	// logQuery is the generator's log-retrieval context — nil on
	// non-generator turns and log-less generators. Gates the
	// query_channel_logs offering and carries the channel names and
	// generator name its handler needs.
	logQuery *generatorLogQuery
	// respondTool mirrors the generator's respond_tool config: offer
	// the respond tool on this ephemeral turn.
	respondTool bool
	// responded is set by handleGeneratorRespond; every runTurn variant
	// checks it after executeToolCalls and ends the turn without
	// another API round-trip.
	responded bool
	// apiLogSessionID overrides the id the apiLogger logs this runner's
	// traffic under. Zero (the default) means "log under sessionID" —
	// normal turns never touch it. Ephemeral (generator) runs set it to
	// a unique negative id (nextEphemeralAPILogID): their sessionID is
	// 0 (a no-op in the apiLogger, and load-bearing for usage
	// attribution), so each run gets its own api-log session instead.
	apiLogSessionID int64
}

func newChatRunner(network Network, client *girc.Client, cfg AIConfig) *chatRunner {
	var svc Service
	readConfig(func() { svc = config.Services[cfg.Service] })
	extraBody := make(map[string]any, len(cfg.ExtraBody)+len(cfg.ChatTemplateKwargs)+1)
	for k, v := range cfg.ExtraBody {
		extraBody[k] = v
	}
	if len(cfg.ChatTemplateKwargs) > 0 {
		extraBody["chat_template_kwargs"] = cfg.ChatTemplateKwargs
	}
	if svc.Type == "llama" {
		extraBody["timings_per_token"] = true
	}
	var extraHeaders map[string]string
	if isGrokService(svc.BaseURL) {
		extraHeaders = make(map[string]string)
	}
	transport := newDaveTransport(extraBody, extraHeaders)
	httpClient := &http.Client{Transport: transport}

	openaiClient := openai.NewClient(
		option.WithAPIKey(svc.Key),
		option.WithBaseURL(svc.BaseURL),
		option.WithHTTPClient(httpClient),
		option.WithMaxRetries(2),
	)

	logger := newLogger(network.Name + ".completion." + cfg.Name)

	return &chatRunner{
		openaiClient: &openaiClient,
		transport:    transport,
		httpClient:   httpClient,
		baseURL:      svc.BaseURL,
		apiKey:       svc.Key,
		cfg:          cfg,
		network:      network,
		client:       client,
		logger:       logger,
	}
}

func isGrokService(baseURL string) bool {
	return strings.HasSuffix(baseURLHost(baseURL), ".x.ai")
}

func isOpenAIService(baseURL string) bool {
	host := baseURLHost(baseURL)
	return host == "api.openai.com" || strings.HasSuffix(host, ".openai.com")
}

func isOpenRouterService(baseURL string) bool {
	host := baseURLHost(baseURL)
	return host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai")
}

// baseURLHost returns the lowercased hostname of a service base URL, or ""
// when the URL cannot be parsed.
func baseURLHost(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func (cr *chatRunner) setChannel(channel, nick string, userID int64) {
	cr.channel = channel
	cr.nick = nick
	cr.userID = userID
	cr.syncAPISessionID()
	cr.syncConvID()
}

func (cr *chatRunner) setSessionInfo(sessionID int64, convID string) {
	cr.sessionID = sessionID
	cr.convID = convID
	cr.syncAPISessionID()
	cr.syncConvID()
}

// apiLogID is the id api-logging paths use for this runner: the
// ephemeral override when set, else the session id.
func (cr *chatRunner) apiLogID() int64 {
	if cr.apiLogSessionID != 0 {
		return cr.apiLogSessionID
	}
	return cr.sessionID
}

func (cr *chatRunner) syncAPISessionID() {
	cr.transport.setAPILogger(apiLogger, cr.apiLogID())
}

func (cr *chatRunner) syncConvID() {
	if cr.convID != "" {
		cr.transport.setExtraHeaders(map[string]string{"x-grok-conv-id": cr.convID})
	}
}

func (cr *chatRunner) sendIRC(out string) {
	for _, line := range wrapForIRC(out) {
		if len(line) <= 0 {
			continue
		}
		select {
		case cr.outputCh <- line:
		case <-cr.ctx.Done():
			return
		}
	}
}

func (cr *chatRunner) sendError(msg string) {
	select {
	case cr.outputCh <- errorMsg(msg):
	case <-cr.ctx.Done():
	}
}

// addContext persists a message to the DB. Use ONLY for pre-turn setup (system
// prompt, user message) and non-turn paths (async injection, compaction).
// During a turn, use turnContext.Add() instead.
func (cr *chatRunner) addContext(msg ChatMessage) {
	if cr.sessionID == 0 {
		cr.logger.Error("addContext called with sessionID=0")
		return
	}
	if err := sessionMgr.AddMessage(cr.sessionID, msg); err != nil {
		cr.logger.Error("Failed to add message", "session", cr.sessionID, "error", err)
	}
}

func (cr *chatRunner) sendWithPastebin(text, rawText string) bool {
	chCfg := cr.network.GetChannelConfig(cr.channel)
	if chCfg.Pastebin {
		lines := wrapForIRC(text)
		if len(lines) >= chCfg.GetMaxLines() {
			pasteTitle := cr.cfg.Service + "/" + cr.cfg.Model
			url, err := uploadToPastebin(rawText, pasteTitle)
			n := getNotices()
			if err != nil {
				cr.sendIRC(errorNotice(n.DB.PastebinUpload, map[string]string{"error": err.Error()}))
				preview := chCfg.GetMaxLines()
				if preview > len(lines) {
					preview = len(lines)
				}
				for i := 0; i < preview; i++ {
					cr.sendIRC(lines[i])
				}
				cr.sendIRC(n.Pastebin.Failed)
			} else {
				preview := chCfg.GetPastebinPreviewLines(config.Pastebin)
				if preview > len(lines) {
					preview = len(lines)
				}
				for i := 0; i < preview; i++ {
					cr.sendIRC(lines[i])
				}
				cr.sendIRC(expandNotice(n.Pastebin.Link, map[string]string{"url": url}))
			}
			return true
		}
	}
	cr.sendIRC(text)
	return false
}

func (cr *chatRunner) renderAPIUser() string {
	if cr.cfg.apiUserTmpl == nil {
		return ""
	}
	data := buildSystemPromptData(cr.network, nil, cr.channel, cr.nick)
	// The identifier renders per-request, when the session exists (unlike the
	// system prompt, which renders before session creation).
	data.SessionID = cr.sessionID
	data.AsyncResultRole = asyncResultRole(cr.cfg)

	var buf strings.Builder
	if err := cr.cfg.apiUserTmpl.Execute(&buf, data); err != nil {
		cr.logger.Error("api_user template execution error", "error", err)
		return ""
	}
	return buf.String()
}

// apiIdentity carries the rendered api_user value in the wire fields the
// target provider supports. Empty fields are omitted from the request.
type apiIdentity struct {
	User     string // legacy "user" field
	SafetyID string // safety_identifier field
	CacheKey string // prompt_cache_key field
}

// apiIdentity routes the rendered api_user template value to the request
// fields each provider documents.
//
// DESIGN NOTE: unknown/self-hosted providers keep the legacy "user" field so
// behavior is unchanged there — strict OpenAI-compatible gateways
// (llama.cpp-style) reject unknown request fields, and dave explicitly
// supports such services. Field support per provider docs (Oct 2026):
//   - OpenAI deprecated "user" in favor of safety_identifier +
//     prompt_cache_key (both Chat Completions and Responses).
//   - xAI mirrors OpenAI: safety_identifier on both paths; prompt_cache_key
//     documented on the Responses API only — its Chat Completions cache
//     routing rides the x-grok-conv-id header, which syncConvID already sends.
//   - OpenRouter documents "user" on chat completions but safety_identifier
//     on the Responses API.
func (cr *chatRunner) apiIdentity(responsesAPI bool) apiIdentity {
	rendered := cr.renderAPIUser()
	if rendered == "" {
		return apiIdentity{}
	}
	switch {
	case isOpenAIService(cr.baseURL):
		return apiIdentity{SafetyID: rendered, CacheKey: rendered}
	case isGrokService(cr.baseURL):
		if responsesAPI {
			return apiIdentity{SafetyID: rendered, CacheKey: rendered}
		}
		return apiIdentity{SafetyID: rendered}
	case isOpenRouterService(cr.baseURL):
		if responsesAPI {
			return apiIdentity{SafetyID: rendered}
		}
		return apiIdentity{User: rendered}
	default:
		return apiIdentity{User: rendered}
	}
}

func (cr *chatRunner) logAPIIncident(err error, messages []ChatMessage, iteration int, apiPath string) {
	if incidentLogger != nil {
		incidentLogger.logIncident(cr, err, messages, iteration, apiPath)
	}
}

func (cr *chatRunner) sendWarning(msg string) {
	select {
	case cr.outputCh <- warnMsg(msg):
	case <-cr.ctx.Done():
	}
}

func (cr *chatRunner) getTools() []Tool {
	if cr.ephemeral {
		// Generator turns: MCP tools work normally, but the builtin LLM
		// tools are never offered (register_background_job delivery is
		// session-bound; ban tools are chat-moderation concerns). The
		// generator tools below are offered per-config — NOT gated on
		// MCP tools existing, unlike the regular builtins.
		tools := mcpToolDefsForConfig(cr.cfg)
		if cr.logQuery != nil {
			tools = append(tools, generatorLogToolDef(cr.logQuery))
		}
		if cr.respondTool {
			tools = append(tools, generatorRespondToolDef())
		}
		return tools
	}
	return toolDefsForConfig(cr.cfg)
}

// mcpToolDefsForConfig is the config's live MCP tools minus its hidden set.
// Extracted from toolDefsForConfig so the ephemeral (generator) path can
// take exactly this subset without the builtins.
func mcpToolDefsForConfig(cfg AIConfig) []Tool {
	var hiddenMCPTools []string
	readConfig(func() {
		hiddenMCPTools = cfg.resolveHiddenMCPTools(config.MCPToolSets)
	})
	return getMCPTools(cfg.MCPs, hiddenMCPTools)
}

// toolDefsForConfig assembles the full tool-definition list a turn run
// under cfg puts on the wire: the config's live MCP tools (minus its
// hidden set) plus the builtin LLM tools, appended only when MCP tools
// exist (the builtins exist to serve tool-calling turns; a tool-less
// config has nothing to call). Extracted verbatim from
// chatRunner.getTools so /tokencount and the compaction trigger log
// count EXACTLY the tool set a real request serializes — the counted
// set can never drift from the sent set.
//
// getMCPTools reflects the LIVE MCP tool map: a disconnected (or still
// connecting) server contributes nothing here, exactly as it would
// contribute nothing to a real request — the count describes the next
// request as it would actually go out. No MCP I/O happens (in-memory
// map reads only), so this is safe to call from any accounting path.
func toolDefsForConfig(cfg AIConfig) []Tool {
	mcpTools := mcpToolDefsForConfig(cfg)
	if len(mcpTools) > 0 {
		mcpTools = append(mcpTools, getBuiltinToolDefs(cfg, cfg.DisabledBuiltinTools)...)
	}
	return mcpTools
}

// DESIGN NOTE: Both empty-response retries and previous_response_id retries
// count toward the maxToolIterations budget. Each retry consumes one iteration
// slot. Setting retry_on_empty too high (close to 20) risks hitting the limit.
const maxToolIterations = 20

func (cr *chatRunner) checkIterationLimit(iteration int) bool {
	if iteration > maxToolIterations {
		cr.sendIRC(getNotices().Tools.CallLimit)
		cr.logger.Warn("max tool iterations reached", "limit", maxToolIterations)
		return true
	}
	return false
}

// emptyResponseCorrection returns the self-correction instruction injected
// into the turn when a response comes back empty and a retry is scheduled,
// so the model is TOLD what went wrong and can fix it instead of repeating
// the same empty output. Reasoning-only failures get an explicit "the
// reasoning channel is not the response" instruction — the observed
// failure mode is the model writing its whole answer into reasoning and
// leaving the response content empty. The "[automated notice, not from
// the user]" marker keeps the row honest in API logs and the history
// viewer regardless of its wire role.
func emptyResponseCorrection(hadReasoning bool) string {
	if hadReasoning {
		return "[automated notice, not from the user] Your previous response was rejected: it produced reasoning but an EMPTY response content, so your answer never reached the user. Respond again now, placing your complete answer in the response content. The reasoning channel is for private working only — the user sees exclusively the response content."
	}
	return "[automated notice, not from the user] Your previous response was rejected: the response content came back completely empty, so the user received nothing. Respond again now, placing your complete answer in the response content."
}

// correctionUserSuffix is the guidance Knob 2 suffix text for empty-
// response corrections (guidance.go).
const correctionUserSuffix = "Respond now with your complete answer in the response content."

// addEmptyResponseCorrection appends the empty-response correction to the
// turn via the shared guidance mechanism (guidance.go): the payload row
// under Knob 1's role (default system) plus, when the provider needs an
// answerable trailing turn (Knob 2), the short user suffix after it. The
// correction is by construction the LAST message of the retry request —
// the trailing-position hazards and the two-knob design are documented in
// docs/superpowers/specs/2026-10-08-guidance-injection-role-design.md.
//
// PERSISTED deliberately (design D7): the stored session stays exactly
// what the model was told (no DB/API drift), and the nudge keeps steering
// later turns for models that chronically strand answers in reasoning.
// Back-to-back identical injections (payload+suffix tail) collapse — one
// nudge per failure shape per turn.
func (cr *chatRunner) addEmptyResponseCorrection(turn *turnContext, reasoning string) {
	seq := guidanceMessages(cr.cfg, emptyResponseCorrection(reasoning != ""), correctionUserSuffix, true)
	if messagesEndWith(turn.Messages(), seq) {
		return
	}
	for _, msg := range seq {
		turn.Add(msg)
	}
}

// checkEmptyRetry gates the retry-on-empty-response behavior shared by all
// four turn paths (chat completions + responses API, streaming and not).
//
// When the response text is empty and retries remain, it schedules a retry
// (callers inject the self-correction via addEmptyResponseCorrection).
//
// When retries are exhausted it makes sure the user is TOLD: the "..."
// sentinel returned here is stored on the session but never sent to IRC
// (sendFinalText skips it), so without an explicit notice the user would be
// left waiting for a reply that already failed. When the model produced
// reasoning but no text, the notice explains what happened and the
// reasoning content itself is delivered as a normal rendered message —
// shown to the user as a fallback, but deliberately NOT accepted as the
// reply (the stored assistant content stays the "..." sentinel).
func (cr *chatRunner) checkEmptyRetry(content, reasoning string, emptyRetries, maxEmptyRetries int) (bool, string) {
	if content != "" {
		return false, content
	}
	if emptyRetries < maxEmptyRetries {
		cr.logger.Warn("empty response from API, retrying", "attempt", emptyRetries+1, "max", maxEmptyRetries)
		return true, ""
	}
	// emptyRetries == maxEmptyRetries here, so the turn made
	// maxEmptyRetries+1 API attempts in total.
	vars := map[string]string{"attempts": fmt.Sprintf("%d", maxEmptyRetries+1)}
	if reasoning != "" {
		cr.logger.Warn("empty response from API, max retries reached, reasoning was present but no text output")
		cr.sendIRC(expandNotice(getNotices().LLM.ReasoningOnly, vars))
		cr.sendRendered(reasoning)
	} else {
		cr.logger.Warn("empty response from API, max retries reached")
		cr.sendIRC(expandNotice(getNotices().LLM.EmptyResponse, vars))
	}
	return false, "..."
}

type streamOutput struct {
	renderer *markdowntoirc.StreamingRenderer
	buffer   string
}

func (so *streamOutput) HandleDelta(delta string, send func(string)) {
	if delta == "" {
		// DESIGN NOTE: empty content deltas are routine on the wire —
		// tool-call chunks, finish chunks, keepalives and interleaved
		// reasoning chunks all carry content:"" — but the markdown
		// renderer's Process("") IS the end-of-stream flush signal.
		// Forwarding the empty delta here flushed the renderer's held
		// paragraph mid-stream, splitting output at arbitrary chunk
		// boundaries. Flushing is Flush()'s job and nothing else's.
		return
	}
	if so.renderer != nil {
		for _, line := range so.renderer.Process(delta) {
			send(line)
		}
		return
	}
	// Plain (no-markdown) mode: buffer is the dangling partial line,
	// released on the next newline. The renderer keeps its own buffer —
	// accumulating the raw text here too was pure O(n²) waste once the
	// historical re-render of it went away.
	so.buffer += delta
	if strings.Contains(so.buffer, "\n") {
		send(so.buffer)
		so.buffer = ""
	}
}

func (so *streamOutput) Flush(send func(string)) {
	if so.renderer != nil {
		for _, line := range so.renderer.Process("") {
			send(line)
		}
		return
	}
	if so.buffer != "" {
		send(so.buffer)
	}
}

// sendRendered renders rawText for IRC (markdown when enabled) and sends
// it through the pastebin-wrapping path — the send shape shared by final
// replies and the empty-response reasoning fallback, extracted so the two
// cannot drift.
func (cr *chatRunner) sendRendered(rawText string) {
	text := rawText
	if cr.cfg.RenderMarkdown {
		text = markdowntoirc.MarkdownToIRC(text)
	}
	cr.sendWithPastebin(text, rawText)
}

func (cr *chatRunner) sendFinalText(content string) {
	text := ExtractFinalText(content)
	if text != "" && text != "..." {
		cr.sendRendered(text)
	}
}

func (cr *chatRunner) handleToolCallResponse(turn *turnContext, text string, toolCalls []ToolCall, reasoning string) {
	cr.logger.Info("assistant made tool calls", "count", len(toolCalls))
	assistantMsg := ChatMessage{
		Role:             RoleAssistant,
		Content:          text,
		ReasoningContent: reasoning,
		ToolCalls:        toolCalls,
	}
	turn.Add(assistantMsg)
	if text != "" && !cr.ephemeral {
		// Ephemeral (generator) turns suppress intermediate tool-loop
		// text: it enters the turn history (the model sees its own
		// words) and the logs, but never IRC — the channel only sees
		// the curated answer (respond tool or the final no-tool-call
		// iteration).
		t := ExtractFinalText(text)
		if cr.cfg.RenderMarkdown {
			t = markdowntoirc.MarkdownToIRC(t)
		}
		cr.sendIRC(t)
	}
	if reasoning != "" {
		cr.logger.Info("reasoning", "content", reasoning)
	}
	cr.executeToolCalls(turn, toolCalls)
}

func (cr *chatRunner) handleResponseIDSave(respID, text string, toolCalls []ToolCall, currentResponseID string) string {
	if cr.ephemeral {
		// Generator runs have no session row to persist a response id on —
		// skip every sessionMgr write. The RETURN-value contract is
		// preserved: a content-bearing response still yields its id so the
		// tool loop can chain server-side WITHIN the turn; an empty output
		// keeps the previous id, mirroring the non-ephemeral decision
		// (never chain onto an empty response). Across-turn chaining is
		// impossible for generators — nothing is persisted to chain from.
		if respID != "" && (text != "" || len(toolCalls) > 0) {
			return respID
		}
		return currentResponseID
	}
	if respID != "" && (text != "" || len(toolCalls) > 0) {
		if err := sessionMgr.UpdateResponseID(cr.sessionID, &respID, cr.cfg.Model); err != nil {
			cr.logger.Error("failed to save response_id", "session", cr.sessionID, "error", err)
		}
		return respID
	}
	if respID != "" {
		if currentResponseID != "" {
			if err := sessionMgr.UpdateResponseID(cr.sessionID, nil, ""); err != nil {
				cr.logger.Error("failed to clear response_id", "session", cr.sessionID, "error", err)
			}
		}
		cr.logger.Warn("response had empty output, clearing previous_response_id", "response_id", respID)
	}
	return currentResponseID
}

func (cr *chatRunner) shouldRetryWithoutResponseID(usePrevID bool, err error, messages []ChatMessage, iteration int, apiPath, currentResponseID string) ([]responses.ResponseInputItemUnionParam, string, bool) {
	if !usePrevID || !isResponseIDError(err) || iteration != 1 {
		return nil, currentResponseID, false
	}
	cr.logAPIIncident(err, messages, iteration, apiPath)
	cr.logger.Warn("previous_response_id invalid, retrying without", "response_id", currentResponseID, "error", err)
	if err := sessionMgr.UpdateResponseID(cr.sessionID, nil, ""); err != nil {
		cr.logger.Error("failed to clear response_id on retry", "session", cr.sessionID, "error", err)
	}
	return messagesToResponseInputItems(messages), "", true
}

type responsesStreamResult struct {
	done              bool
	emptyRetries      int
	currentResponseID string
	usePrevID         bool
	input             []responses.ResponseInputItemUnionParam
}

func (cr *chatRunner) runTurnResponsesStream(
	ctx context.Context,
	params responses.ResponseNewParams,
	turn *turnContext,
	iteration int,
	emptyRetries int,
	maxEmptyRetries int,
	currentResponseID string,
	usePrevID bool,
) responsesStreamResult {
	apiStart := time.Now()
	resp, err := cr.callResponsesStream(ctx, params)
	durationMs := int(time.Since(apiStart).Milliseconds())
	if err != nil {
		if newInput, newResponseID, retry := cr.shouldRetryWithoutResponseID(usePrevID, err, turn.Messages(), iteration, "responses_stream", currentResponseID); retry {
			return responsesStreamResult{
				currentResponseID: newResponseID,
				input:             newInput,
				usePrevID:         false,
				emptyRetries:      emptyRetries,
			}
		}
		cr.sendError(err.Error())
		cr.logger.Error(err.Error())
		cr.logAPIIncident(err, turn.Messages(), iteration, "responses_stream")
		return responsesStreamResult{done: true, currentResponseID: currentResponseID, usePrevID: usePrevID, emptyRetries: emptyRetries}
	}

	if resp == nil {
		return responsesStreamResult{done: true, currentResponseID: currentResponseID, usePrevID: usePrevID, emptyRetries: emptyRetries}
	}
	text, reasoning, toolCalls := parseSDKResponseOutput(*resp)
	currentResponseID = cr.handleResponseIDSave(resp.ID, text, toolCalls, currentResponseID)

	assistantMsg := ChatMessage{
		Role:             RoleAssistant,
		Content:          text,
		ReasoningContent: reasoning,
	}

	if len(toolCalls) == 0 {
		if retry, newText := cr.checkEmptyRetry(text, reasoning, emptyRetries, maxEmptyRetries); retry {
			cr.addEmptyResponseCorrection(turn, reasoning)
			// Full-history retry (correction included) with the chain
			// dropped: handleResponseIDSave already cleared the stored
			// response_id after the empty output, and keeping the old
			// chain head alongside a full-history resend would
			// duplicate context server-side. A successful retry
			// starts a fresh chain.
			return responsesStreamResult{
				emptyRetries:      emptyRetries + 1,
				currentResponseID: "",
				usePrevID:         false,
				input:             messagesToResponseInputItems(turn.Messages()),
			}
		} else if newText != text {
			text = newText
			assistantMsg.Content = text
		}
		turn.Add(assistantMsg)
		cr.logger.Info(FormatOutput(text))
		cr.storeUsage(sdkResponseUsageToUsage(resp.Usage, string(resp.Status)), "responses_stream", durationMs)
		if reasoning != "" {
			cr.logger.Info("reasoning", "content", reasoning)
		}
		if cr.ephemeral {
			// Deferred emission (suppression): the collector no longer
			// flushes; the final iteration's text is sent here,
			// complete — mirroring the non-streaming Responses path.
			cr.sendFinalText(text)
		}
		return responsesStreamResult{done: true, currentResponseID: currentResponseID, usePrevID: usePrevID, emptyRetries: emptyRetries}
	}

	emptyRetries = 0
	assistantMsg.ToolCalls = toolCalls
	turn.Add(assistantMsg)
	cr.logger.Info("assistant made tool calls", "count", len(toolCalls))
	cr.storeUsage(sdkResponseUsageToUsage(resp.Usage, string(resp.Status)), "responses_stream", durationMs)

	numToolCalls := len(toolCalls)
	cr.executeToolCalls(turn, toolCalls)
	if cr.responded {
		// respond delivered the final answer — the turn is complete,
		// no further API round-trip.
		return responsesStreamResult{done: true, currentResponseID: currentResponseID, usePrevID: usePrevID, emptyRetries: emptyRetries}
	}

	var newInput []responses.ResponseInputItemUnionParam
	// Mirror the non-streaming tool-loop: input shape and the chain
	// decision are ONE choice — delta-only input (tool results) must chain
	// currentResponseID on the next request or the provider loses all
	// context; full-history input must drop the id to match.
	chain := chainActive(cr.cfg, currentResponseID)
	if chain {
		toolResultMsgs := turn.LastN(numToolCalls)
		newInput = toolResultMsgsToInputItems(toolResultMsgs)
	} else {
		newInput = messagesToResponseInputItems(turn.Messages())
	}
	return responsesStreamResult{
		emptyRetries:      emptyRetries,
		currentResponseID: currentResponseID,
		usePrevID:         chain,
		input:             newInput,
	}
}

func (cr *chatRunner) runTurnStream(ctx context.Context, params openai.ChatCompletionNewParams, turn *turnContext, iterations, emptyRetries, maxEmptyRetries int) (bool, int) {
	stream := cr.openaiClient.Chat.Completions.NewStreaming(ctx, params)
	apiStart := time.Now()

	fullContent := ""
	reasoningBuffer := ""
	var sOut streamOutput
	// Ephemeral (generator) turns never render live: emission is
	// deferred to the iteration's classification (final text via
	// sendFinalText, stream-death text via sendFinalText, intermediate
	// text never). No renderer means no accidental live path.
	if cr.cfg.RenderMarkdown && !cr.ephemeral {
		sOut.renderer = markdowntoirc.NewStreamingRenderer()
	}

	var accumulatedToolCalls []ToolCall
	var assistantRole string
	var streamUsage *Usage
	var streamTimings *Timings
	var streamFinishReason string
	streamDone := false
	streamModel := cr.cfg.Model

	type recvResult struct {
		chunk openai.ChatCompletionChunk
		done  bool
		err   error
	}

	idleTimer := time.NewTimer(cr.cfg.StreamTimeout)
	defer idleTimer.Stop()

StreamLoop:
	for {
		if cr.ctx.Err() != nil {
			cr.logger.Info("Closing stream")
			stream.Close()
			return true, emptyRetries
		}

		ch := make(chan recvResult, 1)
		go func() {
			if stream.Next() {
				ch <- recvResult{chunk: stream.Current()}
			} else {
				err := stream.Err()
				if err == nil {
					ch <- recvResult{done: true}
				} else {
					ch <- recvResult{err: err, done: true}
				}
			}
		}()

		select {
		case res := <-ch:
			if res.done {
				if res.err != nil {
					stream.Close()
					// The stream died mid-generation: deliver whatever
					// complete text the renderer is still holding before
					// the error notice, instead of silently dropping it.
					if cr.ephemeral {
						// Deferred emission: deliver what arrived (parity
						// with the non-ephemeral flush), complete.
						cr.sendFinalText(fullContent)
					} else {
						sOut.Flush(cr.sendIRC)
					}
					cr.sendError(res.err.Error())
					cr.logger.Error(res.err.Error())
					cr.logAPIIncident(res.err, turn.Messages(), iterations, "chat_completions_stream")
					return true, emptyRetries
				}
				cr.logger.Info("Stream completed")
				stream.Close()
				break StreamLoop
			}
			idleTimer.Reset(cr.cfg.StreamTimeout)

			chunk := res.chunk
			rawBytes := []byte(chunk.RawJSON())
			if apiLogger != nil {
				apiLogger.LogStreamChunk(cr.transport.sessionID, rawBytes)
			}

			chunkReasoning := ""
			if len(chunk.Choices) > 0 {
				// DESIGN NOTE: deliberately NOT gating on f.Valid() —
				// the openai-go SDK marks fields it has no typed
				// destination for (everything lands in ExtraFields)
				// with status=invalid, so Valid() is always false for
				// reasoning_content even though Raw() carries the
				// value. Map presence + a successful non-empty string
				// unmarshal is the correct test (unmarshal of a null
				// raw succeeds but leaves rc empty, which the
				// rc != "" check rejects).
				if f, ok := chunk.Choices[0].Delta.JSON.ExtraFields["reasoning_content"]; ok {
					var rc string
					if err := json.Unmarshal([]byte(f.Raw()), &rc); err == nil && rc != "" {
						chunkReasoning = rc
					}
				}
			}
			if chunkReasoning == "" {
				chunkReasoning = extractStreamReasoning(rawBytes)
			}
			if chunkReasoning == "" {
				chunkReasoning = extractReasoningFromField(rawBytes)
			}
			reasoningBuffer += chunkReasoning

			if chunk.Usage.CompletionTokens > 0 || chunk.Usage.PromptTokens > 0 {
				streamUsage = sdkChatUsageToUsage(chunk.Usage)
			}
			if t := extractTimings(rawBytes); t != nil {
				streamTimings = t
			}
			if len(chunk.Choices) == 0 {
				continue
			}
			choice := chunk.Choices[0]
			delta := choice.Delta

			// DESIGN NOTE: finish_reason is NOT the end of the wire. The
			// OpenAI spec (and llama.cpp, verified against production Oct
			// 2026) streams a FINAL usage chunk with an empty choices
			// array AFTER the finish chunk — closing the stream at the
			// finish chunk is why usage was never captured on ANY
			// streaming command (llama, OpenAI, everything). Keep reading
			// until [DONE]; the loop guards below stop content
			// accumulation once a finish reason has been seen.
			if streamFinishReason == "" {
				if delta.Role != "" {
					assistantRole = delta.Role
				}

				for _, tc := range delta.ToolCalls {
					idx := int(tc.Index)
					for len(accumulatedToolCalls) <= idx {
						accumulatedToolCalls = append(accumulatedToolCalls, ToolCall{})
					}
					if tc.ID != "" {
						accumulatedToolCalls[idx].ID = tc.ID
					}
					if tc.Type != "" {
						accumulatedToolCalls[idx].Type = tc.Type
					}
					accumulatedToolCalls[idx].Function.Name += tc.Function.Name
					accumulatedToolCalls[idx].Function.Arguments += tc.Function.Arguments
				}

				textDelta := delta.Content
				fullContent += textDelta
				// Ephemeral (generator) turns accumulate without emitting —
				// finality is unknowable mid-stream, so emission waits for
				// the iteration's classification.
				if !cr.ephemeral {
					sOut.HandleDelta(textDelta, cr.sendIRC)
				}
			}

			if choice.FinishReason == "tool_calls" {
				streamFinishReason = "tool_calls"
				cr.logger.Info("stream finished with tool calls")
				continue
			}
			if choice.FinishReason == "stop" || choice.FinishReason == "length" {
				streamFinishReason = string(choice.FinishReason)
				streamDone = true
				continue
			}

		case <-idleTimer.C:
			stream.Close()
			if streamFinishReason != "" {
				// The generation finished but the server never sent the
				// usage trailer / [DONE] — accept what arrived instead of
				// erroring out a completed turn.
				break StreamLoop
			}
			// Idle before any finish: the tail the renderer is holding is
			// incomplete but real — show it, then the timeout error.
			if cr.ephemeral {
				// Deferred emission: deliver what arrived (parity with
				// the non-ephemeral flush), complete.
				cr.sendFinalText(fullContent)
			} else {
				sOut.Flush(cr.sendIRC)
			}
			timeoutErr := fmt.Errorf("stream timed out (no data received): timeout=%s", cr.cfg.StreamTimeout)
			cr.sendError(timeoutErr.Error())
			cr.logger.Error("stream idle timeout exceeded", "timeout", cr.cfg.StreamTimeout)
			cr.logAPIIncident(timeoutErr, turn.Messages(), iterations, "chat_completions_stream")
			return true, emptyRetries
		}
	}

	logStreamCompletion(cr.transport.sessionID, streamModel, fullContent, reasoningBuffer, accumulatedToolCalls, streamUsage, assistantRole)

	if streamUsage != nil {
		streamUsage.FinishReason = streamFinishReason
	}
	streamDurationMs := int(time.Since(apiStart).Milliseconds())

	flushStreamedOutput := func() {
		cr.logger.Info(FormatOutput(fullContent))
		content := fullContent
		if content == "" && reasoningBuffer == "" {
			content = "..."
		}
		turn.Add(ChatMessage{
			Role:             RoleAssistant,
			Content:          content,
			ReasoningContent: reasoningBuffer,
		})
		if cr.ephemeral {
			// Deferred emission: the final iteration's text, complete.
			// (content may be the "..." sentinel — sendFinalText skips
			// it exactly like the non-streaming path.)
			cr.sendFinalText(content)
		} else {
			sOut.Flush(cr.sendIRC)
		}
		cr.storeUsage(streamUsage, "chat_completions_stream", streamDurationMs)
		logTimings(cr.logger, streamTimings)
		if reasoningBuffer != "" {
			cr.logger.Info("reasoning", "content", reasoningBuffer)
		}
	}

	if streamDone || len(accumulatedToolCalls) == 0 {
		if len(accumulatedToolCalls) == 0 {
			if retry, newContent := cr.checkEmptyRetry(fullContent, reasoningBuffer, emptyRetries, maxEmptyRetries); retry {
				// The outer loop rebuilds params from turn.Messages(),
				// so the persisted correction rides along.
				cr.addEmptyResponseCorrection(turn, reasoningBuffer)
				emptyRetries++
				return false, emptyRetries
			} else if newContent != fullContent {
				fullContent = newContent
			}
		}
		flushStreamedOutput()
		return true, emptyRetries
	}

	emptyRetries = 0
	cr.logger.Info("stream made tool calls", "count", len(accumulatedToolCalls))
	cr.storeUsage(streamUsage, "chat_completions_stream", streamDurationMs)
	logTimings(cr.logger, streamTimings)

	if assistantRole == "" {
		assistantRole = RoleAssistant
	}

	assistantMsg := ChatMessage{
		Role:             assistantRole,
		Content:          fullContent,
		ReasoningContent: reasoningBuffer,
		ToolCalls:        accumulatedToolCalls,
	}

	if cr.ephemeral {
		// Suppressed from IRC (intermediate iteration); keep it visible
		// in logs for parity with the non-ephemeral path, where the
		// streamed text reached the channel (and the logs).
		if fullContent != "" {
			cr.logger.Info(FormatOutput(fullContent))
		}
	} else {
		// Flush ONLY the renderer's unsent tail. The historical code re-rendered
		// sOut.buffer here, which in renderer mode held the FULL raw text —
		// duplicating every line already streamed to IRC. Flush() sends exactly
		// the pending remainder in both modes (the no-renderer tail is
		// sOut.buffer's dangling partial line).
		sOut.Flush(cr.sendIRC)
	}

	turn.Add(assistantMsg)

	cr.executeToolCalls(turn, accumulatedToolCalls)
	if cr.responded {
		// respond delivered the final answer — the turn is complete,
		// no further API round-trip.
		return true, emptyRetries
	}
	return false, emptyRetries
}

func (cr *chatRunner) runTurn(turn *turnContext) bool {
	// Pre-turn model-load probe (llama-server router / llama-swap): if the
	// service reports our model is not loaded yet, tell the user before the
	// request blocks on the load. No-op unless load_notice resolved true.
	cr.maybeNotifyModelLoad()

	if cr.cfg.ResponsesAPI {
		return cr.runTurnResponses(turn)
	}
	mcpTools := cr.getTools()

	ctx, cancel := context.WithTimeout(cr.ctx, cr.cfg.Timeout)
	defer cancel()

	iterations := 0
	emptyRetries := 0
	maxEmptyRetries := 0
	if cr.cfg.RetryOnEmpty != nil {
		maxEmptyRetries = *cr.cfg.RetryOnEmpty
	}

	for {
		iterations++
		if cr.checkIterationLimit(iterations) {
			return true
		}

		params := buildChatCompletionParams(cr.cfg, turn.Messages(), mcpTools, cr.apiIdentity(false))

		if cr.cfg.Streaming {
			var done bool
			done, emptyRetries = cr.runTurnStream(ctx, params, turn, iterations, emptyRetries, maxEmptyRetries)
			if !done {
				continue
			}
			return true
		}

		cr.transport.setCaptureBody(true)
		apiStart := time.Now()
		resp, err := cr.openaiClient.Chat.Completions.New(ctx, params)
		cr.transport.setCaptureBody(false)
		if err != nil {
			cr.sendError(err.Error())
			cr.logger.Error(err.Error())
			cr.logAPIIncident(err, turn.Messages(), iterations, "chat_completions")
			return true
		}
		durationMs := int(time.Since(apiStart).Milliseconds())

		content, reasoning, toolCalls, usage := parseChatCompletionResponse(*resp)

		capturedBody := cr.transport.getCapturedBody()
		rawReasoning, rawDetails := extractResponseReasoning(capturedBody)
		if reasoning == "" && rawReasoning != "" {
			reasoning = rawReasoning
		}
		if reasoning == "" && len(rawDetails) > 0 {
			reasoning = extractReasoningText(rawDetails)
		}
		nonStreamTimings := extractTimings(capturedBody)

		if len(toolCalls) == 0 {
			if retry, newContent := cr.checkEmptyRetry(content, reasoning, emptyRetries, maxEmptyRetries); retry {
				// Tell the model what went wrong so the retry can
				// correct itself; params are rebuilt from
				// turn.Messages() at the top of the loop.
				cr.addEmptyResponseCorrection(turn, reasoning)
				emptyRetries++
				continue
			} else {
				content = newContent
			}
			turn.Add(ChatMessage{
				Role:             RoleAssistant,
				Content:          content,
				ReasoningContent: reasoning,
			})
			out := FormatOutput(content)
			cr.logger.Info(out)
			cr.storeUsage(usage, "chat_completions", durationMs)
			logTimings(cr.logger, nonStreamTimings)
			if reasoning != "" {
				cr.logger.Info("reasoning", "content", reasoning)
			}

			cr.sendFinalText(content)
			return true
		}

		emptyRetries = 0
		cr.storeUsage(usage, "chat_completions", durationMs)
		logTimings(cr.logger, nonStreamTimings)
		cr.handleToolCallResponse(turn, content, toolCalls, reasoning)
		if cr.responded {
			// respond delivered the final answer — the turn is complete,
			// no further API round-trip.
			return true
		}
	}
}

func (cr *chatRunner) executeToolCalls(turn *turnContext, toolCalls []ToolCall) bool {
	registeredJob := false

	var hiddenTools []string
	var hiddenMCPTools []string
	readConfig(func() {
		hiddenTools = config.HiddenTools
		hiddenMCPTools = cr.cfg.resolveHiddenMCPTools(config.MCPToolSets)
	})
	verbose := cr.cfg.ToolVerbose == nil || *cr.cfg.ToolVerbose
	type toolEntry struct {
		server string
		tool   string
	}
	var visibleTools []toolEntry
	for _, tc := range toolCalls {
		if isToolHidden(tc.Function.Name, hiddenTools) {
			continue
		}
		visibleTools = append(visibleTools, toolEntry{
			server: getToolServerName(tc.Function.Name),
			tool:   tc.Function.Name,
		})
	}
	if verbose && len(visibleTools) > 1 {
		parts := make([]string, len(visibleTools))
		for i, e := range visibleTools {
			server := e.server
			if server == "" {
				// Tool's server isn't registered right now (typically
				// still pending in a background retry loop); the
				// per-tool handling below will wake or fail it.
				server = "(connecting)"
			}
			parts[i] = "[" + server + "] " + e.tool
		}
		cr.sendIRC(expandNotice(getNotices().Tools.CallMulti, map[string]string{
			"tools": strings.Join(parts, ", "),
		}))
	}

	for _, tc := range toolCalls {
		if entry, ok := generatorTools[tc.Function.Name]; ok {
			if isToolDisabled(tc.Function.Name, cr.cfg.DisabledBuiltinTools) {
				toolMsg := toolResultMsg(tc.ID, fmt.Sprintf("error: tool %q is disabled for this command", tc.Function.Name))
				turn.Add(toolMsg)
				continue
			}
			if verbose && !isToolHidden(tc.Function.Name, hiddenTools) && len(visibleTools) <= 1 {
				cr.sendIRC(expandNotice(getNotices().Tools.Call, map[string]string{"server": "builtin", "tool": tc.Function.Name}))
			}
			cr.logger.Info("generator tool call", "tool", tc.Function.Name)
			entry.handler(cr, turn, tc)
			continue
		}
		if entry, ok := builtinTools[tc.Function.Name]; ok {
			if isToolDisabled(tc.Function.Name, cr.cfg.DisabledBuiltinTools) {
				toolMsg := toolResultMsg(tc.ID, fmt.Sprintf("error: tool %q is disabled for this command", tc.Function.Name))
				turn.Add(toolMsg)
				continue
			}
			if verbose && !isToolHidden(tc.Function.Name, hiddenTools) && len(visibleTools) <= 1 {
				cr.sendIRC(expandNotice(getNotices().Tools.Call, map[string]string{"server": "builtin", "tool": tc.Function.Name}))
			}
			cr.logger.Info("builtin tool call", "tool", tc.Function.Name)
			if entry.handler(cr, turn, tc) {
				registeredJob = true
			}
			continue
		}
		// Resolve via the ctx-aware path: if the tool's server is still
		// pending in a background retry loop (initial connect failed),
		// this nudges an immediate reconnect and waits briefly. Doing it
		// here (before injection) matters: inject fields and hidden-tool
		// rules are only readable from a registered tool schema, and the
		// call below would otherwise succeed with missing provenance.
		// Skipping the call when resolution already failed (server down
		// or tool genuinely unknown) also avoids paying the bounded
		// pending-wait twice for the same miss.
		serverName := resolveMCPServerForTool(cr.ctx, tc.Function.Name)
		if serverName == "" {
			toolMsg := toolResultMsg(tc.ID, fmt.Sprintf("error: unknown MCP tool: %s", tc.Function.Name))
			turn.Add(toolMsg)
			continue
		}
		if isMCPToolHidden(tc.Function.Name, serverName, hiddenMCPTools) {
			toolMsg := toolResultMsg(tc.ID, fmt.Sprintf("error: tool %q is hidden for this command", tc.Function.Name))
			turn.Add(toolMsg)
			continue
		}
		if verbose && !isToolHidden(tc.Function.Name, hiddenTools) && len(visibleTools) <= 1 {
			cr.sendIRC(expandNotice(getNotices().Tools.Call, map[string]string{"server": serverName, "tool": tc.Function.Name}))
		}
		var toolArgs map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &toolArgs); err != nil {
			toolMsg := toolResultMsg(tc.ID, "error: failed to parse tool arguments: "+err.Error())
			turn.Add(toolMsg)
			continue
		}
		injectScopeArgsFromRunner(toolArgs, tc.Function.Name, cr)
		result, err := callMCPToolWithContext(cr.ctx, tc.Function.Name, toolArgs)
		if err != nil {
			toolMsg := toolResultMsg(tc.ID, "error: "+err.Error())
			turn.Add(toolMsg)
			continue
		}
		toolText := mcpToolResultToText(result)
		cr.logger.Info("MCP tool result", "tool", tc.Function.Name, "result", toolText)
		toolMsg := toolResultMsg(tc.ID, toolText)
		turn.Add(toolMsg)
	}
	return registeredJob
}

func (cr *chatRunner) handleRegisterBackgroundJob(turn *turnContext, call ToolCall) {
	var args struct {
		JobID      string `json:"job_id"`
		ToolName   string `json:"tool_name"`
		ServerName string `json:"server_name"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		toolMsg := toolResultMsg(call.ID, "error: failed to parse arguments: "+err.Error())
		turn.Add(toolMsg)
		return
	}

	if args.JobID == "" || args.ToolName == "" {
		toolMsg := toolResultMsg(call.ID, "error: job_id and tool_name are required")
		turn.Add(toolMsg)
		return
	}

	if args.ServerName == "" {
		args.ServerName = getMCPServerForTool(args.ToolName)
	}
	if args.ServerName == "" {
		toolMsg := toolResultMsg(call.ID, "error: could not determine MCP server for tool "+args.ToolName)
		turn.Add(toolMsg)
		return
	}

	if asyncJobMgr.contains(args.JobID) {
		toolMsg := toolResultMsg(call.ID, "Job already registered and being monitored.")
		turn.Add(toolMsg)
		return
	}

	cr.logger.Info("registering background job", "job_id", args.JobID, "tool", args.ToolName, "server", args.ServerName)

	if cr.sessionID == 0 {
		toolMsg := toolResultMsg(call.ID, "error: no active session to register job against")
		turn.Add(toolMsg)
		return
	}

	if err := createPendingJob(cr.sessionID, args.JobID, args.ToolName, args.ServerName); err != nil {
		cr.logger.Error("failed to create pending job", "error", err)
		toolMsg := toolResultMsg(call.ID, "error: failed to register job: "+err.Error())
		turn.Add(toolMsg)
		return
	}

	job := registerAsyncJob(args.JobID, cr.sessionID, args.ToolName, args.ServerName, cr.network.Name, cr.channel, cr.nick, cr.userID)
	if job != nil {
		select {
		case resultText := <-job.payload.inlineResultCh:
			toolMsg := toolResultMsg(call.ID, resultText)
			turn.Add(toolMsg)
			return
		case <-time.After(500 * time.Millisecond):
		case <-cr.ctx.Done():
			toolMsg := toolResultMsg(call.ID, "error: context cancelled while waiting for job result")
			turn.Add(toolMsg)
			return
		}
	}

	toolMsg := toolResultMsg(call.ID, "Job registered. You will receive the result when it completes.")
	turn.Add(toolMsg)
}

func (cr *chatRunner) handleBanUser(turn *turnContext, call ToolCall) {
	var args struct {
		Reason   string `json:"reason"`
		Duration string `json:"duration"`
	}
	if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
		toolMsg := toolResultMsg(call.ID, fmt.Sprintf("error: failed to parse arguments: %s", err))
		turn.Add(toolMsg)
		return
	}

	if cr.userID == 0 {
		toolMsg := toolResultMsg(call.ID, "Cannot ban: no user associated with this conversation.")
		turn.Add(toolMsg)
		return
	}

	user, err := getUserByID(cr.userID)
	if err != nil {
		toolMsg := toolResultMsg(call.ID, fmt.Sprintf("Error looking up user: %s", err))
		turn.Add(toolMsg)
		return
	}
	if user == nil {
		toolMsg := toolResultMsg(call.ID, "Cannot ban: user not found.")
		turn.Add(toolMsg)
		return
	}

	var maxDur, defaultDur time.Duration
	readConfig(func() {
		var err error
		maxDur, err = parseBanDuration(config.Bans.MaxDuration)
		if err != nil {
			maxDur, _ = parseBanDuration("6h")
		}
		defaultDur, err = parseBanDuration(config.Bans.DefaultDuration)
		if err != nil {
			defaultDur, _ = parseBanDuration("5m")
		}
	})

	duration := defaultDur
	if args.Duration != "" {
		parsed, err := parseBanDuration(args.Duration)
		if err != nil {
			toolMsg := toolResultMsg(call.ID, fmt.Sprintf("Invalid duration format: %s. Use formats like '5m', '30m', '1h', '6h'.", args.Duration))
			turn.Add(toolMsg)
			return
		}
		duration = parsed
	}

	if duration > maxDur {
		duration = maxDur
	}

	bannerUserID := &cr.userID

	_, err = createBan(theDB, user.ID, cr.network.Name, cr.channel, "", args.Reason, duration, bannerUserID, cr.network.Nick+":"+cr.cfg.Name)
	if err != nil {
		toolMsg := toolResultMsg(call.ID, fmt.Sprintf("Failed to create ban: %s", err))
		turn.Add(toolMsg)
		return
	}

	accountInfo := ""
	if user.IRCAccount != "" {
		accountInfo = " (has account, strong ban)"
	}

	toolMsg := toolResultMsg(call.ID, fmt.Sprintf("User %s (id: %d) banned for %s: %s%s", displayNick(user), user.ID, formatDuration(duration), args.Reason, accountInfo))
	turn.Add(toolMsg)
}

func (cr *chatRunner) handleCheckBanHistory(turn *turnContext, call ToolCall) {
	if cr.userID == 0 {
		toolMsg := toolResultMsg(call.ID, "Cannot look up ban history: no user associated with this conversation.")
		turn.Add(toolMsg)
		return
	}

	user, err := getUserByID(cr.userID)
	if err != nil {
		toolMsg := toolResultMsg(call.ID, fmt.Sprintf("Error looking up user: %s", err))
		turn.Add(toolMsg)
		return
	}
	if user == nil {
		toolMsg := toolResultMsg(call.ID, "Cannot look up ban history: user not found.")
		turn.Add(toolMsg)
		return
	}

	bans, err := getBanHistory(theDB, user.ID)
	if err != nil {
		toolMsg := toolResultMsg(call.ID, fmt.Sprintf("Error fetching ban history: %s", err))
		turn.Add(toolMsg)
		return
	}

	if len(bans) == 0 {
		toolMsg := toolResultMsg(call.ID, fmt.Sprintf("User %s has no ban history.", displayNick(user)))
		turn.Add(toolMsg)
		return
	}

	var lines []string
	for _, b := range bans {
		status := "expired"
		if b.Active {
			status = "ACTIVE"
		}
		ago := formatDuration(time.Since(b.CreatedAt))
		lines = append(lines, fmt.Sprintf("#%d [%s] %s (%s) by %s, %s ago", b.ID, status, b.Reason, formatDuration(b.Duration), b.BannerNick, ago))
	}

	toolMsg := toolResultMsg(call.ID, fmt.Sprintf("Ban history for %s:\n%s", displayNick(user), strings.Join(lines, "\n")))
	turn.Add(toolMsg)
}

func (cr *chatRunner) runTurnResponses(turn *turnContext) bool {
	mu, _ := responseChainMu.LoadOrStore(fmt.Sprintf("%s%s%d", cr.network.Name, cr.channel, cr.userID), &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	responseTools := toolsToResponseToolParams(cr.getTools())

	ctx, cancel := context.WithTimeout(cr.ctx, cr.cfg.Timeout)
	defer cancel()

	session, _ := sessionMgr.GetSession(cr.sessionID)
	currentResponseID := ""
	if session != nil && session.ResponseID != nil {
		currentResponseID = *session.ResponseID
	}
	usePrevID := chainActive(cr.cfg, currentResponseID)
	// Layer 1 chain guard: never chain a stored response across a model
	// change. Cross-model previous_response_id either errors with wording
	// we may not recognize or silently drops the prior assistant history —
	// no error, so no recovery would be possible. NULL response_model means
	// "unknown" (legacy rows) and still chains, relying on the
	// isResponseIDError net.
	if usePrevID && session != nil && session.ResponseModel != nil && *session.ResponseModel != cr.cfg.Model {
		cr.logger.Info("response chain model change, sending full history",
			"chain_model", *session.ResponseModel, "cfg_model", cr.cfg.Model)
		currentResponseID = ""
		usePrevID = false
	}

	var input []responses.ResponseInputItemUnionParam
	if usePrevID {
		if len(turn.Messages()) > 0 {
			input = messagesToResponseInputItems(turn.LastN(1))
		}
	} else {
		input = messagesToResponseInputItems(turn.Messages())
	}

	emptyRetries := 0
	maxEmptyRetries := 0
	if cr.cfg.RetryOnEmpty != nil {
		maxEmptyRetries = *cr.cfg.RetryOnEmpty
	}

	iteration := 0

	for {
		iteration++
		if cr.checkIterationLimit(iteration) {
			return true
		}

		// The previous_response_id param and the input shape are two halves
		// of ONE decision: previous_response_id must be sent if and only if
		// the input is NOT the full history (we are relying on the server's
		// stored context). usePrevID tracks that decision at every site
		// that rebuilds input below. Passing currentResponseID through
		// unconditionally leaked chain ids into requests for commands with
		// previous_response_id disabled — OpenRouter's stateless Responses
		// proxy 400s on any previous_response_id (incident
		// 2026-10-09-084153, session 1806).
		prevID := ""
		if usePrevID {
			prevID = currentResponseID
		}
		params := buildResponseParams(cr.cfg, input, responseTools, prevID, cr.apiIdentity(true))

		if cr.cfg.Streaming {
			r := cr.runTurnResponsesStream(ctx, params, turn, iteration, emptyRetries, maxEmptyRetries, currentResponseID, usePrevID)
			currentResponseID = r.currentResponseID
			input = r.input
			emptyRetries = r.emptyRetries
			usePrevID = r.usePrevID
			if !r.done {
				continue
			}
			return true
		}

		cr.transport.setCaptureBody(true)
		apiStart := time.Now()
		resp, err := cr.openaiClient.Responses.New(ctx, params)
		cr.transport.setCaptureBody(false)
		if err != nil {
			if newInput, newResponseID, retry := cr.shouldRetryWithoutResponseID(usePrevID, err, turn.Messages(), iteration, "responses", currentResponseID); retry {
				input, currentResponseID = newInput, newResponseID
				usePrevID = false
				continue
			}
			cr.sendError(err.Error())
			cr.logger.Error(err.Error())
			cr.logAPIIncident(err, turn.Messages(), iteration, "responses")
			return true
		}
		durationMs := int(time.Since(apiStart).Milliseconds())

		text, reasoning, toolCalls := parseSDKResponseOutput(*resp)
		currentResponseID = cr.handleResponseIDSave(resp.ID, text, toolCalls, currentResponseID)

		if len(toolCalls) == 0 {
			content := text
			if retry, newContent := cr.checkEmptyRetry(content, reasoning, emptyRetries, maxEmptyRetries); retry {
				cr.addEmptyResponseCorrection(turn, reasoning)
				emptyRetries++
				// Rebuild the input from the turn so the correction
				// rides along, and drop the response chain for the
				// retry: handleResponseIDSave already cleared the
				// stored response_id (empty output), and keeping the
				// old chain head alongside a full-history resend
				// would duplicate context server-side. A successful
				// retry starts a fresh chain.
				input = messagesToResponseInputItems(turn.Messages())
				currentResponseID = ""
				usePrevID = false
				continue
			} else {
				content = newContent
			}
			assistantMsg := ChatMessage{
				Role:             RoleAssistant,
				Content:          content,
				ReasoningContent: reasoning,
			}
			turn.Add(assistantMsg)
			out := FormatOutput(content)
			cr.logger.Info(out)

			cr.storeUsage(sdkResponseUsageToUsage(resp.Usage, string(resp.Status)), "responses", durationMs)
			if reasoning != "" {
				cr.logger.Info("reasoning", "content", reasoning)
			}

			cr.sendFinalText(content)
			return true
		}

		emptyRetries = 0
		cr.storeUsage(sdkResponseUsageToUsage(resp.Usage, string(resp.Status)), "responses", durationMs)
		numToolCalls := len(toolCalls)
		cr.handleToolCallResponse(turn, text, toolCalls, reasoning)
		if cr.responded {
			// respond delivered the final answer — the turn is complete,
			// no further API round-trip (skip the chain rebuild too: the
			// ephemeral turn is over and nothing chains from it).
			return true
		}

		if chainActive(cr.cfg, currentResponseID) {
			toolResultMsgs := turn.LastN(numToolCalls)
			input = toolResultMsgsToInputItems(toolResultMsgs)
			// Input is now delta-only (tool results): the next request must
			// chain currentResponseID or the provider loses all context.
			usePrevID = true
		} else {
			input = messagesToResponseInputItems(turn.Messages())
			// Full-history resend: the chain id must be dropped to match.
			usePrevID = false
		}
	}
}

func (cr *chatRunner) callResponsesStream(ctx context.Context, params responses.ResponseNewParams) (*responses.Response, error) {
	stream := cr.openaiClient.Responses.NewStreaming(ctx, params)

	type recvResult struct {
		event responses.ResponseStreamEventUnion
		done  bool
		err   error
	}

	var fullText string
	var reasoningBuffer string
	var completedResponse *responses.Response
	var sOut streamOutput
	// Ephemeral (generator) turns never render live: emission is deferred
	// to the caller's classification of the iteration (final text via
	// sendFinalText in runTurnResponsesStream; stream-death text via
	// sendFinalText on the error/timeout branches below; intermediate
	// text never).
	if cr.cfg.RenderMarkdown && !cr.ephemeral {
		sOut.renderer = markdowntoirc.NewStreamingRenderer()
	}

	idleTimer := time.NewTimer(cr.cfg.StreamTimeout)
	defer idleTimer.Stop()

	for {
		if cr.ctx.Err() != nil {
			stream.Close()
			return completedResponse, nil
		}

		ch := make(chan recvResult, 1)
		go func() {
			if stream.Next() {
				ch <- recvResult{event: stream.Current()}
			} else {
				err := stream.Err()
				if err == nil {
					ch <- recvResult{done: true}
				} else {
					ch <- recvResult{err: err, done: true}
				}
			}
		}()

		select {
		case res := <-ch:
			if res.done {
				if res.err != nil {
					stream.Close()
					// Deliver the held tail before the error notice.
					if cr.ephemeral {
						// Deferred emission: deliver what arrived (parity
						// with the non-ephemeral flush), complete.
						cr.sendFinalText(fullText)
					} else {
						sOut.Flush(cr.sendIRC)
					}
					return nil, res.err
				}
				cr.logger.Info("Responses stream completed")
				stream.Close()
				goto streamDone
			}
			idleTimer.Reset(cr.cfg.StreamTimeout)

			if apiLogger != nil {
				if raw, err := json.Marshal(res.event); err == nil {
					apiLogger.LogStreamChunk(cr.transport.sessionID, raw)
				}
			}

			event := res.event
			switch event.Type {
			case "response.output_text.delta":
				textDelta := event.Delta
				fullText += textDelta
				// Ephemeral (generator) turns accumulate without emitting —
				// finality is unknowable mid-stream, so emission waits for
				// the caller's classification.
				if !cr.ephemeral {
					sOut.HandleDelta(textDelta, cr.sendIRC)
				}

			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				reasoningBuffer += event.Delta

			case "response.function_call_arguments.delta":
				// accumulated via response.completed

			case "response.output_item.done":
				// Tool call notifications handled by executeToolCalls batch logic
			case "response.completed":
				completedResponse = &event.Response
			}

		case <-idleTimer.C:
			stream.Close()
			// Deliver the held tail before the timeout error.
			if cr.ephemeral {
				// Deferred emission: deliver what arrived (parity with
				// the non-ephemeral flush), complete.
				cr.sendFinalText(fullText)
			} else {
				sOut.Flush(cr.sendIRC)
			}
			return nil, fmt.Errorf("responses stream timed out (no data received)")
		}
	}

streamDone:
	// Flush before the completed-response check: the text is real even when
	// the terminal event never arrived, and the historical ordering dropped
	// it on that error path. Ephemeral (generator) turns never flush here —
	// the CALLER classifies the iteration: the final no-tool-call iteration
	// sends the text via sendFinalText (runTurnResponsesStream), and a
	// tool-call iteration is suppressed (intermediate). The one shape the
	// caller cannot classify is a stream that ended without its terminal
	// event — the completedResponse == nil branch below delivers that text
	// before returning the error.
	if !cr.ephemeral {
		sOut.Flush(cr.sendIRC)
	}

	if completedResponse == nil {
		if cr.ephemeral {
			// Deferred emission parity: the stream died without its terminal
			// event — the caller will error out; deliver what arrived first
			// (the non-ephemeral arm flushed just above).
			cr.sendFinalText(fullText)
		}
		return nil, fmt.Errorf("responses stream ended without response.completed event")
	}

	logStreamCompletion(cr.transport.sessionID, cr.cfg.Model, fullText, reasoningBuffer, nil, sdkResponseUsageToUsage(completedResponse.Usage, string(completedResponse.Status)), RoleAssistant)

	cr.logger.Info(FormatOutput(fullText))
	if reasoningBuffer != "" {
		cr.logger.Info("reasoning", "content", reasoningBuffer)
	}

	return completedResponse, nil
}

func chat(network Network, c *girc.Client, e girc.Event, cfg AIConfig, ctx context.Context, output chan<- string, resolvedUser *User, args ...string) {
	runner := newChatRunnerFn(network, c, cfg, ctx, output).(*chatRunner)
	nick := e.Source.Name
	channel := normalizeIRC(e.Params[0], getCasemapping(network.Name))
	if resolvedUser == nil {
		resolvedUser = resolvedUserFromCtx(ctx)
	}
	if resolvedUser == nil {
		var err error
		resolvedUser, err = resolveIRCUser(network, c, e)
		if err != nil {
			runner.logger.Error("failed to resolve user in chat()", "error", err)
		}
		proceed, _ := handleResolveResult(c, e, resolvedUser, err)
		if !proceed {
			return
		}
	}
	if resolvedUser == nil {
		runner.logger.Warn("chat() got nil resolved user, dropping message", "nick", nick)
		return
	}
	if resolvedUser.Flagged {
		// Flagged user: handleResolveResult already sent the persistent
		// notice on the original resolution path. Still continue here so the
		// LLM responds to them.
		runner.logger.Warn("chat() proceeding with flagged user",
			"user_id", resolvedUser.ID, "nick", nick, "reason", resolvedUser.FlaggedReason)
	}
	userID := resolvedUser.ID
	runner.userID = userID
	runner.hostmask = e.Source.Name + "!" + e.Source.Ident + "@" + e.Source.Host
	runner.setChannel(channel, nick, userID)

	session, err := sessionMgr.GetActiveSession(network.Name, channel, userID)
	if err != nil {
		runner.logger.Error("failed to get active session", "error", err)
		return
	}
	if session == nil {
		var systemContent string
		if cfg.SystemTmpl != nil {
			data := buildSystemPromptData(network, c, channel, nick)
			data.AsyncResultRole = asyncResultRole(cfg)

			var buf strings.Builder
			err := cfg.SystemTmpl.Execute(&buf, data)
			if err != nil {
				runner.logger.Error("system prompt template execution error:", err)
				systemContent = cfg.System
			} else {
				systemContent = buf.String()
			}
		} else {
			systemContent = cfg.System
		}

		mu := getSessionCreationLock(network.Name, channel, userID)
		mu.Lock()
		sid, err := sessionMgr.CreateSession(network.Name, channel, userID, cfg.Name, cfg.Service, cfg.Model)
		if err != nil {
			mu.Unlock()
			runner.logger.Error("Failed to create session", "error", err)
			return
		}
		if _, err := sessionMgr.CreateSessionSettings(sid, cfg); err != nil {
			runner.logger.Warn("failed to create session settings", "error", err)
		}
		sessionMgr.AddMessage(sid, ChatMessage{
			Role:    RoleSystem,
			Content: systemContent,
		})
		apiLogger.RestoreSession(sid, network.Name, channel, userID)
		session, err = sessionMgr.GetSession(sid)
		mu.Unlock()
		if err != nil || session == nil {
			runner.logger.Error("Failed to get session after creation", "id", sid, "error", err)
			return
		}
	}

	var userMsg ChatMessage
	if cfg.DetectImages {
		messages, err := sessionMgr.GetMessages(session.ID)
		if err != nil {
			runner.logger.Error("failed to load messages for image detection", "error", err)
			messages = nil
		}

		cleanText, imageUrls := detectImageURLs(args[0])

		if len(imageUrls) > 0 {
			existingImages := countContextImages(messages)
			remaining := cfg.MaxContextImages - existingImages
			n := getNotices()

			if remaining <= 0 {
				runner.sendWarning(expandNotice(n.Images.LimitReached, map[string]string{"max": fmt.Sprintf("%d", cfg.MaxContextImages)}))
				return
			}

			if len(imageUrls) > remaining {
				runner.sendWarning(expandNotice(n.Images.PartialLimit, map[string]string{"remaining": fmt.Sprintf("%d", remaining), "used": fmt.Sprintf("%d", existingImages), "max": fmt.Sprintf("%d", cfg.MaxContextImages)}))
				return
			}

			var err error
			var maxImagePixels int
			readConfig(func() { maxImagePixels = config.MaxImagePixels })
			userMsg, err = buildImageMessage(cleanText, imageUrls, cfg.MaxImages, cfg.ImageFormat, cfg.ImageQuality, cfg.MaxImageWidth, cfg.MaxImageHeight, maxImagePixels)
			if err != nil {
				runner.sendError(expandNotice(n.DB.ProcessImages, map[string]string{"error": err.Error()}))
				return
			}
		} else {
			userMsg = ChatMessage{
				Role:    RoleUser,
				Content: cleanText,
			}
		}
	} else {
		userMsg = ChatMessage{
			Role:    RoleUser,
			Content: args[0],
		}
	}

	sessionMgr.AddMessage(session.ID, userMsg)

	runner.sessionID = session.ID
	if session.ConvID != nil {
		runner.convID = *session.ConvID
	}

	apiLogger.RestoreSession(session.ID, network.Name, channel, userID)
	runner.syncAPISessionID()
	runner.syncConvID()

	messages, err := sessionMgr.GetMessages(runner.sessionID)
	if err != nil {
		runner.logger.Error("failed to load messages", "error", err)
		runner.sendError("failed to load conversation history")
		return
	}
	runner.logger.Debug("running completion", "summary", summarizeMessages(messages))

	turn := newTurnContext(runner.sessionID, messages)
	runner.runTurn(turn)
	runner.logger.Debug("completion finished", "api_log", apiLogger.GetSessionFilePath(runner.apiLogID()))

	if theDB != nil && sessionMgr.IsSessionActive(runner.sessionID) {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			completedJobs, err := getCompletedPendingJobs(runner.sessionID)
			if err != nil || len(completedJobs) == 0 {
				break
			}
			for _, cj := range completedJobs {
				injectAsyncResultFromDB(runner.sessionID, cfg, cj, network.Name, channel, e.Source.Name)
				markPendingJobDelivered(cj.JobID)
			}
			messages, err = sessionMgr.GetMessages(runner.sessionID)
			if err != nil {
				runner.logger.Error("failed to reload messages after job delivery", "error", err)
				break
			}
			turn = newTurnContext(runner.sessionID, messages)
			runner.runTurn(turn)
			// Same api_log pointer the primary turn logs — continuation
			// turns (async job delivery) write to the same api log file
			// and deserve the same breadcrumb.
			runner.logger.Debug("continuation finished", "api_log", apiLogger.GetSessionFilePath(runner.sessionID))
		}
	}

	maybeAutoCompact(runner, cfg, network, c, channel, e.Source.Name)
}

func FormatOutput(text string) string {
	out := text
	out = strings.ReplaceAll(out, "\x03", "\x1b[033m[C]\x1b[0m")
	out = strings.ReplaceAll(out, "\x02", "\x1b[034m[B]\x1b[0m")
	out = strings.ReplaceAll(out, "\x1F", "\x1b[035m[U]\x1b[0m")
	out = strings.ReplaceAll(out, "\x1D", "\x1b[036m[I]\x1b[0m")
	return out
}

func ExtractFinalText(text string) string {
	cut := strings.LastIndex(text, "</think>\n")
	if cut > -1 {
		cut += len("</think>\n")
		return text[cut:]
	}
	return text
}

const backgroundJobToolName = "register_background_job"

type builtinToolHandler func(cr *chatRunner, turn *turnContext, call ToolCall) bool

type builtinToolEntry struct {
	handler builtinToolHandler
	def     Tool
}

var builtinTools = map[string]builtinToolEntry{
	backgroundJobToolName: {
		handler: func(cr *chatRunner, turn *turnContext, call ToolCall) bool {
			cr.handleRegisterBackgroundJob(turn, call)
			return true
		},
		def: Tool{
			Type: "function",
			Function: &FunctionDefinition{
				Name:        backgroundJobToolName,
				Description: "Register a background job for monitoring. When an async tool (e.g. generate_image_async) returns a job_id, call this to have the system monitor the job. You will be notified with the result when it completes. Do not poll or wait for results. Continue the conversation normally.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"job_id": map[string]any{
							"type":        "string",
							"description": "The job_id returned by the async tool",
						},
						"tool_name": map[string]any{
							"type":        "string",
							"description": "Name of the async tool that started the job (e.g. 'generate_image_async')",
						},
						"server_name": map[string]any{
							"type":        "string",
							"description": "Name of the MCP server running the job (auto-detected from tool_name if omitted)",
						},
					},
					"required": []string{"job_id", "tool_name"},
				},
			},
		},
	},
	"ban_user": {
		handler: func(cr *chatRunner, turn *turnContext, call ToolCall) bool {
			cr.handleBanUser(turn, call)
			return false
		},
		def: Tool{
			Type: "function",
			Function: &FunctionDefinition{
				Name:        "ban_user",
				Description: "Ban the current user from using bot commands. They will be ignored until the ban expires. Use this when someone is being abusive, spamming, or otherwise disrupting the channel.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"reason": map[string]any{
							"type":        "string",
							"description": "Why the user is being banned",
						},
						"duration": map[string]any{
							"type":        "string",
							"description": "Duration of the ban (e.g. '5m', '30m', '1h', '6h'). Default: 5m. Maximum: 6h.",
						},
					},
					"required": []string{"reason"},
				},
			},
		},
	},
	"check_ban_history": {
		handler: func(cr *chatRunner, turn *turnContext, call ToolCall) bool {
			cr.handleCheckBanHistory(turn, call)
			return false
		},
		def: Tool{
			Type: "function",
			Function: &FunctionDefinition{
				Name:        "check_ban_history",
				Description: "View ban history for the current user. Shows active and past bans with reasons, durations, and how long ago they were issued.",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
					"required":   []string{},
				},
			},
		},
	},
}

func isToolDisabled(toolName string, disabled []string) bool {
	for _, d := range disabled {
		if d == toolName {
			return true
		}
	}
	return false
}

func isToolHidden(toolName string, hidden []string) bool {
	for _, h := range hidden {
		if h == toolName {
			return true
		}
	}
	return false
}

func getToolServerName(toolName string) string {
	if _, ok := generatorTools[toolName]; ok {
		return "builtin"
	}
	if _, ok := builtinTools[toolName]; ok {
		return "builtin"
	}
	return getMCPServerForTool(toolName)
}

// getBuiltinToolDefs assembles the builtin tool definitions for a turn run
// under cfg. Like ban_user's duration description (live config values), the
// register_background_job description is rewritten per-config: it tells the
// model the wire role its background results will arrive under (guidance
// Knob 1), so a command configured with injection_role = "user" promises a
// user message instead of the default system message.
func getBuiltinToolDefs(cfg AIConfig, disabled []string) []Tool {
	tools := make([]Tool, 0, len(builtinTools))

	var banMaxDur, banDefaultDur string
	readConfig(func() {
		banMaxDur = config.Bans.MaxDuration
		banDefaultDur = config.Bans.DefaultDuration
	})

disabledLoop:
	for _, entry := range builtinTools {
		t := entry.def
		for _, d := range disabled {
			if t.Function.Name == d {
				continue disabledLoop
			}
		}
		if t.Function.Name == backgroundJobToolName {
			jobDef := *t.Function
			if asyncResultDelivery(cfg) == asyncDeliveryTool {
				// Tool-delivery mode (spec addendum): state the delivery
				// fact passively — meta-instructions ("respond to it as
				// you would any tool result") invite tool-call reasoning.
				jobDef.Description = "Register a background job for monitoring. When an async tool (e.g. generate_image_async) returns a job_id, call this to have the system monitor the job. The result will be delivered as a tool response when it completes. Do not poll or wait for results. Continue the conversation normally."
			} else {
				jobDef.Description = fmt.Sprintf(
					"Register a background job for monitoring. When an async tool (e.g. generate_image_async) returns a job_id, call this to have the system monitor the job. You will be notified with the result in a %s message when it completes. Do not poll or wait for results. Continue the conversation normally.",
					guidanceRole(cfg))
			}
			t.Function = &jobDef
		}
		if t.Function.Name == "ban_user" {
			banDef := *t.Function
			paramsMap, _ := t.Function.Parameters.(map[string]any)
			newParams := make(map[string]any, len(paramsMap))
			for k, v := range paramsMap {
				newParams[k] = v
			}
			propsMap, _ := paramsMap["properties"].(map[string]any)
			newProps := make(map[string]any, len(propsMap))
			for k, v := range propsMap {
				newProps[k] = v
			}
			durationDesc := fmt.Sprintf("Duration of the ban (e.g. '5m', '30m', '1h', '6h'). Default: %s. Maximum: %s.", banDefaultDur, banMaxDur)
			newProps["duration"] = map[string]any{
				"type":        "string",
				"description": durationDesc,
			}
			newParams["properties"] = newProps
			banDef.Parameters = newParams
			t.Function = &banDef
		}
		tools = append(tools, t)
	}
	return tools
}

func logStreamCompletion(sessionID int64, model, content, reasoning string, toolCalls []ToolCall, usage *Usage, role string) {
	if apiLogger == nil {
		return
	}
	if role == "" {
		role = RoleAssistant
	}
	msg := map[string]any{
		"choices": []map[string]any{
			{
				"message": map[string]any{
					"role":    role,
					"content": content,
				},
				"finish_reason": "stop",
			},
		},
		"model": model,
	}
	if reasoning != "" {
		msg["choices"].([]map[string]any)[0]["message"].(map[string]any)["reasoning_content"] = reasoning
	}
	if len(toolCalls) > 0 {
		msg["choices"].([]map[string]any)[0]["tool_calls"] = toolCalls
		msg["choices"].([]map[string]any)[0]["finish_reason"] = "tool_calls"
	}
	if usage != nil {
		msg["usage"] = usage
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return
	}
	apiLogger.LogStreamResponse(sessionID, body)
}
