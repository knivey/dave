package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/knivey/dave/MarkdownToIRC/irc"
	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	"github.com/vodkaslime/wildcard"
)

var config Config
var configDir string
var wg sync.WaitGroup
var logger logxi.Logger
var commandsMutex sync.RWMutex

type Bot struct {
	Client    *girc.Client
	Reconnect bool
	Network   Network
	mu        sync.Mutex
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
var help_re = regexp.MustCompile("^help(?:\\s+(.+))?$")
var sessions_re = regexp.MustCompile("^sessions$")
var history_re = regexp.MustCompile("^history(?:\\s+(.+))?$")
var stats_re = regexp.MustCompile("^mystats$")
var delete_re = regexp.MustCompile("^delete\\s+(\\d+)$")
var resume_re = regexp.MustCompile("^resume\\s+(\\d+)$")
var jobs_re = regexp.MustCompile("^jobs$")

type CmdMap map[*regexp.Regexp]CmdFunc

func errorMsg(msg string) string {
	return "\x0304❗ " + msg
}

func warnMsg(msg string) string {
	return "\x0308⚠️ " + msg
}

var builtInCmds = CmdMap{
	stop_re:     func(n Network, c *girc.Client, e girc.Event, s ...string) { stop(n, c, e, nil, s...) },
	help_re:     func(n Network, c *girc.Client, e girc.Event, s ...string) { help(n, c, e, s...) },
	sessions_re: func(n Network, c *girc.Client, e girc.Event, s ...string) { historySessions(n, c, e, s...) },
	history_re:  func(n Network, c *girc.Client, e girc.Event, s ...string) { historyShow(n, c, e, s...) },
	stats_re:    func(n Network, c *girc.Client, e girc.Event, s ...string) { historyStats(n, c, e, s...) },
	delete_re:   func(n Network, c *girc.Client, e girc.Event, s ...string) { historyDelete(n, c, e, s...) },
	resume_re:   func(n Network, c *girc.Client, e girc.Event, s ...string) { historyResume(n, c, e, s...) },
	jobs_re:     func(n Network, c *girc.Client, e girc.Event, s ...string) { historyJobs(n, c, e, s...) },
}
var configCmds CmdMap
var rateExemptCmds map[*regexp.Regexp]bool
var chatCmds map[*regexp.Regexp]bool

func registerCommands(cmds Commands) {
	commandsMutex.Lock()
	defer commandsMutex.Unlock()
	registerCommandsLocked(cmds)
}

func registerCommandsLocked(cmds Commands) {
	newConfigCmds := CmdMap{}
	newExemptCmds := make(map[*regexp.Regexp]bool)
	newChatCmds := make(map[*regexp.Regexp]bool)

	for _, c := range cmds.Completions {
		logger.Debug("added Completions command", c)
		re := regexp.MustCompile("^" + c.Regex + " (.+)$")
		newConfigCmds[re] = func(network Network, client *girc.Client, e girc.Event, args ...string) {
			completion(network, client, e, c, args...)
		}
	}
	for _, c := range cmds.Chats {
		logger.Debug("added Chats command", c)
		re := regexp.MustCompile("^" + c.Regex + " (.+)$")
		newConfigCmds[re] = func(network Network, client *girc.Client, e girc.Event, args ...string) {
			chat(network, client, e, c, args...)
		}
		newChatCmds[re] = true
	}
	for _, c := range cmds.Tools {
		logger.Debug("added Tools command", c)
		pattern := "^" + c.Regex + "$"
		if c.Arg != "" {
			pattern = "^" + c.Regex + " (.+)$"
		}
		re := regexp.MustCompile(pattern)
		newConfigCmds[re] = func(network Network, client *girc.Client, e girc.Event, args ...string) {
			mcpCmd(network, client, e, c, args...)
		}
		if c.SkipBusy {
			newExemptCmds[re] = true
		}
	}

	configCmds = newConfigCmds
	rateExemptCmds = newExemptCmds
	chatCmds = newChatCmds
}

func reloadAll() error {
	commandsMutex.Lock()
	defer commandsMutex.Unlock()
	if err := loadReloadableDir(configDir, &config); err != nil {
		return err
	}
	if apiLogger != nil {
		apiLogger.CloseAll()
	}
	initAPILogger(config, configDir)
	reloadMCPClients(config.MCPs)
	registerCommandsLocked(config.Commands)
	return nil
}

func main() {
	// Load config first (before TUI init) so errors go to real stderr
	if len(os.Args) > 1 {
		configDir = os.Args[1]
	} else {
		configDir = "config"
	}
	config = loadConfigDirOrDie(configDir)

	noTUI := os.Getenv("DAVE_NO_TUI") != ""

	if !noTUI {
		// Initialize TUI - captures all subsequent log output
		app, err := initTUI()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to initialize TUI: %v\n", err)
			os.Exit(1)
		}
		tuiApp = app
	}

	if os.Getenv("LOGXI_FORMAT") == "" {
		logxi.ProcessLogxiFormatEnv("maxcol=9999")
	}
	logger = logxi.New("main")
	logger.SetLevel(logxi.LevelAll)
	logger.Info("Config loaded", "networks", len(config.Networks))
	initAPILogger(config, configDir)

	var dbErr error
	theDB, dbErr = initDB(config.Database)
	if dbErr != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize database: %v\n", dbErr)
		os.Exit(1)
	}

	LoadContextStore()
	CleanupContexts()
	initMCPClients()
	startJobManager()
	recoverPendingJobs()
	registerCommands(config.Commands)

	if !noTUI {
		for _, network := range config.Networks {
			if network.Enabled {
				logger.Info("Starting network", "network", network.Name)
				startClient(network)
			}
		}
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT)
	go func() {
		signal := <-sigs
		if !atomic.CompareAndSwapInt32(&shutdownOnce, 0, 1) {
			return
		}
		logger.Info("Caught signal", "signal", signal.String())

		if logFlushStop != nil {
			close(logFlushStop)
			<-logFlushDone
		}

		StopPendingSave()
		SaveContextStore()
		if apiLogger != nil {
			apiLogger.CloseAll()
		}
		closeMCPClients()
		stopJobManager()
		closeDB(theDB)
		closeLogFile()
		for _, bot := range bots {
			bot.Quit()
		}
		tuiApp.QueueUpdateDraw(func() {
			tuiApp.Stop()
		})
	}()

	if noTUI {
		// Give MCP clients a moment to log connection attempts, then exit
		time.Sleep(500 * time.Millisecond)
		closeMCPClients()
		logger.Info("Nothing left to do bye :)")
		return
	}

	if err := tuiApp.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
	}

	restoreStdoutStderr()

	logger.Info("Nothing left to do bye :)")
}

const maxLineLen = 350

func wrapForIRC(out string) []string {
	var lines []string
	for _, line := range strings.Split(out, "\n") {
		lines = append(lines, irc.ByteWrap(line, maxLineLen)...)
	}
	return lines
}

func sendLoop(out string, network Network, c *girc.Client, e girc.Event) {
	for _, line := range wrapForIRC(out) {
		if !getRunning(network.Name, e.Params[0], e.Source.Name) {
			break
		}
		if len(line) <= 0 {
			continue
		}
		// We prepend lines with a \x02\x02 here to try and prevent our bot from triggering commands on other IRC bots by accident
		c.Cmd.Reply(e, "\x02\x02"+line)
		time.Sleep(time.Millisecond * network.Throttle)
	}
}

func stop(network Network, _ *girc.Client, m girc.Event, _ interface{}, _ ...string) {
	logger.Info("stop requested")
	forceStopRunning(network.Name, m.Params[0], m.Source.Name)
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
			client.Cmd.Reply(event, warnMsg("you dont have a chat context, start one with one of my many fabulous chat commands. After starting, just reply to my nick to continue the conversation"))
			return
		}
		if !checkRate(network, event.Params[0]) {
			client.Cmd.Reply(event, warnMsg(config.Ratemsg()))
			return
		}
		if getRunning(network.Name, event.Params[0], event.Source.Name) {
			client.Cmd.Reply(event, warnMsg(config.Busymsg()))
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
	commandsMutex.RLock()

	for r, cmd := range builtInCmds {
		if r.Match([]byte(msg)) {
			var args []string
			for i, m := range r.FindSubmatch([]byte(msg)) {
				if i != 0 && len(m) > 0 {
					args = append(args, string(m))
				}
			}
			commandsMutex.RUnlock()

			if r == stop_re {
				cmd(network, client, event, args...)
				return
			}
			if r == help_re {
				cmd(network, client, event, args...)
				return
			}
			if r == jobs_re {
				cmd(network, client, event, args...)
				return
			}

			if !checkRate(network, event.Params[0]) {
				client.Cmd.Reply(event, warnMsg(config.Ratemsg()))
				return
			}
			if getRunning(network.Name, event.Params[0], event.Source.Name) {
				client.Cmd.Reply(event, warnMsg(config.Busymsg()))
				return
			}
			go cmd(network, client, event, args...)
			return
		}
	}

	for r, cmd := range configCmds {
		if r.Match([]byte(msg)) {
			var args []string
			for i, m := range r.FindSubmatch([]byte(msg)) {
				if i != 0 && len(m) > 0 {
					args = append(args, string(m))
				}
			}
			commandsMutex.RUnlock()

			if !checkRate(network, event.Params[0]) {
				client.Cmd.Reply(event, warnMsg(config.Ratemsg()))
				return
			}
			if !rateExemptCmds[r] && getRunning(network.Name, event.Params[0], event.Source.Name) {
				client.Cmd.Reply(event, warnMsg(config.Busymsg()))
				return
			}
			if chatCmds[r] {
				ClearContext(ctx_key)
			}
			go cmd(network, client, event, args...)
			return
		}
	}

	commandsMutex.RUnlock()
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
		bot.mu.Lock()
		throttle := bot.Network.Throttle
		channels := make([]string, len(bot.Network.Channels))
		copy(channels, bot.Network.Channels)
		bot.mu.Unlock()
		time.Sleep(time.Microsecond * throttle)
		client.Cmd.Join(strings.Join(channels, ","))
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
		ircServer := bot.Network.getNextServer()
		client.Config.Server = ircServer.Host
		client.Config.Port = ircServer.GetPort()
		client.Config.ServerPass = ircServer.Pass
		client.Config.SSL = ircServer.Ssl
	}
	log.Info("Finished quitting")

}
