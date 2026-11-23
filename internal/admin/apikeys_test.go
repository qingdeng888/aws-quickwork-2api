package admin

import (
	"strings"
	"testing"
)

func TestGenerateAPIKey(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		key, err := generateAPIKey()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !strings.HasPrefix(key, "sk-") || len(key) != 35 {
			t.Fatalf("key shape = %q", key)
		}
		if seen[key] {
			t.Fatalf("duplicate generated key %q", key)
		}
		seen[key] = true
	}
}
