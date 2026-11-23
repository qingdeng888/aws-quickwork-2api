package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quickwork-openai/internal/accounts"
)

// handleAccounts lists every stored account, including disabled ones, so the
// UI can show the whole rotation.
func (s *Server) handleAccounts(w http.ResponseWriter, r *http.Request) {
	lists, err := s.store.List(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	refreshBefore := s.cfg.RefreshBefore.Duration()
	labels := s.proxyLabelMap(r.Context())
	views := make([]accountView, 0, len(lists))
	for i := range lists {
		view := viewAccount(&lists[i], refreshBefore)
		view.ProxyLabel = labels[view.ProxyID]
		views = append(views, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": views, "total": len(views)})
}

// handleRefreshAccounts refreshes one account when "id" is set, otherwise all.
func (s *Server) handleRefreshAccounts(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ID int64 `json:"id"`
	}
	_ = decodeJSON(r, &input, 4<<10)
	ctx := r.Context()
	results := make([]map[string]any, 0, 4)
	failures := 0

	if input.ID > 0 {
		account, err := s.accounts.RefreshOne(ctx, input.ID)
		result := map[string]any{"id": input.ID}
		if err != nil {
			failures++
			result["error"] = err.Error()
			s.logger.Printf("refresh account %d failed: %v", input.ID, err)
		} else {
			result["label"] = accountLabel(account)
			result["expires_at"] = account.ExpiresAt
			result["expires_in"] = account.ExpiresAt - time.Now().Unix()
			s.logger.Printf("refreshed account %d", input.ID)
		}
		results = append(results, result)
	} else {
		all, err := s.accounts.RefreshAll(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "refresh_failed", err.Error())
			return
		}
		refreshed := 0
		for _, item := range all {
			entry := map[string]any{}
			if item.Account != nil {
				entry["id"] = item.Account.ID
				entry["label"] = accountLabel(item.Account)
				entry["expires_at"] = item.Account.ExpiresAt
				entry["expires_in"] = item.Account.ExpiresAt - time.Now().Unix()
			}
			if item.Err != nil {
				failures++
				entry["error"] = item.Err.Error()
			} else {
				refreshed++
			}
			results = append(results, entry)
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": results, "refreshed": refreshed, "failed": failures})
		s.logger.Printf("refresh all: %d ok, %d failed", refreshed, failures)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "refreshed": len(results) - failures, "failed": failures})
}

// handleDeleteAccount removes one stored account.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "account id must be a positive integer")
		return
	}
	account, err := s.store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	removed, err := s.store.Delete(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	s.logger.Printf("deleted account %d (%s)", id, accountLabel(account))
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

// handleAccountState turns rotation on or off for one account. A disabled
// account keeps its tokens but is skipped by rotation, refresh-all and the
// background refresh sweep, so it can be parked without deleting it.
func (s *Server) handleAccountState(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "account id must be a positive integer")
		return
	}
	var disabled bool
	switch r.PathValue("action") {
	case "disable":
		disabled = true
	case "enable":
		disabled = false
	default:
		writeError(w, http.StatusNotFound, "not_found", "unknown account action")
		return
	}
	account, err := s.store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "account not found")
		return
	}
	if err := s.store.SetDisabled(r.Context(), id, disabled); err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	account.Disabled = disabled
	state := "enabled"
	if disabled {
		state = "disabled"
	}
	s.logger.Printf("account %d (%s) %s", id, accountLabel(account), state)
	view := viewAccount(account, s.cfg.RefreshBefore.Duration())
	view.ProxyLabel = s.proxyLabelMap(r.Context())[view.ProxyID]
	writeJSON(w, http.StatusOK, map[string]any{
		"id":       id,
		"disabled": disabled,
		"account":  view,
	})
}

// handleExportAccounts downloads every stored account as a JSON document.
// Secrets are included by default so the file can restore a working install;
// pass include_secrets=false for a metadata-only copy.
func (s *Server) handleExportAccounts(w http.ResponseWriter, r *http.Request) {
	includeSecrets := true
	if raw := strings.TrimSpace(r.URL.Query().Get("include_secrets")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "include_secrets must be true or false")
			return
		}
		includeSecrets = parsed
	}
	result, err := accounts.Export(r.Context(), s.store, includeSecrets)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	name := fmt.Sprintf("quickwork-accounts-%s.json", time.Now().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(result.File)
	s.logger.Printf("exported %d accounts (disabled=%d secrets=%v)",
		result.AccountCount, result.DisabledCount, includeSecrets)
}

// handleImportAccounts merges an exported document into the store. Existing
// accounts (same email + tenant_url) are skipped unless "update" is set, which
// makes repeated imports incremental instead of destructive.
func (s *Server) handleImportAccounts(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Update bool `json:"update"`
		accounts.TransferFile
	}
	if err := decodeJSON(r, &input, 8<<20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(input.Accounts) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request", "import file contains no accounts")
		return
	}
	result, err := accounts.Import(r.Context(), s.store, &input.TransferFile, input.Update)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	status := "ok"
	if len(result.Errors) > 0 {
		status = "partial"
	}
	s.logger.Printf("import accounts: imported=%d updated=%d skipped=%d errors=%d",
		result.Imported, result.Updated, result.Skipped, len(result.Errors))
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   status,
		"imported": result.Imported,
		"updated":  result.Updated,
		"skipped":  result.Skipped,
		"total":    len(input.Accounts),
		"errors":   result.Errors,
	})
}
