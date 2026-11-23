package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ---- OpenAI Responses API (/v1/responses) ----

type responsesRequest struct {
	Model             string          `json:"model"`
	Input             json.RawMessage `json:"input"`
	Instructions      string          `json:"instructions"`
	Stream            bool            `json:"stream"`
	Tools             []responsesTool `json:"tools"`
	ToolChoice        any             `json:"tool_choice"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls"`
}

type responsesTool struct {
	Type        string        `json:"type"`
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Parameters  any           `json:"parameters"`
	Function    *ToolFunction `json:"function,omitempty"`
}

type responsesItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Text      string          `json:"text"`
}

// responsesContentParts decodes a Responses content field, which is either a
// plain string or an array of typed parts.
func responsesContentParts(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be a string or an array of parts")
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		typ, _ := part["type"].(string)
		switch typ {
		case "input_text", "output_text", "text":
			if t, ok := part["text"].(string); ok && t != "" {
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		case "input_image":
			if url := imagePartURL(part); url != "" {
				out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		case "refusal":
			if t, ok := part["refusal"].(string); ok && t != "" {
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		default:
			if t, ok := part["text"].(string); ok && t != "" {
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		}
	}
	if len(out) == 0 {
		return "", nil
	}
	if len(out) == 1 {
		if m, ok := out[0].(map[string]any); ok && m["type"] == "text" {
			return m["text"], nil
		}
	}
	return out, nil
}

// responsesToolChoice maps the Responses tool_choice to the chat form.
func responsesToolChoice(v any) any {
	switch choice := v.(type) {
	case string:
		return choice
	case map[string]any:
		if name, ok := choice["name"].(string); ok && strings.TrimSpace(name) != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
		if fn, ok := choice["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok && strings.TrimSpace(name) != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		}
		if typ, ok := choice["type"].(string); ok {
			switch typ {
			case "function":
				return "auto"
			default:
				return typ
			}
		}
	}
	return nil
}

// chatFromResponses converts a Responses API request body into the internal
// chat request so the existing pipeline (tool bridge, retries, sessions) can
// serve it.
func chatFromResponses(body []byte) (*ChatRequest, error) {
	var req responsesRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	out := &ChatRequest{
		Model:             req.Model,
		Stream:            req.Stream,
		ToolChoice:        responsesToolChoice(req.ToolChoice),
		ParallelToolCalls: req.ParallelToolCalls,
	}
	if strings.TrimSpace(req.Instructions) != "" {
		out.Messages = append(out.Messages, Message{Role: "system", Content: req.Instructions})
	}

	switch {
	case len(req.Input) == 0:
		// No input: rejected below so the caller gets a clear error.
	case bytes.HasPrefix(bytes.TrimSpace(req.Input), []byte(`"`)):
		var text string
		if err := json.Unmarshal(req.Input, &text); err != nil {
			return nil, fmt.Errorf("input must be a string or an array of items")
		}
		if strings.TrimSpace(text) != "" {
			out.Messages = append(out.Messages, Message{Role: "user", Content: text})
		}
	case bytes.HasPrefix(bytes.TrimSpace(req.Input), []byte("[")):
		var items []responsesItem
		if err := json.Unmarshal(req.Input, &items); err != nil {
			return nil, fmt.Errorf("input items could not be decoded: %v", err)
		}
		for _, item := range items {
			switch item.Type {
			case "function_call":
				name := strings.TrimSpace(item.Name)
				if name == "" {
					continue
				}
				id := strings.TrimSpace(item.CallID)
				if id == "" {
					id = "call_" + strings.TrimSpace(uuid())
				}
				out.Messages = append(out.Messages, Message{
					Role: "assistant",
					ToolCalls: []ToolCall{{
						ID:   id,
						Type: "function",
						Function: FunctionCall{
							Name:      name,
							Arguments: argumentString(item.Arguments),
						},
					}},
				})
			case "function_call_output":
				id := strings.TrimSpace(item.CallID)
				content, err := responsesContentParts(item.Output)
				if err != nil {
					content = string(item.Output)
				}
				out.Messages = append(out.Messages, Message{
					Role:       "tool",
					ToolCallID: id,
					Content:    content,
				})
			case "reasoning":
				// Upstream keeps its own reasoning; nothing to forward.
			default:
				role := strings.ToLower(strings.TrimSpace(item.Role))
				if role == "" {
					role = "user"
				}
				if role == "developer" {
					role = "system"
				}
				content, err := responsesContentParts(item.Content)
				if err != nil {
					return nil, err
				}
				if item.Text != "" && !hasVisibleText(contentText(content)) {
					content = item.Text
				}
				out.Messages = append(out.Messages, Message{Role: role, Content: content})
			}
		}
	default:
		return nil, fmt.Errorf("input must be a string or an array of items")
	}

	for _, tool := range req.Tools {
		name := strings.TrimSpace(tool.Name)
		description := tool.Description
		parameters := tool.Parameters
		if tool.Function != nil {
			if n := strings.TrimSpace(tool.Function.Name); n != "" {
				name = n
			}
			if tool.Function.Description != "" {
				description = tool.Function.Description
			}
			if tool.Function.Parameters != nil {
				parameters = tool.Function.Parameters
			}
		}
		switch tool.Type {
		case "", "function":
			if name == "" {
				continue
			}
			out.Tools = append(out.Tools, Tool{
				Type: "function",
				Function: ToolFunction{
					Name:        name,
					Description: description,
					Parameters:  parameters,
				},
			})
		default:
			// Built-in tools (web_search, file_search, ...) have no chat
			// equivalent and are dropped instead of failing the request.
		}
	}

	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("input must contain at least one message")
	}
	return out, nil
}

// ---- Anthropic Messages API (/v1/messages) ----

type anthropicRequest struct {
	Model         string             `json:"model"`
	System        json.RawMessage    `json:"system"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    any                `json:"tool_choice"`
	MaxTokens     int                `json:"max_tokens"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	StopSequences []string           `json:"stop_sequences"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"input_schema"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	// image
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// anthropicSystemText flattens the system field, which is a string or an array
// of text blocks.
func anthropicSystemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// anthropicImagePart turns an Anthropic image source into an OpenAI part.
func anthropicImagePart(block anthropicBlock) (any, bool) {
	if block.Source == nil {
		return nil, false
	}
	switch block.Source.Type {
	case "base64":
		if block.Source.Data == "" {
			return nil, false
		}
		media := block.Source.MediaType
		if media == "" {
			media = "image/png"
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + media + ";base64," + block.Source.Data},
		}, true
	case "url":
		if block.Source.URL == "" {
			return nil, false
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": block.Source.URL},
		}, true
	}
	return nil, false
}

// anthropicToolResultText flattens a tool_result content field.
func anthropicToolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "image":
			parts = append(parts, "[image]")
		default:
			if block.Text != "" {
				parts = append(parts, block.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicToolChoice maps the Anthropic tool_choice to the chat form.
func anthropicToolChoice(v any) any {
	choice, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	typ, _ := choice["type"].(string)
	switch typ {
	case "auto":
		return "auto"
	case "none":
		return "none"
	case "any":
		return "required"
	case "tool":
		if name, ok := choice["name"].(string); ok && strings.TrimSpace(name) != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return nil
}

// chatFromAnthropic converts an Anthropic Messages request body into the
// internal chat request.
func chatFromAnthropic(body []byte) (*ChatRequest, error) {
	var req anthropicRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil {
		return nil, err
	}
	out := &ChatRequest{
		Model:      req.Model,
		Stream:     req.Stream,
		ToolChoice: anthropicToolChoice(req.ToolChoice),
	}
	if system := anthropicSystemText(req.System); strings.TrimSpace(system) != "" {
		out.Messages = append(out.Messages, Message{Role: "system", Content: system})
	}

	for _, msg := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(msg.Role))
		if role == "" {
			role = "user"
		}
		// Plain string content is the common case.
		var plain string
		if len(msg.Content) > 0 && json.Unmarshal(msg.Content, &plain) == nil {
			if role == "assistant" {
				out.Messages = append(out.Messages, Message{Role: "assistant", Content: plain})
			} else {
				out.Messages = append(out.Messages, Message{Role: role, Content: plain})
			}
			continue
		}
		var blocks []anthropicBlock
		if err := json.Unmarshal(msg.Content, &blocks); err != nil {
			return nil, fmt.Errorf("messages[%s].content must be a string or an array of blocks", role)
		}
		var (
			parts     []any
			toolCalls []ToolCall
			toolMsgs  []Message
		)
		for _, block := range blocks {
			switch block.Type {
			case "text":
				if block.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": block.Text})
				}
			case "image":
				if part, ok := anthropicImagePart(block); ok {
					parts = append(parts, part)
				}
			case "tool_use":
				name := strings.TrimSpace(block.Name)
				if name == "" {
					continue
				}
				id := strings.TrimSpace(block.ID)
				if id == "" {
					id = "call_" + strings.TrimSpace(uuid())
				}
				toolCalls = append(toolCalls, ToolCall{
					ID:       id,
					Type:     "function",
					Function: FunctionCall{Name: name, Arguments: argumentString(block.Input)},
				})
			case "tool_result":
				content := anthropicToolResultText(block.Content)
				if block.IsError && content != "" {
					content = "error: " + content
				}
				toolMsgs = append(toolMsgs, Message{
					Role:       "tool",
					ToolCallID: strings.TrimSpace(block.ToolUseID),
					Content:    content,
				})
			case "thinking", "redacted_thinking":
				// Upstream keeps its own reasoning; nothing to forward.
			default:
				if block.Text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": block.Text})
				}
			}
		}
		// Tool results are their own chat messages and must precede any text
		// the caller sent alongside them.
		out.Messages = append(out.Messages, toolMsgs...)
		var content any
		if len(parts) == 1 {
			if m, ok := parts[0].(map[string]any); ok && m["type"] == "text" {
				content = m["text"]
			}
		}
		if content == nil && len(parts) > 0 {
			content = parts
		}
		if role == "assistant" {
			if len(toolCalls) > 0 || content != nil {
				out.Messages = append(out.Messages, Message{Role: "assistant", Content: content, ToolCalls: toolCalls})
			}
			continue
		}
		if content != nil {
			out.Messages = append(out.Messages, Message{Role: role, Content: content})
		}
	}

	for _, tool := range req.Tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		// Server-side tools (web_search_20250305, bash_20250124, ...) carry a
		// type and a version-suffixed name; they cannot be bridged to the
		// caller, so only plain client tools are forwarded.
		if tool.Type != "" && tool.Type != "custom" {
			continue
		}
		out.Tools = append(out.Tools, Tool{
			Type: "function",
			Function: ToolFunction{
				Name:        name,
				Description: tool.Description,
				Parameters:  tool.InputSchema,
			},
		})
	}

	// Anthropic requires at least one message; a system-only body is invalid.
	if len(out.Messages) == 0 {
		return nil, fmt.Errorf("messages must contain at least one entry")
	}
	nonSystem := 0
	for _, m := range out.Messages {
		if !strings.EqualFold(m.Role, "system") {
			nonSystem++
		}
	}
	if nonSystem == 0 {
		return nil, fmt.Errorf("messages must contain at least one user or assistant entry")
	}
	return out, nil
}

// estimateTokens is a cheap approximation for /v1/messages/count_tokens, which
// clients call before sending a large request. Roughly four bytes per token.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	n := (len(text) + 3) / 4
	if n < 1 {
		n = 1
	}
	return n
}
