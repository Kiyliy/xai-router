package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Key is an upstream xAI API key.
type Key struct {
	ID             int64
	Name           string
	Key            string
	BaseURL        string
	Priority       int
	Weight         int
	Enabled        bool
	AutoDisabled   bool
	StatusReason   string
	CooldownUntil  int64 // unix ms
	MaxConcurrency int
	RPMLimit       int
	Requests       int64
	Errors         int64
	CostTicks      int64
	LastUsedAt     int64
	LastError      string
	LastTest       string // JSON
	LastTestedAt   int64
	Notes          string
	CreatedAt      int64
	UpdatedAt      int64
}

const keyCols = `id, name, key, base_url, priority, weight, enabled, auto_disabled, status_reason,
cooldown_until, max_concurrency, rpm_limit, requests, errors, cost_ticks, last_used_at, last_error,
last_test, last_tested_at, notes, created_at, updated_at`

func scanKey(s interface{ Scan(...any) error }) (*Key, error) {
	var k Key
	var en, ad int
	err := s.Scan(&k.ID, &k.Name, &k.Key, &k.BaseURL, &k.Priority, &k.Weight, &en, &ad, &k.StatusReason,
		&k.CooldownUntil, &k.MaxConcurrency, &k.RPMLimit, &k.Requests, &k.Errors, &k.CostTicks, &k.LastUsedAt,
		&k.LastError, &k.LastTest, &k.LastTestedAt, &k.Notes, &k.CreatedAt, &k.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	k.Enabled, k.AutoDisabled = en == 1, ad == 1
	return &k, nil
}

// ListAllKeys returns every key ordered by priority then id.
func (db *DB) ListAllKeys(ctx context.Context) ([]*Key, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+keyCols+` FROM keys ORDER BY priority, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetKey returns one key.
func (db *DB) GetKey(ctx context.Context, id int64) (*Key, error) {
	return scanKey(db.R.QueryRowContext(ctx, `SELECT `+keyCols+` FROM keys WHERE id = ?`, id))
}

// CreateKey inserts k and sets its ID.
func (db *DB) CreateKey(ctx context.Context, k *Key) error {
	now := NowMs()
	k.CreatedAt, k.UpdatedAt = now, now
	res, err := db.W.ExecContext(ctx, `INSERT INTO keys (name, key, base_url, priority, weight, enabled,
max_concurrency, rpm_limit, notes, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		k.Name, k.Key, k.BaseURL, k.Priority, k.Weight, boolInt(k.Enabled), k.MaxConcurrency, k.RPMLimit,
		k.Notes, now, now)
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	k.ID, _ = res.LastInsertId()
	return nil
}

// UpdateKey saves the admin-editable fields of k. Runtime state
// (auto-disable, cooldown) is owned by the pool and written with SetKeyState,
// so an edit cannot undo a concurrent automatic change.
func (db *DB) UpdateKey(ctx context.Context, k *Key) error {
	k.UpdatedAt = NowMs()
	res, err := db.W.ExecContext(ctx, `UPDATE keys SET name=?, key=?, base_url=?, priority=?, weight=?, enabled=?,
max_concurrency=?, rpm_limit=?, notes=?, updated_at=? WHERE id=?`,
		k.Name, k.Key, k.BaseURL, k.Priority, k.Weight, boolInt(k.Enabled), k.MaxConcurrency, k.RPMLimit, k.Notes,
		k.UpdatedAt, k.ID)
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteKey removes a key. Logs keep the key name.
func (db *DB) DeleteKey(ctx context.Context, id int64) error {
	res, err := db.W.ExecContext(ctx, `DELETE FROM keys WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetKeyState persists pool state changes (auto-disable, cooldown).
func (db *DB) SetKeyState(ctx context.Context, id int64, autoDisabled bool, reason string, cooldownUntil int64) error {
	_, err := db.W.ExecContext(ctx, `UPDATE keys SET auto_disabled=?, status_reason=?, cooldown_until=?, updated_at=? WHERE id=?`,
		boolInt(autoDisabled), reason, cooldownUntil, NowMs(), id)
	return err
}

// SetKeyTest stores the latest health-check result.
func (db *DB) SetKeyTest(ctx context.Context, id int64, resultJSON string) error {
	now := NowMs()
	_, err := db.W.ExecContext(ctx, `UPDATE keys SET last_test=?, last_tested_at=? WHERE id=?`, resultJSON, now, id)
	return err
}

// KeyFilter narrows ListKeys.
type KeyFilter struct {
	Q      string
	Status string // active, cooldown, disabled
	Limit  int
	Offset int
}

// ListKeys returns a page of keys and the total count. Status filtering
// needs the current time because cooldown is time-based.
func (db *DB) ListKeys(ctx context.Context, f KeyFilter) ([]*Key, int, error) {
	var where []string
	var args []any
	if f.Q != "" {
		where = append(where, `(name LIKE ? OR key LIKE ? OR notes LIKE ?)`)
		like := "%" + f.Q + "%"
		args = append(args, like, "%"+f.Q, like)
	}
	now := NowMs()
	switch f.Status {
	case "disabled":
		where = append(where, `(enabled = 0 OR auto_disabled = 1)`)
	case "cooldown":
		where = append(where, `(enabled = 1 AND auto_disabled = 0 AND cooldown_until > ?)`)
		args = append(args, now)
	case "active":
		where = append(where, `(enabled = 1 AND auto_disabled = 0 AND cooldown_until <= ?)`)
		args = append(args, now)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM keys`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.R.QueryContext(ctx, `SELECT `+keyCols+` FROM keys`+w+` ORDER BY priority, id LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, k)
	}
	return out, total, rows.Err()
}

// KeyCounts summarises key states.
type KeyCounts struct{ Total, Active, Cooldown, Disabled int }

// CountKeys returns key state counts.
func (db *DB) CountKeys(ctx context.Context) (KeyCounts, error) {
	var c KeyCounts
	now := NowMs()
	err := db.R.QueryRowContext(ctx, `SELECT COUNT(*),
COALESCE(SUM(CASE WHEN enabled=1 AND auto_disabled=0 AND cooldown_until<=? THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN enabled=1 AND auto_disabled=0 AND cooldown_until>? THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN enabled=0 OR auto_disabled=1 THEN 1 ELSE 0 END),0) FROM keys`, now, now).
		Scan(&c.Total, &c.Active, &c.Cooldown, &c.Disabled)
	return c, err
}
