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
	logger = logxi.New("main")
	logger.SetLevel(logxi.LevelAll)
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
		if !atomic.CompareAndSwapInt32(&shutdownOnce, 0, 1) {
			return
		}
		logger.Info("Caught signal", "signal", signal.String())

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

func handleChanMessage(network Network, client *girc.Client, event girc.Event) {
	host := event.Source.Name + "!" + event.Source.Ident + "@" + event.Source.Host
	msg := event.Params[len(event.Params)-1]
	channel := normalizeIRC(event.Params[0], getCasemapping(network.Name))

	casemapping := getCasemapping(network.Name)
	account := ""
	if u := client.LookupUser(event.Source.Name); u != nil {
		account = u.Extras.Account
	}

	isTrigger := strings.HasPrefix(msg, network.Trigger)

	if !isTrigger {
		botnick := client.GetNick()
		if !strings.HasPrefix(msg, botnick+", ") && !strings.HasPrefix(msg, botnick+": ") {
			return
		}
	}
	if isIgnored(host) {
		logger.Info("Ignoring host", host)
		return
	}

	// DESIGN NOTE: resolveUser does multiple DB queries (account lookup, nick lookup,
	// host recovery). For the non-trigger (mention) path we need it immediately.
	// For the trigger path we defer it until after confirming the message matches
	// an actual command, so that "-random_text" that matches nothing skips the DB work.
	if !isTrigger {
		resolvedUser, err := resolveUser(network.Name, event.Source.Name, event.Source.Ident, event.Source.Host, account, casemapping)
		if err != nil {
			logger.Error("failed to resolve user", "error", err)
		}
		proceed, userID := handleResolveResult(client, event, resolvedUser, err)
		if !proceed {
			return
		}

		if isBanned(theDB, userID, network.Name, channel, "") {
			logger.Info("User is banned", "user_id", userID, "nick", event.Source.Name)
			return
		}

		botnick := client.GetNick()
		if !ContextExists(network.Name, channel, userID) {
			logger.Info("Ignoring message due to no existing chat context")
			var noCtxMsg string
			readConfig(func() { noCtxMsg = config.Notices.Context.NoContext })
			client.Cmd.Reply(event, warnMsg(noCtxMsg))
			return
		}
		if !checkRate(network, channel) {
			var rateMsg string
			readConfig(func() { rateMsg = config.Notices.Ratemsg() })
			client.Cmd.Reply(event, warnMsg(rateMsg))
			return
		}
		msg = msg[len(botnick+", "):]

		session, _ := sessionMgr.GetActiveSession(network.Name, channel, userID)
		if session == nil {
			return
		}
		var sessionCfg AIConfig
		readConfig(func() {
			sessionCfg, _ = config.Commands.Chats[session.ChatCommand]
		})
		if session.SettingsID != nil {
			settings, err := sessionMgr.GetSessionSettings(*session.SettingsID)
			if err != nil {
				logger.Warn("failed to load stored settings, using current", "error", err)
			} else if settings != nil {
				sessionCfg = ApplySettings(settings, sessionCfg)
			}
		}
		logger.Info("Running chat completion with existing context")

		position := queueMgr.Enqueue(network.Name, channel, userID, event.Source.Name,
			sessionCfg.Service, "chat",
			func(cx context.Context, output chan<- string) {
				chat(network, client, event, sessionCfg, cx, output, resolvedUser, msg)
			})
		if position > 0 {
			var queueMsg string
			readConfig(func() { queueMsg = config.Notices.QueueMsg(position, 0) })
			client.Cmd.Reply(event, queueMsg)
		}
		return
	}

	// --- Trigger path ---
	stripped := strings.TrimPrefix(msg, network.Trigger)

	// amibanned is a special case: it must work even for banned users,
	// so it resolves the user and returns before the ban check.
	if stripped == "amibanned" {
		resolvedUser, err := resolveUser(network.Name, event.Source.Name, event.Source.Ident, event.Source.Host, account, casemapping)
		if err != nil {
			logger.Error("failed to resolve user", "error", err)
		}
		proceed, userID := handleResolveResult(client, event, resolvedUser, err)
		if !proceed {
			return
		}
		bans := getActiveBansForUser(theDB, userID, network.Name)
		n := getNotices()
		if len(bans) == 0 {
			client.Cmd.Reply(event, n.Bans.AmIBannedNone)
		} else {
			for _, ban := range bans {
				remainingStr := "never"
				if !ban.ExpiresAt.IsZero() {
					remaining := time.Until(ban.ExpiresAt).Round(time.Minute)
					if remaining < 0 {
						remaining = 0
					}
					remainingStr = formatDuration(remaining)
				}
				client.Cmd.Reply(event, expandNotice(n.Bans.AmIBanned, map[string]string{
					"reason":    ban.Reason,
					"remaining": remainingStr,
					"banner":    ban.BannerNick,
				}))
			}
		}
		return
	}

	// Cheap command match phase — no DB work yet.
	type cmdMatch struct {
		cmd      CmdFunc
		re       *regexp.Regexp
		args     []string
		builtin  bool
		disabled bool
	}
	var match *cmdMatch
	commandsMutex.RLock()
	for r, cmd := range builtInCmds {
		if r.Match([]byte(stripped)) {
			match = &cmdMatch{
				cmd: cmd, re: r, args: extractSubmatchArgs(r, stripped),
				builtin: true, disabled: isBuiltinDisabled(builtInNames[r]),
			}
			break
		}
	}
	if match == nil {
		for r, cmd := range configCmds {
			if r.Match([]byte(stripped)) {
				match = &cmdMatch{cmd: cmd, re: r, args: extractSubmatchArgs(r, stripped)}
				break
			}
		}
	}
	commandsMutex.RUnlock()

	// No command matched or disabled builtin — return without DB lookup.
	if match == nil || match.disabled {
		return
	}

	// Command matched — now resolve user and check bans.
	resolvedUser, err := resolveUser(network.Name, event.Source.Name, event.Source.Ident, event.Source.Host, account, casemapping)
	if err != nil {
		logger.Error("failed to resolve user", "error", err)
	}
	proceed, userID := handleResolveResult(client, event, resolvedUser, err)
	if !proceed {
		return
	}

	if isBanned(theDB, userID, network.Name, channel, "") {
		logger.Info("User is banned", "user_id", userID, "nick", event.Source.Name)
		return
	}

	// Execute the matched command.
	if match.builtin {
		if match.re == stop_re {
			match.cmd(network, client, event, ctxWithResolvedUser(context.Background(), resolvedUser), nil, match.args...)
			return
		}
		if !checkRate(network, channel) {
			var rateMsg string
			readConfig(func() { rateMsg = config.Notices.Ratemsg() })
			client.Cmd.Reply(event, warnMsg(rateMsg))
			return
		}
		if match.re == compact_re {
			position := queueMgr.Enqueue(network.Name, channel, userID, event.Source.Name, "", msg,
				func(cx context.Context, output chan<- string) {
					match.cmd(network, client, event, ctxWithResolvedUser(cx, resolvedUser), output, match.args...)
				})
			if position > 0 {
				var queueMsg string
				readConfig(func() { queueMsg = config.Notices.QueueMsg(position, 0) })
				client.Cmd.Reply(event, queueMsg)
			}
			return
		}

		// Clone is queued (not direct-dispatch) because cloneDBSession
		// runs a multi-step DB transaction and acquires sessionCreationMu.
		// Queueing serializes clones per channel slot, preventing concurrent
		// clones from colliding on the same source session or racing with
		// chat() session creation.
		if match.re == clone_re {
			position := queueMgr.Enqueue(network.Name, channel, userID, event.Source.Name, "", msg,
				func(cx context.Context, output chan<- string) {
					match.cmd(network, client, event, ctxWithResolvedUser(cx, resolvedUser), output, match.args...)
				})
			if position > 0 {
				var queueMsg string
				readConfig(func() { queueMsg = config.Notices.QueueMsg(position, 0) })
				client.Cmd.Reply(event, queueMsg)
			}
			return
		}

		if match.re == stats_re || match.re == delete_re || match.re == resume_re || match.re == support_re {
			match.cmd(network, client, event, ctxWithResolvedUser(context.Background(), resolvedUser), nil, match.args...)
			return
		}

		outCh := make(chan string, 200)
		go func() {
			defer close(outCh)
			match.cmd(network, client, event, ctxWithResolvedUser(context.Background(), resolvedUser), outCh, match.args...)
		}()
		go func() {
			for msg := range outCh {
				if action, ok := isIRCAction(msg); ok {
					client.Cmd.Action(channel, action)
				} else {
					client.Cmd.Message(channel, "\x02\x02"+msg)
				}
				time.Sleep(time.Millisecond * time.Duration(network.Throttle))
			}
		}()
		return
	}

	// Config command execution.
	if !checkRate(network, channel) {
		var rateMsg string
		readConfig(func() { rateMsg = config.Notices.Ratemsg() })
		client.Cmd.Reply(event, warnMsg(rateMsg))
		return
	}
	if rateExemptCmds[match.re] {
		if chatCmds[match.re] {
			ClearContext(network.Name, channel, userID)
		}
		outCh := make(chan string, 200)
		go func() {
			defer close(outCh)
			match.cmd(network, client, event, ctxWithResolvedUser(context.Background(), resolvedUser), outCh, match.args...)
		}()
		go func() {
			for m := range outCh {
				if action, ok := isIRCAction(m); ok {
					client.Cmd.Action(channel, action)
				} else {
					client.Cmd.Message(channel, "\x02\x02"+m)
				}
				time.Sleep(time.Millisecond * time.Duration(network.Throttle))
			}
		}()
		return
	}
	if chatCmds[match.re] {
		ClearContext(network.Name, channel, userID)
	}
	svc := getServiceForConfigCmd(match.re)
	position := queueMgr.Enqueue(network.Name, channel, userID, event.Source.Name, svc, msg,
		func(cx context.Context, output chan<- string) {
			match.cmd(network, client, event, ctxWithResolvedUser(cx, resolvedUser), output, match.args...)
		})
	if position > 0 {
		var queueMsg string
		readConfig(func() { queueMsg = config.Notices.QueueMsg(position, 0) })
		client.Cmd.Reply(event, queueMsg)
	}
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

	client.Handlers.Add(girc.ALL_EVENTS, func(client *girc.Client, event girc.Event) {
		if str, ok := event.Pretty(); ok {
			log.Info(str)
		}
	})

	client.Handlers.Add(girc.RPL_WELCOME, func(client *girc.Client, event girc.Event) {
		bot.mu.Lock()
		bot.connectedAt = time.Now()
		bot.reconnects = 0
		if cm, ok := client.GetServerOption("CASEMAPPING"); ok {
			bot.casemapping = cm
		} else {
			bot.casemapping = "rfc1459"
		}
		bot.Network.Casemapping = bot.casemapping
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

	client.Handlers.Add(girc.NICK, func(client *girc.Client, event girc.Event) {
		oldNick := event.Source.Name
		newNick := event.Params[0]
		casemapping := getCasemapping(network.Name)
		if recordNickChange(network.Name, oldNick, newNick, casemapping) {
			log.Debug("tracked nick change", "old", oldNick, "new", newNick)
		}
	})

	client.Handlers.Add(girc.QUIT, func(client *girc.Client, event girc.Event) {
		casemapping := getCasemapping(network.Name)
		nick := event.Source.Name
		norm := normalizeIRC(nick, casemapping)
		user, _ := getActiveUserByNormalizedNick(network.Name, norm)
		if user != nil {
			log.Debug("tracked user quit", "nick", nick, "user_id", user.ID, "network", network.Name)
			if err := releaseUserNick(user.ID); err != nil {
				log.Error("failed to release user nick on quit", "user_id", user.ID, "error", err)
			}
		}
	})

	client.Handlers.Add(girc.JOIN, func(client *girc.Client, event girc.Event) {
		nick := event.Source.Name
		if nick == client.GetNick() {
			return
		}
		casemapping := getCasemapping(network.Name)
		account := ""
		if u := client.LookupUser(nick); u != nil {
			account = u.Extras.Account
		}
		_, err := resolveUser(network.Name, nick, event.Source.Ident, event.Source.Host, account, casemapping)
		if err != nil {
			log.Error("failed to resolve user on join", "nick", nick, "error", err)
		}
	})

	client.Handlers.Add(girc.PART, func(client *girc.Client, event girc.Event) {
		nick := event.Source.Name
		if nick == client.GetNick() {
			return
		}
		if u := client.LookupUser(nick); u == nil || len(u.ChannelList) == 0 {
			casemapping := getCasemapping(network.Name)
			norm := normalizeIRC(nick, casemapping)
			user, _ := getActiveUserByNormalizedNick(network.Name, norm)
			if user != nil {
				log.Debug("user no longer visible after part, releasing nick", "nick", nick, "user_id", user.ID, "network", network.Name)
				if err := releaseUserNick(user.ID); err != nil {
					log.Error("failed to release user nick on part", "user_id", user.ID, "error", err)
				}
			}
		}
	})

	client.Handlers.Add(girc.KICK, func(client *girc.Client, event girc.Event) {
		if len(event.Params) < 2 {
			return
		}
		kickedNick := event.Params[1]
		if kickedNick == client.GetNick() {
			return
		}
		if u := client.LookupUser(kickedNick); u == nil || len(u.ChannelList) == 0 {
			casemapping := getCasemapping(network.Name)
			norm := normalizeIRC(kickedNick, casemapping)
			user, _ := getActiveUserByNormalizedNick(network.Name, norm)
			if user != nil {
				log.Debug("user no longer visible after kick, releasing nick", "nick", kickedNick, "user_id", user.ID, "network", network.Name)
				if err := releaseUserNick(user.ID); err != nil {
					log.Error("failed to release user nick on kick", "user_id", user.ID, "error", err)
				}
			}
		}
	})

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
