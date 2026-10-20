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
	// quotas holds the quota set by an adjustment made while this process
	// runs. It wins over the (possibly cached) token passed to Reserve, so a
	// request admitted after an adjustment is judged by the new quota.
	quotas map[int64]quotaState
}

type quotaState struct {
	ticks     int64
	unlimited bool
}

type hold struct {
	token   int64
	amount  int64
	expires time.Time
}

// New returns an empty ledger.
func New() *Ledger {
	return &Ledger{used: map[int64]int64{}, inflight: map[int64]int64{}, holds: map[string]hold{}, quotas: map[int64]quotaState{}}
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
	if quota, limited := l.quotaOf(t); limited {
		committed := used + l.inflight[t.ID]
		if committed >= quota || committed+amount > quota {
			return nil
		}
	}
	l.inflight[t.ID] += amount
	return &Reservation{l: l, token: t.ID, amount: amount}
}

// Admit returns a zero-amount reservation for token t without checking its
// quota. It is used for requests whose cost is already covered by a hold
// (the poll of an async job the token created): such a poll must reach the
// upstream even when the job's own hold fills the quota, otherwise the job
// could never be delivered, billed or released.
func (l *Ledger) Admit(t *store.Token) *Reservation {
	l.mu.Lock()
	defer l.mu.Unlock()
	if used, ok := l.used[t.ID]; !ok || t.UsedTicks > used {
		l.used[t.ID] = t.UsedTicks
	}
	return &Reservation{l: l, token: t.ID}
}

// Settle releases the reservation and records the actual cost. It is safe
// to call more than once; only the first call counts.
func (r *Reservation) Settle(actual int64) {
	r.SettleReleasing("", actual)
}

// SettleReleasing is Settle that also frees the hold of async job jobID (if
// any) under the same lock, so no concurrent reservation can claim the freed
// hold before the job's actual cost is recorded.
func (r *Reservation) SettleReleasing(jobID string, actual int64) {
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
	if jobID != "" {
		l.releaseHoldLocked(jobID)
	}
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
	l.releaseHoldLocked(jobID)
}

func (l *Ledger) releaseHoldLocked(jobID string) {
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
	l.mu.Lock()
	defer l.mu.Unlock()
	quota, limited := l.quotaOf(t)
	if !limited {
		return -1
	}
	used, ok := l.used[t.ID]
	if !ok || t.UsedTicks > used {
		used = t.UsedTicks
	}
	return max(quota-used-l.inflight[t.ID], 0)
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
	quota, limited := l.quotaOf(t)
	return !limited || l.used[t.ID]+l.inflight[t.ID] < quota
}

// Invalidate forgets the cached spend of a token so the next request reloads
// it from the database (after an admin edits or resets the token).
func (l *Ledger) Invalidate(tokenID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.used, tokenID)
}

// quotaOf returns the quota that applies to t (an adjustment made in this
// process wins over the token passed in). Callers hold l.mu.
func (l *Ledger) quotaOf(t *store.Token) (int64, bool) {
	if q, ok := l.quotas[t.ID]; ok {
		return q.ticks, !q.unlimited
	}
	return t.QuotaTicks, t.Limited
}

// Adjust runs a quota adjustment for a token while holding the ledger's
// lock, so no request is admitted or billed in between. change receives the
// token's known spend (database value as fallback) and returns the new
// quota; on success the ledger applies it to every later reservation.
func (l *Ledger) Adjust(tokenID int64, change func(used func(dbUsed int64) int64) (ticks int64, unlimited bool, err error)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	used := func(dbUsed int64) int64 {
		u, ok := l.used[tokenID]
		if !ok || dbUsed > u {
			u = dbUsed
		}
		return u
	}
	ticks, unlimited, err := change(used)
	if err != nil {
		return err
	}
	l.quotas[tokenID] = quotaState{ticks: ticks, unlimited: unlimited}
	return nil
}
