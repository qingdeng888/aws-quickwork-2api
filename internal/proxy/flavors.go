package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// wireFlavor selects the API dialect used for one request's response. The
// upstream pipeline is identical for all of them: the caller's request is
// converted into a chat request, and the answer is rendered back in the
// dialect the caller expects.
type wireFlavor int

const (
	// wireChat: POST /v1/chat/completions (OpenAI chat completions).
	wireChat wireFlavor = iota
	// wireResponses: POST /v1/responses (OpenAI Responses API).
	wireResponses
	// wireAnthropic: POST /v1/messages (Anthropic Messages API).
	wireAnthropic
)

func (f wireFlavor) idPrefix() string {
	switch f {
	case wireResponses:
		return "resp_"
	case wireAnthropic:
		return "msg_"
	default:
		return "chatcmpl-"
	}
}

// errorType maps an HTTP status to the Anthropic error type vocabulary.
func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == 529:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// apiErrorType maps an HTTP status to the OpenAI error type vocabulary.
func openAIErrorType(status int) string {
	switch {
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// writeFlavorError renders an error in the requested dialect.
func writeFlavorError(w http.ResponseWriter, f wireFlavor, status int, code, message string) {
	switch f {
	case wireAnthropic:
		writeJSON(w, status, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrorType(status), "message": message},
		})
	case wireResponses:
		writeJSON(w, status, map[string]any{
			"error": map[string]any{
				"message": message,
				"type":    openAIErrorType(status),
				"code":    code,
				"param":   nil,
			},
		})
	default:
		writeOpenAIError(w, status, code, message)
	}
}

// flavorUsage renders token usage per dialect.
func flavorUsage(f wireFlavor, u Usage) map[string]any {
	prompt := u.PromptTokens + u.CacheReadTokens + u.CacheWriteTokens
	completion := u.CompletionTokens
	total := u.TotalTokens
	if total == 0 {
		total = prompt + completion
	}
	switch f {
	case wireAnthropic:
		m := map[string]any{
			"input_tokens":  prompt,
			"output_tokens": completion,
		}
		if u.CacheReadTokens > 0 {
			m["cache_read_input_tokens"] = u.CacheReadTokens
		}
		if u.CacheWriteTokens > 0 {
			m["cache_creation_input_tokens"] = u.CacheWriteTokens
		}
		return m
	case wireResponses:
		return map[string]any{
			"input_tokens":          prompt,
			"input_tokens_details":  map[string]any{"cached_tokens": u.CacheReadTokens},
			"output_tokens":         completion,
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":          total,
		}
	default:
		return usageMap(u)
	}
}

// flavorStopReason maps the internal finish reason to each dialect.
func flavorStopReason(f wireFlavor, finish string) string {
	switch f {
	case wireAnthropic:
		switch finish {
		case "tool_calls":
			return "tool_use"
		case "length":
			return "max_tokens"
		default:
			return "end_turn"
		}
	case wireResponses:
		if finish == "length" {
			return "incomplete"
		}
		return "completed"
	default:
		return finish
	}
}

// toolArgumentsObject turns a tool call's JSON argument string into an object
// for the Anthropic tool_use block. Invalid JSON degrades to an empty object.
func toolArgumentsObject(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return map[string]any{}
	}
	if out == nil {
		return map[string]any{}
	}
	return out
}

// writeFlavorCompletion writes a non-streaming answer in the given dialect.
func writeFlavorCompletion(w http.ResponseWriter, f wireFlavor, id string, created int64, model, text string, tools []ToolCall, finish string, u Usage) {
	hasText := hasVisibleText(text)
	switch f {
	case wireAnthropic:
		content := make([]map[string]any, 0, 1+len(tools))
		if hasText {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, tc := range tools {
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": toolArgumentsObject(tc.Function.Arguments),
			})
		}
		stopReason := flavorStopReason(f, finish)
		if !hasText && len(tools) == 0 {
			stopReason = "end_turn"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       content,
			"stop_reason":   stopReason,
			"stop_sequence": nil,
			"usage":         flavorUsage(f, u),
		})
	case wireResponses:
		output := make([]map[string]any, 0, 1+len(tools))
		if hasText {
			output = append(output, responsesMessageItem("msg_"+strings.TrimPrefix(id, "resp_"), text, "completed"))
		}
		for i, tc := range tools {
			output = append(output, responsesFunctionCallItem(fmt.Sprintf("fc_%d_%s", i, strings.TrimPrefix(id, "resp_")), tc, "completed"))
		}
		status := "completed"
		if finish == "length" {
			status = "incomplete"
		}
		body := map[string]any{
			"id":         id,
			"object":     "response",
			"created_at": created,
			"status":     status,
			"model":      model,
			"output":     output,
			"usage":      flavorUsage(f, u),
		}
		if status == "incomplete" {
			body["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		}
		writeJSON(w, http.StatusOK, body)
	default:
		message := map[string]any{"role": "assistant"}
		if hasText {
			message["content"] = text
		} else {
			message["content"] = nil
		}
		if len(tools) > 0 {
			message["tool_calls"] = tools
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      id,
			"object":  "chat.completion",
			"created": created,
			"model":   model,
			"choices": []map[string]any{{
				"index":         0,
				"message":       message,
				"finish_reason": finish,
			}},
			"usage": usageMap(u),
		})
	}
}

// responsesMessageItem builds an assistant message output item.
func responsesMessageItem(itemID, text, status string) map[string]any {
	return map[string]any{
		"type":   "message",
		"id":     itemID,
		"status": status,
		"role":   "assistant",
		"content": []map[string]any{{
			"type":        "output_text",
			"text":        text,
			"annotations": []any{},
		}},
	}
}

// responsesFunctionCallItem builds a function_call output item.
func responsesFunctionCallItem(itemID string, tc ToolCall, status string) map[string]any {
	arguments := strings.TrimSpace(tc.Function.Arguments)
	if arguments == "" {
		arguments = "{}"
	}
	return map[string]any{
		"type":      "function_call",
		"id":        itemID,
		"call_id":   tc.ID,
		"name":      tc.Function.Name,
		"arguments": arguments,
		"status":    status,
	}
}

// streamWriter renders one streamed answer in the requested dialect. It owns
// the SSE headers and the "has the client seen real content yet" flags, which
// the chat handler uses to decide whether a retry is still safe.
type streamWriter struct {
	flavor wireFlavor
	w      http.ResponseWriter
	flush  func()
	mu     sync.Mutex

	id           string
	model        string
	created      int64
	threadID     string
	includeUsage bool

	// headersSent means the SSE response began (keep-alive comments count).
	// started means real content reached the client: a retry is no longer safe.
	headersSent bool
	started     bool

	// Anthropic content-block bookkeeping.
	nextBlock int
	textOpen  bool
	textIndex int

	// Responses output bookkeeping.
	messageItemID      string
	messageItemOpen    bool
	messageOutputIndex int
	outputIndex        int

	textBuf strings.Builder
	tools   []ToolCall
}

func newStreamWriter(w http.ResponseWriter, f wireFlavor, id, model string, created int64, includeUsage bool) *streamWriter {
	flusher, _ := w.(http.Flusher)
	flush := func() {}
	if flusher != nil {
		flush = flusher.Flush
	}
	return &streamWriter{
		flavor:       f,
		w:            w,
		flush:        flush,
		id:           id,
		model:        model,
		created:      created,
		includeUsage: includeUsage,
	}
}

func (s *streamWriter) writeLine(line string) {
	s.mu.Lock()
	_, _ = fmt.Fprint(s.w, line)
	s.flush()
	s.mu.Unlock()
}

func (s *streamWriter) data(payload any) {
	body, _ := json.Marshal(payload)
	s.writeLine("data: " + string(body) + "\n\n")
}

func (s *streamWriter) event(name string, payload any) {
	body, _ := json.Marshal(payload)
	s.writeLine("event: " + name + "\ndata: " + string(body) + "\n\n")
}

// sendHeaders starts the SSE response exactly once.
func (s *streamWriter) sendHeaders() {
	if s.headersSent {
		return
	}
	s.headersSent = true
	s.w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	s.w.Header().Set("X-Accel-Buffering", "no")
	if s.threadID != "" {
		s.w.Header().Set("X-Thread-Id", s.threadID)
	}
	s.w.WriteHeader(http.StatusOK)
	s.flush()
}

// keepAlive keeps idle proxies from dropping a stream that is still buffering
// (the tool bridge holds the whole answer before emitting it).
func (s *streamWriter) keepAlive() {
	if s.flavor == wireAnthropic {
		s.event("ping", map[string]any{"type": "ping"})
		return
	}
	s.writeLine(": keep-alive\n\n")
}

// ensureStarted emits the dialect's opening event exactly once.
func (s *streamWriter) ensureStarted() {
	if s.started {
		return
	}
	s.started = true
	s.sendHeaders()
	switch s.flavor {
	case wireAnthropic:
		s.event("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            s.id,
				"type":          "message",
				"role":          "assistant",
				"model":         s.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		})
	case wireResponses:
		s.messageItemID = "msg_" + strings.TrimPrefix(s.id, "resp_")
		s.event("response.created", map[string]any{
			"type": "response.created",
			"response": map[string]any{
				"id":         s.id,
				"object":     "response",
				"created_at": s.created,
				"status":     "in_progress",
				"model":      s.model,
				"output":     []any{},
			},
		})
		// The assistant message item is opened lazily by textDelta: an answer
		// that only contains tool calls must not carry an empty message."""
		s.outputIndex = 0
	default:
		s.data(map[string]any{
			"id":      s.id,
			"object":  "chat.completion.chunk",
			"created": s.created,
			"model":   s.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"role": "assistant"},
				"finish_reason": nil,
			}},
		})
	}
}

// textDelta streams one piece of the answer.
func (s *streamWriter) textDelta(text string) {
	s.ensureStarted()
	s.textBuf.WriteString(text)
	switch s.flavor {
	case wireAnthropic:
		if !s.textOpen {
			s.textIndex = s.nextBlock
			s.nextBlock++
			s.textOpen = true
			s.event("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         s.textIndex,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		s.event("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": s.textIndex,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	case wireResponses:
		if !s.messageItemOpen {
			s.messageItemOpen = true
			s.messageOutputIndex = s.outputIndex
			s.outputIndex++
			s.event("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": s.messageOutputIndex,
				"item": map[string]any{
					"type":    "message",
					"id":      s.messageItemID,
					"status":  "in_progress",
					"role":    "assistant",
					"content": []any{},
				},
			})
			s.event("response.content_part.added", map[string]any{
				"type":          "response.content_part.added",
				"item_id":       s.messageItemID,
				"output_index":  s.messageOutputIndex,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
		}
		s.event("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       s.messageItemID,
			"output_index":  s.messageOutputIndex,
			"content_index": 0,
			"delta":         text,
		})
	default:
		s.data(map[string]any{
			"id":      s.id,
			"object":  "chat.completion.chunk",
			"created": s.created,
			"model":   s.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{"content": text},
				"finish_reason": nil,
			}},
		})
	}
}

// closeTextBlock ends the open Anthropic text block, if any.
func (s *streamWriter) closeTextBlock() {
	if s.flavor != wireAnthropic || !s.textOpen {
		return
	}
	s.textOpen = false
	s.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": s.textIndex,
	})
}

// closeMessageItem ends the open Responses message item, if any.
func (s *streamWriter) closeMessageItem() {
	if s.flavor != wireResponses || !s.messageItemOpen {
		return
	}
	s.messageItemOpen = false
	text := s.textBuf.String()
	s.event("response.output_text.done", map[string]any{
		"type":          "response.output_text.done",
		"item_id":       s.messageItemID,
		"output_index":  s.messageOutputIndex,
		"content_index": 0,
		"text":          text,
	})
	s.event("response.content_part.done", map[string]any{
		"type":          "response.content_part.done",
		"item_id":       s.messageItemID,
		"output_index":  s.messageOutputIndex,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	s.event("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": s.messageOutputIndex,
		"item":         responsesMessageItem(s.messageItemID, text, "completed"),
	})
}

// toolCalls emits the tool calls collected for this answer.
func (s *streamWriter) toolCalls(tools []ToolCall) {
	if len(tools) == 0 {
		return
	}
	s.ensureStarted()
	s.tools = tools
	switch s.flavor {
	case wireAnthropic:
		s.closeTextBlock()
		for _, tc := range tools {
			index := s.nextBlock
			s.nextBlock++
			s.event("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": map[string]any{},
				},
			})
			arguments := strings.TrimSpace(tc.Function.Arguments)
			if arguments == "" {
				arguments = "{}"
			}
			s.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
			})
			s.event("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			})
		}
	case wireResponses:
		s.closeMessageItem()
		for i, tc := range tools {
			itemID := fmt.Sprintf("fc_%d_%s", i, strings.TrimPrefix(s.id, "resp_"))
			index := s.outputIndex
			s.outputIndex++
			arguments := strings.TrimSpace(tc.Function.Arguments)
			if arguments == "" {
				arguments = "{}"
			}
			s.event("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": index,
				"item": map[string]any{
					"type":      "function_call",
					"id":        itemID,
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": "",
					"status":    "in_progress",
				},
			})
			s.event("response.function_call_arguments.delta", map[string]any{
				"type":         "response.function_call_arguments.delta",
				"item_id":      itemID,
				"output_index": index,
				"delta":        arguments,
			})
			s.event("response.function_call_arguments.done", map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      itemID,
				"output_index": index,
				"arguments":    arguments,
			})
			s.event("response.output_item.done", map[string]any{
				"type":         "response.output_item.done",
				"output_index": index,
				"item":         responsesFunctionCallItem(itemID, tc, "completed"),
			})
		}
	default:
		for i, tc := range tools {
			s.data(map[string]any{
				"id":      s.id,
				"object":  "chat.completion.chunk",
				"created": s.created,
				"model":   s.model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{"tool_calls": []map[string]any{{
						"index": i,
						"id":    tc.ID,
						"type":  "function",
						"function": map[string]any{
							"name":      tc.Function.Name,
							"arguments": tc.Function.Arguments,
						},
					}}},
					"finish_reason": nil,
				}},
			})
		}
	}
}

// finish closes the stream body: chat sends the finish chunk, Anthropic sends
// message_delta, Responses sends response.completed.
func (s *streamWriter) finish(reason string, u Usage) {
	s.ensureStarted()
	switch s.flavor {
	case wireAnthropic:
		s.closeTextBlock()
		s.event("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": flavorStopReason(s.flavor, reason), "stop_sequence": nil},
			"usage": flavorUsage(s.flavor, u),
		})
	case wireResponses:
		s.closeMessageItem()
		output := make([]map[string]any, 0, 1+len(s.tools))
		if hasVisibleText(s.textBuf.String()) {
			output = append(output, responsesMessageItem(s.messageItemID, s.textBuf.String(), "completed"))
		}
		for i, tc := range s.tools {
			output = append(output, responsesFunctionCallItem(
				fmt.Sprintf("fc_%d_%s", i, strings.TrimPrefix(s.id, "resp_")), tc, "completed"))
		}
		status := flavorStopReason(s.flavor, reason)
		body := map[string]any{
			"id":         s.id,
			"object":     "response",
			"created_at": s.created,
			"status":     status,
			"model":      s.model,
			"output":     output,
			"usage":      flavorUsage(s.flavor, u),
		}
		if status == "incomplete" {
			body["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		}
		s.event("response.completed", map[string]any{"type": "response.completed", "response": body})
	default:
		s.data(map[string]any{
			"id":      s.id,
			"object":  "chat.completion.chunk",
			"created": s.created,
			"model":   s.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": reason,
			}},
		})
		if s.includeUsage {
			s.data(map[string]any{
				"id":      s.id,
				"object":  "chat.completion.chunk",
				"created": s.created,
				"model":   s.model,
				"choices": []any{},
				"usage":   usageMap(u),
			})
		}
	}
}

// done writes the dialect's terminator.
func (s *streamWriter) done() {
	switch s.flavor {
	case wireAnthropic:
		s.event("message_stop", map[string]any{"type": "message_stop"})
	case wireResponses:
		// The stream ends after response.completed; no [DONE] marker exists.
	default:
		s.writeLine("data: [DONE]\n\n")
	}
}

// streamError ends a stream with an error in the dialect's shape.
func (s *streamWriter) streamError(message string) {
	if s.flavor == wireChat && s.started {
		// Close the choice so strict clients do not see an unterminated one.
		s.data(map[string]any{
			"id":      s.id,
			"object":  "chat.completion.chunk",
			"created": s.created,
			"model":   s.model,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
		})
	}
	s.sendHeaders()
	switch s.flavor {
	case wireAnthropic:
		s.closeTextBlock()
		s.event("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": message},
		})
	case wireResponses:
		s.event("error", map[string]any{
			"type":    "error",
			"code":    "upstream_error",
			"message": message,
			"param":   nil,
		})
	default:
		s.data(map[string]any{"error": map[string]any{
			"message": message,
			"type":    "upstream_error",
			"code":    "upstream_error",
		}})
		s.writeLine("data: [DONE]\n\n")
	}
}

// compile-time guard: keep time imported for the created timestamp helpers.
var _ = time.Second
