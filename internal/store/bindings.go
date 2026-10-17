package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// Binding ties an upstream resource (async video job, stored response, file,
// batch…) to the key that created it, so follow-up calls use the same key.
type Binding struct {
	ResourceID string
	Kind       string
	KeyID      int64
	TokenID    int64
	Model      string
	Billed     bool
	CreatedAt  int64
	ExpiresAt  int64
}

// Bindings are written on the request path (every /v1/responses call
// creates one). To keep requests from waiting for SQLite's single write lock
// (held by log batches and purges), they are kept in memory and persisted in
// order by a background worker. Reads and the bill-once check consult memory
// first. A crash can lose bindings written in the last moment before it; the
// affected resources then simply fall back to normal key selection.
type bindingCache struct {
	mu      sync.Mutex
	entries map[string]*cachedBinding
	ops     chan bindingOp
	done    chan struct{}
	db      *DB
}

type cachedBinding struct {
	b         Binding
	persisted bool
	added     time.Time
}

type bindingOp struct {
	put    *Binding // insert/replace, or
	billed string   // mark billed
}

// bindingRetention is how long persisted bindings stay in memory.
const bindingRetention = 10 * time.Minute

func newBindingCache(db *DB) *bindingCache {
	c := &bindingCache{entries: map[string]*cachedBinding{}, ops: make(chan bindingOp, 10000),
		done: make(chan struct{}), db: db}
	go c.run()
	return c
}

func (c *bindingCache) run() {
	defer close(c.done)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case op, ok := <-c.ops:
			if !ok {
				return
			}
			c.apply(op)
		case <-tick.C:
			c.evict()
		}
	}
}

func (c *bindingCache) apply(op bindingOp) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if op.put != nil {
		if err := c.db.putBindingDB(ctx, op.put); err == nil {
			c.mu.Lock()
			if e := c.entries[op.put.ResourceID]; e != nil {
				e.persisted = true
			}
			c.mu.Unlock()
		}
		return
	}
	_, _ = c.db.markBilledDB(ctx, op.billed)
}

func (c *bindingCache) evict() {
	cut := time.Now().Add(-bindingRetention)
	c.mu.Lock()
	for id, e := range c.entries {
		if e.persisted && e.added.Before(cut) {
			delete(c.entries, id)
		}
	}
	c.mu.Unlock()
}

// enqueue queues op, or applies it synchronously if the queue is full.
func (c *bindingCache) enqueue(op bindingOp) {
	select {
	case c.ops <- op:
	default:
		c.apply(op)
	}
}

func (c *bindingCache) close() {
	close(c.ops)
	<-c.done
}

// PutBinding inserts or replaces a binding.
func (db *DB) PutBinding(ctx context.Context, b *Binding) error {
	if b.CreatedAt == 0 {
		b.CreatedAt = NowMs()
	}
	if db.bind == nil {
		return db.putBindingDB(ctx, b)
	}
	cp := *b
	db.bind.mu.Lock()
	db.bind.entries[b.ResourceID] = &cachedBinding{b: cp, added: time.Now()}
	db.bind.mu.Unlock()
	db.bind.enqueue(bindingOp{put: &cp})
	return nil
}

func (db *DB) putBindingDB(ctx context.Context, b *Binding) error {
	_, err := db.W.ExecContext(ctx, `INSERT INTO bindings (resource_id, kind, key_id, token_id, model, billed, created_at, expires_at)
VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(resource_id) DO UPDATE SET kind=excluded.kind, key_id=excluded.key_id,
token_id=excluded.token_id, model=excluded.model, billed=MAX(billed, excluded.billed), expires_at=excluded.expires_at`,
		b.ResourceID, b.Kind, b.KeyID, nullID(b.TokenID), b.Model, boolInt(b.Billed), b.CreatedAt, b.ExpiresAt)
	return err
}

// GetBinding returns the binding for a resource id.
func (db *DB) GetBinding(ctx context.Context, id string) (*Binding, error) {
	if db.bind != nil {
		db.bind.mu.Lock()
		e := db.bind.entries[id]
		var cp Binding
		if e != nil {
			cp = e.b
		}
		db.bind.mu.Unlock()
		if e != nil {
			if cp.ExpiresAt <= NowMs() {
				return nil, ErrNotFound
			}
			return &cp, nil
		}
	}
	var b Binding
	var billed int
	err := db.R.QueryRowContext(ctx, `SELECT resource_id, kind, key_id, COALESCE(token_id,0), model, billed, created_at, expires_at
FROM bindings WHERE resource_id = ? AND expires_at > ?`, id, NowMs()).
		Scan(&b.ResourceID, &b.Kind, &b.KeyID, &b.TokenID, &b.Model, &billed, &b.CreatedAt, &b.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.Billed = billed == 1
	return &b, nil
}

// MarkBindingBilled atomically marks a binding billed. It returns true only
// for the first caller, which guarantees an async job is charged once.
func (db *DB) MarkBindingBilled(ctx context.Context, id string) (bool, error) {
	if db.bind != nil {
		db.bind.mu.Lock()
		if e := db.bind.entries[id]; e != nil {
			first := !e.b.Billed
			e.b.Billed = true
			db.bind.mu.Unlock()
			if first {
				// Queued after the binding's own insert, so applied in order.
				db.bind.enqueue(bindingOp{billed: id})
			}
			return first, nil
		}
		db.bind.mu.Unlock()
	}
	return db.markBilledDB(ctx, id)
}

func (db *DB) markBilledDB(ctx context.Context, id string) (bool, error) {
	res, err := db.W.ExecContext(ctx, `UPDATE bindings SET billed = 1 WHERE resource_id = ? AND billed = 0`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FlushBindings waits until every queued binding write is persisted (tests).
func (db *DB) FlushBindings() {
	if db.bind == nil {
		return
	}
	for {
		db.bind.mu.Lock()
		pending := len(db.bind.ops)
		for _, e := range db.bind.entries {
			if !e.persisted {
				pending++
			}
		}
		db.bind.mu.Unlock()
		if pending == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// PurgeExpiredBindings removes expired bindings.
func (db *DB) PurgeExpiredBindings(ctx context.Context) (int64, error) {
	db.FlushBindings()
	if db.bind != nil {
		now := NowMs()
		db.bind.mu.Lock()
		for id, e := range db.bind.entries {
			if e.b.ExpiresAt < now {
				delete(db.bind.entries, id)
			}
		}
		db.bind.mu.Unlock()
	}
	res, err := db.W.ExecContext(ctx, `DELETE FROM bindings WHERE expires_at < ?`, NowMs())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
