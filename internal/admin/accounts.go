package admin

import (
	"net/http"
	"strconv"
	"time"
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
	views := make([]accountView, 0, len(lists))
	for i := range lists {
		views = append(views, viewAccount(&lists[i], refreshBefore))
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
