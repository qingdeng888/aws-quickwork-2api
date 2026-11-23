package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Proxy is one outbound proxy in the pool. The URL may carry credentials
// (http://user:pass@host:port); API responses redact them.
type Proxy struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	Label       string `json:"label"`
	Enabled     bool   `json:"enabled"`
	Healthy     bool   `json:"healthy"`
	LatencyMS   int    `json:"latency_ms"`
	ExitIP      string `json:"exit_ip"`
	LastError   string `json:"last_error"`
	LastChecked int64  `json:"last_checked"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

const proxyColumns = `id, url, label, enabled, healthy, latency_ms, exit_ip,
	last_error, last_checked, created_at, updated_at`

func scanProxy(row scanner) (*Proxy, error) {
	var p Proxy
	var enabled, healthy int
	err := row.Scan(&p.ID, &p.URL, &p.Label, &enabled, &healthy, &p.LatencyMS,
		&p.ExitIP, &p.LastError, &p.LastChecked, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	p.Enabled = enabled != 0
	p.Healthy = healthy != 0
	return &p, nil
}

// ListProxies returns every proxy ordered by id.
func (s *Store) ListProxies(ctx context.Context) ([]Proxy, error) {
	return s.queryProxies(ctx, `SELECT `+proxyColumns+` FROM proxies ORDER BY id`)
}

// EnabledProxies returns the proxies eligible for pool rotation.
func (s *Store) EnabledProxies(ctx context.Context) ([]Proxy, error) {
	return s.queryProxies(ctx, `SELECT `+proxyColumns+` FROM proxies WHERE enabled=1 ORDER BY id`)
}

func (s *Store) queryProxies(ctx context.Context, q string, args ...any) ([]Proxy, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Proxy, 0, 8)
	for rows.Next() {
		p, err := scanProxy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// GetProxy returns one proxy, or sql.ErrNoRows when absent.
func (s *Store) GetProxy(ctx context.Context, id int64) (*Proxy, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+proxyColumns+` FROM proxies WHERE id=?`, id)
	return scanProxy(row)
}

// CreateProxy adds one proxy. Adding the same URL twice is rejected with a
// readable error instead of a raw SQLite constraint message.
func (s *Store) CreateProxy(ctx context.Context, url, label string) (*Proxy, error) {
	url = strings.TrimSpace(url)
	label = strings.TrimSpace(label)
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO proxies (url, label, enabled, created_at, updated_at) VALUES (?,?,?,?,?)`,
		url, label, 1, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("proxy %q is already in the pool", url)
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetProxy(ctx, id)
}

// SetProxyEnabled turns a proxy on or off. Disabled proxies stay listed but
// are skipped by pool rotation and the health sweep.
func (s *Store) SetProxyEnabled(ctx context.Context, id int64, enabled bool) (bool, error) {
	value := 0
	if enabled {
		value = 1
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE proxies SET enabled=?, updated_at=? WHERE id=?`,
		value, time.Now().Unix(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteProxy removes a proxy and unbinds any account that pointed at it.
func (s *Store) DeleteProxy(ctx context.Context, id int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET proxy_id=0 WHERE proxy_id=?`, id); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM proxies WHERE id=?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n > 0, nil
}

// SetProxyCheck records the outcome of a health check.
func (s *Store) SetProxyCheck(ctx context.Context, id int64, healthy bool, latencyMS int, exitIP, lastError string) error {
	value := 0
	if healthy {
		value = 1
	}
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, `
UPDATE proxies SET healthy=?, latency_ms=?, exit_ip=?, last_error=?, last_checked=?, updated_at=?
WHERE id=?`, value, latencyMS, truncate(exitIP, 64), truncate(lastError, 300), now, now, id)
	return err
}

// SetAccountProxy binds an account to one proxy. proxyID 0 means "follow the
// pool" and -1 means "always connect directly".
func (s *Store) SetAccountProxy(ctx context.Context, accountID, proxyID int64) error {
	if proxyID > 0 {
		if _, err := s.GetProxy(ctx, proxyID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("proxy %d not found", proxyID)
			}
			return err
		}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET proxy_id=?, updated_at=? WHERE id=?`,
		proxyID, time.Now().Unix(), accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
