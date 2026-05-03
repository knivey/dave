package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var loggerJM = logxi.New("jobManager")

type asyncJob struct {
	JobID     string
	SessionID int64
	CtxKey    string
	ToolName  string
	MCPServer string
	Network   string
	Channel   string
	Nick      string
	cancel    context.CancelFunc
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
	setChannel(channel, nick string)
	runTurn(messages []ChatMessage) ([]ChatMessage, bool)
}

var newChatRunnerFn = func(network Network, client *girc.Client, cfg AIConfig, ctx context.Context, output chan<- string) chatRunnerInterface {
	r := newChatRunner(network, client, cfg)
	r.ctx = ctx
	r.outputCh = output
	return r
}

var getBotFn = func(network string) *Bot {
	return bots[network]
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

func registerAsyncJob(jobID string, sessionID int64, ctxKey, toolName, mcpServer, network, channel, nick string) {
	jobMgr.mu.Lock()
	defer jobMgr.mu.Unlock()

	if _, exists := jobMgr.jobs[jobID]; exists {
		loggerJM.Warn("job already registered", "job_id", jobID)
		return
	}

	ctx, cancel := context.WithCancel(jobMgr.ctx)
	job := &asyncJob{
		JobID:     jobID,
		SessionID: sessionID,
		CtxKey:    ctxKey,
		ToolName:  toolName,
		MCPServer: mcpServer,
		Network:   network,
		Channel:   channel,
		Nick:      nick,
		cancel:    cancel,
	}
	jobMgr.jobs[jobID] = job

	go waitForAsyncJob(ctx, job)
	loggerJM.Info("registered async job", "job_id", jobID, "server", mcpServer, "tool", toolName)
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

	onAsyncJobCompleted(job, resultText)
}

func onAsyncJobCompleted(job *asyncJob, resultText string) {
	jobMgr.mu.Lock()
	delete(jobMgr.jobs, job.JobID)
	jobMgr.mu.Unlock()

	if err := completePendingJob(job.JobID, resultText); err != nil {
		loggerJM.Error("failed to mark job completed in DB", "job_id", job.JobID, "error", err)
		return
	}

	queueMgr.Enqueue(job.Network, job.Channel, job.Nick, "", job.ToolName,
		func(ctx context.Context, output chan<- string) {
			deliverAsyncResult(job, ctx, output)
		})
}

func deliverAsyncResult(job *asyncJob, ctx context.Context, output chan<- string) {
	chatContextsMutex.Lock()
	currentCtx := chatContextsMap[job.CtxKey]
	currentSessionID := currentCtx.SessionID
	chatContextsMutex.Unlock()

	if currentSessionID != job.SessionID {
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

	chatContextsMutex.Lock()
	ctx2 := chatContextsMap[job.CtxKey]
	chatContextsMutex.Unlock()

	if ctx2.SessionID == 0 || len(ctx2.Messages) == 0 {
		loggerJM.Warn("no context, skipping LLM turn", "job_id", job.JobID)
		return
	}

	runner := newChatRunnerFn(bot.Network, bot.Client, ctx2.Config, ctx, output)
	runner.setChannel(job.Channel, job.Nick)

	for {
		if ctx.Err() != nil {
			return
		}
		completedJobs, err := getCompletedPendingJobs(ctx2.SessionID)
		if err != nil || len(completedJobs) == 0 {
			break
		}
		for _, cj := range completedJobs {
			injectAsyncResultFromDB(job.CtxKey, ctx2, cj, job.Network, job.Channel, job.Nick)
			markPendingJobDelivered(cj.JobID)
		}
		chatContextsMutex.Lock()
		messages := chatContextsMap[job.CtxKey].Messages
		chatContextsMutex.Unlock()
		var done bool
		messages, done = runner.runTurn(messages)
		if done {
			break
		}
	}
}

func switchToSession(job *asyncJob) string {
	chatContextsMutex.Lock()
	defer chatContextsMutex.Unlock()

	currentCtx := chatContextsMap[job.CtxKey]
	if currentCtx.SessionID == job.SessionID {
		return ""
	}

	if currentCtx.SessionID != 0 {
		if err := completeDBSession(currentCtx.SessionID); err != nil {
			loggerJM.Error("failed to complete old session", "id", currentCtx.SessionID, "error", err)
		}
	}

	session, err := getDBSessionByID(job.SessionID)
	if err != nil {
		loggerJM.Error("failed to load session for switch", "id", job.SessionID, "error", err)
		return ""
	}

	var currentCfg AIConfig
	var cfgOk bool
	readConfig(func() {
		currentCfg, cfgOk = config.Commands.Chats[session.ChatCommand]
	})
	if !cfgOk {
		loggerJM.Error("chat command not found for session", "command", session.ChatCommand)
		return ""
	}

	dbMsgs, err := loadDBSessionMessages(job.SessionID)
	if err != nil {
		loggerJM.Error("failed to load session messages", "id", job.SessionID, "error", err)
		return ""
	}

	var messages []ChatMessage
	for _, dm := range dbMsgs {
		msg := ChatMessage{
			Role:    dm.Role,
			Content: dm.Content,
		}
		if dm.ToolCallID != nil {
			msg.ToolCallID = *dm.ToolCallID
		}
		if dm.ReasoningContent != nil {
			msg.ReasoningContent = *dm.ReasoningContent
		}
		if dm.ToolCalls != nil {
			var toolCalls []ToolCall
			if err := json.Unmarshal([]byte(*dm.ToolCalls), &toolCalls); err == nil {
				msg.ToolCalls = toolCalls
			}
		}
		messages = append(messages, msg)
	}

	messages = TruncateHistory(messages, currentCfg.MaxHistory)
	chatContextsMap[job.CtxKey] = ChatContext{
		Messages:  messages,
		Config:    currentCfg,
		SessionID: job.SessionID,
	}
	apiLogger.RestoreSession(job.SessionID, job.CtxKey)

	if theDB != nil {
		theDB.Exec("UPDATE sessions SET status = 'active' WHERE id = ?", job.SessionID)
	}

	var switchMsg string
	if currentCtx.SessionID != 0 {
		bot := getBotFn(job.Network)
		if bot != nil && bot.Client != nil {
			switchMsg = fmt.Sprintf("\x02Switched %s's session to #%d\x02. Use %sresume %d to go back.",
				job.Nick, job.SessionID, bot.Network.Trigger, currentCtx.SessionID)
		}
	}

	loggerJM.Info("switched sessions", "from", currentCtx.SessionID, "to", job.SessionID, "nick", job.Nick)
	return switchMsg
}

func injectAsyncResultFromDB(ctxKey string, ctx ChatContext, job pendingJob, network, channel, nick string) {
	resultText := ""
	if job.Result != nil {
		resultText = *job.Result
	}
	content := fmt.Sprintf("[System: Background task completed — tool: %s, job: %s. Result:\n%s]", job.ToolName, job.JobID, resultText)
	msg := ChatMessage{
		Role:    RoleSystem,
		Content: content,
	}
	AddContext(ctx.Config, ctxKey, msg, network, channel, nick)
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
		registerAsyncJob(j.JobID, *j.SessionID, session.ContextKey, j.ToolName, j.MCPServer, session.Network, session.Channel, session.Nick)
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

func registerToolAsyncJob(jobID, toolName, mcpServer, network, channel, nick, prompt string) {
	if !registerToolAsyncJobMem(jobID, toolName, mcpServer, network, channel, nick, prompt) {
		return
	}

	if theDB != nil {
		if err := createToolPendingJob(jobID, toolName, mcpServer, network, channel, nick); err != nil {
			loggerJM.Error("failed to persist tool job in DB", "job_id", jobID, "error", err)
		}
	}
}

func recoverToolAsyncJob(jobID, toolName, mcpServer, network, channel, nick, prompt string) {
	registerToolAsyncJobMem(jobID, toolName, mcpServer, network, channel, nick, prompt)
}

func registerToolAsyncJobMem(jobID, toolName, mcpServer, network, channel, nick, prompt string) bool {
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

	queueMgr.EnqueueAt(job.Network, job.Channel, job.Nick, "", job.ToolName, job.SubmittedAt,
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
			header := fmt.Sprintf("\x02%s\x02's image", job.Nick)
			if job.Prompt != "" {
				promptDisplay := job.Prompt
				if len(promptDisplay) > 80 {
					promptDisplay = promptDisplay[:77] + "..."
				}
				header += fmt.Sprintf(" (\x0310%s\x0F)", promptDisplay)
			}
			header += ":"
			select {
			case output <- header:
			case <-ctx.Done():
				return
			}
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
			case output <- fmt.Sprintf("\x02%s\x02's image job completed but returned no images.", job.Nick):
			case <-ctx.Done():
			}
		} else {
			select {
			case output <- fmt.Sprintf("\x02%s\x02's image job %s: %s", job.Nick, waitResult.Status, waitResult.Error):
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
		if j.Network != nil {
			network = *j.Network
		}
		if j.Channel != nil {
			channel = *j.Channel
		}
		if j.Nick != nil {
			nick = *j.Nick
		}

		if j.Result != nil {
			submittedAt := time.Now()
			if t, err := time.Parse("2006-01-02 15:04:05", j.CreatedAt); err == nil {
				submittedAt = t
			}
			queueMgr.EnqueueAt(network, channel, nick, "", j.ToolName, submittedAt,
				func(ctx context.Context, output chan<- string) {
					job := &toolAsyncJob{
						JobID:     j.JobID,
						ToolName:  j.ToolName,
						MCPServer: j.MCPServer,
						Network:   network,
						Channel:   channel,
						Nick:      nick,
					}
					deliverToolAsyncResult(job, *j.Result, ctx, output)
				})
			loggerJM.Info("recovered completed tool job, enqueuing delivery", "job_id", j.JobID)
			continue
		}

		recoverToolAsyncJob(j.JobID, j.ToolName, j.MCPServer, network, channel, nick, "")
		loggerJM.Info("recovered pending tool job", "job_id", j.JobID)
	}

	if len(jobs) > 0 {
		loggerJM.Info("recovered tool pending jobs", "count", len(jobs))
	}
}
