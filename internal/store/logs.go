package store

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
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
	WS            bool
	Extra         string // JSON with endpoint-specific details (WebSocket session stats)
	// NoRecord applies the usage accounting (key/token counters, hourly
	// stats) without storing the log row itself (logging disabled).
	NoRecord bool
}

// Success reports whether the request counts as successful.
// Success reports whether the request counts as successful: any 2xx/3xx
// response, or 101 for an established WebSocket session.
func (l *Log) Success() bool {
	return (l.Status >= 200 && l.Status < 400) || l.Status == http.StatusSwitchingProtocols
}

// successSQL is Success as an SQL condition.
const successSQL = "((status >= 200 AND status < 400) OR status = 101)"

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
cost_ticks, cost_source, upstream_id, truncated, ws, extra) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
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
	upStats, err := tx.PrepareContext(ctx, `INSERT INTO stats_dim (dim, hour, id, requests, success,
errors, prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens, total_tokens, cost_ticks, latency_ms_sum)
VALUES (?,?,?,1,?,?,?,?,?,?,?,?,?)
ON CONFLICT(dim, hour, id) DO UPDATE SET requests = requests + 1, success = success + excluded.success,
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
			boolInt(l.Truncated), boolInt(l.WS), l.Extra)
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
	for _, d := range [4][2]string{{"all", ""}, {"model", l.Model}, {"key", strconv.FormatInt(l.KeyID, 10)},
		{"token", strconv.FormatInt(l.TokenID, 10)}} {
		if _, err := upStats.ExecContext(ctx, d[0], hour, d[1], succ, errs, l.PromptTokens, l.CompletionTokens,
			l.CachedTokens, l.ReasoningTokens, l.TotalTokens, l.CostTicks, l.LatencyMs); err != nil {
			return err
		}
	}
	return nil
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
	qIsID    bool // set by ListLogsPage: search Q as a request/upstream id
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
		// Prefix match as a range so idx_logs_path can serve it.
		w, a = append(w, "path >= ? AND path < ?"), append(a, f.Path, f.Path+"\U0010FFFF")
	}
	if f.Q != "" {
		if f.qIsID {
			// Two index lookups. Written as a subquery so that another
			// filter's index (model, key…) is not walked instead.
			w, a = append(w, `id IN (SELECT id FROM request_logs WHERE request_id = ?
UNION SELECT id FROM request_logs WHERE upstream_id = ?)`), append(a, f.Q, f.Q)
		} else {
			// "error <> ''" lets idx_logs_errsearch restrict the scan to
			// failed requests.
			w, a = append(w, `error <> '' AND error LIKE ? ESCAPE '\'`), append(a, "%"+escapeLike(f.Q)+"%")
		}
	}
	switch s := strings.ToLower(f.Status); {
	case s == "":
	case s == "success": // these expressions match the indexes idx_logs_ok / idx_logs_class
		w = append(w, successSQL+" = 1")
	case s == "error":
		w = append(w, successSQL+" = 0")
	case len(s) == 3 && strings.HasSuffix(s, "xx") && s[0] >= '1' && s[0] <= '5':
		w, a = append(w, "(status / 100) = ?"), append(a, int(s[0]-'0'))
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
cost_source, upstream_id, truncated, ws, extra`

func scanLog(s interface{ Scan(...any) error }) (*Log, error) {
	var l Log
	var stream, trunc, ws int
	err := s.Scan(&l.ID, &l.RequestID, &l.CreatedAt, &l.Method, &l.Path, &l.Query, &l.Model, &stream, &l.Status,
		&l.Error, &l.ClientIP, &l.UserAgent, &l.TokenID, &l.TokenName, &l.KeyID, &l.KeyName, &l.AttemptCount, &l.TTFBMs,
		&l.LatencyMs, &l.PromptTokens, &l.CompletionTokens, &l.CachedTokens, &l.ReasoningTokens, &l.ImageTokens,
		&l.TotalTokens, &l.Images, &l.VideoSeconds, &l.CostTicks, &l.CostSource, &l.UpstreamID, &trunc, &ws, &l.Extra)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l.Stream, l.Truncated, l.WS = stream == 1, trunc == 1, ws == 1
	return &l, nil
}

// LogPage is one page of a log listing.
type LogPage struct {
	Logs []*Log
	// Total matches the filter. TotalKind says how it was obtained: "exact",
	// "approx" (from the hourly rollups, when counting would take too long)
	// or "at_least" (a lower bound, when neither is possible).
	Total     int64
	TotalKind string
}

// largeResult is the size above which totals are estimated, not counted.
const largeResult = 50_000

// CountBudget bounds how long an exact count may take before it is
// replaced by an estimate.
var CountBudget = 250 * time.Millisecond

// ListLogs returns a page of logs (newest first) and the total match count.
func (db *DB) ListLogs(ctx context.Context, f LogFilter) ([]*Log, int, error) {
	p, err := db.ListLogsPage(ctx, f)
	if err != nil {
		return nil, 0, err
	}
	return p.Logs, int(p.Total), nil
}

// ListLogsPage returns a page of logs, newest first. Rows are ordered by
// (created_at, id) so that every filter index returns them in order; the
// page is located with an id-only subquery so deep offsets walk an index
// rather than the table.
func (db *DB) ListLogsPage(ctx context.Context, f LogFilter) (*LogPage, error) {
	if f.Q != "" && looksLikeID(f.Q) {
		// Ids are searched exactly (indexed). If nothing has that id, fall
		// back to searching error texts.
		idf := f
		idf.qIsID = true
		p, err := db.listLogsPage(ctx, idf)
		if err != nil || p.Total > 0 || f.Offset > 0 {
			return p, err
		}
	}
	return db.listLogsPage(ctx, f)
}

// looksLikeID reports whether s could be a request or upstream id.
func looksLikeID(s string) bool {
	if len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !(c == '-' || c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}

func (db *DB) listLogsPage(ctx context.Context, f LogFilter) (*LogPage, error) {
	w, args := f.where()
	from := " FROM request_logs"
	if f.Q != "" && !f.qIsID {
		// Error-text search: scan the covering index of failed requests. With
		// another filter the planner would rather walk that filter's index
		// and read every (wide) row to test the error text.
		from = " FROM request_logs INDEXED BY idx_logs_errsearch"
	}
	// Count first, then fetch the page: logs written in between can only
	// add rows, so the total never exceeds what the page shows.
	p := &LogPage{}
	counted := false
	if est, ok := db.estimateCount(ctx, f); ok && est > largeResult {
		// A large result is reported from the rollups straight away;
		// counting it exactly would take longer than the page itself.
		p.Total, p.TotalKind, counted = est, "approx", true
	} else {
		cctx, cancel := context.WithTimeout(ctx, CountBudget)
		var total int64
		err := db.R.QueryRowContext(cctx, `SELECT COUNT(*)`+from+w, args...).Scan(&total)
		cancel()
		switch {
		case err == nil:
			p.Total, p.TotalKind, counted = total, "exact", true
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case ok:
			p.Total, p.TotalKind, counted = est, "approx", true
		}
	}
	q := `SELECT ` + logCols + ` FROM request_logs WHERE id IN (SELECT id` + from + w +
		` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?) ORDER BY created_at DESC, id DESC`
	rows, err := db.R.QueryContext(ctx, q, append(append([]any(nil), args...), f.Limit, f.Offset)...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		p.Logs = append(p.Logs, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !counted {
		p.Total, p.TotalKind = int64(f.Offset+len(p.Logs)), "at_least"
		if len(p.Logs) == f.Limit {
			p.Total++ // there is at least one more
		}
	}
	return p, nil
}

// estimateCount estimates the number of matching logs from the hourly
// rollups. It works for a time range combined with at most one of model,
// key or token and an optional success/error filter.
func (db *DB) estimateCount(ctx context.Context, f LogFilter) (int64, bool) {
	if f.Path != "" || f.Q != "" {
		return 0, false
	}
	dim, id := "all", ""
	n := 0
	if f.Model != "" {
		dim, id, n = "model", f.Model, n+1
	}
	if f.KeyID != 0 {
		dim, id, n = "key", strconv.FormatInt(f.KeyID, 10), n+1
	}
	if f.TokenID != 0 {
		dim, id, n = "token", strconv.FormatInt(f.TokenID, 10), n+1
	}
	col := "requests"
	switch strings.ToLower(f.Status) {
	case "":
	case "success":
		col = "success"
	case "error":
		col = "errors"
	default:
		return 0, false
	}
	if n > 1 {
		return 0, false
	}
	// Rollups outlive deleted logs: start at the oldest remaining log.
	var oldest sql.NullInt64
	if err := db.R.QueryRowContext(ctx, `SELECT MIN(created_at) FROM request_logs`).Scan(&oldest); err != nil || !oldest.Valid {
		return 0, false
	}
	from := max(f.From, oldest.Int64) / 3600_000 * 3600_000
	to := f.To
	if to <= 0 {
		to = 1 << 62
	}
	var est int64
	err := db.R.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+col+`),0) FROM stats_dim WHERE dim = ? AND id = ?
AND hour >= ? AND hour <= ?`, dim, id, from, to).Scan(&est)
	return est, err == nil
}

// LogStats describes the stored logs (for the console).
type LogStats struct {
	Count      int64 // approximate: id range of the remaining logs
	OldestAt   int64
	NewestAt   int64
	FreeBytes  int64 // reusable pages inside the database file
	AutoVacuum string
}

// LogStorageStats returns cheap facts about the log tables.
func (db *DB) LogStorageStats(ctx context.Context) (LogStats, error) {
	var s LogStats
	// Separate statements: SQLite answers a lone MIN or MAX from an index
	// in O(log n) but scans for MIN and MAX together.
	var minID, maxID, oldest, newest sql.NullInt64
	if err := db.R.QueryRowContext(ctx, `SELECT MIN(id) FROM request_logs`).Scan(&minID); err != nil {
		return s, err
	}
	_ = db.R.QueryRowContext(ctx, `SELECT MAX(id) FROM request_logs`).Scan(&maxID)
	if maxID.Valid {
		s.Count = maxID.Int64 - minID.Int64 + 1
	}
	_ = db.R.QueryRowContext(ctx, `SELECT MIN(created_at) FROM request_logs`).Scan(&oldest)
	_ = db.R.QueryRowContext(ctx, `SELECT MAX(created_at) FROM request_logs`).Scan(&newest)
	s.OldestAt, s.NewestAt = oldest.Int64, newest.Int64
	var free, pageSize, av int64
	_ = db.R.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free)
	_ = db.R.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize)
	_ = db.R.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&av)
	s.FreeBytes = free * pageSize
	s.AutoVacuum = map[int64]string{0: "none", 1: "full", 2: "incremental"}[av]
	return s, nil
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

// DeleteLogsBefore deletes logs created before t (unix ms). Aggregated
// stats are kept.
func (db *DB) DeleteLogsBefore(ctx context.Context, t int64) (int64, error) {
	return db.DeleteLogsBeforeProgress(ctx, t, nil)
}

// CountLogsBefore estimates how many logs are older than t (id range).
func (db *DB) CountLogsBefore(ctx context.Context, t int64) int64 {
	var lo, hi sql.NullInt64
	_ = db.R.QueryRowContext(ctx, `SELECT MIN(id) FROM request_logs`).Scan(&lo)
	_ = db.R.QueryRowContext(ctx, `SELECT id FROM request_logs WHERE created_at < ? ORDER BY created_at DESC LIMIT 1`, t).Scan(&hi)
	if !hi.Valid {
		return 0
	}
	return hi.Int64 - lo.Int64 + 1
}

// purgeChunk is how many logs one delete statement removes; between chunks
// the write lock is released briefly so request logging and bindings are
// never held up for long.
const purgeChunk = 1000

// DeleteLogsBeforeProgress deletes logs created before t in small chunks,
// reporting progress after each chunk.
func (db *DB) DeleteLogsBeforeProgress(ctx context.Context, t int64, progress func(deleted int64)) (int64, error) {
	var total int64
	for {
		res, err := db.W.ExecContext(ctx, `DELETE FROM request_logs WHERE id IN
(SELECT id FROM request_logs WHERE created_at < ? ORDER BY created_at LIMIT ?)`, t, purgeChunk)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if progress != nil {
			progress(total)
		}
		if n < purgeChunk {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// IncrementalVacuumIfEnabled reclaims free pages when the database supports
// it without a full rebuild (auto_vacuum = INCREMENTAL); otherwise it only
// checkpoints the WAL.
func (db *DB) IncrementalVacuumIfEnabled(ctx context.Context) (int64, error) {
	var mode int64
	if db.Path == ":memory:" || db.W.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode) != nil || mode != 2 {
		return 0, db.Vacuum(ctx)
	}
	return db.IncrementalVacuum(ctx, nil)
}

// IncrementalVacuum returns free pages to the file system in small steps.
// Databases created before v0.3 have auto_vacuum disabled; they are
// converted first with a one-off VACUUM (which rebuilds the file and blocks
// writes while it runs).
func (db *DB) IncrementalVacuum(ctx context.Context, progress func(done, total int64)) (int64, error) {
	if db.Path == ":memory:" {
		return 0, nil
	}
	before := db.Size()
	var mode int64
	if err := db.W.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return 0, err
	}
	if mode != 2 {
		if _, err := db.W.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
			return 0, err
		}
		if _, err := db.W.ExecContext(ctx, `VACUUM`); err != nil {
			return 0, err
		}
	} else {
		var free int64
		_ = db.W.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free)
		var done int64
		for free > 0 {
			step := min(free, 2000) // 2000 pages = 8 MB per statement
			if _, err := db.W.ExecContext(ctx, `PRAGMA incremental_vacuum(`+strconv.FormatInt(step, 10)+`)`); err != nil {
				return 0, err
			}
			done += step
			free -= step
			if progress != nil {
				progress(done, done+free)
			}
			select {
			case <-ctx.Done():
				return before - db.Size(), ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	_, _ = db.W.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return before - db.Size(), nil
}

// Vacuum checkpoints the WAL after large deletions so the -wal file shrinks.
// Freed pages are reused by SQLite; the main file does not shrink.
func (db *DB) Vacuum(ctx context.Context) error {
	_, err := db.W.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}
