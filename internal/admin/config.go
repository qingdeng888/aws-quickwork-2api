package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"quickwork-openai/internal/config"
)

// oauthForm mirrors the editable part of the oauth block.
type oauthForm struct {
	Provider     string `json:"provider"`
	BuildStage   string `json:"build_stage"`
	Region       string `json:"region"`
	CallbackPort int    `json:"callback_port"`
	Timeout      string `json:"timeout"`
}

// webuiForm mirrors the editable part of the webui block. NewPassword is
// write-only: it is hashed before it reaches disk and never echoed back.
type webuiForm struct {
	Enabled       bool   `json:"enabled"`
	SessionTTL    string `json:"session_ttl"`
	MaxAttempts   int    `json:"max_attempts"`
	TrustProxy    bool   `json:"trust_proxy"`
	NewPassword   string `json:"new_password"`
	ClearPassword bool   `json:"clear_password"`
}

// configForm is what the management UI reads and writes. Database, config path
// and upstream handshake details stay read-only so a save cannot move the
// SQLite file or break the WebSocket handshake by accident.
type configForm struct {
	Addr           string    `json:"addr"`
	APIKeys        []string  `json:"api_keys"`
	Models         []string  `json:"models"`
	DefaultModel   string    `json:"default_model"`
	RequestTimeout string    `json:"request_timeout"`
	RefreshBefore  string    `json:"refresh_before"`
	MaxAttempts    int       `json:"max_attempts"`
	AppVersion     string    `json:"app_version"`
	Surface        string    `json:"surface"`
	Timezone       string    `json:"timezone"`
	Locale         string    `json:"locale"`
	ThinkingEffort string    `json:"thinking_effort"`
	ChatAgentID    string    `json:"chat_agent_id"`
	KeepSessions   bool      `json:"keep_sessions"`
	SessionReuse   bool      `json:"session_reuse"`
	SessionTTL     string    `json:"session_ttl"`
	OAuth          oauthForm `json:"oauth"`
	WebUI          webuiForm `json:"webui"`
}

type configResponse struct {
	Config          configForm        `json:"config"`
	Paths           map[string]string `json:"paths"`
	RestartRequired []string          `json:"restart_required"`
	PasswordSource  string            `json:"password_source"`
	PasswordSet     bool              `json:"password_set"`
}

func formFromConfig(cfg *config.Config) configForm {
	form := configForm{
		Addr:           cfg.Addr,
		APIKeys:        append([]string{}, cfg.APIKeys...),
		Models:         append([]string{}, cfg.Models...),
		DefaultModel:   cfg.DefaultModel,
		RequestTimeout: humanDuration(cfg.RequestTimeout),
		RefreshBefore:  humanDuration(cfg.RefreshBefore),
		MaxAttempts:    cfg.MaxAttempts,
		AppVersion:     cfg.AppVersion,
		Surface:        cfg.Surface,
		Timezone:       cfg.Timezone,
		Locale:         cfg.Locale,
		ThinkingEffort: cfg.ThinkingEffort,
		ChatAgentID:    cfg.ChatAgentID,
		KeepSessions:   cfg.KeepSessions,
		SessionReuse:   cfg.SessionReuse,
		SessionTTL:     humanDuration(cfg.SessionTTL),
		OAuth: oauthForm{
			Provider:     cfg.OAuth.Provider,
			BuildStage:   cfg.OAuth.BuildStage,
			Region:       cfg.OAuth.Region,
			CallbackPort: cfg.OAuth.CallbackPort,
			Timeout:      humanDuration(cfg.OAuth.Timeout),
		},
	}
	if cfg.WebUI != nil {
		form.WebUI = webuiForm{
			Enabled:     cfg.WebUI.Enabled,
			SessionTTL:  humanDuration(cfg.WebUI.SessionTTL),
			MaxAttempts: cfg.WebUI.MaxAttempts,
			TrustProxy:  cfg.WebUI.TrustProxy,
		}
	}
	return form
}

// handleGetConfig returns the configuration on disk (what the next start uses)
// plus the fields that differ from the running process.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	fileCfg, err := config.Load(s.cfg.Path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	credential := s.webui()
	source := "plaintext"
	if credential.PasswordHash != "" {
		source = "hash"
	}
	writeJSON(w, http.StatusOK, configResponse{
		Config:          maskForm(formFromConfig(fileCfg)),
		Paths:           map[string]string{"config": s.cfg.Path, "database": s.cfg.Database},
		RestartRequired: diffConfig(fileCfg, s.cfg),
		PasswordSource:  source,
		PasswordSet:     credential.Password != "" || credential.PasswordHash != "",
	})
}

// handlePutConfig validates the submitted form, writes config.json atomically,
// and applies webui changes to the running process immediately. Every other
// field is picked up on the next start, which the response reports.
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var input configForm
	if err := decodeJSON(r, &input, 512<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	// Serialize read-merge-write so concurrent saves cannot lose updates.
	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	path := s.cfg.Path
	if existing, err := config.Load(path); err == nil {
		keys, err := unmaskAPIKeys(input.APIKeys, existing.APIKeys)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
			return
		}
		input.APIKeys = keys
	}
	form, err := normalizeForm(input)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}

	original, err := os.ReadFile(path)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(original, &raw); err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", "config.json is not a JSON object: "+err.Error())
		return
	}

	credential := s.webui()
	newPasswordHash := credential.PasswordHash
	newPasswordPlain := credential.Password
	if form.WebUI.ClearPassword {
		newPasswordHash, newPasswordPlain = "", ""
	}
	if form.WebUI.NewPassword != "" {
		hash, err := config.HashPassword(form.WebUI.NewPassword)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
			return
		}
		newPasswordHash, newPasswordPlain = hash, ""
	}

	apply := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		raw[key] = encoded
		return nil
	}
	for key, value := range map[string]any{
		"addr":            form.Addr,
		"api_keys":        form.APIKeys,
		"models":          form.Models,
		"default_model":   form.DefaultModel,
		"request_timeout": form.RequestTimeout,
		"refresh_before":  form.RefreshBefore,
		"max_attempts":    form.MaxAttempts,
		"app_version":     form.AppVersion,
		"surface":         form.Surface,
		"timezone":        form.Timezone,
		"locale":          form.Locale,
		"thinking_effort": form.ThinkingEffort,
		"chat_agent_id":   form.ChatAgentID,
		"keep_sessions":   form.KeepSessions,
		"session_reuse":   form.SessionReuse,
		"session_ttl":     form.SessionTTL,
	} {
		if err := apply(key, value); err != nil {
			writeError(w, http.StatusInternalServerError, "config_error", err.Error())
			return
		}
	}
	oauthRaw := map[string]json.RawMessage{}
	if existing, ok := raw["oauth"]; ok {
		_ = json.Unmarshal(existing, &oauthRaw)
	}
	for key, value := range map[string]any{
		"provider":      form.OAuth.Provider,
		"build_stage":   form.OAuth.BuildStage,
		"region":        form.OAuth.Region,
		"callback_port": form.OAuth.CallbackPort,
		"timeout":       form.OAuth.Timeout,
	} {
		encoded, _ := json.Marshal(value)
		oauthRaw[key] = encoded
	}
	encodedOAuth, _ := json.Marshal(oauthRaw)
	raw["oauth"] = encodedOAuth

	webuiRaw := map[string]json.RawMessage{}
	if existing, ok := raw["webui"]; ok {
		_ = json.Unmarshal(existing, &webuiRaw)
	}
	for key, value := range map[string]any{
		"enabled":       form.WebUI.Enabled,
		"session_ttl":   form.WebUI.SessionTTL,
		"max_attempts":  form.WebUI.MaxAttempts,
		"trust_proxy":   form.WebUI.TrustProxy,
		"password_hash": newPasswordHash,
		"password":      newPasswordPlain,
	} {
		encoded, _ := json.Marshal(value)
		webuiRaw[key] = encoded
	}
	encodedWebUI, _ := json.Marshal(webuiRaw)
	raw["webui"] = encodedWebUI

	updated, err := writeConfigAtomic(path, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	// Keep runtime authentication in step with the saved file.
	s.proxy.SetAPIKeys(updated.APIKeys)

	// webui changes take effect without a restart; sessions created under the
	// previous password are dropped below by the fingerprint check.
	s.mu.Lock()
	live := s.cfg.WebUI
	if live == nil {
		live = &config.WebUI{}
		s.cfg.WebUI = live
	}
	live.Enabled = updated.WebUI.Enabled
	live.SessionTTL = updated.WebUI.SessionTTL
	live.MaxAttempts = updated.WebUI.MaxAttempts
	live.TrustProxy = updated.WebUI.TrustProxy
	live.Password = updated.WebUI.Password
	live.PasswordHash = updated.WebUI.PasswordHash
	s.mu.Unlock()

	restart := diffConfig(updated, s.cfg)
	if len(restart) > 0 {
		s.logger.Printf("config saved; restart required for: %s", strings.Join(restart, ", "))
	} else {
		s.logger.Printf("config saved")
	}
	credential = s.webui()
	source := "plaintext"
	if credential.PasswordHash != "" {
		source = "hash"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"saved":            true,
		"restart_required": restart,
		"password_source":  source,
		"password_set":     credential.Password != "" || credential.PasswordHash != "",
		"config":           maskForm(formFromConfig(updated)),
		"paths":            map[string]string{"config": path, "database": s.cfg.Database},
	})
}

// normalizeForm validates and cleans a submitted form.
func normalizeForm(input configForm) (configForm, error) {
	form := input
	form.Addr = strings.TrimSpace(form.Addr)
	if form.Addr == "" {
		form.Addr = "127.0.0.1:8080"
	}
	if _, _, err := net.SplitHostPort(form.Addr); err != nil {
		return form, fmt.Errorf("addr must look like host:port: %w", err)
	}
	keys := make([]string, 0, len(form.APIKeys))
	seen := map[string]bool{}
	for _, key := range form.APIKeys {
		key = strings.TrimSpace(key)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	form.APIKeys = keys
	models := make([]string, 0, len(form.Models))
	modelSeen := map[string]bool{}
	for _, model := range form.Models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" || modelSeen[model] {
			continue
		}
		modelSeen[model] = true
		models = append(models, model)
	}
	if len(models) == 0 {
		models = []string{"smart", "fast", "balanced", "auto"}
	}
	form.Models = models
	form.DefaultModel = strings.ToLower(strings.TrimSpace(form.DefaultModel))
	if form.DefaultModel == "" {
		form.DefaultModel = models[0]
	}
	if !modelSeen[form.DefaultModel] {
		return form, fmt.Errorf("default_model %q must appear in models", form.DefaultModel)
	}
	for name, value := range map[string]*string{
		"request_timeout":   &form.RequestTimeout,
		"refresh_before":    &form.RefreshBefore,
		"session_ttl":       &form.SessionTTL,
		"oauth.timeout":     &form.OAuth.Timeout,
		"webui.session_ttl": &form.WebUI.SessionTTL,
	} {
		*value = strings.TrimSpace(*value)
		if _, err := time.ParseDuration(*value); err != nil {
			return form, fmt.Errorf("%s must be a duration like 30s, 5m or 12h", name)
		}
	}
	if form.MaxAttempts < 1 || form.MaxAttempts > 100 {
		return form, errors.New("max_attempts must be between 1 and 100")
	}
	if form.OAuth.CallbackPort < 0 || form.OAuth.CallbackPort > 65535 {
		return form, errors.New("oauth.callback_port must be 0-65535")
	}
	form.OAuth.Provider = strings.ToLower(strings.TrimSpace(form.OAuth.Provider))
	switch form.OAuth.Provider {
	case "social", "enterprise", "gateway", "social_gateway":
	default:
		return form, errors.New("oauth.provider must be social, enterprise, gateway or social_gateway")
	}
	form.OAuth.BuildStage = strings.ToLower(strings.TrimSpace(form.OAuth.BuildStage))
	switch form.OAuth.BuildStage {
	case "prod", "beta", "integ", "dev":
	default:
		return form, errors.New("oauth.build_stage must be prod, beta, integ or dev")
	}
	form.OAuth.Region = strings.TrimSpace(form.OAuth.Region)
	switch strings.ToLower(strings.TrimSpace(form.ThinkingEffort)) {
	case "", "off", "low", "medium", "high":
		form.ThinkingEffort = strings.ToLower(strings.TrimSpace(form.ThinkingEffort))
	default:
		return form, errors.New("thinking_effort must be empty, off, low, medium or high")
	}
	form.Timezone = strings.TrimSpace(form.Timezone)
	if form.Timezone == "" {
		form.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(form.Timezone); err != nil {
		return form, fmt.Errorf("timezone %q is not an IANA name", form.Timezone)
	}
	form.Locale = strings.TrimSpace(form.Locale)
	if form.Locale == "" {
		form.Locale = "en-US"
	}
	form.AppVersion = strings.TrimSpace(form.AppVersion)
	if form.AppVersion == "" {
		form.AppVersion = "1.2.4660"
	}
	form.Surface = strings.TrimSpace(form.Surface)
	if form.Surface == "" {
		form.Surface = "QUICK_WORK_DESKTOP_WINDOWS"
	}
	if form.WebUI.MaxAttempts < 1 || form.WebUI.MaxAttempts > 100 {
		return form, errors.New("webui.max_attempts must be between 1 and 100")
	}
	if form.WebUI.NewPassword != "" && len(form.WebUI.NewPassword) < 8 {
		return form, errors.New("new password must contain at least 8 characters")
	}
	return form, nil
}

// writeConfigAtomic writes the merged JSON to a temp file, validates it by
// loading it back, and only then renames it over config.json. A failed
// validation leaves the original file untouched.
func writeConfigAtomic(path string, raw map[string]json.RawMessage) (*config.Config, error) {
	body, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".config.json.*.tmp")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	fail := func(err error) (*config.Config, error) {
		f.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	if _, err := f.Write(body); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	updated, err := config.Load(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	updated.Path = path
	return updated, nil
}

// configWriteMu serializes PUT /config read-merge-write cycles.
var configWriteMu sync.Mutex

// maskAPIKey hides all but a short prefix and suffix of a key.
func maskAPIKey(key string) string {
	if len(key) <= 10 {
		return "…****"
	}
	return key[:3] + "…" + key[len(key)-4:]
}

func maskAPIKeys(keys []string) []string {
	out := make([]string, len(keys))
	for i, key := range keys {
		out[i] = maskAPIKey(key)
	}
	return out
}

func maskForm(form configForm) configForm {
	form.APIKeys = maskAPIKeys(form.APIKeys)
	return form
}

// unmaskAPIKeys maps masked placeholders back to the stored key they stand
// for ("keep unchanged"). Values without the mask marker are new keys.
func unmaskAPIKeys(submitted, existing []string) ([]string, error) {
	out := make([]string, 0, len(submitted))
	for _, key := range submitted {
		trimmed := strings.TrimSpace(key)
		if !strings.Contains(trimmed, "…") {
			out = append(out, key)
			continue
		}
		matched := ""
		for _, candidate := range existing {
			if maskAPIKey(candidate) == trimmed {
				if matched != "" && matched != candidate {
					return nil, fmt.Errorf("masked api key %q is ambiguous; re-enter it in full", trimmed)
				}
				matched = candidate
			}
		}
		if matched == "" {
			return nil, fmt.Errorf("masked api key %q does not match a stored key", trimmed)
		}
		out = append(out, matched)
	}
	return out, nil
}

// diffConfig lists the fields where the running process differs from the file
// on disk; those need a restart to take effect.
func diffConfig(fileCfg, running *config.Config) []string {
	var changed []string
	add := func(condition bool, name string) {
		if condition {
			changed = append(changed, name)
		}
	}
	add(fileCfg.Addr != running.Addr, "addr")
	add(fileCfg.Database != running.Database, "database")
	// api_keys apply immediately through the key manager; no restart needed.
	add(!equalStrings(fileCfg.Models, running.Models), "models")
	add(fileCfg.DefaultModel != running.DefaultModel, "default_model")
	add(fileCfg.RequestTimeout != running.RequestTimeout, "request_timeout")
	add(fileCfg.RefreshBefore != running.RefreshBefore, "refresh_before")
	add(fileCfg.MaxAttempts != running.MaxAttempts, "max_attempts")
	add(fileCfg.AppVersion != running.AppVersion, "app_version")
	add(fileCfg.Surface != running.Surface, "surface")
	add(fileCfg.Timezone != running.Timezone, "timezone")
	add(fileCfg.Locale != running.Locale, "locale")
	add(fileCfg.ThinkingEffort != running.ThinkingEffort, "thinking_effort")
	add(fileCfg.ChatAgentID != running.ChatAgentID, "chat_agent_id")
	add(fileCfg.KeepSessions != running.KeepSessions, "keep_sessions")
	add(fileCfg.SessionReuse != running.SessionReuse, "session_reuse")
	add(fileCfg.SessionTTL != running.SessionTTL, "session_ttl")
	add(fileCfg.OAuth.Provider != running.OAuth.Provider, "oauth.provider")
	add(fileCfg.OAuth.BuildStage != running.OAuth.BuildStage, "oauth.build_stage")
	add(fileCfg.OAuth.Region != running.OAuth.Region, "oauth.region")
	add(fileCfg.OAuth.CallbackPort != running.OAuth.CallbackPort, "oauth.callback_port")
	add(fileCfg.OAuth.Timeout != running.OAuth.Timeout, "oauth.timeout")
	sort.Strings(changed)
	return changed
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
