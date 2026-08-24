// Package backend holds the OpenAI-compatible chat completions client
// shared by every replay/routing backend (ollama, together, gemini,
// openai), translation between that format and the Anthropic Messages API,
// and a separate minimal native Anthropic client used only by eval runs.
package backend

import "encoding/json"

// ChatRequest is the subset of the OpenAI chat completions request body
// splitter sends. It is common across ollama, together, gemini and openai
// since all four expose an OpenAI-compatible /chat/completions endpoint.
type ChatRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Tools       []ChatTool    `json:"tools,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
}

// ChatMessage is one entry of ChatRequest.Messages or ChatResponse's choice
// message. Content is always a plain string (Anthropic content blocks are
// flattened into it by ToOpenAI); ToolCalls is populated only on assistant
// messages that invoke a tool; ToolCallID is populated only on role "tool"
// messages, naming the call the result answers.
type ChatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []ChatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// ChatToolCall is one entry of an assistant ChatMessage.ToolCalls.
type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ChatToolCallFunc `json:"function"`
}

// ChatToolCallFunc is the function payload of a ChatToolCall. Arguments is
// a JSON-encoded string (the wire format OpenAI-compatible APIs use), not a
// nested object.
type ChatToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatTool is one entry of ChatRequest.Tools, the OpenAI function-calling
// tool shape.
type ChatTool struct {
	Type     string       `json:"type"`
	Function ChatFunction `json:"function"`
}

// ChatFunction describes one callable tool. Parameters carries the JSON
// schema object verbatim.
type ChatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ChatResponse is the subset of an OpenAI chat completions response body
// splitter reads.
type ChatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   ChatUsage    `json:"usage"`
}

// ChatChoice is one entry of ChatResponse.Choices. Only choice 0 is used
// (splitter never requests n>1).
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

// ChatUsage is the OpenAI chat completions token accounting block.
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
