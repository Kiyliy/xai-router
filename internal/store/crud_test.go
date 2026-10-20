package store

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
)

func memDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMigrationsIdempotentAndMeta(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "x.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := db.GetMeta(ctx, "missing"); err != nil || v != "" {
		t.Fatalf("missing meta: %q %v", v, err)
	}
	if err := db.SetMeta(ctx, "a", "1"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(ctx, "a", "2"); err != nil {
		t.Fatal(err)
	}
	if db.Size() <= 0 {
		t.Fatal("file database reports no size")
	}
	db.Close()

	// Re-opening runs no migration twice and keeps the data.
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.W.QueryRowContext(ctx, `SELECT v FROM schema_version`).Scan(&v); err != nil || v != SchemaVersion() {
		t.Fatalf("schema version %d %v", v, err)
	}
	if got, _ := db.GetMeta(ctx, "a"); got != "2" {
		t.Fatalf("meta %q", got)
	}
	if err := db.Vacuum(ctx); err != nil {
		t.Fatal(err)
	}
	if memDB(t).Size() != 0 {
		t.Fatal("memory database has a size")
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	// A directory where the database file should be.
	if _, err := Open(dir); err == nil {
		t.Fatal("opening a directory should fail")
	}
}

func TestKeysCRUD(t *testing.T) {
	ctx := context.Background()
	db := memDB(t)
	mk := func(name, key string, prio int, enabled bool) *Key {
		k := &Key{Name: name, Key: key, BaseURL: "https://api.x.ai", Priority: prio, Weight: 2, Enabled: enabled,
			MaxConcurrency: 3, RPMLimit: 4, Notes: "note " + name}
		if err := db.CreateKey(ctx, k); err != nil {
			t.Fatal(err)
		}
		return k
	}
	a := mk("alpha", "xai-aaaa", 1, true)
	b := mk("beta", "xai-bbbb", 0, true)
	c := mk("gamma", "xai-cccc", 2, false)

	all, err := db.ListAllKeys(ctx)
	if err != nil || len(all) != 3 || all[0].ID != b.ID || all[2].ID != c.ID {
		t.Fatalf("ordering: %v %v", all, err)
	}
	got, err := db.GetKey(ctx, a.ID)
	if err != nil || got.Name != "alpha" || got.Weight != 2 || got.MaxConcurrency != 3 || got.RPMLimit != 4 || !got.Enabled {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := db.GetKey(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}

	a.Name, a.Priority = "alpha2", 5
	if err := db.UpdateKey(ctx, a); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.GetKey(ctx, a.ID); got.Name != "alpha2" || got.Priority != 5 {
		t.Fatalf("update: %+v", got)
	}
	a.Key = "xai-bbbb"
	if err := db.UpdateKey(ctx, a); !errors.Is(err, ErrConflict) {
		t.Fatalf("update conflict: %v", err)
	}
	if err := db.UpdateKey(ctx, &Key{ID: 999, Key: "zzz"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}

	future := NowMs() + 3600_000
	if err := db.SetKeyState(ctx, b.ID, false, "rate limited", future); err != nil {
		t.Fatal(err)
	}
	if err := db.SetKeyTest(ctx, b.ID, `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.GetKey(ctx, b.ID); got.CooldownUntil != future || got.StatusReason != "rate limited" ||
		got.LastTest != `{"ok":true}` || got.LastTestedAt == 0 {
		t.Fatalf("state: %+v", got)
	}

	counts, err := db.CountKeys(ctx)
	if err != nil || counts != (KeyCounts{Total: 3, Active: 1, Cooldown: 1, Disabled: 1}) {
		t.Fatalf("counts: %+v %v", counts, err)
	}
	cases := []struct {
		f    KeyFilter
		want int
	}{
		{KeyFilter{Limit: 10}, 3},
		{KeyFilter{Status: "active", Limit: 10}, 1},
		{KeyFilter{Status: "cooldown", Limit: 10}, 1},
		{KeyFilter{Status: "disabled", Limit: 10}, 1},
		{KeyFilter{Q: "beta", Limit: 10}, 1},
		{KeyFilter{Q: "cccc", Limit: 10}, 1}, // key suffix
		{KeyFilter{Q: "note", Limit: 10}, 3},
	}
	for _, c := range cases {
		_, n, err := db.ListKeys(ctx, c.f)
		if err != nil || n != c.want {
			t.Errorf("%+v: %d %v", c.f, n, err)
		}
	}
	page, n, _ := db.ListKeys(ctx, KeyFilter{Limit: 1, Offset: 1})
	if n != 3 || len(page) != 1 || page[0].ID != c.ID {
		t.Fatalf("paging: %d %v", n, page)
	}

	if err := db.DeleteKey(ctx, c.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteKey(ctx, c.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestTokensCRUD(t *testing.T) {
	ctx := context.Background()
	db := memDB(t)
	tok := &Token{Name: "app", Hash: "h1", Prefix: "sk-xr-a", Enabled: true, QuotaTicks: 100, ExpiresAt: 5,
		AllowedModels: []string{"grok-4"}, Notes: "first"}
	if err := db.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateToken(ctx, &Token{Name: "dup", Hash: "h1"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate hash: %v", err)
	}
	other := &Token{Name: "svc", Hash: "h2", Prefix: "sk-xr-b"}
	if err := db.CreateToken(ctx, other); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTokenByHash(ctx, "h1")
	if err != nil || got.ID != tok.ID || got.AllowedModels[0] != "grok-4" || got.ExpiresAt != 5 || got.QuotaTicks != 100 {
		t.Fatalf("by hash: %+v %v", got, err)
	}
	if got, _ = db.GetToken(ctx, other.ID); got.AllowedModels == nil || len(got.AllowedModels) != 0 || got.Enabled {
		t.Fatalf("defaults: %+v", got)
	}
	if _, err := db.GetToken(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	list, n, err := db.ListTokens(ctx, "", 10, 0)
	if err != nil || n != 2 || list[0].ID != other.ID {
		t.Fatalf("list (newest first): %v %d %v", list, n, err)
	}
	if _, n, _ = db.ListTokens(ctx, "first", 10, 0); n != 1 {
		t.Fatalf("search notes: %d", n)
	}
	total, enabled, err := db.TokenCounts(ctx)
	if err != nil || total != 2 || enabled != 1 {
		t.Fatalf("counts: %d %d %v", total, enabled, err)
	}

	tok.Name, tok.AllowedModels, tok.Hash = "renamed", nil, "h3"
	if err := db.UpdateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.GetTokenByHash(ctx, "h3"); got.Name != "renamed" || len(got.AllowedModels) != 0 {
		t.Fatalf("update: %+v", got)
	}
	if err := db.UpdateToken(ctx, &Token{ID: 999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE tokens SET used_ticks = 50 WHERE id = ?`, tok.ID); err != nil {
		t.Fatal(err)
	}
	// UpdateToken never touches the quota or the usage.
	if got, _ = db.GetToken(ctx, tok.ID); got.UsedTicks != 50 {
		t.Fatalf("usage changed by an update: %d", got.UsedTicks)
	}
	if err := db.DeleteToken(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteToken(ctx, tok.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestAdminTokensAndSessions(t *testing.T) {
	ctx := context.Background()
	db := memDB(t)
	at := &AdminToken{Name: "ci", Hash: "ah1", Prefix: "xra-1"}
	if err := db.CreateAdminToken(ctx, at); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAdminToken(ctx, &AdminToken{Name: "dup", Hash: "ah1"}); err == nil {
		t.Fatal("duplicate admin token hash accepted")
	}
	if err := db.CreateAdminToken(ctx, &AdminToken{Name: "two", Hash: "ah2"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAdminTokenByHash(ctx, "ah1")
	if err != nil || got.ID != at.ID || got.Name != "ci" {
		t.Fatalf("by hash: %+v %v", got, err)
	}
	db.touches.Wait() // last_used_at is updated in the background
	ts, _ := db.ListAdminTokens(ctx)
	if len(ts) != 2 || ts[0].LastUsedAt == 0 {
		t.Fatalf("list / last used: %+v", ts)
	}
	if _, err := db.GetAdminTokenByHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := db.DeleteAdminToken(ctx, at.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAdminToken(ctx, at.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := db.DeleteAllAdminTokens(ctx); err != nil {
		t.Fatal(err)
	}
	if ts, _ = db.ListAdminTokens(ctx); len(ts) != 0 {
		t.Fatalf("%d admin tokens left", len(ts))
	}

	now := NowMs()
	for _, s := range []struct {
		hash string
		exp  int64
	}{{"keep", now + 3600_000}, {"drop", now + 3600_000}, {"old", now - 1}} {
		if err := db.CreateSession(ctx, s.hash, s.exp); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.PurgeExpiredSessions(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSession(ctx, "old", 1000); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: %v", err)
	}
	if err := db.DeleteSession(ctx, "drop"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSession(ctx, "drop", 1000); !errors.Is(err, ErrNotFound) {
		t.Fatal("deleted session still valid")
	}
	if err := db.DeleteSessionsExcept(ctx, "keep"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSession(ctx, "keep", 1000); err != nil {
		t.Fatalf("kept session: %v", err)
	}
}

func TestModelsCRUD(t *testing.T) {
	ctx := context.Background()
	db := memDB(t)
	m := &Model{ID: "m1", Kind: "image", Aliases: []string{"m1-a"}, Enabled: true, Source: "manual", ImageTicks: 5,
		ImageTiers: []ImageTier{{Quality: "low", Resolution: "1k", Ticks: 4}}, Raw: `{"x":1}`, SyncedAt: 7}
	if err := db.UpsertModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertModel(ctx, &Model{ID: "m2", Kind: "language", InputTicks: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetModel(ctx, "m1")
	if err != nil || got.Aliases[0] != "m1-a" || got.ImageTiers[0].Ticks != 4 || got.Raw != `{"x":1}` || got.SyncedAt != 7 ||
		got.UpdatedAt == 0 || !got.Enabled {
		t.Fatalf("get: %+v %v", got, err)
	}
	if got, _ = db.GetModel(ctx, "m2"); got.Aliases == nil || got.ImageTiers == nil || got.Enabled {
		t.Fatalf("defaults: %+v", got)
	}
	m.ImageTicks = 6
	if err := db.UpsertModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.GetModel(ctx, "m1"); got.ImageTicks != 6 {
		t.Fatal("upsert did not update")
	}
	if all, _ := db.ListModels(ctx, ""); len(all) != 2 {
		t.Fatalf("list all: %d", len(all))
	}
	if imgs, _ := db.ListModels(ctx, "image"); len(imgs) != 1 || imgs[0].ID != "m1" {
		t.Fatalf("list by kind: %v", imgs)
	}
	if _, err := db.GetModel(ctx, "zz"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := db.DeleteModel(ctx, "m2"); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteModel(ctx, "m2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestLogFiltersAndSeries(t *testing.T) {
	ctx := context.Background()
	db := memDB(t)
	now := NowMs()
	logs := []*Log{
		{RequestID: "a", CreatedAt: now - 2000, Method: "POST", Path: "/v1/chat/completions", Model: "m1", Status: 200,
			KeyID: 1, TokenID: 2, UpstreamID: "up-1", TotalTokens: 3, LatencyMs: 10},
		{RequestID: "b", CreatedAt: now - 1000, Method: "POST", Path: "/v1/chat_x", Model: "m2", Status: 429,
			Error: "100% rate_limited", KeyID: 1},
		{RequestID: "c", CreatedAt: now, Method: "GET", Path: "/v1/models", Status: 503, NoRecord: true},
	}
	if err := db.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		f    LogFilter
		want int
	}{
		{LogFilter{}, 2}, // the NoRecord log is not stored
		{LogFilter{From: now - 1500}, 1},
		{LogFilter{To: now - 1500}, 1},
		{LogFilter{Model: "m1"}, 1},
		{LogFilter{KeyID: 1}, 2},
		{LogFilter{TokenID: 2}, 1},
		{LogFilter{Path: "/v1/chat_"}, 1}, // "_" is literal, not a wildcard
		{LogFilter{Q: "up-1"}, 1},
		{LogFilter{Q: "100%"}, 1},
		{LogFilter{Q: "b"}, 1},
		{LogFilter{Status: "success"}, 1},
		{LogFilter{Status: "ERROR"}, 1},
		{LogFilter{Status: "4xx"}, 1},
		{LogFilter{Status: "429"}, 1},
		{LogFilter{Status: "9xx"}, 2}, // unknown filters are ignored
	}
	for _, c := range cases {
		c.f.Limit = 10
		if _, n, err := db.ListLogs(ctx, c.f); err != nil || n != c.want {
			t.Errorf("%+v: %d %v", c.f, n, err)
		}
	}
	if _, err := db.GetLog(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing log: %v", err)
	}
	// The accounting of the NoRecord log is kept.
	agg, _ := db.StatsTotal(ctx, 0)
	if agg.Requests != 3 || agg.Errors != 2 {
		t.Fatalf("stats: %+v", agg)
	}

	for _, by := range []string{"", "none", "model", "key", "token"} {
		rows, err := db.StatsSeriesLogs(ctx, now-10_000, 60_000, by)
		if err != nil || len(rows) == 0 {
			t.Errorf("series logs by %q: %v %v", by, rows, err)
		}
	}
	if _, err := db.StatsSeriesLogs(ctx, 0, 60_000, "color"); err == nil {
		t.Fatal("invalid group accepted")
	}
	if _, err := db.StatsSeries(ctx, 0, 3600_000, 0, "color"); err == nil {
		t.Fatal("invalid group accepted")
	}
	if _, err := db.StatsBreakdown(ctx, 0, "color"); err == nil {
		t.Fatal("invalid group accepted")
	}
	rows, err := db.StatsSeries(ctx, 0, 86400_000, 8*3600_000, "token")
	if err != nil || len(rows) == 0 {
		t.Fatalf("series: %v %v", rows, err)
	}
}

func TestBindingsPurge(t *testing.T) {
	ctx := context.Background()
	db := memDB(t)
	now := NowMs()
	if err := db.PutBinding(ctx, &Binding{ResourceID: "old", Kind: "video", KeyID: 1, ExpiresAt: now - 1}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutBinding(ctx, &Binding{ResourceID: "new", Kind: "file", KeyID: 1, TokenID: 3, ExpiresAt: now + 60_000}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetBinding(ctx, "old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired binding visible: %v", err)
	}
	if n, err := db.PurgeExpiredBindings(ctx); err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if b, err := db.GetBinding(ctx, "new"); err != nil || b.TokenID != 3 {
		t.Fatalf("live binding: %+v %v", b, err)
	}
	if first, err := db.MarkBindingBilled(ctx, "missing"); first || err != nil {
		t.Fatalf("billing a missing binding: %v %v", first, err)
	}
}

func TestClosedDatabaseErrors(t *testing.T) {
	ctx := context.Background()
	db := memDB(t)
	db.Close()
	errs := map[string]error{}
	_, errs["ListAllKeys"] = db.ListAllKeys(ctx)
	_, _, errs["ListKeys"] = db.ListKeys(ctx, KeyFilter{})
	_, errs["CountKeys"] = db.CountKeys(ctx)
	errs["CreateKey"] = db.CreateKey(ctx, &Key{Key: "k"})
	errs["UpdateKey"] = db.UpdateKey(ctx, &Key{ID: 1})
	errs["DeleteKey"] = db.DeleteKey(ctx, 1, "")
	_, _, errs["ListTokens"] = db.ListTokens(ctx, "", 1, 0)
	errs["CreateToken"] = db.CreateToken(ctx, &Token{Hash: "x"})
	errs["UpdateToken"] = db.UpdateToken(ctx, &Token{ID: 1})
	errs["DeleteToken"] = db.DeleteToken(ctx, 1)
	_, errs["ListAdminTokens"] = db.ListAdminTokens(ctx)
	_, errs["GetAdminTokenByHash"] = db.GetAdminTokenByHash(ctx, "x")
	errs["DeleteAdminToken"] = db.DeleteAdminToken(ctx, 1)
	_, errs["GetSession"] = db.GetSession(ctx, "x", 1)
	_, errs["ListModels"] = db.ListModels(ctx, "")
	errs["DeleteModel"] = db.DeleteModel(ctx, "x")
	_, _, errs["ListLogs"] = db.ListLogs(ctx, LogFilter{})
	_, errs["GetLog"] = db.GetLog(ctx, 1)
	errs["InsertLogs"] = db.InsertLogs(ctx, []*Log{{RequestID: "x"}})
	_, errs["DeleteLogsBefore"] = db.DeleteLogsBefore(ctx, 1)
	_, errs["StatsBreakdown"] = db.StatsBreakdown(ctx, 0, "model")
	_, errs["StatsSeries"] = db.StatsSeries(ctx, 0, 3600_000, 0, "")
	_, errs["StatsSeriesLogs"] = db.StatsSeriesLogs(ctx, 0, 60_000, "")
	_, errs["GetBinding"] = db.GetBinding(ctx, "x")
	_, errs["MarkBindingBilled"] = db.MarkBindingBilled(ctx, "x")
	_, errs["PurgeExpiredBindings"] = db.PurgeExpiredBindings(ctx)
	_, errs["GetMeta"] = db.GetMeta(ctx, "x")
	for name, err := range errs {
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s on a closed database: %v", name, err)
		}
	}
}

// A WebSocket session that was relayed successfully is logged with status
// 101 (Switching Protocols); it should count as a success.
func TestWebSocketSessionCountsAsSuccess(t *testing.T) {
	ctx := context.Background()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := NowMs()
	if !(&Log{Status: 101, WS: true}).Success() {
		t.Fatal("status 101 (WebSocket established) must count as success")
	}
	if err := db.InsertLogs(ctx, []*Log{{RequestID: "ws", CreatedAt: now, Method: "GET", Path: "/v1/realtime", Status: 101, WS: true}}); err != nil {
		t.Fatal(err)
	}
	if a, _ := db.StatsTotal(ctx, now-3600_000); a.Success != 1 || a.Errors != 0 {
		t.Fatalf("stats: %+v", a)
	}
	if _, n, _ := db.ListLogs(ctx, LogFilter{Status: "success", Limit: 10}); n != 1 {
		t.Fatalf("success filter: %d", n)
	}
	if _, n, _ := db.ListLogs(ctx, LogFilter{Status: "error", Limit: 10}); n != 0 {
		t.Fatalf("error filter: %d", n)
	}
}

func TestTotalKindsAndStorageStats(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := NowMs()
	var logs []*Log
	for i := 0; i < 300; i++ {
		st := 200
		if i%3 == 0 {
			st = 500
		}
		logs = append(logs, &Log{RequestID: "id-" + strconv.Itoa(i), UpstreamID: "up-" + strconv.Itoa(i), CreatedAt: now - int64(300-i)*1000,
			Method: "POST", Path: "/v1/chat/completions", Model: "m" + strconv.Itoa(i%2), Status: st, Error: map[bool]string{true: "boom", false: ""}[st == 500]})
	}
	if err := db.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	p, err := db.ListLogsPage(ctx, LogFilter{Limit: 10})
	if err != nil || p.Total != 300 || p.TotalKind != "exact" || p.Logs[0].RequestID != "id-299" {
		t.Fatalf("page %+v %v", p, err)
	}
	if p, _ := db.ListLogsPage(ctx, LogFilter{Q: "up-7", Limit: 10}); p.Total != 1 || p.Logs[0].RequestID != "id-7" {
		t.Fatalf("id search %+v", p)
	}
	if p, _ := db.ListLogsPage(ctx, LogFilter{Q: "boom", Limit: 10}); p.Total != 100 {
		t.Fatalf("error text search %+v", p.Total)
	}
	// Force the count over budget: the estimate comes from the rollups.
	old := CountBudget
	CountBudget = 0
	defer func() { CountBudget = old }()
	if p, _ := db.ListLogsPage(ctx, LogFilter{Model: "m1", Limit: 10}); p.TotalKind != "approx" || p.Total != 150 {
		t.Fatalf("approx %+v", p)
	}
	if p, _ := db.ListLogsPage(ctx, LogFilter{Status: "error", Limit: 10}); p.TotalKind != "approx" || p.Total != 100 {
		t.Fatalf("approx errors %+v", p)
	}
	if p, _ := db.ListLogsPage(ctx, LogFilter{Path: "/v1/chat", Limit: 10}); p.TotalKind != "at_least" || p.Total != 11 {
		t.Fatalf("at_least %+v", p)
	}
	ls, err := db.LogStorageStats(ctx)
	if err != nil || ls.Count != 300 || ls.OldestAt != now-300_000 || ls.NewestAt != now-1000 || ls.AutoVacuum != "incremental" {
		t.Fatalf("storage stats %+v %v", ls, err)
	}
	if n := db.CountLogsBefore(ctx, now-200_000); n != 100 {
		t.Fatalf("count before %d", n)
	}
	n, err := db.DeleteLogsBeforeProgress(ctx, now-200_000, nil)
	if err != nil || n != 100 {
		t.Fatalf("deleted %d %v", n, err)
	}
	if _, err := db.IncrementalVacuum(ctx, func(done, total int64) {}); err != nil {
		t.Fatal(err)
	}
	if freed, err := db.IncrementalVacuumIfEnabled(ctx, nil); err != nil || freed < 0 {
		t.Fatalf("vacuum %d %v", freed, err)
	}
}
