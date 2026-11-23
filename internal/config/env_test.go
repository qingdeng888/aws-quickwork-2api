package config

import (
	"strings"
	"testing"
	"time"
)

func envFrom(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestApplyEnvOverrides(t *testing.T) {
	cfg := Default()
	applied, err := ApplyEnv(&cfg, envFrom(map[string]string{
		"ADDR":              "0.0.0.0:9090",
		"API_KEYS":          "sk-a, sk-b, sk-a, ",
		"MODELS":            "smart, fast",
		"DEFAULT_MODEL":     "fast",
		"REQUEST_TIMEOUT":   "90s",
		"MAX_ATTEMPTS":      "5",
		"SESSION_REUSE":     "false",
		"TIMEZONE":          "Asia/Shanghai",
		"WEBUI_PASSWORD":    "docker-pass-123",
		"WEBUI_TRUST_PROXY": "yes",
		"UNRELATED":         "ignored",
	}))
	if err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Addr != "0.0.0.0:9090" {
		t.Errorf("addr = %q", cfg.Addr)
	}
	if len(cfg.APIKeys) != 2 || cfg.APIKeys[0] != "sk-a" || cfg.APIKeys[1] != "sk-b" {
		t.Errorf("api keys = %v", cfg.APIKeys)
	}
	if len(cfg.Models) != 2 || cfg.Models[1] != "fast" {
		t.Errorf("models = %v", cfg.Models)
	}
	if cfg.DefaultModel != "fast" {
		t.Errorf("default model = %q", cfg.DefaultModel)
	}
	if time.Duration(cfg.RequestTimeout) != 90*time.Second {
		t.Errorf("request timeout = %v", time.Duration(cfg.RequestTimeout))
	}
	if cfg.MaxAttempts != 5 {
		t.Errorf("max attempts = %d", cfg.MaxAttempts)
	}
	if cfg.SessionReuse {
		t.Error("session reuse should be false")
	}
	if cfg.Timezone != "Asia/Shanghai" {
		t.Errorf("timezone = %q", cfg.Timezone)
	}
	if cfg.WebUI == nil || !cfg.CheckWebUIPassword("docker-pass-123") {
		t.Fatalf("webui password not applied: %+v", cfg.WebUI)
	}
	if cfg.WebUI.Password != "" || !ValidPasswordHash(cfg.WebUI.PasswordHash) {
		t.Errorf("WEBUI_PASSWORD must be stored hashed only, got plain=%q hash=%q", cfg.WebUI.Password, cfg.WebUI.PasswordHash)
	}
	if !cfg.WebUI.TrustProxy {
		t.Error("trust_proxy should be true")
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	joined := strings.Join(applied, ",")
	for _, name := range []string{"ADDR", "API_KEYS", "WEBUI_PASSWORD", "WEBUI_TRUST_PROXY"} {
		if !strings.Contains(joined, name) {
			t.Errorf("%s missing from applied list %q", name, joined)
		}
	}
	if strings.Contains(joined, "UNRELATED") {
		t.Errorf("unknown variable should not be applied: %q", joined)
	}
}

func TestApplyEnvSkipsEmptyValues(t *testing.T) {
	cfg := Default()
	cfg.Addr = "127.0.0.1:8080"
	cfg.WebUI.Password = "keep-me"
	if _, err := ApplyEnv(&cfg, envFrom(map[string]string{
		"ADDR":           "   ",
		"WEBUI_PASSWORD": "",
	})); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("empty ADDR should not override, got %q", cfg.Addr)
	}
	if cfg.WebUI.Password != "keep-me" {
		t.Errorf("empty WEBUI_PASSWORD should not override, got %q", cfg.WebUI.Password)
	}
}

func TestApplyEnvPasswordHashWins(t *testing.T) {
	hash, err := HashPassword("consistent-horse-42")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	cfg := Default()
	cfg.WebUI.Password = "plaintext"
	if _, err := ApplyEnv(&cfg, envFrom(map[string]string{
		"WEBUI_PASSWORD_HASH": hash,
	})); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.WebUI.Password != "" {
		t.Errorf("plaintext password should be cleared, got %q", cfg.WebUI.Password)
	}
	if cfg.WebUI.PasswordHash != hash {
		t.Errorf("hash not stored")
	}
	if !cfg.CheckWebUIPassword("consistent-horse-42") {
		t.Error("hash should verify the original password")
	}
}

func TestApplyEnvRejectsBadValues(t *testing.T) {
	cases := map[string]map[string]string{
		"integer":  {"MAX_ATTEMPTS": "abc"},
		"bool":     {"WEBUI_ENABLED": "maybe"},
		"duration": {"REQUEST_TIMEOUT": "5 minutes"},
		"hash":     {"WEBUI_PASSWORD_HASH": "not-a-hash"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			if _, err := ApplyEnv(&cfg, envFrom(values)); err == nil {
				t.Fatalf("expected an error for %v", values)
			}
		})
	}
}

func TestApplyEnvValidationCatchesBadCombination(t *testing.T) {
	cfg := Default()
	if _, err := ApplyEnv(&cfg, envFrom(map[string]string{"OAUTH_PROVIDER": "nope"})); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected Validate to reject an unknown provider")
	}
}

func TestApplyEnvKeepsExistingHash(t *testing.T) {
	adminHash, err := HashPassword("changed-by-admin")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.WebUI.PasswordHash = adminHash
	applied, err := ApplyEnv(&cfg, envFrom(map[string]string{"WEBUI_PASSWORD": "from-env-pass"}))
	if err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if cfg.WebUI.PasswordHash != adminHash || !cfg.CheckWebUIPassword("changed-by-admin") {
		t.Error("existing password_hash must not be overridden without WEBUI_PASSWORD_FORCE")
	}
	if strings.Contains(strings.Join(applied, ","), "WEBUI_PASSWORD") {
		t.Errorf("skipped password reported as applied: %v", applied)
	}
}

func TestApplyEnvForceOverridesHash(t *testing.T) {
	adminHash, err := HashPassword("changed-by-admin")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.WebUI.PasswordHash = adminHash
	if _, err := ApplyEnv(&cfg, envFrom(map[string]string{
		"WEBUI_PASSWORD":       "from-env-pass",
		"WEBUI_PASSWORD_FORCE": "1",
	})); err != nil {
		t.Fatalf("ApplyEnv: %v", err)
	}
	if !cfg.CheckWebUIPassword("from-env-pass") || cfg.CheckWebUIPassword("changed-by-admin") {
		t.Error("WEBUI_PASSWORD_FORCE should replace the stored hash")
	}
	if cfg.WebUI.Password != "" {
		t.Error("plaintext must not be persisted")
	}
}

func TestApplyEnvRejectsShortPassword(t *testing.T) {
	cfg := Default()
	if _, err := ApplyEnv(&cfg, envFrom(map[string]string{"WEBUI_PASSWORD": "short"})); err == nil {
		t.Fatal("expected short WEBUI_PASSWORD to be rejected")
	}
}
