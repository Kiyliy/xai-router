package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// L5: authenticating an admin token or session does not wait for the
// single write connection (held here as by a long VACUUM); the last-used and
// session-slide updates are best-effort and happen once it is free.
func TestAdminAuthDoesNotNeedWriter(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateAdminToken(ctx, &AdminToken{Name: "ci", Hash: "ah"}); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateSession(ctx, "s", NowMs()+3600_000); err != nil {
		t.Fatal(err)
	}
	if _, err := db.W.ExecContext(ctx, `UPDATE sessions SET last_seen_at = last_seen_at - 7200000`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.W.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := db.GetAdminTokenByHash(ctx, "ah"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSession(ctx, "s", 3600_000); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("auth waited %v for the write connection", d)
	}
	conn.Close()
	db.touches.Wait()
	ts, _ := db.ListAdminTokens(ctx)
	var seen int64
	_ = db.R.QueryRowContext(ctx, `SELECT last_seen_at FROM sessions WHERE token_hash = 's'`).Scan(&seen)
	if len(ts) != 1 || ts[0].LastUsedAt == 0 || NowMs()-seen > 60_000 {
		t.Fatalf("bookkeeping not applied later: %+v seen %d", ts, seen)
	}
}
