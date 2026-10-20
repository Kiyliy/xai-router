// Package reqlog records request logs asynchronously, sanitises what is
// stored (secret headers, huge base64 payloads, size caps) and enforces log
// retention.
package reqlog

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/store"
)

// Writer batches logs into the database from a single goroutine.
//
// Each log also carries usage accounting (token spend, key spend, hourly
// stats), so logs are never silently dropped:
//   - when the queue is full or the queued bodies exceed the memory budget,
//     the bodies are discarded but the record (and its accounting) is kept;
//   - when the database is busy, locked, out of time or out of disk space
//     (a long VACUUM, a held write lock, an I/O error), the rows are kept in
//     memory and retried with backoff instead of being dropped;
//   - when more than maxRecords rows are waiting, further rows are reduced
//     to accounting-only stubs (a few hundred bytes each), so counters and
//     stats survive a long stall ahead of log rows and bodies;
//   - a batch that fails for another reason is retried row by row, and a row
//     that still fails is stored as an accounting-only update.
type Writer struct {
	db       *store.DB
	settings *config.Holder
	log      *slog.Logger
	ch       chan *store.Log
	dropped  atomic.Int64 // bodies discarded because of back-pressure
	failed   atomic.Int64 // logs whose row could not be stored (accounting kept)
	lost     atomic.Int64 // logs whose accounting could not be stored at all
	bytes    atomic.Int64 // body bytes currently queued or retained
	wg       sync.WaitGroup

	mu       sync.Mutex
	overflow []*store.Log // body-less logs that did not fit in the channel
	pending  []*store.Log // rows kept after a temporary failure, oldest first
	retryAt  time.Time    // next attempt at pending rows
	backoff  time.Duration
	stopped  bool // Run is draining: late logs are written synchronously
	closed   bool // the database is closed: late logs cannot be stored

	// Tunables (tests shorten them).
	batchTimeout time.Duration // one batch insert
	rowTimeout   time.Duration // each fallback step (one row)
	minBackoff   time.Duration
	maxBackoff   time.Duration
	drainBudget  time.Duration // how long shutdown keeps retrying pending rows
	maxRecords   int           // full rows waiting (overflow + pending) before stubs

	// OnFlush is called after each successful batch (used to refresh caches).
	OnFlush func(n int)
}

const (
	// maxQueuedBodyBytes bounds memory used by bodies waiting to be written.
	maxQueuedBodyBytes = 256 << 20
	// maxWaitingRecords bounds the full log rows kept in memory outside the
	// channel (overflow and retries); beyond it rows become accounting stubs.
	maxWaitingRecords = 50_000
	maxBatch          = 200
)

// NewWriter creates a writer with the given queue capacity.
func NewWriter(db *store.DB, settings *config.Holder, log *slog.Logger, queue int) *Writer {
	return &Writer{db: db, settings: settings, log: log, ch: make(chan *store.Log, queue),
		batchTimeout: 30 * time.Second, rowTimeout: 10 * time.Second,
		minBackoff: 500 * time.Millisecond, maxBackoff: 30 * time.Second,
		drainBudget: 20 * time.Second, maxRecords: maxWaitingRecords}
}

func bodySize(l *store.Log) int64 {
	return int64(len(l.ReqBody) + len(l.RespBody) + len(l.RespText) + len(l.ReasoningText) + len(l.ReqHeaders) + len(l.RespHeaders))
}

func sizeOf(logs []*store.Log) int64 {
	var n int64
	for _, l := range logs {
		n += bodySize(l)
	}
	return n
}

func stripBodies(l *store.Log) {
	if l.HasBodies {
		l.ReqBody = "<body discarded: log queue under pressure>"
		l.RespBody, l.RespText, l.ReasoningText = "", "", ""
		l.ReqHeaders, l.RespHeaders = "{}", "{}"
		l.Truncated = true
	}
}

// compact reduces a log to what its usage accounting needs: no row, no
// bodies. It returns the change in queued body bytes.
func (w *Writer) compact(l *store.Log) int64 {
	if l.NoRecord {
		return 0
	}
	before := bodySize(l)
	l.NoRecord, l.HasBodies = true, false
	l.ReqHeaders, l.ReqBody, l.RespHeaders, l.RespBody, l.RespText, l.ReasoningText = "", "", "", "", "", ""
	l.Query, l.UserAgent, l.Extra, l.PriceSnapshot = "", "", "", ""
	l.Error = truncate(l.Error, 200)
	for i := range l.Attempts {
		l.Attempts[i].Error = truncate(l.Attempts[i].Error, 200)
	}
	w.failed.Add(1)
	return -before
}

// Enqueue submits a log without blocking the request path.
func (w *Writer) Enqueue(l *store.Log) {
	capLog(l)
	w.mu.Lock()
	if w.stopped { // shutting down: nobody drains the queue any more
		closed := w.closed
		w.mu.Unlock()
		if closed {
			w.failed.Add(1)
			w.lost.Add(1)
			w.log.Error("usage accounting lost: log arrived after the database was closed",
				"request_id", l.RequestID, "token_id", l.TokenID, "cost_ticks", l.CostTicks)
			return
		}
		w.writeSync(l)
		return
	}
	// The stopped check and the send happen under w.mu, so Run (which sets
	// stopped under the same lock before its final drain) sees every log.
	defer w.mu.Unlock()
	if w.bytes.Load()+bodySize(l) > maxQueuedBodyBytes {
		stripBodies(l)
		w.dropped.Add(1)
	}
	size := bodySize(l)
	select {
	case w.ch <- l:
		w.bytes.Add(size)
		return
	default:
	}
	// Channel full: keep the record (and its accounting) without bodies,
	// and only the accounting once too many rows are waiting.
	stripBodies(l)
	w.dropped.Add(1)
	if len(w.overflow)+len(w.pending) >= w.maxRecords {
		w.compact(l)
	}
	w.bytes.Add(bodySize(l))
	w.overflow = append(w.overflow, l)
}

// writeSync stores one late log during shutdown, retrying briefly while the
// database is busy.
func (w *Writer) writeSync(l *store.Log) {
	deadline := time.Now().Add(w.drainBudget)
	wait := w.minBackoff
	for {
		if len(w.write([]*store.Log{l})) == 0 {
			return
		}
		if time.Now().Add(wait).After(deadline) {
			w.lose([]*store.Log{l}, "database still busy at shutdown")
			return
		}
		time.Sleep(wait)
		wait = min(2*wait, w.maxBackoff)
	}
}

func (w *Writer) lose(logs []*store.Log, why string) {
	for _, l := range logs {
		if !l.NoRecord {
			w.failed.Add(1)
		}
		w.lost.Add(1)
		w.log.Error("usage accounting lost", "reason", why, "request_id", l.RequestID, "token_id", l.TokenID,
			"cost_ticks", l.CostTicks)
	}
}

// QueueLen returns the number of pending logs (queued and retained).
func (w *Writer) QueueLen() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.ch) + len(w.overflow) + len(w.pending)
}

// Dropped returns how many logs lost their bodies because of back-pressure
// or could not be stored as a row (see Lost for those whose usage could not
// be accounted either).
func (w *Writer) Dropped() int64 { return w.dropped.Load() + w.failed.Load() }

// Lost returns how many logs' usage accounting could not be stored
// (permanent errors, or the database still unavailable at shutdown).
func (w *Writer) Lost() int64 { return w.lost.Load() }

// temporary reports whether a write error may go away on its own: the write
// connection is busy (a long VACUUM, a held lock), the step ran out of time,
// or the disk is full or failing. Such rows are kept and retried.
func temporary(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	s := strings.ToLower(err.Error())
	for _, m := range []string{"sqlite_busy", "database is locked", "database table is locked", "sqlite_locked",
		"interrupted", "disk is full", "sqlite_full", "disk i/o error", "sqlite_ioerr", "deadline exceeded"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func (w *Writer) insert(logs []*store.Log, d time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return w.db.InsertLogs(ctx, logs)
}

// write stores a batch, falling back to one row at a time and finally to
// accounting only. Each step has its own timeout. It returns the logs that
// failed temporarily; the caller keeps and retries them. A failed insert is
// rolled back as a whole, so a retry never counts usage twice.
func (w *Writer) write(batch []*store.Log) (retry []*store.Log) {
	err := w.insert(batch, w.batchTimeout)
	if err == nil {
		if w.OnFlush != nil {
			w.OnFlush(len(batch))
		}
		return nil
	}
	if temporary(err) {
		w.log.Warn("write request logs: database unavailable, keeping them for a retry", "count", len(batch), "err", err)
		return batch
	}
	if len(batch) == 1 {
		w.log.Error("write request log", "err", err)
	} else {
		w.log.Error("write request logs; retrying one by one", "count", len(batch), "err", err)
	}
	for _, l := range batch {
		if len(batch) > 1 {
			err := w.insert([]*store.Log{l}, w.rowTimeout)
			if err == nil {
				continue
			}
			if temporary(err) {
				retry = append(retry, l)
				continue
			}
		}
		if !l.NoRecord {
			l.NoRecord = true
			w.failed.Add(1)
		}
		err := w.insert([]*store.Log{l}, w.rowTimeout)
		switch {
		case err == nil:
		case temporary(err):
			retry = append(retry, l)
		default:
			w.lost.Add(1)
			w.log.Error("usage accounting lost", "request_id", l.RequestID, "token_id", l.TokenID,
				"cost_ticks", l.CostTicks, "err", err)
		}
	}
	return retry
}

// retain adds logs to the retry list, reducing them to accounting stubs
// beyond maxRecords. Called with w.mu held.
func (w *Writer) retain(logs []*store.Log) {
	for _, l := range logs {
		if len(w.overflow)+len(w.pending) >= w.maxRecords {
			w.bytes.Add(w.compact(l))
		}
		w.pending = append(w.pending, l)
	}
}

// writePending writes retained rows in batches until they are all stored or
// one batch fails temporarily (then backs off). Unless force is set it does
// nothing before the backoff has elapsed. It reports whether rows remain.
func (w *Writer) writePending(force bool) bool {
	for {
		w.mu.Lock()
		if len(w.pending) == 0 {
			w.pending = nil
			w.mu.Unlock()
			return false
		}
		if !force && time.Now().Before(w.retryAt) {
			w.mu.Unlock()
			return true
		}
		n := min(maxBatch, len(w.pending))
		chunk := append([]*store.Log(nil), w.pending[:n]...)
		w.mu.Unlock()

		size := sizeOf(chunk)
		retry := w.write(chunk)
		w.bytes.Add(sizeOf(retry) - size)

		w.mu.Lock()
		if len(retry) == 0 {
			w.pending = w.pending[n:]
			w.backoff = 0
			w.mu.Unlock()
			continue
		}
		w.pending = append(retry, w.pending[n:]...)
		w.backoff = min(max(2*w.backoff, w.minBackoff), w.maxBackoff)
		w.retryAt = time.Now().Add(w.backoff)
		w.mu.Unlock()
		return true
	}
}

// Run consumes the queue until ctx is cancelled and the queue is drained.
func (w *Writer) Run(ctx context.Context) {
	w.wg.Add(1)
	defer w.wg.Done()
	batch := make([]*store.Log, 0, maxBatch)
	flush := func() {
		w.mu.Lock()
		over := w.overflow
		w.overflow = nil
		w.retain(batch)
		w.retain(over)
		w.mu.Unlock()
		batch = batch[:0]
		w.writePending(false)
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case l := <-w.ch:
			batch = append(batch, l)
			if len(batch) >= maxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			w.mu.Lock()
			w.stopped = true // late logs are written synchronously
			w.mu.Unlock()
		drain:
			for {
				select {
				case l := <-w.ch:
					batch = append(batch, l)
				default:
					break drain
				}
			}
			w.mu.Lock()
			over := w.overflow
			w.overflow = nil
			w.retain(batch)
			w.retain(over)
			w.mu.Unlock()
			w.drainPending()
			return
		}
	}
}

// drainPending writes the retained rows at shutdown, retrying for up to
// drainBudget while the database stays busy.
func (w *Writer) drainPending() {
	deadline := time.Now().Add(w.drainBudget)
	for w.writePending(true) {
		w.mu.Lock()
		wait := w.backoff
		w.mu.Unlock()
		if time.Now().Add(wait).After(deadline) {
			w.mu.Lock()
			left := w.pending
			w.pending = nil
			w.mu.Unlock()
			w.lose(left, "database still unavailable at shutdown")
			return
		}
		time.Sleep(wait)
	}
}

// Wait blocks until Run has returned.
func (w *Writer) Wait() { w.wg.Wait() }

// Close marks the database as closed: logs that arrive later (a request that
// outlived the shutdown budget) are reported as lost instead of being
// written to a closed database. Call it after Run has returned.
func (w *Writer) Close() {
	w.mu.Lock()
	w.stopped, w.closed = true, true
	w.mu.Unlock()
}

// Flush synchronously writes everything currently queued (tests, shutdown).
func (w *Writer) Flush(ctx context.Context) error {
	var batch []*store.Log
	for {
		select {
		case l := <-w.ch:
			batch = append(batch, l)
		default:
			w.mu.Lock()
			batch = append(batch, w.pending...)
			batch = append(batch, w.overflow...)
			w.pending, w.overflow = nil, nil
			w.mu.Unlock()
			if len(batch) == 0 {
				return nil
			}
			err := w.db.InsertLogs(ctx, batch)
			if err == nil {
				w.bytes.Add(-sizeOf(batch))
			}
			return err
		}
	}
}

// ---- size caps ----

const (
	maxPathBytes    = 2048
	maxQueryBytes   = 2048
	maxHeaderBytes  = 16 << 10
	maxHeaderValue  = 1024
	maxErrorBytes   = 4096
	refusedPathCap  = 256
	refusedQueryCap = 256
	refusedUACap    = 200
)

// truncate cuts s to at most n bytes on a UTF-8 boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// refusedBeforeAuth reports whether a log is a request the gateway refused
// before a client token was accepted: no token, no upstream attempt, and the
// status of a failed authentication (missing, invalid, disabled or expired
// key, or no quota left).
func refusedBeforeAuth(l *store.Log) bool {
	return l.TokenID == 0 && len(l.Attempts) == 0 &&
		(l.Status == http.StatusUnauthorized || l.Status == http.StatusForbidden || l.Status == http.StatusTooManyRequests)
}

// capLog bounds what one log stores. A request refused before
// authentication (see refusedBeforeAuth) is reduced to a
// summary: method, a short path and query, status, error, IP and user agent;
// no headers or bodies. Anyone can send such requests, so they must not be
// able to fill the disk; the refusal itself is still logged and counted.
func capLog(l *store.Log) {
	if refusedBeforeAuth(l) {
		l.Path = truncate(l.Path, refusedPathCap)
		l.Query = truncate(l.Query, refusedQueryCap)
		l.UserAgent = truncate(l.UserAgent, refusedUACap)
		l.Error = truncate(l.Error, 512)
		l.HasBodies = false
		l.ReqHeaders, l.ReqBody, l.RespHeaders, l.RespBody, l.RespText, l.ReasoningText = "", "", "", "", "", ""
		return
	}
	l.Path = truncate(l.Path, maxPathBytes)
	l.Query = truncate(l.Query, maxQueryBytes)
	l.Error = truncate(l.Error, maxErrorBytes)
	l.ReqHeaders = capHeaders(l.ReqHeaders)
	l.RespHeaders = capHeaders(l.RespHeaders)
}

// capHeaders bounds a headers JSON object: long values are cut and, past
// maxHeaderBytes, the remaining headers are left out (and counted).
func capHeaders(s string) string {
	if len(s) <= maxHeaderBytes {
		return s
	}
	var m map[string]string
	if json.Unmarshal([]byte(s), &m) != nil {
		return `{"X-Router-Note":"headers not stored (` + itoa(len(s)) + ` bytes)"}`
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]string, len(m))
	used, omitted := 0, 0
	for _, k := range keys {
		k2, v := truncate(k, 128), m[k]
		if len(v) > maxHeaderValue {
			v = truncate(v, maxHeaderValue) + "…[" + itoa(len(m[k])) + " bytes]"
		}
		if used+len(k2)+len(v)+8 > maxHeaderBytes-128 {
			omitted++
			continue
		}
		out[k2] = v
		used += len(k2) + len(v) + 8
	}
	if omitted > 0 {
		out["X-Router-Note"] = itoa(omitted) + " headers omitted (" + itoa(len(s)) + " bytes in total)"
	}
	b, _ := json.Marshal(out)
	return string(b)
}

// ---- sanitising ----

var secretHeaders = map[string]bool{
	"authorization": true, "x-api-key": true, "cookie": true, "set-cookie": true,
	"proxy-authorization": true, "x-goog-api-key": true,
}

// MaskSecret keeps a short prefix and suffix of a secret.
func MaskSecret(s string) string {
	s = strings.TrimSpace(s)
	scheme := ""
	if i := strings.IndexByte(s, ' '); i > 0 && i < 10 {
		scheme, s = s[:i+1], s[i+1:]
	}
	if len(s) <= 12 {
		return scheme + "****"
	}
	return scheme + s[:6] + "…" + s[len(s)-4:]
}

// HeadersJSON serialises headers with secrets masked.
func HeadersJSON(h http.Header) string {
	m := make(map[string]string, len(h))
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := strings.Join(h[k], ", ")
		if secretHeaders[strings.ToLower(k)] {
			v = MaskSecret(v)
		} else if strings.EqualFold(k, "Sec-Websocket-Protocol") && strings.Contains(v, "xai-client-secret.") {
			v = maskProtocols(v)
		}
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// maskProtocols masks credentials passed as WebSocket subprotocols.
func maskProtocols(v string) string {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if rest, ok := strings.CutPrefix(p, "xai-client-secret."); ok {
			p = "xai-client-secret." + MaskSecret(rest)
		}
		parts[i] = p
	}
	return strings.Join(parts, ", ")
}

// SanitizeBody shrinks a body for storage: long base64 runs (inline images,
// audio, data URIs) are replaced with a placeholder and the result is capped
// at maxBytes. It reports whether anything was removed.
func SanitizeBody(b []byte, maxBytes int) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	if !utf8.Valid(b) {
		return "<binary body, " + itoa(len(b)) + " bytes>", true
	}
	s, changed := string(b), false
	if len(s) > minBase64Run {
		s, changed = stripBase64(s)
	}
	if maxBytes > 0 && len(s) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "\n…[truncated, " + itoa(len(b)) + " bytes total]"
		changed = true
	}
	return s, changed
}

const (
	minBase64Run  = 1024 // bare base64 string values at least this long are removed
	minDataURIRun = 256  // data:...;base64, payloads at least this long are removed
)

func isB64(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' ||
		c == '-' || c == '_'
}

// stripBase64 replaces base64 payloads that follow "base64," or form a whole
// JSON string value.
func stripBase64(s string) (string, bool) {
	var out strings.Builder
	changed := false
	last := 0
	for i := 0; i < len(s); i++ {
		threshold := 0
		switch {
		case s[i] == ',' && i >= 6 && s[i-6:i] == "base64":
			threshold = minDataURIRun
		case s[i] == '"':
			threshold = minBase64Run
		default:
			continue
		}
		j := i + 1
		for j < len(s) && isB64(s[j]) {
			j++
		}
		k := j
		for k < len(s) && k-j < 2 && s[k] == '=' {
			k++
		}
		run := j - (i + 1)
		ends := threshold == minDataURIRun || (k < len(s) && s[k] == '"')
		if run >= threshold && ends {
			if !changed {
				out.Grow(len(s) / 4)
			}
			out.WriteString(s[last : i+1])
			out.WriteString("<base64 " + itoa(k-i-1) + " bytes omitted>")
			last, changed = k, true
			i = k - 1
		}
	}
	if !changed {
		return s, false
	}
	out.WriteString(s[last:])
	return out.String(), true
}

func itoa(n int) string { return strconv.Itoa(n) }

// ---- retention ----

// Janitor periodically deletes old logs and expired bindings/sessions.
type Janitor struct {
	DB       *store.DB
	Settings *config.Holder
	Log      *slog.Logger
	Interval time.Duration
}

// Run loops until ctx is cancelled.
func (j *Janitor) Run(ctx context.Context) {
	if j.Interval == 0 {
		j.Interval = time.Hour
	}
	t := time.NewTimer(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.RunOnce(ctx)
			t.Reset(j.Interval)
		}
	}
}

// RunOnce performs one cleanup pass.
func (j *Janitor) RunOnce(ctx context.Context) {
	s := j.Settings.Get()
	if days := s.LogRetentionDays; days > 0 {
		cut := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
		if j.DB.HasLogsBefore(ctx, cut) {
			j.retention(ctx, days, cut)
		}
	}
	if n, err := j.DB.PurgeExpiredBindings(ctx); err != nil {
		j.Log.Error("purge bindings", "err", err)
	} else if n > 0 {
		j.Log.Debug("purged expired bindings", "count", n)
	}
	if err := j.DB.PurgeExpiredSessions(ctx); err != nil {
		j.Log.Error("purge sessions", "err", err)
	}
}

// retention deletes request logs older than the retention period, recorded
// as a start and an end event (the end with what it deleted, or the error).
func (j *Janitor) retention(ctx context.Context, days int, cut int64) {
	start := time.Now()
	before := time.UnixMilli(cut).UTC().Format(time.RFC3339)
	_ = j.DB.AddEvent(ctx, &store.Event{Kind: store.EventSystem, Action: "logs.retention_start", Actor: "system",
		ActorName: "system", TargetType: "logs", Detail: map[string]any{"retention_days": days, "before": before}})
	n, err := j.DB.DeleteLogsBefore(ctx, cut)
	d := map[string]any{"deleted": n, "retention_days": days, "before": before, "duration_ms": time.Since(start).Milliseconds()}
	ev := &store.Event{Kind: store.EventSystem, Action: "logs.retention_end", Actor: "system", ActorName: "system",
		TargetType: "logs", Detail: d}
	if err != nil {
		j.Log.Error("log retention", "err", err)
		ev.Level, d["error"] = store.LevelError, err.Error()
		ctx = context.WithoutCancel(ctx) // record the interrupted run even when shutting down
	} else {
		j.Log.Info("deleted old request logs", "count", n, "retention_days", days)
		defer func() { _ = j.DB.Vacuum(ctx) }()
	}
	_ = j.DB.AddEvent(ctx, ev)
}
