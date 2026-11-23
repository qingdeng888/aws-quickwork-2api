package proxy

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCBORRejectsHugeLength(t *testing.T) {
	cases := [][]byte{
		{0x5b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // byte string, len 2^64-1
		{0x7b, 0x7f, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // text string
		{0x9b, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00}, // array 2^32
		{0xbb, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, // map
		{0x5a, 0xff, 0xff, 0xff, 0xff, 0x00},                   // byte string, len 2^32-1
	}
	for i, c := range cases {
		if _, err := cborDecode(c); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestCBORRejectsDeepNesting(t *testing.T) {
	data := append(bytes.Repeat([]byte{0x81}, 10000), 0x00)
	if _, err := cborDecode(data); err == nil {
		t.Fatal("expected depth error")
	}
	ok := append(bytes.Repeat([]byte{0x81}, 10), 0x00)
	if _, err := cborDecode(ok); err != nil {
		t.Fatalf("shallow nesting rejected: %v", err)
	}
}

func TestConversationKeyDiffersPerAPIKey(t *testing.T) {
	req := &ChatRequest{}
	r1 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r1.Header.Set("Authorization", "Bearer key-a")
	r1.Header.Set("X-Conversation-Id", "conv1")
	r2 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r2.Header.Set("Authorization", "Bearer key-b")
	r2.Header.Set("X-Conversation-Id", "conv1")
	r3 := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r3.Header.Set("X-Api-Key", "key-a")
	r3.Header.Set("X-Thread-Id", "conv1")
	k1 := conversationKey(r1, req, "")
	k2 := conversationKey(r2, req, "")
	k3 := conversationKey(r3, req, "")
	if k1 == k2 {
		t.Fatalf("keys collide across API keys: %q", k1)
	}
	if k1 != k3 {
		t.Fatalf("same API key via different header should match: %q vs %q", k1, k3)
	}
	if !strings.HasPrefix(k1, "h:") || !strings.HasSuffix(k1, ":conv1") {
		t.Fatalf("unexpected key shape %q", k1)
	}
}

func TestTruncationUTF8(t *testing.T) {
	s := strings.Repeat("中文字符😀", 200)
	for n := 1; n < 60; n++ {
		if out := truncateText(s, n); !utf8.ValidString(out) {
			t.Fatalf("truncateText(%d) invalid UTF-8: %q", n, out)
		}
	}
	if out := titleFor(s); !utf8.ValidString(out) {
		t.Fatalf("titleFor invalid UTF-8: %q", out)
	}
	if out := roleLabel(s); !utf8.ValidString(out) {
		t.Fatalf("roleLabel invalid UTF-8: %q", out)
	}
}
