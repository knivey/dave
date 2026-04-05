package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/lrstanley/girc"
)

type helpEntry struct {
	cmd  string
	info string
	desc string
}

func help(network Network, client *girc.Client, event girc.Event, _ ...string) {
	key := network.Name + event.Params[0]
	startedRunning(key)
	defer stoppedRunning(key)

	botnick := client.GetNick()
	var lines []string

	lines = append(lines, fmt.Sprintf("I'm %s! Use my commands below to chat or generate images.", botnick))
	lines = append(lines, fmt.Sprintf("Only Chat commands start a persistent context. After starting one, reply with my nick (e.g. \"%s, your message here\") to continue that context without using a command.", botnick))
	lines = append(lines, "Commands marked with (regex) use pattern matching, the trigger can match more than one name.")
	lines = append(lines, fmt.Sprintf("  %sstop \u2014 Stop text generation (including this help message)", network.Trigger))

	if len(config.Commands.Completions) > 0 {
		var entries []helpEntry
		for _, c := range config.Commands.Completions {
			entries = append(entries, helpEntry{
				cmd:  formatCmd(network.Trigger, c.Regex, c.Name),
				info: fmt.Sprintf("[%s/%s]", c.Service, c.Model),
				desc: formatDesc(c.Description, c.DetectImages, false),
			})
		}
		lines = append(lines, "\x02Completions:\x02")
		for _, l := range formatTable(entries) {
			lines = append(lines, "  "+l)
		}
	}

	if len(config.Commands.Chats) > 0 {
		var entries []helpEntry
		for _, c := range config.Commands.Chats {
			entries = append(entries, helpEntry{
				cmd:  formatCmd(network.Trigger, c.Regex, c.Name),
				info: fmt.Sprintf("[%s/%s]", c.Service, c.Model),
				desc: formatDesc(c.Description, c.DetectImages, false),
			})
		}
		lines = append(lines, "\x02Chats:\x02")
		for _, l := range formatTable(entries) {
			lines = append(lines, "  "+l)
		}
	}

	if len(config.Commands.SD) > 0 || len(config.Commands.Comfy) > 0 {
		var entries []helpEntry
		for _, c := range config.Commands.SD {
			entries = append(entries, helpEntry{
				cmd:  formatCmd(network.Trigger, c.Regex, c.Name),
				info: "",
				desc: formatDesc(c.Description, false, false),
			})
		}
		for _, c := range config.Commands.Comfy {
			entries = append(entries, helpEntry{
				cmd:  formatCmd(network.Trigger, c.Regex, c.Name),
				info: formatComfyInfo(c.EnhancePrompt),
				desc: formatDesc(c.Description, false, false),
			})
		}
		lines = append(lines, "\x02Image commands:\x02")
		for _, l := range formatTable(entries) {
			lines = append(lines, "  "+l)
		}
	}

	for _, line := range lines {
		if !getRunning(key) {
			return
		}
		for _, wrapped := range wrapLine(line) {
			if !getRunning(key) {
				return
			}
			client.Cmd.Reply(event, "\x02\x02"+wrapped)
			time.Sleep(time.Millisecond * network.Throttle)
		}
	}
}

func formatCmd(trigger, regex, name string) string {
	cmd := trigger + regex
	if regex != name {
		cmd += " (regex)"
	}
	return cmd
}

func formatDesc(desc string, detectImages bool, _ bool) string {
	if desc == "" {
		desc = "no description"
	}
	if detectImages {
		desc += " [handles images]"
	}
	return desc
}

func formatComfyInfo(enhancePrompt string) string {
	if enhancePrompt != "" {
		return "[prompt enhanced]"
	}
	return ""
}

func formatTable(entries []helpEntry) []string {
	if len(entries) == 0 {
		return nil
	}

	maxCmd := 0
	maxInfo := 0
	for _, e := range entries {
		if len(e.cmd) > maxCmd {
			maxCmd = len(e.cmd)
		}
		if len(e.info) > maxInfo {
			maxInfo = len(e.info)
		}
	}

	var lines []string
	for _, e := range entries {
		line := e.cmd + strings.Repeat(" ", maxCmd-len(e.cmd)+2)
		if e.info != "" {
			line += e.info + strings.Repeat(" ", maxInfo-len(e.info)+2)
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
