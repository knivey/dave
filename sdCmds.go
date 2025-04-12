package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"

	"fmt"
)

type Txt2imgParams struct {
	Prompt       string        `json:"prompt,omitempty"`
	Height       int64         `json:"height,omitempty"`
	Width        int64         `json:"width,omitempty"`
	Steps        int64         `json:"steps,omitempty"`
	SamplerIndex string        `json:"sampler_index,omitempty"`
	Scheduler    string        `json:"scheduler,omitempty"`
	SamplerName  string        `json:"sampler_name,omitempty"`
	SendImages   bool          `json:"send_images"`
	CfgScale     int64         `json:"cfg_scale",omitempty`
	ScriptArgs   []interface{} `json:"script_args"`
}

type Txt2imgResponse struct {
	Images     []string    `json:"images"`
	Info       *string     `json:"info"`
	Parameters interface{} `json:"parameters"`
}

func sd(network Network, c *girc.Client, e girc.Event, cfg SDConfig, args ...string) {
	logger := logxi.New(network.Name + ".sd." + cfg.Name)
	logger.SetLevel(logxi.LevelAll)

	startedRunning(network.Name + e.Params[0])
	defer stoppedRunning(network.Name + e.Params[0])

	URL := config.Services[cfg.Service].BaseURL + "/sdapi/v1/txt2img"

	params := Txt2imgParams{
		Prompt:       args[0],
		Steps:        cfg.Steps,
		SamplerName:  cfg.SamplerName,
		SamplerIndex: cfg.SamplerIndex,
		Scheduler:    cfg.Scheduler,
		Width:        cfg.Width,
		Height:       cfg.Height,
		CfgScale:     1,
		SendImages:   true,
		ScriptArgs:   []interface{}{},
	}
	logger.Debug("starting sd run with params", params)
	jsonData, err := json.Marshal(params)
	if err != nil {
		c.Cmd.Reply(e, err.Error())
		logger.Error(err.Error())
	}
	req, err := http.NewRequest("POST", URL, bytes.NewBuffer(jsonData))
	if err != nil {
		c.Cmd.Reply(e, err.Error())
		logger.Error(err.Error())
	}

	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: time.Second * 120,
	}
	resp, err := client.Do(req)
	if err != nil {
		c.Cmd.Reply(e, err.Error())
		logger.Error(err.Error())
		return
	}

	defer resp.Body.Close()
	logger.Debug("Response status:", resp.Status)

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.Cmd.Reply(e, err.Error())
		logger.Error(err.Error())
		return
	}

	var respData Txt2imgResponse
	json.Unmarshal(body, &respData)
	logger.Debug("Response Parameters:", respData.Parameters)
	logger.Debug("Response Info:", respData.Info)

	for i, s := range respData.Images {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			c.Cmd.Reply(e, err.Error())
			logger.Error(err.Error())
			return
		}
		url, err := uploadDotBeer(b, fmt.Sprintf("%d.webp", i))
		if err != nil {
			c.Cmd.Reply(e, err.Error())
			logger.Error(err.Error())
			return
		}
		c.Cmd.Reply(e, url)
	}
	c.Cmd.Reply(e, "All done ;)")
}
