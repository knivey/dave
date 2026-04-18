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

func completion(network Network, c *girc.Client, e girc.Event, cfg AIConfig, args ...string) {
	aiConfig := gogpt.DefaultConfig(config.Services[cfg.Service].Key)
	aiConfig.BaseURL = config.Services[cfg.Service].BaseURL
	aiClient := gogpt.NewClientWithConfig(aiConfig)

	logger := logxi.New(network.Name + ".completion." + cfg.Name)
	logger.SetLevel(logxi.LevelAll)

	startedRunning(network.Name + e.Params[0])
	defer stoppedRunning(network.Name + e.Params[0])

	req := gogpt.CompletionRequest{
		Model:       cfg.Model,
		MaxTokens:   cfg.MaxTokens,
		Prompt:      args[0],
		Temperature: cfg.Temperature,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	resp, err := aiClient.CreateCompletion(ctx, req)
	if err != nil {
		c.Cmd.Reply(e, errorMsg(err.Error()))
		logger.Error(err.Error())
		return
	}

	logger.Info(resp.Choices[0].Text)
	sendLoop(resp.Choices[0].Text, network, c, e)
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

func chat(network Network, c *girc.Client, e girc.Event, cfg AIConfig, args ...string) {
	aiConfig := gogpt.DefaultConfig(config.Services[cfg.Service].Key)
	aiConfig.BaseURL = config.Services[cfg.Service].BaseURL
	transport := newDaveTransport(cfg.ExtraBody)
	aiConfig.HTTPClient = &http.Client{Transport: transport}
	aiClient := gogpt.NewClientWithConfig(aiConfig)

	logger := logxi.New(network.Name + ".completion." + cfg.Name)
	logger.SetLevel(logxi.LevelAll)

	startedRunning(network.Name + e.Params[0])
	defer stoppedRunning(network.Name + e.Params[0])

	var messages []gogpt.ChatCompletionMessage
	ctx_key := network.Name + e.Params[0] + e.Source.Name
	if !ContextExists(ctx_key) {
		var systemContent string
		if cfg.SystemTmpl != nil {
			data := SystemPromptData{
				Nick:      e.Source.Name,
				BotNick:   c.GetNick(),
				Channel:   e.Params[0],
				Network:   network.Name,
				ChanNicks: "",
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
				logger.Error("system prompt template execution error:", err)
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
		})
	}
	messages = GetContext(ctx_key).Messages

	var userMsg gogpt.ChatCompletionMessage
	if cfg.DetectImages {
		cleanText, imageUrls := detectImageURLs(args[0])

		if len(imageUrls) > 0 {
			existingImages := countContextImages(messages)
			remaining := cfg.MaxContextImages - existingImages

			if remaining <= 0 {
				c.Cmd.Reply(e, warnMsg(fmt.Sprintf("image limit reached (%d max in context), send text only", cfg.MaxContextImages)))
				return
			}

			if len(imageUrls) > remaining {
				c.Cmd.Reply(e, warnMsg(fmt.Sprintf("only %d more image(s) allowed in this context (%d/%d used)", remaining, existingImages, cfg.MaxContextImages)))
				return
			}

			var err error
			userMsg, err = buildImageMessage(cleanText, imageUrls, cfg.MaxImages, cfg.ImageFormat, cfg.ImageQuality, cfg.MaxImageWidth, cfg.MaxImageHeight)
			if err != nil {
				c.Cmd.Reply(e, errorMsg("failed to process images: "+err.Error()))
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

	messages = AddContext(cfg, ctx_key, userMsg)
	logger.Debug("running completion with messages:", "messages", sanitizeMessages(messages))

	mcpTools := getMCPTools(cfg.MCPs)

	req := BuildChatRequest(cfg, messages)
	req.Tools = mcpTools
	if len(mcpTools) > 0 {
		req.ToolChoice = "auto"
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	for {
		req.Messages = messages

		if cfg.Streaming {
			req.Stream = true
			req.StreamOptions = &gogpt.StreamOptions{IncludeUsage: true}

			stream, err := aiClient.CreateChatCompletionStream(ctx, req)
			if err != nil {
				c.Cmd.Reply(e, errorMsg(err.Error()))
				logger.Error(err.Error())
				return
			}

			bufferb := ""
			reasoningBuffer := ""
			logBuf := strings.Builder{}
			var streamingRenderer *markdowntoirc.StreamingRenderer
			if cfg.RenderMarkdown {
				streamingRenderer = markdowntoirc.NewStreamingRenderer()
			}

			var accumulatedToolCalls []gogpt.ToolCall
			var assistantRole string
			var streamUsage *gogpt.Usage
			streamDone := false

			type recvResult struct {
				data []byte
				err  error
			}

			idleTimer := time.NewTimer(cfg.StreamTimeout)
			defer idleTimer.Stop()

		StreamLoop:
			for {
				if !getRunning(network.Name + e.Params[0]) {
					logger.Info("Closing stream")
					stream.Close()
					return
				}

				ch := make(chan recvResult, 1)
				go func() {
					raw, err := stream.RecvRaw()
					ch <- recvResult{data: raw, err: err}
				}()

				select {
				case res := <-ch:
					if errors.Is(res.err, io.EOF) {
						logger.Info("Stream completed")
						streamDone = true
						stream.Close()
						break StreamLoop
					}
					if res.err != nil {
						stream.Close()
						c.Cmd.Reply(e, errorMsg(res.err.Error()))
						logger.Error(res.err.Error())
						return
					}
					idleTimer.Reset(cfg.StreamTimeout)

					rawBytes := res.data
					var resp gogpt.ChatCompletionStreamResponse
					if err := json.Unmarshal(rawBytes, &resp); err != nil {
						logger.Error("failed to unmarshal stream chunk", "error", err)
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
					if streamingRenderer != nil {
						for _, line := range streamingRenderer.Process(textDelta) {
							logBuf.WriteString(line)
							logBuf.WriteString("\n")
							sendLoop(line, network, c, e)
						}
					} else {
						if strings.Contains(bufferb, "\n") {
							logBuf.WriteString(bufferb)
							sendLoop(bufferb, network, c, e)
							bufferb = ""
						}
					}

					if choice.FinishReason == gogpt.FinishReasonToolCalls {
						logger.Info("stream finished with tool calls")
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
					c.Cmd.Reply(e, "error: stream timed out (no data received)")
					logger.Error("stream idle timeout exceeded", "timeout", cfg.StreamTimeout)
					return
				}
			}

			flushStreamedOutput := func() {
				logger.Info(bufferb)
				AddContext(cfg, ctx_key, gogpt.ChatCompletionMessage{
					Role:             gogpt.ChatMessageRoleAssistant,
					Content:          bufferb,
					ReasoningContent: reasoningBuffer,
				})
				if streamingRenderer != nil {
					for _, line := range streamingRenderer.Process("") {
						logBuf.WriteString(line)
						logBuf.WriteString("\n")
						sendLoop(line, network, c, e)
					}
				} else if bufferb != "" {
					logBuf.WriteString(bufferb)
					sendLoop(bufferb, network, c, e)
				}
				logger.Info("output", "text", logBuf.String())
				logUsage(logger, streamUsage)
				if reasoningBuffer != "" {
					logger.Info("reasoning", "content", reasoningBuffer)
				}
			}

			if streamDone || len(accumulatedToolCalls) == 0 {
				flushStreamedOutput()
				return
			}

			logger.Info("stream made tool calls", "count", len(accumulatedToolCalls))
			logUsage(logger, streamUsage)

			if assistantRole == "" {
				assistantRole = gogpt.ChatMessageRoleAssistant
			}

			assistantMsg := gogpt.ChatCompletionMessage{
				Role:             assistantRole,
				Content:          bufferb,
				ReasoningContent: reasoningBuffer,
				ToolCalls:        accumulatedToolCalls,
			}
			messages = append(messages, assistantMsg)

			if bufferb != "" {
				text := ExtractFinalText(bufferb)
				if cfg.RenderMarkdown {
					text = markdowntoirc.MarkdownToIRC(text)
				}
				sendLoop(text, network, c, e)
			}

			AddContext(cfg, ctx_key, assistantMsg)

			for _, tc := range accumulatedToolCalls {
				var toolArgs map[string]any
				json.Unmarshal([]byte(tc.Function.Arguments), &toolArgs)
				result, err := callMCPTool(tc.Function.Name, toolArgs)
				if err != nil {
					toolMsg := gogpt.ChatCompletionMessage{
						Role:       gogpt.ChatMessageRoleTool,
						Content:    "error: " + err.Error(),
						ToolCallID: tc.ID,
					}
					messages = append(messages, toolMsg)
					AddContext(cfg, ctx_key, toolMsg)
					continue
				}
				toolText := mcpToolResultToText(result)
				logger.Info("MCP tool result", "tool", tc.Function.Name, "result", toolText)
				toolMsg := gogpt.ChatCompletionMessage{
					Role:       gogpt.ChatMessageRoleTool,
					Content:    toolText,
					ToolCallID: tc.ID,
				}
				messages = append(messages, toolMsg)
				AddContext(cfg, ctx_key, toolMsg)
			}

			continue
		}

		transport.setCaptureBody(true)
		resp, err := aiClient.CreateChatCompletion(ctx, req)
		transport.setCaptureBody(false)
		if err != nil {
			c.Cmd.Reply(e, errorMsg(err.Error()))
			logger.Error(err.Error())
			return
		}

		msg := resp.Choices[0].Message

		reasoning := msg.ReasoningContent
		rawReasoning, rawDetails := extractResponseReasoning(transport.getCapturedBody())
		if reasoning == "" && rawReasoning != "" {
			reasoning = rawReasoning
		}
		if reasoning == "" && len(rawDetails) > 0 {
			reasoning = extractReasoningText(rawDetails)
		}

		if len(msg.ToolCalls) == 0 {
			AddContext(cfg, ctx_key, gogpt.ChatCompletionMessage{
				Role:             gogpt.ChatMessageRoleAssistant,
				Content:          msg.Content,
				ReasoningContent: reasoning,
			})
			out := FormatOutput(msg.Content)
			logger.Info(out)
			text := ExtractFinalText(msg.Content)

			logUsage(logger, &resp.Usage)
			if reasoning != "" {
				logger.Info("reasoning", "content", reasoning)
			}

			if cfg.RenderMarkdown {
				text = markdowntoirc.MarkdownToIRC(text)
			}
			sendLoop(text, network, c, e)
			return
		}

		logger.Info("assistant made tool calls", "count", len(msg.ToolCalls))
		logUsage(logger, &resp.Usage)
		messages = append(messages, msg)

		AddContext(cfg, ctx_key, gogpt.ChatCompletionMessage{
			Role:             gogpt.ChatMessageRoleAssistant,
			Content:          msg.Content,
			ReasoningContent: reasoning,
			ToolCalls:        msg.ToolCalls,
		})
		if msg.Content != "" {
			text := ExtractFinalText(msg.Content)
			if cfg.RenderMarkdown {
				text = markdowntoirc.MarkdownToIRC(text)
			}
			sendLoop(text, network, c, e)
		}
		if reasoning != "" {
			logger.Info("reasoning", "content", reasoning)
		}

		for _, tc := range msg.ToolCalls {
			var toolArgs map[string]any
			json.Unmarshal([]byte(tc.Function.Arguments), &toolArgs)
			result, err := callMCPTool(tc.Function.Name, toolArgs)
			if err != nil {
				toolMsg := gogpt.ChatCompletionMessage{
					Role:       gogpt.ChatMessageRoleTool,
					Content:    "error: " + err.Error(),
					ToolCallID: tc.ID,
				}
				messages = append(messages, toolMsg)
				AddContext(cfg, ctx_key, toolMsg)
				continue
			}
			toolText := mcpToolResultToText(result)
			logger.Info("MCP tool result", "tool", tc.Function.Name, "result", toolText)
			toolMsg := gogpt.ChatCompletionMessage{
				Role:       gogpt.ChatMessageRoleTool,
				Content:    toolText,
				ToolCallID: tc.ID,
			}
			messages = append(messages, toolMsg)
			AddContext(cfg, ctx_key, toolMsg)
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
		ParallelToolCalls:   cfg.ParallelToolCalls,
		ReasoningEffort:     cfg.ReasoningEffort,
		ServiceTier:         gogpt.ServiceTier(cfg.ServiceTier),
		Verbosity:           cfg.Verbosity,
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
