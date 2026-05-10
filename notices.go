package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
)

type NoticesConfig struct {
	Queue    QueueNotices    `toml:"queue"`
	Rate     RateNotices     `toml:"rate"`
	Format   FormatNotices   `toml:"format"`
	Context  ContextNotices  `toml:"context"`
	Sessions SessionNotices  `toml:"sessions"`
	DB       DBNotices       `toml:"db"`
	Images   ImageNotices    `toml:"images"`
	Tools    ToolNotices     `toml:"tools"`
	Pastebin PastebinNotices `toml:"pastebin"`
	Jobs     JobNotices      `toml:"jobs"`
	Bans     BanNotices      `toml:"bans"`
	Support  string          `toml:"support"`
}

type QueueNotices struct {
	Msg            string `toml:"msg"`
	Started        string `toml:"started"`
	AsyncSubmitted string `toml:"async_submitted"`
	Stopped        string `toml:"stopped"`
}

type RateNotices struct {
	Msgs []string `toml:"msgs"`
}

type FormatNotices struct {
	ErrorPrefix string `toml:"error_prefix"`
	WarnPrefix  string `toml:"warn_prefix"`
}

type ContextNotices struct {
	NoContext string `toml:"no_context"`
}

type SessionNotices struct {
	Header       string `toml:"header"`
	None         string `toml:"none"`
	DetailHeader string `toml:"detail_header"`
	Truncated    string `toml:"truncated"`
	NotFound     string `toml:"not_found"`
	NotOwned     string `toml:"not_owned"`
	InvalidID    string `toml:"invalid_id"`
	NoMessages   string `toml:"no_messages"`
	CommandGone  string `toml:"command_gone"`
	Deleted      string `toml:"deleted"`
	Paused       string `toml:"paused"`
	Resumed      string `toml:"resumed"`
	Switched     string `toml:"switched"`
	StatsFormat  string `toml:"stats_format"`
	HistoryUsage string `toml:"history_usage"`
	DeleteUsage  string `toml:"delete_usage"`
	ResumeUsage  string `toml:"resume_usage"`
}

type DBNotices struct {
	NotAvailable   string `toml:"not_available"`
	QuerySessions  string `toml:"query_sessions"`
	LoadMessages   string `toml:"load_messages"`
	DeleteFailed   string `toml:"delete_failed"`
	QueryStats     string `toml:"query_stats"`
	QueryJobs      string `toml:"query_jobs"`
	ProcessImages  string `toml:"process_images"`
	PastebinUpload string `toml:"pastebin_upload"`
	InternalError  string `toml:"internal_error"`
}

type ImageNotices struct {
	LimitReached string `toml:"limit_reached"`
	PartialLimit string `toml:"partial_limit"`
	NoImages     string `toml:"no_images"`
	JobStatus    string `toml:"job_status"`
}

type ToolNotices struct {
	Call             string `toml:"call"`
	CallMulti        string `toml:"call_multi"`
	CallLimit        string `toml:"call_limit"`
	Failed           string `toml:"failed"`
	Usage            string `toml:"usage"`
	Unexpected       string `toml:"unexpected"`
	ToolAsyncStarted string `toml:"tool_async_started"`
}

type PastebinNotices struct {
	Link   string `toml:"link"`
	Failed string `toml:"failed"`
}

type JobNotices struct {
	NoJobs       string `toml:"no_jobs"`
	QueueHeader  string `toml:"queue_header"`
	QueueRunning string `toml:"queue_running"`
	QueuePending string `toml:"queue_pending"`
	BgHeader     string `toml:"bg_header"`
	BgLine       string `toml:"bg_line"`
}

type BanNotices struct {
	BanCreated    string `toml:"ban_created"`
	BanList       string `toml:"ban_list"`
	BanListEmpty  string `toml:"ban_list_empty"`
	BanHistory    string `toml:"ban_history"`
	Unbanned      string `toml:"unbanned"`
	UserNotFound  string `toml:"user_not_found"`
	AmIBanned     string `toml:"amibanned"`
	AmIBannedNone string `toml:"amibanned_none"`
}

var (
	noticeErrorPrefix atomic.Value
	noticeWarnPrefix  atomic.Value
)

func init() {
	noticeErrorPrefix.Store("\x0304❗ ")
	noticeWarnPrefix.Store("\x0308⚠️ ")
}

func setNoticesDefaults(n *NoticesConfig) {
	if n.Queue.Msg == "" {
		n.Queue.Msg = "\x0308⏳ Queued (position {position})\x0F"
	}
	if n.Queue.Started == "" {
		n.Queue.Started = "\x0306\u25b6 {nick}: Processing your request (waited {wait})...{prompt}\x0f"
	}
	if n.Queue.AsyncSubmitted == "" {
		n.Queue.AsyncSubmitted = "\x0303🎨 Generating image for \x02{nick}\x02... I'll send the result when it's ready."
	}
	if n.Queue.Stopped == "" {
		n.Queue.Stopped = "\x0315⏹ Generation stopped."
	}
	if len(n.Rate.Msgs) == 0 {
		n.Rate.Msgs = []string{"hold on you're going to fast"}
	}
	if n.Format.ErrorPrefix == "" {
		n.Format.ErrorPrefix = "\x0304❗ "
	}
	if n.Format.WarnPrefix == "" {
		n.Format.WarnPrefix = "\x0308⚠️ "
	}
	if n.Context.NoContext == "" {
		n.Context.NoContext = "you dont have a chat context, start one with one of my many fabulous chat commands. After starting, just reply to my nick to continue the conversation"
	}
	if n.Sessions.Header == "" {
		n.Sessions.Header = "\x02Session History ({nick} on {network}):\x02"
	}
	if n.Sessions.None == "" {
		n.Sessions.None = "No session history found."
	}
	if n.Sessions.DetailHeader == "" {
		n.Sessions.DetailHeader = "\x02Session #{id} ({command}) — {count} messages:\x02"
	}
	if n.Sessions.Truncated == "" {
		n.Sessions.Truncated = "  \x0314... ({count} more) ...\x0F"
	}
	if n.Sessions.NotFound == "" {
		n.Sessions.NotFound = "session not found"
	}
	if n.Sessions.NotOwned == "" {
		n.Sessions.NotOwned = "that session doesn't belong to you"
	}
	if n.Sessions.InvalidID == "" {
		n.Sessions.InvalidID = "invalid session id"
	}
	if n.Sessions.NoMessages == "" {
		n.Sessions.NoMessages = "session has no messages"
	}
	if n.Sessions.CommandGone == "" {
		n.Sessions.CommandGone = "chat command {command} no longer exists, cannot resume"
	}
	if n.Sessions.Deleted == "" {
		n.Sessions.Deleted = "Deleted session #{id}."
	}
	if n.Sessions.Paused == "" {
		n.Sessions.Paused = "Paused your previous session #{id}."
	}
	if n.Sessions.Resumed == "" {
		n.Sessions.Resumed = "Resumed session #{id} ({command}) with {count} messages."
	}
	if n.Sessions.Switched == "" {
		n.Sessions.Switched = "\x02Switched {nick}'s session to #{id}\x02. Use {trigger}resume {old_id} to go back."
	}
	if n.Sessions.StatsFormat == "" {
		n.Sessions.StatsFormat = "\x02Your stats on {network}/{channel}:\x02 {sessions} sessions, {messages} total messages"
	}
	if n.Sessions.HistoryUsage == "" {
		n.Sessions.HistoryUsage = "usage: {trigger}history <session-id>"
	}
	if n.Sessions.DeleteUsage == "" {
		n.Sessions.DeleteUsage = "usage: {trigger}delete <session-id>"
	}
	if n.Sessions.ResumeUsage == "" {
		n.Sessions.ResumeUsage = "usage: {trigger}resume <session-id>"
	}
	if n.DB.NotAvailable == "" {
		n.DB.NotAvailable = "database not available"
	}
	if n.DB.QuerySessions == "" {
		n.DB.QuerySessions = "failed to query sessions: {error}"
	}
	if n.DB.LoadMessages == "" {
		n.DB.LoadMessages = "failed to load messages: {error}"
	}
	if n.DB.DeleteFailed == "" {
		n.DB.DeleteFailed = "failed to delete session: {error}"
	}
	if n.DB.QueryStats == "" {
		n.DB.QueryStats = "failed to query stats: {error}"
	}
	if n.DB.QueryJobs == "" {
		n.DB.QueryJobs = "failed to query jobs: {error}"
	}
	if n.DB.ProcessImages == "" {
		n.DB.ProcessImages = "failed to process images: {error}"
	}
	if n.DB.PastebinUpload == "" {
		n.DB.PastebinUpload = "pastebin: {error}"
	}
	if n.DB.InternalError == "" {
		n.DB.InternalError = "internal error: {error}"
	}
	if n.Images.LimitReached == "" {
		n.Images.LimitReached = "image limit reached ({max} max in context), send text only"
	}
	if n.Images.PartialLimit == "" {
		n.Images.PartialLimit = "only {remaining} more image(s) allowed in this context ({used}/{max} used)"
	}
	if n.Images.NoImages == "" {
		n.Images.NoImages = "\x02{nick}\x02's image job completed but returned no images."
	}
	if n.Images.JobStatus == "" {
		n.Images.JobStatus = "\x02{nick}\x02's image job {status}: {error}"
	}
	if n.Tools.Call == "" {
		n.Tools.Call = "\x0315🔧 ToolCall: {server} > {tool}"
	}
	if n.Tools.CallMulti == "" {
		n.Tools.CallMulti = "\x0315🔧 ToolCalls: {tools}\x0F"
	}
	if n.Tools.CallLimit == "" {
		n.Tools.CallLimit = "\x0308⚠️ Tool call limit reached, stopping.\x0F"
	}
	if n.Tools.Failed == "" {
		n.Tools.Failed = "MCP tool call failed: {error}"
	}
	if n.Tools.Usage == "" {
		n.Tools.Usage = "Usage: <{arg}>"
	}
	if n.Tools.Unexpected == "" {
		n.Tools.Unexpected = "Failed to submit async job: unexpected response"
	}
	if n.Tools.ToolAsyncStarted == "" {
		n.Tools.ToolAsyncStarted = "\x0306\u25b6 {nick}: Processed your image request (waited {wait})...{prompt}\x0f"
	}
	if n.Pastebin.Link == "" {
		n.Pastebin.Link = "... ( full output: {url} )"
	}
	if n.Pastebin.Failed == "" {
		n.Pastebin.Failed = "... (full output could not be pasted)"
	}
	if n.Jobs.NoJobs == "" {
		n.Jobs.NoJobs = "No active jobs or queue items."
	}
	if n.Jobs.QueueHeader == "" {
		n.Jobs.QueueHeader = "\x02Queue:\x02"
	}
	if n.Jobs.QueueRunning == "" {
		n.Jobs.QueueRunning = "  \x0303▶\x0F {desc} ({elapsed} elapsed)"
	}
	if n.Jobs.QueuePending == "" {
		n.Jobs.QueuePending = "  \x0308{position}.\x0F {desc} (waiting {wait})"
	}
	if n.Jobs.BgHeader == "" {
		n.Jobs.BgHeader = "\x02Background jobs:\x02"
	}
	if n.Jobs.BgLine == "" {
		n.Jobs.BgLine = "  {icon} {job_id} [{tool}/{server}] {status}, {elapsed} ago"
	}
	if n.Bans.BanCreated == "" {
		n.Bans.BanCreated = "\x0304🚫 Banned {nick} for {duration}: {reason}\x0F"
	}
	if n.Bans.BanList == "" {
		n.Bans.BanList = "\x02{nick}\x02 — {reason} (expires {expires})"
	}
	if n.Bans.BanListEmpty == "" {
		n.Bans.BanListEmpty = "No active bans."
	}
	if n.Bans.BanHistory == "" {
		n.Bans.BanHistory = "#{id} {active} {reason} ({duration}) by {banner}"
	}
	if n.Bans.Unbanned == "" {
		n.Bans.Unbanned = "\x0303✓ Unbanned {nick}\x0F"
	}
	if n.Bans.UserNotFound == "" {
		n.Bans.UserNotFound = "User {nick} not found."
	}
	if n.Bans.AmIBanned == "" {
		n.Bans.AmIBanned = "\x0304🚫 Banned: {reason} (expires in {remaining}, by {banner})\x0F"
	}
	if n.Bans.AmIBannedNone == "" {
		n.Bans.AmIBannedNone = "You are not currently banned."
	}
	if n.Support == "" {
		n.Support = "If you enjoy using dave, consider supporting development at https://patreon.com/shrew269 ❤️"
	}

	noticeErrorPrefix.Store(n.Format.ErrorPrefix)
	noticeWarnPrefix.Store(n.Format.WarnPrefix)
}

func expandNotice(tmpl string, vars map[string]string) string {
	s := tmpl
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

func loadNoticesFile(dir string, config *Config) error {
	path := filepath.Join(dir, "notices.toml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		setNoticesDefaults(&config.Notices)
		return nil
	}
	if _, err := toml.DecodeFile(path, &config.Notices); err != nil {
		return fmt.Errorf("loading notices: %w", err)
	}
	setNoticesDefaults(&config.Notices)
	return nil
}

func (n *NoticesConfig) Ratemsg() string {
	return n.Rate.Msgs[globalRand.Intn(len(n.Rate.Msgs))]
}

func (n *NoticesConfig) QueueMsg(position int, eta time.Duration) string {
	s := n.Queue.Msg
	s = strings.ReplaceAll(s, "{position}", fmt.Sprintf("%d", position))
	s = strings.ReplaceAll(s, "{eta}", eta.Round(time.Second).String())
	return s
}
