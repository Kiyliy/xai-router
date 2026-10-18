package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Token is a client (downstream) API token.
type Token struct {
	ID            int64
	Name          string
	Hash          string
	Prefix        string
	Enabled       bool
	QuotaTicks    int64 // when Limited: requests are refused once UsedTicks reaches it (0 blocks)
	Limited       bool  // false = unlimited (stored as the column unlimited)
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
requests, last_used_at, notes, created_at, updated_at, unlimited`

func scanToken(s interface{ Scan(...any) error }) (*Token, error) {
	var t Token
	var en, unl int
	var models string
	err := s.Scan(&t.ID, &t.Name, &t.Hash, &t.Prefix, &en, &t.QuotaTicks, &t.UsedTicks, &t.ExpiresAt, &models,
		&t.Requests, &t.LastUsedAt, &t.Notes, &t.CreatedAt, &t.UpdatedAt, &unl)
	t.Limited = unl == 0
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
//
// Every proxied request authenticates, so lookups are served from memory
// for up to tokenCacheTTL: the request path then does not wait on SQLite
// while a large purge or vacuum keeps the database busy. Any change made
// through this package (create, update, regenerate, reset, delete) clears
// the cache, so disabling or deleting a token takes effect at once. Usage
// (used_ticks) may be up to tokenCacheTTL old; quotas are enforced by the
// in-memory ledger, which never goes below the stored value.
func (db *DB) GetTokenByHash(ctx context.Context, hash string) (*Token, error) {
	if t := db.toks.get(hash); t != nil {
		return t, nil
	}
	gen := db.toks.generation()
	t, err := scanToken(db.R.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE token_hash = ?`, hash))
	if err == nil {
		db.toks.put(hash, t, gen)
	}
	return t, err
}

const tokenCacheTTL = 5 * time.Second

type tokenCache struct {
	mu  sync.Mutex
	gen uint64
	m   map[string]cachedToken
}

type cachedToken struct {
	t   Token
	exp time.Time
}

func (c *tokenCache) get(hash string) *Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[hash]
	if !ok || time.Now().After(e.exp) {
		return nil
	}
	cp := e.t
	return &cp
}

func (c *tokenCache) generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// put stores t unless the cache was cleared since the lookup started (a
// change racing with the read must not be overwritten by the old row).
func (c *tokenCache) put(hash string, t *Token, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return
	}
	if c.m == nil || len(c.m) > 10000 {
		c.m = map[string]cachedToken{}
	}
	c.m[hash] = cachedToken{t: *t, exp: time.Now().Add(tokenCacheTTL)}
}

func (c *tokenCache) clear() {
	c.mu.Lock()
	c.gen++
	c.m = nil
	c.mu.Unlock()
}

// GetToken returns one token.
func (db *DB) GetToken(ctx context.Context, id int64) (*Token, error) {
	return scanToken(db.R.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE id = ? AND deleted_at = 0`, id))
}

// ListTokens returns a page of tokens.
func (db *DB) ListTokens(ctx context.Context, q string, limit, offset int) ([]*Token, int, error) {
	return db.ListTokensByStatus(ctx, q, "", limit, offset)
}

// Token list status filters (see ListTokensByStatus).
var tokenStatusSQL = map[string]string{
	// usable: enabled, not expired and not out of quota
	"enabled":   `enabled = 1 AND (expires_at = 0 OR expires_at > ?) AND (unlimited = 1 OR used_ticks < quota_ticks)`,
	"disabled":  `enabled = 0`,
	"expired":   `expires_at > 0 AND expires_at <= ?`,
	"exhausted": `unlimited = 0 AND used_ticks >= quota_ticks`,
}

// ValidTokenStatus reports whether s is a known token status filter.
func ValidTokenStatus(s string) bool { _, ok := tokenStatusSQL[s]; return ok || s == "" }

// ListTokensByStatus returns a page of tokens matching q and status
// ("", enabled, disabled, expired or exhausted).
func (db *DB) ListTokensByStatus(ctx context.Context, q, status string, limit, offset int) ([]*Token, int, error) {
	conds := []string{`deleted_at = 0`}
	args := []any{}
	if q != "" {
		conds = append(conds, `(name LIKE ? OR prefix LIKE ? OR notes LIKE ?)`)
		like := "%" + q + "%"
		args = append(args, like, like, like)
	}
	if c, ok := tokenStatusSQL[status]; ok {
		conds = append(conds, "("+c+")")
		if strings.Contains(c, "?") {
			args = append(args, NowMs())
		}
	}
	w := " WHERE " + strings.Join(conds, " AND ")
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
	return db.CreateTokenRecorded(ctx, t, nil)
}

// CreateTokenRecorded inserts t and, in the same transaction, the event e
// (the token's first quota record: initial quota or unlimited), filling in
// the target and the amounts.
func (db *DB) CreateTokenRecorded(ctx context.Context, t *Token, e *Event) error {
	defer db.toks.clear()
	if t.QuotaTicks > 0 {
		t.Limited = true // a positive quota is a limit (callers predating the Limited flag)
	}
	now := NowMs()
	t.CreatedAt, t.UpdatedAt = now, now
	models, _ := json.Marshal(nonNil(t.AllowedModels))
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	res, err := tx.ExecContext(ctx, `INSERT INTO tokens (name, token_hash, prefix, enabled, quota_ticks, unlimited,
expires_at, allowed_models, notes, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		t.Name, t.Hash, t.Prefix, boolInt(t.Enabled), t.QuotaTicks, boolInt(!t.Limited), t.ExpiresAt, string(models),
		t.Notes, now, now)
	if isUnique(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	t.ID, _ = res.LastInsertId()
	if e != nil {
		e.TargetType, e.TargetID, e.TargetName, e.TargetHint = "token", strconv.FormatInt(t.ID, 10), t.Name, t.Prefix
		if e.Detail == nil {
			e.Detail = map[string]any{}
		}
		d := e.Detail
		d["amount_ticks"], d["before_ticks"], d["after_ticks"] = t.QuotaTicks, int64(0), t.QuotaTicks
		d["used_ticks"], d["unlimited"] = int64(0), !t.Limited
		if err := insertEvent(ctx, tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// UpdateToken saves the editable fields of t (including hash/prefix). Usage
// counters are owned by the log writer and are not touched here.
func (db *DB) UpdateToken(ctx context.Context, t *Token) error {
	defer db.toks.clear()
	t.UpdatedAt = NowMs()
	models, _ := json.Marshal(nonNil(t.AllowedModels))
	// The quota is not written here: it only changes through AdjustTokenQuota.
	res, err := db.W.ExecContext(ctx, `UPDATE tokens SET name=?, token_hash=?, prefix=?, enabled=?,
expires_at=?, allowed_models=?, notes=?, updated_at=? WHERE id=? AND deleted_at = 0`,
		t.Name, t.Hash, t.Prefix, boolInt(t.Enabled), t.ExpiresAt, string(models), t.Notes,
		t.UpdatedAt, t.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteToken soft-deletes a token: it stops authenticating at once (the
// hash is replaced) and leaves every list, but its name, prefix and spend
// stay for the history.
func (db *DB) DeleteToken(ctx context.Context, id int64) error {
	defer db.toks.clear()
	now := NowMs()
	res, err := db.W.ExecContext(ctx, `UPDATE tokens SET deleted_at = ?, enabled = 0, token_hash = 'deleted:' || id,
updated_at = ? WHERE id = ? AND deleted_at = 0`, now, now, id)
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
	err = db.R.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(enabled),0) FROM tokens WHERE deleted_at = 0`).Scan(&total, &enabled)
	return
}

// TokenLabels returns every token's name and prefix, including deleted
// tokens (logs and statistics outlive them).
func (db *DB) TokenLabels(ctx context.Context) (map[int64]Label, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT id, name, prefix, deleted_at FROM tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]Label{}
	for rows.Next() {
		var id, del int64
		var l Label
		if err := rows.Scan(&id, &l.Name, &l.Hint, &del); err != nil {
			return nil, err
		}
		l.Deleted = del > 0
		out[id] = l
	}
	return out, rows.Err()
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

// DeleteAllAdminTokens revokes every admin API token.
func (db *DB) DeleteAllAdminTokens(ctx context.Context) error {
	_, err := db.W.ExecContext(ctx, `DELETE FROM admin_tokens`)
	return err
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

// Quota adjustment actions.
const (
	QuotaIncrease     = "increase"
	QuotaDecrease     = "decrease"
	QuotaSetLimited   = "set_limited"   // amount = balance left to spend: quota = used + amount
	QuotaSetUnlimited = "set_unlimited" // the stored quota is kept (and ignored while unlimited)
)

// ErrQuotaInvalid is an adjustment the rules forbid; the text says why.
type ErrQuotaInvalid struct{ Reason string }

func (e ErrQuotaInvalid) Error() string { return e.Reason }

// QuotaChange is the result of AdjustTokenQuota.
type QuotaChange struct {
	BeforeTicks, AfterTicks         int64
	BeforeUnlimited, AfterUnlimited bool
	UsedTicks                       int64
}

// AdjustTokenQuota applies one quota adjustment and appends its event e in
// the same transaction, so a quota never changes without a record. The
// quota is never written directly; the amounts of a token's quota records
// always add up to its stored quota.
//
// used reports the token's spend at this moment (the caller passes the
// quota ledger's value while holding the ledger's lock, so billing and the
// adjustment cannot interleave); the database value wins when larger. A
// decrease may not take the quota below what has been used.
func (db *DB) AdjustTokenQuota(ctx context.Context, id int64, action string, amount int64, used func(dbUsed int64) int64, e *Event) (QuotaChange, error) {
	defer db.toks.clear()
	var qc QuotaChange
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return qc, err
	}
	defer tx.Rollback() //nolint:errcheck
	var name, prefix string
	var unl int
	err = tx.QueryRowContext(ctx, `SELECT quota_ticks, unlimited, used_ticks, name, prefix FROM tokens
WHERE id = ? AND deleted_at = 0`, id).Scan(&qc.BeforeTicks, &unl, &qc.UsedTicks, &name, &prefix)
	if errors.Is(err, sql.ErrNoRows) {
		return qc, ErrNotFound
	}
	if err != nil {
		return qc, err
	}
	qc.BeforeUnlimited = unl == 1
	if used != nil {
		qc.UsedTicks = max(qc.UsedTicks, used(qc.UsedTicks))
	}
	qc.AfterTicks, qc.AfterUnlimited = qc.BeforeTicks, qc.BeforeUnlimited
	switch action {
	case QuotaIncrease, QuotaDecrease:
		if qc.BeforeUnlimited {
			return qc, ErrQuotaInvalid{"the token is unlimited; switch it to limited first"}
		}
		if amount <= 0 {
			return qc, ErrQuotaInvalid{"amount must be greater than 0"}
		}
		if action == QuotaIncrease {
			qc.AfterTicks = qc.BeforeTicks + amount
		} else if qc.AfterTicks = qc.BeforeTicks - amount; qc.AfterTicks < qc.UsedTicks {
			return qc, ErrQuotaInvalid{"the quota cannot go below what has been used"}
		}
	case QuotaSetLimited:
		if !qc.BeforeUnlimited {
			return qc, ErrQuotaInvalid{"the token is already limited"}
		}
		if amount < 0 {
			return qc, ErrQuotaInvalid{"the balance to allow must be 0 or more"}
		}
		qc.AfterTicks, qc.AfterUnlimited = qc.UsedTicks+amount, false
	case QuotaSetUnlimited:
		if qc.BeforeUnlimited {
			return qc, ErrQuotaInvalid{"the token is already unlimited"}
		}
		qc.AfterUnlimited = true
	default:
		return qc, ErrQuotaInvalid{"action must be increase, decrease, set_limited or set_unlimited"}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tokens SET quota_ticks = ?, unlimited = ?, updated_at = ? WHERE id = ?`,
		qc.AfterTicks, boolInt(qc.AfterUnlimited), NowMs(), id); err != nil {
		return qc, err
	}
	e.TargetType, e.TargetID, e.TargetName, e.TargetHint = "token", strconv.FormatInt(id, 10), name, prefix
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	d := e.Detail
	d["amount_ticks"], d["before_ticks"], d["after_ticks"] = qc.AfterTicks-qc.BeforeTicks, qc.BeforeTicks, qc.AfterTicks
	d["used_ticks"], d["before_unlimited"], d["unlimited"] = qc.UsedTicks, qc.BeforeUnlimited, qc.AfterUnlimited
	if err := insertEvent(ctx, tx, e); err != nil {
		return qc, err
	}
	return qc, tx.Commit()
}
