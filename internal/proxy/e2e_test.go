package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/store"
)

// fakeUpstream speaks the minimal AIDP WebSocket dialect the proxy expects: it
// waits for the user_message frame, then answers with the provided text and
// completes the turn. Text frames are used because readEvents consumes them
// directly as JSON envelopes.
type fakeUpstream struct {
	srv  *httptest.Server
	got  chan string // the relayed user turn text, for assertions
	text []string    // text_delta payloads to emit
}

func newFakeUpstream(t *testing.T, parts ...string) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{got: make(chan string, 4), text: parts}
	upgrader := websocket.Upgrader{
		CheckOrigin:     func(*http.Request) bool { return true },
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if !isUserMessageFrame(data) {
				continue
			}
			if text := extractUserText(data); text != "" {
				select {
				case f.got <- text:
				default:
				}
			}
			for _, part := range f.text {
				frame, _ := json.Marshal(map[string]any{
					"type":    "text_delta",
					"payload": map[string]any{"text": part},
				})
				if err := conn.WriteMessage(websocket.TextMessage, frame); err != nil {
					return
				}
				time.Sleep(5 * time.Millisecond)
			}
			usage, _ := json.Marshal(map[string]any{
				"type": "turn_usage",
				"payload": map[string]any{
					"input_tokens": 11, "output_tokens": 3,
					"total_tokens": 14, "cache_read_tokens": 5, "cache_write_tokens": 1,
				},
			})
			_ = conn.WriteMessage(websocket.TextMessage, usage)
			done, _ := json.Marshal(map[string]any{"type": "message_complete", "payload": map[string]any{}})
			_ = conn.WriteMessage(websocket.TextMessage, done)
			return
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// clientFrame decodes the binary frame the proxy sends: an AIDP envelope
// wrapping base64 JSON, or plain JSON for gateway transports.
func clientFrame(data []byte) (string, bool) {
	var env struct {
		EventType string `json:"eventType"`
		Payload   struct {
			Payload string `json:"payload"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(data, &env); err == nil && env.Payload.Payload != "" {
		if inner, err := base64.StdEncoding.DecodeString(env.Payload.Payload); err == nil {
			return string(inner), true
		}
	}
	if json.Valid(data) {
		return string(data), true
	}
	return "", false
}

// isUserMessageFrame reports whether the frame carries the user turn.
func isUserMessageFrame(data []byte) bool {
	inner, ok := clientFrame(data)
	return ok && strings.Contains(inner, `"user_message"`)
}

// extractUserText pulls the relayed prompt out of the user_message frame.
func extractUserText(data []byte) string {
	inner, ok := clientFrame(data)
	if !ok {
		return ""
	}
	var env struct {
		Payload struct {
			Text string `json:"text"`
		} `json:"payload"`
	}
	if json.Unmarshal([]byte(inner), &env) == nil {
		return env.Payload.Text
	}
	return ""
}

// testProxyServer builds a Server backed by a temp store holding one account
// that points at the fake upstream.
func testProxyServer(t *testing.T, upstream *fakeUpstream) *Server {
	t.Helper()
	cfg := config.Default()
	cfgPtr := &cfg
	cfg.APIKeys = nil
	cfg.SessionReuse = false
	cfg.KeepSessions = false
	cfg.MaxAttempts = 1
	cfg.RequestTimeout = config.Duration(30 * time.Second)
	cfg.ProxyPool.Enabled = false

	st, err := store.Open(filepath.Join(t.TempDir(), "quickwork.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	host := strings.TrimPrefix(upstream.srv.URL, "http://")
	account := &store.Account{
		Email:        "test@example.com",
		Provider:     "social",
		TenantURL:    "https://tenant.example",
		WSHost:       "ws://" + host,
		WSPath:       "/ws",
		AccessToken:  "test-token",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(2 * time.Hour).Unix(),
		ProxyMode:    store.ProxyModeDirect,
	}
	if err := st.Save(context.Background(), account); err != nil {
		t.Fatalf("save account: %v", err)
	}
	return NewServer(cfgPtr, accounts.New(st, cfgPtr))
}

// serve runs one handler and returns the recorded response.
func serve(t *testing.T, s *Server, handler http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler(rec, req)
	return rec
}

// parseSSE returns the data payloads of an SSE response.
func parseSSE(t *testing.T, body string) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, 8)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
			t.Fatalf("bad SSE payload %q: %v", line, err)
		}
		out = append(out, payload)
	}
	return out
}

func TestE2EChatCompletionStreaming(t *testing.T) {
	upstream := newFakeUpstream(t, "he", "llo")
	s := testProxyServer(t, upstream)

	rec := serve(t, s, s.handleChat, "/v1/chat/completions",
		`{"model":"smart","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.HasSuffix(strings.TrimSpace(body), "data: [DONE]") {
		t.Fatalf("missing [DONE]:\n%s", body)
	}
	payloads := parseSSE(t, body)
	var text strings.Builder
	sawFinish, sawUsage := false, false
	for _, p := range payloads {
		choices, _ := p["choices"].([]any)
		if len(choices) == 0 {
			if _, ok := p["usage"]; ok {
				sawUsage = true
			}
			continue
		}
		choice := choices[0].(map[string]any)
		if delta, ok := choice["delta"].(map[string]any); ok {
			if content, ok := delta["content"].(string); ok {
				text.WriteString(content)
			}
		}
		if choice["finish_reason"] == "stop" {
			sawFinish = true
		}
	}
	if text.String() != "hello" {
		t.Errorf("streamed text = %q", text.String())
	}
	if !sawFinish || !sawUsage {
		t.Errorf("finish=%v usage=%v", sawFinish, sawUsage)
	}
	select {
	case got := <-upstream.got:
		if !strings.Contains(got, "hi") {
			t.Errorf("upstream received %q", got)
		}
	case <-time.After(time.Second):
		t.Error("upstream never received the user turn")
	}
}

func TestE2EResponsesStreaming(t *testing.T) {
	upstream := newFakeUpstream(t, "he", "llo")
	s := testProxyServer(t, upstream)

	rec := serve(t, s, s.handleResponses, "/v1/responses",
		`{"model":"fast","stream":true,"input":"hi"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_text.delta",
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	var completed map[string]any
	for _, p := range parseSSE(t, body) {
		if p["type"] == "response.completed" {
			completed, _ = p["response"].(map[string]any)
		}
	}
	if completed == nil {
		t.Fatal("no response.completed event")
	}
	if completed["status"] != "completed" || completed["model"] != "fast" {
		t.Errorf("completed response = %+v", completed)
	}
	output, _ := completed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output = %+v", output)
	}
	item := output[0].(map[string]any)
	parts := item["content"].([]any)
	if text := parts[0].(map[string]any)["text"]; text != "hello" {
		t.Errorf("output text = %v", text)
	}
}

func TestE2EResponsesToolCall(t *testing.T) {
	upstream := newFakeUpstream(t, `<client_tool_call>{"name":"read","arguments":{"path":"a.txt"}}</client_tool_call>`)
	s := testProxyServer(t, upstream)

	rec := serve(t, s, s.handleResponses, "/v1/responses", `{
	  "model":"smart","input":"read a.txt",
	  "tools":[{"type":"function","name":"read","description":"read a file","parameters":{"type":"object"}}],
	  "tool_choice":"required"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	output, _ := body["output"].([]any)
	var call map[string]any
	for _, raw := range output {
		item := raw.(map[string]any)
		if item["type"] == "function_call" {
			call = item
		}
	}
	if call == nil {
		t.Fatalf("no function_call in output: %+v", output)
	}
	if call["name"] != "read" {
		t.Errorf("name = %v", call["name"])
	}
	if call["arguments"] != `{"path":"a.txt"}` {
		t.Errorf("arguments = %v", call["arguments"])
	}
	if id, _ := call["call_id"].(string); !strings.HasPrefix(id, "call_") {
		t.Errorf("call_id = %v", call["call_id"])
	}
}

func TestE2EAnthropicStreamingWithToolCall(t *testing.T) {
	upstream := newFakeUpstream(t, `<client_tool_call>{"name":"read","arguments":{"path":"a.txt"}}</client_tool_call>`)
	s := testProxyServer(t, upstream)

	rec := serve(t, s, s.handleMessages, "/v1/messages", `{
	  "model":"smart","max_tokens":128,"stream":true,
	  "messages":[{"role":"user","content":"read a.txt"}],
	  "tools":[{"name":"read","description":"read a file","input_schema":{"type":"object"}}],
	  "tool_choice":{"type":"any"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		`"type":"tool_use"`,
		`"name":"read"`,
		"event: message_stop",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q:\n%s", want, body)
		}
	}
	var stopReason string
	for _, p := range parseSSE(t, body) {
		if p["type"] == "message_delta" {
			delta, _ := p["delta"].(map[string]any)
			stopReason, _ = delta["stop_reason"].(string)
		}
	}
	if stopReason != "tool_use" {
		t.Errorf("stop_reason = %q, want tool_use", stopReason)
	}
}

func TestE2EAnthropicNonStreaming(t *testing.T) {
	upstream := newFakeUpstream(t, "hello ", "world")
	s := testProxyServer(t, upstream)

	rec := serve(t, s, s.handleMessages, "/v1/messages",
		`{"model":"smart","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["type"] != "message" || body["role"] != "assistant" {
		t.Fatalf("body = %+v", body)
	}
	content, _ := body["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %+v", content)
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "hello world" {
		t.Errorf("text block = %+v", block)
	}
	if body["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v", body["stop_reason"])
	}
	usage := body["usage"].(map[string]any)
	// Anthropic reports uncached input separately from cache reads/writes.
	if usage["input_tokens"].(float64) != 17 {
		t.Errorf("input_tokens = %v", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"].(float64) != 5 {
		t.Errorf("cache_read_input_tokens = %v", usage["cache_read_input_tokens"])
	}
	if usage["output_tokens"].(float64) != 3 {
		t.Errorf("output_tokens = %v", usage["output_tokens"])
	}
}

func TestE2EAnthropicCountTokens(t *testing.T) {
	upstream := newFakeUpstream(t, "unused")
	s := testProxyServer(t, upstream)
	rec := serve(t, s, s.handleCountTokens, "/v1/messages/count_tokens",
		`{"model":"smart","max_tokens":64,"messages":[{"role":"user","content":"hello world, this is a longer prompt"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n, _ := body["input_tokens"].(float64); n <= 0 {
		t.Errorf("input_tokens = %v", body["input_tokens"])
	}
}

func TestE2EErrorShapesPerEndpoint(t *testing.T) {
	upstream := newFakeUpstream(t, "x")
	s := testProxyServer(t, upstream)

	cases := []struct {
		name    string
		handler http.HandlerFunc
		path    string
		body    string
		check   func(t *testing.T, body map[string]any)
	}{
		{"chat", s.handleChat, "/v1/chat/completions", `{"messages":[]}`, func(t *testing.T, body map[string]any) {
			if _, ok := body["error"].(map[string]any); !ok {
				t.Errorf("chat error body = %+v", body)
			}
		}},
		{"responses", s.handleResponses, "/v1/responses", `{}`, func(t *testing.T, body map[string]any) {
			errObj, _ := body["error"].(map[string]any)
			if errObj == nil || errObj["message"] == "" {
				t.Errorf("responses error body = %+v", body)
			}
		}},
		{"messages", s.handleMessages, "/v1/messages", `{"messages":[]}`, func(t *testing.T, body map[string]any) {
			if body["type"] != "error" {
				t.Errorf("anthropic error body = %+v", body)
			}
			errObj, _ := body["error"].(map[string]any)
			if errObj["type"] != "invalid_request_error" {
				t.Errorf("anthropic error type = %+v", errObj)
			}
		}},
	}
	for _, tc := range cases {
		rec := serve(t, s, tc.handler, tc.path, tc.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", tc.name, rec.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		tc.check(t, body)
	}
}

func TestE2EAssistantHistoryWithToolResults(t *testing.T) {
	// A second turn that replays the tool result must reach the upstream with
	// the transcript intact.
	upstream := newFakeUpstream(t, "the file says hi")
	s := testProxyServer(t, upstream)

	rec := serve(t, s, s.handleMessages, "/v1/messages", `{
	  "model":"smart","max_tokens":64,
	  "messages":[
	    {"role":"user","content":"read a.txt"},
	    {"role":"assistant","content":[
	      {"type":"text","text":"reading"},
	      {"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"a.txt"}}
	    ]},
	    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"hi"}]}
	  ],
	  "tools":[{"name":"read","input_schema":{"type":"object"}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	select {
	case got := <-upstream.got:
		for _, want := range []string{"read a.txt", "reading", "tool", "hi"} {
			if !strings.Contains(got, want) {
				t.Errorf("relayed prompt missing %q:\n%s", want, got)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("upstream never received the turn")
	}
}

var _ = fmt.Sprintf

func TestE2EResponsesStreamingToolOnlyHasNoMessageItem(t *testing.T) {
	upstream := newFakeUpstream(t, `<client_tool_call>{"name":"read","arguments":{"path":"a"}}</client_tool_call>`)
	s := testProxyServer(t, upstream)

	rec := serve(t, s, s.handleResponses, "/v1/responses", `{
	  "model":"smart","stream":true,"input":"read a",
	  "tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],
	  "tool_choice":"required"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, `"type":"message"`) {
		t.Fatalf("a tool-only answer must not emit an empty message item:\n%s", body)
	}
	if !strings.Contains(body, "event: response.function_call_arguments.done") {
		t.Fatalf("missing function call events:\n%s", body)
	}
	// The completed response carries only the function_call item.
	var output []any
	for _, p := range parseSSE(t, body) {
		if p["type"] == "response.completed" {
			resp, _ := p["response"].(map[string]any)
			output, _ = resp["output"].([]any)
		}
	}
	if len(output) != 1 {
		t.Fatalf("output = %+v", output)
	}
	if item, _ := output[0].(map[string]any); item["type"] != "function_call" {
		t.Errorf("output item = %+v", output[0])
	}
}
