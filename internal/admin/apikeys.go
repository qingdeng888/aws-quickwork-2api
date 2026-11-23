package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// minAPIKeyLength keeps a user-supplied key from being trivially guessable.
const minAPIKeyLength = 8

// handleListAPIKeys returns the /v1 credentials, in order.
func (s *Server) handleListAPIKeys(w http.ResponseWriter, _ *http.Request) {
	keys := s.proxy.APIKeys()
	writeJSON(w, http.StatusOK, map[string]any{
		"keys":     keys,
		"count":    len(keys),
		"required": len(keys) > 0,
	})
}

// handleCreateAPIKey adds one key. An empty "key" generates a random one, which
// is returned so the console can show it once.
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &input, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	key := strings.TrimSpace(input.Key)
	generated := false
	if key == "" {
		value, err := generateAPIKey()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "key_generation_failed", err.Error())
			return
		}
		key, generated = value, true
	}
	if len(key) < minAPIKeyLength {
		writeError(w, http.StatusBadRequest, "invalid_key", fmt.Sprintf("API key must be at least %d characters", minAPIKeyLength))
		return
	}
	if strings.ContainsAny(key, " \t\r\n") {
		writeError(w, http.StatusBadRequest, "invalid_key", "API key must not contain whitespace")
		return
	}
	current := s.proxy.APIKeys()
	for _, existing := range current {
		if existing == key {
			writeError(w, http.StatusBadRequest, "duplicate_key", "this API key already exists")
			return
		}
	}
	keys := append(append([]string(nil), current...), key)
	if err := s.applyAPIKeys(keys); err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	s.logger.Printf("api key added (generated=%v total=%d)", generated, len(keys))
	writeJSON(w, http.StatusOK, map[string]any{
		"key":       key,
		"generated": generated,
		"keys":      keys,
		"count":     len(keys),
		"required":  true,
	})
}

// handleDeleteAPIKey removes one key. Removing the last key disables /v1
// authentication, which the response and the UI call out.
func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(r, &input, 4<<10); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	target := strings.TrimSpace(input.Key)
	if target == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "key is required")
		return
	}
	current := s.proxy.APIKeys()
	keys := make([]string, 0, len(current))
	removed := false
	for _, key := range current {
		if key == target {
			removed = true
			continue
		}
		keys = append(keys, key)
	}
	if !removed {
		writeError(w, http.StatusNotFound, "not_found", "API key not found")
		return
	}
	if err := s.applyAPIKeys(keys); err != nil {
		writeError(w, http.StatusInternalServerError, "config_error", err.Error())
		return
	}
	s.logger.Printf("api key removed (total=%d)", len(keys))
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted":  true,
		"keys":     keys,
		"count":    len(keys),
		"required": len(keys) > 0,
	})
}

// applyAPIKeys persists the key list to config.json and swaps the runtime set,
// so adding or deleting a key takes effect without a restart.
func (s *Server) applyAPIKeys(keys []string) error {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	path := s.cfg.Path
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return fmt.Errorf("parse config: %w", err)
	}
	encoded, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	raw["api_keys"] = encoded
	if _, err := writeConfigAtomic(path, raw); err != nil {
		return err
	}
	s.proxy.SetAPIKeys(keys)
	return nil
}

// generateAPIKey returns sk-<32 hex chars> from crypto/rand.
func generateAPIKey() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(buf), nil
}
