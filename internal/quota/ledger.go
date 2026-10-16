// Package quota enforces client-token spending limits strictly.
//
// Usage written to the database lags behind requests (logs are written in
// batches), so admission cannot rely on the stored used_ticks alone. The
// ledger keeps, per token, the spend known to this process plus the amount
// reserved by requests that are still running. A request is admitted only if
// its reservation still fits under the quota; when it finishes the
// reservation is replaced by the actual cost. Concurrent requests therefore
// cannot jointly overspend: the sum of reservations never exceeds what is
// left.
package quota

import (
	"sync"
	"time"

	"github.com/Kiyliy/xai-router/internal/store"
)

// Ledger is safe for concurrent use. It assumes a single gateway process
// owns the database (the supported deployment).
type Ledger struct {
	mu       sync.Mutex
	used     map[int64]int64 // known spend per token (loaded from DB, then kept current)
	inflight map[int64]int64 // reserved by running requests and pending async jobs
	holds    map[string]hold // async job id -> reservation kept until the job is billed
}

type hold struct {
	token   int64
	amount  int64
	expires time.Time
}

// New returns an empty ledger.
func New() *Ledger {
	return &Ledger{used: map[int64]int64{}, inflight: map[int64]int64{}, holds: map[string]hold{}}
}

// Reservation is an admitted request's claim on a token's quota.
type Reservation struct {
	l       *Ledger
	token   int64
	amount  int64
	settled bool
}

// Reserve admits a request for token t that may cost up to amount ticks.
// It returns nil when the token's quota cannot cover it. Tokens without a
// quota are always admitted (their spend is still tracked).
func (l *Ledger) Reserve(t *store.Token, amount int64) *Reservation {
	if amount < 0 {
		amount = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	used, ok := l.used[t.ID]
	if !ok || t.UsedTicks > used {
		// First sight of the token, or the database moved ahead (e.g. after a
		// restart with logs flushed): trust the larger value.
		used = t.UsedTicks
		l.used[t.ID] = used
	}
	if t.QuotaTicks > 0 {
		committed := used + l.inflight[t.ID]
		if committed >= t.QuotaTicks || committed+amount > t.QuotaTicks {
			return nil
		}
	}
	l.inflight[t.ID] += amount
	return &Reservation{l: l, token: t.ID, amount: amount}
}

// Settle releases the reservation and records the actual cost. It is safe
// to call more than once; only the first call counts.
func (r *Reservation) Settle(actual int64) {
	if r == nil {
		return
	}
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.settled {
		return
	}
	r.settled = true
	l.inflight[r.token] -= r.amount
	l.used[r.token] += actual
}

// Hold converts the reservation into a hold on an async job (video
// generation): the amount stays reserved until ReleaseHold is called for
// that job id, or until ttl passes. The request's own actual cost (usually
// zero for a submission) is recorded.
func (r *Reservation) Hold(jobID string, actual int64, ttl time.Duration) {
	if r == nil || jobID == "" {
		r.Settle(actual)
		return
	}
	l := r.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if r.settled {
		return
	}
	r.settled = true
	l.used[r.token] += actual
	l.holds[jobID] = hold{token: r.token, amount: r.amount, expires: time.Now().Add(ttl)}
}

// Amount returns the reserved amount.
func (r *Reservation) Amount() int64 {
	if r == nil {
		return 0
	}
	return r.amount
}

// ReleaseHold frees the hold kept for an async job (its actual cost is
// charged to the request that observed completion).
func (l *Ledger) ReleaseHold(jobID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if h, ok := l.holds[jobID]; ok {
		l.inflight[h.token] -= h.amount
		delete(l.holds, jobID)
	}
}

// ExpireHolds drops holds whose jobs were never polled to completion.
func (l *Ledger) ExpireHolds(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, h := range l.holds {
		if now.After(h.expires) {
			l.inflight[h.token] -= h.amount
			delete(l.holds, id)
		}
	}
}

// Remaining returns how much a token can still spend now (quota minus known
// spend and reservations); -1 means unlimited.
func (l *Ledger) Remaining(t *store.Token) int64 {
	if t.QuotaTicks <= 0 {
		return -1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	used, ok := l.used[t.ID]
	if !ok || t.UsedTicks > used {
		used = t.UsedTicks
	}
	return max(t.QuotaTicks-used-l.inflight[t.ID], 0)
}

// Charge adds spend that happens during a long-lived session (WebSocket)
// without a prior reservation, and reports whether the token is still within
// its quota afterwards.
func (l *Ledger) Charge(t *store.Token, ticks int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.used[t.ID]; !ok {
		l.used[t.ID] = t.UsedTicks
	}
	l.used[t.ID] += ticks
	return t.QuotaTicks <= 0 || l.used[t.ID]+l.inflight[t.ID] < t.QuotaTicks
}

// Invalidate forgets the cached spend of a token so the next request reloads
// it from the database (after an admin edits or resets the token).
func (l *Ledger) Invalidate(tokenID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.used, tokenID)
}

// Reset sets a token's known spend to zero (admin "reset usage").
func (l *Ledger) Reset(tokenID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.used[tokenID] = 0
}
