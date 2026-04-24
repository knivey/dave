package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	markdowntoirc "github.com/knivey/dave/MarkdownToIRC"
	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	gogpt "github.com/sashabaranov/go-openai"
)

func completion(network Network, c *girc.Client, e girc.Event, cfg AIConfig, ctx context.Context, output chan<- string, args ...string) {
	aiConfig := gogpt.DefaultConfig(config.Services[cfg.Service].Key)
	aiConfig.BaseURL = config.Services[cfg.Service].BaseURL
	aiClient := gogpt.NewClientWithConfig(aiConfig)

	logger := logxi.New(network.Name + ".completion." + cfg.Name)
	logger.SetLevel(logxi.LevelAll)

	req := gogpt.CompletionRequest{
		Model:       cfg.Model,
		MaxTokens:   cfg.MaxTokens,
		Prompt:      args[0],
		Temperature: cfg.Temperature,
	}

	apiCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	resp, err := aiClient.CreateCompletion(apiCtx, req)
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

func logUsage(logger logxi.Logger, usage *gogpt.Usage) {
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
	aiClient  *gogpt.Client
	transport *daveTransport
	cfg       AIConfig
	network   Network
	client    *girc.Client
	channel   string
	nick      string
	ctxKey    string
	logger    logxi.Logger
	ctx       context.Context
	outputCh  chan<- string
}

func newChatRunner(network Network, client *girc.Client, cfg AIConfig) *chatRunner {
	svc := config.Services[cfg.Service]
	aiConfig := gogpt.DefaultConfig(svc.Key)
	aiConfig.BaseURL = svc.BaseURL
	extraBody := make(map[string]any, len(cfg.ExtraBody)+1)
	for k, v := range cfg.ExtraBody {
		extraBody[k] = v
	}
	if svc.Type == "llama" {
		extraBody["timings_per_token"] = true
	}
	transport := newDaveTransport(extraBody)
	aiConfig.HTTPClient = &http.Client{Transport: transport}
	aiClient := gogpt.NewClientWithConfig(aiConfig)

	ctxKey := network.Name + client.GetNick()
	logger := logxi.New(network.Name + ".completion." + cfg.Name)
	logger.SetLevel(logxi.LevelAll)

	return &chatRunner{
		aiClient:  aiClient,
		transport: transport,
		cfg:       cfg,
		network:   network,
		client:    client,
		logger:    logger,
		ctxKey:    ctxKey,
	}
}

func (cr *chatRunner) setChannel(channel, nick string) {
	cr.channel = channel
	cr.nick = nick
	cr.ctxKey = cr.network.Name + channel + nick
	cr.transport.setAPILogger(apiLogger, cr.ctxKey)
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

func (cr *chatRunner) sendWarning(msg string) {
	select {
	case cr.outputCh <- warnMsg(msg):
	case <-cr.ctx.Done():
	}
}

func (cr *chatRunner) runTurn(messages []gogpt.ChatCompletionMessage) ([]gogpt.ChatCompletionMessage, bool) {
	mcpTools := getMCPTools(cr.cfg.MCPs)
	mcpTools = append(mcpTools, registerBackgroundJobTool())

	req := BuildChatRequest(cr.cfg, messages)
	req.Tools = mcpTools
	if len(mcpTools) > 0 {
		req.ToolChoice = "auto"
		parallelCalls := true
		if cr.cfg.ParallelToolCalls != nil {
			parallelCalls = *cr.cfg.ParallelToolCalls
		}
		req.ParallelToolCalls = parallelCalls
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

		req.Messages = messages

		if cr.cfg.Streaming {
			req.Stream = true
			req.StreamOptions = &gogpt.StreamOptions{IncludeUsage: true}

			stream, err := cr.aiClient.CreateChatCompletionStream(ctx, req)
			if err != nil {
				cr.sendError(err.Error())
				cr.logger.Error(err.Error())
				return messages, true
			}

			bufferb := ""
			fullContent := ""
			reasoningBuffer := ""
			logBuf := strings.Builder{}
			var streamingRenderer *markdowntoirc.StreamingRenderer
			if cr.cfg.RenderMarkdown {
				streamingRenderer = markdowntoirc.NewStreamingRenderer()
			}

			var accumulatedToolCalls []gogpt.ToolCall
			var assistantRole string
			var streamUsage *gogpt.Usage
			var streamTimings *Timings
			streamDone := false
			streamModel := cr.cfg.Model

			type recvResult struct {
				data []byte
				err  error
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
					raw, err := stream.RecvRaw()
					ch <- recvResult{data: raw, err: err}
				}()

				select {
				case res := <-ch:
					if errors.Is(res.err, io.EOF) {
						cr.logger.Info("Stream completed")
						streamDone = true
						stream.Close()
						break StreamLoop
					}
					if res.err != nil {
						stream.Close()
						cr.sendError(res.err.Error())
						cr.logger.Error(res.err.Error())
						return messages, true
					}
					idleTimer.Reset(cr.cfg.StreamTimeout)

					rawBytes := res.data
					if apiLogger != nil {
						apiLogger.LogStreamChunk(cr.ctxKey, rawBytes)
					}
					var resp gogpt.ChatCompletionStreamResponse
					if err := json.Unmarshal(rawBytes, &resp); err != nil {
						cr.logger.Error("failed to unmarshal stream chunk", "error", err)
						continue
					}

					chunkReasoning := resp.Choices[0].Delta.ReasoningContent
					if chunkReasoning == "" {
						chunkReasoning = extractStreamReasoning(rawBytes)
					}
					if chunkReasoning == "" {
						chunkReasoning = extractReasoningFromField(rawBytes)
					}
					reasoningBuffer += chunkReasoning

					if resp.Usage != nil {
						streamUsage = resp.Usage
					}
					if t := extractTimings(rawBytes); t != nil {
						streamTimings = t
					}
					if len(resp.Choices) == 0 {
						continue
					}
					choice := resp.Choices[0]
					delta := choice.Delta

					if delta.Role != "" {
						assistantRole = delta.Role
					}

					for _, tc := range delta.ToolCalls {
						if tc.Index != nil {
							idx := *tc.Index
							for len(accumulatedToolCalls) <= idx {
								accumulatedToolCalls = append(accumulatedToolCalls, gogpt.ToolCall{})
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

					if choice.FinishReason == gogpt.FinishReasonToolCalls {
						cr.logger.Info("stream finished with tool calls")
						stream.Close()
						break StreamLoop
					}
					if choice.FinishReason == gogpt.FinishReasonStop || choice.FinishReason == gogpt.FinishReasonLength {
						streamDone = true
						stream.Close()
						break StreamLoop
					}

				case <-idleTimer.C:
					stream.Close()
					cr.sendError("stream timed out (no data received)")
					cr.logger.Error("stream idle timeout exceeded", "timeout", cr.cfg.StreamTimeout)
					return messages, true
				}
			}

			logStreamCompletion(cr.ctxKey, streamModel, fullContent, reasoningBuffer, accumulatedToolCalls, streamUsage, assistantRole)

			flushStreamedOutput := func() {
				cr.logger.Info(fullContent)
				content := fullContent
				if content == "" && reasoningBuffer == "" {
					content = "..."
				}
				AddContext(cr.cfg, cr.ctxKey, gogpt.ChatCompletionMessage{
					Role:             gogpt.ChatMessageRoleAssistant,
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
				assistantRole = gogpt.ChatMessageRoleAssistant
			}

			assistantMsg := gogpt.ChatCompletionMessage{
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

			var registeredJob bool
			messages, registeredJob = cr.executeToolCalls(messages, accumulatedToolCalls)
			if registeredJob {
				return messages, true
			}
			continue
		}

		cr.transport.setCaptureBody(true)
		resp, err := cr.aiClient.CreateChatCompletion(ctx, req)
		cr.transport.setCaptureBody(false)
		if err != nil {
			cr.sendError(err.Error())
			cr.logger.Error(err.Error())
			return messages, true
		}

		msg := resp.Choices[0].Message

		reasoning := msg.ReasoningContent
		capturedBody := cr.transport.getCapturedBody()
		rawReasoning, rawDetails := extractResponseReasoning(capturedBody)
		if reasoning == "" && rawReasoning != "" {
			reasoning = rawReasoning
		}
		if reasoning == "" && len(rawDetails) > 0 {
			reasoning = extractReasoningText(rawDetails)
		}
		nonStreamTimings := extractTimings(capturedBody)

		if len(msg.ToolCalls) == 0 {
			content := msg.Content
			if content == "" && reasoning == "" {
				content = "..."
			}
			AddContext(cr.cfg, cr.ctxKey, gogpt.ChatCompletionMessage{
				Role:             gogpt.ChatMessageRoleAssistant,
				Content:          content,
				ReasoningContent: reasoning,
			}, cr.network.Name, cr.channel, cr.nick)
			out := FormatOutput(content)
			cr.logger.Info(out)
			text := ExtractFinalText(content)

			logUsage(cr.logger, &resp.Usage)
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
						url, err := uploadToTermbin(rawText)
						if err != nil {
							cr.sendIRC("pastebin error: " + err.Error())
							cr.sendIRC(text)
						} else {
							preview := 3
							if preview > len(lines) {
								preview = len(lines)
							}
							for i := 0; i < preview; i++ {
								cr.sendIRC(lines[i])
							}
							cr.sendIRC(fmt.Sprintf("... (full output: %s)", url))
						}
						return messages, true
					}
				}
				cr.sendIRC(text)
			}
			return messages, true
		}

		cr.logger.Info("assistant made tool calls", "count", len(msg.ToolCalls))
		logUsage(cr.logger, &resp.Usage)
		logTimings(cr.logger, nonStreamTimings)
		messages = append(messages, msg)

		AddContext(cr.cfg, cr.ctxKey, gogpt.ChatCompletionMessage{
			Role:             gogpt.ChatMessageRoleAssistant,
			Content:          msg.Content,
			ReasoningContent: reasoning,
			ToolCalls:        msg.ToolCalls,
		}, cr.network.Name, cr.channel, cr.nick)
		if msg.Content != "" {
			text := ExtractFinalText(msg.Content)
			if cr.cfg.RenderMarkdown {
				text = markdowntoirc.MarkdownToIRC(text)
			}
			cr.sendIRC(text)
		}
		if reasoning != "" {
			cr.logger.Info("reasoning", "content", reasoning)
		}

		var registeredJob bool
		messages, registeredJob = cr.executeToolCalls(messages, msg.ToolCalls)
		if registeredJob {
			return messages, true
		}
	}
}

func (cr *chatRunner) executeToolCalls(messages []gogpt.ChatCompletionMessage, toolCalls []gogpt.ToolCall) ([]gogpt.ChatCompletionMessage, bool) {
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
			toolMsg := gogpt.ChatCompletionMessage{
				Role:       gogpt.ChatMessageRoleTool,
				Content:    "error: " + err.Error(),
				ToolCallID: tc.ID,
			}
			messages = append(messages, toolMsg)
			AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
			continue
		}
		toolText := mcpToolResultToText(result)
		cr.logger.Info("MCP tool result", "tool", tc.Function.Name, "result", toolText)
		toolMsg := gogpt.ChatCompletionMessage{
			Role:       gogpt.ChatMessageRoleTool,
			Content:    toolText,
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
	}
	return messages, registeredJob
}

func (cr *chatRunner) handleRegisterBackgroundJob(messages []gogpt.ChatCompletionMessage, tc gogpt.ToolCall) []gogpt.ChatCompletionMessage {
	var args struct {
		JobID      string `json:"job_id"`
		ToolName   string `json:"tool_name"`
		ServerName string `json:"server_name"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		toolMsg := gogpt.ChatCompletionMessage{
			Role:       gogpt.ChatMessageRoleTool,
			Content:    "error: failed to parse arguments: " + err.Error(),
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
		return messages
	}

	if args.JobID == "" || args.ToolName == "" || args.ServerName == "" {
		toolMsg := gogpt.ChatCompletionMessage{
			Role:       gogpt.ChatMessageRoleTool,
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
		toolMsg := gogpt.ChatCompletionMessage{
			Role:       gogpt.ChatMessageRoleTool,
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
		toolMsg := gogpt.ChatCompletionMessage{
			Role:       gogpt.ChatMessageRoleTool,
			Content:    "error: no active session to register job against",
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
		return messages
	}

	if err := createPendingJob(sessionID, args.JobID, args.ToolName, args.ServerName); err != nil {
		cr.logger.Error("failed to create pending job", "error", err)
		toolMsg := gogpt.ChatCompletionMessage{
			Role:       gogpt.ChatMessageRoleTool,
			Content:    "error: failed to register job: " + err.Error(),
			ToolCallID: tc.ID,
		}
		messages = append(messages, toolMsg)
		AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
		return messages
	}

	registerAsyncJob(args.JobID, sessionID, cr.ctxKey, args.ToolName, args.ServerName, cr.network.Name, cr.channel, cr.nick)

	toolMsg := gogpt.ChatCompletionMessage{
		Role:       gogpt.ChatMessageRoleTool,
		Content:    "Job registered. You will receive the result when it completes.",
		ToolCallID: tc.ID,
	}
	messages = append(messages, toolMsg)
	AddContext(cr.cfg, cr.ctxKey, toolMsg, cr.network.Name, cr.channel, cr.nick)
	return messages
}

func chat(network Network, c *girc.Client, e girc.Event, cfg AIConfig, ctx context.Context, output chan<- string, args ...string) {
	runner := newChatRunnerFn(network, c, cfg, ctx, output).(*chatRunner)
	runner.setChannel(e.Params[0], e.Source.Name)

	ctx_key := runner.ctxKey

	var messages []gogpt.ChatCompletionMessage
	if !ContextExists(ctx_key) {
		var systemContent string
		if cfg.SystemTmpl != nil {
			data := SystemPromptData{
				Nick:      e.Source.Name,
				BotNick:   c.GetNick(),
				Channel:   e.Params[0],
				Network:   network.Name,
				ChanNicks: "",
				Vars:      config.TemplateVars,
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
		AddContext(cfg, ctx_key, gogpt.ChatCompletionMessage{
			Role:    gogpt.ChatMessageRoleSystem,
			Content: systemContent,
		}, network.Name, e.Params[0], e.Source.Name)
	}
	messages = GetContext(ctx_key).Messages

	var userMsg gogpt.ChatCompletionMessage
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
			userMsg = gogpt.ChatCompletionMessage{
				Role:    gogpt.ChatMessageRoleUser,
				Content: cleanText,
			}
		}
	} else {
		userMsg = gogpt.ChatCompletionMessage{
			Role:    gogpt.ChatMessageRoleUser,
			Content: args[0],
		}
	}

	messages = AddContext(cfg, ctx_key, userMsg, network.Name, e.Params[0], e.Source.Name)
	runner.logger.Debug("running completion with messages:", "messages", sanitizeMessages(messages))

	messages, _ = runner.runTurn(messages)

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

func registerBackgroundJobTool() gogpt.Tool {
	return gogpt.Tool{
		Type: "function",
		Function: &gogpt.FunctionDefinition{
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

func BuildChatRequest(cfg AIConfig, messages []gogpt.ChatCompletionMessage) gogpt.ChatCompletionRequest {
	req := gogpt.ChatCompletionRequest{
		Model:               cfg.Model,
		MaxTokens:           cfg.MaxTokens,
		MaxCompletionTokens: cfg.MaxCompletionTokens,
		Messages:            messages,
		Temperature:         cfg.Temperature,
		TopP:                cfg.TopP,
		Stop:                cfg.Stop,
		PresencePenalty:     cfg.PresencePenalty,
		FrequencyPenalty:    cfg.FrequencyPenalty,

		ReasoningEffort: cfg.ReasoningEffort,
		ServiceTier:     gogpt.ServiceTier(cfg.ServiceTier),
		Verbosity:       cfg.Verbosity,
	}
	if cfg.ChatTemplateKwargs != nil {
		req.ChatTemplateKwargs = cfg.ChatTemplateKwargs
	}
	if cfg.Streaming {
		req.Stream = true
		req.StreamOptions = &gogpt.StreamOptions{IncludeUsage: true}
	}
	return req
}

func logStreamCompletion(ctxKey, model, content, reasoning string, toolCalls []gogpt.ToolCall, usage *gogpt.Usage, role string) {
	if apiLogger == nil {
		return
	}
	if role == "" {
		role = gogpt.ChatMessageRoleAssistant
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
	apiLogger.LogStreamResponse(ctxKey, body)
}
