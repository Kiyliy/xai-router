package quota

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/store"
)

func TestReserveIsStrictUnderConcurrency(t *testing.T) {
	l := New()
	tok := &store.Token{ID: 1, Limited: true, QuotaTicks: 1000}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := l.Reserve(tok, 100); r != nil {
				admitted.Add(1)
				time.Sleep(time.Millisecond)
				r.Settle(100)
			}
		}()
	}
	wg.Wait()
	if l.used[1] > 1000 || l.inflight[1] != 0 {
		t.Fatalf("used %d inflight %d", l.used[1], l.inflight[1])
	}
	if admitted.Load() != 10 {
		t.Fatalf("admitted %d, want exactly 10", admitted.Load())
	}
	if l.Reserve(tok, 0) != nil {
		t.Fatal("exhausted token admitted")
	}
}

func TestSettleReleasesUnusedReservation(t *testing.T) {
	l := New()
	tok := &store.Token{ID: 1, Limited: true, QuotaTicks: 1000}
	r := l.Reserve(tok, 900)
	if l.Reserve(tok, 200) != nil {
		t.Fatal("over-reservation admitted")
	}
	r.Settle(50)
	r.Settle(50) // idempotent
	if got := l.Remaining(tok); got != 950 {
		t.Fatalf("remaining %d", got)
	}
}

func TestHolds(t *testing.T) {
	l := New()
	tok := &store.Token{ID: 1, Limited: true, QuotaTicks: 1000}
	l.Reserve(tok, 600).Hold("job1", 0, time.Hour)
	if l.Reserve(tok, 500) != nil {
		t.Fatal("hold not counted")
	}
	l.ReleaseHold("job1")
	if l.Reserve(tok, 500) == nil {
		t.Fatal("hold not released")
	}
	l.Reserve(tok, 100).Hold("job2", 0, -time.Second)
	l.ExpireHolds(time.Now())
	if _, ok := l.holds["job2"]; ok {
		t.Fatal("expired hold kept")
	}
}

func TestDatabaseAheadIsTrusted(t *testing.T) {
	l := New()
	tok := &store.Token{ID: 1, Limited: true, QuotaTicks: 1000, UsedTicks: 0}
	l.Reserve(tok, 0).Settle(100)
	tok.UsedTicks = 990 // e.g. spent by a previous process
	if l.Reserve(tok, 20) != nil {
		t.Fatal("stale in-memory spend used")
	}
	if l.Charge(tok, 200) {
		t.Fatal("charge beyond quota reported as within quota")
	}
}
