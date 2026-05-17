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

func help(network Network, client *girc.Client, event girc.Event, ctx context.Context, output chan<- string, args ...string) {
	botnick := client.GetNick()

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

	lines = append(lines, fmt.Sprintf("I'm %s! Use my commands below to chat or generate images.", botnick))
	lines = append(lines, fmt.Sprintf("Only Chat commands start a persistent context. After starting one, reply with my nick (e.g. \"%s, your message here\") to continue that context without using a command.", botnick))
	lines = append(lines, "Commands marked with (regex) use pattern matching, the trigger can match more than one name.")
	lines = append(lines, fmt.Sprintf("  %sstop \u2014 Stop text generation (including this help message)", network.Trigger))
	lines = append(lines, fmt.Sprintf("  %ssupport \u2014 Support dave's development", network.Trigger))

	if theDB != nil {
		lines = append(lines, "\x02History:\x02")
		lines = append(lines, fmt.Sprintf("  %ssessions [nick|*] \u2014 List sessions (yours, another user's, or all)", network.Trigger))
		lines = append(lines, fmt.Sprintf("  %shistory <id> \u2014 Show messages from a session", network.Trigger))
		lines = append(lines, fmt.Sprintf("  %sresume <id> \u2014 Resume a previous session", network.Trigger))
		lines = append(lines, fmt.Sprintf("  %sdelete <id> \u2014 Delete a session", network.Trigger))
		lines = append(lines, fmt.Sprintf("  %smystats \u2014 Show your session/message stats", network.Trigger))
		lines = append(lines, fmt.Sprintf("  %sjobs \u2014 List your chat queue and background jobs", network.Trigger))
		lines = append(lines, fmt.Sprintf("  %scompact \u2014 Summarize old messages in your active session to free context", network.Trigger))
		lines = append(lines, fmt.Sprintf("  %sclone <nick|id> \u2014 Clone another user's session (or your own, to fork it)", network.Trigger))
	}

	if len(completions) > 0 {
		lines = append(lines, "\x02Completions:\x02")
		for _, l := range formatTable(sortedAIConfigEntries(network.Trigger, completions)) {
			lines = append(lines, "  "+l)
		}
	}

	if len(chats) > 0 {
		lines = append(lines, "\x02Chats:\x02")
		for _, l := range formatTable(sortedAIConfigEntries(network.Trigger, chats)) {
			lines = append(lines, "  "+l)
		}
	}

	if len(tools) > 0 {
		var entries []helpEntry
		toolKeys := make([]string, 0, len(tools))
		for k := range tools {
			toolKeys = append(toolKeys, k)
		}
		sort.Slice(toolKeys, func(i, j int) bool {
			return toolKeys[i] < toolKeys[j]
		})
		for _, k := range toolKeys {
			c := tools[k]
			entries = append(entries, helpEntry{
				cmd:  formatCmd(network.Trigger, c.Regex, c.Name),
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

	chCfg := network.GetChannelConfig(event.Params[0])
	if chCfg.Pastebin {
		rawText := strings.Join(lines, "\n")
		wrappedLines := wrapForIRC(rawText)
		if len(wrappedLines) >= chCfg.GetMaxLines() {
			url, err := uploadToPastebin("```\n"+rawText+"\n```", "Dave's Help")
			n := getNotices()
			if err != nil {
				select {
				case output <- errorNotice(n.DB.PastebinUpload, map[string]string{"error": err.Error()}):
				case <-ctx.Done():
					return
				}
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
			preview := 3
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
			case output <- expandNotice(n.Pastebin.Link, map[string]string{"url": url}):
			case <-ctx.Done():
				return
			}
			return
		}
	}

	for _, line := range lines {
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
		icon = "🖻"
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
			return buildAIConfigEntry(network.Trigger, c), true
		}
	}
	for _, c := range chats {
		if matchesCommand(cmdName, c.Name, c.Regex) {
			return buildAIConfigEntry(network.Trigger, c), true
		}
	}
	for _, c := range tools {
		if matchesCommand(cmdName, c.Name, c.Regex) {
			return helpEntry{
				cmd:  formatCmd(network.Trigger, c.Regex, c.Name),
				info: formatToolInfo(c.MCP, c.Tool),
				desc: formatDesc(c.Description, false),
			}, true
		}
	}
	if tmpl, ok := builtinHelpEntries[cmdName]; ok {
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
