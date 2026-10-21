package admin

import (
	"context"
	"net/http"
	"strconv"
	"testing"

	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/store"
)

// A suspected dead key whose background check disabled it after the log
// row was written: the detail reads the key.auto_disable event that names
// the request, so the attempt shows as "disable" (pm-spec 6.11). A key
// disabled because of another request, or not at all, stays "verify".
func TestDetailVerifiedAttemptShowsDisable(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var keys []*store.Key
	for _, n := range []string{"dead", "other", "ok"} {
		k := &store.Key{Name: n, Key: "xai-" + n, BaseURL: h.up.URL, Weight: 1, Enabled: true}
		if err := h.db.CreateKey(ctx, k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	now := store.NowMs()
	l := &store.Log{RequestID: "req-v", CreatedAt: now - 1000, Method: "POST", Path: "/v1/chat/completions",
		Model: "grok-4.5", Status: 200, KeyID: keys[2].ID, KeyName: "ok", LatencyMs: 900, EndAt: now - 100,
		Attempts: []store.Attempt{
			{Seq: 1, KeyID: keys[0].ID, KeyName: "dead", Status: 400, Error: "Incorrect API key provided", Action: pool.ActionVerify, StartedAt: now - 1000, LatencyMs: 100},
			{Seq: 2, KeyID: keys[1].ID, KeyName: "other", Status: 400, Error: "Incorrect API key provided", Action: pool.ActionVerify, StartedAt: now - 900, LatencyMs: 100},
			{Seq: 3, KeyID: keys[2].ID, KeyName: "ok", Status: 200, Action: pool.ActionSuccess, StartedAt: now - 800},
		}}
	if err := h.db.InsertLogs(ctx, []*store.Log{l}); err != nil {
		t.Fatal(err)
	}
	ev := func(k *store.Key, rid string) *store.Event {
		return &store.Event{Kind: store.EventSystem, Level: store.LevelError, Action: "key.auto_disable", Actor: "system",
			TargetType: "key", TargetID: strconv.FormatInt(k.ID, 10), TargetName: k.Name, RequestID: rid,
			Detail: map[string]any{"reason": "HTTP 400: Incorrect API key provided"}}
	}
	// The dead key was disabled by this request (after the row was
	// written); the other key by an unrelated one.
	if err := h.db.AddEvents(ctx, []*store.Event{ev(keys[0], "req-v"), ev(keys[1], "req-elsewhere")}); err != nil {
		t.Fatal(err)
	}
	d := h.must(h.call(http.MethodGet, "/api/v1/logs/"+itoa(l.ID), ""), 200)
	chain, _ := d["chain"].([]any)
	if len(chain) != 3 {
		t.Fatalf("chain: %v", d["chain"])
	}
	want := []string{pool.ActionDisable, pool.ActionVerify, pool.ActionSuccess}
	for i, c := range chain {
		if a := c.(map[string]any)["action"]; a != want[i] {
			t.Fatalf("chain[%d].action = %v, want %s", i, a, want[i])
		}
	}
	// The retried row itself reports the same.
	first := h.must(h.call(http.MethodGet, "/api/v1/logs/"+itoa(int64(num(chain[0].(map[string]any)["id"]))), ""), 200)
	if first["attempt_action"] != pool.ActionDisable {
		t.Fatalf("row 1 attempt_action = %v", first["attempt_action"])
	}
}
