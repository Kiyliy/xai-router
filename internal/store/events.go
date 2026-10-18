package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
)

// Event kinds. Events are append-only (enforced by triggers) and kept
// forever: they are the audit trail of what administrators did and what
// the gateway decided on its own. Request traffic is not an event; it lives
// in request_logs.
const (
	EventOperation = "operation" // an administrator changed something
	EventSystem    = "system"    // the gateway acted on its own (key disabled, retention run…)
)

// Event is one row of the event log.
type Event struct {
	ID         int64          `json:"id"`
	CreatedAt  int64          `json:"-"`
	Kind       string         `json:"kind"`
	Action     string         `json:"action"`     // e.g. key.update, token.quota_adjust, key.auto_disable
	Actor      string         `json:"actor"`      // password, admin_token:<id>, system
	ActorName  string         `json:"actor_name"` // admin token name, "admin" for the password
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	TargetName string         `json:"target_name"`
	IP         string         `json:"ip"`
	Detail     map[string]any `json:"detail"`
}

// AddEvent appends an event. CreatedAt defaults to now.
func (db *DB) AddEvent(ctx context.Context, e *Event) error {
	return insertEvent(ctx, db.W, e)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertEvent(ctx context.Context, x execer, e *Event) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = NowMs()
	}
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	detail, _ := json.Marshal(e.Detail)
	res, err := x.ExecContext(ctx, `INSERT INTO events (created_at, kind, action, actor, actor_name, target_type,
target_id, target_name, ip, detail) VALUES (?,?,?,?,?,?,?,?,?,?)`, e.CreatedAt, e.Kind, e.Action, e.Actor, e.ActorName,
		e.TargetType, e.TargetID, e.TargetName, e.IP, string(detail))
	if err != nil {
		return err
	}
	e.ID, _ = res.LastInsertId()
	return nil
}

// EventFilter narrows ListEvents. Zero values match everything.
type EventFilter struct {
	Kind       string
	Action     string // exact, or a prefix ending in "." (e.g. "token.")
	TargetType string
	TargetID   string
	From, To   int64 // unix ms
	Q          string
	Limit      int
	Offset     int
}

// ListEvents returns a page of events, newest first, and the total.
func (db *DB) ListEvents(ctx context.Context, f EventFilter) ([]*Event, int64, error) {
	var w []string
	var args []any
	if f.Kind != "" {
		w, args = append(w, "kind = ?"), append(args, f.Kind)
	}
	if f.Action != "" {
		if strings.HasSuffix(f.Action, ".") {
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
	if f.From > 0 {
		w, args = append(w, "created_at >= ?"), append(args, f.From)
	}
	if f.To > 0 {
		w, args = append(w, "created_at <= ?"), append(args, f.To)
	}
	if f.Q != "" {
		like := "%" + escapeLike(f.Q) + "%"
		w = append(w, `(target_name LIKE ? ESCAPE '\' OR actor_name LIKE ? ESCAPE '\' OR action LIKE ? ESCAPE '\' OR detail LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	where := ""
	if len(w) > 0 {
		where = " WHERE " + strings.Join(w, " AND ")
	}
	var total int64
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 {
		f.Limit = 50
	}
	rows, err := db.R.QueryContext(ctx, `SELECT id, created_at, kind, action, actor, actor_name, target_type, target_id,
target_name, ip, detail FROM events`+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		var detail string
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Kind, &e.Action, &e.Actor, &e.ActorName, &e.TargetType, &e.TargetID,
			&e.TargetName, &e.IP, &detail); err != nil {
			return nil, 0, err
		}
		_ = json.Unmarshal([]byte(detail), &e.Detail)
		if e.Detail == nil {
			e.Detail = map[string]any{}
		}
		out = append(out, &e)
	}
	return out, total, rows.Err()
}
