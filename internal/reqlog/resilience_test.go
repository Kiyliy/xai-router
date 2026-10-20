package reqlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/store"
)

func fileDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fastWriter shortens every timeout so a stall "longer than the timeout"
// fits in a test.
func fastWriter(t *testing.T, db *store.DB, queue int) *Writer {
	w := newTestWriter(t, db, queue)
	w.batchTimeout, w.rowTimeout = 200*time.Millisecond, 100*time.Millisecond
	w.minBackoff, w.maxBackoff = 50*time.Millisecond, 200*time.Millisecond
	w.drainBudget = 5 * time.Second
	return w
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// H1: the only write connection is held (as by a full VACUUM) for several
// times the write timeout. No spend, row or stat may be lost.
func TestWriterSurvivesHeldWriteConnection(t *testing.T) {
	db := fileDB(t)
	ctx := context.Background()
	tok := mkToken(t, db)
	w := fastWriter(t, db, 1000)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()

	conn, err := db.W.Conn(ctx) // the pool has one connection: the writer now waits
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		w.Enqueue(mkLog(fmt.Sprintf("stall-%d", i), tok.ID, 1000))
	}
	time.Sleep(1500 * time.Millisecond) // > 7 batch timeouts
	if w.QueueLen() != 5 || w.Lost() != 0 || w.Dropped() != 0 {
		t.Fatalf("during the stall: queued %d lost %d dropped %d", w.QueueLen(), w.Lost(), w.Dropped())
	}
	conn.Close()

	waitFor(t, "the retained logs", 10*time.Second, func() bool { return countLogs(t, db) == 5 })
	got, _ := db.GetToken(ctx, tok.ID)
	if got.UsedTicks != 5000 || got.Requests != 5 {
		t.Fatalf("spend lost: used %d requests %d", got.UsedTicks, got.Requests)
	}
	agg, err := db.StatsTotal(ctx, 0)
	if err != nil || agg.Requests != 5 || agg.CostTicks != 5000 {
		t.Fatalf("stats lost: %+v %v", agg, err)
	}
	if w.Lost() != 0 || w.Dropped() != 0 || w.QueueLen() != 0 || w.bytes.Load() != 0 {
		t.Fatalf("after the stall: lost %d dropped %d queued %d bytes %d", w.Lost(), w.Dropped(), w.QueueLen(), w.bytes.Load())
	}
}

// H1: past the record bound, waiting rows are reduced to accounting stubs:
// counters and stats are kept ahead of log rows and bodies.
func TestWriterRetainedRowsBoundedAccountingFirst(t *testing.T) {
	db := fileDB(t)
	ctx := context.Background()
	tok := mkToken(t, db)
	w := fastWriter(t, db, 2)
	w.maxRecords = 3
	conn, err := db.W.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ { // 2 fit in the channel, 6 overflow: 3 rows + 3 stubs
		w.Enqueue(mkLog(fmt.Sprintf("b-%d", i), tok.ID, 10))
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { w.Run(runCtx); close(done) }()
	time.Sleep(600 * time.Millisecond)
	w.mu.Lock()
	stubs := 0
	for _, l := range w.pending {
		if l.NoRecord {
			stubs++
		}
	}
	waiting := len(w.pending)
	w.mu.Unlock()
	if waiting != 8 || stubs != 5 {
		t.Fatalf("retained %d, %d stubs", waiting, stubs)
	}
	conn.Close()
	waitFor(t, "accounting", 10*time.Second, func() bool {
		got, _ := db.GetToken(ctx, tok.ID)
		return got.Requests == 8
	})
	cancel()
	<-done
	got, _ := db.GetToken(ctx, tok.ID)
	if got.UsedTicks != 80 || countLogs(t, db) != 3 || w.Lost() != 0 {
		t.Fatalf("used %d rows %d lost %d", got.UsedTicks, countLogs(t, db), w.Lost())
	}
}

func TestTemporaryErrors(t *testing.T) {
	for _, e := range []error{context.DeadlineExceeded, fmt.Errorf("x: %w", context.DeadlineExceeded),
		errors.New("database is locked (5) (SQLITE_BUSY)"), errors.New("disk I/O error (10)"), errors.New("database or disk is full (13)")} {
		if !temporary(e) {
			t.Errorf("%v should be temporary", e)
		}
	}
	for _, e := range []error{errors.New("sql: database is closed"), errors.New("constraint failed: boom")} {
		if temporary(e) {
			t.Errorf("%v should be permanent", e)
		}
	}
}

// L1a: logs enqueued while Run stops are either drained or written
// synchronously, never left in the queue.
func TestWriterStopRaceKeepsEveryLog(t *testing.T) {
	db := fileDB(t)
	ctx := context.Background()
	tok := mkToken(t, db)
	for round := 0; round < 5; round++ {
		// This test is about the stop race, not about timeouts: give each
		// write production-like time, since under -race on a slow runner
		// 200 ms per row is not enough and late logs were counted lost.
		w := fastWriter(t, db, 4)
		w.batchTimeout, w.rowTimeout, w.drainBudget = 10*time.Second, 5*time.Second, 60*time.Second
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { w.Run(runCtx); close(done) }()
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 25; i++ {
					w.Enqueue(mkLog(fmt.Sprintf("race-%d-%d-%d", round, g, i), tok.ID, 1))
				}
			}()
		}
		time.Sleep(time.Millisecond)
		cancel()
		wg.Wait()
		<-done
		if n := w.QueueLen(); n != 0 {
			t.Fatalf("round %d: %d logs left in the queue after Run returned", round, n)
		}
	}
	got, _ := db.GetToken(ctx, tok.ID)
	if got.Requests != 5*8*25 {
		t.Fatalf("requests %d", got.Requests)
	}
}

// L1b: after Close no write reaches the (closed) database; the loss is
// counted instead.
func TestWriterAfterClose(t *testing.T) {
	db := testDB(t)
	w := newTestWriter(t, db, 4)
	w.Close()
	db.Close()
	w.Enqueue(mkLog("late", 1, 5))
	if w.Lost() != 1 || w.QueueLen() != 0 {
		t.Fatalf("lost %d queued %d", w.Lost(), w.QueueLen())
	}
}

// M3: a request refused before authentication stores only a bounded
// summary, but it is still logged and counted.
func TestRefusedRequestsStoreSummaryOnly(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	w := newTestWriter(t, db, 10)
	pad := strings.Repeat("a", 900_000)
	h := http.Header{"X-Pad": {pad}, "User-Agent": {"curl"}}
	l := &store.Log{RequestID: "anon", CreatedAt: store.NowMs(), Method: "POST", Path: "/v1/" + pad, Query: "q=" + pad,
		Status: 401, Error: "missing API key", ClientIP: "203.0.113.9", UserAgent: strings.Repeat("u", 300),
		HasBodies: true, ReqHeaders: HeadersJSON(h), ReqBody: pad, RespHeaders: "{}"}
	w.Enqueue(l)
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	logs, n, _ := db.ListLogs(ctx, store.LogFilter{Limit: 5})
	if n != 1 {
		t.Fatalf("refusal not logged: %d", n)
	}
	got, err := db.GetLog(ctx, logs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HasBodies || got.ReqHeaders != "" || got.ReqBody != "" || len(got.Path) > refusedPathCap ||
		len(got.Query) > refusedQueryCap || len(got.UserAgent) > refusedUACap || got.Status != 401 ||
		got.Error != "missing API key" || got.ClientIP != "203.0.113.9" || got.Method != "POST" {
		t.Fatalf("stored: path %d query %d ua %d headers %d body %d bodies %v", len(got.Path), len(got.Query),
			len(got.UserAgent), len(got.ReqHeaders), len(got.ReqBody), got.HasBodies)
	}
	if agg, _ := db.StatsTotal(ctx, 0); agg.Requests != 1 || agg.Errors != 1 {
		t.Fatalf("refusal not counted: %+v", agg)
	}
}

// M3: authenticated requests keep their headers, capped in size.
func TestHeadersAndPathCapped(t *testing.T) {
	h := http.Header{"X-Pad": {strings.Repeat("a", 900_000)}, "Content-Type": {"application/json"}}
	for i := 0; i < 300; i++ {
		h.Set(fmt.Sprintf("X-H-%03d", i), strings.Repeat("v", 200))
	}
	l := mkLog("auth", 7, 1)
	l.ReqHeaders = HeadersJSON(h)
	l.Path = "/v1/" + strings.Repeat("p", 10_000)
	l.Query = strings.Repeat("q", 10_000)
	capLog(l)
	if len(l.ReqHeaders) > maxHeaderBytes || len(l.Path) > maxPathBytes || len(l.Query) > maxQueryBytes {
		t.Fatalf("headers %d path %d query %d", len(l.ReqHeaders), len(l.Path), len(l.Query))
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(l.ReqHeaders), &m); err != nil {
		t.Fatalf("capped headers are not JSON: %v", err)
	}
	if m["Content-Type"] != "application/json" || !strings.Contains(m["X-Router-Note"], "omitted") {
		t.Fatalf("headers: %v", m["X-Router-Note"])
	}
	small := `{"A":"b"}`
	if capHeaders(small) != small {
		t.Fatal("small headers changed")
	}
}
