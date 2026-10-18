// Package reqlog records request logs asynchronously, sanitises what is
// stored (secret headers, huge base64 payloads, size caps) and enforces log
// retention.
package reqlog

import (
	"context"
	"encoding/json"
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
// stats), so logs are never silently dropped: when the queue is full or the
// queued bodies exceed the memory budget, the bodies are discarded but the
// record (and its accounting) is kept; failed batches are retried row by row
// and finally as accounting-only updates.
type Writer struct {
	db       *store.DB
	settings *config.Holder
	log      *slog.Logger
	ch       chan *store.Log
	dropped  atomic.Int64 // bodies discarded because of back-pressure
	failed   atomic.Int64 // logs whose row could not be stored (accounting kept)
	bytes    atomic.Int64 // body bytes currently queued
	wg       sync.WaitGroup

	mu       sync.Mutex
	overflow []*store.Log // body-less logs that did not fit in the channel
	stopped  bool

	// OnFlush is called after each successful batch (used to refresh caches).
	OnFlush func(n int)
}

// maxQueuedBodyBytes bounds memory used by bodies waiting to be written.
const maxQueuedBodyBytes = 256 << 20

// NewWriter creates a writer with the given queue capacity.
func NewWriter(db *store.DB, settings *config.Holder, log *slog.Logger, queue int) *Writer {
	return &Writer{db: db, settings: settings, log: log, ch: make(chan *store.Log, queue)}
}

func bodySize(l *store.Log) int64 {
	return int64(len(l.ReqBody) + len(l.RespBody) + len(l.RespText) + len(l.ReasoningText) + len(l.ReqHeaders) + len(l.RespHeaders))
}

func stripBodies(l *store.Log) {
	if l.HasBodies {
		l.ReqBody = "<body discarded: log queue under pressure>"
		l.RespBody, l.RespText, l.ReasoningText = "", "", ""
		l.Truncated = true
	}
}

// Enqueue submits a log without blocking the request path.
func (w *Writer) Enqueue(l *store.Log) {
	w.mu.Lock()
	stopped := w.stopped
	w.mu.Unlock()
	if stopped { // shutting down: nobody drains the queue any more
		w.write([]*store.Log{l})
		return
	}
	if w.bytes.Load()+bodySize(l) > maxQueuedBodyBytes {
		stripBodies(l)
		w.dropped.Add(1)
	}
	size := bodySize(l)
	w.bytes.Add(size)
	select {
	case w.ch <- l:
		return
	default:
	}
	// Channel full: keep the record (and its accounting) without bodies.
	stripBodies(l)
	w.dropped.Add(1)
	w.bytes.Add(bodySize(l) - size)
	w.mu.Lock()
	w.overflow = append(w.overflow, l)
	w.mu.Unlock()
}

// QueueLen returns the number of pending logs.
func (w *Writer) QueueLen() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.ch) + len(w.overflow)
}

// Dropped returns how many logs lost their bodies because of back-pressure
// or could not be stored at all (their usage was still accounted).
func (w *Writer) Dropped() int64 { return w.dropped.Load() + w.failed.Load() }

// write stores a batch, falling back to one row at a time and finally to
// accounting only, so that usage is never lost.
func (w *Writer) write(batch []*store.Log) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := w.db.InsertLogs(ctx, batch); err == nil {
		if w.OnFlush != nil {
			w.OnFlush(len(batch))
		}
		return
	} else if len(batch) == 1 {
		w.log.Error("write request log", "err", err)
	} else {
		w.log.Error("write request logs; retrying one by one", "count", len(batch), "err", err)
	}
	for _, l := range batch {
		if len(batch) > 1 {
			if err := w.db.InsertLogs(ctx, []*store.Log{l}); err == nil {
				continue
			}
		}
		l.NoRecord = true
		w.failed.Add(1)
		if err := w.db.InsertLogs(ctx, []*store.Log{l}); err != nil {
			w.log.Error("usage accounting lost", "request_id", l.RequestID, "token_id", l.TokenID,
				"cost_ticks", l.CostTicks, "err", err)
		}
	}
}

// Run consumes the queue until ctx is cancelled and the queue is drained.
func (w *Writer) Run(ctx context.Context) {
	w.wg.Add(1)
	defer w.wg.Done()
	const maxBatch = 200
	batch := make([]*store.Log, 0, maxBatch)
	flush := func() {
		w.mu.Lock()
		over := w.overflow
		w.overflow = nil
		w.mu.Unlock()
		batch = append(batch, over...)
		if len(batch) == 0 {
			return
		}
		var size int64
		for _, l := range batch {
			size += bodySize(l)
		}
		w.write(batch)
		w.bytes.Add(-size)
		batch = batch[:0]
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
			for {
				select {
				case l := <-w.ch:
					batch = append(batch, l)
					if len(batch) >= maxBatch {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// Wait blocks until Run has returned.
func (w *Writer) Wait() { w.wg.Wait() }

// Flush synchronously writes everything currently queued (tests, shutdown).
func (w *Writer) Flush(ctx context.Context) error {
	var batch []*store.Log
	for {
		select {
		case l := <-w.ch:
			batch = append(batch, l)
		default:
			w.mu.Lock()
			batch = append(batch, w.overflow...)
			w.overflow = nil
			w.mu.Unlock()
			if len(batch) == 0 {
				return nil
			}
			return w.db.InsertLogs(ctx, batch)
		}
	}
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
		start := time.Now()
		n, err := j.DB.DeleteLogsBefore(ctx, cut)
		d := map[string]any{"deleted": n, "retention_days": days, "before": time.UnixMilli(cut).UTC().Format(time.RFC3339),
			"started_at": start.UTC().Format(time.RFC3339), "duration_ms": time.Since(start).Milliseconds()}
		ev := &store.Event{Kind: store.EventSystem, Action: "logs.retention", Actor: "system", ActorName: "system",
			TargetType: "logs", Detail: d}
		if err != nil {
			j.Log.Error("log retention", "err", err)
			ev.Level, d["error"] = store.LevelError, err.Error()
			if ctx.Err() == nil { // not when shutting down
				_ = j.DB.AddEvent(ctx, ev)
			}
		} else if n > 0 {
			j.Log.Info("deleted old request logs", "count", n, "retention_days", days)
			_ = j.DB.AddEvent(ctx, ev)
			_ = j.DB.Vacuum(ctx)
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
