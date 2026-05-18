package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
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

func newLogger(name string) logxi.Logger {
	l := logxi.New(name)
	l.SetLevel(logxi.LevelAll)
	return l
}

const outputChannelSize = 200

func copyTemplateVars() map[string]string {
	var vars map[string]string
	readConfig(func() {
		vars = maps.Clone(config.TemplateVars)
	})
	if vars == nil {
		vars = make(map[string]string)
	}
	return vars
}

func buildSystemPromptData(network Network, client *girc.Client, channel, userNick string) SystemPromptData {
	data := SystemPromptData{
		Nick:    userNick,
		BotNick: network.Nick,
		Channel: channel,
		Network: network.Name,
		Date:    time.Now().Format("2006-01-02"),
		Vars:    copyTemplateVars(),
	}
	if client != nil {
		data.BotNick = client.GetNick()
		ch := client.LookupChannel(channel)
		var nicks []string
		if ch != nil {
			for _, u := range ch.Users(client) {
				nicks = append(nicks, u.Nick)
			}
			sort.Strings(nicks)
		}
		data.ChanNicks = `["` + strings.Join(nicks, `","`) + `"]`
	}
	return data
}

func resolveIRCUser(network Network, c *girc.Client, nick string, source *girc.Source) (*User, error) {
	casemapping := getCasemapping(network.Name)
	account := ""
	if u := c.LookupUser(nick); u != nil {
		account = u.Extras.Account
	}
	return resolveUser(network.Name, nick, source.Ident, source.Host, account, casemapping)
}

func getSessionConfig(session *Session) (AIConfig, bool) {
	var cfg AIConfig
	var ok bool
	readConfig(func() {
		cfg, ok = config.Commands.Chats[session.ChatCommand]
	})
	if session.SettingsID != nil {
		settings, err := sessionMgr.GetSessionSettings(*session.SettingsID)
		if err == nil && settings != nil {
			cfg = ApplySettings(settings, cfg)
			ok = true
		}
	}
	return cfg, ok
}

func replyIfQueued(client *girc.Client, event girc.Event, position int) {
	if position > 0 {
		var queueMsg string
		readConfig(func() { queueMsg = config.Notices.QueueMsg(position, 0) })
		client.Cmd.Reply(event, queueMsg)
	}
}

func drainToChannel(client *girc.Client, channel string, throttle time.Duration, outCh <-chan string, ctx context.Context, networkName string) {
	for msg := range outCh {
		if ctx != nil && ctx.Err() != nil {
			for range outCh {
			}
			break
		}
		enqueueBotMessage(networkName, channel, msg)
		if action, ok := isIRCAction(msg); ok {
			client.Cmd.Action(channel, action)
		} else {
			client.Cmd.Message(channel, "\x02\x02"+msg)
		}
		time.Sleep(throttle)
	}
}

var commandsMutex sync.RWMutex
var configMu sync.RWMutex

var ignorePatterns []string
var ignoreMu sync.RWMutex
var ignoreWatcher *fsnotify.Watcher
var ignoreFile string

type Bot struct {
	Client      *girc.Client
	Reconnect   bool
	Network     Network
	connectedAt time.Time
	reconnects  int
	quitCh      chan struct{}
	mu          sync.Mutex
	casemapping string
}

func (bot *Bot) Quit() {
	bot.mu.Lock()
	bot.Reconnect = false
	select {
	case <-bot.quitCh:
	default:
		close(bot.quitCh)
	}
	bot.mu.Unlock()
	bot.Client.Cmd.SendRawf("QUIT :%s\r\n", bot.Network.Quitmsg)
}

func (bot *Bot) isReady(channel string) bool {
	return bot.Client != nil && bot.Client.IsConnected() && bot.Client.IsInChannel(channel)
}

var bots map[string]*Bot
var botsMu sync.RWMutex

func init() {
	bots = make(map[string]*Bot)
}

func getBot(network string) (*Bot, bool) {
	botsMu.RLock()
	defer botsMu.RUnlock()
	bot, ok := bots[network]
	return bot, ok
}

func setBot(network string, bot *Bot) {
	botsMu.Lock()
	defer botsMu.Unlock()
	bots[network] = bot
}

func snapshotBots() map[string]*Bot {
	botsMu.RLock()
	defer botsMu.RUnlock()
	m := make(map[string]*Bot, len(bots))
	for k, v := range bots {
		m[k] = v
	}
	return m
}

func getCasemapping(network string) string {
	if bot, ok := getBot(network); ok {
		bot.mu.Lock()
		cm := bot.casemapping
		bot.mu.Unlock()
		if cm != "" {
			return cm
		}
	}
	return "rfc1459"
}

func readConfig(f func()) {
	configMu.RLock()
	defer configMu.RUnlock()
	f()
}

type CmdFunc func(Network, *girc.Client, girc.Event, context.Context, chan<- string, ...string)

// resolvedUserCtxKey carries the *User resolved in handleChanMessage through
// the queue/dispatch path so command handlers (chat, completion, etc.) can
// avoid a redundant DB lookup. Nil if not set.
type resolvedUserCtxKey struct{}

func ctxWithResolvedUser(ctx context.Context, u *User) context.Context {
	if u == nil {
		return ctx
	}
	return context.WithValue(ctx, resolvedUserCtxKey{}, u)
}

func resolvedUserFromCtx(ctx context.Context) *User {
	if ctx == nil {
		return nil
	}
	u, _ := ctx.Value(resolvedUserCtxKey{}).(*User)
	return u
}

var stop_re = regexp.MustCompile("^stop$")
var help_re = regexp.MustCompile("^help(?:\\s+(.+))?$")
var sessions_re = regexp.MustCompile("^sessions(?:\\s+(\\S+))?$")
var history_re = regexp.MustCompile("^history(?:\\s+(.+))?$")
var stats_re = regexp.MustCompile("^mystats$")
var delete_re = regexp.MustCompile("^delete\\s+(\\d+)$")
var resume_re = regexp.MustCompile("^resume\\s+(\\d+)$")
var jobs_re = regexp.MustCompile("^jobs$")
var support_re = regexp.MustCompile("^support$")
var compact_re = regexp.MustCompile("^compact$")
var clone_re = regexp.MustCompile("^clone\\s+(\\S+)$")

type CmdMap map[*regexp.Regexp]CmdFunc

func errorMsg(msg string) string {
	return noticeErrorPrefix.Load().(string) + msg
}

func warnMsg(msg string) string {
	return noticeWarnPrefix.Load().(string) + msg
}

func errorNotice(tmpl string, vars map[string]string) string {
	return errorMsg(expandNotice(tmpl, vars))
}

func extractSubmatchArgs(re *regexp.Regexp, input string) []string {
	var args []string
	for i, m := range re.FindSubmatch([]byte(input)) {
		if i != 0 && len(m) > 0 {
			args = append(args, string(m))
		}
	}
	return args
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
	compact_re: func(n Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, s ...string) {
		historyCompact(n, c, e, ctx, output, s...)
	},
	clone_re: func(n Network, c *girc.Client, e girc.Event, ctx context.Context, output chan<- string, s ...string) {
		historyClone(n, c, e, ctx, output, s...)
	},
}
var configCmds CmdMap
var rateExemptCmds map[*regexp.Regexp]bool
var chatCmds map[*regexp.Regexp]bool

var builtInNames = map[*regexp.Regexp]string{
	stop_re:     "stop",
	help_re:     "help",
	sessions_re: "sessions",
	history_re:  "history",
	stats_re:    "mystats",
	delete_re:   "delete",
	resume_re:   "resume",
	jobs_re:     "jobs",
	support_re:  "support",
	compact_re:  "compact",
	clone_re:    "clone",
}

func isBuiltinDisabled(name string) bool {
	var disabled []string
	readConfig(func() { disabled = config.DisabledBuiltins })
	for _, d := range disabled {
		if d == name {
			return true
		}
	}
	return false
}

func registerCommands(cmds Commands) error {
	commandsMutex.Lock()
	defer commandsMutex.Unlock()
	return registerCommandsLocked(cmds)
}

func registerCommandsLocked(cmds Commands) error {
	newConfigCmds := CmdMap{}
	newExemptCmds := make(map[*regexp.Regexp]bool)
	newChatCmds := make(map[*regexp.Regexp]bool)

	for _, c := range cmds.Completions {
		logger.Debug("added Completions command", c)
		re, err := regexp.Compile("^" + c.Regex + " (.+)$")
		if err != nil {
			return fmt.Errorf("invalid regex for completions command %q: %w", c.Regex, err)
		}
		newConfigCmds[re] = func(network Network, client *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
			completion(network, client, e, c, ctx, output, args...)
		}
	}
	for _, c := range cmds.Chats {
		logger.Debug("added Chats command", c)
		re, err := regexp.Compile("^" + c.Regex + " (.+)$")
		if err != nil {
			return fmt.Errorf("invalid regex for chats command %q: %w", c.Regex, err)
		}
		newConfigCmds[re] = func(network Network, client *girc.Client, e girc.Event, ctx context.Context, output chan<- string, args ...string) {
			chat(network, client, e, c, ctx, output, nil, args...)
		}
		newChatCmds[re] = true
	}
	for _, c := range cmds.Tools {
		logger.Debug("added Tools command", c)
		pattern := "^" + c.Regex + "$"
		if c.Arg != "" {
			pattern = "^" + c.Regex + " (.+)$"
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return fmt.Errorf("invalid regex for tools command %q: %w", c.Regex, err)
		}
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
	return nil
}

func reloadAll() error {
	commandsMutex.Lock()
	defer commandsMutex.Unlock()
	configMu.Lock()
	defer configMu.Unlock()
	if err := loadReloadableDir(configDir, &config); err != nil {
		return err
	}
	for name, bot := range snapshotBots() {
		bot.mu.Lock()
		cm := bot.casemapping
		bot.mu.Unlock()
		if net, ok := config.Networks[name]; ok {
			net.Casemapping = cm
			config.Networks[name] = net
		}
	}
	if apiLogger != nil {
		apiLogger.CloseAll()
	}
	initAPILogger(config, configDir)
	initIncidentLogger(config)
	reloadMCPClients(config.MCPs)
	if err := registerCommandsLocked(config.Commands); err != nil {
		return err
	}
	if queueMgr != nil {
		queueMgr.UpdateServiceLimits(config.Services)
		queueMgr.UpdateNotices(config.Notices)
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
	logger = newLogger("main")
	logger.Info("Config loaded", "networks", len(config.Networks))
	initAPILogger(config, configDir)
	initIncidentLogger(config)

	var dbErr error
	theDB, dbErr = initDB(config.Database, logger)
	if dbErr != nil {
		logger.Error("Failed to initialize database", "error", dbErr)
		os.Exit(1)
	}
	sessionMgr = NewSessionManager(theDB)

	if err := initLogWriter(config.Logging); err != nil {
		logger.Error("Failed to initialize log writer", "error", err)
		os.Exit(1)
	}

	LoadContextStore()
	CleanupContexts()
	initMCPClients()
	queueMgr = NewQueueManager(config.Notices, config.MaxQueueDepth)
	queueMgr.UpdateServiceLimits(config.Services)
	queueMgr.Start()
	startJobManager()
	startToolJobManager()
	if err := registerCommands(config.Commands); err != nil {
		fmt.Fprintf(os.Stderr, "error registering commands: %v\n", err)
		os.Exit(1)
	}

	ignorePath := filepath.Join(configDir, "ignores.txt")
	loadIgnores(ignorePath)
	watchIgnores(ignorePath)
	startRateLimitGC()

	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			sweepExpiredBans(theDB)
		}
	}()

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
		logger.Info("Caught signal", "signal", signal.String())
		shutdown()
	}()

	if noTUI {
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

func shutdown() {
	if !atomic.CompareAndSwapInt32(&shutdownOnce, 0, 1) {
		return
	}

	if logFlushStop != nil {
		close(logFlushStop)
		<-logFlushDone
	}

	if ignoreWatcher != nil {
		ignoreWatcher.Close()
	}
	if apiLogger != nil {
		apiLogger.CloseAll()
	}
	stopLogWriter()
	if queueMgr != nil {
		queueMgr.Stop()
	}
	stopJobManager()
	stopToolJobManager()
	closeMCPClients()

	for _, bot := range snapshotBots() {
		bot.Quit()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	time.Sleep(1 * time.Second)

	closeDB(theDB)
	closeLogFile()
	if tuiApp != nil {
		tuiApp.QueueUpdateDraw(func() {
			tuiApp.Stop()
		})
	}
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

func sendOrDone(ctx context.Context, output chan<- string, msg string) bool {
	select {
	case output <- msg:
		return true
	case <-ctx.Done():
		return false
	}
}

func sendToOutput(out string, output chan<- string, ctx context.Context) {
	for _, line := range wrapForIRC(out) {
		if len(line) <= 0 {
			continue
		}
		if !sendOrDone(ctx, output, line) {
			return
		}
	}
}

// DO NOT DELETE — stop is only for stopping the current text output, not for cancelling async jobs
func stop(network Network, client *girc.Client, m girc.Event, _ interface{}, _ ...string) {
	logger.Info("stop requested")
	channel := normalizeIRC(m.Params[0], getCasemapping(network.Name))
	if queueMgr.StopCurrent(network.Name, channel) {
		var stoppedMsg string
		readConfig(func() { stoppedMsg = config.Notices.Queue.Stopped })
		client.Cmd.Reply(m, stoppedMsg)
	}
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

	log := newLogger(network.Name)

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
		User:       network.User,
		Name:       network.RealName,
		SSL:        ircServer.Ssl,
		TLSConfig:  sslConfig,
		AllowFlood: true,
	})

	bot := Bot{
		Client:    client,
		Reconnect: true,
		Network:   network,
		quitCh:    make(chan struct{}),
	}
	setBot(network.Name, &bot)

	registerIRCHandlers(&bot, client, network, log)

	for {
		if err := client.Connect(); err != nil {
			bot.mu.Lock()
			var uptime string
			if !bot.connectedAt.IsZero() {
				uptime = time.Since(bot.connectedAt).Round(time.Second).String()
			} else {
				uptime = "never connected"
			}
			bot.reconnects++
			attempt := bot.reconnects
			delay := *bot.Network.ReconnectDelay
			if attempt == 1 {
				delay = 0
			}
			quitCh := bot.quitCh
			bot.mu.Unlock()

			log.Warn("disconnected",
				"error", err.Error(),
				"uptime", uptime,
				"attempt", attempt,
				"retry_in", delay,
			)
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-quitCh:
				}
			}
		}
		bot.mu.Lock()
		reconnect := bot.Reconnect
		bot.mu.Unlock()
		if !reconnect {
			log.Info("Reconnect not requested")
			break
		}
		ircServer, err := bot.Network.getNextServer()
		if err != nil {
			log.Error(err.Error())
			break
		}
		log.Info("reconnecting", "host", ircServer.Host, "port", ircServer.GetPort())
		client.Config.Server = ircServer.Host
		client.Config.Port = ircServer.GetPort()
		client.Config.ServerPass = ircServer.Pass
		client.Config.SSL = ircServer.Ssl
		sslConfig.InsecureSkipVerify = ircServer.InsecureSkipVerify
	}
	log.Info("Finished quitting")

}
