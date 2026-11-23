package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"

	"quickwork-openai/internal/config"
)

const (
	defaultWSPath        = "/streaming/quickwork-chat-bidirectional-stream"
	defaultGatewayWSPath = "/ws/service/SpaceNeedleConversationService/operation/ChatBidirectionalStream"
	maxUserMessageBytes  = 63 * 1024
)

// ErrUserMessageTooLarge means the user_message frame cannot fit under the
// upstream 64 KB per-frame limit even after trimming older prompt text.
var ErrUserMessageTooLarge = errors.New("user_message exceeds the upstream 64 KB frame limit")

// errTruncated means the upstream stream ended after partial output but before
// any completion or terminal event, so the answer may be incomplete.
var errTruncated = errors.New("upstream stream ended before the turn completed")

type Upstream struct {
	cfg *config.Config
}

func NewUpstream(cfg *config.Config) *Upstream {
	return &Upstream{cfg: cfg}
}

// ChatInput is one upstream turn.
type ChatInput struct {
	Prompt        string
	Model         string
	ThreadID      string
	Title         string
	SystemPrompt  string
	ChatAgentID   string
	CreateSession bool
	DeleteSession bool
}

// ToolCallState is a native tool invocation reported by the QuickWork agent.
type ToolCallState struct {
	ID        string
	Name      string
	Arguments string
}

// Usage aggregates turn_usage events across a multi-step turn.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CacheReadTokens  int
	CacheWriteTokens int
}

func (u *Usage) add(v Usage) {
	u.PromptTokens += v.PromptTokens
	u.CompletionTokens += v.CompletionTokens
	u.TotalTokens += v.TotalTokens
	u.CacheReadTokens += v.CacheReadTokens
	u.CacheWriteTokens += v.CacheWriteTokens
}

// StreamEvent is a text delta or a native tool call from the upstream agent.
type StreamEvent struct {
	Type     string
	Text     string
	ToolCall *ToolCallState
}

type EventFunc func(StreamEvent) error

type ChatResult struct {
	Text      string
	ToolCalls []ToolCallState
	Usage     Usage
	ThreadID  string
}

// Chat sends one prompt through the QuickWork chat bidi WebSocket. Text
// deltas are reported through onEvent.
func (u *Upstream) Chat(ctx context.Context, sess *Session, in ChatInput, onEvent EventFunc) (ChatResult, error) {
	if strings.TrimSpace(in.Prompt) == "" {
		return ChatResult{}, fmt.Errorf("prompt is empty")
	}
	threadID := strings.TrimSpace(in.ThreadID)
	if threadID == "" {
		threadID = uuid()
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = titleFor(in.Prompt)
	}
	messageID := uuid()
	traceID := uuid()

	wsURL, headers, gateway, err := u.dialInfo(sess)
	if err != nil {
		return ChatResult{}, err
	}

	dialer := *websocket.DefaultDialer
	dialer.HandshakeTimeout = 20 * time.Second
	conn, resp, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		detail := ""
		if resp != nil {
			detail = fmt.Sprintf(" HTTP %d", resp.StatusCode)
		}
		return ChatResult{}, fmt.Errorf("upstream WebSocket dial failed%s: %w", detail, err)
	}
	defer conn.Close()

	writeMu := &sync.Mutex{}
	encode := func(v any) ([]byte, error) {
		body, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		if gateway {
			frame := gatewayEncode(string(body))
			if frame == nil {
				return nil, fmt.Errorf("encode gateway frame")
			}
			return frame, nil
		}
		// AIDP (enterprise and social) uses the block-style binary envelope:
		// {"eventType":"rawInputEvent","payload":{"payload":base64(json)}}.
		return aidpEncode(string(body)), nil
	}
	messageType := websocket.BinaryMessage
	send := func(v any) error {
		frame, err := encode(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(messageType, frame)
	}

	// Keep the socket alive while the agent is thinking.
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				writeMu.Lock()
				_ = conn.WriteControl(websocket.PingMessage, []byte("ping"), time.Now().Add(5*time.Second))
				writeMu.Unlock()
			}
		}
	}()
	// session_delete must be attempted even when ctx is cancelled, before the
	// connection is closed, or the upstream session is orphaned.
	var deleteOnce sync.Once
	sendDelete := func() {
		deleteOnce.Do(func() {
			if !in.DeleteSession {
				return
			}
			frame, err := encode(map[string]any{"type": "session_delete", "payload": map[string]any{"session_id": threadID}})
			if err == nil {
				writeMu.Lock()
				_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
				err = conn.WriteMessage(messageType, frame)
				writeMu.Unlock()
			}
			if err != nil {
				log.Printf("session_delete thread=%s failed: %v", threadID, err)
			}
		})
	}
	connDone := make(chan struct{})
	defer close(connDone)
	go func() {
		select {
		case <-ctx.Done():
			sendDelete()
			_ = conn.Close()
		case <-connDone:
		}
	}()

	// Handshake. The AIDP and Gateway paths use different session_connect shapes.
	var connectErr error
	if gateway {
		connectErr = send(map[string]any{"type": "session_connect", "session_id": sess.OutpostID})
	} else {
		connectErr = send(map[string]any{
			"type": "session_connect",
			"payload": map[string]any{
				"actor_id":               sess.ActorARN,
				"surfaceType":            sess.Surface,
				"outpostId":              sess.OutpostID,
				"appVersion":             u.cfg.AppVersion,
				"external_agent_sign_in": 1,
				"recents_promotion":      1,
			},
		})
	}
	if connectErr != nil {
		return ChatResult{}, fmt.Errorf("send session_connect: %w", connectErr)
	}
	if err := send(map[string]any{"type": "federate_status", "payload": map[string]any{}}); err != nil {
		return ChatResult{}, fmt.Errorf("send federate_status: %w", err)
	}
	if in.CreateSession {
		if err := send(map[string]any{
			"type":    "session_create",
			"payload": map[string]any{"session_id": threadID, "title": title},
		}); err != nil {
			return ChatResult{}, fmt.Errorf("send session_create: %w", err)
		}
	}

	payload := map[string]any{
		"text":               relayPrompt(in.SystemPrompt, in.Prompt),
		"send_origin":        "programmatic",
		"had_physical_input": false,
		"surfaceType":        sess.Surface,
		"outpostId":          sess.OutpostID,
		"appVersion":         u.cfg.AppVersion,
		"traceId":            traceID,
		"message_id":         messageID,
		"thread_id":          threadID,
		"agent_mode":         in.Model,
		"user_timezone":      u.cfg.Timezone,
		"user_locale":        u.cfg.Locale,
		"chat_agent_id":      nil,
		"auto_routing":       false,
		"web_search_enabled": false,
		"pre_approved_tools": []any{},
		"preload_skills":     []any{},
		"panel_edits":        []any{},
		"custom_prompt":      nil,
	}
	if id := strings.TrimSpace(in.ChatAgentID); id != "" {
		payload["chat_agent_id"] = id
	}
	// The service ignores a per-turn "system_prompt" field and treats
	// "custom_prompt" as low-trust saved preferences, so caller instructions are
	// carried at the head of the user_message text instead (see relayPrompt).
	if u.cfg.ThinkingEffort != "" {
		payload["thinking_effort"] = u.cfg.ThinkingEffort
	}

	// A single user_message is capped at 64 KB. When the caller's prompt is
	// what pushes the frame over the limit, upload it out of band and send a
	// short reference instead; the file bytes never enter the WebSocket frame.
	// The normal trimming path below remains the fallback when the tenant
	// attachment service is unavailable or blocked. Gateway transports use a
	// different upload route and keep the trimming path for now.
	if !isGatewayProvider(sess.Provider) && strings.TrimSpace(sess.TenantURL) != "" {
		if n, err := userMessageBytes(payload); err == nil && n > maxUserMessageBytes {
			name := attachmentPromptName(messageID)
			if shouldAttachPrompt(len(in.Prompt)) {
				att, attErr := uploadPromptAttachment(ctx, sess, threadID, name, []byte(in.Prompt))
				if attErr != nil {
					log.Printf("user_message attachment unavailable thread=%s bytes=%d: %v", threadID, len(in.Prompt), attErr)
				} else if att.Path != "" {
					payload["text"] = relayPrompt(in.SystemPrompt, attachmentShortText(att.Name))
					meta := map[string]any{
						"type":     "file",
						"name":     att.Name,
						"size":     att.Size,
						"mimeType": attachmentMimeType,
						"path":     att.Path,
					}
					if att.ID != "" {
						meta["attachmentId"] = att.ID
					}
					payload["attachments"] = []map[string]any{meta}
					log.Printf("user_message attachment thread=%s bytes=%d name=%s id=%s path=%s",
						threadID, att.Size, att.Name, att.ID, att.Path)
				}
			}
		}
	}

	if err := fitUserMessage(payload); err != nil {
		return ChatResult{}, err
	}
	if err := send(map[string]any{"type": "user_message", "payload": payload}); err != nil {
		return ChatResult{}, fmt.Errorf("send user_message: %w", err)
	}

	result, err := u.readEvents(ctx, conn, gateway, threadID, onEvent)
	result.ThreadID = threadID
	sendDelete()
	if err != nil {
		return result, err
	}
	return result, nil
}

func (u *Upstream) dialInfo(sess *Session) (wsURL string, headers http.Header, gateway bool, err error) {
	jwt := sess.jwt()
	if jwt == "" {
		return "", nil, false, fmt.Errorf("missing jwt")
	}
	gateway = isGatewayProvider(sess.Provider)

	host := strings.TrimSpace(sess.WSHost)
	path := strings.TrimSpace(sess.WSPath)
	if path == "" {
		if gateway {
			path = defaultGatewayWSPath
		} else {
			path = defaultWSPath
		}
	}
	switch {
	case strings.HasPrefix(host, "wss://") || strings.HasPrefix(host, "ws://"):
		host = strings.TrimRight(host, "/")
	case strings.HasPrefix(host, "https://"):
		host = "wss://" + strings.TrimPrefix(host, "https://")
	default:
		host = "wss://" + strings.TrimRight(host, "/")
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		wsURL = path
	} else {
		if !strings.HasPrefix(path, "/") {
			path = "/" + path
		}
		wsURL = host + path
	}

	parsed, err := url.Parse(wsURL)
	if err != nil {
		return "", nil, false, fmt.Errorf("invalid ws_url: %w", err)
	}
	if !gateway {
		q := parsed.Query()
		if sess.Surface != "" {
			q.Set("surfaceType", sess.Surface)
		}
		if sess.OutpostID != "" {
			q.Set("outpostId", sess.OutpostID)
		}
		if sess.WorkingSessionID != "" {
			q.Set("workingSessionId", sess.WorkingSessionID)
		}
		// Social AIDP transports accept the bearer in the query; enterprise
		// relies on the Authorization header injected below.
		if strings.EqualFold(sess.Provider, "social") {
			q.Set("token", jwt)
		}
		parsed.RawQuery = q.Encode()
	}

	headers = http.Header{}
	headers.Set("Authorization", "Bearer "+jwt)
	headers.Set("Origin", "file://")
	headers.Set("X-Surface-Type", sess.Surface)
	headers.Set("X-Outpost-Id", sess.OutpostID)
	headers.Set("X-Agent-Service-Type", "UNIFIED_QUICK")
	if sess.WorkingSessionID != "" {
		headers.Set("X-Working-Session-Id", sess.WorkingSessionID)
	}
	return parsed.String(), headers, gateway, nil
}

func (u *Upstream) readEvents(ctx context.Context, conn *websocket.Conn, gateway bool, threadID string, onEvent EventFunc) (ChatResult, error) {
	var result ChatResult
	var text strings.Builder
	sawMessageComplete := false
	var completeDeadline time.Time
	eventCounts := make(map[string]int)

	toolCalls := make(map[string]*ToolCallState)
	toolOrder := make([]string, 0, 2)
	toolSent := make(map[string]bool)
	rawArgs := make(map[string]string)
	argsPartial := make(map[string]bool)

	getTool := func(id, name string) *ToolCallState {
		id = strings.TrimSpace(id)
		if id == "" {
			id = "call_" + strings.ReplaceAll(uuid(), "-", "")
		}
		tc := toolCalls[id]
		if tc == nil {
			tc = &ToolCallState{ID: id}
			toolCalls[id] = tc
			toolOrder = append(toolOrder, id)
		}
		if n := strings.TrimSpace(name); n != "" {
			tc.Name = n
		}
		return tc
	}
	setToolArgs := func(tc *ToolCallState, raw json.RawMessage) {
		if tc == nil || len(raw) == 0 {
			return
		}
		rawArgs[tc.ID] = rawArgumentText(raw)
		argsPartial[tc.ID] = false
		tc.Arguments = normalizeArguments(raw)
	}
	appendToolArgs := func(tc *ToolCallState, delta string) {
		if tc == nil || delta == "" {
			return
		}
		// tool_progress carries an input fragment. Once a tool has been
		// finalized by tool_arguments/tool_call, later progress deltas are
		// ignored instead of corrupting the JSON.
		if !argsPartial[tc.ID] && strings.TrimSpace(tc.Arguments) != "{}" && strings.TrimSpace(tc.Arguments) != "" {
			return
		}
		rawArgs[tc.ID] += delta
		argsPartial[tc.ID] = true
		tc.Arguments = normalizeArguments(json.RawMessage(rawArgs[tc.ID]))
	}
	emitTool := func(tc *ToolCallState) error {
		if tc == nil || onEvent == nil || toolSent[tc.ID] {
			return nil
		}
		toolSent[tc.ID] = true
		copy := *tc
		return onEvent(StreamEvent{Type: "tool_call", ToolCall: &copy})
	}

	finish := func() (ChatResult, error) {
		result.Text = text.String()
		result.ToolCalls = result.ToolCalls[:0]
		for _, id := range toolOrder {
			tc := toolCalls[id]
			if tc == nil {
				continue
			}
			if raw := strings.TrimSpace(rawArgs[id]); raw != "" {
				tc.Arguments = normalizeArguments(json.RawMessage(raw))
			}
			if strings.TrimSpace(tc.Name) == "" {
				tc.Name = "unknown"
			}
			if strings.TrimSpace(tc.Arguments) == "" {
				tc.Arguments = "{}"
			}
			result.ToolCalls = append(result.ToolCalls, *tc)
			if err := emitTool(tc); err != nil {
				return result, err
			}
		}
		if len(eventCounts) > 0 {
			log.Printf("upstream events thread=%s counts=%v text_chars=%d tool_calls=%d",
				threadID, eventCounts, text.Len(), len(toolOrder))
		}
		return result, nil
	}

	terminal := map[string]bool{
		"turn_completed": true,
		"turn_aborted":   true,
		"turn_ended":     true,
		"stream_ended":   true,
		"chat_complete":  true,
	}
	var pendingFrames []string
	for {
		select {
		case <-ctx.Done():
			res, _ := finish()
			return res, ctx.Err()
		default:
		}
		readWait := 2 * time.Minute
		if !completeDeadline.IsZero() {
			if remaining := time.Until(completeDeadline); remaining > 0 {
				readWait = remaining
			} else {
				return finish()
			}
		}
		if len(pendingFrames) == 0 {
			_ = conn.SetReadDeadline(time.Now().Add(readWait))
			msgType, data, err := conn.ReadMessage()
			if err != nil {
				res, emitErr := finish()
				if emitErr != nil {
					return res, emitErr
				}
				// Partial output only counts as success once the turn reached
				// a completion event; otherwise the answer may be cut short.
				if sawMessageComplete && (text.Len() > 0 || len(toolOrder) > 0) {
					return res, nil
				}
				if text.Len() > 0 || len(toolOrder) > 0 {
					return res, fmt.Errorf("%w: upstream WebSocket read: %v", errTruncated, err)
				}
				return res, fmt.Errorf("upstream WebSocket read: %w", err)
			}
			if msgType == websocket.TextMessage {
				pendingFrames = append(pendingFrames, string(data))
			} else {
				var frames []string
				if gateway {
					frames, err = gatewayDecode(data)
				} else {
					frames, err = aidpDecode(data)
				}
				if err != nil {
					log.Printf("upstream frame decode: %v", err)
					if len(frames) == 0 {
						res, emitErr := finish()
						if emitErr != nil {
							return res, emitErr
						}
						return res, err
					}
				}
				pendingFrames = append(pendingFrames, frames...)
			}
		}
		jsonStr := pendingFrames[0]
		pendingFrames = pendingFrames[1:]

		var env struct {
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
			MessageID string          `json:"message_id"`
			Message   string          `json:"message"`
			Code      string          `json:"code"`
			Error     string          `json:"error"`
			ErrorCode string          `json:"error_code"`
		}
		if err := json.Unmarshal([]byte(jsonStr), &env); err != nil {
			continue
		}
		eventCounts[env.Type]++
		switch env.Type {
		case "text_delta":
			delta := deltaText(env.Payload)
			if delta == "" {
				continue
			}
			text.WriteString(delta)
			if onEvent != nil {
				if err := onEvent(StreamEvent{Type: "text", Text: delta}); err != nil {
					res, _ := finish()
					return res, err
				}
			}
		case "text_complete":
			if text.Len() == 0 {
				var p struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(env.Payload, &p)
				if p.Text != "" {
					text.WriteString(p.Text)
					if onEvent != nil {
						if err := onEvent(StreamEvent{Type: "text", Text: p.Text}); err != nil {
							res, _ := finish()
							return res, err
						}
					}
				}
			}
			sawMessageComplete = true
		case "turn_usage":
			var p struct {
				InputTokens      int `json:"input_tokens"`
				OutputTokens     int `json:"output_tokens"`
				TotalTokens      int `json:"total_tokens"`
				CacheReadTokens  int `json:"cache_read_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			}
			if err := json.Unmarshal(env.Payload, &p); err == nil {
				result.Usage.add(Usage{
					PromptTokens:     p.InputTokens,
					CompletionTokens: p.OutputTokens,
					TotalTokens:      p.TotalTokens,
					CacheReadTokens:  p.CacheReadTokens,
					CacheWriteTokens: p.CacheWriteTokens,
				})
			}
		case "tool_start", "tool_arguments", "tool_arguments_edited":
			var p struct {
				ToolName   string          `json:"tool_name"`
				ToolID     string          `json:"tool_id"`
				ToolCallID string          `json:"tool_call_id"`
				Arguments  json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			setToolArgs(tc, p.Arguments)
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_progress":
			var p struct {
				ToolID     string `json:"tool_id"`
				ToolCallID string `json:"tool_call_id"`
				ToolName   string `json:"tool_name"`
				InputDelta string `json:"input_delta"`
				Progress   struct {
					InputDelta string `json:"input_delta"`
				} `json:"progress"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			tc := getTool(id, p.ToolName)
			delta := p.InputDelta
			if delta == "" {
				delta = p.Progress.InputDelta
			}
			appendToolArgs(tc, delta)
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "tool_call", "tool_call_update", "tool_complete":
			var p struct {
				ToolCallID      string          `json:"toolCallId"`
				ToolCallIDSnake string          `json:"tool_call_id"`
				ToolID          string          `json:"tool_id"`
				ToolName        string          `json:"tool_name"`
				Title           string          `json:"title"`
				RawInput        json.RawMessage `json:"rawInput"`
				RawInputSnake   json.RawMessage `json:"raw_input"`
				Arguments       json.RawMessage `json:"arguments"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			id := p.ToolID
			if id == "" {
				id = p.ToolCallID
			}
			if id == "" {
				id = p.ToolCallIDSnake
			}
			name := p.ToolName
			if name == "" {
				name = p.Title
			}
			tc := getTool(id, name)
			args := p.RawInput
			if len(args) == 0 {
				args = p.RawInputSnake
			}
			if len(args) == 0 {
				args = p.Arguments
			}
			setToolArgs(tc, args)
			if err := emitTool(tc); err != nil {
				res, _ := finish()
				return res, err
			}
		case "message_complete":
			sawMessageComplete = true
			var p struct {
				ContinuationExpected bool `json:"continuation_expected"`
				ToolCalls            []struct {
					ID         string          `json:"id"`
					ToolCallID string          `json:"tool_call_id"`
					Name       string          `json:"name"`
					ToolName   string          `json:"tool_name"`
					Arguments  json.RawMessage `json:"arguments"`
					Function   struct {
						Name      string          `json:"name"`
						Arguments json.RawMessage `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			}
			_ = json.Unmarshal(env.Payload, &p)
			for _, item := range p.ToolCalls {
				id := item.ID
				if id == "" {
					id = item.ToolCallID
				}
				name := item.Name
				if name == "" {
					name = item.ToolName
				}
				if name == "" {
					name = item.Function.Name
				}
				tc := getTool(id, name)
				args := item.Arguments
				if len(args) == 0 {
					args = item.Function.Arguments
				}
				setToolArgs(tc, args)
				if err := emitTool(tc); err != nil {
					res, _ := finish()
					return res, err
				}
			}
			if p.ContinuationExpected {
				completeDeadline = time.Time{}
			} else {
				completeDeadline = time.Now().Add(5 * time.Second)
			}
		case "error", "session_error", "stream_error", "turn_failed":
			res, _ := finish()
			message := env.Message
			if message == "" {
				message = env.Error
			}
			code := env.Code
			if code == "" {
				code = env.ErrorCode
			}
			log.Printf("upstream %s error=%q code=%q payload=%s frame=%s",
				env.Type, message, code,
				truncateText(string(env.Payload), 1000), truncateText(jsonStr, 1500))
			return res, fmt.Errorf("upstream %s: %s", env.Type, errorText(env.Payload, message, code))
		default:
			if terminal[env.Type] {
				return finish()
			}
			if env.Type == "ping" {
				// Best effort pong; ignore errors.
				_ = conn.WriteControl(websocket.PongMessage, []byte("pong"), time.Now().Add(3*time.Second))
			}
			if sawMessageComplete && env.Type == "session_list" {
				continue
			}
		}
	}
}

func deltaText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var p struct {
		Delta string `json:"delta"`
		Text  string `json:"text"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if p.Delta != "" {
		return p.Delta
	}
	return p.Text
}

func errorText(raw json.RawMessage, fallback, code string) string {
	if len(raw) > 0 {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			if s := nestedErrorText(v); s != "" {
				return s
			}
			if compact, err := json.Marshal(v); err == nil && len(compact) > 0 {
				return truncateText(string(compact), 800)
			}
		}
		if s := strings.TrimSpace(string(raw)); s != "" {
			return truncateText(s, 800)
		}
	}
	for _, s := range []string{fallback, code} {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return "unknown upstream error"
}

func nestedErrorText(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case map[string]any:
		for _, key := range []string{"message", "error", "detail", "reason", "description", "title"} {
			if s, ok := x[key]; ok {
				if text := nestedErrorText(s); text != "" {
					return text
				}
			}
		}
	case []any:
		for _, item := range x {
			if text := nestedErrorText(item); text != "" {
				return text
			}
		}
	}
	return ""
}

func truncateText(s string, max int) string {
	s = strings.TrimSpace(s)
	if max <= 0 || len(s) <= max {
		return s
	}
	return utf8Prefix(s, max) + "…"
}

// utf8Prefix returns at most max bytes of s without splitting a rune.
func utf8Prefix(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// fitUserMessage keeps the user_message payload under the upstream limit.
// Upstream checks the reassembled user_message JSON, so WebSocket
// fragmentation or multiple user_message frames cannot carry one oversized
// turn: extra frames would become separate turns. Trim within the single frame
// instead. The middle of the prompt text is removed, preserving its beginning
// and its end (where the tool interface and the user request sit). Trimming is
// byte-based because the upstream limit is bytes, not runes.
func fitUserMessage(payload map[string]any) error {
	wrap := func() ([]byte, error) {
		return json.Marshal(map[string]any{"type": "user_message", "payload": payload})
	}
	body, err := wrap()
	if err != nil {
		return err
	}
	if len(body) <= maxUserMessageBytes {
		return nil
	}
	original := len(body)
	text, _ := payload["text"].(string)

	// 1. The prompt text is the normal overflow source; trim its middle first.
	if len(text) > 4096 {
		payload["text"] = shrinkToLimit(text, func(s string) ([]byte, error) {
			payload["text"] = s
			return wrap()
		}, textTrimMarker)
		if body, err = wrap(); err != nil {
			return err
		}
	}

	// 2. If metadata plus text still overflow, shrink text down to the marker.
	if len(body) > maxUserMessageBytes && text != "" {
		payload["text"] = shrinkToLimit(text, func(s string) ([]byte, error) {
			payload["text"] = s
			return wrap()
		}, textTrimMarker)
		if body, err = wrap(); err != nil {
			return err
		}
	}

	if len(body) > maxUserMessageBytes {
		return fmt.Errorf("%w: %d bytes after trimming the prompt", ErrUserMessageTooLarge, len(body))
	}
	log.Printf("user_message trimmed %d -> %d bytes (limit %d)", original, len(body), maxUserMessageBytes)
	return nil
}

const textTrimMarker = "\n\n[earlier conversation trimmed to fit the upstream 64 KB frame limit]\n\n"

// shrinkToLimit removes bytes from the middle of s until wrap(s) fits. It
// preserves the beginning and the end, so recent context and trailing
// authority notices survive.
func shrinkToLimit(s string, wrap func(string) ([]byte, error), marker string) string {
	out := s
	for range 6 {
		body, err := wrap(out)
		if err != nil || len(body) <= maxUserMessageBytes {
			return out
		}
		over := len(body) - maxUserMessageBytes
		next := shrinkMiddle(out, over, marker)
		if next == out {
			return out
		}
		out = next
	}
	return out
}

// shrinkMiddle removes about `remove` bytes from s while keeping 1/4 of the
// retained text from the head and 3/4 from the tail. Slices are adjusted to
// UTF-8 rune boundaries so the JSON stays valid.
func shrinkMiddle(s string, remove int, marker string) string {
	const minKeep = 4096
	target := len(s) - remove - len(marker)
	if target < minKeep {
		target = minKeep
	}
	if target >= len(s) {
		return s
	}
	head := target / 4
	if head > len(s) {
		head = len(s)
	}
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	tailStart := len(s) - (target - head)
	if tailStart < head {
		tailStart = head
	}
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return s[:head] + marker + s[tailStart:]
}

func titleFor(prompt string) string {
	clean := strings.Join(strings.Fields(prompt), " ")
	if len(clean) > 60 {
		clean = utf8Prefix(clean, 60) + "…"
	}
	if clean == "" {
		clean = "New Conversation"
	}
	return clean
}
