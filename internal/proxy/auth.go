package proxy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

type adminAuthKey struct{}

// WithAdminAuth marks a context as an authenticated management request. The
// playground reuses the chat handler in-process, so the admin session has
// already proved identity and the local API key check is skipped.
func WithAdminAuth(ctx context.Context) context.Context {
	return context.WithValue(ctx, adminAuthKey{}, true)
}

func adminAuthenticated(r *http.Request) bool {
	ok, _ := r.Context().Value(adminAuthKey{}).(bool)
	return ok
}

// APIKeyAuth validates local proxy API keys from config.json. An empty key
// list disables authentication for local-only deployments.
type APIKeyAuth struct {
	// digests holds sha256 of each key so comparisons are fixed length and
	// constant time.
	digests [][sha256.Size]byte
}

func NewAPIKeyAuth(keys []string) *APIKeyAuth {
	seen := make(map[string]bool, len(keys))
	digests := make([][sha256.Size]byte, 0, len(keys))
	for _, key := range keys {
		if key = strings.TrimSpace(key); key != "" && !seen[key] {
			seen[key] = true
			digests = append(digests, sha256.Sum256([]byte(key)))
		}
	}
	return &APIKeyAuth{digests: digests}
}

func tokenFromRequest(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	if h := r.Header.Get("X-Api-Key"); h != "" {
		return strings.TrimSpace(h)
	}
	return ""
}

// Allowed reports whether the request may use the proxy.
func (a *APIKeyAuth) Allowed(r *http.Request) bool {
	if len(a.digests) == 0 {
		return true
	}
	token := tokenFromRequest(r)
	if token == "" {
		return false
	}
	got := sha256.Sum256([]byte(token))
	match := 0
	// Check every key so timing does not reveal which one matched.
	for i := range a.digests {
		match |= subtle.ConstantTimeCompare(got[:], a.digests[i][:])
	}
	return match == 1
}
