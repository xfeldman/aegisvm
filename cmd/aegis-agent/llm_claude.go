package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ClaudeLLM implements LLM using the Anthropic Messages API with streaming.
type ClaudeLLM struct {
	apiKey string
}

func (c *ClaudeLLM) StreamChat(ctx context.Context, messages []Message, tools []Tool, onDelta func(string)) (*LLMResponse, error) {
	var system string
	var chatMessages []interface{}

	for _, m := range messages {
		if m.Role == "system" {
			if s, ok := m.Content.(string); ok {
				system = s
			}
			continue
		}
		msg := map[string]interface{}{"role": m.Role}
		switch v := m.Content.(type) {
		case string:
			if m.Role == "tool" {
				msg["role"] = "user"
				msg["content"] = []map[string]interface{}{
					{"type": "tool_result", "tool_use_id": m.ToolCallID, "content": v},
				}
			} else {
				msg["content"] = v
			}
		default:
			msg["content"] = v
		}
		chatMessages = append(chatMessages, msg)
	}

	body := map[string]interface{}{
		"model":      "claude-sonnet-4-20250514",
		"max_tokens": 4096,
		"stream":     true,
		"messages":   chatMessages,
	}
	if system != "" {
		body["system"] = system
	}
	if len(tools) > 0 {
		var ct []map[string]interface{}
		for _, t := range tools {
			ct = append(ct, map[string]interface{}{
				"name": t.Name, "description": t.Description, "input_schema": t.InputSchema,
			})
		}
		body["tools"] = ct
	}

	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("claude API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("claude API %d: %s", resp.StatusCode, string(body))
	}

	return parseClaudeStream(resp.Body, onDelta)
}

func parseClaudeStream(r io.Reader, onDelta func(string)) (*LLMResponse, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)

	llmResp := &LLMResponse{}
	var contentBlocks []interface{}
	var currentToolID, currentToolName string
	var toolInputBuf strings.Builder

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}

		switch event.Type {
		case "content_block_start":
			if event.ContentBlock.Type == "tool_use" {
				currentToolID = event.ContentBlock.ID
				currentToolName = event.ContentBlock.Name
				toolInputBuf.Reset()
			}

		case "content_block_delta":
			if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
				onDelta(event.Delta.Text)
			} else if event.Delta.Type == "input_json_delta" && event.Delta.PartialJSON != "" {
				toolInputBuf.WriteString(event.Delta.PartialJSON)
			}

		case "content_block_stop":
			if currentToolName != "" {
				inputJSON := json.RawMessage(toolInputBuf.String())
				if len(inputJSON) == 0 {
					inputJSON = json.RawMessage("{}")
				}
				llmResp.ToolCalls = append(llmResp.ToolCalls, ToolCall{
					ID: currentToolID, Name: currentToolName, Input: inputJSON,
				})
				contentBlocks = append(contentBlocks, map[string]interface{}{
					"type": "tool_use", "id": currentToolID,
					"name": currentToolName, "input": json.RawMessage(toolInputBuf.String()),
				})
				currentToolName = ""
				currentToolID = ""
			}
		}
	}

	llmResp.RawContent = contentBlocks
	return llmResp, scanner.Err()
}
