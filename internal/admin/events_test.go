package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/store"
)

func events(t *testing.T, h *harness, query string) []map[string]any {
	t.Helper()
	return list(h.must(h.call(http.MethodGet, "/api/v1/events"+query, ""), 200))
}

// Every successful admin write is an operation event with the actor,
// target and request fields, secrets redacted; failed writes are not.
func TestOperationEvents(t *testing.T) {
	h := newHarness(t)
	k := h.must(h.call(http.MethodPost, "/api/v1/keys", `{"name":"main","key":"xai-secret-value-123"}`), 201)
	kid := itoa(num(k["id"]))
	h.must(h.call(http.MethodPatch, "/api/v1/keys/"+kid, `{"priority":3}`), 200)
	h.must(h.call(http.MethodDelete, "/api/v1/keys/"+kid, ""), 204)
	h.call(http.MethodPatch, "/api/v1/keys/999", `{"priority":1}`) // 404: not recorded

	// An admin API token is recorded as the actor.
	at := h.must(h.call(http.MethodPost, "/api/v1/admin-tokens", `{"name":"ci-script"}`), 201)
	raw := at["token"].(string)
	if r := h.callAs(raw, http.MethodPut, "/api/v1/settings", `{"log_retention_days":14}`); r.status != 200 {
		t.Fatalf("settings: %d %s", r.status, r.body)
	}

	evs := events(t, h, "?kind=operation")
	byAction := map[string]map[string]any{}
	for _, e := range evs {
		byAction[e["action"].(string)] = e
	}
	for _, a := range []string{"key.create", "key.update", "key.delete", "admin_token.create", "settings.update"} {
		if byAction[a] == nil {
			t.Fatalf("missing %s in %v", a, evs)
		}
	}
	if len(evs) != 5 {
		t.Fatalf("%d operation events, want 5 (failed write recorded?): %v", len(evs), evs)
	}
	c := byAction["key.create"]
	if c["target_type"] != "key" || c["target_id"] != kid || c["target_name"] != "main" || c["actor_name"] != "ci" {
		t.Fatalf("create event %v", c)
	}
	if d := c["detail"].(map[string]any); d["key"] != "[redacted]" || d["name"] != "main" {
		t.Fatalf("create detail %v", d)
	}
	if strings.Contains(h.call(http.MethodGet, "/api/v1/events", "").body, "xai-secret-value-123") {
		t.Fatal("secret leaked into the event log")
	}
	if d := byAction["key.delete"]; d["target_name"] != "main" {
		t.Fatalf("delete event lost the name: %v", d)
	}
	s := byAction["settings.update"]
	if !strings.HasPrefix(s["actor"].(string), "admin_token:") || s["actor_name"] != "ci-script" {
		t.Fatalf("settings actor %v", s)
	}
	if got := events(t, h, "?action=key."); len(got) != 3 {
		t.Fatalf("prefix filter: %d", len(got))
	}
	if got := events(t, h, "?target_type=key&target_id="+kid); len(got) != 3 {
		t.Fatalf("target filter: %d", len(got))
	}
	if r := h.call(http.MethodGet, "/api/v1/events?kind=bogus", ""); r.status != 400 {
		t.Fatalf("bad kind: %d", r.status)
	}

	// The table is append-only.
	if _, err := h.db.W.Exec(`UPDATE events SET action = 'x'`); err == nil {
		t.Fatal("events must not be updatable")
	}
	if _, err := h.db.W.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("events must not be deletable")
	}
}

// The quota only moves by recorded adjustments.
func TestQuotaAdjust(t *testing.T) {
	h := newHarness(t)
	tok := h.must(h.call(http.MethodPost, "/api/v1/tokens", `{"name":"app","quota_usd":5}`), 201)
	id := itoa(num(tok["id"]))
	path := "/api/v1/tokens/" + id + "/quota"

	got := h.must(h.call(http.MethodPost, path, `{"delta_usd":2.5,"note":"top-up"}`), 200)
	if got["quota_usd"] != 7.5 {
		t.Fatalf("after +2.5: %v", got)
	}
	got = h.must(h.call(http.MethodPost, path, `{"delta_usd":-7}`), 200)
	if got["quota_usd"] != 0.5 {
		t.Fatalf("after -7: %v", got)
	}
	for _, body := range []string{`{"delta_usd":-0.5}`, `{"delta_usd":-10}`, `{"delta_usd":0}`, `{}`, `{"delta_usd":1,"unlimited":true}`} {
		if r := h.call(http.MethodPost, path, body); r.status != 400 {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	got = h.must(h.call(http.MethodPost, path, `{"unlimited":true}`), 200)
	if got["quota_usd"] != 0.0 {
		t.Fatalf("unlimited: %v", got)
	}
	if r := h.call(http.MethodPost, path, `{"delta_usd":-1}`); r.status != 400 {
		t.Fatalf("negative on unlimited: %d", r.status)
	}
	got = h.must(h.call(http.MethodPost, path, `{"delta_usd":3}`), 200)
	if got["quota_usd"] != 3.0 {
		t.Fatalf("limit an unlimited token: %v", got)
	}
	if r := h.call(http.MethodPost, "/api/v1/tokens/999/quota", `{"delta_usd":1}`); r.status != 404 {
		t.Fatalf("missing token: %d", r.status)
	}

	// Exactly one event per successful adjustment, with before/after.
	evs := events(t, h, "?action=token.quota_adjust")
	if len(evs) != 4 {
		t.Fatalf("%d quota events: %v", len(evs), evs)
	}
	first := evs[len(evs)-1]
	d := first["detail"].(map[string]any)
	if first["target_id"] != id || first["target_name"] != "app" || d["note"] != "top-up" ||
		num(d["before_ticks"]) != billing.USDToTicks(5) || num(d["after_ticks"]) != billing.USDToTicks(7.5) {
		t.Fatalf("first event %v", first)
	}
	// The ledger enforces the new quota at once (token cache cleared).
	if tk, _ := h.db.GetToken(context.Background(), num(tok["id"])); tk.QuotaTicks != billing.USDToTicks(3) {
		t.Fatalf("stored quota %d", tk.QuotaTicks)
	}
	_ = store.EventOperation
}
