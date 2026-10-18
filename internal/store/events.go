package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Event kinds. Events are append-only (enforced by triggers) and kept
// forever: they are the record of what administrators did and what the
// gateway decided on its own. Request traffic is in request_logs; the
// console shows both in one list.
const (
	EventAudit  = "audit"  // an administrator (or an admin API token) did something
	EventSystem = "system" // the gateway acted on its own (key disabled, retention run…)
)

// Event levels (requests get theirs from the status code).
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Event is one row of the event log.
type Event struct {
	ID         int64
	CreatedAt  int64
	Kind       string
	Level      string
	Action     string // e.g. key.update, token.quota_increase, key.auto_disable
	Actor      string // password, admin_token:<id>, system, anonymous
	ActorName  string // admin token name, "admin" for the password
	TargetType string
	TargetID   string
	TargetName string
	TargetHint string // token prefix or masked key, to tell same names apart
	IP         string
	RequestID  string // the request that caused it, if any
	Detail     map[string]any
}

// AddEvent appends an event. CreatedAt defaults to now, Level to info.
func (db *DB) AddEvent(ctx context.Context, e *Event) error {
	return insertEvent(ctx, db.W, e)
}

// AddEvents appends events in one transaction (seeding and imports).
func (db *DB) AddEvents(ctx context.Context, es []*Event) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, e := range es {
		if err := insertEvent(ctx, tx, e); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertEvent(ctx context.Context, x execer, e *Event) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = NowMs()
	}
	if e.Level == "" {
		e.Level = LevelInfo
	}
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	detail, _ := json.Marshal(e.Detail)
	res, err := x.ExecContext(ctx, `INSERT INTO events (created_at, kind, level, action, actor, actor_name, target_type,
target_id, target_name, target_hint, ip, request_id, detail) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.CreatedAt, e.Kind,
		e.Level, e.Action, e.Actor, e.ActorName, e.TargetType, e.TargetID, e.TargetName, e.TargetHint, e.IP, e.RequestID,
		string(detail))
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

// EventFilter narrows event queries. Zero values match everything.
type EventFilter struct {
	Kinds        []string // audit, system
	ProblemsOnly bool     // level warn or error
	Action       string   // exact, or a prefix ending in "." or "_" (e.g. "token.", "token.quota_")
	TargetType   string
	TargetID     string
	Actor        string
	From, To     int64 // unix ms, inclusive
	Q            string
	// Before restricts to events strictly older than (BeforeAt, BeforeID)
	// in (created_at DESC, id DESC) order: cursor paging.
	BeforeAt, BeforeID int64
	Limit, Offset      int
}

func (f EventFilter) where() (string, []any) {
	var w []string
	var args []any
	if len(f.Kinds) > 0 {
		w = append(w, "kind IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.Kinds)), ",")+")")
		for _, k := range f.Kinds {
			args = append(args, k)
		}
	}
	if f.ProblemsOnly {
		w = append(w, "level <> 'info'")
	}
	if f.Action != "" {
		if strings.HasSuffix(f.Action, ".") || strings.HasSuffix(f.Action, "_") {
			w, args = append(w, "action >= ? AND action < ?"), append(args, f.Action, f.Action+"\U0010FFFF")
		} else {
			w, args = append(w, "action = ?"), append(args, f.Action)
		}
	}
	if f.TargetType != "" {
		w, args = append(w, "target_type = ?"), append(args, f.TargetType)
	}
	if f.TargetID != "" {
		w, args = append(w, "target_id = ?"), append(args, f.TargetID)
	}
	if f.Actor != "" {
		w, args = append(w, "actor = ?"), append(args, f.Actor)
	}
	if f.From > 0 {
		w, args = append(w, "created_at >= ?"), append(args, f.From)
	}
	if f.To > 0 {
		w, args = append(w, "created_at <= ?"), append(args, f.To)
	}
	if f.BeforeAt > 0 {
		w, args = append(w, "(created_at < ? OR (created_at = ? AND id < ?))"), append(args, f.BeforeAt, f.BeforeAt, f.BeforeID)
	}
	if f.Q != "" {
		like := "%" + escapeLike(f.Q) + "%"
		w = append(w, `(target_name LIKE ? ESCAPE '\' OR actor_name LIKE ? ESCAPE '\' OR action LIKE ? ESCAPE '\' OR request_id = ? OR detail LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, f.Q, like)
	}
	if len(w) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(w, " AND "), args
}

// CountEvents returns how many events match f (ignoring the cursor).
func (db *DB) CountEvents(ctx context.Context, f EventFilter) (int64, error) {
	f.BeforeAt, f.BeforeID = 0, 0
	w, args := f.where()
	var n int64
	err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`+w, args...).Scan(&n)
	return n, err
}

// ListEvents returns events matching f, newest first.
func (db *DB) ListEvents(ctx context.Context, f EventFilter) ([]*Event, error) {
	w, args := f.where()
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rows, err := db.R.QueryContext(ctx, `SELECT id, created_at, kind, level, action, actor, actor_name, target_type,
target_id, target_name, target_hint, ip, request_id, detail FROM events`+w+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		var detail string
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Kind, &e.Level, &e.Action, &e.Actor, &e.ActorName, &e.TargetType,
			&e.TargetID, &e.TargetName, &e.TargetHint, &e.IP, &e.RequestID, &detail); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(detail), &e.Detail)
		if e.Detail == nil {
			e.Detail = map[string]any{}
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// GetEvent returns one event.
func (db *DB) GetEvent(ctx context.Context, id int64) (*Event, error) {
	var e Event
	var detail string
	err := db.R.QueryRowContext(ctx, `SELECT id, created_at, kind, level, action, actor, actor_name, target_type,
target_id, target_name, target_hint, ip, request_id, detail FROM events WHERE id = ?`, id).Scan(&e.ID, &e.CreatedAt, &e.Kind,
		&e.Level, &e.Action, &e.Actor, &e.ActorName, &e.TargetType, &e.TargetID, &e.TargetName, &e.TargetHint, &e.IP,
		&e.RequestID, &detail)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(detail), &e.Detail)
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	return &e, nil
}
