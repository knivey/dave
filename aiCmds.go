package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	gogpt "github.com/sashabaranov/go-openai"
	markdowntoirc "github.come/knivey/dave/MarkdownToIRC"
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
		c.Cmd.Reply(e, err.Error())
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
		AddContext(cfg, ctx_key, gogpt.ChatCompletionMessage{
			Role:    gogpt.ChatMessageRoleSystem,
			Content: cfg.System,
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
				c.Cmd.Reply(e, fmt.Sprintf("image limit reached (%d max in context), send text only", cfg.MaxContextImages))
				return
			}

			if len(imageUrls) > remaining {
				c.Cmd.Reply(e, fmt.Sprintf("only %d more image(s) allowed in this context (%d/%d used)", remaining, existingImages, cfg.MaxContextImages))
				return
			}

			var err error
			userMsg, err = buildImageMessage(cleanText, imageUrls, cfg.MaxImages)
			if err != nil {
				c.Cmd.Reply(e, "failed to process images: "+err.Error())
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

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if !cfg.Streaming {
		resp, err := aiClient.CreateChatCompletion(ctx, req)
		if err != nil {
			c.Cmd.Reply(e, err.Error())
			logger.Error(err.Error())
			return
		}

		AddContext(cfg, ctx_key, gogpt.ChatCompletionMessage{
			Role:    gogpt.ChatMessageRoleAssistant,
			Content: resp.Choices[0].Message.Content,
		})
		out := resp.Choices[0].Message.Content
		out = strings.ReplaceAll(out, "\x03", "\x1b[033m[C]\x1b[0m")
		out = strings.ReplaceAll(out, "\x02", "\x1b[034m[B]\x1b[0m")
		out = strings.ReplaceAll(out, "\x1F", "\x1b[035m[U]\x1b[0m")
		out = strings.ReplaceAll(out, "\x1D", "\x1b[036m[I]\x1b[0m")
		logger.Info(out)
		text := resp.Choices[0].Message.Content
		//cut out </think>
		cut := strings.LastIndex(text, "</think>\n")
		if cut > -1 {
			cut += len("</think>\n")
			//think := text[:cut]
			text = text[cut:]
		}

		logger.Info("token usage", "prompt", resp.Usage.PromptTokens, "completion", resp.Usage.CompletionTokens, "total", resp.Usage.TotalTokens)

		if cfg.RenderMarkdown {
			text = markdowntoirc.MarkdownToIRC(text)
		}
		sendLoop(text, network, c, e)
		return
	}

	stream, err := aiClient.CreateChatCompletionStream(ctx, req)
	if err != nil {
		c.Cmd.Reply(e, err.Error())
		logger.Error(err.Error())
		return
	}
	defer stream.Close()
	buffer := ""
	bufferb := ""
	defer func() {
		logger.Info(bufferb)
		AddContext(cfg, ctx_key, gogpt.ChatCompletionMessage{
			Role:    gogpt.ChatMessageRoleAssistant,
			Content: bufferb,
		})
		sendLoop(buffer, network, c, e)
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
			c.Cmd.Reply(e, err.Error())
			logger.Error(err.Error())
			return
		}
		if resp.Usage != nil {
			logger.Info("token usage", "prompt", resp.Usage.PromptTokens, "completion", resp.Usage.CompletionTokens, "total", resp.Usage.TotalTokens)
		}
		if len(resp.Choices) == 0 {
			continue
		}
		buffer += resp.Choices[0].Delta.Content
		bufferb += resp.Choices[0].Delta.Content
		if before, after, found := strings.Cut(buffer, "\n"); found {
			sendLoop(before, network, c, e)
			buffer = after
		}
	}
}
