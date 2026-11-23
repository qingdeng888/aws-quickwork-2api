package proxy

import "testing"

func TestAPIKeyAuthKeysAndCount(t *testing.T) {
	auth := NewAPIKeyAuth([]string{" sk-a ", "sk-b", "sk-a", ""})
	if auth.Count() != 2 {
		t.Fatalf("count = %d, want 2", auth.Count())
	}
	keys := auth.Keys()
	if len(keys) != 2 || keys[0] != "sk-a" || keys[1] != "sk-b" {
		t.Fatalf("keys = %v", keys)
	}
	// Keys returns a copy: mutating it must not change the auth set.
	keys[0] = "tampered"
	if got := auth.Keys()[0]; got != "sk-a" {
		t.Fatalf("Keys() leaked internal state: %q", got)
	}

	empty := NewAPIKeyAuth(nil)
	if empty.Count() != 0 || len(empty.Keys()) != 0 {
		t.Fatalf("empty auth = %d keys", empty.Count())
	}
}
