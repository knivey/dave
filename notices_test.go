package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandNotice(t *testing.T) {
	tests := []struct {
		name     string
		tmpl     string
		vars     map[string]string
		expected string
	}{
		{
			name:     "no placeholders",
			tmpl:     "hello world",
			vars:     nil,
			expected: "hello world",
		},
		{
			name:     "single placeholder",
			tmpl:     "hello {name}",
			vars:     map[string]string{"name": "dave"},
			expected: "hello dave",
		},
		{
			name:     "multiple placeholders",
			tmpl:     "{greeting} {name}, you are {age}",
			vars:     map[string]string{"greeting": "hi", "name": "dave", "age": "5"},
			expected: "hi dave, you are 5",
		},
		{
			name:     "repeated placeholder",
			tmpl:     "{x} and {x}",
			vars:     map[string]string{"x": "1"},
			expected: "1 and 1",
		},
		{
			name:     "missing placeholder left as-is",
			tmpl:     "hello {missing}",
			vars:     map[string]string{"other": "value"},
			expected: "hello {missing}",
		},
		{
			name:     "empty vars",
			tmpl:     "no change {here}",
			vars:     map[string]string{},
			expected: "no change {here}",
		},
		{
			name:     "IRC color codes preserved",
			tmpl:     "\x0304❗ error: {error}",
			vars:     map[string]string{"error": "something broke"},
			expected: "\x0304❗ error: something broke",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := expandNotice(tt.tmpl, tt.vars)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSetNoticesDefaults(t *testing.T) {
	n := NoticesConfig{}
	setNoticesDefaults(&n)

	assert.Equal(t, "\x0307⏳ Queued (position {position})\x0F", n.Queue.Msg)
	assert.Contains(t, n.Queue.Started, "{nick}")
	assert.Contains(t, n.Queue.Started, "{wait}")
	assert.Contains(t, n.Queue.AsyncSubmitted, "{nick}")
	assert.Equal(t, []string{"hold on you're going to fast"}, n.Rate.Msgs)
	assert.Equal(t, "\x0304❗ ", n.Format.ErrorPrefix)
	assert.Equal(t, "\x0307⚠️ ", n.Format.WarnPrefix)
	assert.NotEmpty(t, n.Mentions.NoContext)
	assert.NotEmpty(t, n.Mentions.Muted)
	assert.NotEmpty(t, n.Sessions.Header)
	assert.NotEmpty(t, n.Sessions.None)
	assert.NotEmpty(t, n.Sessions.DetailHeader)
	assert.NotEmpty(t, n.Sessions.Truncated)
	assert.NotEmpty(t, n.Sessions.NotFound)
	assert.NotEmpty(t, n.Sessions.NotOwned)
	assert.NotEmpty(t, n.Sessions.InvalidID)
	assert.NotEmpty(t, n.Sessions.NoMessages)
	assert.NotEmpty(t, n.Sessions.CommandGone)
	assert.NotEmpty(t, n.Sessions.Deleted)
	assert.NotEmpty(t, n.Sessions.Paused)
	assert.NotEmpty(t, n.Sessions.Resumed)
	assert.NotEmpty(t, n.Sessions.Switched)
	assert.NotEmpty(t, n.Sessions.StatsFormat)
	assert.NotEmpty(t, n.Sessions.HistoryUsage)
	assert.NotEmpty(t, n.Sessions.DeleteUsage)
	assert.NotEmpty(t, n.Sessions.ResumeUsage)
	assert.NotEmpty(t, n.DB.NotAvailable)
	assert.NotEmpty(t, n.DB.QuerySessions)
	assert.NotEmpty(t, n.DB.LoadMessages)
	assert.NotEmpty(t, n.DB.DeleteFailed)
	assert.NotEmpty(t, n.DB.QueryStats)
	assert.NotEmpty(t, n.DB.QueryJobs)
	assert.NotEmpty(t, n.DB.ProcessImages)
	assert.NotEmpty(t, n.DB.PastebinUpload)
	assert.NotEmpty(t, n.DB.InternalError)
	assert.NotEmpty(t, n.Images.LimitReached)
	assert.NotEmpty(t, n.Images.PartialLimit)
	assert.NotEmpty(t, n.Images.NoImages)
	assert.NotEmpty(t, n.Images.JobStatus)
	assert.NotEmpty(t, n.Tools.Call)
	assert.NotEmpty(t, n.Tools.CallMulti)
	assert.NotEmpty(t, n.Tools.CallLimit)
	assert.NotEmpty(t, n.Tools.Failed)
	assert.NotEmpty(t, n.Tools.Usage)
	assert.NotEmpty(t, n.Tools.Unexpected)
	assert.NotEmpty(t, n.Tools.ToolAsyncStarted)
	assert.NotEmpty(t, n.Pastebin.Link)
	assert.NotEmpty(t, n.Pastebin.Failed)
	assert.NotEmpty(t, n.Jobs.NoJobs)
	assert.NotEmpty(t, n.Jobs.QueueHeader)
	assert.NotEmpty(t, n.Jobs.QueueRunning)
	assert.NotEmpty(t, n.Jobs.QueuePending)
	assert.NotEmpty(t, n.Jobs.BgHeader)
	assert.NotEmpty(t, n.Jobs.BgLine)
	assert.NotEmpty(t, n.Users.ResolveTransient)
	assert.NotEmpty(t, n.Users.ResolvePersistent)
	assert.Contains(t, n.Users.ResolveTransient, "{nick}")
	assert.Contains(t, n.Users.ResolvePersistent, "{nick}")
	assert.NotEmpty(t, n.Support)
}

func TestSetNoticesDefaultsPreservesSetValues(t *testing.T) {
	n := NoticesConfig{
		Queue: QueueNotices{
			Msg: "custom queue msg",
		},
		Rate: RateNotices{
			Msgs: []string{"custom rate"},
		},
		Sessions: SessionNotices{
			NotFound: "custom not found",
		},
	}
	setNoticesDefaults(&n)

	assert.Equal(t, "custom queue msg", n.Queue.Msg)
	assert.NotEmpty(t, n.Queue.Started)
	assert.Equal(t, []string{"custom rate"}, n.Rate.Msgs)
	assert.Equal(t, "custom not found", n.Sessions.NotFound)
	assert.NotEmpty(t, n.Sessions.NotOwned)
}

func TestRatemsg(t *testing.T) {
	n := NoticesConfig{}
	setNoticesDefaults(&n)

	msg := n.Ratemsg()
	assert.Equal(t, "hold on you're going to fast", msg)
}

func TestRatemsgMulti(t *testing.T) {
	n := NoticesConfig{
		Rate: RateNotices{
			Msgs: []string{"slow", "wait", "hold up"},
		},
	}
	setNoticesDefaults(&n)

	msg := n.Ratemsg()
	assert.Contains(t, []string{"slow", "wait", "hold up"}, msg)
}

func TestQueueMsg(t *testing.T) {
	n := NoticesConfig{}
	setNoticesDefaults(&n)

	msg := n.QueueMsg(3, 5*time.Second)
	assert.Equal(t, "\x0307⏳ Queued (position 3)\x0F", msg)
}

func TestQueueMsgCustom(t *testing.T) {
	n := NoticesConfig{
		Queue: QueueNotices{
			Msg: "waiting at #{position}, eta {eta}",
		},
	}
	setNoticesDefaults(&n)

	msg := n.QueueMsg(5, 2*time.Minute+30*time.Second)
	assert.Equal(t, "waiting at #5, eta 2m30s", msg)
}

func TestLoadNoticesFileMissing(t *testing.T) {
	dir, err := os.MkdirTemp("", "dave_test_notices_*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	var cfg Config
	err = loadNoticesFile(dir, &cfg)
	require.NoError(t, err)

	assert.Equal(t, "\x0307⏳ Queued (position {position})\x0F", cfg.Notices.Queue.Msg)
	assert.NotEmpty(t, cfg.Notices.Queue.Started)
}

func TestLoadNoticesFileOverride(t *testing.T) {
	dir, err := os.MkdirTemp("", "dave_test_notices_*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	noticesContent := `
[queue]
msg = "custom queued at {position}"

[rate]
msgs = ["custom rate msg"]

[sessions]
not_found = "nope"
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notices.toml"), []byte(noticesContent), 0644))

	var cfg Config
	err = loadNoticesFile(dir, &cfg)
	require.NoError(t, err)

	assert.Equal(t, "custom queued at {position}", cfg.Notices.Queue.Msg)
	assert.Equal(t, []string{"custom rate msg"}, cfg.Notices.Rate.Msgs)
	assert.Equal(t, "nope", cfg.Notices.Sessions.NotFound)
	assert.NotEmpty(t, cfg.Notices.Queue.Started)
	assert.NotEmpty(t, cfg.Notices.Sessions.NotOwned)
}

func TestLoadNoticesFileInvalid(t *testing.T) {
	dir, err := os.MkdirTemp("", "dave_test_notices_*")
	require.NoError(t, err)
	defer os.RemoveAll(dir)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "notices.toml"), []byte("invalid [toml"), 0644))

	var cfg Config
	err = loadNoticesFile(dir, &cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "loading notices")
}

func TestNoticesLoadedWithConfigDir(t *testing.T) {
	mainTOML := `
trigger = "!"
[[networks.test.servers]]
host = "irc.test.com"
`
	noticesTOML := `
[queue]
msg = "test queue {position}"
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"notices.toml": noticesTOML,
		"services.toml": `
[test]
key = "test"
`,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)

	assert.Equal(t, "test queue {position}", cfg.Notices.Queue.Msg)
	assert.NotEmpty(t, cfg.Notices.Queue.Started)
}

func TestNoticeDefaultsWithIntegration(t *testing.T) {
	mainTOML := `
trigger = "!"
[[networks.test.servers]]
host = "irc.test.com"
`
	dir := createTestConfigDir(t, mainTOML, map[string]string{
		"services.toml": `[test]
key = "test"
`,
	})
	defer os.RemoveAll(dir)

	cfg, err := loadConfigDir(dir)
	require.NoError(t, err)

	assert.Equal(t, "\x0307⏳ Queued (position {position})\x0F", cfg.Notices.Queue.Msg)
	assert.Equal(t, "hold on you're going to fast", cfg.Notices.Ratemsg())
	assert.Contains(t, cfg.Notices.Support, "patreon")
	assert.Equal(t, "database not available", cfg.Notices.DB.NotAvailable)
}

func TestErrorMsgUsesNotices(t *testing.T) {
	origError := noticeErrorPrefix.Load()
	origWarn := noticeWarnPrefix.Load()
	defer func() {
		noticeErrorPrefix.Store(origError)
		noticeWarnPrefix.Store(origWarn)
	}()

	noticeErrorPrefix.Store("\x0304❗ ")
	assert.Equal(t, "\x0304❗ test error", errorMsg("test error"))

	noticeWarnPrefix.Store("\x0307⚠️ ")
	assert.Equal(t, "\x0307⚠️ test warning", warnMsg("test warning"))
}

func TestQueueManagerWithNotices(t *testing.T) {
	n := NoticesConfig{}
	setNoticesDefaults(&n)

	qm := NewQueueManager(n, 3)
	assert.Equal(t, n.Queue.Msg, qm.queueMsg)
	assert.Equal(t, n.Queue.Started, qm.startedMsg)
	assert.Equal(t, 3, qm.maxDepth)
}

func TestQueueManagerUpdateNotices(t *testing.T) {
	n := NoticesConfig{}
	setNoticesDefaults(&n)

	qm := NewQueueManager(n, 5)

	n2 := NoticesConfig{
		Queue: QueueNotices{
			Msg:     "updated msg",
			Started: "updated started",
		},
	}
	setNoticesDefaults(&n2)
	qm.UpdateNotices(n2)

	assert.Equal(t, "updated msg", qm.queueMsg)
	assert.Equal(t, "updated started", qm.startedMsg)
}

func TestMentionNoticesDefaults(t *testing.T) {
	var n NoticesConfig
	setNoticesDefaults(&n)
	assert.Contains(t, n.Mentions.NoContext, "{help_url}")
	assert.Contains(t, n.Mentions.NoContext, "reply to my nick")
	assert.NotEmpty(t, n.Mentions.Muted)
}

func TestAllSessionTemplatesHavePlaceholders(t *testing.T) {
	n := NoticesConfig{}
	setNoticesDefaults(&n)

	tests := []struct {
		name     string
		tmpl     string
		vars     map[string]string
		expected string
	}{
		{
			name:     "header",
			tmpl:     n.Sessions.Header,
			vars:     map[string]string{"nick": "alice", "network": "testnet"},
			expected: "\x02Session History (alice on testnet):\x02",
		},
		{
			name:     "detail_header",
			tmpl:     n.Sessions.DetailHeader,
			vars:     map[string]string{"id": "42", "command": "chat", "count": "10", "archived_suffix": "", "cloned_from": ""},
			expected: "\x02Session #42 (chat) — 10 messages:\x02",
		},
		{
			name:     "detail_header_with_archived",
			tmpl:     n.Sessions.DetailHeader,
			vars:     map[string]string{"id": "42", "command": "chat", "count": "10", "archived_suffix": " (24 archived)", "cloned_from": ""},
			expected: "\x02Session #42 (chat) — 10 messages (24 archived):\x02",
		},
		{
			name:     "truncated",
			tmpl:     n.Sessions.Truncated,
			vars:     map[string]string{"count": "5"},
			expected: "  \x0314... (5 more) ...\x0F",
		},
		{
			name:     "deleted",
			tmpl:     n.Sessions.Deleted,
			vars:     map[string]string{"id": "42"},
			expected: "Deleted session #42.",
		},
		{
			name:     "paused",
			tmpl:     n.Sessions.Paused,
			vars:     map[string]string{"id": "7"},
			expected: "Paused your previous session #7.",
		},
		{
			name:     "resumed",
			tmpl:     n.Sessions.Resumed,
			vars:     map[string]string{"id": "3", "command": "ask", "count": "15"},
			expected: "Resumed session #3 (ask) with 15 messages.",
		},
		{
			name:     "switched",
			tmpl:     n.Sessions.Switched,
			vars:     map[string]string{"nick": "bob", "id": "10", "trigger": "!", "old_id": "5"},
			expected: "\x02Switched bob's session to #10\x02. Use !resume 5 to go back.",
		},
		{
			name:     "stats",
			tmpl:     n.Sessions.StatsFormat,
			vars:     map[string]string{"network": "net", "channel": "#chan", "sessions": "3", "messages": "50"},
			expected: "\x02Your stats on net/#chan:\x02 3 sessions, 50 total messages",
		},
		{
			name:     "history_usage",
			tmpl:     n.Sessions.HistoryUsage,
			vars:     map[string]string{"trigger": "!"},
			expected: "usage: !history <session-id>",
		},
		{
			name:     "image_limit",
			tmpl:     n.Images.LimitReached,
			vars:     map[string]string{"max": "5"},
			expected: "image limit reached (5 max in context), send text only",
		},
		{
			name:     "partial_limit",
			tmpl:     n.Images.PartialLimit,
			vars:     map[string]string{"remaining": "2", "used": "3", "max": "5"},
			expected: "only 2 more image(s) allowed in this context (3/5 used)",
		},
		{
			name:     "tool_call",
			tmpl:     n.Tools.Call,
			vars:     map[string]string{"server": "img-mcp", "tool": "generate"},
			expected: "\x0315🔧 ToolCall: img-mcp > generate",
		},
		{
			name:     "tool_failed",
			tmpl:     n.Tools.Failed,
			vars:     map[string]string{"error": "timeout"},
			expected: "MCP tool call failed: timeout",
		},
		{
			name:     "db_query_sessions",
			tmpl:     n.DB.QuerySessions,
			vars:     map[string]string{"error": "connection refused"},
			expected: "failed to query sessions: connection refused",
		},
		{
			name:     "pastebin_link",
			tmpl:     n.Pastebin.Link,
			vars:     map[string]string{"url": "https://paste.example.com/abc"},
			expected: "... ( full output: https://paste.example.com/abc )",
		},
		{
			name:     "job_line",
			tmpl:     n.Jobs.BgLine,
			vars:     map[string]string{"icon": "●", "job_id": "j123", "tool": "gen", "server": "img-mcp", "status": "running", "elapsed": "5m"},
			expected: fmt.Sprintf("  ● j123 [gen/img-mcp] running, 5m ago"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := expandNotice(tt.tmpl, tt.vars)
			assert.Equal(t, tt.expected, result)
		})
	}
}
