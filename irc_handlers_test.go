package main

import (
	"testing"
	"time"

	logxi "github.com/mgutz/logxi/v1"
	"github.com/stretchr/testify/assert"
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
