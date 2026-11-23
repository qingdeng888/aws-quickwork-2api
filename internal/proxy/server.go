package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/netproxy"
	"quickwork-openai/internal/store"
)

type Server struct {
	cfg      *config.Config
	accounts *accounts.Manager
	tokens   atomic.Pointer[APIKeyAuth]
	upstream *Upstream
	sessions *conversationStore
	records  chatRecorder

	// adminHandler, when set by MountAdmin, is served under adminPrefix on the
	// same listener as the OpenAI-compatible API.
	adminHandler http.Handler
	adminPrefix  string
}

func NewServer(cfg *config.Config, mgr *accounts.Manager) *Server {
	s := &Server{
		cfg:      cfg,
		accounts: mgr,
		upstream: NewUpstream(cfg),
		sessions: newConversationStore(cfg.SessionTTL.Duration(), 256),
	}
	s.tokens.Store(NewAPIKeyAuth(cfg.APIKeys))
	return s
}

// SetAPIKeys replaces the accepted /v1 credentials without a restart.
func (s *Server) SetAPIKeys(keys []string) {
	s.tokens.Store(NewAPIKeyAuth(keys))
}

// APIKeys returns the accepted /v1 credentials.
func (s *Server) APIKeys() []string {
	return s.tokens.Load().Keys()
}

// APIKeyCount reports how many /v1 credentials are configured.
func (s *Server) APIKeyCount() int {
	return s.tokens.Load().Count()
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/responses", s.handleResponses)
	mux.HandleFunc("/v1/messages", s.handleMessages)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleCountTokens)
	if s.adminHandler != nil {
		prefix := s.adminPrefix
		if prefix == "" {
			prefix = "/admin/"
		}
		trimmed := strings.TrimSuffix(prefix, "/")
		mux.Handle(trimmed, http.RedirectHandler(prefix, http.StatusTemporaryRedirect))
		mux.Handle(prefix, s.adminHandler)
	}
	return s.withCORS(mux)
}

// MountAdmin serves h under prefix on the proxy's own listener, so the
// management UI and the API share an address. It must be called before
// Handler.
func (s *Server) MountAdmin(prefix string, h http.Handler) {
	if prefix == "" {
		prefix = "/admin/"
	}
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	s.adminPrefix = prefix
	s.adminHandler = h
}

// ChatHandler exposes the chat endpoint for the management UI playground,
// which reuses the full upstream path in-process.
func (s *Server) ChatHandler() http.Handler { return http.HandlerFunc(s.handleChat) }

// ChatRecords returns up to limit finished requests, newest first.
func (s *Server) ChatRecords(limit int) []ChatRecord { return s.records.list(limit) }

// ChatStats aggregates the requests currently held in memory.
func (s *Server) ChatStats() ChatStats { return s.records.stats() }

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The management interface is same-origin only: a wildcard CORS
		// header would let any page on the internet drive it with cookies.
		if strings.HasPrefix(r.URL.Path, "/admin") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Thread-Id, X-Conversation-Id")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "quickwork-openai"})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	if !s.tokens.Load().Allowed(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "invalid proxy API key")
		return
	}
	created := time.Now().Unix()
	data := make([]map[string]any, 0, len(s.cfg.Models))
	for _, id := range s.cfg.Models {
		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"created":  created,
			"owned_by": "quickwork",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// readAPIRequest performs the checks shared by every OpenAI-style endpoint and
// returns the request body. Errors are written in the caller's dialect.
func (s *Server) readAPIRequest(w http.ResponseWriter, r *http.Request, flavor wireFlavor) ([]byte, bool) {
	if r.Method != http.MethodPost {
		writeFlavorError(w, flavor, http.StatusMethodNotAllowed, "method_not_allowed", "POST required")
		return nil, false
	}
	if !s.tokens.Load().Allowed(r) && !adminAuthenticated(r) {
		writeFlavorError(w, flavor, http.StatusUnauthorized, "invalid_api_key", "invalid proxy API key")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeFlavorError(w, flavor, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds the size limit")
			return nil, false
		}
		writeFlavorError(w, flavor, http.StatusBadRequest, "invalid_request_error", "could not read request body: "+err.Error())
		return nil, false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		writeFlavorError(w, flavor, http.StatusBadRequest, "invalid_request_error", "request body is empty")
		return nil, false
	}
	return body, true
}

// decodeChatBody parses an OpenAI chat completions body.
func decodeChatBody(body []byte) (*ChatRequest, error) {
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// handleChat serves POST /v1/chat/completions.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readAPIRequest(w, r, wireChat)
	if !ok {
		return
	}
	req, err := decodeChatBody(body)
	if err != nil {
		writeFlavorError(w, wireChat, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return
	}
	if req.N > 1 {
		writeFlavorError(w, wireChat, http.StatusBadRequest, "invalid_request_error", "n > 1 is not supported")
		return
	}
	s.serveChat(w, r, wireChat, req)
}

// handleResponses serves POST /v1/responses (OpenAI Responses API).
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readAPIRequest(w, r, wireResponses)
	if !ok {
		return
	}
	req, err := chatFromResponses(body)
	if err != nil {
		writeFlavorError(w, wireResponses, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.serveChat(w, r, wireResponses, req)
}

// handleMessages serves POST /v1/messages (Anthropic Messages API).
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readAPIRequest(w, r, wireAnthropic)
	if !ok {
		return
	}
	req, err := chatFromAnthropic(body)
	if err != nil {
		writeFlavorError(w, wireAnthropic, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.serveChat(w, r, wireAnthropic, req)
}

// handleCountTokens serves POST /v1/messages/count_tokens. Anthropic clients
// call it before large requests; the figure is an estimate because the
// upstream does not expose a tokenizer.
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, ok := s.readAPIRequest(w, r, wireAnthropic)
	if !ok {
		return
	}
	req, err := chatFromAnthropic(body)
	if err != nil {
		writeFlavorError(w, wireAnthropic, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	prompt, err := formatPrompt(req.Messages)
	if err != nil {
		writeFlavorError(w, wireAnthropic, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateTokens(prompt)})
}

// serveChat runs one chat request through the upstream pipeline and renders the
// answer in the dialect selected by flavor.
func (s *Server) serveChat(w http.ResponseWriter, r *http.Request, flavor wireFlavor, req *ChatRequest) {
	prompt, err := formatPrompt(req.Messages)
	if err != nil {
		writeFlavorError(w, flavor, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	model := s.mapModel(req.Model)
	allowedTools := requestedToolNames(req)
	choiceMode, choiceName := parseToolChoice(req.ToolChoice)
	if choiceMode == "function" && choiceName != "" {
		allowedTools[choiceName] = true
	}
	parallelTools := true
	if req.ParallelToolCalls != nil {
		parallelTools = *req.ParallelToolCalls
	}
	bridgePrompt := buildToolBridgePrompt(req, choiceMode, choiceName, parallelTools)
	bridgeActive := bridgePrompt != ""
	systemPromptText := systemPrompt(req.Messages, bridgePrompt)
	// QuickWork has no per-turn system prompt channel, so the caller's
	// instructions are attached at the head of the user turn by the upstream
	// layer, while the transcript stays a separate field for attachment upload.
	instructions := ""
	if bridgeActive || hasCallerInstructions(req.Messages) {
		instructions = systemPromptText
	}

	conv, err := s.sessions.acquire(r.Context(), conversationKey(r, req, systemPromptText))
	if err != nil {
		writeFlavorError(w, flavor, http.StatusServiceUnavailable, "request_cancelled", "request ended while waiting for the conversation: "+err.Error())
		return
	}
	defer s.sessions.release(conv)

	resume := false
	promptText := prompt
	if s.cfg.SessionReuse && conv.systemPrompt == systemPromptText {
		if delta, ok := conv.delta(req.Messages); ok {
			if deltaPrompt, err := formatPrompt(delta); err == nil && hasVisibleText(deltaPrompt) {
				promptText = deltaPrompt
				resume = true
			}
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout.Duration())
	defer cancel()

	threadID := strings.TrimSpace(r.Header.Get("X-Thread-Id"))
	id := flavor.idPrefix() + strings.ReplaceAll(uuid(), "-", "")
	created := time.Now().Unix()
	// Responses and Messages echo the model the caller asked for.
	outModel := model
	if flavor != wireChat {
		if requested := strings.TrimSpace(req.Model); requested != "" {
			outModel = requested
		}
	}
	in := ChatInput{
		Prompt:        promptText,
		Model:         model,
		ThreadID:      threadID,
		Title:         titleFor(lastUserText(req.Messages)),
		SystemPrompt:  instructions,
		ChatAgentID:   s.cfg.ChatAgentID,
		CreateSession: true,
		DeleteSession: !s.cfg.SessionReuse && !s.cfg.KeepSessions,
	}
	if resume {
		in.ThreadID = conv.threadID
		in.CreateSession = false
	}

	startedAt := time.Now()
	maxAttempts := s.maxAttempts(ctx)
	if resume {
		maxAttempts++
	}
	attempts := 0
	var usedAccount *store.Account
	requestedModel := strings.TrimSpace(req.Model)
	if requestedModel == "" {
		requestedModel = "(default)"
	}
	log.Printf("chat %s start model=%s upstream_model=%s stream=%t messages=%d prompt_chars=%d system_chars=%d reuse=%t thread=%q",
		id, requestedModel, model, req.Stream, len(req.Messages), len(promptText), len(systemPromptText), resume, in.ThreadID)

	selectAccount := func(attempt int) (*store.Account, error) {
		if resume && attempt == 1 && conv.accountID != 0 {
			acct, err := s.accounts.NextPinned(ctx, conv.accountID)
			if err == nil {
				return acct, nil
			}
			log.Printf("chat %s pinned account %d unavailable: %v; starting a new session",
				id, conv.accountID, err)
			conv.reset()
			resume = false
			in.ThreadID = ""
			in.CreateSession = true
			in.Prompt = prompt
		}
		return s.accounts.Next(ctx)
	}

	if !req.Stream {
		var res ChatResult
		var lastErr error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			attempts = attempt
			acct, err := selectAccount(attempt)
			if err != nil {
				lastErr = err
				log.Printf("chat %s attempt=%d/%d account=none error=%v", id, attempt, maxAttempts, err)
				break
			}
			usedAccount = acct
			sess := SessionFromAccount(acct, s.cfg)
			if err := sess.validate(); err != nil {
				s.accounts.ReportFailure(ctx, acct.ID, err)
				lastErr = err
				log.Printf("chat %s attempt=%d/%d account=%s invalid_session error=%v",
					id, attempt, maxAttempts, accountLogLabel(acct), err)
				continue
			}
			proxyURL, proxyErr := s.accounts.ResolveProxy(ctx, acct)
			if proxyErr != nil {
				lastErr = proxyErr
				log.Printf("chat %s attempt=%d/%d account=%s proxy error=%v",
					id, attempt, maxAttempts, accountLogLabel(acct), proxyErr)
				continue
			}
			sess.Proxy = proxyURL
			attemptCtx := netproxy.WithProxy(ctx, proxyURL)
			log.Printf("chat %s attempt=%d/%d account=%s proxy=%s", id, attempt, maxAttempts, accountLogLabel(acct), proxyLogLabel(proxyURL))
			res, err = s.upstream.Chat(attemptCtx, sess, in, nil)
			if err == nil {
				lastErr = nil
				break
			}
			lastErr = err
			if !errors.Is(err, ErrUserMessageTooLarge) && ctx.Err() == nil {
				s.accounts.ReportFailure(ctx, acct.ID, err)
			}
			log.Printf("chat %s attempt=%d/%d account=%s failed error=%v",
				id, attempt, maxAttempts, accountLogLabel(acct), err)
			if errors.Is(err, ErrUserMessageTooLarge) {
				break
			}
			if resume {
				conv.reset()
				resume = false
				in.ThreadID = ""
				in.CreateSession = true
				in.Prompt = prompt
				log.Printf("chat %s session resume failed; retrying with full transcript", id)
				continue
			}
			if ctx.Err() != nil {
				log.Printf("chat %s request context ended: %v", id, ctx.Err())
				break
			}
		}
		if lastErr != nil {
			conv.reset()
			status, code := http.StatusBadGateway, "upstream_error"
			switch {
			case errors.Is(lastErr, accounts.ErrNoAccounts):
				status, code = http.StatusUnauthorized, "upstream_session_unavailable"
			case errors.Is(lastErr, ErrUserMessageTooLarge):
				status, code = http.StatusBadRequest, "request_too_large"
			}
			s.logChatDone(req.Stream, id, startedAt, in, usedAccount, attempts, "failed", "", res.Usage, 0, lastErr)
			writeFlavorError(w, flavor, status, code, lastErr.Error())
			return
		}
		if s.cfg.SessionReuse && res.ThreadID != "" {
			conv.threadID = res.ThreadID
			conv.accountID = usedAccount.ID
			conv.messages = append([]Message(nil), req.Messages...)
			conv.systemPrompt = systemPromptText
		}
		text := res.Text
		var replies []ToolCallReply
		if bridgeActive {
			text, replies = parseClientToolCalls(text, allowedTools)
			if len(replies) == 0 {
				replies = nativeToolReplies(res.ToolCalls, allowedTools)
			}
			replies = limitToolReplies(replies, choiceMode, choiceName, parallelTools)
		}
		toolCalls := replyToolCalls(replies)
		if !hasVisibleText(text) && len(toolCalls) == 0 {
			errEmpty := fmt.Errorf("upstream returned no visible text or tool calls (completion_tokens=%d)", res.Usage.CompletionTokens)
			s.logChatDone(req.Stream, id, startedAt, in, usedAccount, attempts, "empty", "", res.Usage, 0, errEmpty)
			writeFlavorError(w, flavor, http.StatusBadGateway, "upstream_error", errEmpty.Error())
			return
		}
		finish := "stop"
		if len(toolCalls) > 0 {
			finish = "tool_calls"
		}
		s.logChatDone(req.Stream, id, startedAt, in, usedAccount, attempts, "ok", finish, res.Usage, len(toolCalls), nil)
		// Expose the upstream thread so a client can continue the conversation
		// even without streaming (streaming already sets it in sendHeaders).
		if res.ThreadID != "" {
			w.Header().Set("X-Thread-Id", res.ThreadID)
		}
		writeFlavorCompletion(w, flavor, id, created, outModel, text, toolCalls, finish, res.Usage)
		return
	}

	if _, ok := w.(http.Flusher); !ok {
		writeFlavorError(w, flavor, http.StatusInternalServerError, "streaming_unavailable", "response writer does not support flushing")
		return
	}
	// The stream writer owns the SSE headers and the two flags that decide
	// whether a retry is still safe: started means real content reached the
	// client, headersSent only means the response began (keep-alive comments
	// while the tool bridge buffers still allow a retry).
	sw := newStreamWriter(w, flavor, id, outModel, created,
		flavor == wireChat && req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
	sw.threadID = threadID

	sawVisibleText := false
	var bridgeBuffer strings.Builder

	// While the tool bridge buffers the whole answer, keep idle proxies and
	// clients from timing out. Only headers and comments are sent, never the
	// role chunk, so failed attempts can still retry.
	stopHeartbeat := func() {}
	if bridgeActive {
		sw.sendHeaders()
		hbDone := make(chan struct{})
		hbExited := make(chan struct{})
		go func() {
			defer close(hbExited)
			ticker := time.NewTicker(15 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-hbDone:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					sw.keepAlive()
				}
			}
		}()
		var once sync.Once
		stopHeartbeat = func() {
			once.Do(func() {
				close(hbDone)
				<-hbExited
			})
		}
	}
	defer stopHeartbeat()

	var res ChatResult
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		attempts = attempt
		acct, err := selectAccount(attempt)
		if err != nil {
			lastErr = err
			log.Printf("chat %s attempt=%d/%d account=none error=%v", id, attempt, maxAttempts, err)
			break
		}
		usedAccount = acct
		sess := SessionFromAccount(acct, s.cfg)
		if err := sess.validate(); err != nil {
			s.accounts.ReportFailure(ctx, acct.ID, err)
			lastErr = err
			log.Printf("chat %s attempt=%d/%d account=%s invalid_session error=%v",
				id, attempt, maxAttempts, accountLogLabel(acct), err)
			continue
		}
		proxyURL, proxyErr := s.accounts.ResolveProxy(ctx, acct)
		if proxyErr != nil {
			lastErr = proxyErr
			log.Printf("chat %s attempt=%d/%d account=%s proxy error=%v",
				id, attempt, maxAttempts, accountLogLabel(acct), proxyErr)
			continue
		}
		sess.Proxy = proxyURL
		attemptCtx := netproxy.WithProxy(ctx, proxyURL)
		log.Printf("chat %s attempt=%d/%d account=%s proxy=%s", id, attempt, maxAttempts, accountLogLabel(acct), proxyLogLabel(proxyURL))
		res, err = s.upstream.Chat(attemptCtx, sess, in, func(ev StreamEvent) error {
			if ev.Type == "text" && ev.Text != "" {
				if bridgeActive {
					bridgeBuffer.WriteString(ev.Text)
					return nil
				}
				if hasVisibleText(ev.Text) {
					sawVisibleText = true
				}
				sw.textDelta(ev.Text)
			}
			return nil
		})
		if err == nil {
			lastErr = nil
			break
		}
		lastErr = err
		if !errors.Is(err, ErrUserMessageTooLarge) && ctx.Err() == nil {
			s.accounts.ReportFailure(ctx, acct.ID, err)
		}
		log.Printf("chat %s attempt=%d/%d account=%s failed error=%v",
			id, attempt, maxAttempts, accountLogLabel(acct), err)
		if errors.Is(err, ErrUserMessageTooLarge) {
			break
		}
		if resume && !sw.started {
			conv.reset()
			resume = false
			in.ThreadID = ""
			in.CreateSession = true
			in.Prompt = prompt
			sawVisibleText = false
			bridgeBuffer.Reset()
			log.Printf("chat %s session resume failed; retrying with full transcript", id)
			continue
		}
		if sw.started || ctx.Err() != nil {
			if sw.started {
				log.Printf("chat %s stream already started; not retrying", id)
			}
			break
		}
		// Nothing reached the client yet; reset per-attempt state and try
		// the next account in rotation.
		sawVisibleText = false
		bridgeBuffer.Reset()
	}
	stopHeartbeat()
	if lastErr != nil {
		conv.reset()
		// Once heartbeat headers are out, a JSON error status is no longer
		// possible; fall through to the in-stream error instead.
		if !sw.started && !sw.headersSent {
			status, code := http.StatusBadGateway, "upstream_error"
			switch {
			case errors.Is(lastErr, accounts.ErrNoAccounts):
				status, code = http.StatusUnauthorized, "upstream_session_unavailable"
			case errors.Is(lastErr, ErrUserMessageTooLarge):
				status, code = http.StatusBadRequest, "request_too_large"
			}
			s.logChatDone(req.Stream, id, startedAt, in, usedAccount, attempts, "failed", "", res.Usage, 0, lastErr)
			writeFlavorError(w, flavor, status, code, lastErr.Error())
			return
		}
		s.logChatDone(req.Stream, id, startedAt, in, usedAccount, attempts, "failed", "", res.Usage, 0, lastErr)
		sw.streamError(lastErr.Error())
		return
	}
	if s.cfg.SessionReuse && res.ThreadID != "" {
		conv.threadID = res.ThreadID
		conv.accountID = usedAccount.ID
		conv.messages = append([]Message(nil), req.Messages...)
		conv.systemPrompt = systemPromptText
	}
	var outText string
	var outTools []ToolCall
	finish := "stop"
	if bridgeActive {
		// Assign to the outer variables: a `:=` here would shadow outText and
		// drop the buffered answer, streaming a stop with usage but no content.
		var replies []ToolCallReply
		outText, replies = parseClientToolCalls(bridgeBuffer.String(), allowedTools)
		if len(replies) == 0 {
			replies = nativeToolReplies(res.ToolCalls, allowedTools)
		}
		replies = limitToolReplies(replies, choiceMode, choiceName, parallelTools)
		outTools = replyToolCalls(replies)
		if len(outTools) > 0 {
			finish = "tool_calls"
		} else if !hasVisibleText(outText) {
			errEmpty := fmt.Errorf("upstream returned no visible text or tool calls (completion_tokens=%d)", res.Usage.CompletionTokens)
			s.logChatDone(req.Stream, id, startedAt, in, usedAccount, attempts, "empty", "", res.Usage, 0, errEmpty)
			sw.streamError(errEmpty.Error())
			return
		}
	} else if !sawVisibleText {
		// Without declared tools there is no bridge, so upstream-native tool
		// calls cannot be mapped to the caller and are dropped here.
		if len(res.ToolCalls) > 0 {
			log.Printf("chat %s dropping %d upstream-native tool call(s): the request declared no tools", id, len(res.ToolCalls))
		}
		errEmpty := fmt.Errorf("upstream returned no visible text (completion_tokens=%d)", res.Usage.CompletionTokens)
		s.logChatDone(req.Stream, id, startedAt, in, usedAccount, attempts, "empty", "", res.Usage, 0, errEmpty)
		sw.streamError(errEmpty.Error())
		return
	}
	if hasVisibleText(outText) {
		sw.textDelta(outText)
	}
	sw.toolCalls(outTools)
	sw.finish(finish, res.Usage)
	s.logChatDone(req.Stream, id, startedAt, in, usedAccount, attempts, "ok", finish, res.Usage, len(outTools), nil)
	sw.done()
}

// proxyLogLabel keeps credentials out of request logs.
func proxyLogLabel(proxy *url.URL) string {
	if proxy == nil {
		return "direct"
	}
	return netproxy.Redact(proxy.String())
}

// accountLogLabel identifies an account in request logs without exposing
// tokens. Format: id:provider:email.
func accountLogLabel(a *store.Account) string {
	if a == nil {
		return "none"
	}
	name := strings.TrimSpace(a.Email)
	if name == "" {
		name = strings.TrimSpace(a.DisplayName)
	}
	if name == "" {
		name = "-"
	}
	return fmt.Sprintf("%d:%s:%s", a.ID, a.Provider, name)
}

// logChatDone emits one completion line per chat request with the outcome,
// selected account, mapped model, token usage, finish reason, and error, and
// keeps the same summary in memory for the management UI.
func (s *Server) logChatDone(stream bool, id string, startedAt time.Time, in ChatInput, acct *store.Account, attempts int, status, finish string, u Usage, toolCalls int, err error) {
	elapsed := time.Since(startedAt).Round(time.Millisecond)
	if finish == "" {
		finish = "-"
	}
	line := fmt.Sprintf("chat %s done status=%s account=%s model=%s attempts=%d elapsed=%s tokens{prompt=%d completion=%d total=%d cache_read=%d cache_write=%d} finish=%s tool_calls=%d",
		id, status, accountLogLabel(acct), in.Model, attempts, elapsed,
		u.PromptTokens, u.CompletionTokens, u.TotalTokens, u.CacheReadTokens, u.CacheWriteTokens, finish, toolCalls)
	errText := ""
	if err != nil {
		errText = err.Error()
		log.Printf("%s error=%v", line, err)
	} else {
		log.Print(line)
	}
	record := ChatRecord{
		ID:               id,
		Time:             startedAt,
		Model:            in.Model,
		Status:           status,
		Account:          accountLogLabel(acct),
		Attempts:         attempts,
		ElapsedMS:        elapsed.Milliseconds(),
		Stream:           stream,
		PromptChars:      len(in.Prompt),
		SystemChars:      len(in.SystemPrompt),
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
		Finish:           finish,
		ToolCalls:        toolCalls,
		Error:            errText,
	}
	s.records.add(record)
}

func (s *Server) maxAttempts(ctx context.Context) int {
	n, err := s.accounts.EnabledCount(ctx)
	if err != nil || n < 1 {
		return 1
	}
	max := s.cfg.MaxAttempts
	if max < 1 {
		max = 1
	}
	if n < max {
		return n
	}
	return max
}

func (s *Server) mapModel(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "quickwork/")
	m = strings.TrimPrefix(m, "qw-")
	switch m {
	case "smart", "fast", "balanced", "auto":
		return m
	case "":
		return s.cfg.DefaultModel
	default:
		log.Printf("unknown model %q, using %s", model, s.cfg.DefaultModel)
		return s.cfg.DefaultModel
	}
}

func usageMap(u Usage) map[string]any {
	completion := u.CompletionTokens
	total := u.TotalTokens
	if total == 0 {
		total = u.PromptTokens + u.CacheReadTokens + u.CacheWriteTokens + completion
	}
	prompt := total - completion
	if prompt < 0 {
		prompt = u.PromptTokens + u.CacheReadTokens + u.CacheWriteTokens
	}
	m := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}
	if u.CacheReadTokens > 0 {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CacheReadTokens}
	}
	return m
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}
