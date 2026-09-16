package main

import (
	"testing"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestLogger returns a logxi logger with the level set to LevelAll, per the
// AGENTS.md convention that every logxi.New() logger MUST call SetLevel.
func newTestLogger() logxi.Logger {
	l := logxi.New("test")
	l.SetLevel(logxi.LevelAll)
	return l
}

func TestHandleSelfKick(t *testing.T) {
	origRejoin := rejoinChannel
	origSchedule := scheduleRejoin
	t.Cleanup(func() { rejoinChannel = origRejoin; scheduleRejoin = origSchedule })

	t.Run("enabled schedules rejoin with configured key and default delay", func(t *testing.T) {
		bot := &Bot{Network: Network{
			Name:     "testnet",
			Channels: map[string]ChannelConfig{"#Foo": {Key: "sekret"}},
		}}
		var gotDelay time.Duration
		var joinCh, joinKey string
		var scheduled bool
		scheduleRejoin = func(d time.Duration, f func()) { scheduled = true; gotDelay = d; f() }
		rejoinChannel = func(_ *Bot, ch, key string) { joinCh, joinKey = ch, key }

		handleSelfKick(bot, newTestLogger(), "testnet", "#foo")

		assert.True(t, scheduled, "should schedule a rejoin")
		assert.Equal(t, 3*time.Second, gotDelay, "default delay")
		assert.Equal(t, "#foo", joinCh)
		assert.Equal(t, "sekret", joinKey, "should use configured key")
		assert.Equal(t, "sekret", bot.Network.Channels["#Foo"].Key, "config must be untouched")
	})

	t.Run("uses configured auto_rejoin_delay", func(t *testing.T) {
		customDelay := 7 * time.Second
		bot := &Bot{Network: Network{
			Name:            "testnet",
			AutoRejoinDelay: &customDelay,
			Channels:        map[string]ChannelConfig{"#foo": {Key: "k"}},
		}}
		var gotDelay time.Duration
		scheduleRejoin = func(d time.Duration, f func()) { gotDelay = d; f() }
		rejoinChannel = func(*Bot, string, string) {}

		handleSelfKick(bot, newTestLogger(), "testnet", "#foo")

		assert.Equal(t, customDelay, gotDelay, "should use the configured delay, not the default")
	})

	t.Run("disabled by network does not schedule", func(t *testing.T) {
		bot := &Bot{Network: Network{
			Name:       "testnet",
			AutoRejoin: boolPtr(false),
			Channels:   map[string]ChannelConfig{"#foo": {}},
		}}
		scheduled := false
		scheduleRejoin = func(time.Duration, func()) { scheduled = true }
		rejoinChannel = func(*Bot, string, string) {}

		handleSelfKick(bot, newTestLogger(), "testnet", "#foo")

		assert.False(t, scheduled, "disabled network should not schedule")
		_, ok := bot.Network.Channels["#foo"]
		assert.True(t, ok, "config entry must not be removed")
	})

	t.Run("channel override disables when network enabled", func(t *testing.T) {
		bot := &Bot{Network: Network{
			Name:       "testnet",
			AutoRejoin: boolPtr(true),
			Channels:   map[string]ChannelConfig{"#foo": {AutoRejoin: boolPtr(false)}},
		}}
		scheduled := false
		scheduleRejoin = func(time.Duration, func()) { scheduled = true }
		rejoinChannel = func(*Bot, string, string) {}

		handleSelfKick(bot, newTestLogger(), "testnet", "#foo")

		assert.False(t, scheduled, "channel opt-out should win")
	})

	t.Run("unconfigured channel rejoins plain (no key)", func(t *testing.T) {
		bot := &Bot{Network: Network{Name: "testnet", Channels: map[string]ChannelConfig{}}}
		var joinCh, joinKey string
		scheduleRejoin = func(_ time.Duration, f func()) { f() }
		rejoinChannel = func(_ *Bot, ch, key string) { joinCh, joinKey = ch, key }

		handleSelfKick(bot, newTestLogger(), "testnet", "#newchan")

		assert.Equal(t, "#newchan", joinCh)
		assert.Equal(t, "", joinKey, "unconfigured channel has no key")
	})

	t.Run("disconnected client does not panic and does not rejoin", func(t *testing.T) {
		// Reset to the REAL rejoinChannel so the production nil-Client guard
		// actually runs (earlier subtests stubbed it). bot.Client is left nil,
		// so the IsConnected guard must make the late callback a safe no-op
		// (no panic, no join attempted).
		rejoinChannel = origRejoin
		bot := &Bot{Network: Network{
			Name:     "testnet",
			Channels: map[string]ChannelConfig{"#foo": {}},
		}}
		// scheduleRejoin fires the callback synchronously so rejoinChannel runs.
		scheduleRejoin = func(_ time.Duration, f func()) { f() }

		handleSelfKick(bot, newTestLogger(), "testnet", "#foo")

		// Reaching here means no panic. Config must be untouched (not removed).
		_, ok := bot.Network.Channels["#foo"]
		assert.True(t, ok, "config must not be removed")
	})
}

func TestPendingJoinWHO(t *testing.T) {
	resetPendingJoinWHO(t)

	t.Run("record then take roundtrip", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "shrew")
		assert.True(t, takePendingJoinWHO("testnet", "shrew"))
		assert.False(t, takePendingJoinWHO("testnet", "shrew"), "entry consumed by first take")
	})

	t.Run("network isolation", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "shrew")
		assert.False(t, takePendingJoinWHO("othernet", "shrew"))
		assert.True(t, takePendingJoinWHO("testnet", "shrew"))
	})

	t.Run("nick isolation", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "shrew")
		assert.False(t, takePendingJoinWHO("testnet", "other"))
	})

	t.Run("expired entries are pruned on record", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "old")
		// Age the entry past the TTL by manipulating the recorded time.
		pendingJoinWHOMu.Lock()
		pendingJoinWHO["testnet"]["old"] = time.Now().Add(-2 * pendingJoinWHOTTL)
		pendingJoinWHOMu.Unlock()

		recordPendingJoinWHO("testnet", "new")
		assert.False(t, takePendingJoinWHO("testnet", "old"), "expired entry must be gone")
		assert.True(t, takePendingJoinWHO("testnet", "new"))
	})

	t.Run("re-record refreshes the timestamp", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "shrew")
		recordPendingJoinWHO("testnet", "shrew")
		pendingJoinWHOMu.Lock()
		assert.Len(t, pendingJoinWHO["testnet"], 1, "no duplicate entries")
		pendingJoinWHOMu.Unlock()
	})
}

func resetPendingJoinWHO(t *testing.T) {
	t.Helper()
	pendingJoinWHOMu.Lock()
	pendingJoinWHO = map[string]map[string]time.Time{}
	pendingJoinWHOMu.Unlock()
}

func TestHandleUserJoin(t *testing.T) {
	resetPendingJoinWHO(t)
	setupTestDB(t)

	client := girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})
	network := Network{Name: "testnet"}

	countUsers := func() int64 {
		var n int64
		theDB.Model(&User{}).Count(&n)
		return n
	}

	t.Run("extended-join account resolves immediately", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "shrew", Ident: "~u", Host: "cloak.example"}, Params: []string{"#gay", "shrew", "Ron"}}
		handleUserJoin(network, client, e, newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "shrew").First(&user).Error)
		assert.Equal(t, "shrew", user.IRCAccount)
	})

	t.Run("extended-join star resolves immediately without account", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "anon", Ident: "~u", Host: "cloak.example"}, Params: []string{"#gay", "*", "Ron"}}
		handleUserJoin(network, client, e, newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "anon").First(&user).Error)
		assert.Equal(t, "", user.IRCAccount)
	})

	t.Run("no account info defers resolution", func(t *testing.T) {
		before := countUsers()
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "deferred", Ident: "~u", Host: "cloak.example"}, Params: []string{"#gay"}}
		handleUserJoin(network, client, e, newTestLogger())

		assert.Equal(t, before, countUsers(), "no user row must be created yet")
		assert.True(t, takePendingJoinWHO("testnet", "deferred"))
	})

	t.Run("account tag resolves immediately", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "tagged", Ident: "~u", Host: "cloak.example"}, Params: []string{"#gay"}}
		e.Tags = girc.Tags{"account": "tagacct"}
		handleUserJoin(network, client, e, newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "tagged").First(&user).Error)
		assert.Equal(t, "tagacct", user.IRCAccount)
	})

	t.Run("bot's own join is ignored", func(t *testing.T) {
		before := countUsers()
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "testbot", Ident: "~u", Host: "bot.example"}, Params: []string{"#gay", "botacct", "Bot"}}
		handleUserJoin(network, client, e, newTestLogger())
		assert.Equal(t, before, countUsers())
	})

	t.Run("mixed-case nick records normalized deferral key", func(t *testing.T) {
		before := countUsers()
		e := girc.Event{Command: girc.JOIN, Source: &girc.Source{Name: "Shrew^", Ident: "~u", Host: "cloak.example"}, Params: []string{"#gay"}}
		handleUserJoin(network, client, e, newTestLogger())

		assert.Equal(t, before, countUsers(), "no user row must be created yet")
		assert.True(t, takePendingJoinWHO("testnet", "shrew^"), "deferral key must be casefolded")
	})

	t.Run("nil source does not panic", func(t *testing.T) {
		e := girc.Event{Command: girc.JOIN, Params: []string{"#gay"}}
		handleUserJoin(network, client, e, newTestLogger())
	})
}

func TestHandleWHOXReply(t *testing.T) {
	resetPendingJoinWHO(t)
	setupTestDB(t)

	network := Network{Name: "testnet"}
	whoxEvent := func(nick, ident, host, account string) girc.Event {
		return girc.Event{
			Command: girc.RPL_WHOSPCRPL,
			Params:  []string{"testbot", "1", "#gay", ident, host, nick, account, "Real Name"},
		}
	}

	t.Run("pending nick resolves with whox account", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "whouser")
		handleWHOXReply(network, whoxEvent("whoUser", "~u", "cloak.example", "whoacct"), newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "whouser").First(&user).Error)
		assert.Equal(t, "whoacct", user.IRCAccount)
		assert.False(t, takePendingJoinWHO("testnet", "whouser"), "entry consumed")
	})

	t.Run("whox 0 means unauthenticated", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "zero")
		handleWHOXReply(network, whoxEvent("Zero", "~u", "cloak.example", "0"), newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "zero").First(&user).Error)
		assert.Equal(t, "", user.IRCAccount)
	})

	t.Run("non-pending nick is ignored", func(t *testing.T) {
		before := int64(0)
		theDB.Model(&User{}).Count(&before)
		handleWHOXReply(network, whoxEvent("Random", "~u", "cloak.example", "acct"), newTestLogger())
		after := int64(0)
		theDB.Model(&User{}).Count(&after)
		assert.Equal(t, before, after, "no row created for non-pending nick")
	})

	t.Run("wrong querytype is ignored, pending preserved", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "qt")
		e := girc.Event{Command: girc.RPL_WHOSPCRPL, Params: []string{"testbot", "9", "#gay", "~u", "cloak.example", "QT", "acct", "Real"}}
		handleWHOXReply(network, e, newTestLogger())
		assert.True(t, takePendingJoinWHO("testnet", "qt"), "pending entry must survive")
	})

	t.Run("malformed reply is ignored", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "mal")
		e := girc.Event{Command: girc.RPL_WHOSPCRPL, Params: []string{"testbot", "1"}}
		handleWHOXReply(network, e, newTestLogger())
		assert.True(t, takePendingJoinWHO("testnet", "mal"), "pending entry must survive")
	})
}
