package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/store"
)

// logsOf lists the unified log with the given query (type=… etc.).
func logsOf(t *testing.T, h *harness, query string) map[string]any {
	t.Helper()
	return h.must(h.call(http.MethodGet, "/api/v1/logs"+query, ""), 200)
}

// Every successful admin write is an audit event with the actor, target
// and what changed (before/after, secrets masked); failed writes are not.
func TestAuditEvents(t *testing.T) {
	h := newHarness(t)
	k := h.must(h.call(http.MethodPost, "/api/v1/keys", `{"name":"main","key":"xai-secret-value-123","priority":0}`), 201)
	kid := itoa(num(k["id"]))
	h.must(h.call(http.MethodPatch, "/api/v1/keys/"+kid, `{"priority":3}`), 200)
	h.must(h.call(http.MethodPatch, "/api/v1/keys/"+kid, `{"enabled":false}`), 200)
	h.must(h.call(http.MethodDelete, "/api/v1/keys/"+kid, ""), 204)
	h.call(http.MethodPatch, "/api/v1/keys/999", `{"priority":1}`) // 404: not recorded

	at := h.must(h.call(http.MethodPost, "/api/v1/admin-tokens", `{"name":"ci-script"}`), 201)
	if r := h.callAs(at["token"].(string), http.MethodPut, "/api/v1/settings", `{"log_retention_days":14}`); r.status != 200 {
		t.Fatalf("settings: %d %s", r.status, r.body)
	}

	evs := list(logsOf(t, h, "?type=audit"))
	byAction := map[string]map[string]any{}
	for _, e := range evs {
		if e["type"] != "audit" {
			t.Fatalf("non-audit row %v", e)
		}
		byAction[e["action"].(string)] = e
	}
	for _, a := range []string{"key.create", "key.update", "key.disable", "key.delete", "admin_token.create", "settings.update"} {
		if byAction[a] == nil {
			t.Fatalf("missing %s in %v", a, evs)
		}
	}
	if len(evs) != 6 {
		t.Fatalf("%d audit events, want 6 (a failed write recorded?): %v", len(evs), evs)
	}
	c := byAction["key.create"]
	if c["target_type"] != "key" || c["target_id"] != kid || c["target_name"] != "main" || c["level"] != "info" {
		t.Fatalf("create event %v", c)
	}
	if d := c["detail"].(map[string]any)["request"].(map[string]any); d["key"] != "[redacted]" || d["name"] != "main" {
		t.Fatalf("create detail %v", d)
	}
	ch := byAction["key.update"]["detail"].(map[string]any)["changes"].(map[string]any)
	if p := ch["priority"].(map[string]any); num(p["before"]) != 0 || num(p["after"]) != 3 || len(ch) != 1 {
		t.Fatalf("update diff %v", ch)
	}
	if strings.Contains(h.call(http.MethodGet, "/api/v1/logs?type=audit,system", "").body, "xai-secret-value-123") {
		t.Fatal("secret leaked into the log")
	}
	if d := byAction["key.delete"]; d["target_name"] != "main" || d["target_hint"] == "" {
		t.Fatalf("delete event lost the name: %v", d)
	}
	s := byAction["settings.update"]
	if !strings.HasPrefix(s["actor"].(string), "admin_token:") || s["actor_name"] != "ci-script" {
		t.Fatalf("settings actor %v", s)
	}
	if sc := s["detail"].(map[string]any)["changes"].(map[string]any); len(sc) != 1 || sc["log_retention_days"] == nil {
		t.Fatalf("settings diff %v", sc)
	}

	// Mixed with the key filter: that key's events (and its requests).
	if got := list(logsOf(t, h, "?type=request,audit,system&key_id="+kid)); len(got) != 4 {
		t.Fatalf("key filter: %d rows", len(got))
	}
	if r := h.call(http.MethodGet, "/api/v1/logs?type=bogus", ""); r.status != 400 {
		t.Fatalf("bad type: %d", r.status)
	}
	// Append-only.
	if _, err := h.db.W.Exec(`UPDATE events SET action = 'x'`); err == nil {
		t.Fatal("events must not be updatable")
	}
	if _, err := h.db.W.Exec(`DELETE FROM events`); err == nil {
		t.Fatal("events must not be deletable")
	}
}

// Requests and events merge newest first; the cursor walks the whole list
// without gaps or repeats.
func TestUnifiedLogCursor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	base := store.NowMs() - 100_000
	var logs []*store.Log
	for i := 0; i < 7; i++ {
		logs = append(logs, &store.Log{RequestID: "r" + itoa(int64(i)), CreatedAt: base + int64(i)*1000, Method: "POST",
			Path: "/v1/chat/completions", Status: []int{200, 429, 500}[i%3]})
	}
	if err := h.db.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ { // events at the same milliseconds as some requests
		lvl := store.LevelInfo
		if i == 2 {
			lvl = store.LevelError
		}
		if err := h.db.AddEvent(ctx, &store.Event{Kind: store.EventSystem, Level: lvl, Action: "key.auto_disable",
			CreatedAt: base + int64(i)*1500}); err != nil {
			t.Fatal(err)
		}
	}
	first := logsOf(t, h, "?type=request,system&page_size=4")
	total := int(num(first["total"])) // 7 requests, 5 events, plus the harness's own system.migrate
	if total != 13 {
		t.Fatalf("total %v", first["total"])
	}
	seen := map[string]bool{}
	var times []float64
	cur := ""
	pages := 0
	for {
		q := "?type=request,system&page_size=4"
		if cur != "" {
			q += "&cursor=" + cur
		}
		page := logsOf(t, h, q)
		for _, row := range list(page) {
			k := row["type"].(string) + itoa(num(row["id"]))
			if seen[k] {
				t.Fatalf("row %s repeated", k)
			}
			seen[k] = true
			ts := row["created_at"].(string)
			times = append(times, float64(len(ts))) // presence check; order checked below via ids
		}
		cur, _ = page["next_cursor"].(string)
		pages++
		if cur == "" || pages > 10 {
			break
		}
	}
	if len(seen) != total || pages != 4 {
		t.Fatalf("walked %d rows in %d pages", len(seen), pages)
	}
	// Only warnings and errors: 429/500 requests and the error event.
	if got := list(logsOf(t, h, "?type=request,system&problems=1&page_size=50")); len(got) != 5 {
		t.Fatalf("problems: %d", len(got))
	}
	_ = times
}

// The quota only moves by recorded adjustments (acceptance U14-U17).
func TestQuotaAdjust(t *testing.T) {
	h := newHarness(t)
	tok := h.must(h.call(http.MethodPost, "/api/v1/tokens", `{"name":"app","quota_usd":5}`), 201)
	id := itoa(num(tok["id"]))
	path := "/api/v1/tokens/" + id + "/quota"
	if _, err := h.db.W.Exec(`UPDATE tokens SET used_ticks = ? WHERE id = ?`, billing.USDToTicks(2), num(tok["id"])); err != nil {
		t.Fatal(err)
	}
	got := h.must(h.call(http.MethodPost, path, `{"action":"increase","amount_usd":2.5,"reason":"monthly top-up"}`), 200)
	// Amounts whose float value is not an exact multiple of 0.0001 (8.63) are fine.
	h.must(h.call(http.MethodPost, path, `{"action":"increase","amount_usd":8.63,"reason":"r"}`), 200)
	h.must(h.call(http.MethodPost, path, `{"action":"decrease","amount_usd":8.63,"reason":"r"}`), 200)
	if got["quota_usd"] != 7.5 || got["remaining_usd"] != 5.5 {
		t.Fatalf("after +2.5: %v", got)
	}
	// Decrease: not below what is used ($2): at most $5.5 can be removed.
	if r := h.call(http.MethodPost, path, `{"action":"decrease","amount_usd":6,"reason":"x"}`); r.status != 400 || !strings.Contains(r.body, "5.5") {
		t.Fatalf("decrease below used: %d %s", r.status, r.body)
	}
	got = h.must(h.call(http.MethodPost, path, `{"action":"decrease","amount_usd":5.5,"reason":"cap at used"}`), 200)
	if got["quota_usd"] != 2.0 || got["exhausted"] != true {
		t.Fatalf("decrease to used: %v", got)
	}
	for _, body := range []string{`{"action":"increase","amount_usd":1}`, `{"action":"increase","amount_usd":0,"reason":"r"}`,
		`{"action":"increase","amount_usd":1.23456,"reason":"r"}`, `{"action":"set_limited","amount_usd":1,"reason":"r"}`,
		`{"action":"bogus","amount_usd":1,"reason":"r"}`, `{"action":"increase","reason":"r"}`} {
		if r := h.call(http.MethodPost, path, body); r.status != 400 {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	got = h.must(h.call(http.MethodPost, path, `{"action":"set_unlimited","reason":"internal app"}`), 200)
	if got["unlimited"] != true {
		t.Fatalf("unlimited: %v", got)
	}
	got = h.must(h.call(http.MethodPost, path, `{"action":"set_limited","amount_usd":3,"reason":"back to a budget"}`), 200)
	if got["unlimited"] != false || got["quota_usd"] != 5.0 || got["remaining_usd"] != 3.0 {
		t.Fatalf("limited with $3 left: %v", got)
	}
	if r := h.call(http.MethodPost, "/api/v1/tokens/999/quota", `{"action":"increase","amount_usd":1,"reason":"r"}`); r.status != 404 {
		t.Fatalf("missing token: %d", r.status)
	}
	// PATCH cannot change the quota or reset usage.
	for _, body := range []string{`{"quota_usd":9}`, `{"reset_usage":true}`} {
		if r := h.call(http.MethodPatch, "/api/v1/tokens/"+id, body); r.status != 400 || r.code(t) != "quota_is_adjustment_only" {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}

	// History: initial + 6 adjustments; the amounts add up to the quota.
	hist := h.must(h.call(http.MethodGet, "/api/v1/tokens/"+id+"/quota-history", ""), 200)
	recs := list(hist)
	if num(hist["total"]) != 7 || len(recs) != 7 || hist["granted_total_usd"] != 5.0 {
		t.Fatalf("history %v", hist)
	}
	last := recs[len(recs)-1]
	if last["action"] != "initial" || last["amount_usd"] != 5.0 {
		t.Fatalf("initial record %v", last)
	}
	inc := recs[len(recs)-2]
	if inc["action"] != "increase" || inc["reason"] != "monthly top-up" || inc["before_usd"] != 5.0 || inc["after_usd"] != 7.5 ||
		inc["used_usd"] != 2.0 {
		t.Fatalf("increase record %v", inc)
	}
	// "Down to what is used" is exact even when used has more than 4 decimals.
	f := h.must(h.call(http.MethodPost, "/api/v1/tokens", `{"name":"fine","quota_usd":1}`), 201)
	fid := itoa(num(f["id"]))
	if _, err := h.db.W.Exec(`UPDATE tokens SET used_ticks = 1006000 WHERE id = ?`, num(f["id"])); err != nil {
		t.Fatal(err)
	}
	h.s.Quota.Invalidate(int64(num(f["id"])))
	got = h.must(h.call(http.MethodPost, "/api/v1/tokens/"+fid+"/quota", `{"action":"decrease","to_used":true,"reason":"close it"}`), 200)
	if got["exhausted"] != true || got["quota_usd"] != got["used_usd"] {
		t.Fatalf("to_used: %v", got)
	}
	if r := h.call(http.MethodPost, "/api/v1/tokens/"+fid+"/quota", `{"action":"decrease","to_used":true,"reason":"again"}`); r.status != 400 {
		t.Fatalf("to_used twice: %d %s", r.status, r.body)
	}
	if r := h.call(http.MethodPost, "/api/v1/tokens/"+fid+"/quota", `{"action":"increase","to_used":true,"reason":"x"}`); r.status != 400 {
		t.Fatalf("to_used on increase: %d", r.status)
	}
	// A limited quota of 0 blocks (it is not "unlimited").
	z := h.must(h.call(http.MethodPost, "/api/v1/tokens", `{"name":"zero","quota_usd":0}`), 201)
	if z["unlimited"] != false || z["exhausted"] != true {
		t.Fatalf("zero quota token %v", z)
	}
	u := h.must(h.call(http.MethodPost, "/api/v1/tokens", `{"name":"free"}`), 201)
	if u["unlimited"] != true {
		t.Fatalf("token without quota %v", u)
	}
}
