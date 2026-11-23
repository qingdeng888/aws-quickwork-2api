package accounts

import (
	"testing"
	"time"
)

func TestRefreshInterval(t *testing.T) {
	cases := []struct {
		window time.Duration
		want   time.Duration
	}{
		{10 * time.Minute, time.Minute},
		{2 * time.Hour, time.Minute},
		{2 * time.Minute, time.Minute},
		{90 * time.Second, 45 * time.Second},
		{30 * time.Second, 15 * time.Second},
		{10 * time.Second, 15 * time.Second},
		{0, 15 * time.Second},
	}
	for _, tc := range cases {
		if got := RefreshInterval(tc.window); got != tc.want {
			t.Errorf("RefreshInterval(%s) = %s, want %s", tc.window, got, tc.want)
		}
	}
}
