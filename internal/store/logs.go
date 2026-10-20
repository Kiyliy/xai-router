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
	Action    string // success, retry, return, cooldown, disable, verify
	TTFBMs    int64
	LatencyMs int64
	StartedAt int64

	// The detail of an attempt that was retried, for its own log row (see
	// rowsOf). LogStatus is the status that row records: the HTTP status the
	// failure stands for (an in-stream error after a 200 is its matching
	// error status, a network error 502 or 504); 0 = Status.
	LogStatus   int
	RespHeaders string
	RespBody    string
	CostTicks   int64 // what the upstream reported this failed attempt cost (normally 0)
}

// Log is one request_logs row. A client request is written as one Log:
// InsertLogs stores each of its upstream attempts as its own row (see
// rowsOf), the Log itself becoming the final one. Read back, a Log is one
// row.
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
	TokenPrefix      string // the token value's prefix when the request was made
	CacheWriteTokens *int64 // nil: the upstream did not report it
	PriceSnapshot    string // JSON: the prices the request was billed at
	KeyID            int64  // 0 = none
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

	// Detail, stored in request_log_bodies (and in log_attempts for
	// WebSocket sessions and rows written before v0.6.0).
	ReqHeaders    string
	ReqBody       string
	RespHeaders   string
	RespBody      string
	RespText      string
	ReasoningText string
	// Attempts are the upstream attempts of the client request, in order.
	// For an HTTP request each becomes its own row (the last one is this
	// Log); a WebSocket session stays one row with its dial attempts nested.
	// Read back, they are the nested attempts of a pre-v0.6.0 row or a
	// WebSocket session.
	Attempts     []Attempt
	AttemptCount int // filled when reading: the attempts column
	HasBodies    bool
	WS           bool
	Extra        string // JSON with endpoint-specific details (WebSocket session stats, stream errors)
	// NoRecord applies the usage accounting (key/token counters, hourly
	// stats) without storing the log row itself (logging disabled).
	NoRecord bool

	// Filled when reading: the row's place among the rows sharing its
	// request_id. Rows before v0.6.0 and single attempts are 1 of 1.
	AttemptIndex  int
	AttemptTotal  int
	AttemptFinal  bool
	AttemptAction string // what the pool did after this row's attempt ("" before v0.6.0)
	// StartedAt is when the row's attempt started (read: created_at for
	// older rows). CreatedAt is the list order: for the rows of a chain the
	// end of the client request (see rowsOf), else the request's start.
	StartedAt int64
	// EndAt is when the client request ended (writing; 0 = CreatedAt +
	// LatencyMs).
	EndAt int64
	// ReqBodyFrom is the row the request body was read from when it is not
	// this one (GetLog: the body is stored on the first row of a chain).
	ReqBodyFrom int64
}

// AttemptState is "single" (the only attempt), "final" (the last of
// several) or "retried" (an earlier attempt that failed and was retried).
func (l *Log) AttemptState() string {
	switch {
	case l.AttemptTotal <= 1:
		return "single"
	case l.AttemptFinal:
		return "final"
	}
	return "retried"
}

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

// keyUse is what one row adds to an upstream key's counters.
type keyUse struct {
	keyID   int64
	isErr   bool
	lastErr string
	cost    int64
	at      int64
}

// logRow is one request_logs row of a client request.
type logRow struct {
	l         *Log // the row (a copy, except for a single row)
	index     int
	total     int
	action    string
	attempts  int       // the attempts column
	nested    []Attempt // written to log_attempts (WebSocket sessions)
	startedAt int64     // when the row's attempt started
	keys      []keyUse
}

// failedAttempt reports whether an attempt's action counts as a key error.
func failedAttempt(action string) bool { return action != "success" && action != "return" }

// rowsOf expands a client request into its rows. An HTTP request with n
// upstream attempts is n rows sharing the request id: attempts 1..n-1 are
// "retried" rows built from l.Attempts (their own key, status, error, timing
// and response; no usage, and cost 0 unless the upstream reported one), and
// the last is l itself, timed from the start of its attempt and carrying the
// usage and cost. The request body is kept on the first row only.
//
// The rows of a chain share created_at, the list's sort time: one
// millisecond past the end of the client request. Listed newest first
// (created_at DESC, id DESC, ids in attempt order) a chain is contiguous,
// its final row on top, and the events it caused (a cooldown, an
// auto-disable, written before it ended) sort below it. Each row's own start
// is started_at. A WebSocket session, or a request without attempts, is a
// single row as before v0.6.0, created at its start. l is not modified, so a
// failed batch can be expanded again.
func rowsOf(l *Log) []logRow {
	n := len(l.Attempts)
	if l.WS || n <= 1 {
		r := logRow{l: l, index: 1, total: 1, attempts: n, nested: l.Attempts, startedAt: l.CreatedAt}
		if !l.WS {
			r.nested = nil // an HTTP request's attempt is the row itself
		}
		if n > 0 {
			r.action = l.Attempts[n-1].Action
		}
		if !l.WS && n == 1 {
			a := l.Attempts[0]
			if l.KeyID == 0 && a.KeyID != 0 { // the row's own key, also when the client left early
				c := *l
				c.KeyID, c.KeyName = a.KeyID, a.KeyName
				r.l = &c
			}
			if r.l.KeyID != 0 {
				u := keyUse{keyID: r.l.KeyID, cost: l.CostTicks, at: a.StartedAt}
				if failedAttempt(a.Action) {
					u.isErr, u.lastErr = true, a.Error
				}
				r.keys = []keyUse{u}
			}
			return []logRow{r}
		}
		// Each dial attempt counts on its key; the session's cost goes to
		// the key that served it.
		for _, a := range l.Attempts {
			if a.KeyID == 0 {
				continue
			}
			u := keyUse{keyID: a.KeyID, at: a.StartedAt}
			if failedAttempt(a.Action) {
				u.isErr, u.lastErr = true, a.Error
			}
			if a.Action == "success" || (a.Action == "return" && a.KeyID == l.KeyID) {
				u.cost = l.CostTicks
			}
			r.keys = append(r.keys, u)
		}
		return []logRow{r}
	}
	use := func(row *Log, a Attempt, at int64) []keyUse {
		if row.KeyID == 0 {
			return nil
		}
		u := keyUse{keyID: row.KeyID, cost: row.CostTicks, at: at}
		if failedAttempt(a.Action) {
			u.isErr, u.lastErr = true, a.Error
		}
		return []keyUse{u}
	}
	end := l.EndAt
	if end == 0 {
		end = l.CreatedAt + l.LatencyMs
	}
	sortAt := max(end, l.CreatedAt+l.LatencyMs) + 1
	rows := make([]logRow, 0, n)
	for i, a := range l.Attempts[:n-1] {
		start := a.StartedAt
		if start == 0 {
			start = l.CreatedAt
		}
		status := a.LogStatus
		if status == 0 {
			status = a.Status
		}
		if status == 0 {
			status = http.StatusBadGateway
		}
		r := &Log{RequestID: l.RequestID, CreatedAt: sortAt, Method: l.Method, Path: l.Path, Query: l.Query,
			Model: l.Model, Stream: l.Stream, Status: status, Error: a.Error, ClientIP: l.ClientIP,
			UserAgent: l.UserAgent, TokenID: l.TokenID, TokenName: l.TokenName, TokenPrefix: l.TokenPrefix,
			KeyID: a.KeyID, KeyName: a.KeyName, TTFBMs: a.TTFBMs, LatencyMs: a.LatencyMs, CostTicks: a.CostTicks,
			CostSource: "none", HasBodies: l.HasBodies, NoRecord: l.NoRecord}
		if a.CostTicks > 0 {
			r.CostSource = "upstream"
		}
		if r.HasBodies {
			r.ReqHeaders, r.RespHeaders, r.RespBody = l.ReqHeaders, a.RespHeaders, a.RespBody
			if r.RespHeaders == "" {
				r.RespHeaders = "{}"
			}
			if i == 0 {
				r.ReqBody, r.Truncated = l.ReqBody, l.Truncated
			}
		}
		rows = append(rows, logRow{l: r, index: i + 1, total: n, action: a.Action, attempts: 1, startedAt: start,
			keys: use(r, a, start)})
	}
	last := l.Attempts[n-1]
	f := *l
	f.Attempts = nil
	f.ReqBody = ""
	f.CreatedAt = sortAt
	start := l.CreatedAt
	if last.StartedAt > l.CreatedAt {
		start = last.StartedAt
		off := last.StartedAt - l.CreatedAt
		f.LatencyMs = max(l.LatencyMs-off, 0)
		if f.TTFBMs > 0 {
			f.TTFBMs = max(l.TTFBMs-off, 0)
		}
	}
	if f.KeyID == 0 {
		f.KeyID, f.KeyName = last.KeyID, last.KeyName
	}
	rows = append(rows, logRow{l: &f, index: n, total: n, action: last.Action, attempts: 1, startedAt: start,
		keys: use(&f, last, start)})
	return rows
}

// InsertLogs writes a batch of client requests (each as one or more rows,
// see rowsOf) in a single transaction together with the derived counters:
// key and token usage and the hourly aggregates, all counted per row.
func (db *DB) InsertLogs(ctx context.Context, logs []*Log) error {
	tx, err := db.W.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	insLog, err := tx.PrepareContext(ctx, `INSERT INTO request_logs (request_id, created_at, method, path, query, model,
stream, status, error, client_ip, user_agent, token_id, token_name, key_id, key_name, attempts, ttfb_ms, latency_ms,
prompt_tokens, completion_tokens, cached_tokens, reasoning_tokens, image_tokens, total_tokens, images, video_seconds,
cost_ticks, cost_source, upstream_id, truncated, ws, extra, token_prefix, cache_write_tokens, price_snapshot,
attempt_index, attempt_total, attempt_final, attempt_action, started_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
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
	updKey, err := tx.PrepareContext(ctx, `UPDATE keys SET attempts = attempts + ?, attempt_errors = attempt_errors + ?,
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

	for _, cl := range logs {
		for _, r := range rowsOf(cl) {
			l := r.l
			if !cl.NoRecord {
				res, err := insLog.ExecContext(ctx, l.RequestID, l.CreatedAt, l.Method, l.Path, l.Query, l.Model,
					boolInt(l.Stream), l.Status, l.Error, l.ClientIP, l.UserAgent, nullID(l.TokenID), l.TokenName,
					nullID(l.KeyID), l.KeyName, r.attempts, l.TTFBMs, l.LatencyMs, l.PromptTokens, l.CompletionTokens,
					l.CachedTokens, l.ReasoningTokens, l.ImageTokens, l.TotalTokens, l.Images, l.VideoSeconds, l.CostTicks,
					l.CostSource, l.UpstreamID, boolInt(l.Truncated), boolInt(l.WS), l.Extra, l.TokenPrefix,
					l.CacheWriteTokens, l.PriceSnapshot, r.index, r.total, boolInt(r.index == r.total), r.action, r.startedAt)
				if err != nil {
					return err
				}
				id, _ := res.LastInsertId()
				if r.index == r.total {
					cl.ID = id // the row holding the client request's outcome
				}
				if l.HasBodies {
					if _, err := insBody.ExecContext(ctx, id, l.ReqHeaders, l.ReqBody, l.RespHeaders, l.RespBody, l.RespText,
						l.ReasoningText); err != nil {
						return err
					}
				}
				for _, a := range r.nested {
					if _, err := insAtt.ExecContext(ctx, id, a.Seq, nullID(a.KeyID), a.KeyName, a.Status, a.Error, a.Action,
						a.TTFBMs, a.LatencyMs, a.StartedAt); err != nil {
						return err
					}
				}
			}
			if err := account(ctx, r, updKey, updToken, upStats); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// account applies one row's usage to the key and token counters and the
// hourly aggregates: each row is a request in the stats (under its own key)
// and on its token, and an attempt on its key.
func account(ctx context.Context, r logRow, updKey, updToken, upStats *sql.Stmt) error {
	l := r.l
	for _, u := range r.keys {
		if _, err := updKey.ExecContext(ctx, 1, boolInt(u.isErr), u.cost, u.at, u.lastErr, u.lastErr, u.keyID); err != nil {
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
	Problems bool // only warnings and errors (status not 2xx/3xx/101)
	// FinalOnly keeps the last row of each client request (attempts=final):
	// retried attempts are left out.
	FinalOnly bool
	// BeforeAt/BeforeID restrict to logs strictly older than this position
	// in (created_at DESC, id DESC) order: cursor paging (with Offset 0).
	BeforeAt, BeforeID int64
	NoCount            bool // skip the total (later cursor pages)
	Limit              int
	Offset             int
	qIsID              bool // set by ListLogsPage: search Q as a request/upstream id
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
	if f.BeforeAt > 0 {
		w, a = append(w, "(created_at < ? OR (created_at = ? AND id < ?))"), append(a, f.BeforeAt, f.BeforeAt, f.BeforeID)
	}
	if f.FinalOnly {
		w = append(w, "attempt_final = 1")
	}
	if f.Problems && f.Status == "" {
		w = append(w, successSQL+" = 0")
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
cost_source, upstream_id, truncated, ws, extra, token_prefix, cache_write_tokens, price_snapshot, attempt_index,
attempt_total, attempt_final, attempt_action, started_at`

func scanLog(s interface{ Scan(...any) error }) (*Log, error) {
	var l Log
	var stream, trunc, ws, final int
	var cw sql.NullInt64
	err := s.Scan(&l.ID, &l.RequestID, &l.CreatedAt, &l.Method, &l.Path, &l.Query, &l.Model, &stream, &l.Status,
		&l.Error, &l.ClientIP, &l.UserAgent, &l.TokenID, &l.TokenName, &l.KeyID, &l.KeyName, &l.AttemptCount, &l.TTFBMs,
		&l.LatencyMs, &l.PromptTokens, &l.CompletionTokens, &l.CachedTokens, &l.ReasoningTokens, &l.ImageTokens,
		&l.TotalTokens, &l.Images, &l.VideoSeconds, &l.CostTicks, &l.CostSource, &l.UpstreamID, &trunc, &ws, &l.Extra, &l.TokenPrefix, &cw, &l.PriceSnapshot,
		&l.AttemptIndex, &l.AttemptTotal, &final, &l.AttemptAction, &l.StartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	l.Stream, l.Truncated, l.WS, l.AttemptFinal = stream == 1, trunc == 1, ws == 1, final == 1
	if l.StartedAt == 0 {
		l.StartedAt = l.CreatedAt
	}
	if cw.Valid {
		l.CacheWriteTokens = &cw.Int64
	}
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

// countCap bounds an exact count (see listLogsPage).
const countCap = 100_000

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
		// Ids are searched exactly (indexed). If nothing has that id, error
		// texts are searched instead. The mode is decided from the query and
		// filters alone, never from the page (offset or cursor), so every
		// page of one search uses the same mode.
		isID, err := db.idMatches(ctx, f)
		if err != nil {
			return nil, err
		}
		f.qIsID = isID
	}
	return db.listLogsPage(ctx, f)
}

// idMatches reports whether any log matching the filter has f.Q as its
// request or upstream id, ignoring paging (two index lookups).
func (db *DB) idMatches(ctx context.Context, f LogFilter) (bool, error) {
	f.qIsID = true
	f.BeforeAt, f.BeforeID, f.Offset = 0, 0, 0
	w, args := f.where()
	var one int
	err := db.R.QueryRowContext(ctx, `SELECT 1 FROM request_logs`+w+` LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
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
	} else if f.Q == "" {
		// A dimension filter is answered from its covering index (it also
		// holds the status, path and other dimensions). Combined with a
		// status filter the planner would otherwise walk the much larger
		// status index and read a wide row per match. Most selective first.
		switch {
		case f.TokenID != 0:
			from = " FROM request_logs INDEXED BY idx_logs_token_f"
		case f.KeyID != 0:
			from = " FROM request_logs INDEXED BY idx_logs_key_f"
		case f.Model != "":
			from = " FROM request_logs INDEXED BY idx_logs_model_f"
		}
	}
	// Count first, then fetch the page: logs written in between can only
	// add rows, so the total never exceeds what the page shows.
	p := &LogPage{}
	counted := false
	// A time range alone (the purge preview) is counted on the created_at
	// index: rollup estimates can overcount when log ids are not in time
	// order or hours were partly purged before.
	timeOnly := f.Model == "" && f.KeyID == 0 && f.TokenID == 0 && f.Path == "" && f.Q == "" && f.Status == "" &&
		!f.Problems && (f.From > 0 || f.To > 0)
	if f.NoCount {
		counted = true
	} else if est, ok := db.estimateCount(ctx, f); ok && est > largeResult && !timeOnly {
		// A large result is reported from the rollups straight away;
		// counting it exactly would take longer than the page itself.
		p.Total, p.TotalKind, counted = est, "approx", true
	} else {
		// Count at most up to a cap a little past the requested page: a
		// broad filter (an exact status code has no rollup to estimate
		// from) would otherwise scan millions of index entries for a
		// total nobody needs exactly. Reaching the cap reports a lower
		// bound that still lets the pager move on.
		limit := int64(max(countCap, f.Offset+20*f.Limit)) + 1
		if timeOnly {
			limit = 1 << 62
		}
		cctx, cancel := context.WithTimeout(ctx, CountBudget)
		var total int64
		err := db.R.QueryRowContext(cctx, `SELECT COUNT(*) FROM (SELECT 1`+from+w+` LIMIT ?)`,
			append(append([]any(nil), args...), limit)...).Scan(&total)
		cancel()
		switch {
		case err == nil && total == limit:
			p.Total, p.TotalKind, counted = total-1, "at_least", true
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
	if f.Path != "" || f.Q != "" || f.Problems || f.BeforeAt > 0 {
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
	if err != nil {
		return 0, false
	}
	// The id range of the remaining logs bounds every estimate (logs are
	// deleted oldest first), and is the estimate for an unfiltered list:
	// rollups of deleted hours can otherwise be counted again when older
	// logs are imported later.
	var minID, maxID sql.NullInt64
	_ = db.R.QueryRowContext(ctx, `SELECT MIN(id) FROM request_logs`).Scan(&minID)
	_ = db.R.QueryRowContext(ctx, `SELECT MAX(id) FROM request_logs`).Scan(&maxID)
	if maxID.Valid {
		span := maxID.Int64 - minID.Int64 + 1
		if dim == "all" && col == "requests" && f.From <= 0 && f.To <= 0 {
			return span, true
		}
		est = min(est, span)
	}
	return est, true
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
	if l.AttemptIndex > 1 {
		// The request body is stored on the first row of the chain only.
		var from int64
		var body sql.NullString
		err := db.R.QueryRowContext(ctx, `SELECT l.id, b.req_body FROM request_logs l
LEFT JOIN request_log_bodies b ON b.log_id = l.id WHERE l.request_id = ? AND l.attempt_index = 1
ORDER BY l.id LIMIT 1`, l.RequestID).Scan(&from, &body)
		switch {
		case err == nil:
			l.ReqBodyFrom = from
			if body.Valid {
				l.ReqBody = body.String
			}
		case !errors.Is(err, sql.ErrNoRows): // the first row may have been purged
			return nil, err
		}
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

// LogChain returns the rows of the client request requestID (all its
// upstream attempts), in attempt order. It is an index lookup on request_id.
func (db *DB) LogChain(ctx context.Context, requestID string) ([]*Log, error) {
	rows, err := db.R.QueryContext(ctx, `SELECT `+logCols+` FROM request_logs WHERE request_id = ?
ORDER BY attempt_index, started_at, id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Log
	for rows.Next() {
		l, err := scanLog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// DeleteLogsBefore deletes logs created before t (unix ms). Aggregated
// stats are kept.
func (db *DB) DeleteLogsBefore(ctx context.Context, t int64) (int64, error) {
	return db.DeleteLogsBeforeProgress(ctx, t, nil)
}

// CountLogsBefore counts the logs created before t (the created_at index;
// log ids are not always in time order, so an id range is not used).
func (db *DB) CountLogsBefore(ctx context.Context, t int64) int64 {
	var n int64
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_logs WHERE created_at < ?`, t).Scan(&n)
	return n
}

// Purges and space reclaim run in small steps that hold SQLite's single
// write lock only briefly, so request logging and billing are never held
// up: each step's size adapts to keep it near stepTarget, and after each
// step the writer is left free for at least as long as the step took.
const (
	stepTarget   = 10 * time.Millisecond
	purgeMin     = 50
	purgeMax     = 2000
	reclaimMin   = 16  // pages (64 KB)
	reclaimMax   = 512 // pages (2 MB)
	minStepPause = 5 * time.Millisecond
)

// adapt grows or shrinks a step size so a step takes about stepTarget.
func adapt(size, lo, hi int64, took time.Duration) int64 {
	switch {
	case took > 2*stepTarget:
		size /= 2
	case took < stepTarget/2:
		size *= 2
	}
	return min(max(size, lo), hi)
}

// pause waits after a step: at least as long as the step held the lock.
func pause(ctx context.Context, took time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(max(took, minStepPause)):
		return nil
	}
}

// DeleteLogsBeforeProgress deletes logs created before t in small steps,
// reporting progress after each one.
func (db *DB) DeleteLogsBeforeProgress(ctx context.Context, t int64, progress func(deleted int64)) (int64, error) {
	var total int64
	size := int64(200)
	for {
		start := time.Now()
		res, err := db.W.ExecContext(ctx, `DELETE FROM request_logs WHERE id IN
(SELECT id FROM request_logs WHERE created_at < ? ORDER BY created_at LIMIT ?)`, t, size)
		if err != nil {
			return total, err
		}
		took := time.Since(start)
		n, _ := res.RowsAffected()
		total += n
		if progress != nil {
			progress(total)
		}
		if n < size {
			return total, nil
		}
		size = adapt(size, purgeMin, purgeMax, took)
		if err := pause(ctx, took); err != nil {
			return total, err
		}
	}
}

// PageSize returns the database page size in bytes.
func (db *DB) PageSize(ctx context.Context) int64 {
	var n int64
	if db.R.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&n) != nil || n <= 0 {
		return 4096
	}
	return n
}

// IncrementalVacuumIfEnabled reclaims free pages when the database supports
// it without a full rebuild (auto_vacuum = INCREMENTAL); otherwise it only
// checkpoints the WAL.
func (db *DB) IncrementalVacuumIfEnabled(ctx context.Context, progress func(done, total int64)) (int64, error) {
	var mode int64
	if db.Path == ":memory:" || db.W.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode) != nil || mode != 2 {
		return 0, db.Vacuum(ctx)
	}
	return db.IncrementalVacuum(ctx, progress)
}

// IncrementalVacuum returns free pages to the file system in small steps
// (see stepTarget), reporting pages done / total. Databases created before
// v0.3 have auto_vacuum disabled; they are converted first with a one-off
// VACUUM (which rebuilds the file and blocks writes while it runs).
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
		total, done, step := free, int64(0), int64(64)
		for done < total {
			step = min(step, total-done)
			start := time.Now()
			if _, err := db.W.ExecContext(ctx, `PRAGMA incremental_vacuum(`+strconv.FormatInt(step, 10)+`)`); err != nil {
				return before - db.Size(), err
			}
			took := time.Since(start)
			done += step
			if progress != nil {
				progress(done, total)
			}
			step = adapt(step, reclaimMin, reclaimMax, took)
			if err := pause(ctx, took); err != nil {
				return before - db.Size(), err
			}
			if done%(64*1024) < step { // every ~256 MB, keep the WAL small
				_, _ = db.W.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
			}
		}
	}
	_, _ = db.W.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return before - db.Size(), nil
}

// Vacuum checkpoints the WAL after large deletions so the -wal file shrinks.
// Freed pages are reused by SQLite; the main file does not shrink.
func (db *DB) Vacuum(ctx context.Context) error {
	_, err := db.W.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return err
}

// HasLogsBefore reports whether any request log is older than t.
func (db *DB) HasLogsBefore(ctx context.Context, t int64) bool {
	var one int
	return db.R.QueryRowContext(ctx, `SELECT 1 FROM request_logs WHERE created_at < ? LIMIT 1`, t).Scan(&one) == nil
}
