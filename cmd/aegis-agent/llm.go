package main

import (
	"context"
	"encoding/json"
)

// Tool describes a tool available to the LLM.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"input_schema"`
}

// ToolCall is a tool invocation requested by the LLM.
type ToolCall struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// LLMResponse holds the result of a streaming LLM call.
// Text content is streamed via onDelta; only tool calls are returned here.
type LLMResponse struct {
	ToolCalls  []ToolCall
	RawContent interface{} // raw content blocks for session storage
}

// Message is a chat message sent to the LLM.
type Message struct {
	Role       string      `json:"role"`
	Content    interface{} `json:"content"`
	ToolCallID string      `json:"tool_call_id,omitempty"`
}

// LLM is the interface for LLM providers.
type LLM interface {
	// StreamChat calls the LLM with streaming. Text content is delivered via
	// onDelta in real-time. Returns tool calls (if any) after the stream ends.
	StreamChat(ctx context.Context, messages []Message, tools []Tool, onDelta func(string)) (*LLMResponse, error)
}
