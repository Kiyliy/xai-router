package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
)

// Attempt is one upstream try made while serving a client request.
type Attempt struct {
	Seq       int
	KeyID     int64
	KeyName   string
	Status    int // 0 = network error
	Error     string
	Action    string // success, retry, return, cooldown, disable
	TTFBMs    int64
	LatencyMs int64
	StartedAt int64
}

// Log is one client request.
type Log struct {
	ID               int64
	RequestID        string
	CreatedAt        int64
	Method           string
	Path             string
	Query            string
	Model            string
	Stream           bool
	Status           int
	Error            string
	ClientIP         string
	UserAgent        string
	TokenID          int64 // 0 = none
	TokenName        string
	KeyID            int64 // 0 = none
	KeyName          string
	TTFBMs           int64
	LatencyMs        int64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64
	ReasoningTokens  int64
	ImageTokens      int64
	TotalTokens      int64
	Images           int64
	VideoSeconds     float64
	CostTicks        int64
	CostSource       string
	UpstreamID       string
	Truncated        bool

	// Detail, stored in request_log_bodies / log_attempts.
	ReqHeaders    string
	ReqBody       string
	RespHeaders   string
	RespBody      string
	RespText      string
	ReasoningText string
	Attempts      []Attempt
	AttemptCount  int // filled when reading
	HasBodies     bool
	// NoRecord applies the usage accounting (key/token counters, hourly
	// stats) without storing the log row itself (logging disabled).
	NoRecord bool
}

// Success reports whether the request counts as successful.
func (l *Log) Success() bool { return l.Status >= 200 && l.Status < 400 }

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// InsertLogs writes a batch of logs in a single transaction together with the
// derived counters: key and token usage and the hourly aggregates.
func (db *DB) InsertLogs(ctx context.Context, logs []*Log) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insLog, err := tx.PrepareContext(ctx, `INSERT INTO request_logs (request_id, created_at, method, path, query, model,
stream, status, error, client_ip, user_agent, token_id, token_name, key_id, key_name, attempts, ttfb_ms, latency_ms,
prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens, image_tokens, total_tokens, images, video_seconds,
cost_ticks, cost_source, upstream_id, truncated) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insLog.Close()
	insBody, err := tx.PrepareContext(ctx, `INSERT INTO request_log_bodies (log_id, req_headers, req_body, resp_headers,
resp_body, resp_text, reasoning_text) VALUES (?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insBody.Close()
	insAtt, err := tx.PrepareContext(ctx, `INSERT INTO log_attempts (log_id, seq, key_id, key_name, status, error, action,
ttfb_ms, latency_ms, started_at) VALUES (?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insAtt.Close()
	updKey, err := tx.PrepareContext(ctx, `UPDATE keys SET requests = requests + ?, errors = errors + ?,
cost_ticks = cost_ticks + ?, last_used_at = MAX(last_used_at, ?), last_error = CASE WHEN ? <> '' THEN ? ELSE last_error END WHERE id = ?`)
	if err != nil {
		return err
	}
	defer updKey.Close()
	updToken, err := tx.PrepareContext(ctx, `UPDATE tokens SET requests = requests + 1, used_ticks = used_ticks + ?,
last_used_at = MAX(last_used_at, ?) WHERE id = ?`)
	if err != nil {
		return err
	}
	defer updToken.Close()
	upStats, err := tx.PrepareContext(ctx, `INSERT INTO stats_hourly (hour, key_id, token_id, model, requests, success,
errors, prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens, total_tokens, cost_ticks, latency_ms_sum)
VALUES (?,?,?,?,1,?,?,?,?,?,?,?,?,?)
ON CONFLICT(hour, key_id, token_id, model) DO UPDATE SET requests = requests + 1, success = success + excluded.success,
errors = errors + excluded.errors, prompt_tokens = prompt_tokens + excluded.prompt_tokens,
completion_tokens = completion_tokens + excluded.completion_tokens, cached_tokens = cached_tokens + excluded.cached_tokens,
reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens, total_tokens = total_tokens + excluded.total_tokens,
cost_ticks = cost_ticks + excluded.cost_ticks, latency_ms_sum = latency_ms_sum + excluded.latency_ms_sum`)
	if err != nil {
		return err
	}
	defer upStats.Close()

	for _, l := range logs {
		if l.NoRecord {
			if err := accountOnly(ctx, l, updKey, updToken, upStats); err != nil {
				return err
			}
			continue
		}
		res, err := insLog.ExecContext(ctx, l.RequestID, l.CreatedAt, l.Method, l.Path, l.Query, l.Model, boolInt(l.Stream),
			l.Status, l.Error, l.ClientIP, l.UserAgent, nullID(l.TokenID), l.TokenName, nullID(l.KeyID), l.KeyName,
			len(l.Attempts), l.TTFBMs, l.LatencyMs, l.PromptTokens, l.CompletionTokens, l.CachedTokens, l.ReasoningTokens,
			l.ImageTokens, l.TotalTokens, l.Images, l.VideoSeconds, l.CostTicks, l.CostSource, l.UpstreamID,
			boolInt(l.Truncated))
		if err != nil {
			return err
		}
		l.ID, _ = res.LastInsertId()
		if l.HasBodies {
			if _, err := insBody.ExecContext(ctx, l.ID, l.ReqHeaders, l.ReqBody, l.RespHeaders, l.RespBody, l.RespText,
				l.ReasoningText); err != nil {
				return err
			}
		}
		for _, a := range l.Attempts {
			if _, err := insAtt.ExecContext(ctx, l.ID, a.Seq, nullID(a.KeyID), a.KeyName, a.Status, a.Error, a.Action,
				a.TTFBMs, a.LatencyMs, a.StartedAt); err != nil {
				return err
			}
		}
		if err := accountOnly(ctx, l, updKey, updToken, upStats); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// accountOnly applies a log's usage to key and token counters and the hourly
// aggregates.
func accountOnly(ctx context.Context, l *Log, updKey, updToken, upStats *sql.Stmt) error {
	for _, a := range l.Attempts {
		if a.KeyID == 0 {
			continue
		}
		isErr, lastErr, cost := 0, "", int64(0)
		if a.Action != "success" && a.Action != "return" {
			isErr, lastErr = 1, a.Error
		}
		if a.Action == "success" || (a.Action == "return" && a.KeyID == l.KeyID) {
			cost = l.CostTicks
		}
		if _, err := updKey.ExecContext(ctx, 1, isErr, cost, a.StartedAt, lastErr, lastErr, a.KeyID); err != nil {
			return err
		}
	}
	if l.TokenID != 0 {
		if _, err := updToken.ExecContext(ctx, l.CostTicks, l.CreatedAt, l.TokenID); err != nil {
			return err
		}
	}
	succ, errs := 1, 0
	if !l.Success() {
		succ, errs = 0, 1
	}
	hour := l.CreatedAt / 3600_000 * 3600_000
	_, err := upStats.ExecContext(ctx, hour, l.KeyID, l.TokenID, l.Model, succ, errs, l.PromptTokens,
		l.CompletionTokens, l.CachedTokens, l.ReasoningTokens, l.TotalTokens, l.CostTicks, l.LatencyMs)
	return err
}

// LogFilter narrows ListLogs.
type LogFilter struct {
	From, To int64 // unix ms, 0 = open
	Model    string
	Status   string
	KeyID    int64
	TokenID  int64
	Path     string
	Q        string
	Limit    int
	Offset   int
}

func (f LogFilter) where() (string, []any) {
	var w []string
	var a []any
	if f.From > 0 {
		w, a = append(w, "created_at >= ?"), append(a, f.From)
	}
	if f.To > 0 {
		w, a = append(w, "created_at <= ?"), append(a, f.To)
	}
	if f.Model != "" {
		w, a = append(w, "model = ?"), append(a, f.Model)
	}
	if f.KeyID != 0 {
		w, a = append(w, "key_id = ?"), append(a, f.KeyID)
	}
	if f.TokenID != 0 {
		w, a = append(w, "token_id = ?"), append(a, f.TokenID)
	}
	if f.Path != "" {
		w, a = append(w, `path LIKE ? ESCAPE '\'`), append(a, escapeLike(f.Path)+"%")
	}
	if f.Q != "" {
		like := "%" + escapeLike(f.Q) + "%"
		w, a = append(w, `(request_id = ? OR upstream_id = ? OR error LIKE ? ESCAPE '\')`), append(a, f.Q, f.Q, like)
	}
	switch s := strings.ToLower(f.Status); {
	case s == "":
	case s == "success":
		w = append(w, "status >= 200 AND status < 400")
	case s == "error":
		w = append(w, "(status < 200 OR status >= 400)")
	case len(s) == 3 && strings.HasSuffix(s, "xx") && s[0] >= '1' && s[0] <= '5':
		base := int(s[0]-'0') * 100
		w, a = append(w, "status >= ? AND status < ?"), append(a, base, base+100)
	default:
		if n, err := strconv.Atoi(s); err == nil {
			w, a = append(w, "status = ?"), append(a, n)
		}
	}
	if len(w) == 0 {
		return "", a
	}
	return " WHERE " + strings.Join(w, " AND "), a
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

const logCols = `id, request_id, created_at, method, path, query, model, stream, status, error, client_ip, user_agent,
COALESCE(token_id,0), token_name, COALESCE(key_id,0), key_name, attempts, ttfb_ms, latency_ms, prompt_tokens,
completion_tokens, cached_tokens, reasoning_tokens, image_tokens, total_tokens, images, video_seconds, cost_ticks,
cost_source, upstream_id, truncated`

func scanLog(s interface{ Scan(...any) error }) (*Log, error) {
	var l Log
	var stream, trunc int
	err := s.Scan(&l.ID, &l.RequestID, &l.CreatedAt, &l.Method, &l.Path, &l.Query, &l.Model, &stream, &l.Status,
		&l.Error, &l.ClientIP, &l.UserAgent, &l.TokenID, &l.TokenName, &l.KeyID, &l.KeyName, &l.AttemptCount, &l.TTFBMs,
		&l.LatencyMs, &l.PromptTokens, &l.CompletionTokens, &l.CachedTokens, &l.ReasoningTokens, &l.ImageTokens,
		&l.TotalTokens, &l.Images, &l.VideoSeconds, &l.CostTicks, &l.CostSource, &l.UpstreamID, &trunc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l.Stream, l.Truncated = stream == 1, trunc == 1
	return &l, nil
}

// ListLogs returns a page of logs (newest first) and the total match count.
func (db *DB) ListLogs(ctx context.Context, f LogFilter) ([]*Log, int, error) {
	w, args := f.where()
	var total int
	if err := db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs`+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.R.QueryContext(ctx, `SELECT `+logCols+` FROM request_logs`+w+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*Log
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

// GetLog returns one log with bodies and attempts.
func (db *DB) GetLog(ctx context.Context, id int64) (*Log, error) {
	l, err := scanLog(db.R.QueryRowContext(ctx, `SELECT `+logCols+` FROM request_logs WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	err = db.R.QueryRowContext(ctx, `SELECT req_headers, req_body, resp_headers, resp_body, resp_text, reasoning_text
FROM request_log_bodies WHERE log_id = ?`, id).Scan(&l.ReqHeaders, &l.ReqBody, &l.RespHeaders, &l.RespBody, &l.RespText,
		&l.ReasoningText)
	if err == nil {
		l.HasBodies = true
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := db.R.QueryContext(ctx, `SELECT seq, COALESCE(key_id,0), key_name, status, error, action, ttfb_ms,
latency_ms, started_at FROM log_attempts WHERE log_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a Attempt
		if err := rows.Scan(&a.Seq, &a.KeyID, &a.KeyName, &a.Status, &a.Error, &a.Action, &a.TTFBMs, &a.LatencyMs,
			&a.StartedAt); err != nil {
			return nil, err
		}
		l.Attempts = append(l.Attempts, a)
	}
	return l, rows.Err()
}

// DeleteLogsBefore deletes logs created before t (unix ms) in chunks so the
// writer is never blocked for long. Aggregated stats are kept.
func (db *DB) DeleteLogsBefore(ctx context.Context, t int64) (int64, error) {
	var total int64
	for {
		res, err := db.W.ExecContext(ctx, `DELETE FROM request_logs WHERE id IN
(SELECT id FROM request_logs WHERE created_at < ? LIMIT 2000)`, t)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 2000 {
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// Vacuum reclaims space after large deletions (incremental, cheap).
func (db *DB) Vacuum(ctx context.Context) error {
	_, err := db.W.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}
