package admin

import (
	"net/http"
	"strconv"

	"quickwork-openai/internal/proxy"
)

// handleLogs returns the recent chat summaries kept in memory. The records
// mirror the stdout completion line, without prompt or completion text.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			limit = parsed
		}
	}
	if limit > 500 {
		limit = 500
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": s.proxy.ChatRecords(limit),
		"stats": s.proxy.ChatStats(),
		"limit": limit,
	})
}

// handlePlayground forwards the UI's request to the real chat handler with an
// admin-authenticated context, so the browser never needs the proxy API key.
// Streaming passes straight through because the response writer is the
// original one.
func (s *Server) handlePlayground(w http.ResponseWriter, r *http.Request) {
	if r.Body == nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "empty request body")
		return
	}
	body := http.MaxBytesReader(w, r.Body, 8<<20)
	inner, err := http.NewRequestWithContext(proxy.WithAdminAuth(r.Context()), http.MethodPost, "/v1/chat/completions", body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	inner.Header.Set("Content-Type", "application/json")
	if accept := r.Header.Get("Accept"); accept != "" {
		inner.Header.Set("Accept", accept)
	}
	s.proxy.ChatHandler().ServeHTTP(w, inner)
}
