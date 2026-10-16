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
		pragmas := "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
		dsnW = base + pragmas + "&_txlock=immediate"
		dsnR = base + pragmas
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
	db := &DB{W: w, R: r, Path: path}
	if path == ":memory:" {
		// Keep a single connection for reads too: shared cache across pools is
		// flaky with table locks, and tests do not need read concurrency.
		db.R = w
	}
	if err := db.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}

// Close closes both pools.
func (db *DB) Close() error {
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
	for i := v; i < len(migrations); i++ {
		tx, err := db.W.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
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
