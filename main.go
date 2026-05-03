package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
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
var configMu sync.RWMutex

var ignorePatterns []string
var ignoreMu sync.RWMutex
var ignoreWatcher *fsnotify.Watcher
var ignoreFile string

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

func (bot *Bot) isReady(channel string) bool {
	return bot.Client != nil && bot.Client.IsConnected() && bot.Client.IsInChannel(channel)
}

var bots map[string]*Bot

func init() {
	bots = make(map[string]*Bot)
}

func readConfig(f func()) {
	configMu.RLock()
	defer configMu.RUnlock()
	f()
}

type CmdFunc func(Network, *girc.Client, girc.Event, context.Context, chan<- string, ...string)

var stop_re = regexp.MustCompile("^stop$")
var help_re = regexp.MustCompile("^help(?:\\s+(.+))?$")
var sessions_re = regexp.MustCompile("^sessions$")
var history_re = regexp.MustCompile("^history(?:\\s+(.+))?$")
var stats_re = regexp.MustCompile("^mystats$")
var delete_re = regexp.MustCompile("^delete\\s+(\\d+)$")
var resume_re = regexp.MustCompile("^resume\\s+(\\d+)$")
var jobs_re = regexp.MustCompile("^jobs$")
var support_re = regexp.MustCompile("^support$")

type CmdMap map[*regexp.Regexp]CmdFunc

func errorMsg(msg string) string {
	return "\x0304❗ " + msg
}

func warnMsg(msg string) string {
	return "\x0308⚠️ " + msg
}

var builtInCmds = CmdMap{
	stop_re: func(n Network, c *girc.Client, e girc.Event, _ context.Context, _ chan<- string, s ...string) {
		stop(n, c, e, nil, s...)
	},
	help_re: func(n Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, s ...string) {
		help(n, c, e, ctx, output, s...)
	},
	sessions_re: func(n Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, s ...string) {
		historySessions(n, c, e, ctx, output, s...)
	},
	history_re: func(n Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, s ...string) {
		historyShow(n, c, e, ctx, output, s...)
	},
	stats_re: func(n Network, c *girc.Client, e girc.Event, _ context.Context, _ chan<- string, s ...string) {
		historyStats(n, c, e, s...)
	},
	delete_re: func(n Network, c *girc.Client, e girc.Event, _ context.Context, _ chan<- string, s ...string) {
		historyDelete(n, c, e, s...)
	},
	resume_re: func(n Network, c *girc.Client, e girc.Event, _ context.Context, _ chan<- string, s ...string) {
		historyResume(n, c, e, s...)
	},
	jobs_re: func(n Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, s ...string) {
		historyJobs(n, c, e, ctx, output, s...)
	},
	support_re: func(n Network, c *girc.Client, e girc.Event, _ context.Context, _ chan<- string, s ...string) {
		support(n, c, e, s...)
	},
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
		newConfigCmds[re] = func(network Network, client *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
			completion(network, client, e, c, ctx, output, args...)
		}
	}
	for _, c := range cmds.Chats {
		logger.Debug("added Chats command", c)
		re := regexp.MustCompile("^" + c.Regex + " (.+)$")
		newConfigCmds[re] = func(network Network, client *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
			chat(network, client, e, c, ctx, output, args...)
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
		newConfigCmds[re] = func(network Network, client *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
			mcpCmd(network, client, e, c, ctx, output, args...)
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
	configMu.Lock()
	defer configMu.Unlock()
	if err := loadReloadableDir(configDir, &config); err != nil {
		return err
	}
	if apiLogger != nil {
		apiLogger.CloseAll()
	}
	initAPILogger(config, configDir)
	initIncidentLogger(config)
	reloadMCPClients(config.MCPs)
	registerCommandsLocked(config.Commands)
	if queueMgr != nil {
		queueMgr.UpdateServiceLimits(config.Services)
	}
	loadIgnores(filepath.Join(configDir, "ignores.txt"))
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
	initIncidentLogger(config)

	var dbErr error
	theDB, dbErr = initDB(config.Database)
	if dbErr != nil {
		logger.Error("Failed to initialize database", "error", dbErr)
		os.Exit(1)
	}

	LoadContextStore()
	CleanupContexts()
	initMCPClients()
	queueMgr = NewQueueManager(config.QueueMsgs, config.StartedMsg, config.MaxQueueDepth)
	queueMgr.UpdateServiceLimits(config.Services)
	queueMgr.Start()
	startJobManager()
	startToolJobManager()
	registerCommands(config.Commands)

	ignorePath := filepath.Join(configDir, "ignores.txt")
	loadIgnores(ignorePath)
	watchIgnores(ignorePath)
	startRateLimitGC()

	go func() {
		waitForMCPReady()
		recoverPendingJobs()
		recoverToolPendingJobs()
	}()

	if !noTUI {
		for _, network := range config.Networks {
			if network.IsEnabled() {
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
		if ignoreWatcher != nil {
			ignoreWatcher.Close()
		}
		if apiLogger != nil {
			apiLogger.CloseAll()
		}
		if queueMgr != nil {
			queueMgr.Stop()
		}
		stopJobManager()
		stopToolJobManager()
		closeMCPClients()

		for _, bot := range bots {
			bot.Quit()
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		time.Sleep(1 * time.Second) // give IRC connections time to flush the QUIT message

		closeDB(theDB)
		closeLogFile()
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
		logger.Error("TUI error", "error", err)
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

func isIRCAction(line string) (string, bool) {
	if strings.HasPrefix(line, "/me ") {
		return strings.TrimPrefix(line, "/me "), true
	}
	if strings.HasPrefix(line, "/ me ") {
		return strings.TrimPrefix(line, "/ me "), true
	}
	return line, false
}

func sendToOutput(out string, output chan<- string, ctx context.Context) {
	for _, line := range wrapForIRC(out) {
		if len(line) <= 0 {
			continue
		}
		select {
		case output <- line:
		case <-ctx.Done():
			return
		}
	}
}

// DO NOT DELETE — stop is only for stopping the current text output, not for cancelling async jobs
func stop(network Network, _ *girc.Client, m girc.Event, _ interface{}, _ ...string) {
	logger.Info("stop requested")
	queueMgr.StopCurrent(network.Name, m.Params[0])
}

func loadIgnores(path string) {
	file, err := os.Open(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Info("ignores.txt failed to open", err.Error())
		}
		ignoreMu.Lock()
		ignorePatterns = nil
		ignoreMu.Unlock()
		return
	}
	defer file.Close()

	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			patterns = append(patterns, line)
		}
	}
	if err := scanner.Err(); err != nil {
		logger.Error(err.Error())
	}
	ignoreMu.Lock()
	ignorePatterns = patterns
	ignoreMu.Unlock()
	logger.Info("loaded ignore patterns", "count", len(patterns))
}

func watchIgnores(path string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Warn("failed to create ignores watcher", err.Error())
		return
	}
	ignoreWatcher = watcher
	ignoreFile = path

	dir := filepath.Dir(path)
	if err := watcher.Add(dir); err != nil {
		logger.Warn("failed to watch ignores directory", err.Error())
		return
	}

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Name == path && (event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Remove)) {
					logger.Info("ignores.txt changed, reloading")
					loadIgnores(path)
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				logger.Error("ignores watcher error", err.Error())
			}
		}
	}()
}

func isIgnored(host string) bool {
	ignoreMu.RLock()
	patterns := ignorePatterns
	ignoreMu.RUnlock()

	if len(patterns) == 0 {
		return false
	}

	matcher := wildcard.NewMatcher()
	for _, p := range patterns {
		if m, _ := matcher.Match(p, host); m {
			return true
		}
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
			var rateMsg string
			readConfig(func() { rateMsg = config.Ratemsg() })
			client.Cmd.Reply(event, warnMsg(rateMsg))
			return
		}
		msg = msg[len(botnick+", "):]
		ctx := GetContext(ctx_key)
		logger.Info("Running chat completion with existing context")

		position := queueMgr.Enqueue(network.Name, event.Params[0], event.Source.Name,
			ctx.Config.Service, "chat",
			func(cx context.Context, output chan<- string) {
				chat(network, client, event, ctx.Config, cx, output, msg)
			})
		if position > 0 {
			var queueMsg string
			readConfig(func() { queueMsg = config.QueueMsg(position, 0) })
			client.Cmd.Reply(event, queueMsg)
		}
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
				cmd(network, client, event, context.Background(), nil, args...)
				return
			}

			if !checkRate(network, event.Params[0]) {
				var rateMsg string
				readConfig(func() { rateMsg = config.Ratemsg() })
				client.Cmd.Reply(event, warnMsg(rateMsg))
				return
			}

			if r == stats_re || r == delete_re || r == resume_re || r == support_re {
				cmd(network, client, event, context.Background(), nil, args...)
				return
			}

			position := queueMgr.Enqueue(network.Name, event.Params[0], event.Source.Name, "", msg,
				func(cx context.Context, output chan<- string) {
					cmd(network, client, event, cx, output, args...)
				})
			if position > 0 {
				var queueMsg string
				readConfig(func() { queueMsg = config.QueueMsg(position, 0) })
				client.Cmd.Reply(event, queueMsg)
			}
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
				var rateMsg string
				readConfig(func() { rateMsg = config.Ratemsg() })
				client.Cmd.Reply(event, warnMsg(rateMsg))
				return
			}

			if rateExemptCmds[r] {
				if chatCmds[r] {
					ClearContext(ctx_key)
				}
				outCh := make(chan string, 200)
				go func() {
					defer close(outCh)
					cmd(network, client, event, context.Background(), outCh, args...)
				}()
				go func() {
					for msg := range outCh {
						if action, ok := isIRCAction(msg); ok {
							client.Cmd.Action(event.Params[0], action)
						} else {
							client.Cmd.Message(event.Params[0], "\x02\x02"+msg)
						}
						time.Sleep(time.Millisecond * time.Duration(network.Throttle))
					}
				}()
				return
			}

			if chatCmds[r] {
				ClearContext(ctx_key)
			}

			svc := getServiceForConfigCmd(r)
			position := queueMgr.Enqueue(network.Name, event.Params[0], event.Source.Name, svc, msg,
				func(cx context.Context, output chan<- string) {
					cmd(network, client, event, cx, output, args...)
				})
			if position > 0 {
				var queueMsg string
				readConfig(func() { queueMsg = config.QueueMsg(position, 0) })
				client.Cmd.Reply(event, queueMsg)
			}
			return
		}
	}

	commandsMutex.RUnlock()
}

func getServiceForConfigCmd(r *regexp.Regexp) string {
	commandsMutex.RLock()
	defer commandsMutex.RUnlock()
	var svc string
	readConfig(func() {
		for _, c := range config.Commands.Completions {
			re := regexp.MustCompile("^" + c.Regex + " (.+)$")
			if re.String() == r.String() {
				svc = c.Service
				return
			}
		}
		for _, c := range config.Commands.Chats {
			re := regexp.MustCompile("^" + c.Regex + " (.+)$")
			if re.String() == r.String() {
				svc = c.Service
				return
			}
		}
	})
	return svc
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

	ircServer, err := network.getNextServer()
	if err != nil {
		log.Error(err.Error())
		return
	}

	sslConfig := &tls.Config{
		ServerName:         ircServer.Host,
		InsecureSkipVerify: ircServer.InsecureSkipVerify,
	}

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
		channels := bot.Network.Channels
		bot.mu.Unlock()
		time.Sleep(time.Millisecond * time.Duration(throttle))
		var noKey []string
		for name, cfg := range channels {
			if cfg.Key != "" {
				client.Cmd.JoinKey(name, cfg.Key)
			} else {
				noKey = append(noKey, name)
			}
		}
		if len(noKey) > 0 {
			client.Cmd.Join(noKey...)
		}
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
		ircServer, err := bot.Network.getNextServer()
		if err != nil {
			log.Error(err.Error())
			break
		}
		client.Config.Server = ircServer.Host
		client.Config.Port = ircServer.GetPort()
		client.Config.ServerPass = ircServer.Pass
		client.Config.SSL = ircServer.Ssl
		sslConfig.InsecureSkipVerify = ircServer.InsecureSkipVerify
	}
	log.Info("Finished quitting")

}
