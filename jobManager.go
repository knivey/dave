package main

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var anthropicModelRe = regexp.MustCompile(`(?i)^anthropic/`)

func modelNeedsUserSuffix(model string) bool {
	return anthropicModelRe.MatchString(model)
}

var loggerJM = logxi.New("jobManager")

func init() {
	loggerJM.SetLevel(logxi.LevelAll)
}

// --- Generic job manager ---

type asyncJobPayload struct {
	sessionID      int64
	inlineResultCh chan string
}

type toolJobPayload struct {
	prompt      string
	submittedAt time.Time
}

type jobEntry[T any] struct {
	jobID     string
	payload   T
	toolName  string
	mcpServer string
	network   string
	channel   string
	nick      string
	userID    int64
	cancel    context.CancelFunc
}

type genericJobMgr[T any] struct {
	mu     sync.Mutex
	jobs   map[string]*jobEntry[T]
	ctx    context.Context
	cancel context.CancelFunc
}

func newGenericJobMgr[T any]() *genericJobMgr[T] {
	return &genericJobMgr[T]{
		jobs: make(map[string]*jobEntry[T]),
	}
}

func (m *genericJobMgr[T]) start() {
	m.ctx, m.cancel = context.WithCancel(context.Background())
}

func (m *genericJobMgr[T]) stop() {
	if m.cancel != nil {
		m.cancel()
	}
}

func (m *genericJobMgr[T]) contains(jobID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.jobs[jobID]
	return exists
}

func (m *genericJobMgr[T]) register(entry *jobEntry[T], waitFn func(context.Context, *jobEntry[T])) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.jobs[entry.jobID]; exists {
		return false
	}
	ctx, cancel := context.WithCancel(m.ctx)
	entry.cancel = cancel
	m.jobs[entry.jobID] = entry
	go waitFn(ctx, entry)
	return true
}

func (m *genericJobMgr[T]) remove(jobID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.jobs, jobID)
}

type jobWaitResult struct {
	resultText string
	err        error
	cancelled  bool
}

func (m *genericJobMgr[T]) waitForResult(ctx context.Context, entry *jobEntry[T]) jobWaitResult {
	result, err := waitForJobWithRetry(ctx, entry.jobID, entry.mcpServer)

	if ctx.Err() != nil {
		loggerJM.Info("job wait cancelled", "job_id", entry.jobID)
		return jobWaitResult{cancelled: true}
	}

	var resultText string
	if err != nil {
		resultText = fmt.Sprintf("error waiting for job: %s", err.Error())
		loggerJM.Error("job wait failed", "job_id", entry.jobID, "error", err)
	} else {
		resultText = mcpToolResultToText(result)
		loggerJM.Info("job completed", "job_id", entry.jobID, "result_len", len(resultText))
	}

	return jobWaitResult{resultText: resultText, err: err}
}

func (m *genericJobMgr[T]) cancelWhere(predicate func(*jobEntry[T]) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entry := range m.jobs {
		if !predicate(entry) {
			continue
		}
		loggerJM.Info("cancelling job", "job_id", entry.jobID)
		entry.cancel()
		if _, err := callMCPToolWithTimeout("cancel_job", map[string]any{
			"job_id": entry.jobID,
		}, 10*time.Second); err != nil {
			loggerJM.Warn("failed to cancel job in MCP server", "job_id", entry.jobID, "error", err)
		}
		delete(m.jobs, entry.jobID)
	}
}

// --- Shared helpers ---

type chatRunnerInterface interface {
	setChannel(channel, nick string, userID int64)
	runTurn(messages []ChatMessage) ([]ChatMessage, bool)
	setSessionInfo(sessionID int64, convID string)
}

var newChatRunnerFn = func(network Network, client *girc.Client, cfg AIConfig, ctx context.Context, output chan<- string) chatRunnerInterface {
	r := newChatRunner(network, client, cfg)
	r.ctx = ctx
	r.outputCh = output
	return r
}

var getBotFn = func(network string) *Bot {
	bot, _ := getBot(network)
	return bot
}

var botReadyFn = func(network, channel string) bool {
	bot := getBotFn(network)
	if bot == nil {
		return false
	}
	return bot.isReady(channel)
}

func isJobNotFoundError(result *mcp.CallToolResult, err error) bool {
	if err != nil {
		return strings.Contains(err.Error(), "not found")
	}
	if result != nil && result.IsError {
		text := mcpToolResultToText(result)
		return strings.Contains(text, "not found")
	}
	return false
}

func waitForJobWithRetry(ctx context.Context, jobID, mcpServer string) (*mcp.CallToolResult, error) {
	result, err := callMCPToolWithContext(ctx, "wait_for_job", map[string]any{
		"job_id": jobID,
	})

	if !isJobNotFoundError(result, err) {
		return result, err
	}

	if !checkMCPServerReady(mcpServer) {
		loggerJM.Info("job not found but MCP server not ready, retrying in 5s", "job_id", jobID, "server", mcpServer)
		select {
		case <-ctx.Done():
			return result, err
		case <-time.After(5 * time.Second):
		}
		return callMCPToolWithContext(ctx, "wait_for_job", map[string]any{
			"job_id": jobID,
		})
	}

	loggerJM.Warn("job not found and MCP server ready, job was lost", "job_id", jobID, "server", mcpServer)
	return result, err
}

// --- Async job manager (session-based LLM continuation) ---

var asyncJobMgr = newGenericJobMgr[asyncJobPayload]()

func startJobManager() {
	asyncJobMgr.start()
	loggerJM.Info("Job manager started")
}

func stopJobManager() {
	asyncJobMgr.stop()
	loggerJM.Info("Job manager stopped")
}

func cancelAsyncJobsForSession(sessionID int64) {
	asyncJobMgr.cancelWhere(func(e *jobEntry[asyncJobPayload]) bool {
		return e.payload.sessionID == sessionID
	})
}

func registerAsyncJob(jobID string, sessionID int64, toolName, mcpServer, network, channel, nick string, userID int64) *jobEntry[asyncJobPayload] {
	entry := &jobEntry[asyncJobPayload]{
		jobID: jobID,
		payload: asyncJobPayload{
			sessionID:      sessionID,
			inlineResultCh: make(chan string),
		},
		toolName:  toolName,
		mcpServer: mcpServer,
		network:   network,
		channel:   channel,
		nick:      nick,
		userID:    userID,
	}
	if !asyncJobMgr.register(entry, waitForAsyncJob) {
		loggerJM.Warn("job already registered", "job_id", jobID)
		return nil
	}
	loggerJM.Info("registered async job", "job_id", jobID, "server", mcpServer, "tool", toolName)
	return entry
}

func waitForAsyncJob(ctx context.Context, entry *jobEntry[asyncJobPayload]) {
	wr := asyncJobMgr.waitForResult(ctx, entry)
	if wr.cancelled {
		asyncJobMgr.remove(entry.jobID)
		return
	}

	select {
	case entry.payload.inlineResultCh <- wr.resultText:
		asyncJobMgr.remove(entry.jobID)
		if err := deliverInlinePendingJob(entry.jobID, wr.resultText); err != nil {
			loggerJM.Error("failed to deliver inline job in DB", "job_id", entry.jobID, "error", err)
		}
	default:
		onAsyncJobCompleted(entry, wr.resultText)
	}
}

func onAsyncJobCompleted(entry *jobEntry[asyncJobPayload], resultText string) {
	asyncJobMgr.remove(entry.jobID)

	if err := completePendingJob(entry.jobID, resultText); err != nil {
		loggerJM.Error("failed to mark job completed in DB", "job_id", entry.jobID, "error", err)
		return
	}

	queueMgr.Enqueue(entry.network, entry.channel, entry.userID, entry.nick, "", entry.toolName,
		func(ctx context.Context, output chan<- string) {
			deliverAsyncResult(entry, ctx, output)
		})
}

func deliverAsyncResult(entry *jobEntry[asyncJobPayload], ctx context.Context, output chan<- string) {
	activeSession, _ := sessionMgr.GetActiveSession(entry.network, entry.channel, entry.userID)

	if activeSession == nil || activeSession.ID != entry.payload.sessionID {
		if msg := switchToSession(entry); msg != "" {
			select {
			case output <- msg:
			case <-ctx.Done():
				return
			}
		}
	}

	bot := getBotFn(entry.network)
	if bot == nil || bot.Client == nil {
		loggerJM.Error("no IRC client for network", "network", entry.network)
		return
	}

	session, err := sessionMgr.GetSession(entry.payload.sessionID)
	if err != nil || session == nil {
		loggerJM.Warn("no session found, skipping LLM turn", "job_id", entry.jobID)
		return
	}

	var currentCfg AIConfig
	var cfgOk bool
	currentCfg, cfgOk = getSessionConfig(session)
	if !cfgOk {
		loggerJM.Error("chat command not found for session", "command", session.ChatCommand)
		return
	}

	runner := newChatRunnerFn(bot.Network, bot.Client, currentCfg, ctx, output)
	runner.setChannel(entry.channel, entry.nick, entry.userID)
	convID := ""
	if session.ConvID != nil {
		convID = *session.ConvID
	}
	runner.setSessionInfo(entry.payload.sessionID, convID)

	for {
		if ctx.Err() != nil {
			return
		}
		completedJobs, err := getCompletedPendingJobs(entry.payload.sessionID)
		if err != nil || len(completedJobs) == 0 {
			break
		}
		for _, cj := range completedJobs {
			injectAsyncResultFromDB(entry.payload.sessionID, currentCfg, cj, entry.network, entry.channel, entry.nick)
			markPendingJobDelivered(cj.JobID)
		}
		messages, _ := sessionMgr.GetMessages(entry.payload.sessionID, currentCfg.MaxHistory)
		var done bool
		messages, done = runner.runTurn(messages)
		if done {
			break
		}
	}
}

func switchToSession(entry *jobEntry[asyncJobPayload]) string {
	session, err := sessionMgr.GetSession(entry.payload.sessionID)
	if err != nil || session == nil {
		loggerJM.Error("failed to load session for switch", "id", entry.payload.sessionID, "error", err)
		return ""
	}

	var cfgOk bool
	_, cfgOk = getSessionConfig(session)
	if !cfgOk {
		loggerJM.Error("chat command not found for session", "command", session.ChatCommand)
		return ""
	}

	oldID, err := sessionMgr.SwitchActive(entry.network, entry.channel, entry.userID, entry.payload.sessionID)
	if err != nil {
		loggerJM.Error("failed to switch session", "error", err)
		return ""
	}

	apiLogger.RestoreSession(entry.payload.sessionID, entry.network, entry.channel, entry.userID)

	var switchMsg string
	if oldID != 0 {
		bot := getBotFn(entry.network)
		if bot != nil && bot.Client != nil {
			n := getNotices()
			switchMsg = expandNotice(n.Sessions.Switched, map[string]string{
				"nick":    entry.nick,
				"id":      fmt.Sprintf("%d", entry.payload.sessionID),
				"trigger": bot.Network.Trigger,
				"old_id":  fmt.Sprintf("%d", oldID),
			})
		}
	}

	loggerJM.Info("switched sessions", "from", oldID, "to", entry.payload.sessionID, "nick", entry.nick)
	return switchMsg
}

func injectAsyncResultFromDB(sessionID int64, cfg AIConfig, job PendingJob, network, channel, nick string) {
	resultText := ""
	if job.Result != nil {
		resultText = *job.Result
	}
	content := fmt.Sprintf("[System: Background task completed — tool: %s, job: %s. Result:\n%s]", job.ToolName, job.JobID, resultText)
	msg := ChatMessage{
		Role:    RoleSystem,
		Content: content,
	}
	sessionMgr.AddMessage(sessionID, msg)
	if cfg.NeedsUserSuffix || modelNeedsUserSuffix(cfg.Model) {
		userMsg := ChatMessage{
			Role:    RoleUser,
			Content: "Respond to the user based on the above background task result.",
		}
		sessionMgr.AddMessage(sessionID, userMsg)
	}
}

func recoverPendingJobs() {
	if theDB == nil {
		return
	}

	jobs, err := getPendingJobsForRecovery(true)
	if err != nil {
		loggerJM.Error("failed to query pending jobs for recovery", "error", err)
		return
	}

	for _, j := range jobs {
		if j.SessionID == nil {
			loggerJM.Warn("skipping tool job in session recovery", "job_id", j.JobID)
			continue
		}
		session, err := getDBSessionByID(*j.SessionID)
		if err != nil {
			loggerJM.Warn("skipping orphaned job", "job_id", j.JobID, "error", err)
			continue
		}
		var userID int64
		var nick string
		if session.UserID != nil {
			userID = *session.UserID
			if u, err := getUserByID(userID); err == nil {
				nick = displayNick(u)
			}
		}
		registerAsyncJob(j.JobID, *j.SessionID, j.ToolName, j.MCPServer, session.Network, session.Channel, nick, userID)
		loggerJM.Info("recovered pending job", "job_id", j.JobID)
	}

	if len(jobs) > 0 {
		loggerJM.Info("recovered pending jobs", "count", len(jobs))
	}
}

// --- Tool job manager (async image generation) ---

var toolJobMgr = newGenericJobMgr[toolJobPayload]()

func startToolJobManager() {
	toolJobMgr.start()
	loggerJM.Info("Tool job manager started")
}

func stopToolJobManager() {
	toolJobMgr.stop()
	loggerJM.Info("Tool job manager stopped")
}

func registerToolAsyncJob(jobID, toolName, mcpServer, network, channel, nick, prompt string, userID int64) {
	entry := &jobEntry[toolJobPayload]{
		jobID: jobID,
		payload: toolJobPayload{
			prompt:      prompt,
			submittedAt: time.Now(),
		},
		toolName:  toolName,
		mcpServer: mcpServer,
		network:   network,
		channel:   channel,
		nick:      nick,
		userID:    userID,
	}
	if !toolJobMgr.register(entry, waitForToolAsyncJob) {
		return
	}
	loggerJM.Info("registered tool async job", "job_id", jobID, "tool", toolName)

	if theDB != nil {
		if err := createToolPendingJob(jobID, toolName, mcpServer, network, channel, nick, userID); err != nil {
			loggerJM.Error("failed to persist tool job in DB", "job_id", jobID, "error", err)
		}
	}
}

func recoverToolAsyncJob(jobID, toolName, mcpServer, network, channel, nick, prompt string, userID int64) {
	entry := &jobEntry[toolJobPayload]{
		jobID: jobID,
		payload: toolJobPayload{
			prompt:      prompt,
			submittedAt: time.Now(),
		},
		toolName:  toolName,
		mcpServer: mcpServer,
		network:   network,
		channel:   channel,
		nick:      nick,
		userID:    userID,
	}
	if !toolJobMgr.register(entry, waitForToolAsyncJob) {
		loggerJM.Warn("recoverToolAsyncJob: duplicate job ID, skipping", "jobID", jobID)
	}
}

func waitForToolAsyncJob(ctx context.Context, entry *jobEntry[toolJobPayload]) {
	wr := toolJobMgr.waitForResult(ctx, entry)
	if wr.cancelled {
		toolJobMgr.remove(entry.jobID)
		return
	}

	onToolAsyncJobCompleted(entry, wr.resultText)
}

func onToolAsyncJobCompleted(entry *jobEntry[toolJobPayload], resultText string) {
	toolJobMgr.remove(entry.jobID)

	if theDB != nil {
		if err := completePendingJob(entry.jobID, resultText); err != nil {
			loggerJM.Error("failed to mark tool job completed in DB", "job_id", entry.jobID, "error", err)
		}
	}

	startedOverride := getNotices().Tools.ToolAsyncStarted
	queueMgr.EnqueueAtWithPrompt(entry.network, entry.channel, entry.userID, entry.nick, "", entry.toolName, entry.payload.prompt,
		startedOverride,
		entry.payload.submittedAt,
		func(ctx context.Context, output chan<- string) {
			deliverToolAsyncResult(entry, resultText, ctx, output)
		})
}

type waitForJobResult struct {
	JobID  string           `json:"job_id"`
	Status string           `json:"status"`
	Result *waitForJobInner `json:"result,omitempty"`
	Error  string           `json:"error,omitempty"`
}

type waitForJobInner struct {
	Images []mcpImageData `json:"images"`
}

func deliverToolAsyncResult(entry *jobEntry[toolJobPayload], resultText string, ctx context.Context, output chan<- string) {
	n := getNotices()
	var waitResult waitForJobResult
	if err := json.Unmarshal([]byte(resultText), &waitResult); err == nil {
		if waitResult.Error != "" {
			select {
			case output <- errorMsg(waitResult.Error):
			case <-ctx.Done():
			}
			return
		}
		if waitResult.Result != nil && len(waitResult.Result.Images) > 0 {
			for _, img := range waitResult.Result.Images {
				if img.URL != "" {
					select {
					case output <- img.URL:
					case <-ctx.Done():
						return
					}
				}
			}
		} else if waitResult.Status == StatusCompleted {
			select {
			case output <- expandNotice(n.Images.NoImages, map[string]string{"nick": entry.nick}):
			case <-ctx.Done():
			}
		} else {
			select {
			case output <- expandNotice(n.Images.JobStatus, map[string]string{
				"nick":   entry.nick,
				"status": waitResult.Status,
				"error":  waitResult.Error,
			}):
			case <-ctx.Done():
			}
		}
	} else {
		sendImageOrTextResult(resultText, ctx, output)
	}

	if theDB != nil {
		if err := markPendingJobDelivered(entry.jobID); err != nil {
			loggerJM.Error("failed to mark tool job delivered in DB", "job_id", entry.jobID, "error", err)
		}
	}
}

func cancelToolAsyncJobsForChannel(network, channel string) {
	toolJobMgr.cancelWhere(func(e *jobEntry[toolJobPayload]) bool {
		return e.network == network && e.channel == channel
	})
}

func recoverToolPendingJobs() {
	if theDB == nil {
		return
	}

	jobs, err := getPendingJobsForRecovery(false)
	if err != nil {
		loggerJM.Error("failed to query tool pending jobs for recovery", "error", err)
		return
	}

	for _, j := range jobs {
		network := ""
		channel := ""
		nick := ""
		var userID int64
		if j.Network != nil {
			network = *j.Network
		}
		if j.Channel != nil {
			channel = *j.Channel
		}
		if j.Nick != nil {
			nick = *j.Nick
		}
		if j.UserID != nil {
			userID = *j.UserID
		}

		if j.Result != nil {
			submittedAt := j.CreatedAt
			queueMgr.EnqueueAt(network, channel, userID, nick, "", j.ToolName, submittedAt,
				func(ctx context.Context, output chan<- string) {
					entry := &jobEntry[toolJobPayload]{
						jobID:     j.JobID,
						toolName:  j.ToolName,
						mcpServer: j.MCPServer,
						network:   network,
						channel:   channel,
						nick:      nick,
						userID:    userID,
					}
					deliverToolAsyncResult(entry, *j.Result, ctx, output)
				})
			loggerJM.Info("recovered completed tool job, enqueuing delivery", "job_id", j.JobID)
			continue
		}

		recoverToolAsyncJob(j.JobID, j.ToolName, j.MCPServer, network, channel, nick, "", userID)
		loggerJM.Info("recovered pending tool job", "job_id", j.JobID)
	}

	if len(jobs) > 0 {
		loggerJM.Info("recovered tool pending jobs", "count", len(jobs))
	}
}
