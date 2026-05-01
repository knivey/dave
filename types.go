package main

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

const (
	PartTypeText     = "text"
	PartTypeImageURL = "image_url"
)

const (
	ImageDetailAuto = "auto"
	ImageDetailLow  = "low"
	ImageDetailHigh = "high"
)

type ChatMessage struct {
	Role             string
	Content          string
	ReasoningContent string
	MultiContent     []MessagePart
	ToolCalls        []ToolCall
	ToolCallID       string
	Name             string
}

type MessagePart struct {
	Type     string
	Text     string
	ImageURL *ImageURL
}

type ImageURL struct {
	URL    string
	Detail string
}

type ToolCall struct {
	Index    int
	ID       string
	Type     string
	Function FunctionCall
}

type FunctionCall struct {
	Name      string
	Arguments string
}

type Tool struct {
	Type     string
	Function *FunctionDefinition
}

type FunctionDefinition struct {
	Name        string
	Description string
	Parameters  any
}

type Usage struct {
	PromptTokens            int64
	CompletionTokens        int64
	TotalTokens             int64
	PromptTokensDetails     *PromptTokensDetails
	CompletionTokensDetails *CompletionTokensDetails
}

type PromptTokensDetails struct {
	CachedTokens int64
}

type CompletionTokensDetails struct {
	ReasoningTokens int64
}

const (
	summarizeContentLen = 50
	summarizeHeadTail   = 3
)

type messageTurn struct {
	start int
	end   int
}

func buildTurns(messages []ChatMessage) []messageTurn {
	if len(messages) == 0 {
		return nil
	}
	var turns []messageTurn
	start := 0
	for i, m := range messages {
		if i > 0 && m.Role == RoleUser {
			turns = append(turns, messageTurn{start: start, end: i})
			start = i
		}
	}
	turns = append(turns, messageTurn{start: start, end: len(messages)})
	return turns
}

func summarizeMessages(messages []ChatMessage) string {
	truncate := func(s string) string {
		s = strings.ReplaceAll(s, "\n", " ")
		s = strings.TrimSpace(s)
		if utf8.RuneCountInString(s) > summarizeContentLen {
			runes := []rune(s)
			return string(runes[:summarizeContentLen]) + "..."
		}
		return s
	}

	formatMsg := func(i int, m ChatMessage) string {
		var content string
		if len(m.MultiContent) > 0 {
			parts := make([]string, 0, len(m.MultiContent))
			for _, p := range m.MultiContent {
				if p.Type == PartTypeImageURL {
					parts = append(parts, "[image]")
				} else {
					parts = append(parts, truncate(p.Text))
				}
			}
			content = strings.Join(parts, ", ")
		} else {
			content = truncate(m.Content)
		}
		s := fmt.Sprintf("#%d %s: %s", i, m.Role, content)
		if len(m.ToolCalls) > 0 {
			s += fmt.Sprintf(" [tool_calls: %d]", len(m.ToolCalls))
		}
		if m.ReasoningContent != "" {
			s += " [reasoning]"
		}
		return s
	}

	formatTurn := func(t messageTurn) string {
		parts := make([]string, 0, t.end-t.start)
		for i := t.start; i < t.end; i++ {
			parts = append(parts, formatMsg(i, messages[i]))
		}
		return strings.Join(parts, " + ")
	}

	n := len(messages)
	turns := buildTurns(messages)
	nt := len(turns)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[%d messages, %d turns]", n, nt))

	if nt <= summarizeHeadTail*2 {
		for i, t := range turns {
			sb.WriteString(" | Turn ")
			sb.WriteString(strconv.Itoa(i))
			sb.WriteString(": ")
			sb.WriteString(formatTurn(t))
		}
		return sb.String()
	}

	for i := 0; i < summarizeHeadTail; i++ {
		sb.WriteString(" | Turn ")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(": ")
		sb.WriteString(formatTurn(turns[i]))
	}
	omittedFirst := summarizeHeadTail
	omittedLast := nt - summarizeHeadTail - 1
	sb.WriteString(fmt.Sprintf(" | ... %d turns (#%d-#%d) omitted ...", omittedLast-omittedFirst+1, omittedFirst, omittedLast))
	for i := nt - summarizeHeadTail; i < nt; i++ {
		sb.WriteString(" | Turn ")
		sb.WriteString(strconv.Itoa(i))
		sb.WriteString(": ")
		sb.WriteString(formatTurn(turns[i]))
	}
	return sb.String()
}
