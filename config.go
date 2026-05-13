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

var globalRand = rand.New(rand.NewSource(uint64(time.Now().UnixNano())))

type Config struct {
	Trigger              string
	Quitmsg              string
	Networks             map[string]Network
	Services             map[string]Service
	Commands             Commands
	MaxQueueDepth        int    `toml:"max_queue_depth"`
	UploadURL            string `toml:"uploadurl"`
	Database             DatabaseConfig
	MCPs                 map[string]MCPConfig `toml:"mcps"`
	TUI                  TUIConfig
	APILog               APILogConfig        `toml:"api_log"`
	IncidentLog          IncidentConfig      `toml:"incident_log"`
	Pastebin             PastebinConfig      `toml:"pastebin"`
	TemplateVars         map[string]string   `toml:"-"`
	SessionsDisplayLimit int                 `toml:"sessions_display_limit"`
	Notices              NoticesConfig       `toml:"-"`
	HiddenTools          []string            `toml:"hidden_tools"`
	DisabledBuiltins     []string            `toml:"disabled_builtins"`
	DisabledBuiltinTools []string            `toml:"disabled_builtin_tools"`
	HiddenMCPTools       []string            `toml:"hidden_mcp_tools"`
	HiddenMCPToolSets    []string            `toml:"hidden_mcp_tool_sets"`
	MCPToolSets          map[string][]string `toml:"-"`
	Bans                 BanConfig           `toml:"bans"`
	Compaction           CompactionConfig    `toml:"compaction"`
}

type BanConfig struct {
	MaxDuration     string `toml:"max_duration"`
	DefaultDuration string `toml:"default_duration"`
}

type PastebinConfig struct {
	URL                  string `toml:"url"`
	APIKey               string `toml:"api_key"`
	PastebinPreviewLines *int   `toml:"pastebin_preview_lines"`
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

type ChannelConfig struct {
	Key                  string `toml:"key"`
	Pastebin             bool   `toml:"pastebin"`
	MaxLines             int    `toml:"max_lines"`
	PastebinPreviewLines *int   `toml:"pastebin_preview_lines"`
}

func (c ChannelConfig) GetMaxLines() int {
	if c.MaxLines <= 0 {
		return 5
	}
	return c.MaxLines
}

func (c ChannelConfig) GetPastebinPreviewLines(pastebinCfg PastebinConfig) int {
	maxLines := c.GetMaxLines()
	var value int
	var isSet bool

	if c.PastebinPreviewLines != nil {
		value = *c.PastebinPreviewLines
		isSet = true
	} else if pastebinCfg.PastebinPreviewLines != nil {
		value = *pastebinCfg.PastebinPreviewLines
		isSet = true
	}

	if !isSet {
		value = 3
	}

	if value < 0 {
		return 0
	}
	if value >= maxLines {
		return maxLines - 1
	}
	return value
}

type Network struct {
	Name           string
	Nick           string
	User           string `toml:"user"`
	RealName       string `toml:"real_name"`
	Servers        []Server
	nextServer     int
	Channels       map[string]ChannelConfig
	Enabled        *bool
	Throttle       time.Duration
	ReconnectDelay *time.Duration `toml:"reconnect_delay"`
	Trigger        string
	Quitmsg        string
	Casemapping    string `toml:"-"`
}

func (n *Network) GetChannelConfig(channel string) ChannelConfig {
	if n.Channels == nil {
		return ChannelConfig{}
	}
	cm := n.Casemapping
	if cm == "" {
		cm = "rfc1459"
	}
	normalized := normalizeIRC(channel, cm)
	if cfg, ok := n.Channels[normalized]; ok {
		return cfg
	}
	for k, v := range n.Channels {
		if normalizeIRC(k, cm) == normalized {
			return v
		}
	}
	return ChannelConfig{}
}

func (n *Network) IsEnabled() bool {
	if n.Enabled == nil {
		return true
	}
	return *n.Enabled
}

type Server struct {
	Host               string `toml:"host"`
	Port               int    `toml:"port"`
	Pass               string `toml:"pass"`
	Ssl                bool   `toml:"ssl"`
	InsecureSkipVerify bool   `toml:"insecure_skip_verify"`
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
	Sync        bool   `toml:"sync"`
	AsyncTool   string `toml:"async_tool"`
}

func (c MCPCommandConfig) GetAsyncTool() string {
	if c.AsyncTool != "" {
		return c.AsyncTool
	}
	return c.Tool + "_async"
}

type AIConfig struct {
	Name                 string //gets set to key name
	Service              string
	Model                string
	Regex                string
	System               string
	SystemTmpl           *template.Template `json:"-"`
	Streaming            bool
	MaxTokens            int `toml:"maxtokens"`
	MaxCompletionTokens  int `toml:"maxcompletiontokens"`
	Temperature          float32
	MaxHistory           int    `toml:"maxhistory"`
	RenderMarkdown       bool   `toml:"rendermarkdown"`
	DetectImages         bool   `toml:"detectimages"`
	MaxImages            int    `toml:"maximages"`
	MaxContextImages     int    `toml:"maxcontextimages"`
	ImageFormat          string `toml:"imageformat"`
	ImageQuality         int    `toml:"imagequality"`
	MaxImageSize         string `toml:"maximagesize"`
	MaxImageWidth        int    `toml:"-"`
	MaxImageHeight       int    `toml:"-"`
	Description          string
	MCPs                 []string           `toml:"mcps"`
	TopP                 float32            `toml:"topp"`
	Stop                 []string           `toml:"stop"`
	PresencePenalty      float32            `toml:"presencepenalty"`
	FrequencyPenalty     float32            `toml:"frequencypenalty"`
	ParallelToolCalls    *bool              `toml:"paralleltoolcalls"`
	ReasoningEffort      string             `toml:"reasoningeffort"`
	ServiceTier          string             `toml:"servicetier"`
	Verbosity            string             `toml:"verbosity"`
	ChatTemplateKwargs   map[string]any     `toml:"chat_template_kwargs"`
	ExtraBody            map[string]any     `toml:"extra_body"`
	Timeout              time.Duration      `toml:"timeout"`
	StreamTimeout        time.Duration      `toml:"streamtimeout"`
	ToolVerbose          *bool              `toml:"toolverbose"`
	ResponsesAPI         bool               `toml:"responses_api"`
	PreviousResponseID   bool               `toml:"previous_response_id"`
	NeedsUserSuffix      bool               `toml:"needsusersuffix"`
	APIUser              string             `toml:"api_user"`
	RetryOnEmpty         *int               `toml:"retry_on_empty"`
	DisabledBuiltinTools []string           `toml:"disabled_builtin_tools"`
	HiddenMCPTools       []string           `toml:"hidden_mcp_tools"`
	HiddenMCPToolSets    []string           `toml:"hidden_mcp_tool_sets"`
	apiUserTmpl          *template.Template `json:"-"`
}

type Service struct {
	Key                  string
	Type                 string // "openai" (default) or "llama"
	MaxTokens            int    `toml:"maxtokens"`
	MaxCompletionTokens  int    `toml:"maxcompletiontokens"`
	BaseURL              string
	Temperature          float32
	MaxHistory           int           `toml:"maxhistory"`
	ImageFormat          string        `toml:"imageformat"`
	ImageQuality         int           `toml:"imagequality"`
	MaxImageSize         string        `toml:"maximagesize"`
	Timeout              time.Duration `toml:"timeout"`
	StreamTimeout        time.Duration `toml:"streamtimeout"`
	ToolVerbose          *bool         `toml:"toolverbose"`
	ParallelToolCalls    *bool         `toml:"paralleltoolcalls"`
	Parallel             int           `toml:"parallel"`
	APIUser              string        `toml:"api_user"`
	DisabledBuiltinTools []string      `toml:"disabled_builtin_tools"`
	HiddenMCPTools       []string      `toml:"hidden_mcp_tools"`
	HiddenMCPToolSets    []string      `toml:"hidden_mcp_tool_sets"`
}

type MCPConfig struct {
	Transport string            `toml:"transport"` // "stdio", "http", or "sse"
	Command   string            `toml:"command"`
	Args      []string          `toml:"args"`
	Env       []string          `toml:"env"`
	URL       string            `toml:"url"`
	Timeout   time.Duration     `toml:"timeout"`
	KeepAlive time.Duration     `toml:"keepalive"` // ping interval for liveness; default 30s for http, 0 for stdio
	Headers   map[string]string `toml:"headers"`
}

type SystemPromptData struct {
	Nick      string
	BotNick   string
	Channel   string
	Network   string
	ChanNicks string
	Date      string
	Vars      map[string]string
}

func validateSystemPromptTemplate(tmpl *template.Template) error {
	dummy := SystemPromptData{Nick: "dummy", BotNick: "dummy", Channel: "dummy", Network: "dummy", ChanNicks: `["dummy1","dummy2"]`, Date: "2025-01-01", Vars: map[string]string{"example": "test"}}
	var buf strings.Builder
	return tmpl.Execute(&buf, dummy)
}

func validateAPIUserTemplate(tmpl *template.Template) error {
	dummy := SystemPromptData{Nick: "dummy", BotNick: "dummy", Channel: "dummy", Network: "dummy", ChanNicks: `["dummy1","dummy2"]`, Date: "2025-01-01", Vars: map[string]string{"example": "test"}}
	var buf strings.Builder
	return tmpl.Execute(&buf, dummy)
}

func (cfg *AIConfig) ApplyDefaults(service Service) {
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = service.MaxTokens
	}
	if cfg.MaxCompletionTokens == 0 {
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
	if cfg.APIUser == "" {
		cfg.APIUser = service.APIUser
	}
	if cfg.RetryOnEmpty == nil {
		defaultRetry := 1
		cfg.RetryOnEmpty = &defaultRetry
	}
	if cfg.DisabledBuiltinTools == nil {
		cfg.DisabledBuiltinTools = service.DisabledBuiltinTools
	}
	if cfg.HiddenMCPTools == nil {
		cfg.HiddenMCPTools = service.HiddenMCPTools
	}
	if cfg.HiddenMCPToolSets == nil {
		cfg.HiddenMCPToolSets = service.HiddenMCPToolSets
	}
}

func (cfg *AIConfig) resolveHiddenMCPTools(sets map[string][]string) []string {
	return resolveHiddenMCPToolsFrom(sets, cfg.HiddenMCPTools, cfg.HiddenMCPToolSets)
}

func resolveHiddenMCPToolsFrom(sets map[string][]string, tools []string, setNames []string) []string {
	seen := make(map[string]struct{})
	var result []string
	for _, name := range setNames {
		if toolList, ok := sets[name]; ok {
			for _, t := range toolList {
				if _, ok := seen[t]; !ok {
					seen[t] = struct{}{}
					result = append(result, t)
				}
			}
		}
	}
	for _, t := range tools {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			result = append(result, t)
		}
	}
	return result
}

func isMCPToolHidden(toolName, serverName string, hidden []string) bool {
	for _, h := range hidden {
		if h == toolName {
			return true
		}
		if idx := strings.Index(h, "."); idx >= 0 {
			if h[:idx] == serverName && h[idx+1:] == toolName {
				return true
			}
		}
	}
	return false
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

func (n *Network) getNextServer() (Server, error) {
	if len(n.Servers) == 0 {
		return Server{}, fmt.Errorf("network %q has no servers configured", n.Name)
	}
	s := n.Servers[n.nextServer]
	n.nextServer++
	if n.nextServer > len(n.Servers)-1 {
		n.nextServer = 0
	}
	return s, nil
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

	if config.MaxQueueDepth <= 0 {
		config.MaxQueueDepth = 5
	}
	if len(config.HiddenTools) == 0 {
		config.HiddenTools = []string{"register_background_job", "check_ban_history"}
	}
	if config.Bans.MaxDuration == "" {
		config.Bans.MaxDuration = "6h"
	}
	if config.Bans.DefaultDuration == "" {
		config.Bans.DefaultDuration = "5m"
	}
	config.Compaction.ApplyDefaults()
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
		if network.User == "" {
			network.User = network.Nick
		}
		if network.RealName == "" {
			network.RealName = network.Nick
		}
		if network.ReconnectDelay == nil {
			defaultDelay := 60 * time.Second
			network.ReconnectDelay = &defaultDelay
		}
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

	if err := loadNoticesFile(dir, &config); err != nil {
		return config, err
	}

	config.Database.SetDefaults()

	if config.SessionsDisplayLimit == 0 {
		config.SessionsDisplayLimit = 10
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
			service.MaxHistory = 100
		}
		if service.Parallel <= 0 {
			service.Parallel = 1
		}
		if service.DisabledBuiltinTools == nil {
			service.DisabledBuiltinTools = config.DisabledBuiltinTools
		}
		if service.HiddenMCPTools == nil {
			service.HiddenMCPTools = config.HiddenMCPTools
		}
		if service.HiddenMCPToolSets == nil {
			service.HiddenMCPToolSets = config.HiddenMCPToolSets
		}
		config.Services[name] = service
	}
	return nil
}

type HiddenMCPToolSetConfig struct {
	Tools []string `toml:"tools"`
}

func loadMCPsFile(dir string, config *Config) error {
	path := filepath.Join(dir, "mcps.toml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		config.MCPs = make(map[string]MCPConfig)
		config.MCPToolSets = make(map[string][]string)
		return nil
	}

	var raw map[string]toml.Primitive
	if _, err := toml.DecodeFile(path, &raw); err != nil {
		return fmt.Errorf("loading mcps: %w", err)
	}

	config.MCPToolSets = make(map[string][]string)
	if tsRaw, ok := raw["hidden_mcp_tool_sets"]; ok {
		var toolSets map[string]HiddenMCPToolSetConfig
		if err := toml.PrimitiveDecode(tsRaw, &toolSets); err != nil {
			return fmt.Errorf("loading mcps hidden_mcp_tool_sets: %w", err)
		}
		for name, ts := range toolSets {
			config.MCPToolSets[name] = ts.Tools
		}
		delete(raw, "hidden_mcp_tool_sets")
	}

	config.MCPs = make(map[string]MCPConfig)
	for name, prim := range raw {
		var mcpCfg MCPConfig
		if err := toml.PrimitiveDecode(prim, &mcpCfg); err != nil {
			return fmt.Errorf("loading mcps.%s: %w", name, err)
		}
		config.MCPs[name] = mcpCfg
	}

	for name, mcpCfg := range config.MCPs {
		if mcpCfg.Transport == "" {
			return fmt.Errorf("mcps.%s transport is required (stdio, http, or sse)", name)
		}
		if mcpCfg.Transport != "stdio" && mcpCfg.Transport != "http" && mcpCfg.Transport != "sse" {
			return fmt.Errorf("mcps.%s transport must be 'stdio', 'http', or 'sse'", name)
		}
		if mcpCfg.Transport == "stdio" && mcpCfg.Command == "" {
			return fmt.Errorf("mcps.%s command is required for stdio transport", name)
		}
		if (mcpCfg.Transport == "http" || mcpCfg.Transport == "sse") && mcpCfg.URL == "" {
			return fmt.Errorf("mcps.%s url is required for %s transport", name, mcpCfg.Transport)
		}
		if mcpCfg.Timeout == 0 {
			mcpCfg.Timeout = 30 * time.Second
		}
		if mcpCfg.KeepAlive == 0 && (mcpCfg.Transport == "http" || mcpCfg.Transport == "sse") {
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

	tmpConfig.DisabledBuiltinTools = config.DisabledBuiltinTools
	tmpConfig.HiddenMCPTools = config.HiddenMCPTools
	tmpConfig.HiddenMCPToolSets = config.HiddenMCPToolSets

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

	if err := loadNoticesFile(dir, &tmpConfig); err != nil {
		return err
	}

	config.MCPs = tmpConfig.MCPs
	config.MCPToolSets = tmpConfig.MCPToolSets
	config.Services = tmpConfig.Services
	config.Commands = commands
	config.TemplateVars = tmpConfig.TemplateVars
	config.Notices = tmpConfig.Notices

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
		if cfg.APIUser != "" {
			tmpl, err := template.New(name + "_api_user").Parse(cfg.APIUser)
			if err != nil {
				return fmt.Errorf("commands.completions.%s api_user template parse error: %w", name, err)
			}
			if err := validateAPIUserTemplate(tmpl); err != nil {
				return fmt.Errorf("commands.completions.%s api_user template validation error: %w", name, err)
			}
			cfg.apiUserTmpl = tmpl
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
		if cfg.APIUser != "" {
			tmpl, err := template.New(name + "_api_user").Parse(cfg.APIUser)
			if err != nil {
				return fmt.Errorf("commands.chats.%s api_user template parse error: %w", name, err)
			}
			if err := validateAPIUserTemplate(tmpl); err != nil {
				return fmt.Errorf("commands.chats.%s api_user template validation error: %w", name, err)
			}
			cfg.apiUserTmpl = tmpl
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
