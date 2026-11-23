package store

import (
	"context"
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "quickwork.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestProxyCRUD(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	first, err := st.CreateProxy(ctx, "http://proxy-a:8080", "a")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if first.ID == 0 || !first.Enabled || first.Label != "a" {
		t.Fatalf("unexpected proxy: %+v", first)
	}
	if _, err := st.CreateProxy(ctx, "http://proxy-a:8080", ""); err == nil {
		t.Fatal("duplicate proxy must be rejected")
	}
	second, err := st.CreateProxy(ctx, "socks5://proxy-b:1080", "")
	if err != nil {
		t.Fatalf("create second: %v", err)
	}

	list, err := st.ListProxies(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d, err=%v", len(list), err)
	}

	if ok, err := st.SetProxyEnabled(ctx, first.ID, false); err != nil || !ok {
		t.Fatalf("disable: ok=%v err=%v", ok, err)
	}
	enabled, err := st.EnabledProxies(ctx)
	if err != nil || len(enabled) != 1 || enabled[0].ID != second.ID {
		t.Fatalf("enabled proxies = %+v, err=%v", enabled, err)
	}

	if err := st.SetProxyCheck(ctx, second.ID, true, 42, "203.0.113.7", ""); err != nil {
		t.Fatalf("record check: %v", err)
	}
	got, err := st.GetProxy(ctx, second.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Healthy || got.LatencyMS != 42 || got.ExitIP != "203.0.113.7" || got.LastChecked == 0 {
		t.Fatalf("check not stored: %+v", got)
	}

	if removed, err := st.DeleteProxy(ctx, first.ID); err != nil || !removed {
		t.Fatalf("delete: %v %v", removed, err)
	}
	if removed, err := st.DeleteProxy(ctx, first.ID); err != nil || removed {
		t.Fatalf("second delete: %v %v", removed, err)
	}
}

func TestAccountProxyBinding(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)

	proxy, err := st.CreateProxy(ctx, "http://proxy-a:8080", "")
	if err != nil {
		t.Fatalf("create proxy: %v", err)
	}
	account := &Account{Email: "a@example.com", TenantURL: "https://t.example", AccessToken: "tok"}
	if err := st.Save(ctx, account); err != nil {
		t.Fatalf("save account: %v", err)
	}

	if err := st.SetAccountBinding(ctx, account.ID, ProxyModeBind, proxy.ID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	got, err := st.Get(ctx, account.ID)
	if err != nil || got.ProxyID != proxy.ID || got.ProxyMode != ProxyModeBind {
		t.Fatalf("binding = %s/%d, err=%v", got.ProxyMode, got.ProxyID, err)
	}

	// Deleting the proxy must put the account back on the pool (0).
	if _, err := st.DeleteProxy(ctx, proxy.ID); err != nil {
		t.Fatalf("delete proxy: %v", err)
	}
	got, err = st.Get(ctx, account.ID)
	if err != nil || got.ProxyID != 0 || got.ProxyMode != ProxyModeAuto {
		t.Fatalf("after delete = %s/%d, err=%v", got.ProxyMode, got.ProxyID, err)
	}

	if err := st.SetAccountBinding(ctx, account.ID, ProxyModeBind, 999); err == nil {
		t.Fatal("binding a missing proxy must fail")
	}

	// Re-saving the account (e.g. re-login) must keep the binding.
	if err := st.SetAccountBinding(ctx, account.ID, ProxyModeDirect, 0); err != nil {
		t.Fatalf("direct: %v", err)
	}
	account.ProxyID = 0
	if err := st.Save(ctx, account); err != nil {
		t.Fatalf("re-save: %v", err)
	}
	got, _ = st.Get(ctx, account.ID)
	if got.ProxyMode != ProxyModeDirect || got.ProxyID != 0 {
		t.Fatalf("re-save reset the binding to %s/%d", got.ProxyMode, got.ProxyID)
	}
}
