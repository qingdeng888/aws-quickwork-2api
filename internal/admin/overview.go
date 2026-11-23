package admin

import (
	"net/http"
	"os"
	"runtime"
	"time"

	"quickwork-openai/internal/store"
)

// accountView is the management UI's projection of a stored account. Tokens,
// client secrets, and issuer URLs never leave the server.
type accountView struct {
	ID           int64  `json:"id"`
	Label        string `json:"label"`
	Email        string `json:"email"`
	DisplayName  string `json:"display_name"`
	Provider     string `json:"provider"`
	BuildStage   string `json:"build_stage"`
	Region       string `json:"region"`
	WSHost       string `json:"ws_host"`
	TenantURL    string `json:"tenant_url"`
	AWSAccountID string `json:"aws_account_id"`
	ExpiresAt    int64  `json:"expires_at"`
	ExpiresIn    int64  `json:"expires_in"`
	ObtainedAt   int64  `json:"obtained_at"`
	CreatedAt    int64  `json:"created_at"`
	LastUsedAt   int64  `json:"last_used_at"`
	UseCount     int64  `json:"use_count"`
	Disabled     bool   `json:"disabled"`
	LastError    string `json:"last_error"`
	HasRefresh   bool   `json:"has_refresh"`
	Status       string `json:"status"`
}

func viewAccount(a *store.Account, refreshBefore time.Duration) accountView {
	status := "ready"
	now := time.Now()
	switch {
	case a.Disabled:
		status = "disabled"
	case a.ExpiresAt <= 0:
		status = "unknown"
	case now.Unix() >= a.ExpiresAt:
		status = "expired"
	case now.Add(refreshBefore).Unix() >= a.ExpiresAt:
		status = "expiring"
	}
	view := accountView{
		ID:           a.ID,
		Label:        accountLabel(a),
		Email:        a.Email,
		DisplayName:  a.DisplayName,
		Provider:     a.Provider,
		BuildStage:   a.BuildStage,
		Region:       a.Region,
		WSHost:       a.WSHost,
		TenantURL:    a.TenantURL,
		AWSAccountID: a.AWSAccountID,
		ExpiresAt:    a.ExpiresAt,
		ObtainedAt:   a.ObtainedAt,
		CreatedAt:    a.CreatedAt,
		LastUsedAt:   a.LastUsedAt,
		UseCount:     a.UseCount,
		Disabled:     a.Disabled,
		LastError:    a.LastError,
		HasRefresh:   a.RefreshToken != "",
		Status:       status,
	}
	if a.ExpiresAt > 0 {
		view.ExpiresIn = a.ExpiresAt - now.Unix()
	}
	return view
}

func accountLabel(a *store.Account) string {
	for _, candidate := range []string{a.Email, a.DisplayName, a.AccountName, a.AWSAccountID} {
		if candidate != "" {
			return candidate
		}
	}
	return "account " + itoa64(a.ID)
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	digits := make([]byte, 0, 20)
	for v > 0 {
		digits = append([]byte{byte('0' + v%10)}, digits...)
		v /= 10
	}
	return string(digits)
}

// handleOverview returns the dashboard payload: runtime state, effective
// settings, account health, and in-memory usage statistics.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	lists, err := s.store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	credential := s.webui()
	counts := map[string]int{"total": len(lists)}
	for i := range lists {
		view := viewAccount(&lists[i], cfg.RefreshBefore.Duration())
		if view.Disabled {
			counts["disabled"]++
			continue
		}
		counts["enabled"]++
		switch view.Status {
		case "expired":
			counts["expired"]++
		case "expiring":
			counts["expiring"]++
		default:
			counts["ready"]++
		}
		if view.LastError != "" {
			counts["errored"]++
		}
	}
	passwordSource := "hash"
	if credential.PasswordHash == "" {
		passwordSource = "plaintext"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"service":        "quickwork-openai",
		"revision":       buildRevision(),
		"started_at":     s.startedAt.UTC().Format(time.RFC3339),
		"uptime_seconds": int64(time.Since(s.startedAt).Seconds()),
		"now":            time.Now().UTC().Format(time.RFC3339),
		"listen":         cfg.Addr,
		"paths":          map[string]string{"config": cfg.Path, "database": cfg.Database},
		"models":         cfg.Models,
		"default_model":  cfg.DefaultModel,
		"auth": map[string]any{
			"api_key_required": len(cfg.APIKeys) > 0,
			"api_key_count":    len(cfg.APIKeys),
		},
		"accounts": counts,
		"usage":    s.proxy.ChatStats(),
		"settings": map[string]any{
			"request_timeout": humanDuration(cfg.RequestTimeout),
			"refresh_before":  humanDuration(cfg.RefreshBefore),
			"max_attempts":    cfg.MaxAttempts,
			"session_reuse":   cfg.SessionReuse,
			"session_ttl":     humanDuration(cfg.SessionTTL),
			"keep_sessions":   cfg.KeepSessions,
			"thinking_effort": emptyAs(cfg.ThinkingEffort, "server default"),
			"chat_agent_id":   emptyAs(cfg.ChatAgentID, "built-in agent"),
			"app_version":     cfg.AppVersion,
			"surface":         cfg.Surface,
			"timezone":        cfg.Timezone,
			"locale":          cfg.Locale,
		},
		"admin": map[string]any{
			"session_ttl":     humanDuration(credential.SessionTTL),
			"password_source": passwordSource,
			"trust_proxy":     credential.TrustProxy,
			"max_attempts":    credential.MaxAttempts,
		},
		"runtime": map[string]any{
			"in_container": inContainer(),
			"go_version":   runtime.Version(),
		},
		"oauth": map[string]any{
			"provider":      cfg.OAuth.Provider,
			"build_stage":   cfg.OAuth.BuildStage,
			"region":        emptyAs(cfg.OAuth.Region, "auto"),
			"callback_port": cfg.OAuth.CallbackPort,
			"timeout":       humanDuration(cfg.OAuth.Timeout),
		},
	})
}

// handleEndpoints returns copy-ready connection details for this deployment.
func (s *Server) handleEndpoints(w http.ResponseWriter, r *http.Request) {
	cfg := s.cfg
	scheme := "http"
	if requestIsSecure(r, s.webui().TrustProxy) {
		scheme = "https"
	}
	base := scheme + "://" + r.Host
	key := "local"
	if len(cfg.APIKeys) > 0 {
		key = cfg.APIKeys[0]
	}
	curl := "curl " + base + "/v1/chat/completions \\\n" +
		"  -H \"Authorization: Bearer " + key + "\" \\\n" +
		"  -H \"Content-Type: application/json\" \\\n" +
		"  -d '{\"model\":\"" + cfg.DefaultModel + "\",\"stream\":true,\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'"
	python := "from openai import OpenAI\n\nclient = OpenAI(base_url=\"" + base + "/v1\", api_key=\"" + key + "\")\n" +
		"print(client.chat.completions.create(model=\"" + cfg.DefaultModel + "\", messages=[{\"role\": \"user\", \"content\": \"hello\"}]).choices[0].message.content)"
	writeJSON(w, http.StatusOK, map[string]any{
		"base_url":   base + "/v1",
		"models_url": base + "/v1/models",
		"chat_url":   base + "/v1/chat/completions",
		"admin_url":  base + "/admin/",
		"health_url": base + "/healthz",
		"api_key":    key,
		"curl":       curl,
		"python":     python,
		"models":     cfg.Models,
	})
}

// inContainer reports whether this process runs inside a container: the image
// sets QW_CONTAINER=1 and Docker also creates /.dockerenv.
func inContainer() bool {
	if os.Getenv("QW_CONTAINER") == "1" {
		return true
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

func emptyAs(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
