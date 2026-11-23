package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quickwork-openai/internal/netproxy"
	"quickwork-openai/internal/proxypool"
	"quickwork-openai/internal/store"
)

// proxyView is one row of the proxy pool table.
type proxyView struct {
	ID            int64  `json:"id"`
	URL           string `json:"url"`
	Label         string `json:"label"`
	Enabled       bool   `json:"enabled"`
	Healthy       bool   `json:"healthy"`
	LatencyMS     int    `json:"latency_ms"`
	ExitIP        string `json:"exit_ip"`
	LastError     string `json:"last_error"`
	LastChecked   int64  `json:"last_checked"`
	BoundAccounts int    `json:"bound_accounts"`
}

func (s *Server) proxyViews(proxies []store.Proxy, accounts []store.Account) []proxyView {
	bound := make(map[int64]int, len(proxies))
	for _, account := range accounts {
		if account.ProxyID > 0 {
			bound[account.ProxyID]++
		}
	}
	views := make([]proxyView, 0, len(proxies))
	for _, proxy := range proxies {
		views = append(views, proxyView{
			ID:            proxy.ID,
			URL:           netproxy.Redact(proxy.URL),
			Label:         proxy.Label,
			Enabled:       proxy.Enabled,
			Healthy:       proxy.Healthy,
			LatencyMS:     proxy.LatencyMS,
			ExitIP:        proxy.ExitIP,
			LastError:     proxy.LastError,
			LastChecked:   proxy.LastChecked,
			BoundAccounts: bound[proxy.ID],
		})
	}
	return views
}

func (s *Server) poolSummary() map[string]any {
	cfg := s.cfg
	if cfg.ProxyPool == nil {
		return map[string]any{"enabled": false, "test_url": proxypool.DefaultTestURL, "check_interval": "0s"}
	}
	return map[string]any{
		"enabled":        cfg.ProxyPool.Enabled,
		"test_url":       cfg.ProxyPool.TestURL,
		"check_interval": cfg.ProxyPool.CheckInterval.String(),
	}
}

func (s *Server) handleListProxies(w http.ResponseWriter, r *http.Request) {
	proxies, err := s.store.ListProxies(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	accounts, err := s.store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"proxies":  s.proxyViews(proxies, accounts),
		"pool":     s.poolSummary(),
		"accounts": len(accounts),
	})
}

func (s *Server) handleCreateProxy(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL   string `json:"url"`
		Label string `json:"label"`
	}
	if err := decodeJSON(r, &input, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.createProxy(w, r, input.URL, input.Label)
}

func (s *Server) handleBulkCreateProxies(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Text   string   `json:"text"`
		Values []string `json:"values"`
	}
	if err := decodeJSON(r, &input, 256<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	lines := append([]string(nil), input.Values...)
	lines = append(lines, strings.Split(input.Text, "\n")...)
	seen := make(map[string]bool, len(lines))
	added := 0
	skipped := 0
	var failures []string
	for _, line := range lines {
		value := strings.TrimSpace(line)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		parsed, err := netproxy.Parse(value)
		if err != nil {
			failures = append(failures, value+": "+err.Error())
			continue
		}
		if _, err := s.store.CreateProxy(r.Context(), parsed.String(), ""); err != nil {
			if strings.Contains(err.Error(), "already in the pool") {
				skipped++
				continue
			}
			failures = append(failures, value+": "+err.Error())
			continue
		}
		added++
	}
	s.logger.Printf("proxies imported: added=%d skipped=%d failed=%d", added, skipped, len(failures))
	s.writeProxyList(w, r, map[string]any{
		"added":    added,
		"skipped":  skipped,
		"failures": failures,
	})
}

func (s *Server) createProxy(w http.ResponseWriter, r *http.Request, rawURL, label string) {
	parsed, err := netproxy.Parse(rawURL)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_proxy", err.Error())
		return
	}
	proxy, err := s.store.CreateProxy(r.Context(), parsed.String(), label)
	if err != nil {
		writeError(w, http.StatusBadRequest, "proxy_add_failed", err.Error())
		return
	}
	s.logger.Printf("proxy %d added (%s)", proxy.ID, netproxy.Redact(proxy.URL))
	s.writeProxyList(w, r, map[string]any{"created": proxy.ID})
}

func (s *Server) writeProxyList(w http.ResponseWriter, r *http.Request, extra map[string]any) {
	proxies, err := s.store.ListProxies(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	accounts, err := s.store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	payload := map[string]any{
		"proxies":  s.proxyViews(proxies, accounts),
		"pool":     s.poolSummary(),
		"accounts": len(accounts),
	}
	for key, value := range extra {
		payload[key] = value
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleProxyState(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "proxy id must be a positive integer")
		return
	}
	var enabled bool
	switch r.PathValue("action") {
	case "enable":
		enabled = true
	case "disable":
		enabled = false
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown proxy action")
		return
	}
	found, err := s.store.SetProxyEnabled(r.Context(), id, enabled)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "proxy not found")
		return
	}
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	s.logger.Printf("proxy %d %s", id, state)
	s.writeProxyList(w, r, map[string]any{"id": id, "enabled": enabled})
}

func (s *Server) handleDeleteProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "proxy id must be a positive integer")
		return
	}
	removed, err := s.store.DeleteProxy(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "not_found", "proxy not found")
		return
	}
	s.logger.Printf("proxy %d deleted", id)
	s.writeProxyList(w, r, map[string]any{"deleted": id})
}

// handleTestProxy checks one saved proxy and stores the result.
func (s *Server) handleTestProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "proxy id must be a positive integer")
		return
	}
	proxy, err := s.store.GetProxy(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "proxy not found")
		return
	}
	result, err := s.accounts.ProxyPool().CheckAndRecord(r.Context(), proxy.ID, proxy.URL, s.testURL())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	s.logger.Printf("proxy %d tested healthy=%v latency=%dms exit_ip=%s",
		proxy.ID, result.Healthy, result.LatencyMS, result.ExitIP)
	s.writeProxyList(w, r, map[string]any{"result": result, "id": proxy.ID})
}

// handleTestProxyValue checks a proxy that is not saved yet, so the operator can
// validate it before adding it to the pool.
func (s *Server) handleTestProxyValue(w http.ResponseWriter, r *http.Request) {
	var input struct {
		URL string `json:"url"`
	}
	if err := decodeJSON(r, &input, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	result := proxypool.Check(r.Context(), input.URL, s.testURL(), proxypool.CheckTimeout)
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

func (s *Server) testURL() string {
	if s.cfg.ProxyPool != nil && strings.TrimSpace(s.cfg.ProxyPool.TestURL) != "" {
		return s.cfg.ProxyPool.TestURL
	}
	return proxypool.DefaultTestURL
}

// handleAccountProxy binds one account to a proxy: 0 follows the pool, a
// positive id pins a proxy and -1 forces a direct connection.
func (s *Server) handleAccountProxy(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "account id must be a positive integer")
		return
	}
	var input struct {
		ProxyID *int64 `json:"proxy_id"`
	}
	if err := decodeJSON(r, &input, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if input.ProxyID == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "proxy_id is required")
		return
	}
	proxyID := *input.ProxyID
	if proxyID < -1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "proxy_id must be -1 (direct), 0 (pool) or a proxy id")
		return
	}
	account, err := s.store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	if err := s.store.SetAccountProxy(r.Context(), id, proxyID); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_proxy", err.Error())
		return
	}
	account.ProxyID = proxyID
	s.logger.Printf("account %d (%s) proxy_id=%d", id, accountLabel(account), proxyID)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"proxy_id": proxyID,
		"account":  viewAccount(account, s.cfg.RefreshBefore.Duration()),
		"updated":  time.Now().Unix(),
	})
}

// proxyLabelMap returns id -> "label (redacted url)" for account rows.
func (s *Server) proxyLabelMap(ctx context.Context) map[int64]string {
	proxies, err := s.store.ListProxies(ctx)
	if err != nil {
		return nil
	}
	out := make(map[int64]string, len(proxies))
	for _, proxy := range proxies {
		label := strings.TrimSpace(proxy.Label)
		if label == "" {
			label = netproxy.Redact(proxy.URL)
		}
		out[proxy.ID] = label
	}
	return out
}
