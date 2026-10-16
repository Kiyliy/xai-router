package keytest

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/store"
)

// upstream behaviours, selected by the key ("Bearer <key>").
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	reply := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.URL.Path == "/v1/api-key" {
			switch key {
			case "invalid":
				reply(w, 400, `{"code":"Client specified an invalid argument","error":"Incorrect API key provided: in***id."}`)
			case "unauthorized":
				reply(w, 401, `{"error":{"message":"bad credentials"}}`)
			case "team-blocked":
				reply(w, 200, `{"name":"k","team_blocked":true}`)
			case "key-blocked":
				reply(w, 200, `{"name":"k","api_key_blocked":true}`)
			case "key-disabled":
				reply(w, 200, `{"name":"k","api_key_disabled":true}`)
			case "no-credits":
				reply(w, 403, `{"message":"Your team does not have any credits"}`)
			case "suspended":
				reply(w, 403, `account suspended`)
			case "forbidden":
				reply(w, 403, `{"error":"nope"}`)
			case "limited":
				reply(w, 429, `{"error":"slow down"}`)
			case "broken":
				reply(w, 500, `upstream exploded`)
			case "abort":
				panic(http.ErrAbortHandler)
			default:
				reply(w, 200, `{"name":"k","team_id":"t1","acls":["api-key:model:*"]}`)
			}
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			reply(w, 200, `{"data":[{"id":"grok-b"},{"id":"grok-a"}]}`)
		case "/v1/chat/completions":
			switch key {
			case "probe-limited":
				reply(w, 429, `{"error":"rate limit"}`)
			case "probe-abort":
				panic(http.ErrAbortHandler)
			default:
				b, _ := io.ReadAll(r.Body)
				if !strings.Contains(string(b), `"max_tokens":1`) {
					reply(w, 400, `{"error":"probe must be tiny"}`)
					return
				}
				reply(w, 200, `{"model":"grok-4.5","usage":{"prompt_tokens":10,"completion_tokens":1}}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type env struct {
	t   *testing.T
	db  *store.DB
	p   *pool.Pool
	set *config.Holder
	tr  *Tester
	url string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := config.DefaultSettings()
	s.AllowPrivateUpstreams, s.AllowInsecureUpstreams, s.UpstreamAllowedHosts = true, true, nil
	set := config.NewHolder(s)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := pool.New(db, set, log)
	prices := billing.NewTable()
	prices.Set(billing.BuiltinModels())
	return &env{t: t, db: db, p: p, set: set, url: fakeUpstream(t).URL,
		tr: &Tester{DB: db, Pool: p, Prices: prices, Settings: set, Client: &http.Client{}, Log: log}}
}

func (e *env) key(secret string, mutate func(*store.Key)) *store.Key {
	e.t.Helper()
	k := &store.Key{Name: "n-" + secret, Key: secret, BaseURL: e.url + "/", Weight: 1, Enabled: true}
	if mutate != nil {
		mutate(k)
	}
	if err := e.db.CreateKey(context.Background(), k); err != nil {
		e.t.Fatal(err)
	}
	if err := e.p.Reload(context.Background()); err != nil {
		e.t.Fatal(err)
	}
	return k
}

func TestTestStatuses(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cases := []struct {
		key, status, msg string
		disables         bool
	}{
		{"healthy", "ok", "key is healthy", false},
		{"invalid", "invalid", "Incorrect API key", true},
		{"unauthorized", "invalid", "HTTP 401: bad credentials", true},
		{"team-blocked", "blocked", "team is blocked", true},
		{"key-blocked", "blocked", "API key is blocked", true},
		{"key-disabled", "blocked", "API key is disabled", true},
		{"no-credits", "no_credits", "credits", true},
		{"suspended", "blocked", "account suspended", true},
		{"forbidden", "blocked", "HTTP 403: nope", true},
		{"limited", "rate_limited", "slow down", false},
		{"broken", "error", "HTTP 500: upstream exploded", false},
		{"abort", "error", "network error", false},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			k := e.key(c.key, nil)
			res := e.tr.Test(ctx, k, false)
			if res.Status != c.status || !strings.Contains(res.Message, c.msg) || res.OK != (c.status == "ok") ||
				res.KeyID != k.ID || res.KeyName != k.Name || res.TestedAt.IsZero() {
				t.Fatalf("got %+v", res)
			}
			if got := e.p.State(k.ID).AutoDisabled; got != c.disables {
				t.Fatalf("auto-disabled = %v", got)
			}
			stored, _ := e.db.GetKey(ctx, k.ID)
			if !strings.Contains(stored.LastTest, `"status":"`+c.status+`"`) || stored.LastTestedAt == 0 {
				t.Fatalf("last test not stored: %q", stored.LastTest)
			}
		})
	}
}

func TestHealthyDetails(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := e.key("healthy", nil)
	res := e.tr.Test(ctx, k, false)
	if strings.Join(res.Models, ",") != "grok-a,grok-b" || res.APIKey == nil || res.APIKey.TeamID != "t1" || res.Probe != nil {
		t.Fatalf("result: %+v", res)
	}

	// Probe with the configured model, the cheapest priced one and the fallback.
	s := *e.set.Get()
	s.KeyTestModel = "grok-4.5"
	e.set.Set(s)
	res = e.tr.Test(ctx, k, true)
	if res.Probe == nil || res.Probe.Model != "grok-4.5" || res.Probe.Status != 200 || res.Probe.CostUSD <= 0 || !res.OK {
		t.Fatalf("probe: %+v %+v", res, res.Probe)
	}
	s.KeyTestModel = ""
	e.set.Set(s)
	if res = e.tr.Test(ctx, k, true); res.Probe.Model != e.tr.Prices.Cheapest() {
		t.Fatalf("cheapest: %+v", res.Probe)
	}
	e.tr.Prices = billing.NewTable()
	if res = e.tr.Test(ctx, k, true); res.Probe.Model != "grok-build-0.1" {
		t.Fatalf("fallback: %+v", res.Probe)
	}
}

func TestProbeFailures(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	res := e.tr.Test(ctx, e.key("probe-limited", nil), true)
	if res.Status != "rate_limited" || res.Probe == nil || res.Probe.Status != 429 || !strings.Contains(res.Probe.Error, "rate limit") {
		t.Fatalf("limited probe: %+v %+v", res, res.Probe)
	}
	res = e.tr.Test(ctx, e.key("probe-abort", nil), true)
	if res.Status != "error" || !strings.HasPrefix(res.Message, "probe failed") || res.Probe.Error == "" {
		t.Fatalf("aborted probe: %+v %+v", res, res.Probe)
	}
}

func TestHealthyResetsAutoState(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	k := e.key("healthy", nil)
	if err := e.p.Disable(ctx, k.ID, "suspected"); err != nil {
		t.Fatal(err)
	}
	k, _ = e.db.GetKey(ctx, k.ID)
	if res := e.tr.Test(ctx, k, false); !res.OK {
		t.Fatalf("%+v", res)
	}
	if e.p.State(k.ID).AutoDisabled {
		t.Fatal("healthy key not re-enabled")
	}
	if got, _ := e.db.GetKey(ctx, k.ID); got.AutoDisabled || got.StatusReason != "" {
		t.Fatalf("stored state: %+v", got)
	}
}

func TestPolicyBlocked(t *testing.T) {
	e := newEnv(t)
	s := *e.set.Get()
	s.UpstreamAllowedHosts = []string{"api.x.ai"}
	e.set.Set(s)
	k := e.key("healthy", nil)
	res := e.tr.Test(context.Background(), k, true)
	if res.Status != "error" || !strings.Contains(res.Message, "blocked by upstream policy") {
		t.Fatalf("%+v", res)
	}
	if dead, _ := e.tr.Verify(context.Background(), k); dead {
		t.Fatal("verify must not condemn a key it cannot reach")
	}
}

func TestTestManyKeepsOrder(t *testing.T) {
	e := newEnv(t)
	var keys []*store.Key
	for _, s := range []string{"healthy", "invalid", "limited", "broken", "key-blocked"} {
		keys = append(keys, e.key(s, nil))
	}
	res := e.tr.TestMany(context.Background(), keys, false)
	want := []string{"ok", "invalid", "rate_limited", "error", "blocked"}
	for i, r := range res {
		if r.KeyID != keys[i].ID || r.Status != want[i] {
			t.Fatalf("%d: %+v", i, r)
		}
	}
	if len(e.tr.TestMany(context.Background(), nil, false)) != 0 {
		t.Fatal("empty input")
	}
}

func TestVerify(t *testing.T) {
	e := newEnv(t)
	cases := map[string]bool{
		"healthy": false, "team-blocked": true, "key-disabled": true, "invalid": true, "unauthorized": true,
		"no-credits": true, "limited": false, "broken": false, "abort": false,
	}
	for key, want := range cases {
		dead, reason := e.tr.Verify(context.Background(), e.key(key, nil))
		if dead != want || (dead && !strings.HasPrefix(reason, "verified")) {
			t.Errorf("%s: dead=%v reason=%q", key, dead, reason)
		}
	}
	// A key with an unparsable base URL is left alone.
	if dead, _ := e.tr.Verify(context.Background(), &store.Key{Key: "x", BaseURL: "http://bad host"}); dead {
		t.Fatal("bad URL condemned the key")
	}
}

func TestClassify(t *testing.T) {
	long := strings.Repeat("é", 400)
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{402, `{"error":"spending limit reached"}`, "no_credits"},
		{400, `{"error":{"message":"API key is disabled"}}`, "blocked"},
		{400, `{"error":"Invalid API key"}`, "invalid"},
		{429, `too many`, "rate_limited"},
		{403, `{}`, "blocked"},
		{404, `{"message":"no such thing"}`, "error"},
	}
	for _, c := range cases {
		if got, _ := classify(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%d %s: %s want %s", c.status, c.body, got, c.want)
		}
	}
	for body, want := range map[string]string{
		`{"error":"plain"}`:                  "plain",
		`{"error":{"message":"nested"}}`:     "nested",
		`{"error":{"code":1},"message":"m"}`: "m",
		` raw text `:                         "raw text",
		`{"code":"only code"}`:               `{"code":"only code"}`,
	} {
		if got := extractMessage([]byte(body)); got != want {
			t.Errorf("extractMessage(%s) = %q", body, got)
		}
	}
	got := extractMessage([]byte(`{"error":"` + long + `"}`))
	if !strings.HasSuffix(got, "…") || len(got) > 303 || !strings.HasPrefix(got, "éé") {
		t.Fatalf("truncation: %d bytes", len(got))
	}
	if firstNonEmpty("", "b") != "b" || firstNonEmpty() != "" {
		t.Fatal("firstNonEmpty")
	}
}

func TestApplyWithClosedDatabase(t *testing.T) {
	e := newEnv(t)
	k := e.key("invalid", nil)
	h := e.key("healthy", func(k *store.Key) { k.Name = "h" })
	h.AutoDisabled = true
	e.db.Close()
	// Storing the result and updating the pool fail; both are logged only.
	if res := e.tr.Test(context.Background(), k, false); res.Status != "invalid" {
		t.Fatalf("%+v", res)
	}
	if res := e.tr.Test(context.Background(), h, false); res.Status != "ok" {
		t.Fatalf("%+v", res)
	}
}
