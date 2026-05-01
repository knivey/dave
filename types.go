package main

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
