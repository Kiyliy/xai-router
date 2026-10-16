package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Token is a client (downstream) API token.
type Token struct {
	ID            int64
	Name          string
	Hash          string
	Prefix        string
	Enabled       bool
	QuotaTicks    int64 // 0 = unlimited
	UsedTicks     int64
	ExpiresAt     int64 // unix ms, 0 = never
	AllowedModels []string
	Requests      int64
	LastUsedAt    int64
	Notes         string
	CreatedAt     int64
	UpdatedAt     int64
}

const tokenCols = `id, name, token_hash, prefix, enabled, quota_ticks, used_ticks, expires_at, allowed_models,
requests, last_used_at, notes, created_at, updated_at`

func scanToken(s interface{ Scan(...any) error }) (*Token, error) {
	var t Token
	var en int
	var models string
	err := s.Scan(&t.ID, &t.Name, &t.Hash, &t.Prefix, &en, &t.QuotaTicks, &t.UsedTicks, &t.ExpiresAt, &models,
		&t.Requests, &t.LastUsedAt, &t.Notes, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.Enabled = en == 1
	_ = json.Unmarshal([]byte(models), &t.AllowedModels)
	if t.AllowedModels == nil {
		t.AllowedModels = []string{}
	}
	return &t, nil
}

// GetTokenByHash looks up a token by the SHA-256 hash of its secret.
func (db *DB) GetTokenByHash(ctx context.Context, hash string) (*Token, error) {
	return scanToken(db.R.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE token_hash = ?`, hash))
}

// GetToken returns one token.
func (db *DB) GetToken(ctx context.Context, id int64) (*Token, error) {
	return scanToken(db.R.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE id = ?`, id))
}

// ListTokens returns a page of tokens.
func (db *DB) ListTokens(ctx context.Context, q string, limit, offset int) ([]*Token, int, error) {
	w, args := "", []any{}
	if q != "" {
		w = ` WHERE name LIKE ? OR prefix LIKE ? OR notes LIKE ?`
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	var total int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM tokens`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.R.QueryContext(ctx, `SELECT `+tokenCols+` FROM tokens`+w+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// CreateToken inserts t.
func (db *DB) CreateToken(ctx context.Context, t *Token) error {
	now := NowMs()
	t.CreatedAt, t.UpdatedAt = now, now
	models, _ := json.Marshal(nonNil(t.AllowedModels))
	res, err := db.W.ExecContext(ctx, `INSERT INTO tokens (name, token_hash, prefix, enabled, quota_ticks, expires_at,
allowed_models, notes, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.Hash, t.Prefix, boolInt(t.Enabled), t.QuotaTicks, t.ExpiresAt, string(models), t.Notes, now, now)
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

// UpdateToken saves the editable fields of t (including hash/prefix). Usage
// counters are owned by the log writer and are not touched here.
func (db *DB) UpdateToken(ctx context.Context, t *Token) error {
	t.UpdatedAt = NowMs()
	models, _ := json.Marshal(nonNil(t.AllowedModels))
	res, err := db.W.ExecContext(ctx, `UPDATE tokens SET name=?, token_hash=?, prefix=?, enabled=?, quota_ticks=?,
expires_at=?, allowed_models=?, notes=?, updated_at=? WHERE id=?`,
		t.Name, t.Hash, t.Prefix, boolInt(t.Enabled), t.QuotaTicks, t.ExpiresAt, string(models), t.Notes,
		t.UpdatedAt, t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ResetTokenUsage sets a token's spend back to zero.
func (db *DB) ResetTokenUsage(ctx context.Context, id int64) error {
	_, err := db.W.ExecContext(ctx, `UPDATE tokens SET used_ticks = 0, updated_at = ? WHERE id = ?`, NowMs(), id)
	return err
}

// DeleteToken removes a token.
func (db *DB) DeleteToken(ctx context.Context, id int64) error {
	res, err := db.W.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TokenCounts returns total and enabled token counts.
func (db *DB) TokenCounts(ctx context.Context) (total, enabled int, err error) {
	err = db.R.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(enabled),0) FROM tokens`).Scan(&total, &enabled)
	return
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// AdminToken is a long-lived credential for the admin API.
type AdminToken struct {
	ID         int64
	Name       string
	Hash       string
	Prefix     string
	LastUsedAt int64
	CreatedAt  int64
}

// CreateAdminToken inserts t.
func (db *DB) CreateAdminToken(ctx context.Context, t *AdminToken) error {
	t.CreatedAt = NowMs()
	res, err := db.W.ExecContext(ctx, `INSERT INTO admin_tokens (name, token_hash, prefix, created_at) VALUES (?,?,?,?)`,
		t.Name, t.Hash, t.Prefix, t.CreatedAt)
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

// ListAdminTokens returns all admin tokens.
func (db *DB) ListAdminTokens(ctx context.Context) ([]*AdminToken, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT id, name, token_hash, prefix, last_used_at, created_at FROM admin_tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AdminToken
	for rows.Next() {
		var t AdminToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Hash, &t.Prefix, &t.LastUsedAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

// GetAdminTokenByHash looks up an admin token and bumps last_used_at.
func (db *DB) GetAdminTokenByHash(ctx context.Context, hash string) (*AdminToken, error) {
	var t AdminToken
	err := db.R.QueryRowContext(ctx, `SELECT id, name, token_hash, prefix, last_used_at, created_at FROM admin_tokens WHERE token_hash = ?`, hash).
		Scan(&t.ID, &t.Name, &t.Hash, &t.Prefix, &t.LastUsedAt, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if now := NowMs(); now-t.LastUsedAt > 60_000 {
		_, _ = db.W.ExecContext(ctx, `UPDATE admin_tokens SET last_used_at=? WHERE id=?`, now, t.ID)
	}
	return &t, nil
}

// DeleteAdminToken revokes an admin token.
func (db *DB) DeleteAdminToken(ctx context.Context, id int64) error {
	res, err := db.W.ExecContext(ctx, `DELETE FROM admin_tokens WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// CreateSession stores a login session.
func (db *DB) CreateSession(ctx context.Context, hash string, expiresAt int64) error {
	now := NowMs()
	_, err := db.W.ExecContext(ctx, `INSERT INTO sessions (token_hash, created_at, expires_at, last_seen_at) VALUES (?,?,?,?)`,
		hash, now, expiresAt, now)
	return err
}

// GetSession returns the expiry of a valid session, sliding it forward when
// it is more than a day old.
func (db *DB) GetSession(ctx context.Context, hash string, ttlMs int64) (int64, error) {
	var exp, seen int64
	err := db.R.QueryRowContext(ctx, `SELECT expires_at, last_seen_at FROM sessions WHERE token_hash = ?`, hash).Scan(&exp, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	now := NowMs()
	if exp < now {
		_, _ = db.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash)
		return 0, ErrNotFound
	}
	if now-seen > 3600_000 {
		exp = now + ttlMs
		_, _ = db.W.ExecContext(ctx, `UPDATE sessions SET last_seen_at=?, expires_at=? WHERE token_hash=?`, now, exp, hash)
	}
	return exp, nil
}

// DeleteSession removes one session.
func (db *DB) DeleteSession(ctx context.Context, hash string) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash)
	return err
}

// DeleteSessionsExcept removes all sessions but one (used on password change).
func (db *DB) DeleteSessionsExcept(ctx context.Context, keep string) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash <> ?`, keep)
	return err
}

// PurgeExpiredSessions deletes expired sessions.
func (db *DB) PurgeExpiredSessions(ctx context.Context) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, NowMs())
	return err
}
