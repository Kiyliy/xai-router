package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestKeysCRUD(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	bad := []struct{ body, want string }{
		{`{"key":"has space"}`, "whitespace"},
		{`{}`, "key is required"},
		{`{"key":"k-w","weight":0}`, "weight"},
		{`{"key":"k-l","max_concurrency":-1}`, "limits"},
		{`{"key":"k-u","base_url":"ftp://example.com"}`, "base_url must be"},
	}
	for _, b := range bad {
		r := h.call(http.MethodPost, "/api/v1/keys", b.body)
		if r.status != 400 || r.code(t) != "invalid_key" || !strings.Contains(r.body, b.want) {
			t.Fatalf("%s: %d %s", b.body, r.status, r.body)
		}
	}
	// The upstream policy is enforced on key URLs.
	strict := testSettings()
	strict.UpstreamAllowedHosts = []string{"api.x.ai"}
	prev := *h.s.Settings.Get()
	h.s.Settings.Set(strict)
	if r := h.call(http.MethodPost, "/api/v1/keys", `{"key":"k-p","base_url":"http://127.0.0.1:9"}`); r.status != 400 ||
		!strings.Contains(r.body, "base_url:") {
		t.Fatalf("policy: %d %s", r.status, r.body)
	}
	h.s.Settings.Set(prev)

	k := h.must(h.call(http.MethodPost, "/api/v1/keys", `{"key":" xai-abc123def456 ","priority":2,"notes":"n"}`), 201)
	if k["name"] != "key-f456" || k["base_url"] != h.up.URL || k["status"] != "active" || k["key_masked"] != "xai-ab…f456" {
		t.Fatalf("created: %v", k)
	}
	id := itoa(num(k["id"]))
	if r := h.call(http.MethodPost, "/api/v1/keys", `{"key":"xai-abc123def456"}`); r.status != 409 || r.code(t) != "conflict" {
		t.Fatalf("duplicate: %d %s", r.status, r.body)
	}
	h.must(h.call(http.MethodPost, "/api/v1/keys", `{"name":"second","key":"xai-second-key-0002","weight":5}`), 201)

	ls := h.must(h.call(http.MethodGet, "/api/v1/keys?page=1&page_size=1", ""), 200)
	if num(ls["total"]) != 2 || len(list(ls)) != 1 || num(ls["page_size"]) != 1 {
		t.Fatalf("paging: %v", ls)
	}
	if ls = h.must(h.call(http.MethodGet, "/api/v1/keys?q=f456", ""), 200); num(ls["total"]) != 1 {
		t.Fatalf("search: %v", ls)
	}
	h.must(h.call(http.MethodGet, "/api/v1/keys?page=0&page_size=999", ""), 200)

	if r := h.call(http.MethodGet, "/api/v1/keys/abc", ""); r.status != 400 || r.code(t) != "invalid_id" {
		t.Fatalf("bad id: %d", r.status)
	}
	if r := h.call(http.MethodGet, "/api/v1/keys/999", ""); r.status != 404 || r.code(t) != "not_found" {
		t.Fatalf("missing: %d", r.status)
	}
	for _, m := range []string{http.MethodPatch, http.MethodDelete} {
		if r := h.call(m, "/api/v1/keys/999", `{}`); r.status != 404 {
			t.Fatalf("%s missing: %d", m, r.status)
		}
	}
	if r := h.call(http.MethodPost, "/api/v1/keys/999/reset", ""); r.status != 404 {
		t.Fatalf("reset missing: %d", r.status)
	}

	// Admin disable / enable.
	got := h.must(h.call(http.MethodPatch, "/api/v1/keys/"+id, `{"enabled":false,"name":"renamed"}`), 200)
	if got["status"] != "disabled" || got["status_reason"] != "disabled by admin" || got["name"] != "renamed" {
		t.Fatalf("disabled: %v", got)
	}
	if ls = h.must(h.call(http.MethodGet, "/api/v1/keys?status=disabled", ""), 200); num(ls["total"]) != 1 {
		t.Fatalf("status filter: %v", ls)
	}
	if r := h.call(http.MethodPatch, "/api/v1/keys/"+id, `{"weight":0}`); r.status != 400 {
		t.Fatalf("invalid patch: %d", r.status)
	}
	if r := h.call(http.MethodPatch, "/api/v1/keys/"+id, `{"key":"xai-second-key-0002"}`); r.status != 409 {
		t.Fatalf("conflicting patch: %d", r.status)
	}
	got = h.must(h.call(http.MethodPatch, "/api/v1/keys/"+id, `{"enabled":true}`), 200)
	if got["status"] != "active" || got["status_reason"] != "" {
		t.Fatalf("re-enabled: %v", got)
	}

	// Automatic states are reported and can be reset.
	kid := num(k["id"])
	_ = h.s.Pool.Disable(ctx, kid, "dead key")
	got = h.must(h.call(http.MethodGet, "/api/v1/keys/"+id, ""), 200)
	if got["status"] != "disabled" || got["status_reason"] != "dead key" || got["auto_disabled"] != true {
		t.Fatalf("auto-disabled: %v", got)
	}
	got = h.must(h.call(http.MethodPost, "/api/v1/keys/"+id+"/reset", ""), 200)
	if got["status"] != "active" || got["auto_disabled"] != false {
		t.Fatalf("reset: %v", got)
	}
	h.s.Pool.Report(kid, pool.Outcome{Status: 429, Body: `{"error":"slow"}`})
	got = h.must(h.call(http.MethodGet, "/api/v1/keys/"+id, ""), 200)
	if got["status"] != "cooldown" || got["cooldown_until"] == nil || !strings.Contains(got["status_reason"].(string), "429") {
		t.Fatalf("cooldown: %v", got)
	}
	// Reset also re-enables an admin-disabled key.
	h.must(h.call(http.MethodPatch, "/api/v1/keys/"+id, `{"enabled":false}`), 200)
	if got = h.must(h.call(http.MethodPost, "/api/v1/keys/"+id+"/reset", ""), 200); got["enabled"] != true {
		t.Fatalf("reset of disabled key: %v", got)
	}

	h.must(h.call(http.MethodDelete, "/api/v1/keys/"+id, ""), 204)
	if r := h.call(http.MethodGet, "/api/v1/keys/"+id, ""); r.status != 404 {
		t.Fatalf("deleted key still there: %d", r.status)
	}
	if len(h.s.Pool.Candidates(0)) != 1 {
		t.Fatal("pool not reloaded after delete")
	}
}

func TestKeyHealthChecks(t *testing.T) {
	h := newHarness(t)
	good := h.must(h.call(http.MethodPost, "/api/v1/keys", `{"name":"good","key":"xai-good-0001"}`), 201)
	dead := h.must(h.call(http.MethodPost, "/api/v1/keys", `{"name":"dead","key":"xai-dead-0002"}`), 201)

	res := h.must(h.call(http.MethodPost, "/api/v1/keys/"+itoa(num(good["id"]))+"/test", `{"probe":false}`), 200)
	if res["ok"] != true || res["status"] != "ok" || res["probe"] != nil || len(res["models"].([]any)) != 2 {
		t.Fatalf("test: %v", res)
	}
	res = h.must(h.call(http.MethodPost, "/api/v1/keys/"+itoa(num(good["id"]))+"/test", ""), 200)
	if p, _ := res["probe"].(map[string]any); p == nil || num(p["status"]) != 200 {
		t.Fatalf("probe: %v", res)
	}
	got := h.must(h.call(http.MethodGet, "/api/v1/keys/"+itoa(num(good["id"])), ""), 200)
	if lt, _ := got["last_test"].(map[string]any); lt == nil || lt["status"] != "ok" || lt["key_name"] != "good" {
		t.Fatalf("last_test: %v", got["last_test"])
	}
	if r := h.call(http.MethodPost, "/api/v1/keys/999/test", ""); r.status != 404 {
		t.Fatalf("missing key test: %d", r.status)
	}

	all := h.must(h.call(http.MethodPost, "/api/v1/keys/test", `{"probe":false}`), 200)
	if n := len(all["results"].([]any)); n != 2 {
		t.Fatalf("test all: %d results", n)
	}
	one := h.must(h.call(http.MethodPost, "/api/v1/keys/test", `{"ids":[`+itoa(num(dead["id"]))+`],"probe":false}`), 200)
	rs := one["results"].([]any)
	if len(rs) != 1 || rs[0].(map[string]any)["status"] != "invalid" {
		t.Fatalf("test dead: %v", one)
	}
	got = h.must(h.call(http.MethodGet, "/api/v1/keys/"+itoa(num(dead["id"])), ""), 200)
	if got["status"] != "disabled" || got["auto_disabled"] != true {
		t.Fatalf("dead key not disabled: %v", got)
	}
}

func TestImportKeys(t *testing.T) {
	h := newHarness(t)
	h.must(h.call(http.MethodPost, "/api/v1/keys", `{"key":"xai-existing-0009"}`), 201)
	body := `{"keys":"# comment\r\nfirst, xai-imp-0001\n\txai-imp-0002\nxai-imp-0002\nbad key here\nxai-existing-0009\n\n",` +
		`"name_prefix":"team","priority":3,"test":false,"notes":"bulk"}`
	out := h.must(h.call(http.MethodPost, "/api/v1/keys/import", body), 200)
	created := out["created"].([]any)
	skipped := out["skipped"].([]any)
	if len(created) != 2 || len(skipped) != 3 || len(out["tests"].([]any)) != 0 {
		t.Fatalf("import: %v", out)
	}
	if n := created[0].(map[string]any)["name"]; n != "first" {
		t.Fatalf("named key: %v", n)
	}
	if n := created[1].(map[string]any)["name"]; n != "team-0002" {
		t.Fatalf("prefixed key: %v", n)
	}
	reasons := []string{}
	for _, s := range skipped {
		m := s.(map[string]any)
		reasons = append(reasons, itoa(num(m["line"]))+":"+m["reason"].(string))
	}
	want := "4:duplicate in input|5:key is required and must not contain whitespace|6:already in the pool"
	if strings.Join(reasons, "|") != want {
		t.Fatalf("skipped: %v", reasons)
	}
	// Imported keys are tested by default.
	out = h.must(h.call(http.MethodPost, "/api/v1/keys/import", `{"keys":"xai-imp-0003"}`), 200)
	tests := out["tests"].([]any)
	if len(tests) != 1 || tests[0].(map[string]any)["status"] != "ok" {
		t.Fatalf("tests: %v", out)
	}
}

func TestTokensCRUD(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for body, want := range map[string]string{
		`{"name":""}`:                        "name is required",
		`{"name":"a","quota_usd":-1}`:        "quota_usd",
		`{"name":"a","expires_at":5}`:        "expires_at",
		`{"name":"a","expires_at":"lately"}`: "invalid time",
	} {
		r := h.call(http.MethodPost, "/api/v1/tokens", body)
		if r.status != 400 || r.code(t) != "invalid_token" || !strings.Contains(r.body, want) {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	out := h.must(h.call(http.MethodPost, "/api/v1/tokens",
		`{"name":" app ","quota_usd":2.5,"expires_at":"2099-01-02T03:04:05Z","allowed_models":[" grok-4.5 ",""],"notes":"n"}`), 201)
	raw, _ := out["token"].(string)
	if !strings.HasPrefix(raw, clientTokenPrefix) || out["name"] != "app" || out["quota_usd"] != 2.5 ||
		out["remaining_usd"] != 2.5 || out["expired"] != false || out["expires_at"] == nil {
		t.Fatalf("created: %v", out)
	}
	if ms := out["allowed_models"].([]any); len(ms) != 1 || ms[0] != "grok-4.5" {
		t.Fatalf("allowed models: %v", ms)
	}
	id := num(out["id"])
	tok, err := h.db.GetTokenByHash(ctx, secret.Hash(raw))
	if err != nil || tok.ID != id {
		t.Fatalf("stored hash: %v", err)
	}
	h.must(h.call(http.MethodPost, "/api/v1/tokens", `{"name":"other"}`), 201)
	if ls := h.must(h.call(http.MethodGet, "/api/v1/tokens?q=app", ""), 200); num(ls["total"]) != 1 {
		t.Fatalf("search: %v", ls)
	}
	if ls := h.must(h.call(http.MethodGet, "/api/v1/tokens", ""), 200); num(ls["total"]) != 2 {
		t.Fatalf("list: %v", ls)
	}
	h.must(h.call(http.MethodGet, "/api/v1/tokens/"+itoa(id), ""), 200)
	if r := h.call(http.MethodGet, "/api/v1/tokens/12345", ""); r.status != 404 {
		t.Fatalf("missing: %d", r.status)
	}

	if _, err := h.db.W.Exec(`UPDATE tokens SET used_ticks = ? WHERE id = ?`, billing.USDToTicks(3), id); err != nil {
		t.Fatal(err)
	}
	if got := h.must(h.call(http.MethodGet, "/api/v1/tokens/"+itoa(id), ""), 200); got["remaining_usd"] != 0.0 {
		t.Fatalf("over quota remaining: %v", got["remaining_usd"])
	}
	got := h.must(h.call(http.MethodPatch, "/api/v1/tokens/"+itoa(id), `{"expires_at":null,"reset_usage":true,"enabled":false}`), 200)
	if got["expires_at"] != nil || got["used_usd"] != 0.0 || got["enabled"] != false {
		t.Fatalf("patched: %v", got)
	}
	if r := h.call(http.MethodPatch, "/api/v1/tokens/"+itoa(id), `{"name":""}`); r.status != 400 {
		t.Fatalf("invalid patch: %d", r.status)
	}
	if r := h.call(http.MethodPatch, "/api/v1/tokens/12345", `{}`); r.status != 404 {
		t.Fatalf("patch missing: %d", r.status)
	}
	got = h.must(h.call(http.MethodPatch, "/api/v1/tokens/"+itoa(id), `{"expires_at":"2000-01-01"}`), 200)
	if got["expired"] != true {
		t.Fatalf("expired: %v", got)
	}

	regen := h.must(h.call(http.MethodPost, "/api/v1/tokens/"+itoa(id)+"/regenerate", ""), 200)
	if nraw, _ := regen["token"].(string); nraw == raw || !strings.HasPrefix(nraw, clientTokenPrefix) {
		t.Fatalf("regenerate: %v", regen)
	}
	if _, err := h.db.GetTokenByHash(ctx, secret.Hash(raw)); err == nil {
		t.Fatal("old secret still valid")
	}
	if r := h.call(http.MethodPost, "/api/v1/tokens/12345/regenerate", ""); r.status != 404 {
		t.Fatalf("regenerate missing: %d", r.status)
	}
	h.must(h.call(http.MethodDelete, "/api/v1/tokens/"+itoa(id), ""), 204)
	if r := h.call(http.MethodDelete, "/api/v1/tokens/"+itoa(id), ""); r.status != 404 {
		t.Fatalf("second delete: %d", r.status)
	}
}

func TestModelsAPI(t *testing.T) {
	h := newHarness(t)
	all := h.must(h.call(http.MethodGet, "/api/v1/models", ""), 200)
	if int(num(all["total"])) != len(billing.BuiltinModels()) || all["last_sync"] != nil {
		t.Fatalf("list: total %v last_sync %v", all["total"], all["last_sync"])
	}
	if v := h.must(h.call(http.MethodGet, "/api/v1/models?kind=video", ""), 200); num(v["total"]) != 3 {
		t.Fatalf("video models: %v", v["total"])
	}
	for body, want := range map[string]string{
		`{"id":" "}`:                      "id is required",
		`{"id":"m1","kind":"audio"}`:      "kind must be",
		`{"id":"m1","input_price":-1}`:    "prices must be",
		`{"id":"m1","image_price":-0.01}`: "prices must be",
	} {
		r := h.call(http.MethodPost, "/api/v1/models", body)
		if r.status != 400 || r.code(t) != "invalid_model" || !strings.Contains(r.body, want) {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	m := h.must(h.call(http.MethodPost, "/api/v1/models", `{"id":"m1","aliases":["m1-alias"],"input_price":1.5,
"cached_input_price":0.1,"image_input_price":1,"output_price":3,"long_context_threshold":5,"long_input_price":2,
"long_cached_input_price":0.2,"long_output_price":4,"image_price":0.02,"video_price_per_second":0.1,"context_length":1000,
"image_tiers":[{"quality":"low","resolution":"1k","price":0.01}],"source":"sync","synced_at":"x","updated_at":"y"}`), 201)
	if m["source"] != "manual" || m["input_price"] != 1.5 || m["output_price"] != 3.0 || m["image_price"] != 0.02 ||
		len(m["image_tiers"].([]any)) != 1 || m["kind"] != "language" {
		t.Fatalf("created: %v", m)
	}
	if lm := h.s.Prices.Lookup("m1-alias"); lm == nil || lm.InputTicks != billing.USDPerMillionToTicks(1.5) {
		t.Fatal("price table not reloaded")
	}
	// Video tiers and media input prices round-trip and reach the price table.
	v := h.must(h.call(http.MethodPatch, "/api/v1/models/grok-imagine-video-1.5", `{"video_tiers":[{"resolution":"480p",
"price_per_second":0.08},{"resolution":"720p","price_per_second":0.15}],"input_image_price":0.02,"input_video_price_per_second":0.03}`), 200)
	vt, _ := v["video_tiers"].([]any)
	if len(vt) != 2 || vt[1].(map[string]any)["price_per_second"] != 0.15 || v["input_image_price"] != 0.02 ||
		v["input_video_price_per_second"] != 0.03 {
		t.Fatalf("video patch: %v", v)
	}
	if lm := h.s.Prices.Lookup("grok-imagine-video-1.5"); billing.VideoPrice(lm, "720p") != 1_500_000_000 || lm.InputImageTicks != 200_000_000 {
		t.Fatalf("price table: %+v", lm)
	}
	for _, body := range []string{`{"video_tiers":[{"resolution":"","price_per_second":1}]}`,
		`{"video_tiers":[{"resolution":"720p","price_per_second":-1}]}`, `{"input_image_price":-1}`} {
		if r := h.call(http.MethodPatch, "/api/v1/models/grok-imagine-video-1.5", body); r.status != 400 {
			t.Fatalf("%s: %d", body, r.status)
		}
	}
	m = h.must(h.call(http.MethodPatch, "/api/v1/models/grok-4.5", `{"enabled":false,"kind":"language"}`), 200)
	if m["enabled"] != false || m["source"] != "manual" {
		t.Fatalf("patched: %v", m)
	}
	if lm := h.s.Prices.Lookup("grok-4.5"); lm.Enabled {
		t.Fatal("disabled model still enabled in the price table")
	}
	if r := h.call(http.MethodPatch, "/api/v1/models/nope", `{}`); r.status != 404 {
		t.Fatalf("patch missing: %d", r.status)
	}
	if r := h.call(http.MethodPatch, "/api/v1/models/m1", `{"kind":"x"}`); r.status != 400 {
		t.Fatalf("invalid patch: %d", r.status)
	}
	if r := h.call(http.MethodPatch, "/api/v1/models/m1", `{"x":1}`); r.status != 400 {
		t.Fatalf("unknown field: %d", r.status)
	}
	h.must(h.call(http.MethodDelete, "/api/v1/models/m1", ""), 204)
	if r := h.call(http.MethodDelete, "/api/v1/models/m1", ""); r.status != 404 {
		t.Fatalf("second delete: %d", r.status)
	}
	if h.s.Prices.Lookup("m1") != nil {
		t.Fatal("deleted model still priced")
	}
}

func TestModelSync(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if r := h.call(http.MethodPost, "/api/v1/models/sync", ""); r.status != 503 || r.code(t) != "no_upstream_keys" {
		t.Fatalf("no keys: %d %s", r.status, r.body)
	}
	if r := h.call(http.MethodPost, "/api/v1/models/sync", `{"bad":1}`); r.status != 400 {
		t.Fatalf("bad body: %d", r.status)
	}
	dead := h.must(h.call(http.MethodPost, "/api/v1/keys", `{"key":"xai-dead-0001"}`), 201)
	if r := h.call(http.MethodPost, "/api/v1/models/sync", ""); r.status != 502 || r.code(t) != "sync_failed" {
		t.Fatalf("failing sync: %d %s", r.status, r.body)
	}
	_ = h.s.Pool.Disable(ctx, num(dead["id"]), "x")
	h.must(h.call(http.MethodPost, "/api/v1/keys", `{"key":"xai-good-0002"}`), 201)
	res := h.must(h.call(http.MethodPost, "/api/v1/models/sync", `{"overwrite_manual":true}`), 200)
	if res["ok"] != true || len(res["added"].([]any)) != 1 {
		t.Fatalf("sync: %v", res)
	}
	all := h.must(h.call(http.MethodGet, "/api/v1/models", ""), 200)
	if ls, _ := all["last_sync"].(map[string]any); ls == nil || ls["ok"] != true {
		t.Fatalf("last_sync: %v", all["last_sync"])
	}
	// A key outside the upstream policy is refused.
	strict := testSettings()
	strict.UpstreamAllowedHosts = []string{"api.x.ai"}
	h.s.Settings.Set(strict)
	if _, err := h.s.RunSync(ctx, false); err == nil || !strings.Contains(err.Error(), "not in the allowed hosts") {
		t.Fatalf("policy: %v", err)
	}
}

func TestTokenStatusFilterAndHints(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	mk := func(body string) map[string]any { return h.must(h.call(http.MethodPost, "/api/v1/tokens", body), 201) }
	ok := mk(`{"name":"same"}`)
	mk(`{"name":"same","enabled":false}`)
	mk(`{"name":"old","expires_at":"2020-01-01T00:00:00Z"}`)
	spent := mk(`{"name":"spent","quota_usd":0.01}`)
	if _, err := h.db.W.ExecContext(ctx, `UPDATE tokens SET used_ticks = quota_ticks WHERE id = ?`, num(spent["id"])); err != nil {
		t.Fatal(err)
	}
	for status, want := range map[string]int{"": 4, "enabled": 1, "disabled": 1, "expired": 1, "exhausted": 1} {
		got := h.must(h.call(http.MethodGet, "/api/v1/tokens?status="+status, ""), 200)
		if int(num(got["total"])) != want || len(list(got)) != want {
			t.Errorf("status=%q: total %v, want %d", status, got["total"], want)
		}
	}
	if r := h.call(http.MethodGet, "/api/v1/tokens?status=bogus", ""); r.status != 400 {
		t.Fatalf("bad status: %d", r.status)
	}
	// Breakdown and log rows carry the token prefix to tell same names apart.
	if err := h.db.InsertLogs(ctx, []*store.Log{{RequestID: "r1", CreatedAt: store.NowMs(), Method: "POST",
		Path: "/v1/chat/completions", Status: 200, TokenID: num(ok["id"]), TokenName: "same", CostTicks: 5}}); err != nil {
		t.Fatal(err)
	}
	prefix := ok["prefix"].(string)
	bd := h.must(h.call(http.MethodGet, "/api/v1/stats/breakdown?by=token&range=24h", ""), 200)
	if rows := list(bd); len(rows) != 1 || rows[0]["hint"] != prefix || prefix == "" {
		t.Fatalf("breakdown %v (prefix %q)", rows, prefix)
	}
	logs := list(h.must(h.call(http.MethodGet, "/api/v1/logs", ""), 200))
	if len(logs) != 1 || logs[0]["token_prefix"] != prefix { // resolved from the token (the row stores none)
		t.Fatalf("logs %v", logs)
	}
	d := h.must(h.call(http.MethodGet, "/api/v1/logs/"+itoa(num(logs[0]["id"])), ""), 200)
	if d["token_prefix"] != prefix {
		t.Fatalf("detail %v", d["token_prefix"])
	}
}

// Inline edits in the key list send one field at a time: only that field
// changes, the pool's runtime state survives, and the new priority takes
// effect immediately.
func TestKeyPatchSingleField(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	a := h.must(h.call(http.MethodPost, "/api/v1/keys", `{"name":"a","key":"xai-patch-a","priority":0,"weight":3,"notes":"keep"}`), 201)
	b := h.must(h.call(http.MethodPost, "/api/v1/keys", `{"name":"b","key":"xai-patch-b","priority":1}`), 201)
	cands := func() []int64 {
		var ids []int64
		for _, k := range h.s.Pool.Candidates(0) {
			ids = append(ids, k.ID)
		}
		return ids
	}
	if c := cands(); len(c) != 2 || c[0] != num(a["id"]) {
		t.Fatalf("initial order %v", c)
	}
	// Runtime state owned by the pool (cooldown) must not be overwritten.
	until := store.NowMs() + 3_600_000
	if err := h.db.SetKeyState(ctx, num(b["id"]), false, "rate limited", until); err != nil {
		t.Fatal(err)
	}
	k := h.must(h.call(http.MethodPatch, "/api/v1/keys/"+itoa(num(a["id"])), `{"priority":5}`), 200)
	if num(k["priority"]) != 5 || num(k["weight"]) != 3 || k["notes"] != "keep" || k["name"] != "a" {
		t.Fatalf("patch changed other fields: %v", k)
	}
	h.must(h.call(http.MethodPatch, "/api/v1/keys/"+itoa(num(b["id"])), `{"weight":7}`), 200)
	got, _ := h.db.GetKey(ctx, num(b["id"]))
	if got.Weight != 7 || got.CooldownUntil != until || got.StatusReason != "rate limited" {
		t.Fatalf("runtime state overwritten: %+v", got)
	}
	// b is cooling down, so only a is a candidate; once b recovers it comes
	// first (priority 1 < 5).
	if err := h.db.SetKeyState(ctx, num(b["id"]), false, "", 0); err != nil {
		t.Fatal(err)
	}
	if err := h.s.Pool.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if c := cands(); len(c) != 2 || c[0] != num(b["id"]) {
		t.Fatalf("priority change not in effect: %v", c)
	}
	for _, body := range []string{`{"priority":"x"}`, `{"weight":0}`, `{"weight":-1}`} {
		if r := h.call(http.MethodPatch, "/api/v1/keys/"+itoa(num(a["id"])), body); r.status != 400 {
			t.Errorf("%s: %d", body, r.status)
		}
	}
}
