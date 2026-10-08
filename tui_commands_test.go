package main

import (
	"fmt"
	"testing"
	"text/template"

	"github.com/lrstanley/girc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rivo/tview"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTUITest(t *testing.T) {
	t.Helper()
	setupTestDB(t)

	client := girc.New(girc.Config{
		Server: "localhost",
		Port:   6667,
		Nick:   "testbot",
	})
	origBots := bots
	bots = map[string]*Bot{
		"testnet": {Client: client, Network: Network{Name: "testnet", Nick: "testbot"}},
	}
	t.Cleanup(func() { bots = origBots })

	origLogView := logView
	logView = tview.NewTextView()
	t.Cleanup(func() { logView = origLogView })
}

func setupTUIWithChannel(t *testing.T) {
	setupTUITest(t)
	origBotIsInChannel := botIsInChannel
	botIsInChannel = func(_ *Bot, _ string) bool { return true }
	t.Cleanup(func() { botIsInChannel = origBotIsInChannel })
}

func getLogViewText() string {
	if logView == nil {
		return ""
	}
	return logView.GetText(true)
}

func TestTuiCmdReinject_SessionNotFound(t *testing.T) {
	setupTUITest(t)
	tuiCmdReinject([]string{"/reinject", "99999"}, "/reinject 99999")
	assert.Contains(t, getLogViewText(), "not found")
	msgs, _ := loadDBSessionMessages(99999)
	assert.Empty(t, msgs)
}

func TestTuiCmdReinject_NoBotForNetwork(t *testing.T) {
	setupTUITest(t)
	userID := ensureTestUser(t, "othernet", "testnick")
	sid, err := sessionMgr.CreateSession("othernet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "No bot connected")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdReinject_NoSystemPrompt(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", Service: "svc", Model: "model"},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "No system prompt configured")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdReinject_CompletedSession(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)
	require.NoError(t, sessionMgr.CompleteSession(sid))

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", System: "static prompt", Service: "svc", Model: "model"},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "completed")
	assert.Contains(t, getLogViewText(), "Injected system prompt")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Len(t, msgs, 1)
}

func TestTuiCmdReinject_BotNotInChannel(t *testing.T) {
	setupTUITest(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", System: "prompt", Service: "svc", Model: "model"},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "not joined")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdReinject_NilUserID(t *testing.T) {
	setupTUIWithChannel(t)
	sid, err := sessionMgr.CreateSession("testnet", "#test", 0, "chat", "svc", "model")
	require.NoError(t, err)

	require.NoError(t, theDB.Model(&Session{}).Where("id = ?", sid).Update("user_id", nil).Error)

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {Name: "chat", System: "prompt", Service: "svc", Model: "model"},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "no associated user")
}

func TestTuiCmdReinject_Success(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	respID := "resp_abc123"
	require.NoError(t, sessionMgr.UpdateResponseID(sid, &respID, ""))

	prevChats := config.Commands.Chats
	config.Commands.Chats = map[string]AIConfig{
		"chat": {
			Name:       "chat",
			System:     "You are a test bot in {{.Channel}}",
			SystemTmpl: template.Must(template.New("system").Parse("You are a test bot in {{.Channel}}")),
			Service:    "svc",
			Model:      "model",
		},
	}
	t.Cleanup(func() { config.Commands.Chats = prevChats })

	tuiCmdReinject([]string{"/reinject", fmt.Sprintf("%d", sid)}, fmt.Sprintf("/reinject %d", sid))
	assert.Contains(t, getLogViewText(), "Injected system prompt")

	msgs, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, RoleSystem, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "#test")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err)
	assert.Nil(t, session.ResponseID)
}

func TestTuiCmdSystemMsg_SessionNotFound(t *testing.T) {
	setupTUITest(t)
	tuiCmdSystemMsg([]string{"/systemmsg", "99999", "hello"}, "/systemmsg 99999 hello")
	assert.Contains(t, getLogViewText(), "not found")
}

func TestTuiCmdSystemMsg_TemplateRendering(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	respID := "resp_xyz"
	require.NoError(t, sessionMgr.UpdateResponseID(sid, &respID, ""))

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "Channel: {{.Channel}}, Nick: {{.Nick}}"},
		fmt.Sprintf("/systemmsg %d Channel: {{.Channel}}, Nick: {{.Nick}}", sid),
	)
	assert.Contains(t, getLogViewText(), "Injected system message")

	msgs, err := loadDBSessionMessages(sid)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, RoleSystem, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "#test")
	assert.Contains(t, msgs[0].Content, "testnick")

	session, err := getDBSessionByID(sid)
	require.NoError(t, err)
	assert.Nil(t, session.ResponseID)
}

func TestTuiCmdSystemMsg_TemplateParseError(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "{{.BadField"},
		fmt.Sprintf("/systemmsg %d {{.BadField", sid),
	)
	assert.Contains(t, getLogViewText(), "Template parse error")

	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdSystemMsg_NoBotForNetwork(t *testing.T) {
	setupTUITest(t)
	userID := ensureTestUser(t, "othernet", "testnick")
	sid, err := sessionMgr.CreateSession("othernet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "hello"},
		fmt.Sprintf("/systemmsg %d hello", sid),
	)
	assert.Contains(t, getLogViewText(), "No bot connected")
	msgs, _ := loadDBSessionMessages(sid)
	assert.Empty(t, msgs)
}

func TestTuiCmdSystemMsg_CompletedSession(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	require.NoError(t, sessionMgr.CompleteSession(sid))

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "post-completion instruction"},
		fmt.Sprintf("/systemmsg %d post-completion instruction", sid),
	)
	assert.Contains(t, getLogViewText(), "completed")
	assert.Contains(t, getLogViewText(), "Injected system message")

	msgs, _ := loadDBSessionMessages(sid)
	assert.Len(t, msgs, 1)
}

func TestTuiCmdSystemMsg_PlainTextNoTemplate(t *testing.T) {
	setupTUIWithChannel(t)
	userID := ensureTestUser(t, "testnet", "testnick")
	sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", "model")
	require.NoError(t, err)

	tuiCmdSystemMsg(
		[]string{"/systemmsg", fmt.Sprintf("%d", sid), "Be more helpful"},
		fmt.Sprintf("/systemmsg %d Be more helpful", sid),
	)
	assert.Contains(t, getLogViewText(), "Injected system message")

	msgs, _ := loadDBSessionMessages(sid)
	require.Len(t, msgs, 1)
	assert.Equal(t, "Be more helpful", msgs[0].Content)
}

func TestTuiCmdJoin(t *testing.T) {
	t.Run("creates config entry when channel missing", func(t *testing.T) {
		setupTUITest(t)
		tuiCmdJoin([]string{"/join", "testnet", "#newchan"}, "/join testnet #newchan")
		assert.Contains(t, getLogViewText(), "Joined #newchan on testnet")
		bot := bots["testnet"]
		_, ok := bot.Network.Channels["#newchan"]
		assert.True(t, ok, "config entry should be created for new channel")
	})

	t.Run("preserves existing channel key (no clobber)", func(t *testing.T) {
		setupTUITest(t)
		bots["testnet"].Network.Channels = map[string]ChannelConfig{"#secret": {Key: "passw0rd"}}
		tuiCmdJoin([]string{"/join", "testnet", "#secret"}, "/join testnet #secret")
		assert.Contains(t, getLogViewText(), "Joined #secret on testnet")
		assert.Equal(t, "passw0rd", bots["testnet"].Network.Channels["#secret"].Key,
			"existing key must not be clobbered")
	})

	t.Run("does not report already in (joins regardless)", func(t *testing.T) {
		setupTUITest(t)
		bots["testnet"].Network.Channels = map[string]ChannelConfig{"#x": {}}
		tuiCmdJoin([]string{"/join", "testnet", "#x"}, "/join testnet #x")
		assert.NotContains(t, getLogViewText(), "Already in")
		assert.Contains(t, getLogViewText(), "Joined #x on testnet")
	})
}

func TestTuiCmdTokenCount(t *testing.T) {
	newSession := func(t *testing.T, model string) int64 {
		t.Helper()
		userID := ensureTestUser(t, "testnet", "tokennick")
		sid, err := sessionMgr.CreateSession("testnet", "#test", userID, "chat", "svc", model)
		require.NoError(t, err)
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleSystem, Content: "sys"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleUser, Content: "hello world"}))
		require.NoError(t, sessionMgr.AddMessage(sid, ChatMessage{Role: RoleAssistant, Content: "hi there"}))
		return sid
	}
	setChats := func(t *testing.T, cfg AIConfig) {
		t.Helper()
		prevChats := config.Commands.Chats
		config.Commands.Chats = map[string]AIConfig{"chat": cfg}
		t.Cleanup(func() { config.Commands.Chats = prevChats })
	}

	t.Run("usage line", func(t *testing.T) {
		setupTUITest(t)
		tuiCmdTokenCount([]string{"/tokencount"}, "/tokencount")
		assert.Contains(t, getLogViewText(), "Usage: /tokencount <session-id>")
	})

	t.Run("invalid session id", func(t *testing.T) {
		setupTUITest(t)
		tuiCmdTokenCount([]string{"/tokencount", "abc"}, "/tokencount abc")
		assert.Contains(t, getLogViewText(), "Invalid session id")
	})

	t.Run("session not found", func(t *testing.T) {
		setupTUITest(t)
		tuiCmdTokenCount([]string{"/tokencount", "99999"}, "/tokencount 99999")
		assert.Contains(t, getLogViewText(), "not found")
	})

	t.Run("chat command no longer exists", func(t *testing.T) {
		setupTUITest(t)
		sid := newSession(t, "gpt-4o")
		setChats(t, AIConfig{Name: "chat", Service: "svc", Model: "gpt-4o"})
		// Point the session at a command name that is not configured.
		require.NoError(t, theDB.Model(&Session{}).Where("id = ?", sid).Update("chat_command", "gone").Error)
		tuiCmdTokenCount([]string{"/tokencount", fmt.Sprintf("%d", sid)}, "/tokencount "+fmt.Sprint(sid))
		assert.Contains(t, getLogViewText(), "no longer exists")
	})

	t.Run("count without provider usage", func(t *testing.T) {
		setupTUITest(t)
		sid := newSession(t, "gpt-4o")
		setChats(t, AIConfig{Name: "chat", Service: "svc", Model: "gpt-4o"})

		tuiCmdTokenCount([]string{"/tokencount", fmt.Sprintf("%d", sid)}, "/tokencount "+fmt.Sprint(sid))
		out := getLogViewText()
		assert.Contains(t, out, fmt.Sprintf("Session #%d testnet/#test model:gpt-4o service:svc", sid))
		assert.Contains(t, out, "encoding o200k_base, exact")
		assert.Contains(t, out, "across 3 messages")
		assert.Contains(t, out, "0 image part(s)")
		assert.Contains(t, out, "tools: 0 definitions, ~0 tokens", "cfg has no MCPs — no tools serialized")
		assert.Contains(t, out, "our request total (messages+tools): ")
		assert.Contains(t, out, "No provider usage recorded")
		assert.NotContains(t, out, "Ratio")
		assert.NotContains(t, out, "Reasoning tokens", "no usage rows → no reasoning section")
	})

	t.Run("count with provider usage and ratio", func(t *testing.T) {
		setupTUITest(t)
		sid := newSession(t, "grok-4")
		setChats(t, AIConfig{Name: "chat", Service: "svc", Model: "grok-4"})
		// Provider reported 2x-ish prompt tokens with partial cache.
		require.NoError(t, theDB.Create(&TurnUsage{
			SessionID: sid, Model: "grok-4", Service: "svc",
			PromptTokens: 2000, CompletionTokens: 300, CachedTokens: 800,
		}).Error)

		tuiCmdTokenCount([]string{"/tokencount", fmt.Sprintf("%d", sid)}, "/tokencount "+fmt.Sprint(sid))
		out := getLogViewText()
		assert.Contains(t, out, "encoding o200k_base, approximate")
		assert.Contains(t, out, "Provider last turn: prompt 2000 (cached 800, adjusted 1200), completion 300")
		// Primary ratio is over the messages+tools total against the RAW
		// prompt (the counting comparand); cached>0 additionally prints
		// the billing-view adjusted ratio. No Responses chain and no
		// reasoning rows → neither the reasoning section nor the second
		// ratio appears.
		assert.Regexp(t, `Ratio provider_prompt/our_total = \d+\.\d\d`, out)
		assert.Regexp(t, `Ratio provider_adjusted/our_total = \d+\.\d\d \(billing view`, out)
		assert.NotContains(t, out, "Reasoning tokens", "usage row carries no reasoning")
		assert.NotContains(t, out, "prior_reasoning")
	})

	t.Run("reasoning with responses chain surfaces replay bound and second ratio", func(t *testing.T) {
		setupTUITest(t)
		sid := newSession(t, "grok-4")
		setChats(t, AIConfig{Name: "chat", Service: "svc", Model: "grok-4"})
		// Two reasoning turns: all=140, prior=100 (the latest row's 40
		// belongs to its own completion and is not replay-eligible).
		require.NoError(t, theDB.Create(&TurnUsage{
			SessionID: sid, Model: "grok-4", Service: "svc",
			PromptTokens: 1500, CachedTokens: 0, ReasoningTokens: 100,
		}).Error)
		require.NoError(t, theDB.Create(&TurnUsage{
			SessionID: sid, Model: "grok-4", Service: "svc",
			PromptTokens: 2000, CompletionTokens: 300, CachedTokens: 800, ReasoningTokens: 40,
		}).Error)
		rid := "resp_123"
		require.NoError(t, sessionMgr.UpdateResponseID(sid, &rid, "grok-4"))

		tuiCmdTokenCount([]string{"/tokencount", fmt.Sprintf("%d", sid)}, "/tokencount "+fmt.Sprint(sid))
		out := getLogViewText()
		assert.Contains(t, out, "Reasoning tokens (turn_usage): all turns 140, prior turns 100 (replay-eligible upper bound)")
		assert.Contains(t, out, "Responses chain active — provider prompt legitimately includes replayed prior reasoning (~100 tok upper bound; server may evict)")
		assert.Contains(t, out, "Provider last turn: prompt 2000 (cached 800, adjusted 1200), completion 300")
		assert.Regexp(t, `Ratio provider_prompt/our_total = \d+\.\d\d`, out)
		assert.Regexp(t, `Ratio provider_prompt/\(our_total\+prior_reasoning\) = \d+\.\d\d`, out)
	})

	t.Run("reasoning without responses chain notes reasoning is not replayed", func(t *testing.T) {
		setupTUITest(t)
		sid := newSession(t, "grok-4")
		setChats(t, AIConfig{Name: "chat", Service: "svc", Model: "grok-4"})
		require.NoError(t, theDB.Create(&TurnUsage{
			SessionID: sid, Model: "grok-4", Service: "svc",
			PromptTokens: 1500, CachedTokens: 0, ReasoningTokens: 100,
		}).Error)
		require.NoError(t, theDB.Create(&TurnUsage{
			SessionID: sid, Model: "grok-4", Service: "svc",
			PromptTokens: 2000, CompletionTokens: 300, CachedTokens: 800, ReasoningTokens: 40,
		}).Error)

		tuiCmdTokenCount([]string{"/tokencount", fmt.Sprintf("%d", sid)}, "/tokencount "+fmt.Sprint(sid))
		out := getLogViewText()
		assert.Contains(t, out, "Reasoning tokens (turn_usage): all turns 140, prior turns 100 (replay-eligible upper bound)")
		assert.Contains(t, out, "(no Responses chain — reasoning is not replayed; prior reasoning should NOT appear in prompt_tokens)")
		assert.NotContains(t, out, "Responses chain active")
		assert.NotContains(t, out, "prior_reasoning", "no chain → no second ratio line")
		assert.Regexp(t, `Ratio provider_prompt/our_total = \d+\.\d\d`, out)
	})

	t.Run("tools from the live MCP map are counted", func(t *testing.T) {
		setupTUITest(t)
		sid := newSession(t, "grok-4")
		setChats(t, AIConfig{Name: "chat", Service: "svc", Model: "grok-4", MCPs: []string{"img-mcp"}})
		origServers := mcpServers
		mcpServers = map[string]*MCPServer{"img-mcp": {
			Tools: []*mcp.Tool{{Name: "generate_image", Description: "Generate an image from a text prompt"}},
		}}
		t.Cleanup(func() { mcpServers = origServers })
		require.NoError(t, theDB.Create(&TurnUsage{
			SessionID: sid, Model: "grok-4", Service: "svc",
			PromptTokens: 2000, CompletionTokens: 300, CachedTokens: 800,
		}).Error)

		tuiCmdTokenCount([]string{"/tokencount", fmt.Sprintf("%d", sid)}, "/tokencount "+fmt.Sprint(sid))
		out := getLogViewText()
		// 1 MCP tool + 3 builtins (builtins ride along whenever MCP
		// tools exist — getTools' exact assembly via toolDefsForConfig).
		assert.Contains(t, out, "tools: 4 definitions")
		assert.Regexp(t, `tools: 4 definitions, ~\d+ tokens`, out)
		assert.NotContains(t, out, "~0 tokens", "tool definitions must actually be counted")
	})
}
