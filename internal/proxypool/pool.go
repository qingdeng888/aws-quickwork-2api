// Package proxypool resolves which outbound proxy a request should use and
// runs the health checks behind the /admin proxy page.
package proxypool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"quickwork-openai/internal/netproxy"
	"quickwork-openai/internal/store"
)

// DefaultTestURL is the endpoint used by the "test proxy" button. It answers
// with the exit IP, which also confirms what the upstream sees.
const DefaultTestURL = "https://api.ipify.org?format=json"

// CheckTimeout bounds one proxy check.
const CheckTimeout = 10 * time.Second

// Pool resolves account bindings against the stored proxy list.
type Pool struct {
	store *store.Store
	next  atomic.Uint64

	mu     sync.Mutex
	locks  map[int64]*sync.Mutex
	checks atomic.Int64
}

// New builds a proxy pool over the given store.
func New(st *store.Store) *Pool {
	return &Pool{store: st, locks: make(map[int64]*sync.Mutex)}
}

// Resolve returns the proxy URL for one account. A nil URL means a direct
// connection. Accounts with ProxyID 0 follow the pool when poolEnabled is true
// and no proxy is healthy; the pool rotates round-robin, preferring proxies
// whose last health check passed so a dead entry does not take traffic.
func (p *Pool) Resolve(ctx context.Context, account *store.Account, poolEnabled bool) (*url.URL, error) {
	if account == nil {
		return nil, nil
	}
	switch {
	case account.ProxyID < 0:
		return nil, nil
	case account.ProxyID > 0:
		proxy, err := p.store.GetProxy(ctx, account.ProxyID)
		if err != nil {
			return nil, fmt.Errorf("account %d: proxy %d not found", account.ID, account.ProxyID)
		}
		if !proxy.Enabled {
			return nil, fmt.Errorf("account %d: proxy %d (%s) is disabled", account.ID, proxy.ID, netproxy.Redact(proxy.URL))
		}
		return netproxy.Parse(proxy.URL)
	}
	if !poolEnabled {
		return nil, nil
	}
	list, err := p.store.EnabledProxies(ctx)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	healthy := make([]store.Proxy, 0, len(list))
	for _, proxy := range list {
		if proxy.Healthy {
			healthy = append(healthy, proxy)
		}
	}
	candidates := healthy
	if len(candidates) == 0 {
		candidates = list
	}
	index := (p.next.Add(1) - 1) % uint64(len(candidates))
	return netproxy.Parse(candidates[index].URL)
}

// Rotate is Resolve for callers that want the pool pick without an account.
func (p *Pool) Rotate(ctx context.Context, poolEnabled bool) (*url.URL, error) {
	return p.Resolve(ctx, &store.Account{}, poolEnabled)
}

// CheckResult is the outcome of one proxy connectivity test.
type CheckResult struct {
	Healthy   bool   `json:"healthy"`
	LatencyMS int    `json:"latency_ms"`
	ExitIP    string `json:"exit_ip"`
	Error     string `json:"error,omitempty"`
}

// Check sends a request to testURL through rawProxy and reports latency and
// the exit IP the far end sees.
func Check(ctx context.Context, rawProxy, testURL string, timeout time.Duration) CheckResult {
	proxy, err := netproxy.Parse(rawProxy)
	if err != nil {
		return CheckResult{Error: err.Error()}
	}
	if strings.TrimSpace(testURL) == "" {
		testURL = DefaultTestURL
	}
	if timeout <= 0 {
		timeout = CheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	transport := netproxy.NewTransport(proxy)
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: timeout, Transport: transport}

	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return CheckResult{Error: err.Error()}
	}
	req.Header.Set("User-Agent", "quickwork-openai proxy-check")
	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{Error: netproxy.Redact(err.Error())}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	latency := time.Since(started)
	if err != nil {
		return CheckResult{LatencyMS: int(latency.Milliseconds()), Error: "read response: " + netproxy.Redact(err.Error())}
	}
	result := CheckResult{
		Healthy:   resp.StatusCode >= 200 && resp.StatusCode < 400,
		LatencyMS: int(latency.Milliseconds()),
		ExitIP:    exitIP(body),
	}
	if !result.Healthy {
		result.Error = fmt.Sprintf("HTTP %d from %s", resp.StatusCode, testURL)
	}
	return result
}

// exitIP extracts an IP from a JSON {"ip":...} body or a bare IP response.
func exitIP(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	var payload struct {
		IP    string `json:"ip"`
		Query string `json:"query"`
	}
	if json.Unmarshal([]byte(trimmed), &payload) == nil {
		for _, candidate := range []string{payload.IP, payload.Query} {
			if net.ParseIP(strings.TrimSpace(candidate)) != nil {
				return strings.TrimSpace(candidate)
			}
		}
	}
	if net.ParseIP(trimmed) != nil {
		return trimmed
	}
	return ""
}

// CheckAndRecord runs one check and stores the outcome. Checks for the same
// proxy are serialized so the UI button and the background sweep cannot fight.
func (p *Pool) CheckAndRecord(ctx context.Context, id int64, rawProxy, testURL string) (CheckResult, error) {
	lock := p.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	result := Check(ctx, rawProxy, testURL, CheckTimeout)
	p.checks.Add(1)
	if err := p.store.SetProxyCheck(ctx, id, result.Healthy, result.LatencyMS, result.ExitIP, result.Error); err != nil {
		return result, err
	}
	return result, nil
}

func (p *Pool) lockFor(id int64) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	lock := p.locks[id]
	if lock == nil {
		lock = &sync.Mutex{}
		p.locks[id] = lock
	}
	return lock
}

// KeepChecked sweeps every enabled proxy at the configured interval. The first
// pass runs immediately so a restart recovers health state without waiting.
func (p *Pool) KeepChecked(ctx context.Context, interval time.Duration, testURL string, logf func(format string, args ...any)) {
	if interval <= 0 {
		return
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	pass := func() {
		list, err := p.store.EnabledProxies(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logf("proxy sweep failed: %v", err)
			}
			return
		}
		for i := range list {
			if ctx.Err() != nil {
				return
			}
			proxy := &list[i]
			result := Check(ctx, proxy.URL, testURL, CheckTimeout)
			_ = p.store.SetProxyCheck(ctx, proxy.ID, result.Healthy, result.LatencyMS, result.ExitIP, result.Error)
			if !result.Healthy {
				logf("proxy %d (%s) check failed: %s", proxy.ID, netproxy.Redact(proxy.URL), result.Error)
			}
		}
	}
	pass()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pass()
		}
	}
}

// Checks returns how many checks this process has run (used by tests).
func (p *Pool) Checks() int64 { return p.checks.Load() }
