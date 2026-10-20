// Package store is the SQLite persistence layer.
//
// Writes go through a single connection (SQLite allows one writer at a time and
// this avoids SQLITE_BUSY storms); reads use a separate pool. The database runs
// in WAL mode so readers never block the writer.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned on unique-constraint violations.
var ErrConflict = errors.New("already exists")

// DB wraps the write and read connection pools.
type DB struct {
	W    *sql.DB
	R    *sql.DB
	Path string
	bind *bindingCache // write-behind cache for bindings (see bindings.go)
	toks tokenCache    // client tokens by hash, for request authentication (see tokens.go)

	touchSem chan struct{}  // best-effort bookkeeping writes in flight (see touch)
	touches  sync.WaitGroup // waited for by Close
}

// Open opens (creating if needed) the database at path and runs migrations.
// Use ":memory:" for an in-memory database (tests).
func Open(path string) (*DB, error) {
	var dsnW, dsnR string
	if path == ":memory:" {
		// A shared-cache in-memory DB so that both pools see the same data.
		name := fmt.Sprintf("file:mem%d?mode=memory&cache=shared", time.Now().UnixNano())
		dsnW = name + "&_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"
		dsnR = dsnW
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		base := "file:" + (&url.URL{Path: filepath.ToSlash(path)}).EscapedPath()
		// auto_vacuum must come first: it only takes effect on a new, empty
		// file (no-op on existing databases).
		pragmas := "?_pragma=auto_vacuum(INCREMENTAL)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
		dsnW = base + pragmas + "&_txlock=immediate"
		// Readers: a larger page cache and memory-mapped I/O, so index scans
		// (deep pages, counts) read pages from the OS cache without a copy.
		dsnR = base + pragmas + "&_pragma=cache_size(-65536)&_pragma=mmap_size(4294967296)"
	}
	w, err := sql.Open("sqlite", dsnW)
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxLifetime(0)
	r, err := sql.Open("sqlite", dsnR)
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	db := &DB{W: w, R: r, Path: path, touchSem: make(chan struct{}, touchSlots)}
	if path == ":memory:" {
		// Keep a single connection for reads too: shared cache across pools is
		// flaky with table locks, and tests do not need read concurrency.
		db.R = w
	}
	if err := db.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	db.bind = newBindingCache(db)
	return db, nil
}

// Close closes both pools.
func (db *DB) Close() error {
	db.touches.Wait() // each has a short timeout
	if db.bind != nil {
		db.bind.close() // persist queued bindings first
		db.bind = nil
	}
	err := db.W.Close()
	if db.R != db.W {
		if e := db.R.Close(); err == nil {
			err = e
		}
	}
	return err
}

// Size returns the on-disk size of the database including the WAL file.
func (db *DB) Size() int64 {
	if db.Path == ":memory:" {
		return 0
	}
	var n int64
	for _, suffix := range []string{"", "-wal"} {
		if st, err := os.Stat(db.Path + suffix); err == nil {
			n += st.Size()
		}
	}
	return n
}

var migrations = []string{
	// v1
	`
CREATE TABLE meta (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);

CREATE TABLE keys (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  name            TEXT    NOT NULL,
  key             TEXT    NOT NULL UNIQUE,
  base_url        TEXT    NOT NULL,
  priority        INTEGER NOT NULL DEFAULT 0,
  weight          INTEGER NOT NULL DEFAULT 1,
  enabled         INTEGER NOT NULL DEFAULT 1,
  auto_disabled   INTEGER NOT NULL DEFAULT 0,
  status_reason   TEXT    NOT NULL DEFAULT '',
  cooldown_until  INTEGER NOT NULL DEFAULT 0,
  max_concurrency INTEGER NOT NULL DEFAULT 0,
  rpm_limit       INTEGER NOT NULL DEFAULT 0,
  requests        INTEGER NOT NULL DEFAULT 0,
  errors          INTEGER NOT NULL DEFAULT 0,
  cost_ticks      INTEGER NOT NULL DEFAULT 0,
  last_used_at    INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT    NOT NULL DEFAULT '',
  last_test       TEXT    NOT NULL DEFAULT '',
  last_tested_at  INTEGER NOT NULL DEFAULT 0,
  notes           TEXT    NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE TABLE tokens (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  name           TEXT    NOT NULL,
  token_hash     TEXT    NOT NULL UNIQUE,
  prefix         TEXT    NOT NULL,
  enabled        INTEGER NOT NULL DEFAULT 1,
  quota_ticks    INTEGER NOT NULL DEFAULT 0,
  used_ticks     INTEGER NOT NULL DEFAULT 0,
  expires_at     INTEGER NOT NULL DEFAULT 0,
  allowed_models TEXT    NOT NULL DEFAULT '[]',
  requests       INTEGER NOT NULL DEFAULT 0,
  last_used_at   INTEGER NOT NULL DEFAULT 0,
  notes          TEXT    NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);

CREATE TABLE admin_tokens (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  name         TEXT    NOT NULL,
  token_hash   TEXT    NOT NULL UNIQUE,
  prefix       TEXT    NOT NULL,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  created_at   INTEGER NOT NULL
);

CREATE TABLE sessions (
  token_hash   TEXT PRIMARY KEY,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
);

CREATE TABLE models (
  id                   TEXT PRIMARY KEY,
  kind                 TEXT    NOT NULL,
  aliases              TEXT    NOT NULL DEFAULT '[]',
  enabled              INTEGER NOT NULL DEFAULT 1,
  source               TEXT    NOT NULL DEFAULT 'builtin',
  context_length       INTEGER NOT NULL DEFAULT 0,
  input_ticks          INTEGER NOT NULL DEFAULT 0,
  cached_ticks         INTEGER NOT NULL DEFAULT 0,
  image_input_ticks    INTEGER NOT NULL DEFAULT 0,
  output_ticks         INTEGER NOT NULL DEFAULT 0,
  long_threshold       INTEGER NOT NULL DEFAULT 0,
  long_input_ticks     INTEGER NOT NULL DEFAULT 0,
  long_cached_ticks    INTEGER NOT NULL DEFAULT 0,
  long_output_ticks    INTEGER NOT NULL DEFAULT 0,
  image_ticks          INTEGER NOT NULL DEFAULT 0,
  image_tiers          TEXT    NOT NULL DEFAULT '[]',
  video_ticks_per_sec  INTEGER NOT NULL DEFAULT 0,
  raw                  TEXT    NOT NULL DEFAULT '',
  synced_at            INTEGER NOT NULL DEFAULT 0,
  updated_at           INTEGER NOT NULL
);

CREATE TABLE request_logs (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id        TEXT    NOT NULL,
  created_at        INTEGER NOT NULL,
  method            TEXT    NOT NULL,
  path              TEXT    NOT NULL,
  query             TEXT    NOT NULL DEFAULT '',
  model             TEXT    NOT NULL DEFAULT '',
  stream            INTEGER NOT NULL DEFAULT 0,
  status            INTEGER NOT NULL DEFAULT 0,
  error             TEXT    NOT NULL DEFAULT '',
  client_ip         TEXT    NOT NULL DEFAULT '',
  user_agent        TEXT    NOT NULL DEFAULT '',
  token_id          INTEGER,
  token_name        TEXT    NOT NULL DEFAULT '',
  key_id            INTEGER,
  key_name          TEXT    NOT NULL DEFAULT '',
  attempts          INTEGER NOT NULL DEFAULT 0,
  ttfb_ms           INTEGER NOT NULL DEFAULT 0,
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
  image_tokens      INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  images            INTEGER NOT NULL DEFAULT 0,
  video_seconds     REAL    NOT NULL DEFAULT 0,
  cost_ticks        INTEGER NOT NULL DEFAULT 0,
  cost_source       TEXT    NOT NULL DEFAULT 'none',
  upstream_id       TEXT    NOT NULL DEFAULT '',
  truncated         INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_logs_created ON request_logs(created_at);
CREATE INDEX idx_logs_model   ON request_logs(model, created_at);
CREATE INDEX idx_logs_key     ON request_logs(key_id, created_at);
CREATE INDEX idx_logs_token   ON request_logs(token_id, created_at);
CREATE INDEX idx_logs_status  ON request_logs(status, created_at);
CREATE INDEX idx_logs_reqid   ON request_logs(request_id);

CREATE TABLE request_log_bodies (
  log_id         INTEGER PRIMARY KEY REFERENCES request_logs(id) ON DELETE CASCADE,
  req_headers    TEXT NOT NULL DEFAULT '{}',
  req_body       TEXT NOT NULL DEFAULT '',
  resp_headers   TEXT NOT NULL DEFAULT '{}',
  resp_body      TEXT NOT NULL DEFAULT '',
  resp_text      TEXT NOT NULL DEFAULT '',
  reasoning_text TEXT NOT NULL DEFAULT ''
);

CREATE TABLE log_attempts (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  log_id     INTEGER NOT NULL REFERENCES request_logs(id) ON DELETE CASCADE,
  seq        INTEGER NOT NULL,
  key_id     INTEGER,
  key_name   TEXT    NOT NULL DEFAULT '',
  status     INTEGER NOT NULL DEFAULT 0,
  error      TEXT    NOT NULL DEFAULT '',
  action     TEXT    NOT NULL DEFAULT '',
  ttfb_ms    INTEGER NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  started_at INTEGER NOT NULL
);
CREATE INDEX idx_attempts_log ON log_attempts(log_id);

CREATE TABLE bindings (
  resource_id TEXT PRIMARY KEY,
  kind        TEXT    NOT NULL,
  key_id      INTEGER NOT NULL,
  token_id    INTEGER,
  model       TEXT    NOT NULL DEFAULT '',
  billed      INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);
CREATE INDEX idx_bindings_expires ON bindings(expires_at);

CREATE TABLE stats_hourly (
  hour              INTEGER NOT NULL,
  key_id            INTEGER NOT NULL DEFAULT 0,
  token_id          INTEGER NOT NULL DEFAULT 0,
  model             TEXT    NOT NULL DEFAULT '',
  requests          INTEGER NOT NULL DEFAULT 0,
  success           INTEGER NOT NULL DEFAULT 0,
  errors            INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  cost_ticks        INTEGER NOT NULL DEFAULT 0,
  latency_ms_sum    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (hour, key_id, token_id, model)
);
`,
	// v2: WebSocket sessions
	`
ALTER TABLE request_logs ADD COLUMN ws INTEGER NOT NULL DEFAULT 0;
ALTER TABLE request_logs ADD COLUMN extra TEXT NOT NULL DEFAULT '';
`,
	// v3: scale to millions of logs. One small hourly rollup per dimension
	// (stats_hourly is keyed by every dimension at once and grows almost as
	// fast as the logs), and indexes that serve each list filter in
	// (created_at, id) order.
	`
CREATE TABLE stats_dim (
  dim               TEXT    NOT NULL,
  hour              INTEGER NOT NULL,
  id                TEXT    NOT NULL,
  requests          INTEGER NOT NULL DEFAULT 0,
  success           INTEGER NOT NULL DEFAULT 0,
  errors            INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens  INTEGER NOT NULL DEFAULT 0,
  total_tokens      INTEGER NOT NULL DEFAULT 0,
  cost_ticks        INTEGER NOT NULL DEFAULT 0,
  latency_ms_sum    INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (dim, hour, id)
) WITHOUT ROWID;
INSERT INTO stats_dim SELECT 'all', hour, '', SUM(requests), SUM(success), SUM(errors), SUM(prompt_tokens),
  SUM(completion_tokens), SUM(cached_tokens), SUM(reasoning_tokens), SUM(total_tokens), SUM(cost_ticks),
  SUM(latency_ms_sum) FROM stats_hourly GROUP BY hour;
INSERT INTO stats_dim SELECT 'model', hour, model, SUM(requests), SUM(success), SUM(errors), SUM(prompt_tokens),
  SUM(completion_tokens), SUM(cached_tokens), SUM(reasoning_tokens), SUM(total_tokens), SUM(cost_ticks),
  SUM(latency_ms_sum) FROM stats_hourly GROUP BY hour, model;
INSERT INTO stats_dim SELECT 'key', hour, CAST(key_id AS TEXT), SUM(requests), SUM(success), SUM(errors), SUM(prompt_tokens),
  SUM(completion_tokens), SUM(cached_tokens), SUM(reasoning_tokens), SUM(total_tokens), SUM(cost_ticks),
  SUM(latency_ms_sum) FROM stats_hourly GROUP BY hour, key_id;
INSERT INTO stats_dim SELECT 'token', hour, CAST(token_id AS TEXT), SUM(requests), SUM(success), SUM(errors), SUM(prompt_tokens),
  SUM(completion_tokens), SUM(cached_tokens), SUM(reasoning_tokens), SUM(total_tokens), SUM(cost_ticks),
  SUM(latency_ms_sum) FROM stats_hourly GROUP BY hour, token_id;
DELETE FROM stats_hourly;
CREATE INDEX idx_logs_ok ON request_logs(((status >= 200 AND status < 400) OR status = 101), created_at);
CREATE INDEX idx_logs_class ON request_logs((status / 100), created_at);
CREATE INDEX idx_logs_path ON request_logs(path, created_at);
CREATE INDEX idx_logs_upstream ON request_logs(upstream_id);
`,
	// v4: error-text search scans only failed requests.
	`
CREATE INDEX idx_logs_errored ON request_logs(created_at) WHERE error <> '';
`,
	// v5: resolution-tiered video prices and media input prices; the request
	// parameters an async job is priced by.
	`
ALTER TABLE models ADD COLUMN video_tiers TEXT NOT NULL DEFAULT '[]';
ALTER TABLE models ADD COLUMN input_image_ticks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE models ADD COLUMN input_video_ticks_per_sec INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bindings ADD COLUMN price_meta TEXT NOT NULL DEFAULT '';
`,
	// v6: error-text search reads the index only (the rows are wide), also
	// when combined with the other list filters.
	`
CREATE INDEX idx_logs_errsearch ON request_logs(created_at, id, error, model, key_id, token_id, status, path)
  WHERE error <> '';
DROP INDEX idx_logs_errored;
`,
	// v7: the model, key and token indexes also hold the other list filters,
	// so a combination (key + token + model…) is answered from one index
	// without reading the wide rows.
	`
CREATE INDEX idx_logs_model_f ON request_logs(model, created_at, key_id, token_id, status, path);
CREATE INDEX idx_logs_key_f   ON request_logs(key_id, created_at, token_id, model, status, path);
CREATE INDEX idx_logs_token_f ON request_logs(token_id, created_at, key_id, model, status, path);
DROP INDEX idx_logs_model;
DROP INDEX idx_logs_key;
DROP INDEX idx_logs_token;
`,
	// v8 (v0.4.0, schema review): upstream keys and client tokens are soft
	// deleted so their spend, name and prefix stay with the history; a token
	// usage reset is recorded instead of lost; the key counters are named for
	// what they count (upstream attempts); the per-image-token price is no
	// longer one letter away from the per-input-image price; the unused v1
	// stats table goes. All small tables: no rebuild of the log tables.
	`
ALTER TABLE keys ADD COLUMN deleted_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE keys RENAME COLUMN requests TO attempts;
ALTER TABLE keys RENAME COLUMN errors TO attempt_errors;
ALTER TABLE tokens ADD COLUMN deleted_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tokens ADD COLUMN reset_ticks INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tokens ADD COLUMN used_reset_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE models RENAME COLUMN image_input_ticks TO image_token_ticks;
ALTER TABLE request_logs ADD COLUMN token_prefix TEXT NOT NULL DEFAULT '';
DROP TABLE stats_hourly;
`,
	// v9 (v0.4.1): the event log, behind the console's unified "Logs" page
	// together with request_logs. audit = what administrators did, system =
	// what the gateway decided on its own; both kept forever and append-only
	// (the triggers refuse any UPDATE or DELETE). Client token quotas become
	// adjustment-only: "unlimited" is its own flag (a limited quota of 0
	// blocks), usage is never reset again, and the usage cleared by v0.4.0's
	// resets is folded back (with a matching quota increase, so what is left
	// does not change); every token gets a migration record so that the sum
	// of its quota records equals its quota. Request logs gain the cache-write
	// token count (NULL when the upstream does not report it) and the prices
	// the request was billed at.
	`
CREATE TABLE events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at  INTEGER NOT NULL,
  kind        TEXT    NOT NULL,
  level       TEXT    NOT NULL DEFAULT 'info',
  action      TEXT    NOT NULL,
  actor       TEXT    NOT NULL DEFAULT '',
  actor_name  TEXT    NOT NULL DEFAULT '',
  target_type TEXT    NOT NULL DEFAULT '',
  target_id   TEXT    NOT NULL DEFAULT '',
  target_name TEXT    NOT NULL DEFAULT '',
  target_hint TEXT    NOT NULL DEFAULT '',
  ip          TEXT    NOT NULL DEFAULT '',
  request_id  TEXT    NOT NULL DEFAULT '',
  detail      TEXT    NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_events_created ON events(created_at);
CREATE INDEX idx_events_kind ON events(kind, created_at);
CREATE INDEX idx_events_target ON events(target_type, target_id, created_at);
CREATE INDEX idx_events_problems ON events(created_at) WHERE level <> 'info';
CREATE TRIGGER events_append_only_update BEFORE UPDATE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;
CREATE TRIGGER events_append_only_delete BEFORE DELETE ON events
BEGIN SELECT RAISE(ABORT, 'events are append-only'); END;

INSERT INTO events (created_at, kind, level, action, actor, actor_name, target_type, target_id, target_name, target_hint, detail)
SELECT CAST(strftime('%s','now') AS INTEGER) * 1000, 'audit', 'info', 'token.quota_migrate', 'system', 'system', 'token',
  CAST(id AS TEXT), name, prefix,
  json_object('amount_ticks', quota_ticks, 'before_ticks', 0, 'after_ticks', quota_ticks, 'used_ticks', used_ticks,
    'unlimited', json(CASE WHEN quota_ticks = 0 THEN 'true' ELSE 'false' END), 'reason_code', 'pre_v0_4_1')
FROM tokens;
INSERT INTO events (created_at, kind, level, action, actor, actor_name, target_type, target_id, target_name, target_hint, detail)
SELECT CAST(strftime('%s','now') AS INTEGER) * 1000, 'audit', 'info', 'token.quota_migrate', 'system', 'system', 'token',
  CAST(id AS TEXT), name, prefix,
  json_object('amount_ticks', CASE WHEN quota_ticks = 0 THEN 0 ELSE reset_ticks END, 'before_ticks', quota_ticks,
    'after_ticks', CASE WHEN quota_ticks = 0 THEN 0 ELSE quota_ticks + reset_ticks END,
    'used_ticks', used_ticks + reset_ticks, 'unlimited', json(CASE WHEN quota_ticks = 0 THEN 'true' ELSE 'false' END),
    'reason_code', 'reset_folded', 'reset_ticks', reset_ticks, 'last_reset_at', used_reset_at)
FROM tokens WHERE reset_ticks > 0;
ALTER TABLE tokens ADD COLUMN unlimited INTEGER NOT NULL DEFAULT 0;
UPDATE tokens SET unlimited = 1 WHERE quota_ticks = 0;
UPDATE tokens SET used_ticks = used_ticks + reset_ticks,
  quota_ticks = CASE WHEN unlimited = 1 THEN quota_ticks ELSE quota_ticks + reset_ticks END
WHERE reset_ticks > 0;
ALTER TABLE tokens DROP COLUMN reset_ticks;
ALTER TABLE tokens DROP COLUMN used_reset_at;

ALTER TABLE request_logs ADD COLUMN cache_write_tokens INTEGER;
ALTER TABLE request_logs ADD COLUMN price_snapshot TEXT NOT NULL DEFAULT '';
`,
}

// codeMigrations follow the SQL migrations (v10 onwards). Each checks what
// it changes first, so that it is safe on a database it was already partly
// applied to.
var codeMigrations = []func(ctx context.Context, tx *sql.Tx) error{migrateV10}

// MetaStatsBasisSince is the meta key holding the time (unix ms) the stats
// started counting log rows instead of client requests (the v10 upgrade).
// It is absent on a database that always counted rows.
const MetaStatsBasisSince = "stats_basis_since"

// migrateV10 (v0.6.0): every upstream attempt of a client request is its
// own request_logs row. The rows of one client request share request_id;
// attempt_index (1-based) and attempt_total place a row in its chain,
// attempt_final marks the row that holds the outcome (and the usage and
// cost), attempt_action is what the pool did after the attempt, started_at
// is when the row's attempt started (0 = created_at). The rows of a chain
// share created_at, the list order: just past the end of the client
// request, so the chain stays together and events it caused sort below it.
// Existing rows are single final attempts (the defaults). Stats count rows
// from now on: when there are stats from before, the cutover time is kept
// in meta.
func migrateV10(ctx context.Context, tx *sql.Tx) error {
	have := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info('request_logs')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range [][2]string{
		{"attempt_index", "INTEGER NOT NULL DEFAULT 1"},
		{"attempt_total", "INTEGER NOT NULL DEFAULT 1"},
		{"attempt_final", "INTEGER NOT NULL DEFAULT 1"},
		{"attempt_action", "TEXT NOT NULL DEFAULT ''"},
		{"started_at", "INTEGER NOT NULL DEFAULT 0"},
	} {
		if have[c[0]] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE request_logs ADD COLUMN `+c[0]+` `+c[1]); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO meta (k, v)
SELECT ?, CAST(CAST((julianday('now') - 2440587.5) * 86400000 AS INTEGER) AS TEXT)
WHERE EXISTS (SELECT 1 FROM stats_dim) OR EXISTS (SELECT 1 FROM request_logs)`, MetaStatsBasisSince)
	return err
}

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.W.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (v INTEGER NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := db.W.QueryRowContext(ctx, `SELECT v FROM schema_version`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := db.W.ExecContext(ctx, `INSERT INTO schema_version (v) VALUES (0)`); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	latest := SchemaVersion()
	if v > latest {
		// An older binary on a newer database would misread it (for example
		// treat soft-deleted tokens as live): refuse instead.
		return fmt.Errorf("database schema v%d is newer than this version of xai-router supports (v%d); upgrade xai-router",
			v, latest)
	}
	for i := v; i < latest; i++ {
		tx, err := db.W.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if i < len(migrations) {
			_, err = tx.ExecContext(ctx, migrations[i])
		} else {
			err = codeMigrations[i-len(migrations)](ctx, tx)
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE schema_version SET v = ?`, i+1); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	if v < latest { // the events table exists from v9
		_ = db.AddEvent(ctx, &Event{Kind: EventSystem, Action: "system.migrate", Actor: "system", ActorName: "system",
			TargetType: "database", Detail: map[string]any{"from_version": v, "to_version": latest}})
	}
	return nil
}

// GetMeta reads a value from the meta table ("" if missing).
func (db *DB) GetMeta(ctx context.Context, k string) (string, error) {
	var v string
	err := db.R.QueryRowContext(ctx, `SELECT v FROM meta WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetMeta writes a value to the meta table.
func (db *DB) SetMeta(ctx context.Context, k, v string) error {
	_, err := db.W.ExecContext(ctx, `INSERT INTO meta (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// NowMs returns the current time in Unix milliseconds.
func NowMs() int64 { return time.Now().UnixMilli() }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// SchemaVersion is the schema version this binary migrates to.
func SchemaVersion() int { return len(migrations) + len(codeMigrations) }
