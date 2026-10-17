package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// A database created by v0.1.0 (schema v1) upgrades in place without losing
// data, and opening it again is a no-op (migrations are idempotent).
func TestUpgradeFromV010Schema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "xai-router.db")

	// Build a v0.1.0 database: only the first migration, plus data.
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`CREATE TABLE schema_version (v INTEGER NOT NULL)`,
		migrations[0],
		`INSERT INTO schema_version (v) VALUES (1)`,
		`INSERT INTO keys (name, key, base_url, created_at, updated_at) VALUES ('old', 'xai-old', 'https://api.x.ai', 1, 1)`,
		`INSERT INTO tokens (name, token_hash, prefix, quota_ticks, used_ticks, created_at, updated_at)
		 VALUES ('t', 'hash', 'sk-xr-a', 1000, 250, 1, 1)`,
		`INSERT INTO request_logs (request_id, created_at, method, path, status, cost_ticks) VALUES ('r1', 1, 'POST', '/v1/chat/completions', 200, 250)`,
		`INSERT INTO stats_hourly (hour, requests, success, cost_ticks) VALUES (0, 1, 1, 250)`,
		`INSERT INTO meta (k, v) VALUES ('admin_password', 'pbkdf2-sha256$1$c2FsdA$aGFzaA')`,
	}
	for _, s := range stmts {
		if _, err := raw.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	raw.Close()

	for i := 0; i < 2; i++ { // second open must be a no-op
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		var v int
		if err := db.R.QueryRowContext(ctx, `SELECT v FROM schema_version`).Scan(&v); err != nil || v != len(migrations) {
			t.Fatalf("schema version %d (%v), want %d", v, err, len(migrations))
		}
		k, err := db.GetKey(ctx, 1)
		if err != nil || k.Name != "old" || k.Key != "xai-old" {
			t.Fatalf("key lost: %+v %v", k, err)
		}
		tok, err := db.GetTokenByHash(ctx, "hash")
		if err != nil || tok.UsedTicks != 250 || tok.QuotaTicks != 1000 {
			t.Fatalf("token lost: %+v %v", tok, err)
		}
		logs, n, err := db.ListLogs(ctx, LogFilter{Limit: 10})
		if err != nil || n != 1 || logs[0].WS || logs[0].Extra != "" || logs[0].CostTicks != 250 {
			t.Fatalf("logs after upgrade: n=%d %+v %v", n, logs, err)
		}
		if a, _ := db.StatsTotal(ctx, 0); a.Requests != 1 || a.CostTicks != 250 {
			t.Fatalf("stats lost: %+v", a)
		}
		if pw, _ := db.GetMeta(ctx, "admin_password"); pw == "" {
			t.Fatal("admin password lost")
		}
		db.Close()
	}
}

// A v0.3.1 database (schema v7) upgrades to v8 without losing data: renamed
// columns keep their values, and soft delete, reset and labels work on the
// upgraded rows. A newer schema than the binary knows is refused.
func TestUpgradeFromV031Schema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "xai-router.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE schema_version (v INTEGER NOT NULL)`}
	stmts = append(stmts, migrations[:7]...)
	stmts = append(stmts, `INSERT INTO schema_version (v) VALUES (7)`,
		`INSERT INTO keys (name, key, base_url, requests, errors, cost_ticks, created_at, updated_at)
		 VALUES ('k', 'xai-abcdefghijklmnop', 'https://api.x.ai', 5, 2, 300, 1, 1)`,
		`INSERT INTO tokens (name, token_hash, prefix, quota_ticks, used_ticks, created_at, updated_at)
		 VALUES ('t', 'hash', 'sk-xr-a', 1000, 300, 1, 1)`,
		`INSERT INTO models (id, kind, input_ticks, image_input_ticks, updated_at) VALUES ('m', 'language', 10, 7, 1)`,
		`INSERT INTO request_logs (request_id, created_at, method, path, status, key_id, key_name, token_id, token_name, cost_ticks)
		 VALUES ('r1', 1, 'POST', '/v1/chat/completions', 200, 1, 'k', 1, 't', 300)`,
		`INSERT INTO stats_dim (dim, hour, id, requests, cost_ticks) VALUES ('token', 0, '1', 1, 300)`)
	for _, s := range stmts {
		if _, err := raw.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	k, err := db.GetKey(ctx, 1)
	if err != nil || k.Attempts != 5 || k.AttemptErrors != 2 || k.CostTicks != 300 {
		t.Fatalf("key after upgrade: %+v %v", k, err)
	}
	if m, err := db.GetModel(ctx, "m"); err != nil || m.ImageTokenTicks != 7 || m.InputTicks != 10 {
		t.Fatalf("model after upgrade: %+v %v", m, err)
	}
	var n int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'stats_hourly'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("stats_hourly still there: %d %v", n, err)
	}

	// Reset keeps the cleared usage; total spend still equals the logs.
	if err := db.ResetTokenUsage(ctx, 1); err != nil {
		t.Fatal(err)
	}
	tok, _ := db.GetToken(ctx, 1)
	if tok.UsedTicks != 0 || tok.ResetTicks != 300 || tok.UsedResetAt == 0 {
		t.Fatalf("reset: %+v", tok)
	}
	// Soft delete: gone from lists and auth, still labelled; the same key can
	// be added again.
	if err := db.DeleteToken(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteKey(ctx, 1, "xai-…mnop"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetTokenByHash(ctx, "hash"); err != ErrNotFound {
		t.Fatalf("deleted token still authenticates: %v", err)
	}
	if _, err := db.GetKey(ctx, 1); err != ErrNotFound {
		t.Fatalf("deleted key still visible: %v", err)
	}
	if keys, _ := db.ListAllKeys(ctx); len(keys) != 0 {
		t.Fatalf("deleted key still in the pool: %d", len(keys))
	}
	if _, total, _ := db.ListTokens(ctx, "", 10, 0); total != 0 {
		t.Fatalf("deleted token still listed: %d", total)
	}
	kl, _ := db.KeyLabels(ctx, func(string) string { return "live" })
	tl, _ := db.TokenLabels(ctx)
	if kl[1] != (Label{Name: "k", Hint: "xai-…mnop", Deleted: true}) || tl[1] != (Label{Name: "t", Hint: "sk-xr-a", Deleted: true}) {
		t.Fatalf("labels: %+v %+v", kl[1], tl[1])
	}
	var plain int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM keys WHERE key = 'xai-abcdefghijklmnop'`).Scan(&plain)
	if plain != 0 {
		t.Fatal("deleted key's plaintext kept")
	}
	if err := db.CreateKey(ctx, &Key{Name: "again", Key: "xai-abcdefghijklmnop", BaseURL: "https://api.x.ai", Weight: 1, Enabled: true}); err != nil {
		t.Fatalf("re-adding a deleted key: %v", err)
	}
	var spent int64
	_ = db.R.QueryRowContext(ctx, `SELECT SUM(used_ticks + reset_ticks) FROM tokens`).Scan(&spent)
	if spent != 300 {
		t.Fatalf("token spend %d, want the log total 300", spent)
	}
	db.Close()

	// A database from a newer version is refused.
	raw, _ = sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	_, _ = raw.ExecContext(ctx, `UPDATE schema_version SET v = v + 1`)
	raw.Close()
	if db, err := Open(path); err == nil {
		db.Close()
		t.Fatal("a newer schema must be refused")
	}
}
