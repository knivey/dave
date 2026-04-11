package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

func chat(network Network, c *girc.Client, e girc.Event, cfg AIConfig, args ...string) {
	aiConfig := gogpt.DefaultConfig(config.Services[cfg.Service].Key)
	aiConfig.BaseURL = config.Services[cfg.Service].BaseURL
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

	req := BuildChatRequest(cfg, messages)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if !cfg.Streaming {
		resp, err := aiClient.CreateChatCompletion(ctx, req)
		if err != nil {
			c.Cmd.Reply(e, errorMsg(err.Error()))
			logger.Error(err.Error())
			return
		}

		AddContext(cfg, ctx_key, gogpt.ChatCompletionMessage{
			Role:    gogpt.ChatMessageRoleAssistant,
			Content: resp.Choices[0].Message.Content,
		})
		out := FormatOutput(resp.Choices[0].Message.Content)
		logger.Info(out)
		text := ExtractFinalText(resp.Choices[0].Message.Content)

		logger.Info("token usage", "prompt", resp.Usage.PromptTokens, "completion", resp.Usage.CompletionTokens, "total", resp.Usage.TotalTokens)

		if cfg.RenderMarkdown {
			text = markdowntoirc.MarkdownToIRC(text)
		}
		sendLoop(text, network, c, e)
		return
	}

	stream, err := aiClient.CreateChatCompletionStream(ctx, req)
	if err != nil {
		c.Cmd.Reply(e, errorMsg(err.Error()))
		logger.Error(err.Error())
		return
	}
	defer stream.Close()
	bufferb := ""
	streamingRenderer := markdowntoirc.NewStreamingRenderer()
	defer func() {
		logger.Info(bufferb)
		AddContext(cfg, ctx_key, gogpt.ChatCompletionMessage{
			Role:    gogpt.ChatMessageRoleAssistant,
			Content: bufferb,
		})
		// Render any remaining partial line
		for _, line := range streamingRenderer.Process("") {
			sendLoop(line, network, c, e)
		}
	}()
	for {
		if !getRunning(network.Name + e.Params[0]) {
			logger.Info("Closing stream")
			stream.Close()
			return
		}
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			logger.Info("Stream completed")
			return
		}
		if err != nil {
			c.Cmd.Reply(e, errorMsg(err.Error()))
			logger.Error(err.Error())
			return
		}
		if resp.Usage != nil {
			logger.Info("token usage", "prompt", resp.Usage.PromptTokens, "completion", resp.Usage.CompletionTokens, "total", resp.Usage.TotalTokens)
		}
		if len(resp.Choices) == 0 {
			continue
		}
		delta := resp.Choices[0].Delta.Content
		bufferb += delta
		for _, line := range streamingRenderer.Process(delta) {
			sendLoop(line, network, c, e)
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
	}
	if cfg.Streaming {
		req.Stream = true
		req.StreamOptions = &gogpt.StreamOptions{IncludeUsage: true}
	}
	return req
}
