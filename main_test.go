package main

import (
	"testing"

	"github.com/lrstanley/girc"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
