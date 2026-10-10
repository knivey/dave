package main

import (
	"bytes"
	"testing"
	"text/template"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildSystemPromptDataNow pins the {{.Now}} template var: weekday +
// date + clock, server-local, embedding today's date — the grounding the
// model uses to resolve relative date/time language ("yesterday", "last
// Tuesday evening") into query_channel_logs from/to ranges.
func TestBuildSystemPromptDataNow(t *testing.T) {
	data := buildSystemPromptData(Network{Name: "testnet"}, nil, "#chan", "shrew")
	assert.NotEmpty(t, data.Now)
	assert.Contains(t, data.Now, data.Date, "Now embeds today's date")
	assert.Regexp(t, `\w+day 20\d\d-\d\d-\d\d \d\d:\d\d`, data.Now,
		"weekday + date + clock, e.g. \"Monday 2006-01-02 15:04\"")

	tmpl := template.Must(template.New("t").Parse("It is {{.Now}}."))
	var buf bytes.Buffer
	require.NoError(t, tmpl.Execute(&buf, data))
	assert.Contains(t, buf.String(), data.Date)
}

func TestAccountFromEvent(t *testing.T) {
	client := girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})

	t.Run("extended-join param", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "shrew", "Ron"}}
		assert.Equal(t, "shrew", accountFromEvent(client, e))
	})

	t.Run("extended-join star means unauthenticated", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "*", "Ron"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})

	t.Run("account-notify param", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_ACCOUNT, Source: &girc.Source{Name: "shrew"}, Params: []string{"shrew"}}
		assert.Equal(t, "shrew", accountFromEvent(client, e))
	})

	t.Run("account-notify logout", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_ACCOUNT, Source: &girc.Source{Name: "shrew"}, Params: []string{"*"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})

	t.Run("account message tag", func(t *testing.T) {
		e := girc.Event{Command: girc.PRIVMSG, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "lol"}}
		e.Tags = girc.Tags{"account": "shrew"}
		assert.Equal(t, "shrew", accountFromEvent(client, e))
	})

	t.Run("no account info falls back to empty girc state", func(t *testing.T) {
		e := girc.Event{Command: girc.PRIVMSG, Source: &girc.Source{Name: "shrew"}, Params: []string{"#gay", "lol"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})

	t.Run("nil source does not panic", func(t *testing.T) {
		e := girc.Event{Command: girc.PRIVMSG, Params: []string{"#gay", "lol"}}
		assert.Equal(t, "", accountFromEvent(client, e))
	})
}

// TestGetSessionConfigLiveConfigWins is the regression test for the incident
// where a session created before a config change kept its stored model while
// the live config's new reasoningeffort fell through the old overlay's
// zero-value semantics — producing an old-model-plus-new-effort request the
// API rejected. Stored settings are provenance only; the live config always
// wins (spec: docs/superpowers/specs/2026-10-06-live-config-and-usage-attribution-design.md).
func TestGetSessionConfigLiveConfigWins(t *testing.T) {
	setupTestDB(t)

	sid := createTestSession(t, "net", "#chan", "user", "chat", "openai", "old-model")
	_, err := sessionMgr.CreateSessionSettings(sid, AIConfig{
		System: "stored system",
		Model:  "old-model",
		// ReasoningEffort intentionally empty: the config had no effort
		// setting when this session was created.
	})
	require.NoError(t, err)

	session, err := sessionMgr.GetSession(sid)
	require.NoError(t, err)
	require.NotNil(t, session.SettingsID, "precondition: session has a settings row")

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Service: "openai", Model: "new-model", System: "live system", ReasoningEffort: "low"},
	}
	defer func() { config.Commands.Chats = prevChats }()

	cfg, ok := getSessionConfig(session)
	require.True(t, ok)
	assert.Equal(t, "new-model", cfg.Model, "live config model must win over the stored snapshot")
	assert.Equal(t, "low", cfg.ReasoningEffort, "live config reasoning effort must win")
	assert.Equal(t, "live system", cfg.System, "live config system prompt must win")

	// A chat command removed from config reports ok=false uniformly, even
	// for sessions that still have a settings row.
	config.Commands.Chats = map[string]AIConfig{}
	_, ok = getSessionConfig(session)
	assert.False(t, ok, "missing chat command must report ok=false despite stored settings")
}

// resetRegisteredCommands re-registers empty command maps so a test's
// registrations cannot leak into later tests (configCmds, configCmdNames,
// configCmdTakesArgs, configCmdOptionalArgs, chatCmds, rateExemptCmds are
// all rebuilt empty).
func resetRegisteredCommands(t *testing.T) {
	t.Helper()
	require.NoError(t, registerCommands(Commands{
		Completions: map[string]AIConfig{}, Chats: map[string]AIConfig{}, Tools: map[string]MCPCommandConfig{}, Generators: map[string]GeneratorConfig{},
	}))
}

func TestRegisterGenerators(t *testing.T) {
	if logger == nil {
		logger = newTestLogger()
	}
	cmds := Commands{}
	cmds.Generators = map[string]GeneratorConfig{
		"summary":  {AIConfig: AIConfig{Name: "summary", Service: "svc"}, Log: &LogQuerySpec{}},
		"fakenews": {AIConfig: AIConfig{Name: "fakenews", Service: "svc", Aliases: []string{"fn"}}},
	}
	require.NoError(t, registerCommands(cmds))
	t.Cleanup(func() {
		// reset maps to avoid leaking into other tests
		resetRegisteredCommands(t)
	})

	commandsMutex.RLock()
	defer commandsMutex.RUnlock()
	assert.NotNil(t, configCmds["summary"], "summary registered")
	assert.NotNil(t, configCmds["fakenews"])
	assert.NotNil(t, configCmds["fn"], "alias registered")
	assert.Equal(t, "fakenews", configCmdNames["fn"])
	assert.True(t, configCmdOptionalArgs["summary"], "log-fed generator takes optional args")
	assert.False(t, configCmdOptionalArgs["fakenews"])
	assert.False(t, configCmdTakesArgs["summary"], "optional-args commands are NOT takesArgs")
	assert.True(t, configCmdTakesArgs["fakenews"], "non-log generator requires args")
	assert.False(t, chatCmds["summary"], "generators never join chatCmds (no context to clear)")
}

func TestRegisterGeneratorsConflictDetected(t *testing.T) {
	if logger == nil {
		logger = newTestLogger()
	}
	cmds := Commands{
		Chats:      map[string]AIConfig{"dupe": {Name: "dupe", Service: "svc"}},
		Generators: map[string]GeneratorConfig{"dupe": {AIConfig: AIConfig{Name: "dupe", Service: "svc"}}},
	}
	err := registerCommands(cmds)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicts with")
}

func TestDispatchOptionalArgsGenerator(t *testing.T) {
	if logger == nil {
		logger = newTestLogger()
	}
	// Wire a generator into the live maps and drive the match predicate the
	// way handleTrigger computes it.
	cmds := Commands{Generators: map[string]GeneratorConfig{
		"summary": {AIConfig: AIConfig{Name: "summary", Service: "svc"}, Log: &LogQuerySpec{}},
	}}
	require.NoError(t, registerCommands(cmds))
	t.Cleanup(func() {
		resetRegisteredCommands(t)
	})

	commandsMutex.RLock()
	_, ok := configCmds["summary"]
	optional := configCmdOptionalArgs["summary"]
	takesArgs := configCmdTakesArgs["summary"]
	commandsMutex.RUnlock()
	require.True(t, ok)

	// The predicate handleTrigger applies (irc_handlers.go):
	match := func(hasArgs bool) bool {
		return takesArgs == hasArgs || (optional && hasArgs)
	}
	assert.True(t, match(false), "bare ^summary dispatches")
	assert.True(t, match(true), "^summary 6h dispatches")
}
