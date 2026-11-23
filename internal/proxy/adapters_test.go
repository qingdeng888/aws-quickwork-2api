package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatFromResponsesStringInput(t *testing.T) {
	body := []byte(`{"model":"smart","instructions":"be brief","input":"hello","stream":false}`)
	req, err := chatFromResponses(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if req.Model != "smart" || req.Stream {
		t.Fatalf("model/stream = %q/%v", req.Model, req.Stream)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "be brief" {
		t.Errorf("system message = %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "hello" {
		t.Errorf("user message = %+v", req.Messages[1])
	}
}

func TestChatFromResponsesItemList(t *testing.T) {
	body := []byte(`{
	  "model":"fast",
	  "stream":true,
	  "input":[
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	    {"type":"function_call","call_id":"call_1","name":"read","arguments":"{\"path\":\"a\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"file body"},
	    {"role":"user","content":"thanks"}
	  ],
	  "tools":[{"type":"function","name":"read","description":"read a file","parameters":{"type":"object"}},
	           {"type":"web_search"}],
	  "tool_choice":{"type":"function","name":"read"},
	  "parallel_tool_calls":false
	}`)
	req, err := chatFromResponses(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !req.Stream {
		t.Error("stream flag lost")
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[0].Role != "user" || req.Messages[0].Content != "hi" {
		t.Errorf("first message = %+v", req.Messages[0])
	}
	assistant := req.Messages[1]
	if assistant.Role != "assistant" || len(assistant.ToolCalls) != 1 {
		t.Fatalf("assistant = %+v", assistant)
	}
	if assistant.ToolCalls[0].ID != "call_1" || assistant.ToolCalls[0].Function.Name != "read" {
		t.Errorf("tool call = %+v", assistant.ToolCalls[0])
	}
	if assistant.ToolCalls[0].Function.Arguments != `{"path":"a"}` {
		t.Errorf("arguments = %q", assistant.ToolCalls[0].Function.Arguments)
	}
	toolMsg := req.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "call_1" || toolMsg.Content != "file body" {
		t.Errorf("tool message = %+v", toolMsg)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "read" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	mode, name := parseToolChoice(req.ToolChoice)
	if mode != "function" || name != "read" {
		t.Errorf("tool_choice = %q %q", mode, name)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Error("parallel_tool_calls lost")
	}
}

func TestChatFromResponsesErrors(t *testing.T) {
	cases := map[string]string{
		"empty":      `{}`,
		"bad input":  `{"input":42}`,
		"empty list": `{"input":[]}`,
	}
	for name, body := range cases {
		if _, err := chatFromResponses([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestChatFromAnthropicBasic(t *testing.T) {
	body := []byte(`{
	  "model":"smart","max_tokens":1024,"stream":true,
	  "system":[{"type":"text","text":"be terse"}],
	  "messages":[{"role":"user","content":"hello"}],
	  "tools":[{"name":"read","description":"read a file","input_schema":{"type":"object"}}],
	  "tool_choice":{"type":"any"}
	}`)
	req, err := chatFromAnthropic(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !req.Stream || req.Model != "smart" {
		t.Fatalf("model/stream = %q/%v", req.Model, req.Stream)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "be terse" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if req.Messages[1].Content != "hello" {
		t.Errorf("user content = %+v", req.Messages[1].Content)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "read" {
		t.Fatalf("tools = %+v", req.Tools)
	}
	if mode, _ := parseToolChoice(req.ToolChoice); mode != "required" {
		t.Errorf("tool_choice mode = %q, want required", mode)
	}
}

func TestChatFromAnthropicToolBlocks(t *testing.T) {
	body := []byte(`{
	  "model":"smart","max_tokens":512,
	  "messages":[
	    {"role":"user","content":[{"type":"text","text":"read a.txt"}]},
	    {"role":"assistant","content":[
	      {"type":"text","text":"reading"},
	      {"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a.txt"}}
	    ]},
	    {"role":"user","content":[
	      {"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"file body"}]},
	      {"type":"text","text":"summarize it"}
	    ]}
	  ],
	  "tools":[{"name":"read","description":"read","input_schema":{"type":"object"}},
	           {"name":"web_search","type":"web_search_20250305"}]
	}`)
	req, err := chatFromAnthropic(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d: %+v", len(req.Messages), req.Messages)
	}
	assistant := req.Messages[1]
	if assistant.Role != "assistant" || assistant.Content != "reading" {
		t.Errorf("assistant = %+v", assistant)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "toolu_1" {
		t.Fatalf("tool calls = %+v", assistant.ToolCalls)
	}
	if assistant.ToolCalls[0].Function.Arguments != `{"path":"a.txt"}` {
		t.Errorf("arguments = %q", assistant.ToolCalls[0].Function.Arguments)
	}
	toolMsg := req.Messages[2]
	if toolMsg.Role != "tool" || toolMsg.ToolCallID != "toolu_1" || toolMsg.Content != "file body" {
		t.Errorf("tool result = %+v", toolMsg)
	}
	if req.Messages[3].Content != "summarize it" {
		t.Errorf("follow-up user message = %+v", req.Messages[3])
	}
	// Server-side tools are dropped, client tools forwarded.
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "read" {
		t.Fatalf("tools = %+v", req.Tools)
	}
}

func TestChatFromAnthropicImageBlock(t *testing.T) {
	body := []byte(`{"model":"smart","max_tokens":16,"messages":[{"role":"user","content":[
	  {"type":"text","text":"what is this"},
	  {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}
	]}]}`)
	req, err := chatFromAnthropic(body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	parts, ok := req.Messages[0].Content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("content = %#v", req.Messages[0].Content)
	}
	img, _ := parts[1].(map[string]any)
	url, _ := img["image_url"].(map[string]any)
	if got, _ := url["url"].(string); got != "data:image/png;base64,AAA" {
		t.Errorf("image url = %q", got)
	}
	if text := contentText(req.Messages[0].Content); !strings.Contains(text, "what is this") || !strings.Contains(text, "[image]") {
		t.Errorf("flattened content = %q", text)
	}
}

func TestChatFromAnthropicErrors(t *testing.T) {
	cases := map[string]string{
		"no messages": `{"model":"smart","max_tokens":8}`,
		"system only": `{"model":"smart","max_tokens":8,"system":"x","messages":[]}`,
	}
	for name, body := range cases {
		if _, err := chatFromAnthropic([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFlavorErrorShapes(t *testing.T) {
	cases := []struct {
		flavor wireFlavor
		check  func(t *testing.T, body map[string]any)
	}{
		{wireChat, func(t *testing.T, body map[string]any) {
			errObj, _ := body["error"].(map[string]any)
			if errObj["code"] != "invalid_request_error" || errObj["type"] != "invalid_request_error" {
				t.Errorf("chat error = %+v", body)
			}
		}},
		{wireResponses, func(t *testing.T, body map[string]any) {
			errObj, _ := body["error"].(map[string]any)
			if errObj["type"] != "invalid_request_error" || errObj["code"] != "invalid_request_error" {
				t.Errorf("responses error = %+v", body)
			}
		}},
		{wireAnthropic, func(t *testing.T, body map[string]any) {
			if body["type"] != "error" {
				t.Fatalf("anthropic error = %+v", body)
			}
			errObj, _ := body["error"].(map[string]any)
			if errObj["type"] != "invalid_request_error" {
				t.Errorf("anthropic error type = %+v", errObj)
			}
		}},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		writeFlavorError(rec, tc.flavor, 400, "invalid_request_error", "boom")
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		tc.check(t, body)
	}
}

func TestWriteFlavorCompletionShapes(t *testing.T) {
	created := int64(1700000000)
	tools := []ToolCall{{
		ID:       "call_1",
		Type:     "function",
		Function: FunctionCall{Name: "read", Arguments: `{"path":"a"}`},
	}}
	usage := Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
	for _, flavor := range []wireFlavor{wireChat, wireResponses, wireAnthropic} {
		rec := httptest.NewRecorder()
		writeFlavorCompletion(rec, flavor, flavor.idPrefix()+"abc", created, "smart", "hello", tools, "tool_calls", usage)
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%d: decode: %v", flavor, err)
		}
		switch flavor {
		case wireChat:
			choices, _ := body["choices"].([]any)
			if len(choices) != 1 {
				t.Fatalf("chat choices = %+v", body)
			}
			choice := choices[0].(map[string]any)
			if choice["finish_reason"] != "tool_calls" {
				t.Errorf("chat finish = %+v", choice)
			}
			msg := choice["message"].(map[string]any)
			if msg["content"] != "hello" {
				t.Errorf("chat content = %+v", msg)
			}
			if body["object"] != "chat.completion" {
				t.Errorf("chat object = %v", body["object"])
			}
		case wireResponses:
			if body["object"] != "response" || body["status"] != "completed" {
				t.Fatalf("responses body = %+v", body)
			}
			output, _ := body["output"].([]any)
			if len(output) != 2 {
				t.Fatalf("responses output = %+v", output)
			}
			msg := output[0].(map[string]any)
			if msg["type"] != "message" {
				t.Errorf("responses first item = %+v", msg)
			}
			content := msg["content"].([]any)[0].(map[string]any)
			if content["type"] != "output_text" || content["text"] != "hello" {
				t.Errorf("responses text part = %+v", content)
			}
			call := output[1].(map[string]any)
			if call["type"] != "function_call" || call["call_id"] != "call_1" || call["arguments"] != `{"path":"a"}` {
				t.Errorf("responses function call = %+v", call)
			}
			u := body["usage"].(map[string]any)
			if u["input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 5 {
				t.Errorf("responses usage = %+v", u)
			}
		case wireAnthropic:
			if body["type"] != "message" || body["role"] != "assistant" {
				t.Fatalf("anthropic body = %+v", body)
			}
			if body["stop_reason"] != "tool_use" {
				t.Errorf("anthropic stop_reason = %v", body["stop_reason"])
			}
			content, _ := body["content"].([]any)
			if len(content) != 2 {
				t.Fatalf("anthropic content = %+v", content)
			}
			if content[0].(map[string]any)["type"] != "text" {
				t.Errorf("anthropic text block = %+v", content[0])
			}
			block := content[1].(map[string]any)
			if block["type"] != "tool_use" || block["id"] != "call_1" || block["name"] != "read" {
				t.Errorf("anthropic tool block = %+v", block)
			}
			input := block["input"].(map[string]any)
			if input["path"] != "a" {
				t.Errorf("anthropic tool input = %+v", input)
			}
			u := body["usage"].(map[string]any)
			if u["input_tokens"].(float64) != 10 || u["output_tokens"].(float64) != 5 {
				t.Errorf("anthropic usage = %+v", u)
			}
		}
	}
}

func TestStreamFlavorSequences(t *testing.T) {
	usage := Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}
	tools := []ToolCall{{ID: "call_1", Function: FunctionCall{Name: "read", Arguments: `{"path":"a"}`}}}

	cases := []struct {
		flavor  wireFlavor
		want    []string
		noLines []string
	}{
		{
			flavor: wireChat,
			want:   []string{"data: ", `"content":"he"`, `"content":"llo"`, `"tool_calls"`, `"finish_reason":"tool_calls"`, "data: [DONE]"},
		},
		{
			flavor: wireResponses,
			want: []string{
				"event: response.created",
				"event: response.output_item.added",
				"event: response.content_part.added",
				"event: response.output_text.delta",
				"event: response.output_text.done",
				"event: response.function_call_arguments.done",
				"event: response.completed",
			},
		},
		{
			flavor: wireAnthropic,
			want: []string{
				"event: message_start",
				"event: content_block_start",
				"event: content_block_delta",
				`"type":"text_delta"`,
				"event: privacy", // placeholder replaced below
			},
		},
	}
	// Fix the Anthropic expectations without a placeholder.
	cases[2].want = []string{
		"event: message_start",
		"event: content_block_start",
		`"type":"text_delta"`,
		`"type":"tool_use"`,
		"event: content_block_stop",
		"event: message_delta",
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	}

	for _, tc := range cases {
		rec := httptest.NewRecorder()
		sw := newStreamWriter(rec, tc.flavor, tc.flavor.idPrefix()+"x", "smart", 1700000000, false)
		sw.textDelta("he")
		sw.textDelta("llo")
		sw.toolCalls(tools)
		sw.finish("tool_calls", usage)
		sw.done()
		out := rec.Body.String()
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("flavor %d: output missing %q\n%s", tc.flavor, want, out)
			}
		}
		if rec.Header().Get("Content-Type") != "text/event-stream; charset=utf-8" {
			t.Errorf("flavor %d: content type = %q", tc.flavor, rec.Header().Get("Content-Type"))
		}
		if tc.flavor != wireResponses && strings.Count(out, "event: message_start") > 1 {
			t.Errorf("flavor %d: start emitted twice", tc.flavor)
		}
	}
}

func TestStreamChatIncludesUsageWhenRequested(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newStreamWriter(rec, wireChat, "chatcmpl-x", "smart", 1, true)
	sw.textDelta("hi")
	sw.finish("stop", Usage{PromptTokens: 4, CompletionTokens: 1, TotalTokens: 5})
	sw.done()
	out := rec.Body.String()
	if !strings.Contains(out, `"usage":{"completion_tokens":1`) {
		t.Errorf("usage chunk missing:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Errorf("missing [DONE] terminator:\n%s", out)
	}
}

func TestStreamErrorShapes(t *testing.T) {
	for _, flavor := range []wireFlavor{wireChat, wireResponses, wireAnthropic} {
		rec := httptest.NewRecorder()
		sw := newStreamWriter(rec, flavor, flavor.idPrefix()+"x", "smart", 1, false)
		sw.textDelta("partial")
		sw.streamError("upstream exploded")
		out := rec.Body.String()
		if !strings.Contains(out, "upstream exploded") {
			t.Errorf("flavor %d: error message missing:\n%s", flavor, out)
		}
		if flavor == wireAnthropic && !strings.Contains(out, `"type":"error"`) {
			t.Errorf("anthropic error shape missing:\n%s", out)
		}
		if flavor == wireChat && !strings.Contains(out, `"finish_reason":"stop"`) {
			t.Errorf("chat error did not close the choice:\n%s", out)
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	if estimateTokens("") != 0 {
		t.Error("empty text should be 0")
	}
	if n := estimateTokens("12345678"); n != 2 {
		t.Errorf("estimate = %d, want 2", n)
	}
	if estimateTokens("a") != 1 {
		t.Error("short text should be at least 1")
	}
}
