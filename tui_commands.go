package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rivo/tview"
)

var tuiCommands = map[string]func(parts []string, text string){
	"/help":        tuiCmdHelp,
	"/reload":      tuiCmdReload,
	"/quit":        tuiCmdQuit,
	"/exit":        tuiCmdQuit,
	"/join":        tuiCmdJoin,
	"/part":        tuiCmdPart,
	"/nick":        tuiCmdNick,
	"/ban":         tuiCmdBan,
	"/unban":       tuiCmdUnban,
	"/bans":        tuiCmdBans,
	"/banhistory":  tuiCmdBanHistory,
	"/user":        tuiCmdUser,
	"/usersearch":  tuiCmdUserSearch,
	"/usermerge":   tuiCmdUserMerge,
	"/userrelease": tuiCmdUserRelease,
	"/flagged":     tuiCmdFlagged,
	"/sessions":    tuiCmdSessions,
	"/compact":     tuiCmdCompact,
}

func tuiCmdHelp(_ []string, _ string) {
	fmt.Fprintf(logView, "[white]Commands:\n")
	fmt.Fprintf(logView, "  /help                        - Show this help\n")
	fmt.Fprintf(logView, "  /reload                      - Reload config from disk\n")
	fmt.Fprintf(logView, "  /reload <mcp-name>           - Reload MCP server config (SIGHUP/HTTP)\n")
	fmt.Fprintf(logView, "  /quit, /exit                 - Shut down\n")
	fmt.Fprintf(logView, "  /join <network> <channel>    - Join a channel\n")
	fmt.Fprintf(logView, "  /part <network> <channel> [message]\n")
	fmt.Fprintf(logView, "                               - Leave a channel\n")
	fmt.Fprintf(logView, "  /nick <network> <nick>       - Change nickname\n")
	fmt.Fprintf(logView, "  /ban <network> <nick> <duration> [reason]\n")
	fmt.Fprintf(logView, "                               - Ban a user\n")
	fmt.Fprintf(logView, "  /unban <network> <nick>      - Unban a user\n")
	fmt.Fprintf(logView, "  /bans [network]              - List active bans\n")
	fmt.Fprintf(logView, "  /banhistory <network> <nick>  - Show ban history for a user\n")
	fmt.Fprintf(logView, "  /user <network> <nick|id>    - Show user details\n")
	fmt.Fprintf(logView, "  /usersearch <network> <query> - Search users by nick/account/host\n")
	fmt.Fprintf(logView, "  /usermerge <ghost_id> <target_id> [hash]\n")
	fmt.Fprintf(logView, "                               - Merge ghost user into target\n")
	fmt.Fprintf(logView, "  /userrelease <network> <nick|id>\n")
	fmt.Fprintf(logView, "                               - Release a user's nick claim\n")
	fmt.Fprintf(logView, "  /flagged [network]           - List flagged users (resolveUser fallback rows)\n")
	fmt.Fprintf(logView, "  /sessions <network> <nick|id> [channel]\n")
	fmt.Fprintf(logView, "                               - List sessions for a user\n")
	fmt.Fprintf(logView, "  /compact <session-id>        - Summarize old messages of a session\n")
}

func tuiCmdReload(parts []string, _ string) {
	if len(parts) >= 2 {
		mcpName := parts[1]
		result, err := signalMCPServer(mcpName)
		if err != nil {
			fmt.Fprintf(logView, "[red]Reload %s failed: %s[white]\n", mcpName, err)
		} else {
			fmt.Fprintf(logView, "[green]Reload signal sent to %s[white]\n", mcpName)
			for _, w := range result.Warnings {
				fmt.Fprintf(logView, "[yellow]Warning: %s[white]\n", w)
			}
		}
	} else {
		if err := reloadAll(); err != nil {
			fmt.Fprintf(logView, "[red]Reload failed: %s[white]\n", err)
		} else {
			fmt.Fprintf(logView, "[green]Reloaded commands, services, and prompt enhancements[white]\n")
		}
	}
}

func tuiCmdQuit(_ []string, _ string) {
	requestShutdown()
}

func tuiCmdJoin(parts []string, _ string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /join <network> <channel>[white]\n")
		return
	}
	network, channel := parts[1], parts[2]
	bot, ok := getBot(network)
	if !ok {
		fmt.Fprintf(logView, "[red]Unknown network: %s[white]\n", network)
		return
	}
	bot.mu.Lock()
	if bot.Network.Channels == nil {
		bot.Network.Channels = make(map[string]ChannelConfig)
	}
	_, alreadyJoined := bot.Network.Channels[channel]
	if alreadyJoined {
		bot.mu.Unlock()
		fmt.Fprintf(logView, "[yellow]Already in %s on %s[white]\n", channel, network)
		return
	}
	bot.Network.Channels[channel] = ChannelConfig{}
	bot.mu.Unlock()
	bot.Client.Cmd.Join(channel)
	fmt.Fprintf(logView, "[green]Joined %s on %s[white]\n", channel, network)
}

func tuiCmdPart(parts []string, _ string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /part <network> <channel> [message][white]\n")
		return
	}
	network, channel := parts[1], parts[2]
	bot, ok := getBot(network)
	if !ok {
		fmt.Fprintf(logView, "[red]Unknown network: %s[white]\n", network)
		return
	}
	bot.mu.Lock()
	if bot.Network.Channels == nil {
		bot.Network.Channels = make(map[string]ChannelConfig)
	}
	_, found := bot.Network.Channels[channel]
	if found {
		delete(bot.Network.Channels, channel)
	}
	bot.mu.Unlock()
	if !found {
		fmt.Fprintf(logView, "[yellow]Not in %s on %s[white]\n", channel, network)
		return
	}
	if len(parts) >= 4 {
		bot.Client.Cmd.PartMessage(channel, parts[3])
	} else {
		bot.Client.Cmd.Part(channel)
	}
	fmt.Fprintf(logView, "[green]Parted %s on %s[white]\n", channel, network)
}

func tuiCmdNick(parts []string, _ string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /nick <network> <nick>[white]\n")
		return
	}
	network, nick := parts[1], parts[2]
	bot, ok := getBot(network)
	if !ok {
		fmt.Fprintf(logView, "[red]Unknown network: %s[white]\n", network)
		return
	}
	bot.mu.Lock()
	bot.Network.Nick = nick
	bot.mu.Unlock()
	bot.Client.Config.Nick = nick
	bot.Client.Cmd.Nick(nick)
	fmt.Fprintf(logView, "[green]Nick change to %s on %s[white]\n", nick, network)
}

func tuiCmdBan(_ []string, text string) {
	parts := strings.SplitN(text, " ", 5)
	if len(parts) < 4 {
		fmt.Fprintf(logView, "[yellow]Usage: /ban <network> <nick> <duration> [reason][white]\n")
		return
	}
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
		return
	}
	network, banNick := parts[1], parts[2]
	durationStr := parts[3]
	reason := "manual ban"
	if len(parts) >= 5 {
		reason = parts[4]
	}
	duration, err := parseBanDuration(durationStr)
	if err != nil {
		fmt.Fprintf(logView, "[red]Invalid duration: %s[white]\n", err)
		return
	}
	var maxDur time.Duration
	readConfig(func() {
		maxDur, _ = parseBanDuration(config.Bans.MaxDuration)
	})
	if maxDur > 0 && duration > maxDur {
		fmt.Fprintf(logView, "[yellow]Capping ban duration from %s to max %s[white]\n", formatDuration(duration), formatDuration(maxDur))
		duration = maxDur
	}
	cm := getCasemapping(network)
	user, err := resolveUserByNick(network, banNick, cm)
	if err != nil || user == nil {
		fmt.Fprintf(logView, "[red]User %s not found on %s[white]\n", banNick, network)
		return
	}
	_, err = createBan(theDB, user.ID, network, "", "", reason, duration, nil, "tui")
	if err != nil {
		fmt.Fprintf(logView, "[red]Failed to ban: %s[white]\n", err)
		return
	}
	fmt.Fprintf(logView, "[green]Banned %s on %s for %s: %s[white]\n", banNick, network, formatDuration(duration), reason)
}

func tuiCmdUnban(parts []string, _ string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /unban <network> <nick>[white]\n")
		return
	}
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
		return
	}
	network, unbanNick := parts[1], parts[2]
	cm := getCasemapping(network)
	user, err := resolveUserByNick(network, unbanNick, cm)
	if err != nil || user == nil {
		fmt.Fprintf(logView, "[red]User %s not found on %s[white]\n", unbanNick, network)
		return
	}
	if err := deactivateBansForUser(theDB, user.ID, network); err != nil {
		fmt.Fprintf(logView, "[red]Failed to unban: %s[white]\n", err)
		return
	}
	fmt.Fprintf(logView, "[green]Unbanned %s on %s[white]\n", unbanNick, network)
}

func tuiCmdBans(parts []string, _ string) {
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
		return
	}
	network := ""
	if len(parts) >= 2 {
		network = parts[1]
	}
	if network == "" {
		var bans []Ban
		theDB.Where("active = ?", true).Order("created_at DESC").Find(&bans)
		if len(bans) == 0 {
			fmt.Fprintf(logView, "[white]No active bans.[white]\n")
			return
		}
		for _, b := range bans {
			fmt.Fprintf(logView, "[white]#%d %s/%d %s expires %s[white]\n", b.ID, b.Network, b.UserID, b.Reason, b.ExpiresAt.Format("2006-01-02 15:04"))
		}
		return
	}
	bans, err := getActiveBans(theDB, network)
	if err != nil {
		fmt.Fprintf(logView, "[red]Failed to list bans: %s[white]\n", err)
		return
	}
	if len(bans) == 0 {
		fmt.Fprintf(logView, "[white]No active bans on %s.[white]\n", network)
		return
	}
	for _, b := range bans {
		var user User
		theDB.First(&user, b.UserID)
		fmt.Fprintf(logView, "[white]#%d %s (%s) %s expires %s[white]\n", b.ID, tview.Escape(displayNick(&user)), formatDuration(b.Duration), tview.Escape(b.Reason), b.ExpiresAt.Format("2006-01-02 15:04"))
	}
}

func tuiCmdBanHistory(parts []string, _ string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /banhistory <network> <nick>[white]\n")
		return
	}
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
		return
	}
	network, histNick := parts[1], parts[2]
	cm := getCasemapping(network)
	user, err := resolveUserByNick(network, histNick, cm)
	if err != nil {
		fmt.Fprintf(logView, "[red]Error looking up user: %s[white]\n", err)
		return
	}
	if user == nil {
		fmt.Fprintf(logView, "[yellow]User %s not found on %s[white]\n", histNick, network)
		return
	}
	bans, err := getBanHistory(theDB, user.ID)
	if err != nil {
		fmt.Fprintf(logView, "[red]Error fetching ban history: %s[white]\n", err)
		return
	}
	if len(bans) == 0 {
		fmt.Fprintf(logView, "[white]No ban history for %s (id: %d).[white]\n", tview.Escape(displayNick(user)), user.ID)
		return
	}
	fmt.Fprintf(logView, "[white]Ban history for %s (id: %d):[white]\n", tview.Escape(displayNick(user)), user.ID)
	for _, b := range bans {
		status := "expired"
		if b.Active {
			status = "ACTIVE"
		}
		fmt.Fprintf(logView, "[white]#%d %s %s (%s) by %s, %s ago[white]\n", b.ID, status, tview.Escape(b.Reason), formatDuration(b.Duration), tview.Escape(b.BannerNick), formatDuration(time.Since(b.CreatedAt)))
	}
}

func tuiCmdUser(parts []string, _ string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /user <network> <nick|id>[white]\n")
		return
	}
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
		return
	}
	network, userRef := parts[1], parts[2]
	var info *UserInfo
	if id, err := strconv.ParseInt(userRef, 10, 64); err == nil {
		var infoErr error
		info, infoErr = getUserInfo(id)
		if infoErr != nil {
			fmt.Fprintf(logView, "[red]Error: %s[white]\n", infoErr)
			return
		}
	} else {
		cm := getCasemapping(network)
		user, err := resolveUserByNick(network, userRef, cm)
		if err != nil {
			fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
			return
		}
		if user == nil {
			fmt.Fprintf(logView, "[yellow]User %s not found on %s[white]\n", userRef, network)
			return
		}
		info, err = getUserInfo(user.ID)
		if err != nil {
			fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
			return
		}
	}
	if info == nil {
		fmt.Fprintf(logView, "[yellow]User not found[white]\n")
		return
	}
	printUserInfo(logView, info)
}

func tuiCmdUserSearch(parts []string, _ string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /usersearch <network> <query>[white]\n")
		return
	}
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
		return
	}
	network, query := parts[1], parts[2]
	results, err := searchUsers(network, query)
	if err != nil {
		fmt.Fprintf(logView, "[red]Search error: %s[white]\n", err)
		return
	}
	if len(results) == 0 {
		fmt.Fprintf(logView, "[white]No users matching %q on %s.[white]\n", query, network)
		return
	}
	fmt.Fprintf(logView, "[white]%d user(s) matching %q on %s:[white]\n", len(results), query, network)
	for _, r := range results {
		released := ""
		if r.Released {
			released = " [red](released)[white]"
		}
		account := ""
		if r.IRCAccount != "" {
			account = fmt.Sprintf(" account:%s", tview.Escape(r.IRCAccount))
		}
		fmt.Fprintf(logView, "[white]  #%d %s hosts:%d sessions:%d%s%s[white]\n",
			r.ID, tview.Escape(r.DisplayName()), r.HostCount, r.SessionCount, account, released)
	}
}

func tuiCmdUserMerge(_ []string, text string) {
	parts := strings.SplitN(text, " ", 5)
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /usermerge <ghost_id> <target_id> [hash][white]\n")
		return
	}
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
		return
	}
	ghostID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		fmt.Fprintf(logView, "[red]Invalid ghost user ID: %s[white]\n", parts[1])
		return
	}
	targetID, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		fmt.Fprintf(logView, "[red]Invalid target user ID: %s[white]\n", parts[2])
		return
	}
	if ghostID == targetID {
		fmt.Fprintf(logView, "[red]Cannot merge a user into itself[white]\n")
		return
	}
	ghost, err := getUserByID(ghostID)
	if err != nil {
		fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
		return
	}
	if ghost == nil {
		fmt.Fprintf(logView, "[red]Ghost user #%d not found[white]\n", ghostID)
		return
	}
	target, err := getUserByID(targetID)
	if err != nil {
		fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
		return
	}
	if target == nil {
		fmt.Fprintf(logView, "[red]Target user #%d not found[white]\n", targetID)
		return
	}

	if len(parts) >= 4 && parts[3] != "" {
		expected := computeMergeHash(ghost, target)
		if parts[3] != expected {
			fmt.Fprintf(logView, "[red]Hash mismatch. Users may have changed. Re-run without hash to verify.[white]\n")
			return
		}
		if err := mergeUser(ghostID, targetID); err != nil {
			fmt.Fprintf(logView, "[red]Merge failed: %s[white]\n", err)
			return
		}
		fmt.Fprintf(logView, "[green]Merged user #%d (%s) into #%d (%s)[white]\n",
			ghostID, tview.Escape(displayNick(ghost)), targetID, tview.Escape(displayNick(target)))
	} else {
		fmt.Fprintf(logView, "[white]Ghost (will be deleted):[white]\n")
		ghostInfo, _ := getUserInfo(ghostID)
		if ghostInfo != nil {
			printUserInfo(logView, ghostInfo)
		}
		fmt.Fprintf(logView, "[white]Target (will survive):[white]\n")
		targetInfo, _ := getUserInfo(targetID)
		if targetInfo != nil {
			printUserInfo(logView, targetInfo)
		}
		hash := computeMergeHash(ghost, target)
		fmt.Fprintf(logView, "[yellow]Confirm: /usermerge %d %d %s[white]\n", ghostID, targetID, hash)
	}
}

func tuiCmdUserRelease(parts []string, _ string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /userrelease <network> <nick|id>[white]\n")
		return
	}
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
		return
	}
	network, userRef := parts[1], parts[2]
	var user *User
	if id, err := strconv.ParseInt(userRef, 10, 64); err == nil {
		user, err = getUserByID(id)
		if err != nil {
			fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
			return
		}
	} else {
		cm := getCasemapping(network)
		var err error
		user, err = resolveUserByNick(network, userRef, cm)
		if err != nil {
			fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
			return
		}
	}
	if user == nil {
		fmt.Fprintf(logView, "[yellow]User not found[white]\n")
		return
	}
	if user.Released {
		fmt.Fprintf(logView, "[yellow]User #%d nick is already released[white]\n", user.ID)
		return
	}
	oldNick := displayNick(user)
	if err := releaseUserNick(user.ID); err != nil {
		fmt.Fprintf(logView, "[red]Failed to release nick: %s[white]\n", err)
		return
	}
	fmt.Fprintf(logView, "[green]Released nick %q for user #%d on %s[white]\n",
		tview.Escape(oldNick), user.ID, network)
}

func tuiCmdFlagged(parts []string, _ string) {
	var netFilter string
	if len(parts) >= 2 {
		netFilter = parts[1]
	}
	flagged, err := getFlaggedUsers(netFilter)
	if err != nil {
		fmt.Fprintf(logView, "[red]Failed to list flagged users: %s[white]\n", err)
		return
	}
	if len(flagged) == 0 {
		if netFilter == "" {
			fmt.Fprintf(logView, "[dim]No flagged users.[white]\n")
		} else {
			fmt.Fprintf(logView, "[dim]No flagged users on %s.[white]\n", tview.Escape(netFilter))
		}
		return
	}
	header := "Flagged users:"
	if netFilter != "" {
		header = fmt.Sprintf("Flagged users on %s:", netFilter)
	}
	fmt.Fprintf(logView, "[white]%s[white]\n", header)
	for _, u := range flagged {
		account := ""
		if u.IRCAccount != "" {
			account = fmt.Sprintf(" account:%s", tview.Escape(u.IRCAccount))
		}
		fmt.Fprintf(logView, "[yellow]  #%d[white] %s%s reason:%s network:%s created:%s\n",
			u.ID,
			tview.Escape(displayNick(&u)),
			account,
			tview.Escape(u.FlaggedReason),
			tview.Escape(u.Network),
			u.CreatedAt.Format("2006-01-02 15:04:05"))
	}
}

func tuiCmdSessions(parts []string, _ string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /sessions <network> <nick|id> [channel][white]\n")
		return
	}
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
		return
	}
	network, userRef := parts[1], parts[2]
	cm := getCasemapping(network)
	var userID int64
	if id, err := strconv.ParseInt(userRef, 10, 64); err == nil {
		userID = id
	} else {
		user, err := resolveUserByNick(network, userRef, cm)
		if err != nil {
			fmt.Fprintf(logView, "[red]Error: %s[white]\n", err)
			return
		}
		if user == nil {
			fmt.Fprintf(logView, "[yellow]User %s not found on %s[white]\n", tview.Escape(userRef), network)
			return
		}
		userID = user.ID
	}
	var limit int
	readConfig(func() { limit = config.SessionsDisplayLimit })
	var sessions []Session
	var err error
	showChannel := false
	if len(parts) >= 4 {
		channel := normalizeIRC(parts[3], cm)
		sessions, err = getUserDBSessions(network, channel, userID, limit)
	} else {
		sessions, err = getUserDBSessionsByNetwork(network, userID, limit)
		showChannel = true
	}
	if err != nil {
		fmt.Fprintf(logView, "[red]Error querying sessions: %s[white]\n", err)
		return
	}
	if len(sessions) == 0 {
		fmt.Fprintf(logView, "[white]No sessions found for %s on %s[white]\n", tview.Escape(userRef), network)
		return
	}
	var trigger string
	readConfig(func() { trigger = config.Trigger })
	if trigger == "" {
		trigger = "!"
	}
	type sessionLine struct {
		icon    string
		idStr   string
		channel string
		msgStr  string
		timeStr string
		cmd     string
		preview string
	}
	lines := make([]sessionLine, len(sessions))
	maxID := 0
	maxChan := 0
	maxMsg := 0
	maxTime := 0
	for i, s := range sessions {
		icon := "[green]●[white]"
		if s.Status != "active" {
			icon = "[red]○[white]"
		}
		var activeMsgs, archivedMsgs int64
		theDB.Model(&Message{}).Where("session_id = ? AND archived = ?", s.ID, false).Count(&activeMsgs)
		theDB.Model(&Message{}).
			Where("session_id = ? AND archived = ? AND superseded = ?", s.ID, true, false).
			Count(&archivedMsgs)
		idStr := fmt.Sprintf("#%d", s.ID)
		msgStr := fmt.Sprintf("%d msgs", activeMsgs)
		if archivedMsgs > 0 {
			msgStr = fmt.Sprintf("%d msgs (%d archived)", activeMsgs, archivedMsgs)
		}
		timeStr := formatTimeAgo(s.LastActive)
		preview := strings.ReplaceAll(s.FirstMessage, "\n", " ")
		if len(preview) > 80 {
			preview = preview[:77] + "..."
		}
		lines[i] = sessionLine{icon, idStr, tview.Escape(s.Channel), msgStr, timeStr, tview.Escape(s.ChatCommand), tview.Escape(preview)}
		if l := utf8.RuneCountInString(idStr); l > maxID {
			maxID = l
		}
		if showChannel {
			if l := utf8.RuneCountInString(s.Channel); l > maxChan {
				maxChan = l
			}
		}
		if l := utf8.RuneCountInString(msgStr); l > maxMsg {
			maxMsg = l
		}
		if l := utf8.RuneCountInString(timeStr); l > maxTime {
			maxTime = l
		}
	}
	for _, l := range lines {
		var line string
		if showChannel {
			line = fmt.Sprintf("  %s %s  %s  %s  %s  %s%s",
				l.icon,
				l.idStr+strings.Repeat(" ", maxID-utf8.RuneCountInString(l.idStr)),
				l.channel+strings.Repeat(" ", maxChan-utf8.RuneCountInString(l.channel)),
				l.msgStr+strings.Repeat(" ", maxMsg-utf8.RuneCountInString(l.msgStr)),
				l.timeStr+strings.Repeat(" ", maxTime-utf8.RuneCountInString(l.timeStr)),
				tview.Escape(trigger),
				l.cmd,
			)
		} else {
			line = fmt.Sprintf("  %s %s  %s  %s  %s%s",
				l.icon,
				l.idStr+strings.Repeat(" ", maxID-utf8.RuneCountInString(l.idStr)),
				l.msgStr+strings.Repeat(" ", maxMsg-utf8.RuneCountInString(l.msgStr)),
				l.timeStr+strings.Repeat(" ", maxTime-utf8.RuneCountInString(l.timeStr)),
				tview.Escape(trigger),
				l.cmd,
			)
		}
		if l.preview != "" {
			line += " " + l.preview
		}
		fmt.Fprintln(logView, line)
	}
}

func tuiCmdCompact(parts []string, _ string) {
	if len(parts) < 2 {
		fmt.Fprintf(logView, "[yellow]Usage: /compact <session-id>[white]\n")
		return
	}
	sessionID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		fmt.Fprintf(logView, "[red]Invalid session id: %s[white]\n", parts[1])
		return
	}
	session, err := getDBSessionByID(sessionID)
	if err != nil || session == nil {
		fmt.Fprintf(logView, "[red]Session %d not found[white]\n", sessionID)
		return
	}
	bot, ok := getBot(session.Network)
	if !ok {
		fmt.Fprintf(logView, "[red]Unknown network for session: %s[white]\n", session.Network)
		return
	}
	var cfg AIConfig
	var cfgOk bool
	cfg, cfgOk = getSessionConfig(session)
	if !cfgOk {
		fmt.Fprintf(logView, "[red]Chat command %q for session %d no longer exists[white]\n", session.ChatCommand, sessionID)
		return
	}
	userNick := ""
	if session.UserID != nil {
		if u, err := getUserByID(*session.UserID); err == nil && u != nil {
			userNick = displayNick(u)
		}
	}
	fmt.Fprintf(logView, "[white]Compacting session #%d...[white]\n", sessionID)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		res, err := sessionMgr.CompactSession(ctx, CompactSessionInputs{
			SessionID: sessionID,
			Network:   bot.Network,
			Channel:   session.Channel,
			UserNick:  userNick,
			Client:    bot.Client,
			Trigger:   "manual",
		}, cfg)
		tuiApp.QueueUpdateDraw(func() {
			if err != nil {
				fmt.Fprintf(logView, "[red]Compaction failed for session %d: %s[white]\n", sessionID, err)
				return
			}
			vars := compactionNoticeVars(res, sessionID)
			fmt.Fprintf(logView, "[green]Compacted session %d: %s messages, total: %s tokens, cached: %s, %dms[white]\n",
				sessionID, vars["count"], vars["total"], vars["cached"], res.DurationMs)
		})
	}()
}
