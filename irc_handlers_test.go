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

		var hosts []UserKnownHost
		theDB.Where("user_id = ?", user.ID).Find(&hosts)
		require.Len(t, hosts, 1)
		assert.Equal(t, "~u", hosts[0].Ident)
		assert.Equal(t, "cloak.example", hosts[0].Host)
	})

	t.Run("whox 0 means unauthenticated", func(t *testing.T) {
		recordPendingJoinWHO("testnet", "zero")
		handleWHOXReply(network, whoxEvent("Zero", "~u", "cloak.example", "0"), newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "zero").First(&user).Error)
		assert.Equal(t, "", user.IRCAccount)
	})

	t.Run("whox star means unauthenticated", func(t *testing.T) {
		// Unseen host so the row is created fresh here rather than recycled
		// by host recovery from the earlier subtests' cloak.example rows.
		recordPendingJoinWHO("testnet", "starnick")
		handleWHOXReply(network, whoxEvent("StarNick", "~u", "star.example", "*"), newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "starnick").First(&user).Error)
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

func TestHandleAccountChange(t *testing.T) {
	setupTestDB(t)

	client := girc.New(girc.Config{Server: "localhost", Port: 6667, Nick: "testbot"})
	network := Network{Name: "testnet"}
	acctEvent := func(nick, account string) girc.Event {
		return girc.Event{
			Command: girc.CAP_ACCOUNT,
			Source:  &girc.Source{Name: nick, Ident: "~u", Host: "cloak.example"},
			Params:  []string{account},
		}
	}

	t.Run("account attaches to active row holding the nick", func(t *testing.T) {
		ghost, err := createNewUser("testnet", "Newbie", "newbie", "", "~u", "cloak.example")
		require.NoError(t, err)

		handleAccountChange(network, client, acctEvent("Newbie", "newacct"), newTestLogger())

		var reloaded User
		require.NoError(t, theDB.First(&reloaded, ghost.ID).Error)
		assert.Equal(t, "newacct", reloaded.IRCAccount)
	})

	t.Run("account claim displaces ghost holding the nick (production incident)", func(t *testing.T) {
		// Real account holder: active under a different nick.
		owner, err := createNewUser("testnet", "again", "again", "shrew", "~u", "old.host")
		require.NoError(t, err)
		// Ghost row created at join time before the account was known.
		ghost, err := createNewUser("testnet", "shrew2", "shrew2", "", "~u", "cloak.example")
		require.NoError(t, err)

		handleAccountChange(network, client, acctEvent("shrew2", "shrew"), newTestLogger())

		var ownerRow, ghostRow User
		require.NoError(t, theDB.First(&ownerRow, owner.ID).Error)
		require.NoError(t, theDB.First(&ghostRow, ghost.ID).Error)
		assert.Equal(t, "shrew2", ownerRow.CurrentNick, "account holder takes the nick")
		assert.True(t, ghostRow.Released, "ghost nick released")
	})

	t.Run("bot's own account event is ignored", func(t *testing.T) {
		before := int64(0)
		theDB.Model(&User{}).Count(&before)
		handleAccountChange(network, client, acctEvent("testbot", "botacct"), newTestLogger())
		after := int64(0)
		theDB.Model(&User{}).Count(&after)
		assert.Equal(t, before, after, "bot's own ACCOUNT must not create or change rows")
	})

	t.Run("previously unseen authed user gets a new row", func(t *testing.T) {
		// Unseen host so the row is created fresh here rather than recycled
		// by host recovery from the earlier subtests' cloak.example rows.
		e := girc.Event{
			Command: girc.CAP_ACCOUNT,
			Source:  &girc.Source{Name: "FreshFace", Ident: "~u", Host: "unseen.example"},
			Params:  []string{"freshacct"},
		}
		handleAccountChange(network, client, e, newTestLogger())

		var user User
		require.NoError(t, theDB.Where("normalized_nick = ?", "freshface").First(&user).Error)
		assert.Equal(t, "freshacct", user.IRCAccount)
	})

	t.Run("logout is ignored", func(t *testing.T) {
		before := int64(0)
		theDB.Model(&User{}).Count(&before)
		handleAccountChange(network, client, acctEvent("nobody", "*"), newTestLogger())
		after := int64(0)
		theDB.Model(&User{}).Count(&after)
		assert.Equal(t, before, after, "logout must not create or change rows")
	})

	t.Run("malformed event is ignored", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_ACCOUNT, Source: &girc.Source{Name: "x"}, Params: []string{}}
		handleAccountChange(network, client, e, newTestLogger()) // must not panic
	})
}

func TestHandleHostChange(t *testing.T) {
	setupTestDB(t)

	chgEvent := func(nick, ident, host string) girc.Event {
		return girc.Event{
			Command: girc.CAP_CHGHOST,
			Source:  &girc.Source{Name: nick, Ident: "~old", Host: "old.example"},
			Params:  []string{ident, host},
		}
	}

	t.Run("new ident@host recorded for active user", func(t *testing.T) {
		user, err := createNewUser("testnet", "Mover", "mover", "", "~old", "old.example")
		require.NoError(t, err)

		handleHostChange("testnet", chgEvent("Mover", "~new", "new.example"), newTestLogger())

		var hosts []UserKnownHost
		theDB.Where("user_id = ?", user.ID).Find(&hosts)
		assert.Len(t, hosts, 2, "old and new host both known")
		found := false
		for _, h := range hosts {
			if h.Ident == "~new" && h.Host == "new.example" {
				found = true
			}
		}
		assert.True(t, found, "new host must be recorded")
	})

	t.Run("unknown nick is a no-op", func(t *testing.T) {
		before := int64(0)
		theDB.Model(&UserKnownHost{}).Count(&before)
		handleHostChange("testnet", chgEvent("Stranger", "~x", "x.example"), newTestLogger())
		after := int64(0)
		theDB.Model(&UserKnownHost{}).Count(&after)
		assert.Equal(t, before, after)
	})

	t.Run("released row is a no-op", func(t *testing.T) {
		rel, err := createNewUser("testnet", "GoneUser", "goneuser", "", "~old", "gone.example")
		require.NoError(t, err)
		require.NoError(t, releaseUserNick(rel.ID))

		before := int64(0)
		theDB.Model(&UserKnownHost{}).Count(&before)
		handleHostChange("testnet", chgEvent("GoneUser", "~new", "new.example"), newTestLogger())
		after := int64(0)
		theDB.Model(&UserKnownHost{}).Count(&after)
		assert.Equal(t, before, after, "released rows must not accumulate host evidence")
	})

	t.Run("malformed event is ignored", func(t *testing.T) {
		e := girc.Event{Command: girc.CAP_CHGHOST, Source: &girc.Source{Name: "x"}, Params: []string{"onlyone"}}
		handleHostChange("testnet", e, newTestLogger()) // must not panic
	})
}
