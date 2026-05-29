package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lrstanley/girc"
)

type helpEntry struct {
	cmd     string
	info    string
	desc    string
	mcpInfo string
}

func buildHelpText(botnick, trigger string, network Network) string {
	var completions map[string]AIConfig
	var chats map[string]AIConfig
	var tools map[string]MCPCommandConfig
	readConfig(func() {
		completions = make(map[string]AIConfig, len(config.Commands.Completions))
		for k, v := range config.Commands.Completions {
			completions[k] = v
		}
		chats = make(map[string]AIConfig, len(config.Commands.Chats))
		for k, v := range config.Commands.Chats {
			chats[k] = v
		}
		tools = make(map[string]MCPCommandConfig, len(config.Commands.Tools))
		for k, v := range config.Commands.Tools {
			tools[k] = v
		}
	})

	var lines []string

	lines = append(lines, fmt.Sprintf("I'm %s! Use my commands below to chat or generate images.", botnick))
	lines = append(lines, fmt.Sprintf("Only Chat commands start a persistent context. After starting one, reply with my nick (e.g. \"%s, your message here\") to continue that context without using a command.", botnick))
	lines = append(lines, "Commands marked with (regex) use pattern matching, the trigger can match more than one name.")
	if !isNetworkCommandDisabled(network, "stop") {
		lines = append(lines, fmt.Sprintf("  %sstop \u2014 Stop text generation (including this help message)", trigger))
	}
	if !isNetworkCommandDisabled(network, "support") {
		lines = append(lines, fmt.Sprintf("  %ssupport \u2014 Support dave's development", trigger))
	}

	if theDB != nil {
		var histLines []string
		histBuiltins := []struct {
			name string
			line string
		}{
			{"sessions", fmt.Sprintf("  %ssessions [nick|*] \u2014 List sessions (yours, another user's, or all)", trigger)},
			{"history", fmt.Sprintf("  %shistory <id> \u2014 Show messages from a session", trigger)},
			{"resume", fmt.Sprintf("  %sresume <id> \u2014 Resume a previous session", trigger)},
			{"delete", fmt.Sprintf("  %sdelete <id> \u2014 Delete a session", trigger)},
			{"mystats", fmt.Sprintf("  %smystats \u2014 Show your session/message stats", trigger)},
			{"jobs", fmt.Sprintf("  %sjobs \u2014 List your chat queue and background jobs", trigger)},
			{"compact", fmt.Sprintf("  %scompact \u2014 Summarize old messages in your active session to free context", trigger)},
			{"clone", fmt.Sprintf("  %sclone <nick|id> \u2014 Clone another user's session (or your own, to fork it)", trigger)},
		}
		for _, h := range histBuiltins {
			if !isNetworkCommandDisabled(network, h.name) {
				histLines = append(histLines, h.line)
			}
		}
		if len(histLines) > 0 {
			lines = append(lines, "\x02History:\x02")
			lines = append(lines, histLines...)
		}
	}

	filteredCompletions := make(map[string]AIConfig)
	for k, v := range completions {
		if !isNetworkCommandDisabled(network, k) {
			filteredCompletions[k] = v
		}
	}
	if len(filteredCompletions) > 0 {
		lines = append(lines, "\x02Completions:\x02")
		for _, l := range formatTable(sortedAIConfigEntries(trigger, filteredCompletions)) {
			lines = append(lines, "  "+l)
		}
	}

	filteredChats := make(map[string]AIConfig)
	for k, v := range chats {
		if !isNetworkCommandDisabled(network, k) {
			filteredChats[k] = v
		}
	}
	if len(filteredChats) > 0 {
		lines = append(lines, "\x02Chats:\x02")
		for _, l := range formatTable(sortedAIConfigEntries(trigger, filteredChats)) {
			lines = append(lines, "  "+l)
		}
	}

	filteredTools := make(map[string]MCPCommandConfig)
	for k, v := range tools {
		if !isNetworkCommandDisabled(network, k) {
			filteredTools[k] = v
		}
	}
	if len(filteredTools) > 0 {
		var entries []helpEntry
		toolKeys := make([]string, 0, len(filteredTools))
		for k := range filteredTools {
			toolKeys = append(toolKeys, k)
		}
		sort.Slice(toolKeys, func(i, j int) bool {
			return toolKeys[i] < toolKeys[j]
		})
		for _, k := range toolKeys {
			c := filteredTools[k]
			entries = append(entries, helpEntry{
				cmd:  formatCmd(trigger, c.Regex, c.Name),
				info: formatToolInfo(c.MCP, c.Tool),
				desc: formatDesc(c.Description, false),
			})
		}
		lines = append(lines, "\x02Tool commands:\x02")
		for _, l := range formatTable(entries) {
			lines = append(lines, "  "+l)
		}
	}

	mcpLines := getAllMCPServerInfo()
	if len(mcpLines) > 0 {
		lines = append(lines, "\x02MCP Servers:\x02")
		for _, l := range mcpLines {
			lines = append(lines, l)
		}
	}

	return strings.Join(lines, "\n")
}

type pastebinEntry struct {
	cmd          string
	regex        bool
	service      string
	model        string
	detectImages bool
	desc         string
}

func escapeMDPipe(s string) string {
	return strings.ReplaceAll(s, "|", `\|`)
}

func escapeDescPipe(s string) string {
	return strings.ReplaceAll(s, "|", "&#124;")
}

func pastebinCmd(cmd string) string {
	return "`" + escapeMDPipe(cmd) + "`"
}

func buildPastebinHelpText(botnick, trigger string, network Network) string {
	var completions map[string]AIConfig
	var chats map[string]AIConfig
	var tools map[string]MCPCommandConfig
	readConfig(func() {
		completions = make(map[string]AIConfig, len(config.Commands.Completions))
		for k, v := range config.Commands.Completions {
			completions[k] = v
		}
		chats = make(map[string]AIConfig, len(config.Commands.Chats))
		for k, v := range config.Commands.Chats {
			chats[k] = v
		}
		tools = make(map[string]MCPCommandConfig, len(config.Commands.Tools))
		for k, v := range config.Commands.Tools {
			tools[k] = v
		}
	})

	var b strings.Builder

	fmt.Fprintf(&b, "# %s Help\n\n", botnick)
	fmt.Fprintf(&b, "I'm %s! Use my commands below to chat or generate images.\n", botnick)
	fmt.Fprintf(&b, "Only **Chat commands** start a persistent context. After starting one, reply to my nick (e.g. `%s, your message here`) to continue that context without using a command.\n\n", botnick)
	b.WriteString("---\n\n")

	filteredChats := make(map[string]AIConfig)
	for k, v := range chats {
		if !isNetworkCommandDisabled(network, k) {
			filteredChats[k] = v
		}
	}
	if len(filteredChats) > 0 {
		entries := sortedPastebinEntries(trigger, filteredChats)
		writeGFMCmdTable(&b, "## Chat Commands", entries)
		b.WriteString("\n")
	}

	fmt.Fprintf(&b, "## Example Session\n\n")
	fmt.Fprintf(&b, "```\n")
	fmt.Fprintf(&b, "<alice> %schat hey %s, what's a good recipe for pasta?\n", trigger, botnick)
	fmt.Fprintf(&b, "<%s> Hey Alice! Here's a classic — Cacio e Pepe. You'll need...\n", botnick)
	fmt.Fprintf(&b, "<alice> %s, can you make it vegetarian?\n", botnick)
	fmt.Fprintf(&b, "<%s> Sure! Swap the pecorino for a good vegetarian alternative...\n", botnick)
	fmt.Fprintf(&b, "```\n\n")

	filteredCompletions := make(map[string]AIConfig)
	for k, v := range completions {
		if !isNetworkCommandDisabled(network, k) {
			filteredCompletions[k] = v
		}
	}
	if len(filteredCompletions) > 0 {
		entries := sortedPastebinEntries(trigger, filteredCompletions)
		writeGFMCmdTable(&b, "## Completions", entries)
		b.WriteString("\n")
	}

	filteredTools := make(map[string]MCPCommandConfig)
	for k, v := range tools {
		if !isNetworkCommandDisabled(network, k) {
			filteredTools[k] = v
		}
	}
	if len(filteredTools) > 0 {
		var entries []pastebinEntry
		toolKeys := make([]string, 0, len(filteredTools))
		for k := range filteredTools {
			toolKeys = append(toolKeys, k)
		}
		sort.Slice(toolKeys, func(i, j int) bool {
			return toolKeys[i] < toolKeys[j]
		})
		for _, k := range toolKeys {
			c := filteredTools[k]
			entries = append(entries, pastebinEntry{
				cmd:     escapeMDPipe(trigger + c.Regex),
				regex:   c.Regex != c.Name,
				service: c.MCP,
				model:   c.Tool,
				desc:    formatDesc(c.Description, false),
			})
		}
		writeGFMCmdTable(&b, "## Tool Commands", entries)
		b.WriteString("\n")
	}

	if theDB != nil {
		var rows []string
		histBuiltins := []struct {
			name string
			line string
		}{
			{"sessions", fmt.Sprintf("%s | %s", pastebinCmd(trigger+"sessions [nick|*]"), escapeDescPipe("List sessions (yours, another user's, or all)"))},
			{"history", fmt.Sprintf("%s | %s", pastebinCmd(trigger+"history <id>"), escapeDescPipe("Show messages from a session"))},
			{"resume", fmt.Sprintf("%s | %s", pastebinCmd(trigger+"resume <id>"), escapeDescPipe("Resume a previous session"))},
			{"delete", fmt.Sprintf("%s | %s", pastebinCmd(trigger+"delete <id>"), escapeDescPipe("Delete a session"))},
			{"mystats", fmt.Sprintf("%s | %s", pastebinCmd(trigger+"mystats"), escapeDescPipe("Show your session/message stats"))},
			{"jobs", fmt.Sprintf("%s | %s", pastebinCmd(trigger+"jobs"), escapeDescPipe("List your chat queue and background jobs"))},
			{"compact", fmt.Sprintf("%s | %s", pastebinCmd(trigger+"compact"), escapeDescPipe("Summarize old messages to free context"))},
			{"clone", fmt.Sprintf("%s | %s", pastebinCmd(trigger+"clone <nick|id>"), escapeDescPipe("Clone another user's session"))},
		}
		for _, h := range histBuiltins {
			if !isNetworkCommandDisabled(network, h.name) {
				rows = append(rows, h.line)
			}
		}
		if len(rows) > 0 {
			b.WriteString("## History & Sessions\n\n")
			b.WriteString("| Command | Description |\n")
			b.WriteString("|---------|-------------|\n")
			for _, r := range rows {
				fmt.Fprintf(&b, "| %s |\n", r)
			}
			b.WriteString("\n")
		}
	}

	var builtinRows []string
	builtinItems := []struct {
		name string
		desc string
	}{
		{"stop", "Stop text generation"},
		{"support", "Support dave's development"},
	}
	for _, bi := range builtinItems {
		if !isNetworkCommandDisabled(network, bi.name) {
			builtinRows = append(builtinRows, fmt.Sprintf("%s | %s", pastebinCmd(trigger+bi.name), escapeDescPipe(bi.desc)))
		}
	}
	if len(builtinRows) > 0 {
		b.WriteString("## Built-in Commands\n\n")
		b.WriteString("| Command | Description |\n")
		b.WriteString("|---------|-------------|\n")
		for _, r := range builtinRows {
			fmt.Fprintf(&b, "| %s |\n", r)
		}
		b.WriteString("\n")
	}

	mcpServers := getAllMCPServerInfo()
	if len(mcpServers) > 0 {
		b.WriteString("## MCP Servers\n\n")
		for _, l := range mcpServers {
			stripped := strings.TrimSpace(strings.ReplaceAll(l, "\x02", "**"))
			fmt.Fprintf(&b, "- %s\n", stripped)
		}
		b.WriteString("\n")
	}

	return b.String()
}

func sortedPastebinEntries(trigger string, m map[string]AIConfig) []pastebinEntry {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return formatModelInfo(m[keys[i]].Service, m[keys[i]].Model, m[keys[i]].DetectImages) <
			formatModelInfo(m[keys[j]].Service, m[keys[j]].Model, m[keys[j]].DetectImages)
	})
	entries := make([]pastebinEntry, 0, len(m))
	for _, k := range keys {
		c := m[k]
		entries = append(entries, pastebinEntry{
			cmd:          escapeMDPipe(trigger + c.Regex),
			regex:        c.Regex != c.Name,
			service:      c.Service,
			model:        c.Model,
			detectImages: c.DetectImages,
			desc:         formatDesc(c.Description, false),
		})
	}
	return entries
}

func writeGFMCmdTable(b *strings.Builder, header string, entries []pastebinEntry) {
	b.WriteString(header)
	b.WriteString("\n\n")
	b.WriteString("| Command | Regex | Service | Model | Media | Description |\n")
	b.WriteString("|---------|-------|---------|-------|-------|-------------|\n")
	for _, e := range entries {
		cmd := "`" + e.cmd + "`"
		regexCol := ""
		if e.regex {
			regexCol = "✱"
		}
		media := ""
		if e.detectImages {
			media = "🖼️"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", cmd, regexCol, e.service, e.model, media, escapeDescPipe(e.desc))
	}
}

func help(network Network, client *girc.Client, event girc.Event, ctx context.Context, output chan<- string, args ...string) {
	botnick := client.GetNick()

	if len(args) > 0 && args[0] != "" {
		cmdName := args[0]
		entry, found := findCommandHelp(network, cmdName)
		if !found {
			select {
			case output <- fmt.Sprintf("\x0304❗ Command '%s' not found. Use %shelp to see all commands.", cmdName, network.Trigger):
			case <-ctx.Done():
			}
			return
		}
		var lines []string
		lines = append(lines, fmt.Sprintf("Help for %s:", entry.cmd))
		if entry.info != "" {
			lines = append(lines, "  "+entry.info)
		}
		lines = append(lines, "  "+entry.desc)
		if entry.mcpInfo != "" {
			lines = append(lines, "  "+entry.mcpInfo)
		}
		for _, line := range lines {
			select {
			case output <- line:
			case <-ctx.Done():
				return
			}
		}
		return
	}

	rawText := buildHelpText(botnick, network.Trigger, network)

	var pastebinAvailable bool
	readConfig(func() { pastebinAvailable = config.Pastebin.URL != "" })

	if pastebinAvailable {
		mdText := buildPastebinHelpText(botnick, network.Trigger, network)
		url, err := uploadToPastebin(mdText, "Dave's Help")
		n := getNotices()
		if err != nil {
			select {
			case output <- errorNotice(n.DB.PastebinUpload, map[string]string{"error": err.Error()}):
			case <-ctx.Done():
				return
			}
			wrappedLines := wrapForIRC(rawText)
			chCfg := network.GetChannelConfig(event.Params[0])
			preview := chCfg.GetMaxLines()
			if preview > len(wrappedLines) {
				preview = len(wrappedLines)
			}
			for i := 0; i < preview; i++ {
				select {
				case output <- wrappedLines[i]:
				case <-ctx.Done():
					return
				}
			}
			select {
			case output <- n.Pastebin.Failed:
			case <-ctx.Done():
				return
			}
			return
		}
		select {
		case output <- expandNotice(n.Pastebin.Link, map[string]string{"url": url}):
		case <-ctx.Done():
			return
		}
		return
	}

	for _, line := range strings.Split(rawText, "\n") {
		for _, wrapped := range wrapLine(line) {
			select {
			case output <- wrapped:
			case <-ctx.Done():
				return
			}
		}
	}
}

func formatModelInfo(service, model string, detectImages bool) string {
	icon := ""
	if detectImages {
		icon = "🖼️"
	}
	if model == "" {
		return fmt.Sprintf("[%s/]%s", service, icon)
	}
	return fmt.Sprintf("[%s/%s]%s", service, model, icon)
}

func matchesCommand(cmdName, name, regex string) bool {
	if name == cmdName || regex == cmdName {
		return true
	}
	if regex != name {
		re := regexp.MustCompile("^" + regex + "$")
		return re.MatchString(cmdName)
	}
	return false
}

func buildAIConfigEntry(trigger string, c AIConfig) helpEntry {
	return helpEntry{
		cmd:     formatCmd(trigger, c.Regex, c.Name),
		info:    formatModelInfo(c.Service, c.Model, c.DetectImages),
		desc:    formatDesc(c.Description, false),
		mcpInfo: getMCPServerNames(c.MCPs),
	}
}

var builtinHelpEntries = map[string]struct {
	cmdSuffix string
	desc      string
}{
	"stop":     {"stop", "Stop text generation (including this help message)"},
	"sessions": {"sessions [nick|*]", "List sessions. No args = yours, <nick> = another user's, * = all in channel"},
	"history":  {"history <session-id>", "Show messages from a session (first/last 2 with ... in between, tool calls hidden)"},
	"resume":   {"resume <session-id>", "Resume a previous session, pausing any current active session"},
	"delete":   {"delete <session-id>", "Delete a session and its messages"},
	"mystats":  {"mystats", "Show your total sessions and messages on this network/channel"},
	"jobs":     {"jobs", "List your chat queue status and pending/running/completed background jobs"},
	"compact":  {"compact", "Summarize the first 2/3 of your active session into a single message to free context tokens"},
	"clone":    {"clone <nick|id>", "Clone another user's session (or your own, to fork it). Creates a new session with a copy of the source's message history"},
	"support":  {"support", "Support dave's development"},
}

func findCommandHelp(network Network, cmdName string) (helpEntry, bool) {
	var completions map[string]AIConfig
	var chats map[string]AIConfig
	var tools map[string]MCPCommandConfig
	readConfig(func() {
		completions = config.Commands.Completions
		chats = config.Commands.Chats
		tools = config.Commands.Tools
	})
	for _, c := range completions {
		if matchesCommand(cmdName, c.Name, c.Regex) {
			if isNetworkCommandDisabled(network, c.Name) {
				return helpEntry{}, false
			}
			return buildAIConfigEntry(network.Trigger, c), true
		}
	}
	for _, c := range chats {
		if matchesCommand(cmdName, c.Name, c.Regex) {
			if isNetworkCommandDisabled(network, c.Name) {
				return helpEntry{}, false
			}
			return buildAIConfigEntry(network.Trigger, c), true
		}
	}
	for _, c := range tools {
		if matchesCommand(cmdName, c.Name, c.Regex) {
			if isNetworkCommandDisabled(network, c.Name) {
				return helpEntry{}, false
			}
			return helpEntry{
				cmd:  formatCmd(network.Trigger, c.Regex, c.Name),
				info: formatToolInfo(c.MCP, c.Tool),
				desc: formatDesc(c.Description, false),
			}, true
		}
	}
	if tmpl, ok := builtinHelpEntries[cmdName]; ok {
		if isNetworkCommandDisabled(network, cmdName) {
			return helpEntry{}, false
		}
		return helpEntry{
			cmd:  network.Trigger + tmpl.cmdSuffix,
			desc: tmpl.desc,
		}, true
	}
	return helpEntry{}, false
}

func sortedAIConfigEntries(trigger string, m map[string]AIConfig) []helpEntry {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		iInfo := formatModelInfo(m[keys[i]].Service, m[keys[i]].Model, m[keys[i]].DetectImages)
		jInfo := formatModelInfo(m[keys[j]].Service, m[keys[j]].Model, m[keys[j]].DetectImages)
		return iInfo < jInfo
	})
	entries := make([]helpEntry, 0, len(m))
	for _, k := range keys {
		c := m[k]
		entries = append(entries, helpEntry{
			cmd:  formatCmd(trigger, c.Regex, c.Name),
			info: formatModelInfo(c.Service, c.Model, c.DetectImages),
			desc: formatDesc(c.Description, false),
		})
	}
	return entries
}

func formatCmd(trigger, regex, name string) string {
	cmd := trigger + regex
	if regex != name {
		cmd += " (regex)"
	}
	return cmd
}

func formatDesc(desc string, detectImages bool) string {
	if desc == "" {
		desc = "no description"
	}
	if detectImages {
		desc += " [handles images]"
	}
	return desc
}

func formatToolInfo(mcpServer, tool string) string {
	return fmt.Sprintf("[%s/%s]", mcpServer, tool)
}

func formatTable(entries []helpEntry) []string {
	if len(entries) == 0 {
		return nil
	}

	maxCmd := 0
	maxInfo := 0
	for _, e := range entries {
		cmdLen := utf8.RuneCountInString(e.cmd)
		infoLen := utf8.RuneCountInString(e.info)
		if cmdLen > maxCmd {
			maxCmd = cmdLen
		}
		if infoLen > maxInfo {
			maxInfo = infoLen
		}
	}

	var lines []string
	for _, e := range entries {
		cmdLen := utf8.RuneCountInString(e.cmd)
		infoLen := utf8.RuneCountInString(e.info)
		line := e.cmd + strings.Repeat(" ", maxCmd-cmdLen+2)
		if e.info != "" {
			line += e.info + strings.Repeat(" ", maxInfo-infoLen+2)
		} else if maxInfo > 0 {
			line += strings.Repeat(" ", maxInfo+2)
		}
		line += e.desc
		lines = append(lines, line)
	}
	return lines
}

func wrapLine(line string) []string {
	const maxLen = 400
	if len(line) <= maxLen {
		return []string{line}
	}
	var parts []string
	words := strings.Fields(line)
	var current string
	for _, w := range words {
		if len(current) == 0 {
			current = w
		} else if len(current)+1+len(w) <= maxLen {
			current += " " + w
		} else {
			parts = append(parts, current)
			current = w
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}
