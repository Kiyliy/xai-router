package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestBindingCacheWriteBehind(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "b.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	exp := NowMs() + 3600_000
	if err := db.PutBinding(ctx, &Binding{ResourceID: "r1", Kind: "response", KeyID: 7, TokenID: 3, ExpiresAt: exp}); err != nil {
		t.Fatal(err)
	}
	// Visible immediately, before it is persisted.
	if b, err := db.GetBinding(ctx, "r1"); err != nil || b.KeyID != 7 || b.TokenID != 3 {
		t.Fatalf("cached binding %+v %v", b, err)
	}
	first, _ := db.MarkBindingBilled(ctx, "r1")
	second, _ := db.MarkBindingBilled(ctx, "r1")
	if !first || second {
		t.Fatalf("bill once: %v %v", first, second)
	}
	db.FlushBindings()
	// Evicted from memory, the database still answers (billed included).
	db.bind.mu.Lock()
	for _, e := range db.bind.entries {
		e.added = time.Now().Add(-2 * bindingRetention)
	}
	db.bind.mu.Unlock()
	db.bind.evict()
	if n := len(db.bind.entries); n != 0 {
		t.Fatalf("%d entries left after eviction", n)
	}
	if again, _ := db.MarkBindingBilled(ctx, "r1"); again {
		t.Fatal("billed flag lost when persisting")
	}
	// Queued writes survive Close.
	_ = db.PutBinding(ctx, &Binding{ResourceID: "r2", Kind: "video", KeyID: 1, ExpiresAt: exp})
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if b, err := db.GetBinding(ctx, "r2"); err != nil || b.Kind != "video" {
		t.Fatalf("binding lost across restart: %+v %v", b, err)
	}
	// Queue full: written synchronously instead.
	db.bind.close()
	db.bind = &bindingCache{entries: map[string]*cachedBinding{}, ops: make(chan bindingOp), done: make(chan struct{}), db: db}
	_ = db.PutBinding(ctx, &Binding{ResourceID: "r3", Kind: "file", KeyID: 1, ExpiresAt: exp})
	var n int
	_ = db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM bindings WHERE resource_id = 'r3'`).Scan(&n)
	if n != 1 {
		t.Fatal("synchronous fallback did not persist")
	}
	close(db.bind.done)
	db.bind = nil
}
