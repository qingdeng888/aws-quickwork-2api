// Package config loads and persists the server configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Duration is a time.Duration encoded as a Go duration string ("5m", "1h").
type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		v, err := time.ParseDuration(strings.TrimSpace(s))
		if err != nil {
			return err
		}
		*d = Duration(v)
		return nil
	}
	var seconds float64
	if err := json.Unmarshal(data, &seconds); err != nil {
		return fmt.Errorf("duration must be a string like \"5m\" or seconds")
	}
	*d = Duration(seconds * float64(time.Second))
	return nil
}

// OAuth holds defaults used by the `login` command.
type OAuth struct {
	Provider     string   `json:"provider"`
	BuildStage   string   `json:"build_stage"`
	Region       string   `json:"region"`
	CallbackPort int      `json:"callback_port"`
	Timeout      Duration `json:"timeout"`
}

// WebUI configures the password-protected management interface served under
// /admin on the proxy's own address. Password holds a plaintext password;
// PasswordHash (pbkdf2-sha256, written by `quickproxy passwd -set`) wins when
// both are present. With neither set the management interface stays off.
type WebUI struct {
	Enabled      bool     `json:"enabled"`
	Password     string   `json:"password"`
	PasswordHash string   `json:"password_hash"`
	SessionTTL   Duration `json:"session_ttl"`
	MaxAttempts  int      `json:"max_attempts"`
	TrustProxy   bool     `json:"trust_proxy"`
}

// ProxyPool configures the shared outbound proxy pool. Accounts set to
// "follow the pool" rotate through its enabled proxies; a disabled pool makes
// them connect directly.
type ProxyPool struct {
	Enabled       bool     `json:"enabled"`
	TestURL       string   `json:"test_url"`
	CheckInterval Duration `json:"check_interval"`
}

// Config is the server configuration stored in config.json.
type Config struct {
	Addr             string     `json:"addr"`
	Database         string     `json:"database"`
	APIKeys          []string   `json:"api_keys"`
	Models           []string   `json:"models"`
	DefaultModel     string     `json:"default_model"`
	RequestTimeout   Duration   `json:"request_timeout"`
	RefreshBefore    Duration   `json:"refresh_before"`
	MaxAttempts      int        `json:"max_attempts"`
	AppVersion       string     `json:"app_version"`
	Surface          string     `json:"surface"`
	OutpostID        string     `json:"outpost_id"`
	WorkingSessionID string     `json:"working_session_id"`
	Timezone         string     `json:"timezone"`
	Locale           string     `json:"locale"`
	ThinkingEffort   string     `json:"thinking_effort"`
	ChatAgentID      string     `json:"chat_agent_id"`
	KeepSessions     bool       `json:"keep_sessions"`
	SessionReuse     bool       `json:"session_reuse"`
	SessionTTL       Duration   `json:"session_ttl"`
	OAuth            OAuth      `json:"oauth"`
	WebUI            *WebUI     `json:"webui,omitempty"`
	ProxyPool        *ProxyPool `json:"proxy_pool,omitempty"`

	// Path is the absolute config file path; not serialized.
	Path string `json:"-"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Addr:           "127.0.0.1:8080",
		Database:       "data/quickwork.db",
		APIKeys:        []string{},
		Models:         []string{"smart", "fast", "balanced", "auto"},
		DefaultModel:   "smart",
		RequestTimeout: Duration(5 * time.Minute),
		RefreshBefore:  Duration(10 * time.Minute),
		MaxAttempts:    3,
		AppVersion:     "1.2.4660",
		Surface:        "QUICK_WORK_DESKTOP_WINDOWS",
		Timezone:       "UTC",
		Locale:         "en-US",
		ThinkingEffort: "",
		KeepSessions:   false,
		SessionReuse:   true,
		SessionTTL:     Duration(30 * time.Minute),
		OAuth: OAuth{
			Provider:     "social",
			BuildStage:   "prod",
			CallbackPort: 18080,
			Timeout:      Duration(time.Hour),
		},
		ProxyPool: &ProxyPool{
			Enabled:       true,
			TestURL:       "https://api.ipify.org?format=json",
			CheckInterval: Duration(10 * time.Minute),
		},
		WebUI: &WebUI{
			Enabled:     true,
			SessionTTL:  Duration(12 * time.Hour),
			MaxAttempts: 10,
		},
	}
}

// Load reads configPath, creating it with defaults when it does not exist.
// Relative paths inside the config (database) are resolved against the
// config file's directory so the server behaves the same from any cwd.
func Load(configPath string) (*Config, error) {
	if strings.TrimSpace(configPath) == "" {
		configPath = "config.json"
	}
	abs, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	cfg := Default()
	body, err := os.ReadFile(abs)
	switch {
	case err == nil:
		if err := json.Unmarshal(body, &cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", abs, err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := write(abs, &cfg); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	cfg.Path = abs
	applyDefaults(&cfg)
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}
	if !filepath.IsAbs(cfg.Database) {
		cfg.Database = filepath.Join(filepath.Dir(abs), cfg.Database)
	}
	return &cfg, nil
}

func write(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o600)
}

func applyDefaults(c *Config) {
	d := Default()
	if strings.TrimSpace(c.Addr) == "" {
		c.Addr = d.Addr
	}
	if strings.TrimSpace(c.Database) == "" {
		c.Database = d.Database
	}
	if len(c.Models) == 0 {
		c.Models = d.Models
	}
	if strings.TrimSpace(c.DefaultModel) == "" {
		c.DefaultModel = d.DefaultModel
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = d.RequestTimeout
	}
	if c.RefreshBefore <= 0 {
		c.RefreshBefore = d.RefreshBefore
	}
	if c.SessionTTL <= 0 {
		c.SessionTTL = d.SessionTTL
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = d.MaxAttempts
	}
	if strings.TrimSpace(c.AppVersion) == "" {
		c.AppVersion = d.AppVersion
	}
	if strings.TrimSpace(c.Surface) == "" {
		c.Surface = d.Surface
	}
	if strings.TrimSpace(c.Timezone) == "" {
		c.Timezone = d.Timezone
	}
	if strings.TrimSpace(c.Locale) == "" {
		c.Locale = d.Locale
	}
	if c.ProxyPool == nil {
		c.ProxyPool = d.ProxyPool
	} else {
		if strings.TrimSpace(c.ProxyPool.TestURL) == "" {
			c.ProxyPool.TestURL = d.ProxyPool.TestURL
		}
		if c.ProxyPool.CheckInterval <= 0 {
			c.ProxyPool.CheckInterval = d.ProxyPool.CheckInterval
		}
	}
	if strings.TrimSpace(c.OAuth.Provider) == "" {
		c.OAuth.Provider = d.OAuth.Provider
	}
	if strings.TrimSpace(c.OAuth.BuildStage) == "" {
		c.OAuth.BuildStage = d.OAuth.BuildStage
	}
	if c.OAuth.CallbackPort <= 0 {
		c.OAuth.CallbackPort = d.OAuth.CallbackPort
	}
	if c.OAuth.Timeout <= 0 {
		c.OAuth.Timeout = d.OAuth.Timeout
	}
	if c.WebUI == nil {
		c.WebUI = d.WebUI
	} else {
		if c.WebUI.SessionTTL <= 0 {
			c.WebUI.SessionTTL = d.WebUI.SessionTTL
		}
		if c.WebUI.MaxAttempts <= 0 {
			c.WebUI.MaxAttempts = d.WebUI.MaxAttempts
		}
	}
}

func (c *Config) validate() error {
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request_timeout must be positive")
	}
	if c.OAuth.CallbackPort < 0 || c.OAuth.CallbackPort > 65535 {
		return fmt.Errorf("oauth.callback_port must be 0-65535")
	}
	switch strings.ToLower(c.OAuth.Provider) {
	case "social", "enterprise", "gateway", "social_gateway":
	default:
		return fmt.Errorf("oauth.provider must be social, enterprise, gateway or social_gateway")
	}
	switch strings.ToLower(c.OAuth.BuildStage) {
	case "prod", "beta", "integ", "dev":
	default:
		return fmt.Errorf("oauth.build_stage must be prod, beta, integ or dev")
	}
	if c.WebUI == nil {
		return fmt.Errorf("webui block is missing")
	}
	if c.WebUI.SessionTTL <= 0 {
		return fmt.Errorf("webui.session_ttl must be positive")
	}
	if c.WebUI.MaxAttempts < 1 || c.WebUI.MaxAttempts > 100 {
		return fmt.Errorf("webui.max_attempts must be between 1 and 100")
	}
	if c.WebUI.PasswordHash != "" && !ValidPasswordHash(c.WebUI.PasswordHash) {
		return fmt.Errorf("webui.password_hash is not a pbkdf2-sha256 hash; run `quickproxy passwd -set`")
	}
	return nil
}

// Save writes the configuration back to the file it was loaded from.
func Save(cfg *Config) error {
	if cfg == nil || strings.TrimSpace(cfg.Path) == "" {
		return errors.New("config path is not set")
	}
	return write(cfg.Path, cfg)
}
