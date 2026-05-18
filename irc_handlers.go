package main

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
)

func registerIRCHandlers(bot *Bot, client *girc.Client, network Network, log logxi.Logger) {
	client.Handlers.Add(girc.ALL_EVENTS, func(client *girc.Client, event girc.Event) {
		if str, ok := event.Pretty(); ok {
			log.Info(str)
		}
		enqueueFromEvent(network.Name, event)
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
		_, err := resolveIRCUser(network, client, nick, event.Source)
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
}

func handleChanMessage(network Network, client *girc.Client, event girc.Event) {
	host := event.Source.Name + "!" + event.Source.Ident + "@" + event.Source.Host
	msg := event.Params[len(event.Params)-1]
	channel := normalizeIRC(event.Params[0], getCasemapping(network.Name))

	isTrigger := strings.HasPrefix(msg, network.Trigger)

	var mentionMsg string
	if !isTrigger {
		botnick := client.GetNick()
		if strings.HasPrefix(msg, botnick+", ") {
			mentionMsg = msg[len(botnick+", "):]
		} else if strings.HasPrefix(msg, botnick+": ") {
			mentionMsg = msg[len(botnick+": "):]
		} else {
			return
		}
	}

	if isIgnored(host) {
		logger.Info("Ignoring host", host)
		return
	}

	if !isTrigger {
		handleMention(network, client, event, channel, mentionMsg)
		return
	}
	handleTrigger(network, client, event, channel, msg, strings.TrimPrefix(msg, network.Trigger))
}

func handleMention(network Network, client *girc.Client, event girc.Event, channel, msg string) {
	resolvedUser, err := resolveIRCUser(network, client, event.Source.Name, event.Source)
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

	if !ContextExists(network.Name, channel, userID) {
		logger.Info("Ignoring message due to no existing chat context")
		var noCtxMsg string
		readConfig(func() { noCtxMsg = config.Notices.Context.NoContext })
		noCtxMsg = expandNotice(noCtxMsg, map[string]string{"trigger": network.Trigger})
		client.Cmd.Reply(event, warnMsg(noCtxMsg))
		return
	}
	if !checkRate(network, channel) {
		var rateMsg string
		readConfig(func() { rateMsg = config.Notices.Ratemsg() })
		client.Cmd.Reply(event, warnMsg(rateMsg))
		return
	}

	session, err := sessionMgr.GetActiveSession(network.Name, channel, userID)
	if err != nil {
		logger.Error("failed to get active session", "error", err)
		return
	}
	if session == nil {
		return
	}
	sessionCfg, cfgOk := getSessionConfig(session)
	if !cfgOk {
		logger.Warn("chat command not found for session, ignoring mention", "command", session.ChatCommand)
		return
	}
	logger.Info("Running chat completion with existing context")

	position := queueMgr.Enqueue(network.Name, channel, userID, event.Source.Name,
		sessionCfg.Service, "chat",
		func(cx context.Context, output chan<- string) {
			chat(network, client, event, sessionCfg, cx, output, resolvedUser, msg)
		})
	replyIfQueued(client, event, position)
}

func handleTrigger(network Network, client *girc.Client, event girc.Event, channel, msg, stripped string) {
	// amibanned is a special case: it must work even for banned users,
	// so it resolves the user and returns before the ban check.
	if stripped == "amibanned" {
		resolvedUser, err := resolveIRCUser(network, client, event.Source.Name, event.Source)
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
				builtin: true, disabled: isBuiltinDisabled(builtInNames[r]) || isNetworkCommandDisabled(network, builtInNames[r]),
			}
			break
		}
	}
	if match == nil {
		for r, cmd := range configCmds {
			if r.Match([]byte(stripped)) {
				name := configCmdNames[r]
				match = &cmdMatch{cmd: cmd, re: r, args: extractSubmatchArgs(r, stripped), disabled: isNetworkCommandDisabled(network, name)}
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
	resolvedUser, err := resolveIRCUser(network, client, event.Source.Name, event.Source)
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
			replyIfQueued(client, event, position)
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
			replyIfQueued(client, event, position)
			return
		}

		if match.re == stats_re || match.re == delete_re || match.re == resume_re || match.re == support_re {
			match.cmd(network, client, event, ctxWithResolvedUser(context.Background(), resolvedUser), nil, match.args...)
			return
		}

		outCh := make(chan string, outputChannelSize)
		go func() {
			defer close(outCh)
			match.cmd(network, client, event, ctxWithResolvedUser(context.Background(), resolvedUser), outCh, match.args...)
		}()
		go drainToChannel(client, channel, time.Millisecond*network.Throttle, outCh, nil, network.Name)
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
		outCh := make(chan string, outputChannelSize)
		go func() {
			defer close(outCh)
			match.cmd(network, client, event, ctxWithResolvedUser(context.Background(), resolvedUser), outCh, match.args...)
		}()
		go drainToChannel(client, channel, time.Millisecond*network.Throttle, outCh, nil, network.Name)
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
	replyIfQueued(client, event, position)
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
