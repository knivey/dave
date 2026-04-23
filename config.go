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
	Trigger           string
	Quitmsg           string
	Networks          map[string]Network
	Services          map[string]Service
	Commands          Commands
	Busymsgs          []string
	Ratemsgs          []string
	UploadURL         string `toml:"uploadurl"`
	Database          DatabaseConfig
	MCPs              map[string]MCPConfig `toml:"mcps"`
	TUI               TUIConfig
	APILog            APILogConfig      `toml:"api_log"`
	TemplateVars      map[string]string `toml:"-"`
	MaxSessionHistory int               `toml:"max_session_history"`
}

type TUIConfig struct {
	ScrollbackLines int                `toml:"scrollback_lines"`
	Scrollbar       TUIScrollbarConfig `toml:"scrollbar"`
}

type TUIScrollbarConfig struct {
	Visible         *bool  `toml:"visible"`
	ShowAlways      *bool  `toml:"show_always"`
	Color           string `toml:"color"`
	BackgroundColor string `toml:"background_color"`
	TrackColor      string `toml:"track_color"`
	Width           int    `toml:"width"`
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
	Tools       map[string]MCPCommandConfig
}

type MCPCommandConfig struct {
	Name        string
	Regex       string
	MCP         string         `toml:"mcp"`
	Tool        string         `toml:"tool"`
	Arg         string         `toml:"arg"`
	Args        map[string]any `toml:"args"`
	Timeout     time.Duration  `toml:"timeout"`
	SkipBusy    bool           `toml:"skipbusy"`
	Description string
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
	MCPs                []string       `toml:"mcps"`
	TopP                float32        `toml:"topp"`
	Stop                []string       `toml:"stop"`
	PresencePenalty     float32        `toml:"presencepenalty"`
	FrequencyPenalty    float32        `toml:"frequencypenalty"`
	ParallelToolCalls   *bool          `toml:"paralleltoolcalls"`
	ReasoningEffort     string         `toml:"reasoningeffort"`
	ServiceTier         string         `toml:"servicetier"`
	Verbosity           string         `toml:"verbosity"`
	ChatTemplateKwargs  map[string]any `toml:"chat_template_kwargs"`
	ExtraBody           map[string]any `toml:"extra_body"`
	Timeout             time.Duration  `toml:"timeout"`
	StreamTimeout       time.Duration  `toml:"streamtimeout"`
	ToolVerbose         *bool          `toml:"toolverbose"`
}

type Service struct {
	Key                 string
	Type                string // "openai" (default) or "llama"
	MaxTokens           int    `toml:"maxtokens"`
	MaxCompletionTokens int    `toml:"maxcompletiontokens"`
	BaseURL             string
	Temperature         float32
	MaxHistory          int           `toml:"maxhistory"`
	ImageFormat         string        `toml:"imageformat"`
	ImageQuality        int           `toml:"imagequality"`
	MaxImageSize        string        `toml:"maximagesize"`
	Timeout             time.Duration `toml:"timeout"`
	StreamTimeout       time.Duration `toml:"streamtimeout"`
	ToolVerbose         *bool         `toml:"toolverbose"`
	ParallelToolCalls   *bool         `toml:"paralleltoolcalls"`
}

type MCPConfig struct {
	Transport string        `toml:"transport"` // "stdio" or "http"
	Command   string        `toml:"command"`
	Args      []string      `toml:"args"`
	URL       string        `toml:"url"`
	Timeout   time.Duration `toml:"timeout"`
	KeepAlive time.Duration `toml:"keepalive"` // ping interval for liveness; default 30s for http, 0 for stdio
}

type SystemPromptData struct {
	Nick      string
	BotNick   string
	Channel   string
	Network   string
	ChanNicks string
	Vars      map[string]string
}

func validateSystemPromptTemplate(tmpl *template.Template) error {
	dummy := SystemPromptData{Nick: "dummy", BotNick: "dummy", Channel: "dummy", Network: "dummy", ChanNicks: `["dummy1","dummy2"]`, Vars: map[string]string{"example": "test"}}
	var buf strings.Builder
	return tmpl.Execute(&buf, dummy)
}

func (config *Config) Busymsg() string {
	return config.Busymsgs[rand.Intn(len(config.Busymsgs))]
}

func (config *Config) Ratemsg() string {
	return config.Ratemsgs[rand.Intn(len(config.Ratemsgs))]
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
	if cfg.Timeout == 0 {
		cfg.Timeout = service.Timeout
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.StreamTimeout == 0 {
		cfg.StreamTimeout = service.StreamTimeout
	}
	if cfg.StreamTimeout == 0 {
		cfg.StreamTimeout = cfg.Timeout
	}
	if cfg.ToolVerbose == nil {
		cfg.ToolVerbose = service.ToolVerbose
	}
	if cfg.ParallelToolCalls == nil {
		cfg.ParallelToolCalls = service.ParallelToolCalls
	}
	if cfg.ParallelToolCalls == nil {
		defaultTrue := true
		cfg.ParallelToolCalls = &defaultTrue
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
	if config.TUI.ScrollbackLines == 0 {
		config.TUI.ScrollbackLines = 5000
	}
	if config.TUI.Scrollbar.Color == "" {
		config.TUI.Scrollbar.Color = "gray"
	}
	if config.TUI.Scrollbar.BackgroundColor == "" {
		config.TUI.Scrollbar.BackgroundColor = "black"
	}
	if config.TUI.Scrollbar.TrackColor == "" {
		config.TUI.Scrollbar.TrackColor = "darkgray"
	}
	if config.TUI.Scrollbar.Width == 0 {
		config.TUI.Scrollbar.Width = 1
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

	if err := loadMCPsFile(dir, &config); err != nil {
		return config, err
	}

	if err := loadCommandsInto(dir, &config); err != nil {
		return config, err
	}

	if err := loadTemplateVarsFile(dir, &config); err != nil {
		return config, err
	}

	config.Database.SetDefaults()

	if config.MaxSessionHistory == 0 {
		config.MaxSessionHistory = 10
	}

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

func loadMCPsFile(dir string, config *Config) error {
	if err := loadCommandFile(filepath.Join(dir, "mcps.toml"), &config.MCPs); err != nil {
		return fmt.Errorf("loading mcps: %w", err)
	}
	if config.MCPs == nil {
		config.MCPs = make(map[string]MCPConfig)
	}
	for name, mcpCfg := range config.MCPs {
		if mcpCfg.Transport == "" {
			return fmt.Errorf("mcps.%s transport is required (stdio or http)", name)
		}
		if mcpCfg.Transport != "stdio" && mcpCfg.Transport != "http" {
			return fmt.Errorf("mcps.%s transport must be 'stdio' or 'http'", name)
		}
		if mcpCfg.Transport == "stdio" && mcpCfg.Command == "" {
			return fmt.Errorf("mcps.%s command is required for stdio transport", name)
		}
		if mcpCfg.Transport == "http" && mcpCfg.URL == "" {
			return fmt.Errorf("mcps.%s url is required for http transport", name)
		}
		if mcpCfg.Timeout == 0 {
			mcpCfg.Timeout = 30 * time.Second
		}
		if mcpCfg.KeepAlive == 0 && mcpCfg.Transport == "http" {
			mcpCfg.KeepAlive = 30 * time.Second
		}
		config.MCPs[name] = mcpCfg
	}
	return nil
}

func loadTemplateVarsFile(dir string, config *Config) error {
	path := filepath.Join(dir, "templatevars.toml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if config.TemplateVars == nil {
			config.TemplateVars = make(map[string]string)
		}
		return nil
	}
	if _, err := toml.DecodeFile(path, &config.TemplateVars); err != nil {
		return fmt.Errorf("loading templatevars: %w", err)
	}
	if config.TemplateVars == nil {
		config.TemplateVars = make(map[string]string)
	}
	return nil
}

func loadReloadableDir(dir string, config *Config) error {
	var tmpConfig Config

	if err := loadMCPsFile(dir, &tmpConfig); err != nil {
		return err
	}

	if err := loadServicesFile(dir, &tmpConfig); err != nil {
		return err
	}

	if err := loadTemplateVarsFile(dir, &tmpConfig); err != nil {
		return err
	}

	commands, err := loadCommandsDir(dir, &tmpConfig)
	if err != nil {
		return err
	}

	config.MCPs = tmpConfig.MCPs
	config.Services = tmpConfig.Services
	config.Commands = commands
	config.TemplateVars = tmpConfig.TemplateVars

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
	commands.Tools = make(map[string]MCPCommandConfig)

	if err := loadCommandFile(filepath.Join(dir, "completions.toml"), &commands.Completions); err != nil {
		return commands, fmt.Errorf("loading completions: %w", err)
	}
	if err := loadCommandFile(filepath.Join(dir, "chats.toml"), &commands.Chats); err != nil {
		return commands, fmt.Errorf("loading chats: %w", err)
	}
	if err := loadCommandFile(filepath.Join(dir, "tools.toml"), &commands.Tools); err != nil {
		return commands, fmt.Errorf("loading tools: %w", err)
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

	for name, cfg := range commands.Tools {
		cfg.Name = name
		if cfg.Regex == "" {
			cfg.Regex = name
		}
		if cfg.MCP == "" {
			return fmt.Errorf("commands.tools.%s mcp is required", name)
		}
		if mcpCfg, ok := config.MCPs[cfg.MCP]; ok {
			if cfg.Timeout == 0 {
				cfg.Timeout = mcpCfg.Timeout
			}
		} else {
			return fmt.Errorf("commands.tools.%s mcp %s is undefined", name, cfg.MCP)
		}
		if cfg.Tool == "" {
			return fmt.Errorf("commands.tools.%s tool is required", name)
		}
		commands.Tools[name] = cfg
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
