package proxypool

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"quickwork-openai/internal/store"
)

func poolStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "quickwork.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestResolveModes(t *testing.T) {
	ctx := context.Background()
	st := poolStore(t)
	pool := New(st)

	if url, err := pool.Resolve(ctx, &store.Account{ID: 1, ProxyMode: store.ProxyModeDirect}, true); err != nil || url != nil {
		t.Fatalf("direct = %v, err=%v", url, err)
	}
	// Mode auto with an empty pool falls back to a direct connection.
	if url, err := pool.Resolve(ctx, &store.Account{ID: 1, ProxyMode: store.ProxyModeAuto}, true); err != nil || url != nil {
		t.Fatalf("empty pool = %v, err=%v", url, err)
	}
	if _, err := st.CreateProxy(ctx, "http://127.0.0.1:9", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A disabled pool also connects directly for auto/rotate.
	if url, err := pool.Resolve(ctx, &store.Account{ID: 1, ProxyMode: store.ProxyModeAuto}, false); err != nil || url != nil {
		t.Fatalf("disabled pool = %v, err=%v", url, err)
	}
}

func TestResolveBindAndRotation(t *testing.T) {
	ctx := context.Background()
	st := poolStore(t)
	pool := New(st)

	first, _ := st.CreateProxy(ctx, "http://127.0.0.1:1001", "")
	second, _ := st.CreateProxy(ctx, "http://127.0.0.1:1002", "")
	third, _ := st.CreateProxy(ctx, "http://127.0.0.1:1003", "")

	// bind pins the proxy and ignores the pool toggle.
	url, err := pool.Resolve(ctx, &store.Account{ID: 1, ProxyMode: store.ProxyModeBind, ProxyID: first.ID}, false)
	if err != nil || url.Host != "127.0.0.1:1001" {
		t.Fatalf("pinned = %v, err=%v", url, err)
	}
	// A disabled pinned proxy is an error, not a silent direct connection.
	if _, err := st.SetProxyEnabled(ctx, first.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := pool.Resolve(ctx, &store.Account{ID: 1, ProxyMode: store.ProxyModeBind, ProxyID: first.ID}, true); err == nil {
		t.Fatal("disabled pinned proxy must fail")
	}
	if _, err := pool.Resolve(ctx, &store.Account{ID: 1, ProxyMode: store.ProxyModeBind}, true); err == nil {
		t.Fatal("bind without a proxy id must fail")
	}

	// rotate cycles over healthy proxies.
	_ = st.SetProxyCheck(ctx, second.ID, true, 10, "", "")
	_ = st.SetProxyCheck(ctx, third.ID, true, 10, "", "")
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		url, err := pool.Resolve(ctx, &store.Account{ID: 1, ProxyMode: store.ProxyModeRotate}, true)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		seen[url.Host]++
	}
	if seen["127.0.0.1:1002"] != 2 || seen["127.0.0.1:1003"] != 2 || seen["127.0.0.1:1001"] != 0 {
		t.Fatalf("rotation = %v", seen)
	}
}

func TestResolveAutoIsStickyWithFailover(t *testing.T) {
	ctx := context.Background()
	st := poolStore(t)
	pool := New(st)

	a, _ := st.CreateProxy(ctx, "http://127.0.0.1:2001", "")
	b, _ := st.CreateProxy(ctx, "http://127.0.0.1:2002", "")
	// Both healthy: the choice depends only on the account id.
	_ = st.SetProxyCheck(ctx, a.ID, true, 10, "", "")
	_ = st.SetProxyCheck(ctx, b.ID, true, 10, "", "")
	var firstHost string
	for i := 0; i < 5; i++ {
		url, err := pool.Resolve(ctx, &store.Account{ID: 7, ProxyMode: store.ProxyModeAuto}, true)
		if err != nil {
			t.Fatalf("auto: %v", err)
		}
		if firstHost == "" {
			firstHost = url.Host
		} else if url.Host != firstHost {
			t.Fatalf("auto mode is not sticky: %s vs %s", url.Host, firstHost)
		}
	}
	// Break the preferred proxy: auto fails over to the healthy one.
	preferredID := a.ID
	if firstHost == "127.0.0.1:2002" {
		preferredID = b.ID
	}
	_ = st.SetProxyCheck(ctx, preferredID, false, 0, "", "boom")
	url, err := pool.Resolve(ctx, &store.Account{ID: 7, ProxyMode: store.ProxyModeAuto}, true)
	if err != nil {
		t.Fatalf("auto failover: %v", err)
	}
	if firstHost == "127.0.0.1:2002" && url.Host != "127.0.0.1:2001" {
		t.Fatalf("failover picked %s", url.Host)
	}
	if firstHost == "127.0.0.1:2001" && url.Host != "127.0.0.1:2002" {
		t.Fatalf("failover picked %s", url.Host)
	}
}

func TestCheckUnreachableProxy(t *testing.T) {
	result := Check(context.Background(), "http://127.0.0.1:1", "http://127.0.0.1:1/healthz", 2e9)
	if result.Healthy {
		t.Fatalf("unreachable proxy reported healthy: %+v", result)
	}
	if strings.TrimSpace(result.Error) == "" {
		t.Fatal("failure must carry an error")
	}
}

func TestExitIPParsing(t *testing.T) {
	tests := map[string]string{
		`{"ip":"203.0.113.9"}`:            "203.0.113.9",
		`{"query":"198.51.100.4"}`:        "198.51.100.4",
		`203.0.113.9`:                     "203.0.113.9",
		`not an ip`:                       "",
		`{"ip":"nope","query":"1.1.1.1"}`: "1.1.1.1",
	}
	for body, want := range tests {
		if got := exitIP([]byte(body)); got != want {
			t.Errorf("exitIP(%q) = %q, want %q", body, got, want)
		}
	}
}
