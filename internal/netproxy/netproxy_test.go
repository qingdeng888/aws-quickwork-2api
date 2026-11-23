package netproxy

import (
	"context"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "http://1.2.3.4:8080", want: "http://1.2.3.4:8080"},
		{in: "1.2.3.4:8080", want: "http://1.2.3.4:8080"},
		{in: "socks5://user:pass@proxy.example:1080", want: "socks5://user:pass@proxy.example:1080"},
		{in: "https://proxy.example", want: "https://proxy.example:443"},
		{in: "http://proxy.example", want: "http://proxy.example:80"},
		{in: "ftp://proxy.example:21", wantErr: true},
		{in: "", wantErr: true},
		{in: "http://", wantErr: true},
	}
	for _, tc := range tests {
		got, err := Parse(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) expected an error, got %v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): %v", tc.in, err)
			continue
		}
		if got.String() != tc.want {
			t.Errorf("Parse(%q) = %q, want %q", tc.in, got.String(), tc.want)
		}
	}
}

func TestRedact(t *testing.T) {
	if got := Redact("http://user:secret@1.2.3.4:8080"); got != "http://1.2.3.4:8080" {
		t.Errorf("Redact with credentials = %q", got)
	}
	if got := Redact("socks5://1.2.3.4:1080"); got != "socks5://1.2.3.4:1080" {
		t.Errorf("Redact without credentials = %q", got)
	}
}

func TestContextProxy(t *testing.T) {
	proxy, _ := Parse("http://1.2.3.4:8080")
	ctx := WithProxy(context.Background(), proxy)
	if FromContext(ctx) == nil || FromContext(ctx).Host != "1.2.3.4:8080" {
		t.Fatal("proxy not carried by context")
	}
	if FromContext(WithProxy(context.Background(), nil)) != nil {
		t.Fatal("nil proxy should mean direct")
	}
	if ProxyFunc(nil) != nil {
		t.Fatal("nil proxy must not install a proxy function")
	}
}
