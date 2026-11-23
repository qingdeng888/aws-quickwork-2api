package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"time"
)

// session is one logged-in management session. The map is keyed by the
// SHA-256 of the cookie value, so the raw token never sits in memory twice.
type session struct {
	CSRF        string
	Expires     time.Time
	Fingerprint string
}

// attemptWindow counts failed logins for one client in a fixed window.
type attemptWindow struct {
	started time.Time
	count   int
}

type sessionKey struct{}

const (
	maxSessions    = 256
	loginWindow    = 5 * time.Minute
	maxLoginChecks = 4
	// maxLoginChecksPerIP stops one client from holding every global slot.
	maxLoginChecksPerIP = 2
	// loginCheckWait is how long a request waits for a free global slot
	// before it is answered with 429.
	loginCheckWait = 2 * time.Second
)

// handleLogin verifies the management password, opens a session, and returns
// the CSRF token the UI must send with state-changing requests.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	client := s.clientIP(r)
	var input struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &input, 16<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !s.acquireLoginCheck(r.Context(), client) {
		writeError(w, http.StatusTooManyRequests, "rate_limited", "too many login attempts; wait a few minutes and retry")
		return
	}
	defer s.releaseLoginCheck(client)

	credential := s.webui()
	if !credential.Check(input.Password) {
		s.logger.Printf("login failed client=%s", client)
		writeError(w, http.StatusUnauthorized, "invalid_credentials", "wrong password")
		return
	}

	token, err := randomToken(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not create a session")
		return
	}
	csrf, err := randomToken(24)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "could not create a session")
		return
	}
	ttl := time.Duration(credential.SessionTTL)
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	expires := time.Now().Add(ttl)

	s.mu.Lock()
	delete(s.attempts, client)
	s.pruneSessionsLocked(time.Now())
	if len(s.sessions) >= maxSessions {
		s.evictEarliestSessionLocked()
	}
	s.sessions[tokenDigest(token)] = &session{CSRF: csrf, Expires: expires, Fingerprint: credential.Fingerprint()}
	s.mu.Unlock()

	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    token,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsSecure(r, s.webui().TrustProxy),
		Expires:  expires,
		MaxAge:   int(ttl.Seconds()),
	})
	s.logger.Printf("login ok client=%s ttl=%s", client, ttl)
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"csrf_token":    csrf,
		"expires_at":    expires.UTC().Format(time.RFC3339),
		"ttl_seconds":   int(ttl.Seconds()),
	})
}

// handleSession reports the live session, which the UI uses on page load.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	sess, ok := sessionFromContext(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_required", "login required")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"csrf_token":    sess.CSRF,
		"expires_at":    sess.Expires.UTC().Format(time.RFC3339),
	})
}

// handleLogout drops the session and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
		s.mu.Lock()
		delete(s.sessions, tokenDigest(cookie.Value))
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   requestIsSecure(r, s.webui().TrustProxy),
		MaxAge:   -1,
	})
	w.WriteHeader(http.StatusNoContent)
}

// auth rejects requests without a live session and publishes the session on
// the request context. A session also dies when the password changes.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := s.sessionFromRequest(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication_required", "login required")
			return
		}
		ctx := context.WithValue(r.Context(), sessionKey{}, sess)
		next(w, r.WithContext(ctx))
	}
}

// csrf rejects a state-changing request without the session's CSRF token, or
// with an Origin header that does not belong to this server.
func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := sessionFromContext(r)
		if !ok || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(sess.CSRF)) != 1 {
			writeError(w, http.StatusForbidden, "csrf_failed", "missing or invalid CSRF token")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !originAllowed(r, origin, s.webui().TrustProxy) {
			writeError(w, http.StatusForbidden, "origin_failed", "request origin does not match this server")
			return
		}
		next(w, r)
	}
}

func sessionFromContext(r *http.Request) (*session, bool) {
	sess, ok := r.Context().Value(sessionKey{}).(*session)
	return sess, ok
}

// sessionFromRequest resolves the cookie to a live session.
func (s *Server) sessionFromRequest(r *http.Request) (*session, bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}
	digest := tokenDigest(cookie.Value)
	now := time.Now()
	credential := s.webui().Fingerprint()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[digest]
	if !ok {
		return nil, false
	}
	if now.After(sess.Expires) || sess.Fingerprint != credential {
		delete(s.sessions, digest)
		return nil, false
	}
	return sess, true
}

// reserveLoginCheck counts an attempt before verification so parallel guesses
// cannot all pass the same failure count, and caps how many expensive password
// hashes run at once.
//
// The global cap is a semaphore: a request waits up to loginCheckWait for a
// slot (or until its context ends) before giving up. The per-IP cap and the
// attempt window are checked without waiting.
func (s *Server) acquireLoginCheck(ctx context.Context, client string) bool {
	sem := s.loginSemaphore()
	timer := time.NewTimer(loginCheckWait)
	defer timer.Stop()
	select {
	case sem <- struct{}{}:
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
	if !s.reserveLoginCheck(client) {
		<-sem
		return false
	}
	return true
}

func (s *Server) loginSemaphore() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loginSem == nil {
		s.loginSem = make(chan struct{}, maxLoginChecks)
	}
	return s.loginSem
}

func (s *Server) reserveLoginCheck(client string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loginChecksByIP == nil {
		s.loginChecksByIP = make(map[string]int)
	}
	if s.loginChecksByIP[client] >= maxLoginChecksPerIP {
		return false
	}
	now := time.Now()
	limit := 10
	if s.cfg != nil && s.cfg.WebUI != nil && s.cfg.WebUI.MaxAttempts > 0 {
		limit = s.cfg.WebUI.MaxAttempts
	}
	if len(s.attempts) > 1024 {
		for key, window := range s.attempts {
			if now.Sub(window.started) > loginWindow {
				delete(s.attempts, key)
			}
		}
	}
	window := s.attempts[client]
	if window == nil || now.Sub(window.started) > loginWindow {
		if len(s.attempts) >= 4096 {
			s.evictOldestWindowLocked()
		}
		window = &attemptWindow{started: now}
		s.attempts[client] = window
	}
	if window.count >= limit {
		return false
	}
	window.count++
	s.loginChecks++
	s.loginChecksByIP[client]++
	return true
}

func (s *Server) releaseLoginCheck(client string) {
	s.mu.Lock()
	if s.loginChecks > 0 {
		s.loginChecks--
	}
	if n := s.loginChecksByIP[client]; n > 1 {
		s.loginChecksByIP[client] = n - 1
	} else {
		delete(s.loginChecksByIP, client)
	}
	sem := s.loginSem
	s.mu.Unlock()
	if sem != nil {
		select {
		case <-sem:
		default:
		}
	}
}

func (s *Server) evictOldestWindowLocked() {
	var oldestKey string
	var oldest time.Time
	for key, window := range s.attempts {
		if oldestKey == "" || window.started.Before(oldest) {
			oldestKey, oldest = key, window.started
		}
	}
	if oldestKey != "" {
		delete(s.attempts, oldestKey)
	}
}

func (s *Server) pruneSessionsLocked(now time.Time) {
	for token, sess := range s.sessions {
		if now.After(sess.Expires) {
			delete(s.sessions, token)
		}
	}
}

func (s *Server) evictEarliestSessionLocked() {
	var earliestToken string
	var earliest time.Time
	for token, sess := range s.sessions {
		if earliestToken == "" || sess.Expires.Before(earliest) {
			earliestToken, earliest = token, sess.Expires
		}
	}
	if earliestToken != "" {
		delete(s.sessions, earliestToken)
	}
}

func randomToken(length int) (string, error) {
	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
