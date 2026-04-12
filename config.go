package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
	"golang.org/x/exp/rand"
)

type Config struct {
	Trigger            string
	Quitmsg            string
	Networks           map[string]Network
	Services           map[string]Service
	Commands           Commands
	PromptEnhancements map[string]PromptEnhancementConfig `toml:"promptenhancements"`
	Busymsgs           []string
	Ratemsgs           []string
	UploadURL          string `toml:"uploadurl"`
	Persist            PersistConfig
	MCPs               map[string]MCPConfig `toml:"mcps"`
}

type Network struct {
	Name       string
	Nick       string
	Servers    []Server
	nextServer int
	Channels   []string
	Enabled    bool
	Throttle   time.Duration
	Trigger    string
	Quitmsg    string
}

type Server struct {
	Host string
	Port int
	Pass string
	Ssl  bool
}

type Commands struct {
	Completions map[string]AIConfig
	Chats       map[string]AIConfig
	SD          map[string]SDConfig
	Comfy       map[string]ComfyConfig
}

type SDConfig struct {
	Name         string //gets set to key name
	Regex        string
	Service      string
	Steps        int64
	SamplerName  string `toml:"samplername"`
	SamplerIndex string `toml:"samplerindex"`
	Scheduler    string
	Width        int64
	Height       int64
	Description  string
}

type PromptEnhancementConfig struct {
	Service      string `toml:"service"`
	Model        string `toml:"model"`
	SystemPrompt string `toml:"systemprompt"`
}

type ComfyConfig struct {
	Name               string //gets set to key name
	Regex              string
	Service            string
	WorkflowPath       string   `toml:"workflow_path"`
	ClientID           string   `toml:"clientid"`
	OutputNode         string   `toml:"output_node"`
	PromptNode         string   `toml:"prompt_node"`
	NegativePromptNode string   `toml:"negative_prompt_node"`
	SeedNodes          []string `toml:"seed_nodes"`
	Timeout            int
	EnhancePrompt      string `toml:"enhanceprompt"`
	Description        string
}

type AIConfig struct {
	Name                string //gets set to key name
	Service             string
	Model               string
	Regex               string
	System              string
	SystemTmpl          *template.Template `json:"-"`
	Streaming           bool
	MaxTokens           int `toml:"maxtokens"`
	MaxCompletionTokens int `toml:"maxcompletiontokens"`
	Temperature         float32
	MaxHistory          int    `toml:"maxhistory"`
	RenderMarkdown      bool   `toml:"rendermarkdown"`
	DetectImages        bool   `toml:"detectimages"`
	MaxImages           int    `toml:"maximages"`
	MaxContextImages    int    `toml:"maxcontextimages"`
	ImageFormat         string `toml:"imageformat"`
	ImageQuality        int    `toml:"imagequality"`
	MaxImageSize        string `toml:"maximagesize"`
	MaxImageWidth       int    `toml:"-"`
	MaxImageHeight      int    `toml:"-"`
	Description         string
	MCPs                []string `toml:"mcps"`
}

type Service struct {
	Key                 string
	MaxTokens           int `toml:"maxtokens"`           //best to keep both for compatibility with non-openai
	MaxCompletionTokens int `toml:"maxcompletiontokens"` //use this one now with openai
	BaseURL             string
	Temperature         float32
	MaxHistory          int    `toml:"maxhistory"`
	ComfyTimeout        int    `toml:"comfy_timeout"` // WebSocket timeout in seconds
	ImageFormat         string `toml:"imageformat"`
	ImageQuality        int    `toml:"imagequality"`
	MaxImageSize        string `toml:"maximagesize"`
}

type MCPConfig struct {
	Transport string        `toml:"transport"` // "stdio" or "http"
	Command   string        `toml:"command"`
	Args      []string      `toml:"args"`
	URL       string        `toml:"url"`
	Timeout   time.Duration `toml:"timeout"`
}

type SystemPromptData struct {
	Nick      string
	BotNick   string
	Channel   string
	Network   string
	ChanNicks string // JSON array of channel nicks
}

func validateSystemPromptTemplate(tmpl *template.Template) error {
	dummy := SystemPromptData{Nick: "dummy", BotNick: "dummy", Channel: "dummy", Network: "dummy", ChanNicks: `["dummy1","dummy2"]`}
	var buf strings.Builder
	return tmpl.Execute(&buf, dummy)
}

func (config *Config) Busymsg() string {
	return config.Busymsgs[rand.Intn(len(config.Busymsgs))]
}

func (config *Config) Ratemsg() string {
	return config.Ratemsgs[rand.Intn(len(config.Ratemsgs))]
}

func (cfg *SDConfig) ApplyDefaults(service Service) {

}

func (cfg *ComfyConfig) ApplyDefaults(service Service) {
	if cfg.Timeout == 0 {
		cfg.Timeout = service.ComfyTimeout
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 300
	}
}

func (cfg *AIConfig) ApplyDefaults(service Service) {
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = service.MaxTokens
	}
	if cfg.MaxTokens == 0 {
		cfg.MaxCompletionTokens = service.MaxCompletionTokens
	}
	if cfg.Temperature == 0 {
		cfg.Temperature = service.Temperature
	}
	if cfg.MaxHistory == 0 {
		cfg.MaxHistory = service.MaxHistory
	}
	if cfg.MaxImages == 0 {
		cfg.MaxImages = 5
	}
	if cfg.MaxContextImages == 0 {
		cfg.MaxContextImages = 5
	}
	if cfg.ImageFormat == "" {
		cfg.ImageFormat = service.ImageFormat
		if cfg.ImageFormat == "" {
			cfg.ImageFormat = "jpg"
		}
	}
	if cfg.ImageQuality == 0 {
		cfg.ImageQuality = service.ImageQuality
		if cfg.ImageQuality == 0 {
			cfg.ImageQuality = 75
		}
	}
	if cfg.MaxImageSize == "" {
		cfg.MaxImageSize = service.MaxImageSize
		if cfg.MaxImageSize == "" {
			cfg.MaxImageSize = "1024x1024"
		}
	}
}

func (cfg AIConfig) MarshalJSON() ([]byte, error) {
	type Alias AIConfig
	return json.Marshal(Alias(cfg))
}

func (cfg *AIConfig) UnmarshalJSON(data []byte) error {
	type Alias AIConfig
	aux := &struct {
		*Alias
	}{
		Alias: (*Alias)(cfg),
	}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	cfg.SystemTmpl = nil
	return nil
}

func (n *Network) getNextServer() Server {
	defer func() {
		n.nextServer++
		if n.nextServer > len(n.Servers)-1 {
			n.nextServer = 0
		}
	}()
	return n.Servers[n.nextServer]
}

func (s *Server) GetPort() int {
	if s.Port != 0 {
		return s.Port
	}
	if s.Ssl {
		return 6697
	} else {
		return 6667
	}
}

func loadConfigOrDie(file string) (config Config) {
	_, err := toml.DecodeFile(file, &config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(config.Busymsgs) == 0 {
		config.Busymsgs = []string{"hold on i'm already busy"}
	}
	if len(config.Ratemsgs) == 0 {
		config.Ratemsgs = []string{"hold on you're going to fast"}
	}
	for name, network := range config.Networks {
		network.Name = name
		if network.Trigger == "" {
			network.Trigger = config.Trigger
		}
		if network.Quitmsg == "" {
			network.Quitmsg = config.Quitmsg
		}
		config.Networks[name] = network
	}

	for name, service := range config.Services {
		if service.MaxHistory == 0 {
			service.MaxHistory = 8
		}
		config.Services[name] = service
	}

	fff := func(cfg AIConfig, name string, config *Config) (AIConfig, error) {
		cfg.Name = name
		if cfg.Regex == "" {
			cfg.Regex = name
		}
		if service, ok := config.Services[cfg.Service]; ok {
			cfg.ApplyDefaults(service)
		} else {
			return cfg, fmt.Errorf("commands.completions.%s service %s is undefined", name, cfg.Service)
		}
		if cfg.RenderMarkdown && cfg.Streaming {
			// Streaming markdown supported with limitations:
			// - Tables omitted
			// - Code blocks: plain text only (no Chroma), fixed 80-char background padding
		}
		return cfg, nil
	}

	for name, cfg := range config.Commands.Completions {
		cfg, err := fff(cfg, name, &config)
		if err != nil {
			log.Fatalln("commands.completions."+name, err)
		}
		config.Commands.Completions[name] = cfg
	}

	for name, cfg := range config.Commands.Chats {
		cfg, err := fff(cfg, name, &config)
		if err != nil {
			log.Fatalln("commands.chats."+name, err)
		}
		if cfg.ImageFormat != "" && cfg.ImageFormat != "webp" && cfg.ImageFormat != "jpg" && cfg.ImageFormat != "jpeg" {
			log.Fatalln("commands.chats."+name, "imageformat must be 'webp', 'jpg', or 'jpeg'")
		}
		if cfg.ImageQuality < 1 || cfg.ImageQuality > 100 {
			log.Fatalln("commands.chats."+name, "imagequality must be between 1 and 100")
		}
		if cfg.MaxImageSize != "" {
			parts := strings.Split(cfg.MaxImageSize, "x")
			if len(parts) != 2 {
				log.Fatalln("commands.chats."+name, "maximagesize must be in format WxH (e.g., 1024x1024)")
			}
			var w, h int
			if _, err := fmt.Sscanf(parts[0], "%d", &w); err != nil {
				log.Fatalln("commands.chats."+name, "invalid maximagesize width:", err)
			}
			if _, err := fmt.Sscanf(parts[1], "%d", &h); err != nil {
				log.Fatalln("commands.chats."+name, "invalid maximagesize height:", err)
			}
			cfg.MaxImageWidth = w
			cfg.MaxImageHeight = h
		}
		if cfg.System != "" {
			tmpl, err := template.New(name + "_system").Parse(cfg.System)
			if err != nil {
				log.Fatalln("commands.chats."+name, "system prompt template parse error:", err)
			}
			cfg.SystemTmpl = tmpl
			if err := validateSystemPromptTemplate(cfg.SystemTmpl); err != nil {
				log.Fatalln("commands.chats."+name, "system prompt template validation error:", err)
			}
		}
		config.Commands.Chats[name] = cfg
	}

	for name, cfg := range config.Commands.SD {
		cfg.Name = name
		if cfg.Regex == "" {
			cfg.Regex = name
		}
		if service, ok := config.Services[cfg.Service]; ok {
			cfg.ApplyDefaults(service)
		} else {
			log.Fatalln("commands.SD."+name, "service", cfg.Service, "is undefined")
		}
		config.Commands.SD[name] = cfg
	}

	for name, cfg := range config.Commands.Comfy {
		cfg.Name = name
		if cfg.Regex == "" {
			cfg.Regex = name
		}
		if service, ok := config.Services[cfg.Service]; ok {
			cfg.ApplyDefaults(service)
		} else {
			log.Fatalln("commands.comfy."+name, "service", cfg.Service, "is undefined")
		}
		if cfg.WorkflowPath == "" {
			log.Fatalln("commands.comfy." + name + " workflow_path is required")
		}
		if cfg.ClientID == "" {
			log.Fatalln("commands.comfy." + name + " clientid is required")
		}
		if cfg.OutputNode == "" {
			log.Fatalln("commands.comfy." + name + " output_node is required")
		}
		if cfg.PromptNode == "" {
			log.Fatalln("commands.comfy." + name + " prompt_node is required")
		}
		if cfg.EnhancePrompt != "" {
			if _, ok := config.PromptEnhancements[cfg.EnhancePrompt]; !ok {
				log.Fatalln("commands.comfy."+name, "enhanceprompt", cfg.EnhancePrompt, "is not defined in [promptenhancements]")
			}
			enhCfg := config.PromptEnhancements[cfg.EnhancePrompt]
			if enhCfg.Service == "" {
				log.Fatalln("promptenhancements."+cfg.EnhancePrompt, "service is required")
			}
			if _, ok := config.Services[enhCfg.Service]; !ok {
				log.Fatalln("promptenhancements."+cfg.EnhancePrompt, "service", enhCfg.Service, "is undefined")
			}
			if enhCfg.Model == "" {
				log.Fatalln("promptenhancements."+cfg.EnhancePrompt, "model is required")
			}
			if enhCfg.SystemPrompt == "" {
				log.Fatalln("promptenhancements."+cfg.EnhancePrompt, "systemprompt is required")
			}
		}
		config.Commands.Comfy[name] = cfg
	}

	for name, mcpCfg := range config.MCPs {
		if mcpCfg.Transport == "" {
			log.Fatalln("mcps." + name + " transport is required (stdio or http)")
		}
		if mcpCfg.Transport != "stdio" && mcpCfg.Transport != "http" {
			log.Fatalln("mcps." + name + " transport must be 'stdio' or 'http'")
		}
		if mcpCfg.Transport == "stdio" && mcpCfg.Command == "" {
			log.Fatalln("mcps." + name + " command is required for stdio transport")
		}
		if mcpCfg.Transport == "http" && mcpCfg.URL == "" {
			log.Fatalln("mcps." + name + " url is required for http transport")
		}
		if mcpCfg.Timeout == 0 {
			mcpCfg.Timeout = 30 * time.Second
		}
		config.MCPs[name] = mcpCfg
	}

	validateMCPRefs := func(section, name string, cfg AIConfig) {
		for _, mcpName := range cfg.MCPs {
			if _, ok := config.MCPs[mcpName]; !ok {
				log.Fatalln(section+"."+name, "mcps references undefined MCP:", mcpName)
			}
		}
	}

	for name, cfg := range config.Commands.Completions {
		validateMCPRefs("commands.completions", name, cfg)
		config.Commands.Completions[name] = cfg
	}
	for name, cfg := range config.Commands.Chats {
		validateMCPRefs("commands.chats", name, cfg)
		config.Commands.Chats[name] = cfg
	}

	config.Persist.SetDefaults()

	return
}
