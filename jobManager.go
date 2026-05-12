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

type asyncJob struct {
	JobID          string
	SessionID      int64
	ToolName       string
	MCPServer      string
	Network        string
	Channel        string
	Nick           string
	UserID         int64
	cancel         context.CancelFunc
	inlineResultCh chan string
}

var jobMgr struct {
	mu     sync.Mutex
	jobs   map[string]*asyncJob
	ctx    context.Context
	cancel context.CancelFunc
}

func init() {
	loggerJM.SetLevel(logxi.LevelAll)
	jobMgr.jobs = make(map[string]*asyncJob)
}

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

func startJobManager() {
	jobMgr.ctx, jobMgr.cancel = context.WithCancel(context.Background())
	loggerJM.Info("Job manager started")
}

func stopJobManager() {
	if jobMgr.cancel != nil {
		jobMgr.cancel()
		loggerJM.Info("Job manager stopped")
	}
}

func cancelAsyncJobsForSession(sessionID int64) {
	jobMgr.mu.Lock()
	defer jobMgr.mu.Unlock()

	for _, job := range jobMgr.jobs {
		if job.SessionID != sessionID {
			continue
		}
		loggerJM.Info("cancelling async job for session", "job_id", job.JobID, "session_id", sessionID)
		job.cancel()
		if _, err := callMCPToolWithTimeout("cancel_job", map[string]any{
			"job_id": job.JobID,
		}, 10*time.Second); err != nil {
			loggerJM.Warn("failed to cancel job in MCP server", "job_id", job.JobID, "error", err)
		}
		delete(jobMgr.jobs, job.JobID)
	}
}

func registerAsyncJob(jobID string, sessionID int64, toolName, mcpServer, network, channel, nick string, userID int64) *asyncJob {
	jobMgr.mu.Lock()
	defer jobMgr.mu.Unlock()

	if _, exists := jobMgr.jobs[jobID]; exists {
		loggerJM.Warn("job already registered", "job_id", jobID)
		return nil
	}

	ctx, cancel := context.WithCancel(jobMgr.ctx)
	job := &asyncJob{
		JobID:          jobID,
		SessionID:      sessionID,
		ToolName:       toolName,
		MCPServer:      mcpServer,
		Network:        network,
		Channel:        channel,
		Nick:           nick,
		UserID:         userID,
		cancel:         cancel,
		inlineResultCh: make(chan string),
	}
	jobMgr.jobs[jobID] = job

	go waitForAsyncJob(ctx, job)
	loggerJM.Info("registered async job", "job_id", jobID, "server", mcpServer, "tool", toolName)
	return job
}

func waitForAsyncJob(ctx context.Context, job *asyncJob) {
	result, err := waitForJobWithRetry(ctx, job.JobID, job.MCPServer)

	if ctx.Err() != nil {
		jobMgr.mu.Lock()
		delete(jobMgr.jobs, job.JobID)
		jobMgr.mu.Unlock()
		loggerJM.Info("job wait cancelled", "job_id", job.JobID)
		return
	}

	var resultText string
	if err != nil {
		resultText = fmt.Sprintf("error waiting for job: %s", err.Error())
		loggerJM.Error("job wait failed", "job_id", job.JobID, "error", err)
	} else {
		resultText = mcpToolResultToText(result)
		loggerJM.Info("job completed", "job_id", job.JobID, "result_len", len(resultText))
	}

	select {
	case job.inlineResultCh <- resultText:
		jobMgr.mu.Lock()
		delete(jobMgr.jobs, job.JobID)
		jobMgr.mu.Unlock()
		if err := deliverInlinePendingJob(job.JobID, resultText); err != nil {
			loggerJM.Error("failed to deliver inline job in DB", "job_id", job.JobID, "error", err)
		}
	default:
		onAsyncJobCompleted(job, resultText)
	}
}

func onAsyncJobCompleted(job *asyncJob, resultText string) {
	jobMgr.mu.Lock()
	delete(jobMgr.jobs, job.JobID)
	jobMgr.mu.Unlock()

	if err := completePendingJob(job.JobID, resultText); err != nil {
		loggerJM.Error("failed to mark job completed in DB", "job_id", job.JobID, "error", err)
		return
	}

	queueMgr.Enqueue(job.Network, job.Channel, job.UserID, job.Nick, "", job.ToolName,
		func(ctx context.Context, output chan<- string) {
			deliverAsyncResult(job, ctx, output)
		})
}

func deliverAsyncResult(job *asyncJob, ctx context.Context, output chan<- string) {
	activeSession, _ := sessionMgr.GetActiveSession(job.Network, job.Channel, job.UserID)

	if activeSession == nil || activeSession.ID != job.SessionID {
		if msg := switchToSession(job); msg != "" {
			select {
			case output <- msg:
			case <-ctx.Done():
				return
			}
		}
	}

	bot := getBotFn(job.Network)
	if bot == nil || bot.Client == nil {
		loggerJM.Error("no IRC client for network", "network", job.Network)
		return
	}

	session, err := sessionMgr.GetSession(job.SessionID)
	if err != nil || session == nil {
		loggerJM.Warn("no session found, skipping LLM turn", "job_id", job.JobID)
		return
	}

	var currentCfg AIConfig
	var cfgOk bool
	readConfig(func() {
		currentCfg, cfgOk = config.Commands.Chats[session.ChatCommand]
	})
	if session.SettingsID != nil {
		settings, err := sessionMgr.GetSessionSettings(*session.SettingsID)
		if err != nil {
			loggerJM.Warn("failed to load stored settings", "error", err)
		} else if settings != nil {
			currentCfg = ApplySettings(settings, currentCfg)
			cfgOk = true
		}
	}
	if !cfgOk {
		loggerJM.Error("chat command not found for session", "command", session.ChatCommand)
		return
	}

	runner := newChatRunnerFn(bot.Network, bot.Client, currentCfg, ctx, output)
	runner.setChannel(job.Channel, job.Nick, job.UserID)
	convID := ""
	if session.ConvID != nil {
		convID = *session.ConvID
	}
	runner.setSessionInfo(job.SessionID, convID)

	for {
		if ctx.Err() != nil {
			return
		}
		completedJobs, err := getCompletedPendingJobs(job.SessionID)
		if err != nil || len(completedJobs) == 0 {
			break
		}
		for _, cj := range completedJobs {
			injectAsyncResultFromDB(job.SessionID, currentCfg, cj, job.Network, job.Channel, job.Nick)
			markPendingJobDelivered(cj.JobID)
		}
		messages, _ := sessionMgr.GetMessages(job.SessionID, currentCfg.MaxHistory)
		var done bool
		messages, done = runner.runTurn(messages)
		if done {
			break
		}
	}
}

func switchToSession(job *asyncJob) string {
	session, err := sessionMgr.GetSession(job.SessionID)
	if err != nil || session == nil {
		loggerJM.Error("failed to load session for switch", "id", job.SessionID, "error", err)
		return ""
	}

	var cfgOk bool
	if session.SettingsID != nil {
		settings, cfgErr := sessionMgr.GetSessionSettings(*session.SettingsID)
		if cfgErr != nil {
			loggerJM.Warn("failed to load stored settings for switch validation", "error", cfgErr)
		}
		cfgOk = settings != nil
	}
	if !cfgOk {
		readConfig(func() {
			_, cfgOk = config.Commands.Chats[session.ChatCommand]
		})
	}
	if !cfgOk {
		loggerJM.Error("chat command not found for session", "command", session.ChatCommand)
		return ""
	}

	oldID, err := sessionMgr.SwitchActive(job.Network, job.Channel, job.UserID, job.SessionID)
	if err != nil {
		loggerJM.Error("failed to switch session", "error", err)
		return ""
	}

	apiLogger.RestoreSession(job.SessionID, job.Network, job.Channel, job.UserID)

	var switchMsg string
	if oldID != 0 {
		bot := getBotFn(job.Network)
		if bot != nil && bot.Client != nil {
			n := getNotices()
			switchMsg = expandNotice(n.Sessions.Switched, map[string]string{
				"nick":    job.Nick,
				"id":      fmt.Sprintf("%d", job.SessionID),
				"trigger": bot.Network.Trigger,
				"old_id":  fmt.Sprintf("%d", oldID),
			})
		}
	}

	loggerJM.Info("switched sessions", "from", oldID, "to", job.SessionID, "nick", job.Nick)
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

	jobs, err := getPendingJobsForRecovery()
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
				nick = u.CurrentNick
			}
		}
		registerAsyncJob(j.JobID, *j.SessionID, j.ToolName, j.MCPServer, session.Network, session.Channel, nick, userID)
		loggerJM.Info("recovered pending job", "job_id", j.JobID)
	}

	if len(jobs) > 0 {
		loggerJM.Info("recovered pending jobs", "count", len(jobs))
	}
}

type toolAsyncJob struct {
	JobID       string
	ToolName    string
	MCPServer   string
	Network     string
	Channel     string
	Nick        string
	UserID      int64
	Prompt      string
	SubmittedAt time.Time
	cancel      context.CancelFunc
}

var toolJobMgr struct {
	mu     sync.Mutex
	jobs   map[string]*toolAsyncJob
	ctx    context.Context
	cancel context.CancelFunc
}

func init() {
	toolJobMgr.jobs = make(map[string]*toolAsyncJob)
}

func startToolJobManager() {
	toolJobMgr.ctx, toolJobMgr.cancel = context.WithCancel(context.Background())
	loggerJM.Info("Tool job manager started")
}

func stopToolJobManager() {
	if toolJobMgr.cancel != nil {
		toolJobMgr.cancel()
		loggerJM.Info("Tool job manager stopped")
	}
}

func registerToolAsyncJob(jobID, toolName, mcpServer, network, channel, nick, prompt string, userID int64) {
	if !registerToolAsyncJobMem(jobID, toolName, mcpServer, network, channel, nick, prompt, userID) {
		return
	}

	if theDB != nil {
		if err := createToolPendingJob(jobID, toolName, mcpServer, network, channel, nick, userID); err != nil {
			loggerJM.Error("failed to persist tool job in DB", "job_id", jobID, "error", err)
		}
	}
}

func recoverToolAsyncJob(jobID, toolName, mcpServer, network, channel, nick, prompt string, userID int64) {
	registerToolAsyncJobMem(jobID, toolName, mcpServer, network, channel, nick, prompt, userID)
}

func registerToolAsyncJobMem(jobID, toolName, mcpServer, network, channel, nick, prompt string, userID int64) bool {
	toolJobMgr.mu.Lock()
	defer toolJobMgr.mu.Unlock()

	if _, exists := toolJobMgr.jobs[jobID]; exists {
		loggerJM.Warn("tool job already registered", "job_id", jobID)
		return false
	}

	ctx, cancel := context.WithCancel(toolJobMgr.ctx)
	job := &toolAsyncJob{
		JobID:       jobID,
		ToolName:    toolName,
		MCPServer:   mcpServer,
		Network:     network,
		Channel:     channel,
		Nick:        nick,
		UserID:      userID,
		Prompt:      prompt,
		SubmittedAt: time.Now(),
		cancel:      cancel,
	}
	toolJobMgr.jobs[jobID] = job

	go waitForToolAsyncJob(ctx, job)
	loggerJM.Info("registered tool async job", "job_id", jobID, "tool", toolName)
	return true
}

func waitForToolAsyncJob(ctx context.Context, job *toolAsyncJob) {
	result, err := waitForJobWithRetry(ctx, job.JobID, job.MCPServer)

	if ctx.Err() != nil {
		loggerJM.Info("tool job wait cancelled", "job_id", job.JobID)
		return
	}

	var resultText string
	if err != nil {
		resultText = fmt.Sprintf("error waiting for job: %s", err.Error())
		loggerJM.Error("tool job wait failed", "job_id", job.JobID, "error", err)
	} else {
		resultText = mcpToolResultToText(result)
		loggerJM.Info("tool job completed", "job_id", job.JobID, "result_len", len(resultText))
	}

	onToolAsyncJobCompleted(job, resultText)
}

func onToolAsyncJobCompleted(job *toolAsyncJob, resultText string) {
	toolJobMgr.mu.Lock()
	delete(toolJobMgr.jobs, job.JobID)
	toolJobMgr.mu.Unlock()

	if theDB != nil {
		if err := completeToolPendingJob(job.JobID, resultText); err != nil {
			loggerJM.Error("failed to mark tool job completed in DB", "job_id", job.JobID, "error", err)
		}
	}

	startedOverride := getNotices().Tools.ToolAsyncStarted
	queueMgr.EnqueueAtWithPrompt(job.Network, job.Channel, job.UserID, job.Nick, "", job.ToolName, job.Prompt,
		startedOverride,
		job.SubmittedAt,
		func(ctx context.Context, output chan<- string) {
			deliverToolAsyncResult(job, resultText, ctx, output)
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

func deliverToolAsyncResult(job *toolAsyncJob, resultText string, ctx context.Context, output chan<- string) {
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
		} else if waitResult.Status == "completed" {
			select {
			case output <- expandNotice(n.Images.NoImages, map[string]string{"nick": job.Nick}):
			case <-ctx.Done():
			}
		} else {
			select {
			case output <- expandNotice(n.Images.JobStatus, map[string]string{
				"nick":   job.Nick,
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
		if err := markPendingJobDelivered(job.JobID); err != nil {
			loggerJM.Error("failed to mark tool job delivered in DB", "job_id", job.JobID, "error", err)
		}
	}
}

func cancelToolAsyncJobsForChannel(network, channel string) {
	toolJobMgr.mu.Lock()
	defer toolJobMgr.mu.Unlock()

	for _, job := range toolJobMgr.jobs {
		if job.Network == network && job.Channel == channel {
			loggerJM.Info("cancelling tool async job", "job_id", job.JobID, "channel", channel)
			job.cancel()
			if _, err := callMCPToolWithTimeout("cancel_job", map[string]any{
				"job_id": job.JobID,
			}, 10*time.Second); err != nil {
				loggerJM.Warn("failed to cancel job in MCP server", "job_id", job.JobID, "error", err)
			}
		}
	}
}

func recoverToolPendingJobs() {
	if theDB == nil {
		return
	}

	jobs, err := getToolPendingJobsForRecovery()
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
					job := &toolAsyncJob{
						JobID:     j.JobID,
						ToolName:  j.ToolName,
						MCPServer: j.MCPServer,
						Network:   network,
						Channel:   channel,
						Nick:      nick,
						UserID:    userID,
					}
					deliverToolAsyncResult(job, *j.Result, ctx, output)
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
