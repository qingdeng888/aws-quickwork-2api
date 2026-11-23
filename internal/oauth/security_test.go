package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestRandomTokenUniqueAndLength(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		tok, err := randomToken()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(raw) != 32 {
			t.Fatalf("token %q: decoded %d bytes, err %v", tok, len(raw), err)
		}
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

func TestNewLoginFlowPKCE(t *testing.T) {
	f, err := newLoginFlow(normalizeLoginOptions(LoginOptions{}), nil, 18080)
	if err != nil {
		t.Fatal(err)
	}
	if f.state == "" || f.nonce == "" || f.verifier == "" {
		t.Fatal("empty state/nonce/verifier")
	}
	if f.state == f.verifier || f.state == f.nonce || f.nonce == f.verifier {
		t.Fatal("state/nonce/verifier must be independent")
	}
	if n := len(f.verifier); n < 43 || n > 128 {
		t.Fatalf("verifier length %d outside RFC 7636 range", n)
	}
	sum := sha256.Sum256([]byte(f.verifier))
	if want := base64.RawURLEncoding.EncodeToString(sum[:]); f.challenge != want {
		t.Fatalf("challenge %q, want S256 %q", f.challenge, want)
	}
}

func TestCompleteLoginStateMismatch(t *testing.T) {
	f, err := newLoginFlow(normalizeLoginOptions(LoginOptions{}), nil, 18080)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"", "wrong", f.state + "x"} {
		q := url.Values{"code": {"abc"}}
		if state != "" {
			q.Set("state", state)
		}
		_, err := f.completeLogin(context.Background(), q)
		if err == nil || !strings.Contains(err.Error(), "state mismatch") {
			t.Fatalf("state %q: got %v, want state mismatch", state, err)
		}
		if statusOf(err) != http.StatusBadRequest {
			t.Fatalf("state %q: status %d", state, statusOf(err))
		}
	}
}

func TestCallbackHandlerRejectsStateMismatch(t *testing.T) {
	f, err := newLoginFlow(normalizeLoginOptions(LoginOptions{}), nil, 18080)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/?code=abc&state=bogus", nil)
	f.handle(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", rec.Code)
	}
	select {
	case <-f.done:
		if f.fail == nil || f.acct != nil {
			t.Fatal("flow should finish with failure")
		}
	default:
		t.Fatal("flow not finished")
	}
}

func TestErrorTextOmitsBodyAndTokens(t *testing.T) {
	secret := "SECRET-REFRESH-TOKEN-VALUE"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"bad token ` + secret + `","refresh_token":"` + secret + `"}`))
	}))
	defer srv.Close()

	_, err := postJSON(context.Background(), srv.URL+"/token", map[string]any{"refreshToken": secret}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) || strings.Contains(msg, "error_description") {
		t.Fatalf("error leaks body: %q", msg)
	}
	if !strings.Contains(msg, "HTTP 400") || !strings.Contains(msg, "error=invalid_grant") {
		t.Fatalf("error lacks status/code: %q", msg)
	}
}

func TestErrorCodeSanitizing(t *testing.T) {
	cases := map[string]string{
		`{"error":"invalid_client"}`:                      "invalid_client",
		`{"__type":"com.amazon#AccessDeniedException"}`:   "AccessDeniedException",
		`{"error":"a b<script>"}`:                         "abscript",
		`not json at all with token xyz`:                  "",
		`{"error":"` + strings.Repeat("x", 500) + `"}`:    strings.Repeat("x", 64),
		`{"message":"only free text here","error":12345}`: "",
	}
	for body, want := range cases {
		if got := errorCode([]byte(body)); got != want {
			t.Errorf("errorCode(%.40q) = %q, want %q", body, got, want)
		}
	}
}
