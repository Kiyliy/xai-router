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
