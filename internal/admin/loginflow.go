package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"quickwork-openai/internal/oauth"
	"quickwork-openai/internal/store"
)

// upstreamLogin is one in-flight browser login started from the management UI.
// The OAuth flow itself is unchanged: manual mode feeds pasted callback URLs
// into LoginManual, local mode lets the browser hit 127.0.0.1:<port>.
type upstreamLogin struct {
	id        string
	mode      string
	provider  string
	stage     string
	startedAt time.Time
	cancel    context.CancelFunc
	feed      io.WriteCloser

	mu           sync.Mutex
	text         strings.Builder
	status       string
	errText      string
	accountID    int64
	accountLabel string
	updated      time.Time
}

var urlPattern = regexp.MustCompile(`https?://[^\s"'<>\\]+`)

// Write collects the flow's terminal output for the UI.
func (l *upstreamLogin) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.text.Len() > 48<<10 {
		existing := l.text.String()
		if cut := strings.IndexByte(existing[len(existing)/2:], '\n'); cut >= 0 {
			existing = existing[len(existing)/2+cut+1:]
		} else {
			existing = existing[len(existing)/2:]
		}
		l.text.Reset()
		l.text.WriteString(existing)
	}
	l.text.Write(p)
	l.updated = time.Now()
	return len(p), nil
}

func (l *upstreamLogin) logf(format string, args ...any) {
	_, _ = l.Write([]byte(fmt.Sprintf(format, args...) + "\n"))
}

func (l *upstreamLogin) finish(account *store.Account, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.updated = time.Now()
	if err == nil && account != nil {
		l.status = "done"
		l.accountID = account.ID
		l.accountLabel = accountLabel(account)
		l.errText = ""
		return
	}
	if l.status == "cancelled" {
		return
	}
	if err == nil {
		err = errors.New("login ended without an account")
	}
	if errors.Is(err, context.Canceled) {
		l.status = "cancelled"
		return
	}
	l.status = "failed"
	l.errText = err.Error()
}

func (l *upstreamLogin) cancelLogin() {
	l.cancel()
	l.mu.Lock()
	if l.status == "running" {
		l.status = "cancelled"
		l.errText = "cancelled from the management interface"
	}
	l.updated = time.Now()
	l.mu.Unlock()
}

// loginView is what the UI polls while a login runs.
type loginView struct {
	ID         string       `json:"id"`
	Mode       string       `json:"mode"`
	Provider   string       `json:"provider"`
	Stage      string       `json:"stage"`
	Status     string       `json:"status"`
	Text       string       `json:"text"`
	URLs       []string     `json:"urls"`
	Waiting    bool         `json:"waiting"`
	Error      string       `json:"error,omitempty"`
	AccountID  int64        `json:"account_id,omitempty"`
	Account    string       `json:"account,omitempty"`
	StartedAt  time.Time    `json:"started_at"`
	UpdatedAt  time.Time    `json:"updated_at"`
	ElapsedSec int64        `json:"elapsed_seconds"`
	Hint       string       `json:"hint"`
	AccountRow *accountView `json:"account_row,omitempty"`
}

func (l *upstreamLogin) view(st *store.Store) loginView {
	l.mu.Lock()
	text := l.text.String()
	view := loginView{
		ID:         l.id,
		Mode:       l.mode,
		Provider:   l.provider,
		Stage:      l.stage,
		Status:     l.status,
		Text:       text,
		Waiting:    strings.HasSuffix(strings.TrimSpace(text), "callback URL:"),
		Error:      l.errText,
		AccountID:  l.accountID,
		Account:    l.accountLabel,
		StartedAt:  l.startedAt,
		UpdatedAt:  l.updated,
		ElapsedSec: int64(time.Since(l.startedAt).Seconds()),
	}
	l.mu.Unlock()

	seen := make(map[string]bool, 4)
	for _, match := range urlPattern.FindAllString(text, -1) {
		match = strings.TrimRight(match, ".,);")
		if seen[match] {
			continue
		}
		seen[match] = true
		view.URLs = append(view.URLs, match)
		if len(view.URLs) >= 6 {
			break
		}
	}
	switch l.mode {
	case "local":
		view.Hint = "在服务器本机的浏览器里打开上面的 URL；浏览器会回调 127.0.0.1 的回调端口完成登录。"
	default:
		view.Hint = "在任意浏览器打开第一个 URL 登录，然后把地址栏里的完整 callback URL 粘回下面的输入框。"
	}
	if view.AccountID > 0 && st != nil {
		if account, err := st.Get(context.Background(), view.AccountID); err == nil {
			row := viewAccount(account, 10*time.Minute)
			view.AccountRow = &row
		}
	}
	return view
}

// handleStartLogin launches a manual or localhost OAuth flow in the
// background and returns its id for polling.
func (s *Server) handleStartLogin(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Provider string `json:"provider"`
		Stage    string `json:"stage"`
		Mode     string `json:"mode"`
	}
	_ = decodeJSON(r, &input, 8<<10)

	cfg := s.cfg
	provider := strings.ToLower(strings.TrimSpace(input.Provider))
	if provider == "" {
		provider = strings.ToLower(cfg.OAuth.Provider)
	}
	stage := strings.ToLower(strings.TrimSpace(input.Stage))
	if stage == "" {
		stage = strings.ToLower(cfg.OAuth.BuildStage)
	}
	mode := strings.ToLower(strings.TrimSpace(input.Mode))
	if mode == "" {
		mode = "manual"
	}
	switch provider {
	case "social", "enterprise", "gateway", "social_gateway":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "provider must be social, enterprise, gateway or social_gateway")
		return
	}
	switch stage {
	case "prod", "beta", "integ", "dev":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "stage must be prod, beta, integ or dev")
		return
	}
	if mode != "manual" && mode != "local" {
		writeError(w, http.StatusBadRequest, "invalid_request", "mode must be manual or local")
		return
	}

	id, err := randomToken(9)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not allocate a login id")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Now()
	login := &upstreamLogin{
		id:        id,
		mode:      mode,
		provider:  provider,
		stage:     stage,
		startedAt: now,
		updated:   now,
		status:    "running",
		cancel:    cancel,
	}
	opts := oauth.LoginOptions{
		Provider:   provider,
		Stage:      stage,
		Region:     cfg.OAuth.Region,
		Port:       cfg.OAuth.CallbackPort,
		Timeout:    cfg.OAuth.Timeout.Duration(),
		AppVersion: cfg.AppVersion,
		Logf:       login.logf,
	}

	s.loginsMu.Lock()
	s.pruneLoginsLocked()
	s.logins[id] = login
	s.loginsMu.Unlock()

	if mode == "manual" {
		reader, writer := io.Pipe()
		login.feed = writer
		go func() {
			defer writer.Close()
			account, err := oauth.LoginManual(ctx, opts, s.store, reader, login)
			login.finish(account, err)
			if err != nil {
				s.logger.Printf("manual login %s ended: %v", id, err)
			} else {
				s.logger.Printf("manual login %s stored account %d", id, account.ID)
			}
		}()
	} else {
		go func() {
			account, err := oauth.Login(ctx, opts, s.store)
			login.finish(account, err)
			if err != nil {
				s.logger.Printf("local login %s ended: %v", id, err)
			} else {
				s.logger.Printf("local login %s stored account %d", id, account.ID)
			}
		}()
	}
	s.logger.Printf("login %s started provider=%s stage=%s mode=%s", id, provider, stage, mode)
	writeJSON(w, http.StatusOK, map[string]any{"login": login.view(s.store)})
}

// handleLoginStatus returns the progress of one login.
func (s *Server) handleLoginStatus(w http.ResponseWriter, r *http.Request) {
	login := s.findLogin(r.PathValue("id"))
	if login == nil {
		writeError(w, http.StatusNotFound, "not_found", "login session not found or already expired")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"login": login.view(s.store)})
}

// handleLoginCallback feeds one pasted callback URL into the manual flow.
func (s *Server) handleLoginCallback(w http.ResponseWriter, r *http.Request) {
	login := s.findLogin(r.PathValue("id"))
	if login == nil {
		writeError(w, http.StatusNotFound, "not_found", "login session not found or already expired")
		return
	}
	if login.mode != "manual" {
		writeError(w, http.StatusBadRequest, "invalid_request", "this login uses the localhost callback port, not pasted URLs")
		return
	}
	var input struct {
		URL string `json:"url"`
	}
	if err := decodeJSON(r, &input, 64<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	target := strings.TrimSpace(input.URL)
	if target == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "callback URL is empty")
		return
	}
	if len(target) > 16<<10 {
		writeError(w, http.StatusBadRequest, "invalid_request", "callback URL is too long")
		return
	}
	login.logf("> %s", target)
	if feed := login.feed; feed != nil {
		go func() {
			written := make(chan error, 1)
			go func() {
				_, err := io.WriteString(feed, target+"\n")
				written <- err
			}()
			select {
			case <-written:
			case <-time.After(20 * time.Second):
			case <-r.Context().Done():
			}
		}()
	}
	writeJSON(w, http.StatusOK, map[string]any{"queued": true})
}

// handleCancelLogin stops a login and drops its OAuth state.
func (s *Server) handleCancelLogin(w http.ResponseWriter, r *http.Request) {
	login := s.findLogin(r.PathValue("id"))
	if login == nil {
		writeError(w, http.StatusNotFound, "not_found", "login session not found")
		return
	}
	login.cancelLogin()
	s.logger.Printf("login %s cancelled", login.id)
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": true})
}

func (s *Server) findLogin(id string) *upstreamLogin {
	s.loginsMu.Lock()
	defer s.loginsMu.Unlock()
	if id == "" {
		return nil
	}
	return s.logins[id]
}

// pruneLoginsLocked keeps the login map small: finished entries older than
// thirty minutes go first, then the oldest entry when the cap is reached.
func (s *Server) pruneLoginsLocked() {
	now := time.Now()
	for id, login := range s.logins {
		login.mu.Lock()
		stale := login.status != "running" && now.Sub(login.updated) > 30*time.Minute
		login.mu.Unlock()
		if stale {
			delete(s.logins, id)
		}
	}
	if len(s.logins) < 32 {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, login := range s.logins {
		login.mu.Lock()
		started := login.startedAt
		login.mu.Unlock()
		if oldestID == "" || started.Before(oldest) {
			oldestID, oldest = id, started
		}
	}
	if oldestID != "" {
		if login := s.logins[oldestID]; login != nil {
			login.cancelLogin()
		}
		delete(s.logins, oldestID)
	}
}
