package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestLogsStatsAndCounters(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	k := &Key{Name: "k", Key: "xai-1", BaseURL: "https://api.x.ai", Weight: 1, Enabled: true}
	if err := db.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateKey(ctx, &Key{Name: "dup", Key: "xai-1", BaseURL: "x", Weight: 1}); err != ErrConflict {
		t.Fatalf("duplicate key: %v", err)
	}
	tok := &Token{Name: "t", Hash: "h1", Prefix: "sk-xr-a", Enabled: true, QuotaTicks: 1000}
	if err := db.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	logs := []*Log{
		{RequestID: "r1", CreatedAt: now, Method: "POST", Path: "/v1/chat/completions", Model: "grok-4.3", Status: 200,
			TokenID: tok.ID, KeyID: k.ID, KeyName: "k", PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
			CostTicks: 300, CostSource: "upstream", LatencyMs: 100, HasBodies: true, ReqBody: `{"a":1}`,
			Attempts: []Attempt{{Seq: 1, KeyID: k.ID, Status: 429, Action: "cooldown", Error: "rl", StartedAt: now},
				{Seq: 2, KeyID: k.ID, Status: 200, Action: "success", StartedAt: now}}},
		{RequestID: "r2", CreatedAt: now, Method: "POST", Path: "/v1/responses", Model: "grok-4.3", Status: 502,
			TokenID: tok.ID, Error: "upstream down", LatencyMs: 50},
	}
	if err := db.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	// Stats count log rows (v0.6.0): r1's retried attempt is a row of its own.
	a, err := db.StatsTotal(ctx, now-3600_000)
	if err != nil {
		t.Fatal(err)
	}
	if a.Requests != 3 || a.Success != 1 || a.Errors != 2 || a.CostTicks != 300 || a.LatencySum != 150 {
		t.Fatalf("stats: %+v", a)
	}
	k2, _ := db.GetKey(ctx, k.ID)
	if k2.Attempts != 2 || k2.AttemptErrors != 1 || k2.CostTicks != 300 || k2.LastError != "rl" {
		t.Fatalf("key counters: %+v", k2)
	}
	t2, _ := db.GetToken(ctx, tok.ID)
	if t2.Requests != 3 || t2.UsedTicks != 300 {
		t.Fatalf("token counters: %+v", t2)
	}
	// Filters.
	for status, want := range map[string]int{"": 3, "success": 1, "error": 2, "5xx": 1, "200": 1, "4xx": 1, "429": 1} {
		_, n, err := db.ListLogs(ctx, LogFilter{Status: status, Limit: 10})
		if err != nil || n != want {
			t.Errorf("status %q: n=%d err=%v want %d", status, n, err, want)
		}
	}
	if _, n, _ := db.ListLogs(ctx, LogFilter{Q: "down", Limit: 10}); n != 1 {
		t.Errorf("q filter n=%d", n)
	}
	if _, n, _ := db.ListLogs(ctx, LogFilter{Path: "/v1/resp", Limit: 10}); n != 1 {
		t.Errorf("path filter n=%d", n)
	}
	if _, n, _ := db.ListLogs(ctx, LogFilter{KeyID: k.ID, Limit: 10}); n != 2 {
		t.Errorf("key filter n=%d", n)
	}
	if _, n, _ := db.ListLogs(ctx, LogFilter{FinalOnly: true, Limit: 10}); n != 2 {
		t.Errorf("final filter n=%d", n)
	}
	// logs[0].ID is the final row; its request body comes from row 1.
	l, err := db.GetLog(ctx, logs[0].ID)
	if err != nil || len(l.Attempts) != 0 || l.ReqBody != `{"a":1}` || l.AttemptCount != 1 || l.AttemptIndex != 2 ||
		l.AttemptTotal != 2 || !l.AttemptFinal || l.ReqBodyFrom == 0 || l.ReqBodyFrom == l.ID || l.CostTicks != 300 {
		t.Fatalf("detail: %+v err=%v", l, err)
	}
	first, err := db.GetLog(ctx, l.ReqBodyFrom)
	if err != nil || first.AttemptState() != "retried" || first.Status != 429 || first.Error != "rl" ||
		first.AttemptAction != "cooldown" || first.CostTicks != 0 || first.ReqBodyFrom != 0 {
		t.Fatalf("first row: %+v %v", first, err)
	}
	// Breakdown and series.
	rows, err := db.StatsBreakdown(ctx, now-3600_000, "model")
	if err != nil || len(rows) != 1 || rows[0].Requests != 3 {
		t.Fatalf("breakdown: %+v %v", rows, err)
	}
	series, err := db.StatsSeries(ctx, now-3600_000, 3600_000, 0, "key")
	if err != nil || len(series) != 2 {
		t.Fatalf("series: %+v %v", series, err)
	}
	// Retention keeps aggregates.
	n, err := db.DeleteLogsBefore(ctx, now+1000) // the chain sorts at its end
	if err != nil || n != 3 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if a, _ := db.StatsTotal(ctx, now-3600_000); a.Requests != 3 {
		t.Fatal("aggregates lost on retention")
	}
	if _, err := db.GetLog(ctx, logs[0].ID); err != ErrNotFound {
		t.Fatal("log not deleted")
	}
}

func TestBindingsBilledOnce(t *testing.T) {
	ctx := context.Background()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutBinding(ctx, &Binding{ResourceID: "v1", Kind: "video", KeyID: 3, ExpiresAt: NowMs() + 60000}); err != nil {
		t.Fatal(err)
	}
	b, err := db.GetBinding(ctx, "v1")
	if err != nil || b.KeyID != 3 {
		t.Fatalf("binding: %+v %v", b, err)
	}
	first, _ := db.MarkBindingBilled(ctx, "v1")
	second, _ := db.MarkBindingBilled(ctx, "v1")
	if !first || second {
		t.Fatalf("billed once: %v %v", first, second)
	}
	_ = db.PutBinding(ctx, &Binding{ResourceID: "old", Kind: "video", KeyID: 1, ExpiresAt: NowMs() - 1})
	if _, err := db.GetBinding(ctx, "old"); err != ErrNotFound {
		t.Fatal("expired binding returned")
	}
	if n, _ := db.PurgeExpiredBindings(ctx); n != 1 {
		t.Fatalf("purged %d", n)
	}
}

func TestSessions(t *testing.T) {
	ctx := context.Background()
	db, _ := Open(":memory:")
	defer db.Close()
	_ = db.CreateSession(ctx, "s1", NowMs()+1000_000)
	_ = db.CreateSession(ctx, "s2", NowMs()-1)
	if _, err := db.GetSession(ctx, "s1", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSession(ctx, "s2", 1000); err != ErrNotFound {
		t.Fatal("expired session accepted")
	}
	_ = db.DeleteSessionsExcept(ctx, "")
	if _, err := db.GetSession(ctx, "s1", 1000); err != ErrNotFound {
		t.Fatal("session not revoked")
	}
}

// A retry chain whose rows are not stored (logging off, or reduced to an
// accounting stub) is still accounted per row: stats, key and token
// counters as if the rows had been written.
func TestChainAccountingWithoutRecord(t *testing.T) {
	ctx := context.Background()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	k1 := &Key{Name: "k1", Key: "xai-1", BaseURL: "https://api.x.ai", Weight: 1, Enabled: true}
	k2 := &Key{Name: "k2", Key: "xai-2", BaseURL: "https://api.x.ai", Weight: 1, Enabled: true}
	_ = db.CreateKey(ctx, k1)
	_ = db.CreateKey(ctx, k2)
	tok := &Token{Name: "t", Hash: "h", Prefix: "sk-xr-a", Enabled: true}
	_ = db.CreateToken(ctx, tok)
	now := time.Now().UnixMilli()
	l := &Log{RequestID: "r", CreatedAt: now, Method: "POST", Path: "/v1/chat/completions", Status: 200, TokenID: tok.ID,
		KeyID: k2.ID, CostTicks: 500, LatencyMs: 30, NoRecord: true,
		Attempts: []Attempt{{Seq: 1, KeyID: k1.ID, Status: 500, Action: "retry", Error: "boom", StartedAt: now, LatencyMs: 10, CostTicks: 7},
			{Seq: 2, KeyID: k2.ID, Status: 200, Action: "success", StartedAt: now + 10}}}
	if err := db.InsertLogs(ctx, []*Log{l}); err != nil {
		t.Fatal(err)
	}
	if _, n, _ := db.ListLogs(ctx, LogFilter{Limit: 10}); n != 0 {
		t.Fatalf("rows stored: %d", n)
	}
	a, _ := db.StatsTotal(ctx, 0)
	if a.Requests != 2 || a.Success != 1 || a.Errors != 1 || a.CostTicks != 507 {
		t.Fatalf("stats: %+v", a)
	}
	g1, _ := db.GetKey(ctx, k1.ID)
	g2, _ := db.GetKey(ctx, k2.ID)
	if g1.Attempts != 1 || g1.AttemptErrors != 1 || g1.CostTicks != 7 || g2.Attempts != 1 || g2.AttemptErrors != 0 || g2.CostTicks != 500 {
		t.Fatalf("keys: %+v %+v", g1, g2)
	}
	if tk, _ := db.GetToken(ctx, tok.ID); tk.Requests != 2 || tk.UsedTicks != 507 {
		t.Fatalf("token: %+v", tk)
	}
}
