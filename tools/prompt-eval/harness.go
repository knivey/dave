package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// wireMsg is one message of the chat-completions wire format, shaped like
// the rows dave sends in production.
type wireMsg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`

	// Reasoning history replay, mirroring dave's default "tool_calls"
	// replay mode: assistant rows that carry tool calls replay the real
	// reasoning_content so llama.cpp's preserve_thinking renders the
	// thought channel exactly like prod. Rows without tool calls never
	// replay reasoning.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFuncCall `json:"function"`
}

type wireFuncCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// chatRequest is the non-streaming chat-completions body, mirroring the
// prod eunuch request shape: tools + tool_choice "auto", reasoning_effort,
// and chat_template_kwargs riding alongside the model and messages.
type chatRequest struct {
	Model              string         `json:"model"`
	Messages           []wireMsg      `json:"messages"`
	Tools              []toolDef      `json:"tools,omitempty"`
	ToolChoice         string         `json:"tool_choice,omitempty"`
	ReasoningEffort    string         `json:"reasoning_effort,omitempty"`
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`

	// Temperature is the -temperature axis. Zero omits the field (the
	// server-default sampling prod also uses) — dave's own convention:
	// only a value > 0 is ever sent.
	Temperature float64 `json:"temperature,omitempty"`
}

// flexString accepts a JSON string (the spec shape llama-server emits for
// tool-call arguments) or a bare pre-encoded object, so one quirky gateway
// cannot abort the whole run at unmarshal time.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(string(b))
	return nil
}

type respToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string     `json:"name"`
		Arguments flexString `json:"arguments"`
	} `json:"function"`
}

type respMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content"`
	ToolCalls        []respToolCall `json:"tool_calls"`
}

type respChoice struct {
	Message      respMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type respBody struct {
	Choices []respChoice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

type parsedResp struct {
	content   string
	reasoning string
	toolCalls []wireToolCall
}

func parseChatResponse(body []byte) (parsedResp, error) {
	var pr parsedResp
	var rb respBody
	if err := json.Unmarshal(body, &rb); err != nil {
		return pr, fmt.Errorf("unmarshaling response: %w", err)
	}
	if rb.Error != nil {
		return pr, fmt.Errorf("server error: %s", rb.Error.Message)
	}
	if len(rb.Choices) == 0 {
		return pr, fmt.Errorf("response has no choices")
	}
	msg := rb.Choices[0].Message
	pr.content = msg.Content
	pr.reasoning = msg.ReasoningContent
	for _, tc := range msg.ToolCalls {
		typ := tc.Type
		if typ == "" {
			typ = "function"
		}
		pr.toolCalls = append(pr.toolCalls, wireToolCall{
			ID:   tc.ID,
			Type: typ,
			Function: wireFuncCall{
				Name:      tc.Function.Name,
				Arguments: string(tc.Function.Arguments),
			},
		})
	}
	return pr, nil
}

// responder abstracts one HTTP exchange: it receives the marshaled request
// body and returns the raw response body. The live implementation sleeps
// and POSTs; tests script it.
type responder func(reqBody []byte) (respBody []byte, err error)

// maxToolRounds is the per-turn tool-loop hard cap: after this many
// tool-call rounds the pending calls are answered and one tools-free final
// request forces a text answer.
const maxToolRounds = 8

// session drives one scripted run: a system prompt, a growing history, a
// responder, an api-log writer, and a seeded rng for fabrications.
type session struct {
	cfg       runConfig
	sysPrompt string
	msgs      []wireMsg
	respond   responder
	log       *jsonlWriter
	rng       *rand.Rand

	// curJobID tracks the async image job id fabricated during the
	// current turn so the delivery marker can reference the right job.
	curJobID string

	reqCount       int
	respCount      int
	reasoningChars int
	fabricated     bool

	// retryCount tallies retry re-sends (the CSV retries column —
	// server-flakiness bookkeeping per cell); reqCount above stays
	// one-per-logical-request, so a request that flaked twice and then
	// succeeded still reads requests=1, retries=2.
	retryCount int
}

func newSession(cfg runConfig, sysPrompt string, respond responder, w *jsonlWriter, rng *rand.Rand) *session {
	return &session{
		cfg:       cfg,
		sysPrompt: sysPrompt,
		msgs:      []wireMsg{{Role: "system", Content: sysPrompt}},
		respond:   respond,
		log:       w,
		rng:       rng,
	}
}

// buildRequest assembles the request for the current history. forceFinal
// drops the tool advertisement entirely — the universal "answer now" shape
// that also works on servers without tool_choice:"none" support.
func (s *session) buildRequest(forceFinal bool) chatRequest {
	req := chatRequest{
		Model:              s.cfg.model,
		Messages:           s.msgs,
		ReasoningEffort:    s.cfg.reasoningEffort,
		ChatTemplateKwargs: s.cfg.kwargs,
	}
	if !forceFinal {
		req.Tools = s.cfg.tools
		req.ToolChoice = "auto"
	}
	// dave's convention: the field rides only when set (> 0).
	if s.cfg.temperature > 0 {
		req.Temperature = s.cfg.temperature
	}
	return req
}

// request performs one HTTP exchange, logs both sides api-log style, and
// appends the assistant row to the history. A non-nil body returned
// alongside an error (an HTTP error status, say) is still logged before
// the error aborts the session.
//
// Transient-failure retries live HERE rather than deeper inside the
// responder because the failed-attempt error records need the session's
// JSONL writer: a retryable failure — HTTP >= 500 (the router in front of
// the llama-server intermittently answers "proxy error: Failed to read
// connection", which killed ~5.5% of sessions in the 70-session sweep) or
// a transport-level error, never a 4xx (our fault) or a client timeout
// (see retryableCallErr) — re-sends the
// IDENTICAL marshaled body up to cfg.retries times with exponential
// backoff, so the session continues when a request eventually succeeds.
// Each failed attempt leaves one response-type, choices-less error record
// in the JSONL (the shape the analysis scripts already skip) plus one
// stdout line; the request record itself is logged once, unchanged. The
// body is marshaled once above precisely so a retry cannot drift from the
// original request — the seeded determinism streams stay untouched.
func (s *session) request(forceFinal bool) (parsedResp, error) {
	var pr parsedResp
	body, err := json.Marshal(s.buildRequest(forceFinal))
	if err != nil {
		return pr, err
	}
	s.reqCount++
	if err := s.log.logExchange("request", body); err != nil {
		return pr, fmt.Errorf("logging request: %w", err)
	}
	raw, callErr := s.respond(body)
	for attempt := 0; callErr != nil && attempt < s.cfg.retries && retryableCallErr(callErr); attempt++ {
		if err := s.logFailedAttempt(raw, callErr); err != nil {
			return pr, fmt.Errorf("logging failed attempt: %w", err)
		}
		s.retryCount++
		fmt.Printf("retry %d/%d after %s: %v\n", attempt+1, s.cfg.retries, failureLabel(callErr), callErr)
		time.Sleep(retryBackoff(s.cfg.retryWait, attempt))
		raw, callErr = s.respond(body)
	}
	if len(raw) > 0 {
		s.respCount++
		if err := s.log.logExchange("response", raw); err != nil {
			return pr, fmt.Errorf("logging response: %w", err)
		}
	}
	if callErr != nil {
		return pr, callErr
	}
	pr, err = parseChatResponse(raw)
	if err != nil {
		return pr, err
	}
	if len(pr.reasoning) > 0 {
		s.reasoningChars += len(pr.reasoning)
	}
	if fabricationDetected(pr.content) {
		s.fabricated = true
	}
	am := wireMsg{Role: "assistant", Content: pr.content}
	if len(pr.toolCalls) > 0 {
		am.ToolCalls = pr.toolCalls
		if pr.reasoning != "" {
			am.ReasoningContent = pr.reasoning
		}
	}
	s.msgs = append(s.msgs, am)
	return pr, nil
}

// logFailedAttempt writes one JSONL error record for a failed exchange
// that is about to be retried: the server's error body when it sent one
// (the same response-type, choices-less record the final-failure path
// writes — the analysis scripts already skip those), or a synthesized
// error object for transport-level failures that produced no body at all.
func (s *session) logFailedAttempt(raw []byte, callErr error) error {
	body := raw
	if len(body) == 0 {
		b, err := json.Marshal(map[string]string{"error": callErr.Error()})
		if err != nil {
			return fmt.Errorf("marshaling retry error record: %w", err)
		}
		body = b
	}
	return s.log.logExchange("response", body)
}

// answerToolCalls appends one fabricated tool result per call, exactly how
// dave appends one RoleTool row per call.
func (s *session) answerToolCalls(calls []wireToolCall) {
	for _, tc := range calls {
		result, jobID := fabricateToolResult(tc.Function.Name, s.rng)
		if jobID != "" {
			s.curJobID = jobID
		}
		s.msgs = append(s.msgs, wireMsg{Role: "tool", ToolCallID: tc.ID, Content: result})
	}
}

// toolLoop drives rounds of tool calls until the model produces a
// no-tool-call final. Hard cap of maxToolRounds per turn: at the cap the
// pending calls are answered (history stays well-formed for later turns)
// and one tools-free request forces the final text.
func (s *session) toolLoop() error {
	for round := 0; ; round++ {
		pr, err := s.request(false)
		if err != nil {
			return err
		}
		if len(pr.toolCalls) == 0 {
			return nil
		}
		s.answerToolCalls(pr.toolCalls)
		if round == maxToolRounds-1 {
			_, err := s.request(true)
			return err
		}
	}
}

// userTurn appends a user message and runs the tool loop to a final.
func (s *session) userTurn(content string) error {
	s.curJobID = ""
	s.msgs = append(s.msgs, wireMsg{Role: "user", Content: content})
	return s.toolLoop()
}

// deliveryTurn injects the async delivery marker as a message whose wire
// role comes from -delivery-role — "system" is the exact prod guidance
// mechanism (message mode, injection_role system); "user"/"developer" are
// the A/B axis (does a user-role delivery stop the model forging the
// marker itself?). The marker TEXT is the prod format regardless of role.
func (s *session) deliveryTurn(marker string) error {
	s.curJobID = ""
	s.msgs = append(s.msgs, wireMsg{Role: s.cfg.deliveryRole, Content: marker})
	return s.toolLoop()
}

// resetHistory drops the conversation back to the bare system prompt (the
// prompts scenario is a fresh one-shot exchange per prompt).
func (s *session) resetHistory() {
	s.msgs = []wireMsg{{Role: "system", Content: s.sysPrompt}}
}

// httpError marks an HTTP-level failure (non-200) so the retry path can
// tell transient 5xx (the router's proxy drops) from permanent 4xx (the
// request's own fault) without string matching. It carries the raw body so
// the existing log-and-abort behavior is unchanged.
type httpError struct {
	status int
	url    string
	raw    []byte
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d from %s: %.400s", e.status, e.url, e.raw)
}

// retryableCallErr reports whether a failed exchange is worth re-sending:
// HTTP >= 500 or a transport-level error (connection reset, EOF). A 4xx is
// the caller's fault, deliberate cancellation is deliberate, and a client
// timeout has already burned the request's full time budget — none of
// those are retried.
func retryableCallErr(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	// Client timeouts never retry, in either shape they surface: a bare
	// net.Error with Timeout()=true, or the *url.Error that
	// http.Client.Do wraps around one (it satisfies net.Error itself and
	// delegates Timeout to the cause, so a single errors.As covers both).
	// Retries target the observed failure class — instant 5xx and
	// connection resets, the router's "Failed to read connection" drops —
	// where a re-send is cheap and usually lands. A 5-minute timeout
	// retried N times instead stalls the sweep ~20 minutes on a single
	// request, queues duplicate generations behind the server's
	// parallel=1, and a generation that outlives the 5-minute budget is a
	// degenerate sample anyway.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	var he *httpError
	if errors.As(err, &he) {
		return he.status >= 500
	}
	return true
}

// failureLabel names what went wrong for the retry stdout line ("500",
// "transport error").
func failureLabel(err error) string {
	var he *httpError
	if errors.As(err, &he) {
		return strconv.Itoa(he.status)
	}
	return "transport error"
}

// maxRetryBackoff caps the exponential curve: a large -retries value must
// never overflow the duration or park the sweep for days — the server is
// shared with a live bot.
const maxRetryBackoff = 5 * time.Minute

// retryBackoff returns the sleep after the (attempt+1)-th failed exchange:
// the base doubled per attempt — 2s, 4s, 8s with the default base.
func retryBackoff(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	d := base
	for i := 0; i < attempt; i++ {
		next := d * 2
		if next < d || next > maxRetryBackoff {
			return maxRetryBackoff
		}
		d = next
	}
	return d
}

// newLiveResponder returns the real HTTP responder: POST
// {baseurl}/chat/completions, non-streaming, sleeping cfg.delay between
// consecutive requests. The server is shared with a live bot, so the
// harness stays sequential and polite — retry re-sends (one layer up, in
// session.request) pass through here too and therefore also wait the
// politeness delay on top of their backoff. Error-status bodies are
// returned alongside a typed *httpError so they still land in the JSONL
// and the retry path can classify them.
func newLiveResponder(cfg runConfig) responder {
	client := &http.Client{Timeout: 5 * time.Minute}
	url := strings.TrimSuffix(cfg.baseURL, "/") + "/chat/completions"
	first := true
	return func(body []byte) ([]byte, error) {
		if !first {
			time.Sleep(cfg.delay)
		}
		first = false
		req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if cfg.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.apiKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			return raw, err
		}
		if resp.StatusCode != http.StatusOK {
			return raw, &httpError{status: resp.StatusCode, url: url, raw: raw}
		}
		return raw, nil
	}
}
