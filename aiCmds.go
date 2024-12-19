package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"

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

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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

	req := gogpt.ChatCompletionRequest{
		Model:     cfg.Model,
		MaxTokens: cfg.MaxTokens,
		Messages: []gogpt.ChatCompletionMessage{
			{
				Role:    gogpt.ChatMessageRoleSystem,
				Content: cfg.System,
			},
			{
				Role:    gogpt.ChatMessageRoleUser,
				Content: args[0],
			},
		},
		Temperature: cfg.Temperature,
	}

	if cfg.Streaming {
		req.Stream = true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	if !cfg.Streaming {
		resp, err := aiClient.CreateChatCompletion(ctx, req)
		if err != nil {
			c.Cmd.Reply(e, err.Error())
			logger.Error(err.Error())
			return
		}

		logger.Info(resp.Choices[0].Message.Content)
		sendLoop(resp.Choices[0].Message.Content, network, c, e)
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
		sendLoop(buffer, network, c, e)
	}()
	for {
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
		buffer += resp.Choices[0].Delta.Content
		bufferb += resp.Choices[0].Delta.Content
		if before, after, found := strings.Cut(buffer, "\n"); found {
			sendLoop(before, network, c, e)
			buffer = after
		}
	}
}
