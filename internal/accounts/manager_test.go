package accounts

import (
	"testing"
	"time"

	"quickwork-openai/internal/store"
)

func TestRecentlyRefreshed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tests := []struct {
		name string
		acct *store.Account
		want bool
	}{
		{name: "nil account", acct: nil, want: false},
		{
			name: "fresh with token",
			acct: &store.Account{ObtainedAt: now.Add(-10 * time.Second).Unix(), AccessToken: "tok"},
			want: true,
		},
		{
			name: "older than the window",
			acct: &store.Account{ObtainedAt: now.Add(-time.Minute).Unix(), AccessToken: "tok"},
			want: false,
		},
		{
			name: "with a recorded error",
			acct: &store.Account{ObtainedAt: now.Add(-10 * time.Second).Unix(), AccessToken: "tok", LastError: "boom"},
			want: false,
		},
		{
			name: "no token",
			acct: &store.Account{ObtainedAt: now.Add(-10 * time.Second).Unix()},
			want: false,
		},
		{
			name: "never obtained",
			acct: &store.Account{AccessToken: "tok"},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := recentlyRefreshed(tc.acct, now); got != tc.want {
				t.Errorf("recentlyRefreshed = %v, want %v", got, tc.want)
			}
		})
	}
}
