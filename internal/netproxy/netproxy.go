// Package netproxy carries the outbound HTTP proxy for one request through the
// context, so every HTTP helper (OAuth refresh, attachment upload, proxy
// checks) uses the proxy selected for the account without changing all their
// signatures.
package netproxy

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type contextKey struct{}

// WithProxy returns a child context that binds outbound HTTP to proxy.
// A nil proxy means a direct connection.
func WithProxy(ctx context.Context, proxy *url.URL) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextKey{}, proxy)
}

// FromContext returns the proxy bound by WithProxy, or nil for direct.
func FromContext(ctx context.Context) *url.URL {
	if ctx == nil {
		return nil
	}
	if proxy, ok := ctx.Value(contextKey{}).(*url.URL); ok {
		return proxy
	}
	return nil
}

// ProxyFunc adapts a proxy URL to http.Transport.Proxy. A nil URL disables the
// proxy (direct connection).
func ProxyFunc(proxy *url.URL) func(*http.Request) (*url.URL, error) {
	if proxy == nil {
		return nil
	}
	return http.ProxyURL(proxy)
}

// NewTransport builds a transport that dials through proxy. Connection reuse is
// enabled so a proxy check does not leave sockets behind.
func NewTransport(proxy *url.URL) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = ProxyFunc(proxy)
	transport.MaxIdleConnsPerHost = 4
	return transport
}

// NewClient returns an HTTP client that uses the proxy bound to ctx.
func NewClient(ctx context.Context, timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: NewTransport(FromContext(ctx))}
}

// Parse validates and normalizes a proxy URL. Supported schemes are http,
// https, socks5 and socks5h.
func Parse(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("proxy is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (use http, https, socks5 or socks5h)", parsed.Scheme)
	}
	if parsed.Host == "" || parsed.Hostname() == "" {
		return nil, fmt.Errorf("proxy has no host")
	}
	if parsed.Port() == "" {
		switch strings.ToLower(parsed.Scheme) {
		case "https":
			parsed.Host = parsed.Host + ":443"
		default:
			parsed.Host = parsed.Host + ":80"
		}
	}
	return parsed, nil
}

// Redact hides credentials in a proxy URL for logs and API responses.
func Redact(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	cloned := *parsed
	cloned.User = nil
	return cloned.String()
}
