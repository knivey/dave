package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func loadConfigDirOrDie(dir string) Config {
	config, err := loadConfigDir(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return config
}

func loadConfigDir(dir string) (Config, error) {
	var config Config

	mainFile := filepath.Join(dir, "config.toml")
	if _, err := toml.DecodeFile(mainFile, &config); err != nil {
		return config, fmt.Errorf("loading %s: %w", mainFile, err)
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

	if err := loadServicesFile(dir, &config); err != nil {
		return config, err
	}

	if err := loadPromptEnhancementsFile(dir, &config); err != nil {
		return config, err
	}

	if err := loadCommandsInto(dir, &config); err != nil {
		return config, err
	}

	for name, mcpCfg := range config.MCPs {
		if mcpCfg.Transport == "" {
			return config, fmt.Errorf("mcps.%s transport is required (stdio or http)", name)
		}
		if mcpCfg.Transport != "stdio" && mcpCfg.Transport != "http" {
			return config, fmt.Errorf("mcps.%s transport must be 'stdio' or 'http'", name)
		}
		if mcpCfg.Transport == "stdio" && mcpCfg.Command == "" {
			return config, fmt.Errorf("mcps.%s command is required for stdio transport", name)
		}
		if mcpCfg.Transport == "http" && mcpCfg.URL == "" {
			return config, fmt.Errorf("mcps.%s url is required for http transport", name)
		}
		if mcpCfg.Timeout == 0 {
			mcpCfg.Timeout = 30 * time.Second
		}
		config.MCPs[name] = mcpCfg
	}

	config.Persist.SetDefaults()

	return config, nil
}

func loadServicesFile(dir string, config *Config) error {
	if err := loadCommandFile(filepath.Join(dir, "services.toml"), &config.Services); err != nil {
		return fmt.Errorf("loading services: %w", err)
	}
	if config.Services == nil {
		config.Services = make(map[string]Service)
	}
	for name, service := range config.Services {
		if service.MaxHistory == 0 {
			service.MaxHistory = 8
		}
		config.Services[name] = service
	}
	return nil
}

func loadPromptEnhancementsFile(dir string, config *Config) error {
	if err := loadCommandFile(filepath.Join(dir, "promptenhancements.toml"), &config.PromptEnhancements); err != nil {
		return fmt.Errorf("loading promptenhancements: %w", err)
	}
	if config.PromptEnhancements == nil {
		config.PromptEnhancements = make(map[string]PromptEnhancementConfig)
	}
	return nil
}

func loadReloadableDir(dir string, config *Config) error {
	var tmpConfig Config
	tmpConfig.MCPs = config.MCPs

	if err := loadServicesFile(dir, &tmpConfig); err != nil {
		return err
	}

	if err := loadPromptEnhancementsFile(dir, &tmpConfig); err != nil {
		return err
	}

	commands, err := loadCommandsDir(dir, &tmpConfig)
	if err != nil {
		return err
	}

	config.Services = tmpConfig.Services
	config.PromptEnhancements = tmpConfig.PromptEnhancements
	config.Commands = commands

	return nil
}

func loadCommandsInto(dir string, config *Config) error {
	commands, err := loadCommandsDir(dir, config)
	if err != nil {
		return err
	}
	config.Commands = commands
	return nil
}

func loadCommandsDir(dir string, config *Config) (Commands, error) {
	var commands Commands
	commands.Completions = make(map[string]AIConfig)
	commands.Chats = make(map[string]AIConfig)
	commands.SD = make(map[string]SDConfig)
	commands.Comfy = make(map[string]ComfyConfig)

	if err := loadCommandFile(filepath.Join(dir, "completions.toml"), &commands.Completions); err != nil {
		return commands, fmt.Errorf("loading completions: %w", err)
	}
	if err := loadCommandFile(filepath.Join(dir, "chats.toml"), &commands.Chats); err != nil {
		return commands, fmt.Errorf("loading chats: %w", err)
	}
	if err := loadCommandFile(filepath.Join(dir, "sd.toml"), &commands.SD); err != nil {
		return commands, fmt.Errorf("loading sd: %w", err)
	}
	if err := loadCommandFile(filepath.Join(dir, "comfy.toml"), &commands.Comfy); err != nil {
		return commands, fmt.Errorf("loading comfy: %w", err)
	}

	if err := validateCommands(&commands, config); err != nil {
		return commands, err
	}

	return commands, nil
}

func loadCommandFile(path string, dest interface{}) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	_, err := toml.DecodeFile(path, dest)
	return err
}

func validateCommands(commands *Commands, config *Config) error {
	for name, cfg := range commands.Completions {
		cfg, err := validateAIConfig(cfg, name, "completions", config)
		if err != nil {
			return err
		}
		if err := validateMCPRefsFor("completions", name, cfg.MCPs, config); err != nil {
			return err
		}
		commands.Completions[name] = cfg
	}

	for name, cfg := range commands.Chats {
		cfg, err := validateAIConfig(cfg, name, "chats", config)
		if err != nil {
			return err
		}
		if cfg.ImageFormat != "" && cfg.ImageFormat != "webp" && cfg.ImageFormat != "jpg" && cfg.ImageFormat != "jpeg" {
			return fmt.Errorf("commands.chats.%s imageformat must be 'webp', 'jpg', or 'jpeg'", name)
		}
		if cfg.ImageQuality < 1 || cfg.ImageQuality > 100 {
			return fmt.Errorf("commands.chats.%s imagequality must be between 1 and 100", name)
		}
		if cfg.MaxImageSize != "" {
			parts := strings.Split(cfg.MaxImageSize, "x")
			if len(parts) != 2 {
				return fmt.Errorf("commands.chats.%s maximagesize must be in format WxH (e.g., 1024x1024)", name)
			}
			var w, h int
			if _, err := fmt.Sscanf(parts[0], "%d", &w); err != nil {
				return fmt.Errorf("commands.chats.%s invalid maximagesize width: %w", name, err)
			}
			if _, err := fmt.Sscanf(parts[1], "%d", &h); err != nil {
				return fmt.Errorf("commands.chats.%s invalid maximagesize height: %w", name, err)
			}
			cfg.MaxImageWidth = w
			cfg.MaxImageHeight = h
		}
		if cfg.System != "" {
			tmpl, err := template.New(name + "_system").Parse(cfg.System)
			if err != nil {
				return fmt.Errorf("commands.chats.%s system prompt template parse error: %w", name, err)
			}
			cfg.SystemTmpl = tmpl
			if err := validateSystemPromptTemplate(cfg.SystemTmpl); err != nil {
				return fmt.Errorf("commands.chats.%s system prompt template validation error: %w", name, err)
			}
		}
		if err := validateMCPRefsFor("chats", name, cfg.MCPs, config); err != nil {
			return err
		}
		commands.Chats[name] = cfg
	}

	for name, cfg := range commands.SD {
		cfg.Name = name
		if cfg.Regex == "" {
			cfg.Regex = name
		}
		if service, ok := config.Services[cfg.Service]; ok {
			cfg.ApplyDefaults(service)
		} else {
			return fmt.Errorf("commands.sd.%s service %s is undefined", name, cfg.Service)
		}
		commands.SD[name] = cfg
	}

	for name, cfg := range commands.Comfy {
		cfg.Name = name
		if cfg.Regex == "" {
			cfg.Regex = name
		}
		if service, ok := config.Services[cfg.Service]; ok {
			cfg.ApplyDefaults(service)
		} else {
			return fmt.Errorf("commands.comfy.%s service %s is undefined", name, cfg.Service)
		}
		if cfg.WorkflowPath == "" {
			return fmt.Errorf("commands.comfy.%s workflow_path is required", name)
		}
		if cfg.ClientID == "" {
			return fmt.Errorf("commands.comfy.%s clientid is required", name)
		}
		if cfg.OutputNode == "" {
			return fmt.Errorf("commands.comfy.%s output_node is required", name)
		}
		if cfg.PromptNode == "" {
			return fmt.Errorf("commands.comfy.%s prompt_node is required", name)
		}
		if cfg.EnhancePrompt != "" {
			if _, ok := config.PromptEnhancements[cfg.EnhancePrompt]; !ok {
				return fmt.Errorf("commands.comfy.%s enhanceprompt %s is not defined in [promptenhancements]", name, cfg.EnhancePrompt)
			}
			enhCfg := config.PromptEnhancements[cfg.EnhancePrompt]
			if enhCfg.Service == "" {
				return fmt.Errorf("promptenhancements.%s service is required", cfg.EnhancePrompt)
			}
			if _, ok := config.Services[enhCfg.Service]; !ok {
				return fmt.Errorf("promptenhancements.%s service %s is undefined", cfg.EnhancePrompt, enhCfg.Service)
			}
			if enhCfg.Model == "" {
				return fmt.Errorf("promptenhancements.%s model is required", cfg.EnhancePrompt)
			}
			if enhCfg.SystemPrompt == "" {
				return fmt.Errorf("promptenhancements.%s systemprompt is required", cfg.EnhancePrompt)
			}
		}
		commands.Comfy[name] = cfg
	}

	return nil
}

func validateAIConfig(cfg AIConfig, name, section string, config *Config) (AIConfig, error) {
	cfg.Name = name
	if cfg.Regex == "" {
		cfg.Regex = name
	}
	if service, ok := config.Services[cfg.Service]; ok {
		cfg.ApplyDefaults(service)
	} else {
		return cfg, fmt.Errorf("commands.%s.%s service %s is undefined", section, name, cfg.Service)
	}
	return cfg, nil
}

func validateMCPRefsFor(section, name string, mcpRefs []string, config *Config) error {
	for _, mcpName := range mcpRefs {
		if _, ok := config.MCPs[mcpName]; !ok {
			return fmt.Errorf("commands.%s.%s mcps references undefined MCP: %s", section, name, mcpName)
		}
	}
	return nil
}
