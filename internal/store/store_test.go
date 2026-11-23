package store

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenModeAndDisabledKeptOnResave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "qw.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Fatalf("db mode = %o, want 600", mode)
		}
	}

	ctx := t.Context()
	a := &Account{Email: "a@example.com", TenantURL: "https://t.example", AccessToken: "x"}
	if err := s.Save(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled(ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	again := &Account{Email: "a@example.com", TenantURL: "https://t.example", AccessToken: "y"}
	if err := s.Save(ctx, again); err != nil {
		t.Fatal(err)
	}
	if again.ID != a.ID {
		t.Fatalf("re-save created new id %d, want %d", again.ID, a.ID)
	}
	got, err := s.Get(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled {
		t.Fatal("disabled flag lost on re-save")
	}
	if got.AccessToken != "y" {
		t.Fatalf("access token not updated: %q", got.AccessToken)
	}
}
