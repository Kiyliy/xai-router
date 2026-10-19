package pool

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/store"
)

func newPool(t *testing.T, keys ...*store.Key) (*Pool, *store.DB) {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, k := range keys {
		if k.BaseURL == "" {
			k.BaseURL = "https://api.x.ai"
		}
		if k.Weight == 0 {
			k.Weight = 1
		}
		k.Enabled = true
		if err := db.CreateKey(context.Background(), k); err != nil {
			t.Fatal(err)
		}
	}
	p := New(db, config.NewHolder(config.DefaultSettings()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := p.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p, db
}

func ids(ks []*store.Key) []int64 {
	var out []int64
	for _, k := range ks {
		out = append(out, k.ID)
	}
	return out
}

func TestPriorityOrder(t *testing.T) {
	p, _ := newPool(t, &store.Key{Name: "low", Key: "k1", Priority: 5}, &store.Key{Name: "high", Key: "k2", Priority: 0},
		&store.Key{Name: "high2", Key: "k3", Priority: 0})
	for i := 0; i < 50; i++ {
		got := ids(p.Candidates(0))
		if len(got) != 3 || got[2] != 1 {
			t.Fatalf("priority not respected: %v", got)
		}
	}
}

func TestWeightedDistribution(t *testing.T) {
	p, _ := newPool(t, &store.Key{Key: "a", Weight: 9}, &store.Key{Key: "b", Weight: 1})
	first := map[int64]int{}
	for i := 0; i < 2000; i++ {
		first[p.Candidates(0)[0].ID]++
	}
	if first[1] < 1600 || first[1] > 1950 {
		t.Fatalf("weight 9:1 gave %v", first)
	}
}

func TestRoundRobin(t *testing.T) {
	p, _ := newPool(t, &store.Key{Key: "a"}, &store.Key{Key: "b"}, &store.Key{Key: "c"})
	s := config.DefaultSettings()
	s.SelectionStrategy = "round_robin"
	p.settings.Set(s)
	seen := map[int64]int{}
	for i := 0; i < 9; i++ {
		seen[p.Candidates(0)[0].ID]++
	}
	if seen[1] != 3 || seen[2] != 3 || seen[3] != 3 {
		t.Fatalf("round robin uneven: %v", seen)
	}
}

func TestCooldownDisableBreaker(t *testing.T) {
	p, db := newPool(t, &store.Key{Key: "a"}, &store.Key{Key: "b"}, &store.Key{Key: "c"})
	h := http.Header{}
	h.Set("Retry-After", "120")
	if v := p.Report(1, Outcome{Status: 429, Body: "slow down", Header: h}); v.Action != ActionCooldown || !v.Retry {
		t.Fatalf("429 verdict %+v", v)
	}
	if v := p.Report(2, Outcome{Status: 401, Body: `{"error":"Incorrect API key provided"}`, RequestID: "req-bad"}); v.Action != ActionDisable {
		t.Fatalf("bad key verdict %+v", v)
	}
	got := ids(p.Candidates(0))
	if len(got) != 1 || got[0] != 3 {
		t.Fatalf("candidates = %v, want [3]", got)
	}
	st := p.State(1)
	if d := time.Until(st.CooldownUntil); d < 100*time.Second || d > 121*time.Second {
		t.Fatalf("cooldown = %v", d)
	}
	k2, _ := db.GetKey(context.Background(), 2)
	if !k2.AutoDisabled || k2.StatusReason == "" {
		t.Fatalf("auto-disable not persisted: %+v", k2)
	}
	// The state changes are system events linked to the request.
	evs, _ := db.ListEvents(context.Background(), store.EventFilter{Kinds: []string{store.EventSystem}, Limit: 10})
	by := map[string]*store.Event{}
	for _, e := range evs {
		by[e.Action] = e
	}
	if e := by["key.auto_disable"]; e == nil || e.RequestID != "req-bad" || e.Level != store.LevelError || e.TargetID != "2" {
		t.Fatalf("auto-disable event %+v", e)
	}
	if e := by["key.cooldown"]; e == nil || e.Level != store.LevelWarn || e.TargetID != "1" {
		t.Fatalf("cooldown event %+v", e)
	}
	k1, _ := db.GetKey(context.Background(), 1)
	if k1.CooldownUntil == 0 {
		t.Fatal("cooldown not persisted")
	}
	// Breaker: three consecutive 5xx open it.
	for i := 0; i < 2; i++ {
		if v := p.Report(3, Outcome{Status: 502}); v.Action != ActionRetry {
			t.Fatalf("502 #%d verdict %+v", i, v)
		}
	}
	if v := p.Report(3, Outcome{Status: 502}); v.Action != ActionCooldown {
		t.Fatalf("breaker did not open: %+v", v)
	}
	// Everything is resting: the soonest-available key is offered as a last resort.
	if got := ids(p.Candidates(0)); len(got) != 2 || got[0] != 3 {
		t.Fatalf("last-resort candidates = %v", got)
	}
	if err := p.ResetKey(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if got := ids(p.Candidates(0)); len(got) != 1 || got[0] != 2 {
		t.Fatalf("after reset = %v", got)
	}
	// A success closes the breaker.
	p.Report(3, Outcome{Status: 200})
	if st := p.State(3); !st.CooldownUntil.IsZero() {
		t.Fatal("breaker still open after success")
	}
}

func TestClassify(t *testing.T) {
	s := config.DefaultSettings()
	cases := []struct {
		o      Outcome
		action string
	}{
		{Outcome{Status: 200}, ActionSuccess},
		{Outcome{Status: 400, Body: `{"error":"invalid model"}`}, ActionReturn},
		{Outcome{Status: 422, Body: `missing field`}, ActionReturn},
		{Outcome{Status: 404, Body: `not found`}, ActionReturn},
		{Outcome{Status: 401}, ActionDisable},
		{Outcome{Status: 403, Body: "Your team has used all available credits or reached its monthly spending limit"}, ActionVerify},
		{Outcome{Status: 429, Body: "Your team has exhausted its spending limit"}, ActionVerify},
		{Outcome{Status: 429, Body: "slow down"}, ActionCooldown},
		{Outcome{Status: 429, Body: `{"code":"resource-exhausted","error":"The model is currently at capacity due to high demand. Please try again in a few minutes."}`}, ActionRetry},
		{Outcome{Status: 503}, ActionRetry},
		{Outcome{Status: 400, Body: "quota exceeded"}, ActionRetry},
		// Dead-key wording on a 400 is verified out of band, not trusted.
		{Outcome{Status: 400, Body: `{"code":"invalid-argument","error":"Incorrect API key provided: xa***01"}`}, ActionVerify},
		// Echoed client input never matches keywords.
		{Outcome{Status: 404, Body: `{"error":"The model incorrect api key provided does not exist"}`, Echo: []string{"Incorrect API key provided"}}, ActionReturn},
		{Outcome{Status: 403, Body: `{"error":"model no credits blocked is not allowed"}`, Echo: []string{"no credits blocked"}}, ActionRetry},
		{Outcome{NetErr: errors.New("dial tcp: refused")}, ActionRetry},
	}
	for _, c := range cases {
		if v := classify(&s, c.o); v.Action != c.action {
			t.Errorf("%+v: got %s want %s", c.o, v.Action, c.action)
		}
	}
}

func TestConcurrencyAndRPM(t *testing.T) {
	p, _ := newPool(t, &store.Key{Key: "a", MaxConcurrency: 2}, &store.Key{Key: "b", RPMLimit: 2})
	l1, _ := p.Acquire(1)
	l2, _ := p.Acquire(1)
	if l, _ := p.Acquire(1); l != nil {
		t.Fatal("concurrency limit not enforced")
	}
	l1.Release()
	if l, _ := p.Acquire(1); l == nil {
		t.Fatal("slot not released")
	}
	l2.Release()
	for i := 0; i < 2; i++ {
		l, _ := p.Acquire(2)
		if l == nil {
			t.Fatal("rpm too strict")
		}
		l.Release()
	}
	if l, why := p.Acquire(2); l != nil || why == "" {
		t.Fatal("rpm limit not enforced")
	}
	p.now = func() time.Time { return time.Now().Add(61 * time.Second) }
	if l, _ := p.Acquire(2); l == nil {
		t.Fatal("rpm window did not slide")
	}
}

func TestBoundKey(t *testing.T) {
	p, _ := newPool(t, &store.Key{Key: "a"}, &store.Key{Key: "b"})
	p.Report(2, Outcome{Status: 401})
	if got := ids(p.Candidates(2)); len(got) != 1 || got[0] != 2 {
		t.Fatalf("bound key must be returned even when disabled: %v", got)
	}
	if got := p.Candidates(99); len(got) != 0 {
		t.Fatalf("unknown bound key: %v", got)
	}
}

func TestClientErrorsDoNotTripBreaker(t *testing.T) {
	p, _ := newPool(t, &store.Key{Key: "a"})
	for i := 0; i < 10; i++ {
		if v := p.Report(1, Outcome{Status: 403, Body: "forbidden for this model"}); v.Action != ActionRetry || v.Penalty {
			t.Fatalf("403 verdict %+v", v)
		}
	}
	if st := p.State(1); !st.CooldownUntil.IsZero() || st.AutoDisabled {
		t.Fatalf("client errors opened the breaker: %+v", st)
	}
}

func TestVerifyBeforeDisable(t *testing.T) {
	p, db := newPool(t, &store.Key{Key: "a"}, &store.Key{Key: "b"})
	calls := make(chan int64, 4)
	p.Verify = func(_ context.Context, k *store.Key) (bool, string) {
		calls <- k.ID
		return k.ID == 1, "verified: invalid"
	}
	body := `{"error":"Incorrect API key provided"}`
	p.Report(1, Outcome{Status: 400, Body: body, RequestID: "req-verify"})
	p.Report(2, Outcome{Status: 400, Body: body})
	<-calls
	<-calls
	deadline := time.Now().Add(2 * time.Second)
	for !p.State(1).AutoDisabled && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !p.State(1).AutoDisabled || p.State(2).AutoDisabled {
		t.Fatalf("key1 %+v key2 %+v", p.State(1), p.State(2))
	}
	// The verified disable still names the request that raised the suspicion.
	var evs []*store.Event
	for time.Now().Before(deadline) {
		evs, _ = db.ListEvents(context.Background(), store.EventFilter{Action: "key.auto_disable", Limit: 5})
		if len(evs) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(evs) != 1 || evs[0].RequestID != "req-verify" || evs[0].TargetID != "1" {
		t.Fatalf("auto-disable events %+v", evs)
	}
}
