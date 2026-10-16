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
type Writer struct {
	db       *store.DB
	settings *config.Holder
	log      *slog.Logger
	ch       chan *store.Log
	dropped  atomic.Int64
	wg       sync.WaitGroup
	// OnFlush is called after each successful batch (used to refresh caches).
	OnFlush func(n int)
}

// NewWriter creates a writer with the given queue capacity.
func NewWriter(db *store.DB, settings *config.Holder, log *slog.Logger, queue int) *Writer {
	return &Writer{db: db, settings: settings, log: log, ch: make(chan *store.Log, queue)}
}

// Enqueue submits a log without blocking the request path. When the queue is
// full the log is dropped and counted (see Dropped).
func (w *Writer) Enqueue(l *store.Log) {
	select {
	case w.ch <- l:
	default:
		w.dropped.Add(1)
	}
}

// QueueLen returns the number of pending logs.
func (w *Writer) QueueLen() int { return len(w.ch) }

// Dropped returns how many logs were dropped because the queue was full.
func (w *Writer) Dropped() int64 { return w.dropped.Load() }

// Run consumes the queue until ctx is cancelled and the queue is drained.
func (w *Writer) Run(ctx context.Context) {
	w.wg.Add(1)
	defer w.wg.Done()
	const maxBatch = 200
	batch := make([]*store.Log, 0, maxBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := w.db.InsertLogs(fctx, batch)
		cancel()
		if err != nil {
			w.log.Error("write request logs", "count", len(batch), "err", err)
		} else if w.OnFlush != nil {
			w.OnFlush(len(batch))
		}
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
		}
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
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
		n, err := j.DB.DeleteLogsBefore(ctx, cut)
		if err != nil {
			j.Log.Error("log retention", "err", err)
		} else if n > 0 {
			j.Log.Info("deleted old request logs", "count", n, "retention_days", days)
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
