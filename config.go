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
	Trigger  string
	Quitmsg  string
	Networks map[string]Network
	Services map[string]Service
	Commands Commands
	Busymsgs []string
	Ratemsgs []string
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
}

type SDConfig struct {
	Name         string //gets set to key name
	Regex        string
	Service      string
	Steps        int64
	SamplerName  string
	SamplerIndex string
	Scheduler    string
	Width        int64
	Height       int64
}

type AIConfig struct {
	Name           string //gets set to key name
	Service        string
	Model          string
	Regex          string
	System         string
	Streaming      bool
	MaxTokens      int
	Temperature    float32
	MaxHistory     int
	RenderMarkdown bool
}

type Service struct {
	Key         string
	MaxTokens   int
	BaseURL     string
	Temperature float32
	MaxHistory  int
}

func (config *Config) Busymsg() string {
	return config.Busymsgs[rand.Intn(len(config.Busymsgs))]
}

func (config *Config) Ratemsg() string {
	return config.Busymsgs[rand.Intn(len(config.Busymsgs))]
}

func (cfg *SDConfig) ApplyDefaults(service Service) {

}

func (cfg *AIConfig) ApplyDefaults(service Service) {
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = service.MaxTokens
	}
	if cfg.Temperature == 0 {
		cfg.Temperature = service.Temperature
	}
	if cfg.MaxHistory == 0 {
		cfg.MaxHistory = service.MaxHistory
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

	return
}
