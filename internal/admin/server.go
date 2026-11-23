// Package admin serves the password-protected management interface and its
// JSON API. It is mounted by the proxy under /admin, so the management UI and
// the OpenAI-compatible API share one listener and one port.
package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"path"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"quickwork-openai/internal/accounts"
	"quickwork-openai/internal/config"
	"quickwork-openai/internal/proxy"
	"quickwork-openai/internal/store"
	"quickwork-openai/webui"
)

const (
	adminPath  = "/admin/"
	apiPath    = "/admin/api/"
	cookieName = "qw_admin"
)

// Server serves the management UI and API. All mutable state is guarded by mu
// so concurrent requests, logins, and rotation stay consistent.
type Server struct {
	cfg      *config.Config
	store    *store.Store
	accounts *accounts.Manager
	proxy    *proxy.Server
	logger   *log.Logger

	startedAt time.Time

	mu          sync.Mutex
	sessions    map[string]*session
	attempts    map[string]*attemptWindow
	loginChecks int
	// loginChecksByIP counts in-flight password checks per client.
	loginChecksByIP map[string]int
	// loginSem bounds concurrent password checks globally.
	loginSem chan struct{}

	loginsMu sync.Mutex
	logins   map[string]*upstreamLogin
}

// New builds the management server. The config is shared with the proxy; only
// the webui block is ever mutated at runtime, and always under Server.mu.
func New(cfg *config.Config, st *store.Store, mgr *accounts.Manager, px *proxy.Server) *Server {
	return &Server{
		cfg:       cfg,
		store:     st,
		accounts:  mgr,
		proxy:     px,
		logger:    log.New(log.Writer(), "[admin] ", 0),
		startedAt: time.Now(),
		sessions:  make(map[string]*session),
		attempts:  make(map[string]*attemptWindow),
		logins:    make(map[string]*upstreamLogin),
	}
}

// Handler returns the /admin route tree.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /admin/api/auth/login", s.handleLogin)
	mux.HandleFunc("GET /admin/api/auth/session", s.auth(s.handleSession))
	mux.HandleFunc("POST /admin/api/auth/logout", s.auth(s.csrf(s.handleLogout)))

	mux.HandleFunc("GET /admin/api/overview", s.auth(s.handleOverview))
	mux.HandleFunc("GET /admin/api/accounts", s.auth(s.handleAccounts))
	mux.HandleFunc("POST /admin/api/accounts/refresh", s.auth(s.csrf(s.handleRefreshAccounts)))
	mux.HandleFunc("GET /admin/api/accounts/export", s.auth(s.handleExportAccounts))
	mux.HandleFunc("POST /admin/api/accounts/import", s.auth(s.csrf(s.handleImportAccounts)))
	mux.HandleFunc("POST /admin/api/accounts/{id}/proxy", s.auth(s.csrf(s.handleAccountProxy)))
	mux.HandleFunc("POST /admin/api/accounts/{id}/{action}", s.auth(s.csrf(s.handleAccountState)))
	mux.HandleFunc("GET /admin/api/proxies", s.auth(s.handleListProxies))
	mux.HandleFunc("POST /admin/api/proxies", s.auth(s.csrf(s.handleCreateProxy)))
	mux.HandleFunc("POST /admin/api/proxies/bulk", s.auth(s.csrf(s.handleBulkCreateProxies)))
	mux.HandleFunc("POST /admin/api/proxies/test", s.auth(s.csrf(s.handleTestProxyValue)))
	mux.HandleFunc("POST /admin/api/proxies/{id}/test", s.auth(s.csrf(s.handleTestProxy)))
	mux.HandleFunc("POST /admin/api/proxies/{id}/{action}", s.auth(s.csrf(s.handleProxyState)))
	mux.HandleFunc("DELETE /admin/api/proxies/{id}", s.auth(s.csrf(s.handleDeleteProxy)))
	mux.HandleFunc("DELETE /admin/api/accounts/{id}", s.auth(s.csrf(s.handleDeleteAccount)))
	mux.HandleFunc("POST /admin/api/accounts/login", s.auth(s.csrf(s.handleStartLogin)))
	mux.HandleFunc("GET /admin/api/accounts/login/{id}", s.auth(s.handleLoginStatus))
	mux.HandleFunc("POST /admin/api/accounts/login/{id}/callback", s.auth(s.csrf(s.handleLoginCallback)))
	mux.HandleFunc("DELETE /admin/api/accounts/login/{id}", s.auth(s.csrf(s.handleCancelLogin)))

	mux.HandleFunc("GET /admin/api/config", s.auth(s.handleGetConfig))
	mux.HandleFunc("PUT /admin/api/config", s.auth(s.csrf(s.handlePutConfig)))
	mux.HandleFunc("GET /admin/api/logs", s.auth(s.handleLogs))
	mux.HandleFunc("POST /admin/api/playground", s.auth(s.csrf(s.handlePlayground)))
	mux.HandleFunc("GET /admin/api/endpoints", s.auth(s.handleEndpoints))

	mux.HandleFunc("/admin/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "unknown management endpoint")
	})
	mux.HandleFunc("/admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, adminPath, http.StatusTemporaryRedirect)
	})
	mux.HandleFunc(adminPath, s.serveAsset)

	return s.securityHeaders(s.gate(mux))
}

// gate answers every management route with 503 until a password is configured,
// so a fresh install cannot be driven by anyone who can reach the port.
func (s *Server) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.enabled() {
			writeError(w, http.StatusServiceUnavailable, "admin_disabled",
				"management interface is off: set webui.password (or webui.password_hash) in config.json")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, apiPath) {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// enabled reports whether a credential is configured.
func (s *Server) enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg != nil && s.cfg.WebUI != nil && s.cfg.WebUI.Enabled &&
		(strings.TrimSpace(s.cfg.WebUI.PasswordHash) != "" || strings.TrimSpace(s.cfg.WebUI.Password) != "")
}

// webui returns a copy of the live webui block, safe to read concurrently with
// a configuration update.
func (s *Server) webui() config.WebUI {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil || s.cfg.WebUI == nil {
		return config.WebUI{}
	}
	return *s.cfg.WebUI
}

// clientIP returns the direct peer address. X-Forwarded-For is only used when
// webui.trust_proxy is on, because the header is client-controlled.
func (s *Server) clientIP(r *http.Request) string {
	if s.webui().TrustProxy {
		// The trusted proxy appends the peer it saw, so the rightmost entry
		// is the only one not under client control.
		if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
			parts := strings.Split(forwarded, ",")
			for i := len(parts) - 1; i >= 0; i-- {
				if ip := strings.TrimSpace(parts[i]); ip != "" {
					return ip
				}
			}
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func requestIsSecure(r *http.Request, trustProxy bool) bool {
	if r == nil {
		return false
	}
	if r.TLS != nil {
		return true
	}
	// X-Forwarded-Proto is client-controlled unless a trusted proxy sets it.
	return trustProxy && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

// ---- assets -------------------------------------------------------------

type assetFile struct {
	body        []byte
	contentType string
	etag        string
}

var (
	assetsOnce sync.Once
	assets     map[string]assetFile
	assetTag   string
)

func loadAssets() {
	assets = make(map[string]assetFile, 3)
	sum := sha256.New()
	for _, name := range []string{"index.html", "app.js", "styles.css"} {
		body, err := webui.Assets.ReadFile(name)
		if err != nil {
			continue
		}
		digest := sha256.Sum256(body)
		contentType := "text/plain; charset=utf-8"
		switch path.Ext(name) {
		case ".html":
			contentType = "text/html; charset=utf-8"
		case ".js":
			contentType = "text/javascript; charset=utf-8"
		case ".css":
			contentType = "text/css; charset=utf-8"
		}
		assets[name] = assetFile{body: body, contentType: contentType, etag: `"` + hex.EncodeToString(digest[:8]) + `"`}
		sum.Write(body)
	}
	assetTag = hex.EncodeToString(sum.Sum(nil)[:4])
}

// serveAsset serves the embedded single-page interface. Only the three known
// files are reachable; everything else falls back to index.html so the UI can
// own its in-page routing.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	assetsOnce.Do(loadAssets)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "GET required")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, adminPath)
	if name == "" || name == "index.html" {
		name = "index.html"
	}
	file, ok := assets[name]
	if !ok {
		file, ok = assets["index.html"]
	}
	if !ok {
		writeError(w, http.StatusInternalServerError, "assets_missing", "embedded interface is missing")
		return
	}
	w.Header().Set("Content-Type", file.contentType)
	w.Header().Set("ETag", file.etag)
	if name == "index.html" {
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=300")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, file.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	http.ServeContent(w, r, name, s.startedAt, bytes.NewReader(file.body))
}

// ---- helpers ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message},
	})
}

// decodeJSON reads one bounded JSON body into v.
func decodeJSON(r *http.Request, v any, limit int64) error {
	if r.Body == nil {
		return errors.New("empty request body")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("empty request body")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// buildRevision reports the VCS revision the binary was built from, when the
// toolchain recorded one.
func buildRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	revision := ""
	dirty := false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if dirty {
		revision += "+dirty"
	}
	return revision
}

// humanDuration renders a duration without the zero tail time.Duration adds,
// so "45m0s" shows as "45m" and "12h0m0s" as "12h".
func humanDuration(d config.Duration) string {
	text := time.Duration(d).String()
	text = strings.Replace(text, "h0m0s", "h", 1)
	text = strings.Replace(text, "h0m", "h", 1)
	text = strings.Replace(text, "m0s", "m", 1)
	return text
}

func originAllowed(r *http.Request, origin string, trustProxy bool) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	if !strings.EqualFold(parsed.Host, r.Host) {
		return false
	}
	scheme := "http"
	if requestIsSecure(r, trustProxy) {
		scheme = "https"
	}
	return strings.EqualFold(parsed.Scheme, scheme)
}
