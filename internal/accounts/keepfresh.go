package accounts

import (
	"context"
	"time"

	"quickwork-openai/internal/oauth"
)

// refreshRetryAfter is how long a failed scheduled refresh waits before it is
// attempted again. A revoked refresh token stays broken until a new login, so
// backing off keeps the logs and the token endpoint quiet.
const refreshRetryAfter = 15 * time.Minute

// RefreshInterval picks how often KeepFresh looks for accounts that entered
// the refresh window: half of the window, clamped to [15s, 1m]. A 10m window
// therefore refreshes within a minute of becoming due.
func RefreshInterval(window time.Duration) time.Duration {
	interval := window / 2
	if interval > time.Minute {
		return time.Minute
	}
	if interval < 15*time.Second {
		return 15 * time.Second
	}
	return interval
}

// KeepFresh refreshes accounts that enter the refresh window without waiting
// for a chat request to select them. Request-driven refresh (Manager.Next)
// stays in place; this only makes sure an idle rotation does not let tokens
// expire, and that the expiry shown in the management UI recovers by itself.
func (m *Manager) KeepFresh(ctx context.Context, interval time.Duration, logf func(format string, args ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if interval <= 0 {
		interval = time.Minute
	}
	failedAt := make(map[int64]time.Time)

	pass := func() {
		list, err := m.store.Enabled(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logf("account refresh sweep failed: %v", err)
			}
			return
		}
		now := time.Now()
		window := m.cfg.RefreshBefore.Duration()
		for i := range list {
			account := &list[i]
			if account.RefreshToken == "" || !account.Expiring(now, window) {
				continue
			}
			if last, ok := failedAt[account.ID]; ok && now.Sub(last) < refreshRetryAfter {
				continue
			}
			if err := m.refreshIfDue(ctx, account.ID); err != nil {
				if ctx.Err() != nil {
					return
				}
				failedAt[account.ID] = time.Now()
				_ = m.store.SetError(ctx, account.ID, err.Error())
				logf("scheduled refresh account=%d failed: %v", account.ID, err)
				continue
			}
			delete(failedAt, account.ID)
			if refreshed, err := m.store.Get(ctx, account.ID); err == nil && refreshed.ExpiresAt > 0 {
				logf("scheduled refresh account=%d ok expires_in=%s",
					account.ID, time.Until(time.Unix(refreshed.ExpiresAt, 0)).Round(time.Second))
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

// refreshIfDue refreshes one account only if it is still inside the refresh
// window once the account lock is held, so a concurrent request (or another
// sweep) cannot trigger a second refresh for the same account.
func (m *Manager) refreshIfDue(ctx context.Context, id int64) error {
	lock := m.lockFor(id)
	lock.Lock()
	defer lock.Unlock()

	account, err := m.store.Get(ctx, id)
	if err != nil {
		return err
	}
	if account.RefreshToken == "" {
		return oauth.ErrNoRefreshToken
	}
	if !account.Expiring(time.Now(), m.cfg.RefreshBefore.Duration()) {
		return nil
	}
	_, err = m.refreshLocked(ctx, account)
	return err
}
