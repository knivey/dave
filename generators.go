package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lrstanley/girc"
)

// fetchChannelLogFn is the injection point for tests (same pattern as
// connectMCPServerImpl).
var fetchChannelLogFn = fetchChannelLog

// generator is the executor for one-shot generator commands (spec
// docs/superpowers/specs/2026-10-09-generators-design.md): chat() minus
// persistence. It renders the system prompt, builds one user message
// (optionally carrying the token-capped log transcript), and runs the
// UNCHANGED runTurn agentic loop on an ephemeral turn — streaming, tools,
// empty-response retry all apply. No session row is created; usage rows are
// attributed with SessionID 0.
func generator(network Network, c *girc.Client, e girc.Event, cfg GeneratorConfig, ctx context.Context, output chan<- string, resolvedUser *User, args ...string) {
	runner := newChatRunnerFn(network, c, cfg.AIConfig, ctx, output).(*chatRunner)
	nick := e.Source.Name
	channelRaw := e.Params[0]
	channel := normalizeIRC(channelRaw, getCasemapping(network.Name))
	if resolvedUser == nil {
		resolvedUser = resolvedUserFromCtx(ctx)
	}
	if resolvedUser == nil {
		var err error
		resolvedUser, err = resolveIRCUser(network, c, e)
		if err != nil {
			runner.logger.Error("failed to resolve user in generator()", "error", err)
		}
		proceed, _ := handleResolveResult(c, e, resolvedUser, err)
		if !proceed {
			return
		}
	}
	if resolvedUser == nil {
		runner.logger.Warn("generator() got nil resolved user, dropping message", "nick", nick)
		return
	}
	if resolvedUser.Flagged {
		runner.logger.Warn("generator() proceeding with flagged user",
			"user_id", resolvedUser.ID, "nick", nick, "reason", resolvedUser.FlaggedReason)
	}
	runner.userID = resolvedUser.ID
	runner.hostmask = e.Source.Name + "!" + e.Source.Ident + "@" + e.Source.Host
	runner.setChannel(channel, nick, resolvedUser.ID)
	runner.ephemeral = true

	userText := ""
	if len(args) > 0 {
		userText = args[0]
	}

	if cfg.Log != nil {
		spec := *cfg.Log
		// Production specs are defaulted at config-load time; hand-built or
		// partial specs (tests, future callers) re-default here so an empty
		// window never renders "last 0s" in the transcript header or the
		// no_activity notice.
		applyLogQueryDefaults(&spec)
		focus := userText
		if userText != "" {
			first, rest, has := splitFirstWord(userText)
			if _, ok := parseWindowDuration(first); ok {
				// The token IS the duration string (same grammar as the
				// config field) — assign it raw; windowDuration() below
				// resolves it.
				spec.Window = first
				focus = ""
				if has {
					focus = rest
				}
			}
		}
		window, werr := spec.windowDuration()
		if werr != nil {
			// Unreachable via config (validateLogQuerySpec rejects bad
			// windows at load) and via args (parseWindowDuration vetted the
			// token above); guards hand-built specs. Defense in depth.
			runner.sendError(werr.Error())
			runner.logger.Error("generator log window invalid", "error", werr)
			return
		}
		now := time.Now()
		lw, err := fetchChannelLogFn(spec, network.Name, channelRaw, channel, cfg.Model, now)
		if err != nil {
			if errors.Is(err, errLogWindowTooLarge) {
				runner.sendError(expandNotice(getNotices().Generators.WindowTooLarge, map[string]string{
					"cap": fmt.Sprintf("%d", logQueryRowCap),
				}))
				return
			}
			runner.sendError(err.Error())
			runner.logger.Error("generator log query failed", "error", err)
			return
		}
		if lw.TotalLines == 0 {
			runner.sendError(expandNotice(getNotices().Generators.NoActivity, map[string]string{
				"window": formatDuration(window),
			}))
			return
		}
		// "coverage" carries the kept rows' actual time range; BOTH endpoints
		// must be set or the var stays empty (a hand-built result with only
		// one of the two would otherwise render "14:32 to 00:00") —
		// production results always set both together when any row was kept.
		// Shared by the truncated notice's {coverage} var and the INFO log
		// below (the log's actual-coverage field).
		coverage := ""
		if !lw.FirstKept.IsZero() && !lw.LastKept.IsZero() {
			coverage = fmt.Sprintf("%s to %s", lw.FirstKept.Format("15:04"), lw.LastKept.Format("15:04"))
		}
		if lw.Truncated {
			// "dropped" rides the vars map (and the default truncated
			// template) alongside kept/total so the disclosure is direct —
			// see the notice-default deviation note in
			// setNoticesDefaults and task-5-report.md.
			runner.sendWarning(expandNotice(getNotices().Generators.Truncated, map[string]string{
				"kept":     fmt.Sprintf("%d", lw.TotalLines-lw.DroppedLines),
				"total":    fmt.Sprintf("%d", lw.TotalLines),
				"dropped":  fmt.Sprintf("%d", lw.DroppedLines),
				"tokens":   fmt.Sprintf("%d", lw.Tokens),
				"budget":   fmt.Sprintf("%d", spec.MaxTokens),
				"coverage": coverage,
			}))
		}
		runner.logger.Info("generator log query",
			"trigger", cfg.Name,
			"window", spec.Window, "coverage", coverage,
			"files", len(lw.Files),
			"lines", lw.TotalLines, "dropped", lw.DroppedLines,
			"tokens", lw.Tokens, "budget", spec.MaxTokens,
			"truncated", lw.Truncated)

		instruction := focus
		if instruction == "" {
			instruction = cfg.Prompt
		}
		if instruction == "" {
			instruction = "Summarize the following channel activity."
		}
		var b strings.Builder
		b.WriteString(instruction)
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "Channel activity for %s on %s, last %s (%d lines, %d tokens):\n",
			channel, network.Name, formatDuration(window), lw.TotalLines, lw.Tokens)
		b.WriteString(strings.Join(lw.Lines, "\n"))
		userText = b.String()
	} else if userText == "" {
		userText = cfg.Prompt // defensive: dispatch requires args for non-log commands
	}

	systemContent := cfg.System
	if cfg.SystemTmpl != nil {
		data := buildSystemPromptData(network, c, channel, nick)
		data.AsyncResultRole = asyncResultRole(cfg.AIConfig)
		var buf strings.Builder
		if err := cfg.SystemTmpl.Execute(&buf, data); err != nil {
			runner.logger.Error("system prompt template execution error:", err)
		} else {
			systemContent = buf.String()
		}
	}

	turn := newEphemeralTurnContext([]ChatMessage{
		{Role: RoleSystem, Content: systemContent},
		{Role: RoleUser, Content: userText},
	})
	runner.runTurn(turn)
}
