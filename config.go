package main

import (
	"fmt"
	"log"
	"os"
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
	Name          string //gets set to key name
	Regex         string
	Service       string
	WorkflowPath  string   `toml:"workflow_path"`
	ClientID      string   `toml:"clientid"`
	OutputNode    string   `toml:"output_node"`
	PromptNode    string   `toml:"prompt_node"`
	SeedNodes     []string `toml:"seed_nodes"`
	Timeout       int
	EnhancePrompt string `toml:"enhanceprompt"`
	Description   string
}

type AIConfig struct {
	Name                string //gets set to key name
	Service             string
	Model               string
	Regex               string
	System              string
	Streaming           bool
	MaxTokens           int `toml:"maxtokens"`
	MaxCompletionTokens int `toml:"maxcompletiontokens"`
	Temperature         float32
	MaxHistory          int  `toml:"maxhistory"`
	RenderMarkdown      bool `toml:"rendermarkdown"`
	DetectImages        bool `toml:"detectimages"`
	MaxImages           int  `toml:"maximages"`
	MaxContextImages    int  `toml:"maxcontextimages"`
	Description         string
}

type Service struct {
	Key                 string
	MaxTokens           int `toml:"maxtokens"`           //best to keep both for compatibility with non-openai
	MaxCompletionTokens int `toml:"maxcompletiontokens"` //use this one now with openai
	BaseURL             string
	Temperature         float32
	MaxHistory          int `toml:"maxhistory"`
	ComfyTimeout        int `toml:"comfy_timeout"` // WebSocket timeout in seconds
}

func (config *Config) Busymsg() string {
	return config.Busymsgs[rand.Intn(len(config.Busymsgs))]
}

func (config *Config) Ratemsg() string {
	return config.Busymsgs[rand.Intn(len(config.Busymsgs))]
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

	fff := func(cfg AIConfig, name string, config *Config) AIConfig {
		cfg.Name = name
		if cfg.Regex == "" {
			cfg.Regex = name
		}
		if service, ok := config.Services[cfg.Service]; ok {
			cfg.ApplyDefaults(service)
		} else {
			log.Fatalln("commands.completions."+name, "service", cfg.Service, "is undefined")
		}
		if cfg.RenderMarkdown && cfg.Streaming {
			log.Fatalln("commands.completions."+name, "cannot render markdown with streaming")
		}
		return cfg
	}

	for name, cfg := range config.Commands.Completions {
		config.Commands.Completions[name] = fff(cfg, name, &config)
	}

	for name, cfg := range config.Commands.Chats {
		config.Commands.Chats[name] = fff(cfg, name, &config)
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
	config.Persist.SetDefaults()

	return
}
