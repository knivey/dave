package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
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

	logger := logxi.New(network.Name + ".completion." + cfg.Name)
	logger.SetLevel(logxi.LevelAll)

	apiCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	resp, err := aiClient.Completions.New(apiCtx, openai.CompletionNewParams{
		Model: openai.CompletionNewParamsModel(cfg.Model),
		Prompt: openai.CompletionNewParamsPromptUnion{
			OfString: openai.String(args[0]),
		},
		MaxTokens:   openai.Int(int64(cfg.MaxTokens)),
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
	ctxKey       string
	logger       logxi.Logger
	ctx          context.Context
	outputCh     chan<- string
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

	ctxKey := network.Name + client.GetNick()
	logger := logxi.New(network.Name + ".completion." + cfg.Name)
	logger.SetLevel(logxi.LevelAll)

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
		ctxKey:       ctxKey,
	}
}

func isGrokService(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Hostname()), ".x.ai")
}

func (cr *chatRunner) setChannel(channel, nick string) {
	cr.channel = channel
	cr.nick = nick
	cr.ctxKey = cr.network.Name + channel + nick
	cr.syncAPISessionID()
	cr.syncConvID()
}

func (cr *chatRunner) syncAPISessionID() {
	sessionID := GetContext(cr.ctxKey).SessionID
	cr.transport.setAPILogger(apiLogger, sessionID)
}

func (cr *chatRunner) syncConvID() {
	ctx := GetContext(cr.ctxKey)
	if ctx.ConvID != "" {
		cr.transport.setExtraHeaders(map[string]string{"x-grok-conv-id": ctx.ConvID})
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

func (cr *chatRunner) runTurn(messages []ChatMessage) ([]ChatMessage, bool) {
	if cr.cfg.ResponsesAPI {
		return cr.runTurnResponses(messages)
	}
	mcpTools := getMCPTools(cr.cfg.MCPs)
	if len(mcpTools) > 0 {
		mcpTools = append(mcpTools, registerBackgroundJobTool())
	}

	ctx, cancel := context.WithTimeout(cr.ctx, cr.cfg.Timeout)
	defer cancel()

	const maxToolIterations = 20
	iterations := 0

	for {
		if iterations >= maxToolIterations {
			cr.sendIRC("\x0308⚠️ Tool call limit reached, stopping.\x0F")
			cr.logger.Warn("max tool iterations reached", "limit", maxToolIterations)
			return messages, true
		}
		iterations++

		params := buildChatCompletionParams(cr.cfg, messages, mcpTools)

		if cr.cfg.Streaming {
			stream := cr.openaiClient.Chat.Completions.NewStreaming(ctx, params)

			bufferb := ""
			fullContent := ""
			reasoningBuffer := ""
			logBuf := strings.Builder{}
			var streamingRenderer *markdowntoirc.StreamingRenderer
			if cr.cfg.RenderMarkdown {
				streamingRenderer = markdowntoirc.NewStreamingRenderer()
			}

			var accumulatedToolCalls []ToolCall
			var assistantRole string
			var streamUsage *Usage
			var streamTimings *Timings
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
					return messages, true
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
							cr.sendError(res.err.Error())
							cr.logger.Error(res.err.Error())
							cr.logAPIIncident(res.err, messages, iterations, "chat_completions_stream")
							return messages, true
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
						if f, ok := chunk.Choices[0].Delta.JSON.ExtraFields["reasoning_content"]; ok && f.Valid() {
							var rc string
							json.Unmarshal([]byte(f.Raw()), &rc)
							chunkReasoning = rc
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
					bufferb += textDelta
					fullContent += textDelta
					if streamingRenderer != nil {
						for _, line := range streamingRenderer.Process(textDelta) {
							logBuf.WriteString(line)
							logBuf.WriteString("\n")
							cr.sendIRC(line)
						}
					} else {
						if strings.Contains(bufferb, "\n") {
							logBuf.WriteString(bufferb)
							cr.sendIRC(bufferb)
							bufferb = ""
						}
					}

					if choice.FinishReason == "tool_calls" {
						cr.logger.Info("stream finished with tool calls")
						stream.Close()
						break StreamLoop
					}
					if choice.FinishReason == "stop" || choice.FinishReason == "length" {
						streamDone = true
						stream.Close()
						break StreamLoop
					}

				case <-idleTimer.C:
					stream.Close()
					timeoutErr := fmt.Errorf("stream timed out (no data received): timeout=%s", cr.cfg.StreamTimeout)
					cr.sendError(timeoutErr.Error())
					cr.logger.Error("stream idle timeout exceeded", "timeout", cr.cfg.StreamTimeout)
					cr.logAPIIncident(timeoutErr, messages, iterations, "chat_completions_stream")
					return messages, true
				}
			}

			logStreamCompletion(cr.transport.sessionID, streamModel, fullContent, reasoningBuffer, accumulatedToolCalls, streamUsage, assistantRole)

			flushStreamedOutput := func() {
				cr.logger.Info(fullContent)
				content := fullContent
				if content == "" && reasoningBuffer == "" {
					content = "..."
				}
				AddContext(cr.cfg, cr.ctxKey, ChatMessage{
					Role:             RoleAssistant,
					Content:          content,
					ReasoningContent: reasoningBuffer,
				}, cr.network.Name, cr.channel, cr.nick)
				if streamingRenderer != nil {
					for _, line := range streamingRenderer.Process("") {
						logBuf.WriteString(line)
						logBuf.WriteString("\n")
						cr.sendIRC(line)
					}
				} else if bufferb != "" {
					logBuf.WriteString(bufferb)
					cr.sendIRC(bufferb)
				}
				cr.logger.Info("output", "text", logBuf.String())
				logUsage(cr.logger, streamUsage)
				logTimings(cr.logger, streamTimings)
				if reasoningBuffer != "" {
					cr.logger.Info("reasoning", "content", reasoningBuffer)
				}
			}

			if streamDone || len(accumulatedToolCalls) == 0 {
				flushStreamedOutput()
				return messages, true
			}

			cr.logger.Info("stream made tool calls", "count", len(accumulatedToolCalls))
			logUsage(cr.logger, streamUsage)
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
			messages = append(messages, assistantMsg)

			if bufferb != "" {
				text := ExtractFinalText(bufferb)
				if cr.cfg.RenderMarkdown {
					text = markdowntoirc.MarkdownToIRC(text)
				}
				cr.sendIRC(text)
			}

			AddContext(cr.cfg, cr.ctxKey, assistantMsg, cr.network.Name, cr.channel, cr.nick)

			messages, _ = cr.executeToolCalls(messages, accumulatedToolCalls)
			continue
		}

		cr.transport.setCaptureBody(true)
		resp, err := cr.openaiClient.Chat.Completions.New(ctx, params)
		cr.transport.setCaptureBody(false)
		if err != nil {
			cr.sendError(err.Error())
			cr.logger.Error(err.Error())
			cr.logAPIIncident(err, messages, iterations, "chat_completions")
			return messages, true
		}

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
			if content == "" && reasoning == "" {
				content = "..."
			}
			AddContext(cr.cfg, cr.ctxKey, ChatMessage{
				Role:             RoleAssistant,
				Content:          content,
				ReasoningContent: reasoning,
			}, cr.network.Name, cr.channel, cr.nick)
			out := FormatOutput(content)
			cr.logger.Info(out)
			text := ExtractFinalText(content)

			logUsage(cr.logger, usage)
			logTimings(cr.logger, nonStreamTimings)
			if reasoning != "" {
				cr.logger.Info("reasoning", "content", reasoning)
			}

			if text != "" && text != "..." {
				rawText := text
				if cr.cfg.RenderMarkdown {
					text = markdowntoirc.MarkdownToIRC(text)
				}
				chCfg := cr.network.GetChannelConfig(cr.channel)
				if chCfg.Pastebin {
					lines := wrapForIRC(text)
					if len(lines) >= chCfg.GetMaxLines() {
						url, err := uploadToPastebin(rawText)
						if err != nil {
							cr.sendIRC(errorMsg("pastebin: " + err.Error()))
							preview := chCfg.GetMaxLines()
							if preview > len(lines) {
								preview = len(lines)
							}
							for i := 0; i < preview; i++ {
								cr.sendIRC(lines[i])
							}
							cr.sendIRC("... (full output could not be pasted)")
						} else {
							preview := 3
							if preview > len(lines) {
								preview = len(lines)
							}
							for i := 0; i < preview; i++ {
								cr.sendIRC(lines[i])
							}
							cr.sendIRC(fmt.Sprintf("... ( full output: %s )", url))
						}
						return messages, true
					}
				}
				cr.sendIRC(text)
			}
			return messages, true
		}

		cr.logger.Info("assistant made tool calls", "count", len(toolCalls))
		logUsage(cr.logger, usage)
		logTimings(cr.logger, nonStreamTimings)

		assistantMsg := ChatMessage{
			Role:             RoleAssistant,
			Content:          content,
			ReasoningContent: reasoning,
			ToolCalls:        toolCalls,
		}
		messages = append(messages, assistantMsg)

		AddContext(cr.cfg, cr.ctxKey, assistantMsg, cr.network.Name, cr.channel, cr.nick)
		if content != "" {
			text := ExtractFinalText(content)
			if cr.cfg.RenderMarkdown {
				text = markdowntoirc.MarkdownToIRC(text)
			}
			cr.sendIRC(text)
		}
		if reasoning != "" {
			cr.logger.Info("reasoning", "content", reasoning)
		}

		messages, _ = cr.executeToolCalls(messages, toolCalls)
	}
}

func (cr *chatRunner) executeToolCalls(messages []ChatMessage, toolCalls []ToolCall) ([]ChatMessage, bool) {
	registeredJob := false
	for _, tc := range toolCalls {
		if tc.Function.Name == backgroundJobToolName {
			messages = cr.handleRegisterBackgroundJob(messages, tc)
			registeredJob = true
			continue
		}
		if cr.cfg.ToolVerbose == nil || *cr.cfg.ToolVerbose {
			serverName := getMCPServerForTool(tc.Function.Name)
			cr.sendIRC(fmt.Sprintf("\x0315🔧 ToolCall: %s > %s", serverName, tc.Function.Name))
		}
		var toolArgs map[string]any
		json.Unmarshal([]byte(tc.Function.Arguments), &toolArgs)
		result, err := callMCPToolWithContext(cr.ctx, tc.Function.Name, toolArgs)
		if err != nil {
			toolMsg := ChatMessage{
				Role:       RoleTool,
				Content:    "error: " + err.Error(),
				ToolCallID: tc.ID,
			}
			messages = append(messages, toolMsg)
			AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
			continue
		}
		toolText := mcpToolResultToText(result)
		cr.logger.Info("MCP tool result", "tool", tc.Function.Name, "result", toolText)
		toolMsg := ChatMessage{
			Role:       RoleTool,
			Content:    toolText,
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
	}
	return messages, registeredJob
}

func (cr *chatRunner) handleRegisterBackgroundJob(messages []ChatMessage, tc ToolCall) []ChatMessage {
	var args struct {
		JobID      string `json:"job_id"`
		ToolName   string `json:"tool_name"`
		ServerName string `json:"server_name"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		toolMsg := ChatMessage{
			Role:       RoleTool,
			Content:    "error: failed to parse arguments: " + err.Error(),
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
		return messages
	}

	if args.JobID == "" || args.ToolName == "" || args.ServerName == "" {
		toolMsg := ChatMessage{
			Role:       RoleTool,
			Content:    "error: job_id, tool_name, and server_name are required",
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
		return messages
	}

	jobMgr.mu.Lock()
	_, alreadyRegistered := jobMgr.jobs[args.JobID]
	jobMgr.mu.Unlock()
	if alreadyRegistered {
		toolMsg := ChatMessage{
			Role:       RoleTool,
			Content:    "Job already registered and being monitored.",
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
		return messages
	}

	cr.logger.Info("registering background job", "job_id", args.JobID, "tool", args.ToolName, "server", args.ServerName)

	chatContextsMutex.Lock()
	ctx := chatContextsMap[cr.ctxKey]
	sessionID := ctx.SessionID
	chatContextsMutex.Unlock()

	if sessionID == 0 {
		toolMsg := ChatMessage{
			Role:       RoleTool,
			Content:    "error: no active session to register job against",
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
		return messages
	}

	if err := createPendingJob(sessionID, args.JobID, args.ToolName, args.ServerName); err != nil {
		cr.logger.Error("failed to create pending job", "error", err)
		toolMsg := ChatMessage{
			Role:       RoleTool,
			Content:    "error: failed to register job: " + err.Error(),
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
		return messages
	}

	registerAsyncJob(args.JobID, sessionID, cr.ctxKey, args.ToolName, args.ServerName, cr.network.Name, cr.channel, cr.nick)

	toolMsg := ChatMessage{
		Role:       RoleTool,
		Content:    "Job registered. You will receive the result when it completes.",
		ToolCallID: tc.ID,
	}
	messages = append(messages, toolMsg)
	AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
	return messages
}

func (cr *chatRunner) runTurnResponses(messages []ChatMessage) ([]ChatMessage, bool) {
	mu, _ := responseChainMu.LoadOrStore(cr.ctxKey, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	mcpTools := getMCPTools(cr.cfg.MCPs)
	if len(mcpTools) > 0 {
		mcpTools = append(mcpTools, registerBackgroundJobTool())
	}
	responseTools := toolsToResponseToolParams(mcpTools)

	ctx, cancel := context.WithTimeout(cr.ctx, cr.cfg.Timeout)
	defer cancel()

	chatCtx := GetContext(cr.ctxKey)
	currentResponseID := chatCtx.ResponseID
	usePrevID := cr.cfg.PreviousResponseID && currentResponseID != ""

	var input []responses.ResponseInputItemUnionParam
	if usePrevID {
		if len(messages) > 0 {
			input = messagesToResponseInputItems(messages[len(messages)-1:])
		}
	} else {
		input = messagesToResponseInputItems(messages)
	}

	const maxToolIterations = 20

	for iteration := 1; iteration <= maxToolIterations; iteration++ {
		params := buildResponseParams(cr.cfg, input, responseTools, currentResponseID)

		if cr.cfg.Streaming {
			resp, err := cr.callResponsesStream(ctx, params)
			if err != nil {
				if usePrevID && isResponseIDError(err) && iteration == 1 {
					cr.logger.Warn("previous_response_id invalid, retrying without", "response_id", currentResponseID, "error", err)
					currentResponseID = ""
					SetContextResponseID(cr.ctxKey, "")
					usePrevID = false
					input = messagesToResponseInputItems(messages)
					iteration--
					continue
				}
				cr.sendError(err.Error())
				cr.logger.Error(err.Error())
				cr.logAPIIncident(err, messages, iteration, "responses_stream")
				return messages, true
			}

			if resp == nil {
				return messages, true
			}
			if resp.ID != "" {
				currentResponseID = resp.ID
				SetContextResponseID(cr.ctxKey, resp.ID)
			}
			text, reasoning, toolCalls := parseSDKResponseOutput(*resp)

			assistantMsg := ChatMessage{
				Role:             RoleAssistant,
				Content:          text,
				ReasoningContent: reasoning,
			}

			if len(toolCalls) == 0 {
				if text == "" && reasoning == "" {
					text = "..."
					assistantMsg.Content = text
				}
				messages = append(messages, assistantMsg)
				AddContext(cr.cfg, cr.ctxKey, assistantMsg, cr.network.Name, cr.channel, cr.nick)
				cr.logger.Info(FormatOutput(text))
				logUsage(cr.logger, sdkResponseUsageToUsage(resp.Usage))
				if reasoning != "" {
					cr.logger.Info("reasoning", "content", reasoning)
				}
				return messages, true
			}

			assistantMsg.ToolCalls = toolCalls
			messages = append(messages, assistantMsg)
			AddContext(cr.cfg, cr.ctxKey, assistantMsg, cr.network.Name, cr.channel, cr.nick)
			cr.logger.Info("assistant made tool calls", "count", len(toolCalls))
			logUsage(cr.logger, sdkResponseUsageToUsage(resp.Usage))

			numToolCalls := len(toolCalls)
			messages, _ = cr.executeToolCalls(messages, toolCalls)

			if cr.cfg.PreviousResponseID && currentResponseID != "" {
				toolResultMsgs := messages[len(messages)-numToolCalls:]
				input = toolResultMsgsToInputItems(toolResultMsgs)
			} else {
				input = messagesToResponseInputItems(messages)
			}
			continue
		}

		cr.transport.setCaptureBody(true)
		resp, err := cr.openaiClient.Responses.New(ctx, params)
		cr.transport.setCaptureBody(false)
		if err != nil {
			if usePrevID && isResponseIDError(err) && iteration == 1 {
				cr.logger.Warn("previous_response_id invalid, retrying without", "response_id", currentResponseID, "error", err)
				currentResponseID = ""
				SetContextResponseID(cr.ctxKey, "")
				usePrevID = false
				input = messagesToResponseInputItems(messages)
				iteration--
				continue
			}
			cr.sendError(err.Error())
			cr.logger.Error(err.Error())
			cr.logAPIIncident(err, messages, iteration, "responses")
			return messages, true
		}

		if resp.ID != "" {
			currentResponseID = resp.ID
			SetContextResponseID(cr.ctxKey, resp.ID)
		}

		text, reasoning, toolCalls := parseSDKResponseOutput(*resp)

		if len(toolCalls) == 0 {
			content := text
			if content == "" && reasoning == "" {
				content = "..."
			}
			assistantMsg := ChatMessage{
				Role:             RoleAssistant,
				Content:          content,
				ReasoningContent: reasoning,
			}
			AddContext(cr.cfg, cr.ctxKey, assistantMsg, cr.network.Name, cr.channel, cr.nick)
			out := FormatOutput(content)
			cr.logger.Info(out)
			textFinal := ExtractFinalText(content)

			logUsage(cr.logger, sdkResponseUsageToUsage(resp.Usage))
			if reasoning != "" {
				cr.logger.Info("reasoning", "content", reasoning)
			}

			if textFinal != "" && textFinal != "..." {
				rawText := textFinal
				if cr.cfg.RenderMarkdown {
					textFinal = markdowntoirc.MarkdownToIRC(textFinal)
				}
				chCfg := cr.network.GetChannelConfig(cr.channel)
				if chCfg.Pastebin {
					lines := wrapForIRC(textFinal)
					if len(lines) >= chCfg.GetMaxLines() {
						url, err := uploadToPastebin(rawText)
						if err != nil {
							cr.sendIRC(errorMsg("pastebin: " + err.Error()))
							preview := chCfg.GetMaxLines()
							if preview > len(lines) {
								preview = len(lines)
							}
							for i := 0; i < preview; i++ {
								cr.sendIRC(lines[i])
							}
							cr.sendIRC("... (full output could not be pasted)")
						} else {
							preview := 3
							if preview > len(lines) {
								preview = len(lines)
							}
							for i := 0; i < preview; i++ {
								cr.sendIRC(lines[i])
							}
							cr.sendIRC(fmt.Sprintf("... ( full output: %s )", url))
						}
						return messages, true
					}
				}
				cr.sendIRC(textFinal)
			}
			return messages, true
		}

		cr.logger.Info("assistant made tool calls", "count", len(toolCalls))
		logUsage(cr.logger, sdkResponseUsageToUsage(resp.Usage))
		assistantMsg := ChatMessage{
			Role:             RoleAssistant,
			Content:          text,
			ReasoningContent: reasoning,
			ToolCalls:        toolCalls,
		}
		messages = append(messages, assistantMsg)
		AddContext(cr.cfg, cr.ctxKey, assistantMsg, cr.network.Name, cr.channel, cr.nick)
		if text != "" {
			t := ExtractFinalText(text)
			if cr.cfg.RenderMarkdown {
				t = markdowntoirc.MarkdownToIRC(t)
			}
			cr.sendIRC(t)
		}
		if reasoning != "" {
			cr.logger.Info("reasoning", "content", reasoning)
		}

		numToolCalls := len(toolCalls)
		messages, _ = cr.executeToolCalls(messages, toolCalls)

		if cr.cfg.PreviousResponseID && currentResponseID != "" {
			toolResultMsgs := messages[len(messages)-numToolCalls:]
			input = toolResultMsgsToInputItems(toolResultMsgs)
		} else {
			input = messagesToResponseInputItems(messages)
		}
	}

	cr.sendIRC("\x0308⚠️ Tool call limit reached, stopping.\x0F")
	cr.logger.Warn("max tool iterations reached")
	return messages, true
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
	var streamingRenderer *markdowntoirc.StreamingRenderer
	if cr.cfg.RenderMarkdown {
		streamingRenderer = markdowntoirc.NewStreamingRenderer()
	}
	bufferb := ""
	logBuf := strings.Builder{}

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
				bufferb += textDelta
				fullText += textDelta
				if streamingRenderer != nil {
					for _, line := range streamingRenderer.Process(textDelta) {
						logBuf.WriteString(line)
						logBuf.WriteString("\n")
						cr.sendIRC(line)
					}
				} else {
					if strings.Contains(bufferb, "\n") {
						logBuf.WriteString(bufferb)
						cr.sendIRC(bufferb)
						bufferb = ""
					}
				}

			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				reasoningBuffer += event.Delta

			case "response.function_call_arguments.delta":
				// accumulated via response.completed

			case "response.output_item.done":
				if event.Item.Type == "function_call" && (cr.cfg.ToolVerbose == nil || *cr.cfg.ToolVerbose) {
					serverName := getMCPServerForTool(event.Item.Name)
					cr.sendIRC(fmt.Sprintf("\x0315🔧 ToolCall: %s > %s", serverName, event.Item.Name))
				}

			case "response.completed":
				completedResponse = &event.Response
			}

		case <-idleTimer.C:
			stream.Close()
			return nil, fmt.Errorf("responses stream timed out (no data received)")
		}
	}

streamDone:
	if completedResponse == nil {
		return nil, fmt.Errorf("responses stream ended without response.completed event")
	}

	logStreamCompletion(cr.transport.sessionID, cr.cfg.Model, fullText, reasoningBuffer, nil, sdkResponseUsageToUsage(completedResponse.Usage), RoleAssistant)

	if streamingRenderer != nil {
		for _, line := range streamingRenderer.Process("") {
			logBuf.WriteString(line)
			logBuf.WriteString("\n")
			cr.sendIRC(line)
		}
	} else if bufferb != "" {
		logBuf.WriteString(bufferb)
		cr.sendIRC(bufferb)
	}

	cr.logger.Info("output", "text", logBuf.String())
	logUsage(cr.logger, sdkResponseUsageToUsage(completedResponse.Usage))
	if reasoningBuffer != "" {
		cr.logger.Info("reasoning", "content", reasoningBuffer)
	}

	return completedResponse, nil
}

func chat(network Network, c *girc.Client, e girc.Event, cfg AIConfig, ctx context.Context, output chan<- string, args ...string) {
	runner := newChatRunnerFn(network, c, cfg, ctx, output).(*chatRunner)
	runner.setChannel(e.Params[0], e.Source.Name)

	ctx_key := runner.ctxKey

	var messages []ChatMessage
	if !ContextExists(ctx_key) {
		var systemContent string
		if cfg.SystemTmpl != nil {
			var templateVars map[string]string
			readConfig(func() {
				templateVars = make(map[string]string, len(config.TemplateVars))
				for k, v := range config.TemplateVars {
					templateVars[k] = v
				}
			})
			data := SystemPromptData{
				Nick:      e.Source.Name,
				BotNick:   c.GetNick(),
				Channel:   e.Params[0],
				Network:   network.Name,
				ChanNicks: "",
				Vars:      templateVars,
			}

			ch := c.LookupChannel(data.Channel)
			var nicks []string
			if ch != nil {
				for _, u := range ch.Users(c) {
					nicks = append(nicks, u.Nick)
				}
				sort.Strings(nicks)
			}
			data.ChanNicks = `["` + strings.Join(nicks, `","`) + `"]`

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
		AddContext(cfg, ctx_key, ChatMessage{
			Role:    RoleSystem,
			Content: systemContent,
		}, network.Name, e.Params[0], e.Source.Name)
	}
	messages = GetContext(ctx_key).Messages

	var userMsg ChatMessage
	if cfg.DetectImages {
		cleanText, imageUrls := detectImageURLs(args[0])

		if len(imageUrls) > 0 {
			existingImages := countContextImages(messages)
			remaining := cfg.MaxContextImages - existingImages

			if remaining <= 0 {
				runner.sendWarning(fmt.Sprintf("image limit reached (%d max in context), send text only", cfg.MaxContextImages))
				return
			}

			if len(imageUrls) > remaining {
				runner.sendWarning(fmt.Sprintf("only %d more image(s) allowed in this context (%d/%d used)", remaining, existingImages, cfg.MaxContextImages))
				return
			}

			var err error
			userMsg, err = buildImageMessage(cleanText, imageUrls, cfg.MaxImages, cfg.ImageFormat, cfg.ImageQuality, cfg.MaxImageWidth, cfg.MaxImageHeight)
			if err != nil {
				runner.sendError("failed to process images: " + err.Error())
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

	messages = AddContext(cfg, ctx_key, userMsg, network.Name, e.Params[0], e.Source.Name)
	runner.syncAPISessionID()
	runner.logger.Debug("running completion", "summary", summarizeMessages(messages))
	runner.syncConvID()

	messages, _ = runner.runTurn(messages)
	runner.logger.Debug("completion finished", "api_log", apiLogger.GetSessionFilePath(GetContext(runner.ctxKey).SessionID))

	if theDB != nil {
		chatContextsMutex.Lock()
		chatCtx := chatContextsMap[ctx_key]
		chatContextsMutex.Unlock()
		if chatCtx.SessionID != 0 {
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				completedJobs, err := getCompletedPendingJobs(chatCtx.SessionID)
				if err != nil || len(completedJobs) == 0 {
					break
				}
				for _, cj := range completedJobs {
					injectAsyncResultFromDB(ctx_key, chatCtx, cj, network.Name, e.Params[0], e.Source.Name)
					markPendingJobDelivered(cj.JobID)
				}
				chatContextsMutex.Lock()
				messages = chatContextsMap[ctx_key].Messages
				chatContextsMutex.Unlock()
				messages, _ = runner.runTurn(messages)
			}
		}
	}
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

func registerBackgroundJobTool() Tool {
	return Tool{
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
						"description": "Name of the MCP server running the job",
					},
				},
				"required": []string{"job_id", "tool_name", "server_name"},
			},
		},
	}
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
