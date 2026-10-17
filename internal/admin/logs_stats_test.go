package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/store"
)

// seedLogs inserts three logs and returns them (with ids).
func seedLogs(t *testing.T, h *harness) (*store.Key, *store.Token, []*store.Log) {
	t.Helper()
	ctx := context.Background()
	k := &store.Key{Name: "kA", Key: "xai-ka", BaseURL: h.up.URL, Weight: 1, Enabled: true}
	if err := h.db.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	tok := &store.Token{Name: "tA", Hash: "hash-a", Prefix: "sk-xr-a", Enabled: true}
	if err := h.db.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	now := store.NowMs()
	logs := []*store.Log{
		{RequestID: "req-1", CreatedAt: now - 1000, Method: "POST", Path: "/v1/chat/completions", Query: "a=1",
			Model: "grok-4.5", Stream: true, Status: 200, TokenID: tok.ID, TokenName: "tA", KeyID: k.ID, KeyName: "kA",
			PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, CostTicks: 1000, CostSource: "computed",
			HasBodies: true, Truncated: true,
			ReqHeaders: `{"Anthropic-Version":"2023-06-01","Authorization":"Bearer sk-xr-…abcd","Content-Type":"application/json"}`,
			ReqBody:    `{"msg":"it's"}`, RespHeaders: `{"X-Up":"1"}`, RespBody: "ok", RespText: "hi",
			Attempts: []store.Attempt{{Seq: 1, KeyID: k.ID, KeyName: "kA", Status: 500, Action: "retry", StartedAt: now},
				{Seq: 2, KeyID: k.ID, KeyName: "kA", Status: 200, Action: "success", StartedAt: now}}},
		{RequestID: "req-2", CreatedAt: now - 500, Method: "GET", Path: "/v1/models", Status: 500, Error: "upstream exploded",
			KeyID: 999, KeyName: "gone", CostSource: "none"},
		{RequestID: "req-3", CreatedAt: now, Method: "GET", Path: "/v1/realtime", Status: 101, WS: true,
			Extra: `{"duration_ms":5,"closed_by":"client"}`, TokenID: tok.ID, CostSource: "none"},
	}
	if err := h.db.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	return k, tok, logs
}

func TestLogsAPI(t *testing.T) {
	h := newHarness(t)
	k, tok, logs := seedLogs(t, h)
	ls := h.must(h.call(http.MethodGet, "/api/v1/logs", ""), 200)
	if num(ls["total"]) != 3 || list(ls)[0]["request_id"] != "req-3" {
		t.Fatalf("list: %v", ls)
	}
	filters := map[string]int64{
		"status=5xx":           1,
		"status=500":           1,
		"status=2xx":           1,
		"model=grok-4.5":       1,
		"path=/v1/chat":        1,
		"q=req-2":              1,
		"q=exploded":           1,
		"key_id=" + itoa(k.ID): 1,
		"token_id=" + itoa(tok.ID) + "&from=" + itoa(logs[0].CreatedAt/1000-5): 2,
		"to=" + itoa(logs[0].CreatedAt):                                        1,
		"from=2000-01-01":                                                      3,
	}
	for q, want := range filters {
		if got := h.must(h.call(http.MethodGet, "/api/v1/logs?"+q, ""), 200); num(got["total"]) != want {
			t.Errorf("%s: total %v want %d", q, got["total"], want)
		}
	}
	for _, q := range []string{"from=yesterday", "to=soon"} {
		if r := h.call(http.MethodGet, "/api/v1/logs?"+q, ""); r.status != 400 || r.code(t) != "invalid_time" {
			t.Fatalf("%s: %d", q, r.status)
		}
	}

	d := h.must(h.call(http.MethodGet, "/api/v1/logs/"+itoa(logs[0].ID), ""), 200)
	if d["request_body"] != `{"msg":"it's"}` || d["response_text"] != "hi" || d["query"] != "a=1" || d["truncated"] != true {
		t.Fatalf("detail: %v", d)
	}
	if at := d["upstream_attempts"].([]any); len(at) != 2 || num(at[0].(map[string]any)["status"]) != 500 {
		t.Fatalf("attempts: %v", d["upstream_attempts"])
	}
	if hd := d["request_headers"].(map[string]any); hd["Content-Type"] != "application/json" {
		t.Fatalf("headers: %v", hd)
	}
	if num(d["attempts"]) != 2 || num(d["token_id"]) != tok.ID || d["websocket"] != false {
		t.Fatalf("summary: %v", d)
	}
	ws := h.must(h.call(http.MethodGet, "/api/v1/logs/"+itoa(logs[2].ID), ""), 200)
	if s, _ := ws["websocket_session"].(map[string]any); s == nil || s["closed_by"] != "client" || ws["key_id"] != nil {
		t.Fatalf("ws detail: %v", ws)
	}
	if r := h.call(http.MethodGet, "/api/v1/logs/99999", ""); r.status != 404 {
		t.Fatalf("missing: %d", r.status)
	}
	if r := h.call(http.MethodGet, "/api/v1/logs/x/curl", ""); r.status != 400 {
		t.Fatalf("bad id: %d", r.status)
	}
	if r := h.call(http.MethodGet, "/api/v1/logs/99999/curl", ""); r.status != 404 {
		t.Fatalf("missing curl: %d", r.status)
	}

	c := h.must(h.call(http.MethodGet, "/api/v1/logs/"+itoa(logs[0].ID)+"/curl", ""), 200)
	curl := c["curl"].(string)
	mustHave := []string{"curl -X POST '" + h.srv.URL + "/v1/chat/completions?a=1' -N",
		`-H "Authorization: Bearer $XR_API_KEY"`, `-H 'Content-Type: application/json'`,
		`-H 'Anthropic-Version: 2023-06-01'`, `--data-raw '{"msg":"it'\''s"}'`}
	for _, s := range mustHave {
		if !strings.Contains(curl, s) {
			t.Fatalf("curl missing %q:\n%s", s, curl)
		}
	}
	if strings.Contains(curl, "sk-xr-") || c["warning"] == nil {
		t.Fatalf("curl: %v", c)
	}
	if strings.Index(curl, "Content-Type") > strings.Index(curl, "Anthropic-Version") {
		t.Fatal("headers not in canonical order")
	}
	c = h.must(h.call(http.MethodGet, "/api/v1/logs/"+itoa(logs[1].ID)+"/curl?base_url=https://gw.example/", ""), 200)
	if curl = c["curl"].(string); !strings.HasPrefix(curl, "curl 'https://gw.example/v1/models'") || c["warning"] != nil ||
		strings.Contains(curl, "--data-raw") {
		t.Fatalf("GET curl: %v", c)
	}
	c = h.must(h.callAs(h.token, http.MethodGet, "/api/v1/logs/"+itoa(logs[1].ID)+"/curl", "", "X-Forwarded-Proto: https"), 200)
	if !strings.HasPrefix(c["curl"].(string), "curl 'https://") {
		t.Fatalf("forwarded proto: %v", c)
	}

	for _, q := range []string{"", "?before=never"} {
		if r := h.call(http.MethodDelete, "/api/v1/logs"+q, ""); r.status != 400 {
			t.Fatalf("delete %q: %d", q, r.status)
		}
	}
	del := h.must(h.call(http.MethodDelete, "/api/v1/logs?wait=true&before="+itoa(logs[1].CreatedAt), ""), 200)
	if num(del["deleted"]) != 1 {
		t.Fatalf("delete: %v", del)
	}
	if ls = h.must(h.call(http.MethodGet, "/api/v1/logs", ""), 200); num(ls["total"]) != 2 {
		t.Fatalf("after delete: %v", ls["total"])
	}
}

func TestStatsAPI(t *testing.T) {
	h := newHarness(t)
	seedLogs(t, h)
	ov := h.must(h.call(http.MethodGet, "/api/v1/stats/overview", ""), 200)
	if num(ov["requests"]) != 3 || num(ov["success"])+num(ov["errors"]) != 3 || ov["range"] != "24h" || ov["cost_usd"] != 1e-7 {
		t.Fatalf("overview: %v", ov)
	}
	if keys := ov["keys"].(map[string]any); num(keys["total"]) != 1 || num(keys["active"]) != 1 {
		t.Fatalf("key counts: %v", keys)
	}
	if toks := ov["tokens"].(map[string]any); num(toks["total"]) != 1 {
		t.Fatalf("token counts: %v", toks)
	}
	for _, p := range []string{"overview", "breakdown?by=model", "timeseries"} {
		sep := "?"
		if strings.Contains(p, "?") {
			sep = "&"
		}
		if r := h.call(http.MethodGet, "/api/v1/stats/"+p+sep+"range=1y", ""); r.status != 400 || r.code(t) != "invalid_range" {
			t.Fatalf("%s bad range: %d %s", p, r.status, r.body)
		}
	}

	names := func(by string) []string {
		out := h.must(h.call(http.MethodGet, "/api/v1/stats/breakdown?range=7d&by="+by, ""), 200)
		var ns []string
		for _, row := range list(out) {
			ns = append(ns, row["name"].(string))
		}
		return ns
	}
	if got := strings.Join(names("key"), ","); !strings.Contains(got, "kA") || !strings.Contains(got, "#999 (deleted)") ||
		!strings.Contains(got, "(none)") {
		t.Fatalf("by key: %s", got)
	}
	if got := strings.Join(names("model"), ","); !strings.Contains(got, "grok-4.5") || !strings.Contains(got, "(unknown)") {
		t.Fatalf("by model: %s", got)
	}
	if got := strings.Join(names("token"), ","); !strings.Contains(got, "tA") {
		t.Fatalf("by token: %s", got)
	}
	if r := h.call(http.MethodGet, "/api/v1/stats/breakdown?by=color", ""); r.status != 400 || r.code(t) != "invalid_group" {
		t.Fatalf("bad group: %d", r.status)
	}

	series := func(q string) map[string]any {
		return h.must(h.call(http.MethodGet, "/api/v1/stats/timeseries?"+q, ""), 200)
	}
	total := func(s map[string]any) (n int64) {
		for _, x := range s["series"].([]any) {
			for _, p := range x.(map[string]any)["points"].([]any) {
				n += num(p.(map[string]any)["requests"])
			}
		}
		return n
	}
	cases := []struct {
		q, interval string
		buckets     int
	}{
		{"", "hour", 24},
		{"range=1h", "5m", 12},
		{"range=30d&tz_offset_minutes=480", "day", 30},
		{"range=24h&interval=5m&group=model", "5m", 288},
		{"range=7d&interval=day&tz_offset_minutes=99999", "day", 7},
	}
	for _, c := range cases {
		s := series(c.q)
		if s["interval"] != c.interval || len(s["buckets"].([]any)) < c.buckets || total(s) != 3 {
			t.Errorf("%q: interval %v, %d buckets, %d requests", c.q, s["interval"], len(s["buckets"].([]any)), total(s))
		}
	}
	s := series("group=key&limit=1")
	var sn []string
	for _, x := range s["series"].([]any) {
		sn = append(sn, x.(map[string]any)["name"].(string))
	}
	if len(sn) != 2 || sn[1] != "other" {
		t.Fatalf("top-N series: %v", sn)
	}
	for q, code := range map[string]string{
		"range=7d&interval=5m": "invalid_interval",
		"interval=week":        "invalid_interval",
		"group=color":          "invalid_group",
		"range=1h&group=color": "invalid_group",
	} {
		if r := h.call(http.MethodGet, "/api/v1/stats/timeseries?"+q, ""); r.status != 400 || r.code(t) != code {
			t.Errorf("%s: %d %s", q, r.status, r.body)
		}
	}
}

func TestEmptyTimeseries(t *testing.T) {
	h := newHarness(t)
	s := h.must(h.call(http.MethodGet, "/api/v1/stats/timeseries?group=model", ""), 200)
	ser := s["series"].([]any)
	if len(ser) != 1 || ser[0].(map[string]any)["name"] != "all" {
		t.Fatalf("empty series: %v", ser)
	}
}

func TestSettingsAPI(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var changed config.Settings
	h.s.OnSettingsChanged = func(s config.Settings) { changed = s }
	got := h.must(h.call(http.MethodGet, "/api/v1/settings", ""), 200)
	if got["upstream_base_url"] != h.up.URL || num(got["max_attempts"]) != 5 {
		t.Fatalf("get: %v", got)
	}
	got = h.must(h.call(http.MethodPut, "/api/v1/settings", `{"max_attempts":3,"retry_keywords":[" Quota ","quota"]}`), 200)
	if num(got["max_attempts"]) != 3 || changed.MaxAttempts != 3 || h.s.Settings.Get().MaxAttempts != 3 {
		t.Fatalf("put: %v", got)
	}
	if kw := h.s.Settings.Get().RetryKeywords; len(kw) != 1 || kw[0] != "quota" {
		t.Fatalf("keywords not normalised: %v", kw)
	}
	loaded, err := LoadSettings(ctx, h.db)
	if err != nil || loaded.MaxAttempts != 3 {
		t.Fatalf("load: %+v %v", loaded.MaxAttempts, err)
	}
	for body, want := range map[string]string{
		`{"bogus":1}`:                  "unknown field",
		`{"max_attempts":0}`:           "max_attempts",
		`{"selection_strategy":"lol"}`: "selection_strategy",
	} {
		r := h.call(http.MethodPut, "/api/v1/settings", body)
		if r.status != 400 || r.code(t) != "invalid_settings" || !strings.Contains(r.body, want) {
			t.Fatalf("%s: %d %s", body, r.status, r.body)
		}
	}
	if r := h.call(http.MethodPut, "/api/v1/settings", strings.Repeat(" ", 2<<20)); r.status != 413 {
		t.Fatalf("large body: %d", r.status)
	}

	// Invalid stored settings fall back to the defaults.
	for _, raw := range []string{`{`, `{"max_attempts":0}`} {
		if err := h.db.SetMeta(ctx, MetaSettings, raw); err != nil {
			t.Fatal(err)
		}
		s, err := LoadSettings(ctx, h.db)
		if err == nil || s.MaxAttempts != config.DefaultSettings().MaxAttempts {
			t.Fatalf("%s: %+v %v", raw, s.MaxAttempts, err)
		}
	}
}

func TestSystemInfo(t *testing.T) {
	h := newHarness(t)
	info := h.must(h.call(http.MethodGet, "/api/v1/system/info", ""), 200)
	if info["version"] != "v-test" || info["commit"] != "abc" || info["needs_password_change"] != false ||
		info["go_version"] == "" || num(info["log_queue_length"]) != 0 {
		t.Fatalf("info: %v", info)
	}
}

func TestParseTimeParam(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		err  bool
	}{
		{"", 0, false},
		{" 1700000000 ", 1700000000_000, false},
		{"1700000000123", 1700000000123, false},
		{"2024-01-02T03:04:05Z", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli(), false},
		{"2024-01-02T03:04", time.Date(2024, 1, 2, 3, 4, 0, 0, time.UTC).UnixMilli(), false},
		{"2024-01-02 03:04:05", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC).UnixMilli(), false},
		{"2024-01-02", time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC).UnixMilli(), false},
		{"tomorrow", 0, true},
	}
	for _, c := range cases {
		got, err := parseTimeParam(c.in)
		if got != c.want || (err != nil) != c.err {
			t.Errorf("%q: %d %v", c.in, got, err)
		}
	}
	if msTime(0) != nil || msTime(1000).Unix() != 1 {
		t.Fatal("msTime")
	}
	if shellQuote("a'b") != `'a'\''b'` {
		t.Fatal("shellQuote")
	}
	if lastN("abc", 4) != "abc" || lastN("abcdef", 2) != "ef" {
		t.Fatal("lastN")
	}
}

// With the database gone, handlers report internal errors in the API format.
func TestInternalErrors(t *testing.T) {
	h := newHarness(t)
	h.db.Close()
	cred := &credential{Type: "admin_token", Name: "ci"}
	handlers := map[string]http.HandlerFunc{
		"GET /api/v1/keys":                        h.s.listKeys,
		"POST /api/v1/keys/test":                  h.s.testKeys,
		"GET /api/v1/tokens":                      h.s.listTokens,
		"GET /api/v1/admin-tokens":                h.s.listAdminTokens,
		"POST /api/v1/admin-tokens":               h.s.createAdminToken,
		"GET /api/v1/models":                      h.s.listModels,
		"POST /api/v1/models":                     h.s.createModel,
		"GET /api/v1/logs":                        h.s.listLogs,
		"DELETE /api/v1/logs?wait=true&before=10": h.s.deleteLogs,
		"GET /api/v1/stats/overview":              h.s.statsOverview,
		"PUT /api/v1/settings":                    h.s.putSettings,
		"POST /api/v1/auth/login":                 h.s.login,
		"PUT /api/v1/auth/password":               h.s.changePassword,
	}
	bodies := map[string]string{
		"POST /api/v1/admin-tokens": `{"name":"x"}`,
		"POST /api/v1/models":       `{"id":"m"}`,
		"PUT /api/v1/settings":      `{}`,
		"POST /api/v1/auth/login":   `{"password":"x"}`,
		"PUT /api/v1/auth/password": `{"old_password":"x","new_password":"yyyyyyyyyy"}`,
	}
	for route, fn := range handlers {
		method, path, _ := strings.Cut(route, " ")
		req := httptest.NewRequest(method, path, strings.NewReader(bodies[route]))
		req = req.WithContext(context.WithValue(req.Context(), credKey, cred))
		rec := httptest.NewRecorder()
		fn(rec, req)
		if rec.Code != 500 || !strings.Contains(rec.Body.String(), "internal_error") {
			t.Errorf("%s: %d %s", route, rec.Code, rec.Body.String())
		}
	}
}

func TestRequireAuthExported(t *testing.T) {
	h := newHarness(t)
	ok := h.s.RequireAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if credFrom(r) == nil {
			t.Error("credential missing from context")
		}
		w.WriteHeader(299)
	}))
	req := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	req.Header.Set("Authorization", "Bearer "+h.token)
	rec := httptest.NewRecorder()
	ok.ServeHTTP(rec, req)
	if rec.Code != 299 {
		t.Fatalf("status %d", rec.Code)
	}
}
