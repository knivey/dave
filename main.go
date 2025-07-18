package main

import (
	"bufio"
	"crypto/tls"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	"github.com/muesli/reflow/wordwrap"
	"github.com/muesli/reflow/wrap"
	"github.com/vodkaslime/wildcard"
)

var config Config
var wg sync.WaitGroup
var logger logxi.Logger

type Bot struct {
	Client    *girc.Client
	Reconnect bool
	Network   Network
}

func (bot *Bot) Quit() {
	bot.Reconnect = false
	bot.Client.Cmd.SendRawf("QUIT :%s\r\n", bot.Network.Quitmsg)
}

var bots map[string]*Bot

func init() {
	bots = make(map[string]*Bot)
}

type CmdFunc func(Network, *girc.Client, girc.Event, ...string)

var stop_re = regexp.MustCompile("^stop$")

type CmdMap map[*regexp.Regexp]CmdFunc

var commands = CmdMap{
	stop_re: func(n Network, c *girc.Client, e girc.Event, s ...string) { stop(n, c, e, nil, s...) },
}

func main() {
	logger = logxi.New("main")
	logger.SetLevel(logxi.LevelAll)
	if len(os.Args) > 1 {
		config = loadConfigOrDie(os.Args[1])
	} else {
		config = loadConfigOrDie("config.toml")
	}
	logger.Info("Config loaded", "networks", len(config.Networks))
	for _, c := range config.Commands.Completions {
		logger.Debug("added Completions command", c)
		commands[regexp.MustCompile("^"+c.Regex+" (.+)$")] =
			func(network Network, client *girc.Client, e girc.Event, args ...string) {
				completion(network, client, e, c, args...)
			}

	}
	for _, c := range config.Commands.Chats {
		logger.Debug("added Chats command", c)
		commands[regexp.MustCompile("^"+c.Regex+" (.+)$")] =
			func(network Network, client *girc.Client, e girc.Event, args ...string) {
				chat(network, client, e, c, args...)
			}
	}
	for _, c := range config.Commands.SD {
		logger.Debug("added SD command", c)
		commands[regexp.MustCompile("^"+c.Regex+" (.+)$")] =
			func(network Network, client *girc.Client, e girc.Event, args ...string) {
				sd(network, client, e, c, args...)
			}
	}

	for _, network := range config.Networks {
		if network.Enabled {
			logger.Info("Starting network", "network", network.Name)
			startClient(network)
		}
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		signal := <-sigs
		logger.Info("Caught signal", "signal", signal.String())
		for _, bot := range bots {
			bot.Quit()
		}
	}()

	wg.Wait()
	logger.Info("Nothing left to do bye :)")
	os.Exit(0)
}

func sendLoop(out string, network Network, c *girc.Client, e girc.Event) {
	out = wrap.String(wordwrap.String(out, 350), 420)

	// for each new line break in response choices write to channel
	for _, line := range strings.Split(out, "\n") {
		//TODO better sync here
		if !getRunning(network.Name + e.Params[0]) {
			break
		}
		if len(line) <= 0 {
			continue
		}
		c.Cmd.Reply(e, "\x02\x02"+line)
		time.Sleep(time.Millisecond * network.Throttle)
	}
}

func stop(network Network, _ *girc.Client, m girc.Event, _ interface{}, _ ...string) {
	logger.Info("stop requested")
	stoppedRunning(network.Name + m.Params[0])
}

func isIgnored(host string) bool {
	file, err := os.Open("ignores.txt")
	if err != nil {
		logger.Info("ignores.txt failed to open", err.Error())
		return false
	}
	defer file.Close()

	matcher := wildcard.NewMatcher()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if m, _ := matcher.Match(scanner.Text(), host); m {
			return true
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Error(err.Error())
	}
	return false
}

func handleChanMessage(network Network, client *girc.Client, event girc.Event) {
	ctx_key := network.Name + event.Params[0] + event.Source.Name
	host := event.Source.Name + "!" + event.Source.Ident + "@" + event.Source.Host
	msg := event.Params[len(event.Params)-1]
	if !strings.HasPrefix(msg, network.Trigger) {
		botnick := client.GetNick()
		if !strings.HasPrefix(msg, botnick+", ") && !strings.HasPrefix(msg, botnick+": ") {
			return
		}
		if isIgnored(host) {
			logger.Info("Ignoring host", host)
			return
		}
		if !ContextExists(ctx_key) {
			logger.Info("Ignoring message due to no existing chat context")
			client.Cmd.Reply(event, "you dont have a chat context, start one with one of my many fabulous chat commands")
			return
		}
		if !checkRate(network, event.Params[0]) {
			client.Cmd.Reply(event, config.Ratemsg())
			return
		}
		if getRunning(network.Name + event.Params[0]) {
			client.Cmd.Reply(event, config.Busymsg())
			return
		}
		msg = msg[len(botnick+", "):]
		ctx := GetContext(ctx_key)
		logger.Info("Running chat completion with existing context")
		go chat(network, client, event, ctx.Config, msg)
		return
	}
	if isIgnored(host) {
		logger.Info("Ignoring host", host)
		return
	}
	msg = strings.TrimPrefix(msg, network.Trigger)
	for r, cmd := range commands {
		if r.Match([]byte(msg)) {
			var args []string
			for i, m := range r.FindSubmatch([]byte(msg)) {
				if i != 0 {
					args = append(args, string(m))
				}
			}

			//special case for stop command to skip rate limits
			if r == stop_re {
				cmd(network, client, event, args...)
				return
			}

			if !checkRate(network, event.Params[0]) {
				client.Cmd.Reply(event, config.Ratemsg())
				return
			}
			if getRunning(network.Name + event.Params[0]) {
				client.Cmd.Reply(event, config.Busymsg())
				return
			}
			//TODO only clear if its a chat command type
			ClearContext(ctx_key)
			go cmd(network, client, event, args...)
			return
		}
	}
}

func startClient(network Network) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ircClient(network)
	}()
}

func ircClient(network Network) {
	wg.Add(1)
	defer wg.Done()

	log := logxi.New(network.Name)
	log.SetLevel(logxi.LevelAll)

	sslConfig := &tls.Config{
		InsecureSkipVerify: true,
	}

	ircServer := network.getNextServer()
	log.Info("dialing server", "host", ircServer.Host, "port", ircServer.GetPort())

	client := girc.New(girc.Config{
		Server:     ircServer.Host,
		Port:       ircServer.GetPort(),
		Nick:       network.Nick,
		ServerPass: ircServer.Pass,
		User:       network.Nick,
		Name:       network.Nick,
		SSL:        ircServer.Ssl,
		TLSConfig:  sslConfig,
		AllowFlood: true,
	})

	bot := Bot{
		Client:    client,
		Reconnect: true,
		Network:   network,
	}
	bots[network.Name] = &bot

	client.Handlers.Add(girc.ALL_EVENTS, func(client *girc.Client, event girc.Event) {
		if str, ok := event.Pretty(); ok {
			log.Info(str)
		}
	})

	client.Handlers.Add(girc.RPL_WELCOME, func(client *girc.Client, event girc.Event) {
		time.Sleep(time.Microsecond * network.Throttle)
		client.Cmd.Join(strings.Join(network.Channels, ","))
	})

	client.Handlers.AddBg(girc.PRIVMSG, func(client *girc.Client, event girc.Event) {
		if !event.IsFromChannel() {
			return
		}
		handleChanMessage(network, client, event)
	})

	for {
		if err := client.Connect(); err != nil {
			log.Warn(err.Error())
		}
		if !bot.Reconnect {
			log.Info("Reconnect not requested")
			break
		}
		log.Info("Reconnecting in 60s")
		time.Sleep(60 * time.Second)
		ircServer := network.getNextServer()
		client.Config.Server = ircServer.Host
		client.Config.Port = ircServer.GetPort()
		client.Config.ServerPass = ircServer.Pass
		client.Config.SSL = ircServer.Ssl
	}
	log.Info("Finished quitting")

}
