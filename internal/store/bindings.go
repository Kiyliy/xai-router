package store

import (
	"context"
	"database/sql"
	"errors"
)

// Binding ties an upstream resource (async video job, stored response, file,
// batch…) to the key that created it, so follow-up calls use the same key.
type Binding struct {
	ResourceID string
	Kind       string
	KeyID      int64
	TokenID    int64
	Model      string
	Billed     bool
	CreatedAt  int64
	ExpiresAt  int64
}

// PutBinding inserts or replaces a binding.
func (db *DB) PutBinding(ctx context.Context, b *Binding) error {
	if b.CreatedAt == 0 {
		b.CreatedAt = NowMs()
	}
	_, err := db.W.ExecContext(ctx, `INSERT INTO bindings (resource_id, kind, key_id, token_id, model, billed, created_at, expires_at)
VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(resource_id) DO UPDATE SET kind=excluded.kind, key_id=excluded.key_id,
token_id=excluded.token_id, model=excluded.model, expires_at=excluded.expires_at`,
		b.ResourceID, b.Kind, b.KeyID, nullID(b.TokenID), b.Model, boolInt(b.Billed), b.CreatedAt, b.ExpiresAt)
	return err
}

// GetBinding returns the binding for a resource id.
func (db *DB) GetBinding(ctx context.Context, id string) (*Binding, error) {
	var b Binding
	var billed int
	err := db.R.QueryRowContext(ctx, `SELECT resource_id, kind, key_id, COALESCE(token_id,0), model, billed, created_at, expires_at
FROM bindings WHERE resource_id = ? AND expires_at > ?`, id, NowMs()).
		Scan(&b.ResourceID, &b.Kind, &b.KeyID, &b.TokenID, &b.Model, &billed, &b.CreatedAt, &b.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.Billed = billed == 1
	return &b, nil
}

// MarkBindingBilled atomically marks a binding billed. It returns true only
// for the first caller, which guarantees an async job is charged once.
func (db *DB) MarkBindingBilled(ctx context.Context, id string) (bool, error) {
	res, err := db.W.ExecContext(ctx, `UPDATE bindings SET billed = 1 WHERE resource_id = ? AND billed = 0`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// PurgeExpiredBindings removes expired bindings.
func (db *DB) PurgeExpiredBindings(ctx context.Context) (int64, error) {
	res, err := db.W.ExecContext(ctx, `DELETE FROM bindings WHERE expires_at < ?`, NowMs())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
