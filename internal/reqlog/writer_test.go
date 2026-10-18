package reqlog

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/store"
)

func testDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestWriter(t *testing.T, db *store.DB, queue int) *Writer {
	return NewWriter(db, config.NewHolder(config.DefaultSettings()), quietLog(), queue)
}

func mkLog(id string, tokenID, cost int64) *store.Log {
	return &store.Log{RequestID: id, CreatedAt: store.NowMs(), Method: "POST", Path: "/v1/chat/completions",
		Status: 200, TokenID: tokenID, CostTicks: cost, HasBodies: true, ReqBody: "req-" + id, RespBody: "resp-" + id,
		ReqHeaders: "{}", RespHeaders: "{}"}
}

func mkToken(t *testing.T, db *store.DB) *store.Token {
	t.Helper()
	tok := &store.Token{Name: "t", Hash: "h-" + t.Name(), Prefix: "p", Enabled: true}
	if err := db.CreateToken(context.Background(), tok); err != nil {
		t.Fatal(err)
	}
	return tok
}

func countLogs(t *testing.T, db *store.DB) int {
	t.Helper()
	_, n, err := db.ListLogs(context.Background(), store.LogFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestWriterRunFlushesAndDrains(t *testing.T) {
	db := testDB(t)
	w := newTestWriter(t, db, 1000)
	var flushed atomic.Int64
	w.OnFlush = func(n int) { flushed.Add(int64(n)) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()

	// The ticker flushes a small batch without waiting for shutdown.
	w.Enqueue(mkLog("first", 0, 0))
	deadline := time.Now().Add(5 * time.Second)
	for countLogs(t, db) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("ticker never flushed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// More than one batch, then shutdown drains everything.
	for i := 0; i < 450; i++ {
		w.Enqueue(mkLog("bulk", 0, 0))
	}
	cancel()
	<-done
	w.Wait()
	if n := countLogs(t, db); n != 451 {
		t.Fatalf("stored %d logs", n)
	}
	if flushed.Load() != 451 {
		t.Fatalf("OnFlush saw %d", flushed.Load())
	}
	if w.QueueLen() != 0 || w.bytes.Load() != 0 {
		t.Fatalf("queue %d bytes %d", w.QueueLen(), w.bytes.Load())
	}
	// After shutdown logs are written synchronously.
	w.Enqueue(mkLog("late", 0, 0))
	if n := countLogs(t, db); n != 452 {
		t.Fatalf("late log not written: %d", n)
	}
}

func TestWriterOverflowDrainedByRun(t *testing.T) {
	db := testDB(t)
	w := newTestWriter(t, db, 1)
	for i := 0; i < 5; i++ {
		w.Enqueue(mkLog("o", 0, 0))
	}
	if w.QueueLen() != 5 || w.Dropped() != 4 {
		t.Fatalf("queue %d dropped %d", w.QueueLen(), w.Dropped())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.Run(ctx) // already cancelled: drains and returns
	if n := countLogs(t, db); n != 5 {
		t.Fatalf("stored %d", n)
	}
	list, _, _ := db.ListLogs(context.Background(), store.LogFilter{Limit: 10})
	stripped := 0
	for _, l := range list {
		full, err := db.GetLog(context.Background(), l.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(full.ReqBody, "<body discarded") && full.RespBody == "" && full.Truncated {
			stripped++
		}
	}
	if stripped != 4 {
		t.Fatalf("%d stripped bodies", stripped)
	}
}

func TestWriterByteBudget(t *testing.T) {
	db := testDB(t)
	w := newTestWriter(t, db, 10)
	w.bytes.Store(maxQueuedBodyBytes) // pretend the budget is used up
	l := mkLog("big", 0, 0)
	w.Enqueue(l)
	if w.Dropped() != 1 || l.RespBody != "" || !l.Truncated {
		t.Fatalf("bodies kept over budget: %+v", l)
	}
	// Logs without bodies are untouched by stripping.
	nb := &store.Log{RequestID: "nb", ReqBody: "x"}
	stripBodies(nb)
	if nb.ReqBody != "x" {
		t.Fatal("body-less log modified")
	}
}

func TestWriterFallsBackToRowsAndAccounting(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	tok := mkToken(t, db)
	if _, err := db.W.ExecContext(ctx, `CREATE TRIGGER fail_bad BEFORE INSERT ON request_logs
WHEN NEW.request_id = 'bad' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	w := newTestWriter(t, db, 10)
	w.write([]*store.Log{mkLog("good", tok.ID, 5), mkLog("bad", tok.ID, 7)})
	if n := countLogs(t, db); n != 1 {
		t.Fatalf("stored %d", n)
	}
	if w.Dropped() != 1 {
		t.Fatalf("failed count %d", w.Dropped())
	}
	got, _ := db.GetToken(ctx, tok.ID)
	if got.UsedTicks != 12 || got.Requests != 2 {
		t.Fatalf("accounting lost: %+v", got)
	}
	// A single failing log goes straight to accounting.
	w.write([]*store.Log{mkLog("bad", tok.ID, 1)})
	if got, _ = db.GetToken(ctx, tok.ID); got.UsedTicks != 13 {
		t.Fatalf("single-log fallback: %+v", got)
	}
}

func TestWriterClosedDatabase(t *testing.T) {
	db := testDB(t)
	w := newTestWriter(t, db, 10)
	db.Close()
	w.write([]*store.Log{mkLog("a", 0, 1), mkLog("b", 0, 1)}) // must not panic
	if w.Dropped() != 2 {
		t.Fatalf("failed count %d", w.Dropped())
	}
	w.Enqueue(mkLog("c", 0, 1))
	if err := w.Flush(context.Background()); err == nil {
		t.Fatal("flush into a closed database should fail")
	}
	if err := w.Flush(context.Background()); err != nil {
		t.Fatalf("empty flush: %v", err)
	}
}

func TestJanitorRunOnce(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	old := time.Now().Add(-40 * 24 * time.Hour).UnixMilli()
	logs := []*store.Log{mkLog("old", 0, 3), mkLog("new", 0, 4)}
	logs[0].CreatedAt = old
	if err := db.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	for id, exp := range map[string]int64{"expired": store.NowMs() - 1000, "live": store.NowMs() + 3600_000} {
		if err := db.PutBinding(ctx, &store.Binding{ResourceID: id, Kind: "video", KeyID: 1, ExpiresAt: exp}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateSession(ctx, "dead", store.NowMs()-1000); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, "alive", store.NowMs()+3600_000); err != nil {
		t.Fatal(err)
	}

	s := config.DefaultSettings()
	s.LogRetentionDays = 0 // retention disabled: logs stay
	h := config.NewHolder(s)
	j := &Janitor{DB: db, Settings: h, Log: quietLog()}
	j.RunOnce(ctx)
	if n := countLogs(t, db); n != 2 {
		t.Fatalf("retention 0 deleted logs: %d", n)
	}
	if _, err := db.GetBinding(ctx, "live"); err != nil {
		t.Fatal("live binding purged")
	}
	var bindings int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM bindings`).Scan(&bindings)
	if bindings != 1 {
		t.Fatalf("%d bindings left", bindings)
	}
	var sessions int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessions)
	if sessions != 1 {
		t.Fatalf("%d sessions left", sessions)
	}

	s.LogRetentionDays = 30
	h.Set(s)
	j.RunOnce(ctx)
	list, n, _ := db.ListLogs(ctx, store.LogFilter{Limit: 10})
	if n != 1 || list[0].RequestID != "new" {
		t.Fatalf("retention: %d logs", n)
	}
	agg, err := db.StatsTotal(ctx, 0)
	if err != nil || agg.Requests != 2 || agg.CostTicks != 7 {
		t.Fatalf("stats must survive retention: %+v %v", agg, err)
	}
	if evs, _ := db.ListEvents(ctx, store.EventFilter{Action: "logs.retention"}); len(evs) != 1 || evs[0].Detail["deleted"] != float64(1) {
		t.Fatalf("retention run not recorded: %+v", evs)
	}

	db.Close()
	j.RunOnce(ctx) // errors are logged, not fatal
}

func TestJanitorRunStops(t *testing.T) {
	db := testDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	j := &Janitor{DB: db, Settings: config.NewHolder(config.DefaultSettings()), Log: quietLog()}
	j.Run(ctx)
	if j.Interval != time.Hour {
		t.Fatalf("default interval %v", j.Interval)
	}
}

func TestMaskingHelpers(t *testing.T) {
	cases := map[string]string{
		"":                           "****",
		"Bearer short":               "Bearer ****",
		"Bearer sk-abcdefghijklmnop": "Bearer sk-abc…mnop",
		"abcdefghijklmnopqrstu":      "abcdef…rstu",
	}
	for in, want := range cases {
		if got := MaskSecret(in); got != want {
			t.Errorf("MaskSecret(%q) = %q want %q", in, got, want)
		}
	}
	if got := maskProtocols("realtime, xai-client-secret.sk-abcdefghijklmnop"); got != "realtime, xai-client-secret.sk-abc…mnop" {
		t.Errorf("maskProtocols: %q", got)
	}
	h := http.Header{}
	h.Set("Sec-Websocket-Protocol", "xai-client-secret.sk-abcdefghijklmnop")
	h.Set("Proxy-Authorization", "Basic dXNlcjpwYXNzd29yZDEyMzQ=")
	out := HeadersJSON(h)
	if strings.Contains(out, "defghijkl") || strings.Contains(out, "cjpwYXNzd29y") {
		t.Fatalf("secret leaked: %s", out)
	}
	h = http.Header{"Sec-Websocket-Protocol": {"realtime"}}
	if out := HeadersJSON(h); out != `{"Sec-Websocket-Protocol":"realtime"}` {
		t.Fatalf("plain protocol changed: %s", out)
	}
}

func TestSanitizeBodyEdgeCases(t *testing.T) {
	b64 := strings.Repeat("QUJD", 300) // 1200 chars
	cases := []struct {
		name, in string
		max      int
		want     string
		changed  bool
	}{
		{"empty", "", 0, "", false},
		{"padding kept inside placeholder", `{"a":"` + b64 + `=="}`, 0, `{"a":"<base64 1202 bytes omitted>"}`, true},
		{"run not ending in a quote", `{"a":"` + b64 + ` and text"}`, 0, `{"a":"` + b64 + ` and text"}`, false},
		{"short data uri kept", `data:image/png;base64,QUJD`, 0, `data:image/png;base64,QUJD`, false},
		{"url-safe alphabet", `"` + strings.Repeat("-_", 600) + `"`, 0, `"<base64 1200 bytes omitted>"`, true},
		{"cap after stripping", `{"a":"` + b64 + `","b":"0123456789"}`, 40,
			`{"a":"<base64 1200 bytes omitted>","b":"` + "\n…[truncated, 1225 bytes total]", true},
		{"no cap", "hello", 0, "hello", false},
		{"under cap", "hello", 5, "hello", false},
	}
	for _, c := range cases {
		got, changed := SanitizeBody([]byte(c.in), c.max)
		if got != c.want || changed != c.changed {
			t.Errorf("%s: got %q %v\nwant %q %v", c.name, got, changed, c.want, c.changed)
		}
	}
}
