package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// LookupEnv matches os.LookupEnv, which keeps "set but empty" distinguishable
// from "not set".
type LookupEnv func(key string) (string, bool)

// ApplyEnv overlays configuration from environment variables and returns the
// names of the variables that were applied. Only non-empty variables win, so an
// unset variable keeps whatever the file already holds. This is what the
// container entrypoint calls before starting the server, which lets .env drive
// the deployment without rewriting the JSON by hand.
func ApplyEnv(cfg *Config, lookup LookupEnv) ([]string, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is nil")
	}
	if lookup == nil {
		return nil, fmt.Errorf("lookup function is nil")
	}
	applied := make([]string, 0, 16)

	apply := func(name string, setter func(string) error) error {
		raw, ok := lookup(name)
		if !ok {
			return nil
		}
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil
		}
		if err := setter(value); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		applied = append(applied, name)
		return nil
	}

	text := func(target *string) func(string) error {
		return func(value string) error {
			*target = value
			return nil
		}
	}
	number := func(target *int) func(string) error {
		return func(value string) error {
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("expected an integer, got %q", value)
			}
			*target = parsed
			return nil
		}
	}
	boolean := func(target *bool) func(string) error {
		return func(value string) error {
			parsed, err := parseBool(value)
			if err != nil {
				return err
			}
			*target = parsed
			return nil
		}
	}
	duration := func(target *Duration) func(string) error {
		return func(value string) error {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("expected a duration like 30s, 5m or 12h: %w", err)
			}
			*target = Duration(parsed)
			return nil
		}
	}
	list := func(target *[]string) func(string) error {
		return func(value string) error {
			items := make([]string, 0, 4)
			seen := make(map[string]bool, 4)
			for _, item := range strings.Split(value, ",") {
				item = strings.TrimSpace(item)
				if item == "" || seen[item] {
					continue
				}
				seen[item] = true
				items = append(items, item)
			}
			*target = items
			return nil
		}
	}

	type override struct {
		name  string
		apply func(string) error
	}
	overrides := []override{
		{"ADDR", text(&cfg.Addr)},
		{"DATABASE", text(&cfg.Database)},
		{"API_KEYS", list(&cfg.APIKeys)},
		{"MODELS", list(&cfg.Models)},
		{"DEFAULT_MODEL", text(&cfg.DefaultModel)},
		{"REQUEST_TIMEOUT", duration(&cfg.RequestTimeout)},
		{"REFRESH_BEFORE", duration(&cfg.RefreshBefore)},
		{"MAX_ATTEMPTS", number(&cfg.MaxAttempts)},
		{"APP_VERSION", text(&cfg.AppVersion)},
		{"SURFACE", text(&cfg.Surface)},
		{"TIMEZONE", text(&cfg.Timezone)},
		{"LOCALE", text(&cfg.Locale)},
		{"THINKING_EFFORT", text(&cfg.ThinkingEffort)},
		{"CHAT_AGENT_ID", text(&cfg.ChatAgentID)},
		{"KEEP_SESSIONS", boolean(&cfg.KeepSessions)},
		{"SESSION_REUSE", boolean(&cfg.SessionReuse)},
		{"SESSION_TTL", duration(&cfg.SessionTTL)},
		{"OAUTH_PROVIDER", text(&cfg.OAuth.Provider)},
		{"OAUTH_STAGE", text(&cfg.OAuth.BuildStage)},
		{"OAUTH_REGION", text(&cfg.OAuth.Region)},
		{"OAUTH_CALLBACK_PORT", number(&cfg.OAuth.CallbackPort)},
		{"OAUTH_TIMEOUT", duration(&cfg.OAuth.Timeout)},
	}
	for _, item := range overrides {
		if err := apply(item.name, item.apply); err != nil {
			return applied, err
		}
	}

	if cfg.WebUI == nil {
		defaults := Default()
		cfg.WebUI = defaults.WebUI
	}
	webui := []override{
		{"WEBUI_ENABLED", boolean(&cfg.WebUI.Enabled)},
		{"WEBUI_SESSION_TTL", duration(&cfg.WebUI.SessionTTL)},
		{"WEBUI_MAX_ATTEMPTS", number(&cfg.WebUI.MaxAttempts)},
		{"WEBUI_TRUST_PROXY", boolean(&cfg.WebUI.TrustProxy)},
		{"WEBUI_PASSWORD", func(value string) error {
			cfg.WebUI.Password = value
			cfg.WebUI.PasswordHash = ""
			return nil
		}},
		{"WEBUI_PASSWORD_HASH", func(value string) error {
			if !ValidPasswordHash(value) {
				return fmt.Errorf("not a pbkdf2-sha256 hash; generate one with `quickproxy passwd -password ...`")
			}
			cfg.WebUI.PasswordHash = value
			cfg.WebUI.Password = ""
			return nil
		}},
	}
	for _, item := range webui {
		if err := apply(item.name, item.apply); err != nil {
			return applied, err
		}
	}
	return applied, nil
}

// Validate re-checks a configuration after in-place changes.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	applyDefaults(c)
	return c.validate()
}

func parseBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("expected true/false, got %q", value)
	}
}
