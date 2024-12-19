package main

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Trigger  string
	Quitmsg  string
	Networks map[string]Network
	Services map[string]Service
	Commands Commands
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
}

type AIConfig struct {
	Name        string //set to key name
	Service     string
	Model       string
	Regex       string
	System      string
	Streaming   bool
	MaxTokens   int
	Temperature float32
}

type Service struct {
	Key         string
	MaxTokens   int
	BaseURL     string
	Temperature float32
}

func (cfg *AIConfig) ApplyDefaults(service Service) {
	if cfg.MaxTokens == 0 {
		cfg.MaxTokens = service.MaxTokens
	}
	if cfg.Temperature == 0 {
		cfg.Temperature = service.Temperature
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

	fff := func(cfg AIConfig, name string, config *Config) {
		cfg.Name = name
		if cfg.Regex == "" {
			cfg.Regex = name
		}
		if service, ok := config.Services[cfg.Service]; ok {
			cfg.ApplyDefaults(service)
		} else {
			log.Fatalln("commands.completions."+name, "service", cfg.Service, "is undefined")
		}
		config.Commands.Completions[name] = cfg
	}

	for name, cfg := range config.Commands.Completions {
		fff(cfg, name, &config)
	}

	for name, cfg := range config.Commands.Chats {
		fff(cfg, name, &config)
	}

	return
}
