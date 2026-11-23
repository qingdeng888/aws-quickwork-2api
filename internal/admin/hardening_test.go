package admin

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"

	"quickwork-openai/internal/config"
)

func testServer(trustProxy bool) *Server {
	return &Server{cfg: &config.Config{WebUI: &config.WebUI{TrustProxy: trustProxy}}}
}

func TestClientIPRightmostForwardedFor(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2, 3.3.3.3 ")
	if got := testServer(true).clientIP(r); got != "3.3.3.3" {
		t.Fatalf("trusted proxy: got %q, want rightmost 3.3.3.3", got)
	}
	if got := testServer(false).clientIP(r); got != "10.0.0.1" {
		t.Fatalf("untrusted: got %q, want peer 10.0.0.1", got)
	}
}

func TestRequestIsSecureIgnoresForwardedProtoWithoutTrust(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	if requestIsSecure(r, false) {
		t.Fatal("X-Forwarded-Proto must be ignored without trust_proxy")
	}
	if !requestIsSecure(r, true) {
		t.Fatal("X-Forwarded-Proto should count with trust_proxy")
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.TLS = &tls.ConnectionState{}
	if !requestIsSecure(r2, false) {
		t.Fatal("TLS request must be secure")
	}
}

func TestUnmaskAPIKeysRoundTrip(t *testing.T) {
	existing := []string{"sk-aaaaaaaaaaaa1111", "sk-bbbbbbbbbbbb2222"}
	submitted := append(maskAPIKeys(existing), "sk-brand-new-key")
	got, err := unmaskAPIKeys(submitted, existing)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{existing[0], existing[1], "sk-brand-new-key"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
	if _, err := unmaskAPIKeys([]string{"sk-…9999"}, existing); err == nil {
		t.Fatal("unknown masked key should fail")
	}
}

func TestLoginCheckPerIPCap(t *testing.T) {
	s := testServer(false)
	s.attempts = make(map[string]*attemptWindow)
	ctx := t.Context()
	for i := 0; i < maxLoginChecksPerIP; i++ {
		if !s.acquireLoginCheck(ctx, "1.2.3.4") {
			t.Fatalf("check %d should pass", i)
		}
	}
	if s.acquireLoginCheck(ctx, "1.2.3.4") {
		t.Fatal("third concurrent check from one IP should be refused")
	}
	if !s.acquireLoginCheck(ctx, "5.6.7.8") {
		t.Fatal("another IP should still get a slot")
	}
	s.releaseLoginCheck("1.2.3.4")
	if !s.acquireLoginCheck(ctx, "1.2.3.4") {
		t.Fatal("slot should be free after release")
	}
}

func TestOriginAllowedBehindProxy(t *testing.T) {
	req := func(host, forwardedHost, forwardedProto string, withTLS bool) *http.Request {
		r := &http.Request{Host: host, Header: http.Header{}}
		if forwardedHost != "" {
			r.Header.Set("X-Forwarded-Host", forwardedHost)
		}
		if forwardedProto != "" {
			r.Header.Set("X-Forwarded-Proto", forwardedProto)
		}
		if withTLS {
			r.TLS = &tls.ConnectionState{}
		}
		return r
	}

	// The exact case from a Cloudflare + nginx deployment: nginx forwards
	// X-Forwarded-Proto https, the browser sends an https Origin.
	if !originAllowed(req("aws.example.com", "", "https", false), "https://aws.example.com", true) {
		t.Fatal("https origin should pass when trust_proxy is on")
	}
	// Without trust_proxy the forwarded protocol is ignored, so the check
	// fails and the log explains why. That is the expected secure default.
	if originAllowed(req("aws.example.com", "", "https", false), "https://aws.example.com", false) {
		t.Fatal("forwarded proto must be ignored without trust_proxy")
	}
	// A proxy that rewrites Host still passes through X-Forwarded-Host.
	if !originAllowed(req("127.0.0.1:18099", "aws.example.com", "https", false), "https://aws.example.com", true) {
		t.Fatal("x-forwarded-host should be accepted with trust_proxy")
	}
	if originAllowed(req("127.0.0.1:18099", "aws.example.com", "https", false), "https://evil.example.com", true) {
		t.Fatal("a foreign origin must be rejected")
	}
	// Direct TLS keeps working without any proxy headers.
	if !originAllowed(req("aws.example.com", "", "", true), "https://aws.example.com", false) {
		t.Fatal("direct TLS origin should pass")
	}
}
