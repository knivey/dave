package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/lrstanley/girc"
	logxi "github.com/mgutz/logxi/v1"
	gogpt "github.com/sashabaranov/go-openai"
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
	runTurn(messages []gogpt.ChatCompletionMessage) ([]gogpt.ChatCompletionMessage, bool)
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
	result, err := callMCPToolWithContext(ctx, "wait_for_job", map[string]any{
		"job_id": job.JobID,
	})

	if ctx.Err() != nil {
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

	if currentSessionID != 0 && currentSessionID != job.SessionID {
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

	currentCfg, ok := config.Commands.Chats[session.ChatCommand]
	if !ok {
		loggerJM.Error("chat command not found for session", "command", session.ChatCommand)
		return ""
	}

	dbMsgs, err := loadDBSessionMessages(job.SessionID)
	if err != nil {
		loggerJM.Error("failed to load session messages", "id", job.SessionID, "error", err)
		return ""
	}

	var messages []gogpt.ChatCompletionMessage
	for _, dm := range dbMsgs {
		msg := gogpt.ChatCompletionMessage{
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
			var toolCalls []gogpt.ToolCall
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

	if theDB != nil {
		theDB.Exec("UPDATE sessions SET status = 'active' WHERE id = ?", job.SessionID)
	}

	var switchMsg string
	bot := getBotFn(job.Network)
	if bot != nil && bot.Client != nil {
		var oldID int64
		if currentCtx.SessionID != 0 {
			oldID = currentCtx.SessionID
		}
		switchMsg = fmt.Sprintf("\x02Switched to session #%d\x02. Use %sresume %d to go back.",
			job.SessionID, bot.Network.Trigger, oldID)
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
	msg := gogpt.ChatCompletionMessage{
		Role:    gogpt.ChatMessageRoleSystem,
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
		session, err := getDBSessionByID(j.SessionID)
		if err != nil {
			loggerJM.Warn("skipping orphaned job", "job_id", j.JobID, "error", err)
			continue
		}
		registerAsyncJob(j.JobID, j.SessionID, session.ContextKey, j.ToolName, j.MCPServer, session.Network, session.Channel, session.Nick)
		loggerJM.Info("recovered pending job", "job_id", j.JobID)
	}

	if len(jobs) > 0 {
		loggerJM.Info("recovered pending jobs", "count", len(jobs))
	}
}
