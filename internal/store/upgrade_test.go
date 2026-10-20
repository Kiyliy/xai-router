package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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
		if err := db.R.QueryRowContext(ctx, `SELECT v FROM schema_version`).Scan(&v); err != nil || v != SchemaVersion() {
			t.Fatalf("schema version %d (%v), want %d", v, err, SchemaVersion())
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
		if l, err := db.GetLog(ctx, logs[0].ID); err != nil || l.AttemptState() != "single" || !l.AttemptFinal {
			t.Fatalf("old row after v10: %+v %v", l, err)
		}
		if since, _ := db.GetMeta(ctx, MetaStatsBasisSince); since == "" {
			t.Fatal("no stats-basis cutover recorded for a database with history")
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

	if tok, _ := db.GetToken(ctx, 1); !tok.Limited || tok.QuotaTicks != 1000 || tok.UsedTicks != 300 {
		t.Fatalf("token after upgrade: %+v", tok)
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
	_ = db.R.QueryRowContext(ctx, `SELECT SUM(used_ticks) FROM tokens`).Scan(&spent)
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

// v0.4.0 (schema v8) to v9: the usage cleared by "reset usage" is folded
// back with a matching quota increase, so what is left does not change; the
// token's usage equals its log total again; every token gets migration
// records whose amounts add up to its quota (acceptance U18).
func TestUpgradeFromV040ResetFolding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "xai-router.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE schema_version (v INTEGER NOT NULL)`}
	stmts = append(stmts, migrations[:8]...)
	stmts = append(stmts, `INSERT INTO schema_version (v) VALUES (8)`,
		// 1: limited $10, spent 7 of which 5 were reset (used 2 now) -> left 8
		`INSERT INTO tokens (id, name, token_hash, prefix, quota_ticks, used_ticks, reset_ticks, used_reset_at, created_at, updated_at)
		 VALUES (1, 'limited-reset', 'h1', 'sk-xr-1', 100, 20, 50, 1700000000000, 1, 1)`,
		// 2: unlimited, spent 9 of which 4 were reset
		`INSERT INTO tokens (id, name, token_hash, prefix, quota_ticks, used_ticks, reset_ticks, used_reset_at, created_at, updated_at)
		 VALUES (2, 'unlimited-reset', 'h2', 'sk-xr-2', 0, 50, 40, 1700000000000, 1, 1)`,
		// 3: limited, never reset
		`INSERT INTO tokens (id, name, token_hash, prefix, quota_ticks, used_ticks, created_at, updated_at)
		 VALUES (3, 'plain', 'h3', 'sk-xr-3', 30, 10, 1, 1)`)
	// Each token's logs add up to its whole spend (used + reset).
	for id, cost := range map[int]int{1: 70, 2: 90, 3: 10} {
		stmts = append(stmts, fmt.Sprintf(`INSERT INTO request_logs (request_id, created_at, method, path, status, token_id, cost_ticks)
			VALUES ('r%d', 1, 'POST', '/v1/chat/completions', 200, %d, %d)`, id, id, cost))
	}
	for _, q := range stmts {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	raw.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	want := map[int64]struct {
		limited     bool
		quota, used int64
	}{1: {true, 150, 70}, 2: {false, 0, 90}, 3: {true, 30, 10}}
	for id, w := range want {
		tok, err := db.GetToken(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if tok.Limited != w.limited || tok.QuotaTicks != w.quota || tok.UsedTicks != w.used {
			t.Errorf("token %d: %+v, want %+v", id, tok, w)
		}
		var logs int64
		_ = db.R.QueryRowContext(ctx, `SELECT SUM(cost_ticks) FROM request_logs WHERE token_id = ?`, id).Scan(&logs)
		if tok.UsedTicks != logs {
			t.Errorf("token %d: used %d != log total %d", id, tok.UsedTicks, logs)
		}
		evs, _ := db.ListEvents(ctx, EventFilter{Action: "token.quota_", TargetType: "token", TargetID: fmt.Sprint(id), Limit: 10})
		var sum int64
		for _, e := range evs {
			if e.Action != "token.quota_migrate" || e.Actor != "system" || e.Kind != EventAudit {
				t.Errorf("token %d: unexpected record %+v", id, e)
			}
			sum += int64(e.Detail["amount_ticks"].(float64))
		}
		wantRecords := 1
		if id != 3 {
			wantRecords = 2 // plus the folded resets
		}
		if len(evs) != wantRecords || sum != tok.QuotaTicks {
			t.Errorf("token %d: %d records summing to %d, want %d summing to the quota %d", id, len(evs), sum, wantRecords, tok.QuotaTicks)
		}
	}
	// Left to spend is unchanged for the limited token: 100-20 = 150-70.
	if tok, _ := db.GetToken(ctx, 1); tok.QuotaTicks-tok.UsedTicks != 80 {
		t.Fatalf("remaining changed: %+v", tok)
	}
	var cols int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('tokens') WHERE name IN ('reset_ticks', 'used_reset_at')`).Scan(&cols)
	if cols != 0 {
		t.Fatal("reset columns still present")
	}
}

// v0.5.x (schema v9) to v10 (v0.6.0): request_logs gains the attempt
// columns; every existing row is a single final attempt, keeps its nested
// attempts (log_attempts), and stats are not recomputed. The upgrade time
// is kept as the stats-basis cutover. The migration tolerates columns that
// already exist, and opening again changes nothing.
func TestUpgradeFromV9ToV10(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "xai-router.db")
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	stmts := []string{`CREATE TABLE schema_version (v INTEGER NOT NULL)`}
	stmts = append(stmts, migrations[:9]...)
	stmts = append(stmts, `INSERT INTO schema_version (v) VALUES (9)`,
		`INSERT INTO request_logs (request_id, created_at, method, path, status, attempts, cost_ticks)
		 VALUES ('r1', 1000, 'POST', '/v1/chat/completions', 200, 2, 250)`,
		`INSERT INTO log_attempts (log_id, seq, status, action, started_at) VALUES (1, 1, 500, 'retry', 1000), (1, 2, 200, 'success', 1001)`,
		`INSERT INTO stats_dim (dim, hour, id, requests, success, cost_ticks) VALUES ('all', 0, '', 1, 1, 250)`,
		// A column a previous, interrupted attempt may have left behind.
		`ALTER TABLE request_logs ADD COLUMN attempt_index INTEGER NOT NULL DEFAULT 1`)
	for _, s := range stmts {
		if _, err := raw.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	raw.Close()

	var since string
	for i := 0; i < 2; i++ {
		db, err := Open(path)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		var v int
		_ = db.R.QueryRowContext(ctx, `SELECT v FROM schema_version`).Scan(&v)
		if v != SchemaVersion() || v != 10 {
			t.Fatalf("schema version %d", v)
		}
		l, err := db.GetLog(ctx, 1)
		if err != nil || l.AttemptIndex != 1 || l.AttemptTotal != 1 || !l.AttemptFinal || l.AttemptAction != "" ||
			l.AttemptState() != "single" || len(l.Attempts) != 2 || l.CostTicks != 250 {
			t.Fatalf("historical row: %+v %v", l, err)
		}
		if _, n, _ := db.ListLogs(ctx, LogFilter{FinalOnly: true, Limit: 10}); n != 1 {
			t.Fatalf("historical rows are final: %d", n)
		}
		if a, _ := db.StatsTotal(ctx, 0); a.Requests != 1 || a.CostTicks != 250 {
			t.Fatalf("stats changed: %+v", a)
		}
		got, _ := db.GetMeta(ctx, MetaStatsBasisSince)
		if ms, err := strconv.ParseInt(got, 10, 64); err != nil || ms < time.Now().Add(-time.Minute).UnixMilli() {
			t.Fatalf("stats basis %q", got)
		}
		if i == 1 && got != since {
			t.Fatalf("cutover moved on reopen: %s -> %s", since, got)
		}
		since = got
		db.Close()
	}

	var idx int
	db, _ := Open(path)
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'idx_logs_retried'`).Scan(&idx)
	db.Close()
	if idx != 1 {
		t.Fatal("idx_logs_retried missing")
	}
	// A database made by a pre-release v10 build (without started_at and the
	// index) gets them on the next start.
	raw, _ = sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	for _, q := range []string{`DROP INDEX idx_logs_retried`, `ALTER TABLE request_logs DROP COLUMN started_at`} {
		if _, err := raw.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if l, err := db.GetLog(ctx, 1); err != nil || l.StartedAt != 1000 {
		t.Fatalf("pre-release v10 database: %+v %v", l, err)
	}
	db.Close()

	// A new database has always counted rows: no cutover.
	fresh, err := Open(filepath.Join(t.TempDir(), "new.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if got, _ := fresh.GetMeta(ctx, MetaStatsBasisSince); got != "" {
		t.Fatalf("fresh database has a cutover: %q", got)
	}
}
