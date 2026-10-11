package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
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
	return req
}

// request performs one HTTP exchange, logs both sides api-log style, and
// appends the assistant row to the history. A non-nil body returned
// alongside an error (an HTTP error status, say) is still logged before
// the error aborts the session.
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

// deliveryTurn injects the async delivery marker as a system-role message —
// the exact prod guidance mechanism (message mode, injection_role system) —
// and runs to a final.
func (s *session) deliveryTurn(marker string) error {
	s.curJobID = ""
	s.msgs = append(s.msgs, wireMsg{Role: "system", Content: marker})
	return s.toolLoop()
}

// resetHistory drops the conversation back to the bare system prompt (the
// prompts scenario is a fresh one-shot exchange per prompt).
func (s *session) resetHistory() {
	s.msgs = []wireMsg{{Role: "system", Content: s.sysPrompt}}
}

// newLiveResponder returns the real HTTP responder: POST
// {baseurl}/chat/completions, non-streaming, sleeping cfg.delay between
// consecutive requests. The server is shared with a live bot, so the
// harness stays sequential and polite. Error-status bodies are returned
// alongside the error so they still land in the JSONL.
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
			return raw, fmt.Errorf("HTTP %d from %s: %.400s", resp.StatusCode, url, raw)
		}
		return raw, nil
	}
}
