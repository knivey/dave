package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/rivo/tview"
)

var botIsInChannel = func(bot *Bot, channel string) bool {
	return bot.Client != nil && bot.Client.LookupChannel(channel) != nil
}

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
	"/tokencount":  tuiCmdTokenCount,
	"/reinject":    tuiCmdReinject,
	"/systemmsg":   tuiCmdSystemMsg,
}

func formatClonedFromTUI(s Session, sourceNicks map[int64]string) string {
	if s.ClonedFromID == nil {
		return ""
	}
	id := *s.ClonedFromID
	if nick, ok := sourceNicks[id]; ok {
		return fmt.Sprintf(" [yellow][cloned from #%d by %s][white]", id, tview.Escape(nick))
	}
	if s.ClonedFromNick != "" {
		return fmt.Sprintf(" [yellow][cloned from #%d by %s][white]", id, tview.Escape(s.ClonedFromNick))
	}
	return fmt.Sprintf(" [yellow][cloned from #%d (deleted session)][white]", id)
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
	fmt.Fprintf(logView, "  /tokencount <session-id>     - Our tokenizer count vs provider-reported usage\n")
	fmt.Fprintf(logView, "  /reinject <session-id>       - Re-render and inject system prompt into session\n")
	fmt.Fprintf(logView, "  /systemmsg <session-id> <text> - Inject custom system message (Go template) into session\n")
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
		reloadAll()
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
	// Send the IRC JOIN regardless of cached/config state — girc's joined-state
	// can lag or be wrong, and joining while already joined is harmless. Use the
	// configured key (if any) so +k channels work.
	bot.mu.Lock()
	key := bot.Network.GetChannelConfig(channel).Key
	if bot.Network.Channels == nil {
		bot.Network.Channels = make(map[string]ChannelConfig)
	}
	// Create a config entry only if one doesn't exist, so we never clobber a
	// configured key or other channel settings.
	if _, exists := bot.Network.Channels[channel]; !exists {
		bot.Network.Channels[channel] = ChannelConfig{}
	}
	bot.mu.Unlock()

	if key != "" {
		bot.Client.Cmd.JoinKey(channel, key)
	} else {
		bot.Client.Cmd.Join(channel)
	}
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
		icon       string
		idStr      string
		channel    string
		msgStr     string
		timeStr    string
		cmd        string
		preview    string
		clonedFrom string
	}
	sourceNicks := resolveClonedFromNicks(sessions)
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
		var clonedSuffix string
		if s.ClonedFromID != nil {
			clonedSuffix = formatClonedFromTUI(s, sourceNicks)
		}
		lines[i] = sessionLine{icon, idStr, tview.Escape(s.Channel), msgStr, timeStr, tview.Escape(s.ChatCommand), tview.Escape(preview), clonedSuffix}
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
		if l.clonedFrom != "" {
			line += l.clonedFrom
		}
		fmt.Fprintln(logView, line)
	}
}

func tuiCmdCompact(parts []string, _ string) {
	// Gate on [compaction] enabled for consistency with the IRC `^compact$`
	// handler (historyCompact) and the config docs — enabled covers the
	// manual IRC command + TUI /compact alike.
	var compactionEnabled bool
	readConfig(func() { compactionEnabled = config.Compaction.Enabled })
	if !compactionEnabled {
		fmt.Fprintf(logView, "[yellow]Compaction is disabled in config.[white]\n")
		return
	}
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
				if errors.Is(err, ErrCompactionSessionChanged) {
					fmt.Fprintf(logView, "[yellow]Session %d changed during compaction; try again.[white]\n", sessionID)
					return
				}
				if errors.Is(err, ErrCompactionNothingNew) {
					fmt.Fprintf(logView, "[yellow]Session %d has no new messages to compact since the last compaction.[white]\n", sessionID)
					return
				}
				if errors.Is(err, ErrCompactionNoSystemRow) {
					fmt.Fprintf(logView, "[red]Compaction failed for session %d: live history does not start with a system message[white]\n", sessionID)
					return
				}
				fmt.Fprintf(logView, "[red]Compaction failed for session %d: %s[white]\n", sessionID, err)
				return
			}
			vars := compactionNoticeVars(res, sessionID)
			// Uses the compaction's OWN numbers (summary size + post-compaction
			// live history), not the last chat turn's usage — see the
			// provenance note on compactionNoticeVars.
			fmt.Fprintf(logView, "[green]Compacted session %d: %s messages, summary: %s tok, live history ~%s tok / %s msgs, %dms[white]\n",
				sessionID, vars["count"], vars["summary_tokens"], vars["live_tokens_est"], vars["live_messages"], res.DurationMs)
		})
	}()
}

// tuiCmdTokenCount is the owner's on-demand provider-accounting
// investigation tool (/tokencount <session-id>): it computes dave's OWN
// neutral token count of exactly the payload the next turn would send
// — MESSAGES PLUS TOOL DEFINITIONS, both of which the request
// serializes and OpenAI-style providers fold into prompt_tokens — using
// the real tokenizer via tokencount.go and the same MaxHistory
// truncation GetMessages applies, then lays it next to what the
// provider last REPORTED for that session's most recent API call
// (prompt/completion/cached from turn_usage).
//
// COMPARAND LOGIC: the primary ratio is the RAW provider prompt over
// our messages+tools total — the raw prompt is the full wire payload
// and therefore the counting comparand. When caching is active the
// adjusted figure (prompt minus cached) is also printed, labeled the
// billing view: it strips the cached prefix, which contains the tool
// definitions and shared history, so it is NOT comparable against our
// full request count. The one known legitimate residual is REASONING
// REPLAY on Responses API chains (session.ResponseID set): prior
// turns' reasoning items are re-sent as input and billed in
// prompt_tokens. That is real input the model consumes — it is NOT
// subtracted from the provider side; instead it is surfaced as a
// separate labeled estimate (prior turns' reasoning from turn_usage)
// alongside a second raw-prompt ratio, and flagged as an UPPER BOUND
// because the server may evict replayed reasoning. A provider whose
// raw prompt still exceeds our count plus that bound is accounting
// differently (the xAI/Grok ~2x prompt_tokens observation), not
// sending a bigger payload — this command is how that gets diagnosed
// per-session without touching IRC.
//
// Purely local: DB reads + tokenizer + in-memory MCP tool map, no API
// call, no bot connection needed, so unlike /compact it runs
// synchronously.
func tuiCmdTokenCount(parts []string, _ string) {
	if len(parts) < 2 {
		fmt.Fprintf(logView, "[yellow]Usage: /tokencount <session-id>[white]\n")
		return
	}
	if theDB == nil {
		fmt.Fprint(logView, tuiDBNotAvailable)
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
	cfg, cfgOk := getSessionConfig(session)
	if !cfgOk {
		fmt.Fprintf(logView, "[red]Chat command %q for session %d no longer exists[white]\n", session.ChatCommand, sessionID)
		return
	}
	// The same truncation a real turn would send (GetMessages applies
	// TruncateHistory with the live config's MaxHistory).
	msgs, err := sessionMgr.GetMessages(session.ID, cfg.MaxHistory)
	if err != nil {
		fmt.Fprintf(logView, "[red]Failed to load messages for session %d: %s[white]\n", sessionID, err)
		return
	}

	// The same tool definitions a real turn would serialize (live MCP
	// tool map via toolDefsForConfig — getTools' exact assembly, so the
	// count can never drift from the sent set).
	tools := toolDefsForConfig(cfg)
	ours := countRequestTokens(cfg.Model, msgs, tools)

	fmt.Fprintf(logView, "[white]Session #%d %s/%s model:%s service:%s[white]\n",
		session.ID, tview.Escape(session.Network), tview.Escape(session.Channel),
		tview.Escape(cfg.Model), tview.Escape(cfg.Service))
	exactNote := "approximate — NOT this model's real tokenizer"
	if ours.Exact {
		exactNote = "exact"
	}
	msgTokens := ours.Tokens - ours.ToolTokens
	fmt.Fprintf(logView, "[white]Our count: %d tokens across %d messages (encoding %s, %s; %d image part(s))[white]\n",
		msgTokens, len(msgs), ours.Encoding, exactNote, ours.ImageParts)
	if ours.ImageParts > 0 {
		fmt.Fprintf(logView, "[white]  images counted at a flat ~%d tokens each (low-detail lower bound — real cost scales with resolution)[white]\n",
			imageTokenEstimate)
	}
	fmt.Fprintf(logView, "[white]  tools: %d definitions, ~%d tokens[white]\n", ours.Tools, ours.ToolTokens)
	fmt.Fprintf(logView, "[white]  our request total (messages+tools): %d[white]\n", ours.Tokens)

	lastUsage, err := getLastTurnUsageForSession(session.ID)
	if err != nil || lastUsage == nil {
		fmt.Fprintf(logView, "[yellow]No provider usage recorded for this session yet.[white]\n")
		return
	}
	// Reasoning replay surface: only PREVIOUS turns' reasoning can be
	// replayed into the next request's prompt, hence the prior-turns
	// split from sumSessionReasoningTokens. Omitted cleanly when there
	// are no usage rows with reasoning (nothing to surface). The error
	// is deliberately swallowed: the same table was just read via
	// getLastTurnUsageForSession, so a failure here is a transient we
	// degrade on (section omitted) rather than abort the command for.
	reasoningAll, reasoningPrior, _ := sumSessionReasoningTokens(session.ID)
	if reasoningAll > 0 {
		fmt.Fprintf(logView, "[white]Reasoning tokens (turn_usage): all turns %d, prior turns %d (replay-eligible upper bound)[white]\n",
			reasoningAll, reasoningPrior)
		if session.ResponseID != nil {
			fmt.Fprintf(logView, "[white]  Responses chain active — provider prompt legitimately includes replayed prior reasoning (~%d tok upper bound; server may evict)[white]\n",
				reasoningPrior)
		} else {
			fmt.Fprint(logView, "[white]  (no Responses chain — reasoning is not replayed; prior reasoning should NOT appear in prompt_tokens)[white]\n")
		}
	}
	adjusted := lastUsage.PromptTokens - lastUsage.CachedTokens
	fmt.Fprintf(logView, "[white]Provider last turn: prompt %d (cached %d, adjusted %d), completion %d[white]\n",
		lastUsage.PromptTokens, lastUsage.CachedTokens, adjusted, lastUsage.CompletionTokens)
	if ours.Tokens > 0 {
		// When cache is active the adjusted figure strips the cached
		// prefix — which contains the tool definitions and shared
		// history — so it is NOT comparable against our full request
		// count. The raw-prompt ratio is the counting-comparand; the
		// adjusted ratio is the billing story. Identical when cached
		// is zero, so we only print both when they differ.
		fmt.Fprintf(logView, "[white]Ratio provider_prompt/our_total = %.2f[white]\n",
			float64(lastUsage.PromptTokens)/float64(ours.Tokens))
		if lastUsage.CachedTokens > 0 {
			fmt.Fprintf(logView, "[white]  Ratio provider_adjusted/our_total = %.2f (billing view — cached prefix stripped)[white]\n",
				float64(adjusted)/float64(ours.Tokens))
		}
		if session.ResponseID != nil && reasoningAll > 0 {
			// Second ratio accounting for replay-eligible reasoning —
			// the comparable figure while a Responses chain is active.
			fmt.Fprintf(logView, "[white]  Ratio provider_prompt/(our_total+prior_reasoning) = %.2f[white]\n",
				float64(lastUsage.PromptTokens)/float64(ours.Tokens+int(reasoningPrior)))
		}
	}
}

func tuiCmdReinject(parts []string, _ string) {
	if len(parts) < 2 {
		fmt.Fprintf(logView, "[yellow]Usage: /reinject <session-id>[white]\n")
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
		fmt.Fprintf(logView, "[red]No bot connected for network %s[white]\n", session.Network)
		return
	}

	if !botIsInChannel(bot, session.Channel) {
		fmt.Fprintf(logView, "[red]Bot is not joined to %s on %s[white]\n", session.Channel, session.Network)
		return
	}

	if session.UserID == nil {
		fmt.Fprintf(logView, "[red]Session %d has no associated user[white]\n", sessionID)
		return
	}
	u, err := getUserByID(*session.UserID)
	if err != nil || u == nil {
		fmt.Fprintf(logView, "[red]User for session %d not found[white]\n", sessionID)
		return
	}
	userNick := displayNick(u)

	cfg, cfgOk := getSessionConfig(session)
	if !cfgOk {
		fmt.Fprintf(logView, "[red]Chat command %q for session %d no longer exists[white]\n", session.ChatCommand, sessionID)
		return
	}

	rendered := renderFreshSystemPrompt(cfg, bot.Network, bot.Client, session.Channel, userNick, "")
	if rendered == "" {
		fmt.Fprintf(logView, "[red]No system prompt configured for session %d[white]\n", sessionID)
		return
	}

	if session.Status == "completed" {
		fmt.Fprintf(logView, "[yellow]Warning: session %d is completed[white]\n", sessionID)
	}

	if err := sessionMgr.AddMessage(sessionID, ChatMessage{Role: RoleSystem, Content: rendered}); err != nil {
		fmt.Fprintf(logView, "[red]Failed to inject message: %s[white]\n", err)
		return
	}

	if err := sessionMgr.UpdateResponseID(sessionID, nil, ""); err != nil {
		fmt.Fprintf(logView, "[red]Failed to clear response_id: %s[white]\n", err)
		return
	}

	fmt.Fprintf(logView, "[green]Injected system prompt into session %d (%d chars)[white]\n", sessionID, len(rendered))
}

func tuiCmdSystemMsg(parts []string, text string) {
	if len(parts) < 3 {
		fmt.Fprintf(logView, "[yellow]Usage: /systemmsg <session-id> <template-text>[white]\n")
		return
	}
	sessionID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		fmt.Fprintf(logView, "[red]Invalid session id: %s[white]\n", parts[1])
		return
	}

	tmplText := strings.TrimSpace(strings.TrimPrefix(text, parts[0]+" "+parts[1]+" "))
	if tmplText == "" {
		fmt.Fprintf(logView, "[red]Empty template text[white]\n")
		return
	}

	session, err := getDBSessionByID(sessionID)
	if err != nil || session == nil {
		fmt.Fprintf(logView, "[red]Session %d not found[white]\n", sessionID)
		return
	}

	bot, ok := getBot(session.Network)
	if !ok {
		fmt.Fprintf(logView, "[red]No bot connected for network %s[white]\n", session.Network)
		return
	}

	if !botIsInChannel(bot, session.Channel) {
		fmt.Fprintf(logView, "[red]Bot is not joined to %s on %s[white]\n", session.Channel, session.Network)
		return
	}

	if session.UserID == nil {
		fmt.Fprintf(logView, "[red]Session %d has no associated user[white]\n", sessionID)
		return
	}
	u, err := getUserByID(*session.UserID)
	if err != nil || u == nil {
		fmt.Fprintf(logView, "[red]User for session %d not found[white]\n", sessionID)
		return
	}
	userNick := displayNick(u)

	tmpl, err := template.New("systemmsg").Parse(tmplText)
	if err != nil {
		fmt.Fprintf(logView, "[red]Template parse error: %s[white]\n", err)
		return
	}

	data := buildSystemPromptData(bot.Network, bot.Client, session.Channel, userNick)
	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		fmt.Fprintf(logView, "[red]Template execute error: %s[white]\n", err)
		return
	}
	rendered := buf.String()

	if session.Status == "completed" {
		fmt.Fprintf(logView, "[yellow]Warning: session %d is completed[white]\n", sessionID)
	}

	if err := sessionMgr.AddMessage(sessionID, ChatMessage{Role: RoleSystem, Content: rendered}); err != nil {
		fmt.Fprintf(logView, "[red]Failed to inject message: %s[white]\n", err)
		return
	}

	if err := sessionMgr.UpdateResponseID(sessionID, nil, ""); err != nil {
		fmt.Fprintf(logView, "[red]Failed to clear response_id: %s[white]\n", err)
		return
	}

	fmt.Fprintf(logView, "[green]Injected system message into session %d (%d chars)[white]\n", sessionID, len(rendered))
}
