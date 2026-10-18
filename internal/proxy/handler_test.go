package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/store"
)

const chatOK = `{"id":"chatcmpl-1","object":"chat.completion","model":"grok-4.5",` +
	`"choices":[{"message":{"role":"assistant","content":"Hello!","reasoning_content":"think"}}],` +
	`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

func errCode(t *testing.T, body string) string {
	t.Helper()
	var v struct {
		Error struct{ Code, Type, Message string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("not an error body: %q", body)
	}
	return v.Error.Code
}

func TestCORSPreflight(t *testing.T) {
	hs := newHarness(t, nil)
	resp, _ := hs.doAs("", http.MethodOptions, "/v1/chat/completions", "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "X-Api-Key") ||
		resp.Header.Get("Access-Control-Expose-Headers") != "X-Request-Id" {
		t.Fatalf("CORS headers: %v", resp.Header)
	}
}

func TestAuthentication(t *testing.T) {
	hs := newHarness(t, nil)
	up := newMockUp(t, jsonReply(200, chatOK))
	hs.addKey("k1", up.srv.URL, nil)
	disabled, _ := hs.addToken("disabled", func(t *store.Token) { t.Enabled = false })
	expired, _ := hs.addToken("expired", func(t *store.Token) { t.ExpiresAt = time.Now().Add(-time.Minute).UnixMilli() })
	future, _ := hs.addToken("future", func(t *store.Token) { t.ExpiresAt = time.Now().Add(time.Hour).UnixMilli() })
	spent, spentTok := hs.addToken("spent", func(t *store.Token) { t.QuotaTicks = 100 })
	if _, err := hs.db.W.Exec(`UPDATE tokens SET used_ticks = 100 WHERE id = ?`, spentTok.ID); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"grok-4.5","messages":[]}`
	cases := []struct {
		name   string
		hdr    []string
		status int
		code   string
	}{
		{"missing", nil, 401, "missing_api_key"},
		{"non-bearer scheme", []string{"Authorization: Basic abc"}, 401, "missing_api_key"},
		{"invalid", []string{"Authorization: Bearer sk-nope"}, 401, "invalid_api_key"},
		{"disabled", []string{"Authorization: Bearer " + disabled}, 403, "token_disabled"},
		{"expired", []string{"Authorization: Bearer " + expired}, 403, "token_expired"},
		{"quota used", []string{"Authorization: Bearer " + spent}, 429, "insufficient_quota"},
		{"bearer lower-case", []string{"Authorization: bearer " + hs.token}, 200, ""},
		{"x-api-key", []string{"X-Api-Key: " + hs.token}, 200, ""},
		{"not yet expired", []string{"Authorization: Bearer " + future}, 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, b := hs.doAs("", http.MethodPost, "/v1/chat/completions", body, c.hdr...)
			if resp.StatusCode != c.status {
				t.Fatalf("status %d (%s), want %d", resp.StatusCode, b, c.status)
			}
			if resp.Header.Get("X-Request-Id") == "" {
				t.Fatal("missing X-Request-Id")
			}
			if c.code != "" && errCode(t, b) != c.code {
				t.Fatalf("code %q, want %q", errCode(t, b), c.code)
			}
		})
	}
	unauth := 0
	for _, l := range hs.waitLogs(len(cases)) {
		if l.Status == 401 {
			if l.TokenID != 0 || l.Error == "" {
				t.Fatalf("auth failure log: %+v", l)
			}
			unauth++
		}
	}
	if unauth != 3 {
		t.Fatalf("%d 401 logs", unauth)
	}
}

func TestBodyLimit(t *testing.T) {
	hs := newHarness(t, func(s *config.Settings) { s.MaxBodyMB = 1 })
	up := newMockUp(t, jsonReply(200, chatOK))
	hs.addKey("k1", up.srv.URL, nil)
	big := `{"model":"grok-4.5","x":"` + strings.Repeat("a", 1<<20+10) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+hs.token)
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge || errCode(t, rec.Body.String()) != "request_too_large" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	if len(up.requests()) != 0 {
		t.Fatal("oversized request reached the upstream")
	}
	// A body that fails to read for another reason.
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", io.NopCloser(errReader{}))
	req.Header.Set("Authorization", "Bearer "+hs.token)
	rec = httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || errCode(t, rec.Body.String()) != "invalid_request" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestNonStreamingSuccess(t *testing.T) {
	hs := newHarness(t, nil)
	up := newMockUp(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "upstream=1")
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, chatOK)
	})
	k := hs.addKey("k1", up.srv.URL, nil)
	resp, b := hs.do(http.MethodPost, "/v1/chat/completions?trace=1", `{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`,
		"Cookie: secret=1", "X-Grok-Conv-Id: conv-9", "X-Forwarded-For: 1.2.3.4")
	if resp.StatusCode != 200 || b != chatOK {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("X-Upstream") != "yes" {
		t.Fatalf("response headers not filtered: %v", resp.Header)
	}
	got := up.requests()
	if len(got) != 1 {
		t.Fatalf("upstream hits %d", len(got))
	}
	r := got[0]
	if r.Auth != "Bearer "+k.Key || r.Query != "trace=1" || r.Path != "/v1/chat/completions" {
		t.Fatalf("upstream request: %+v", r)
	}
	if r.Header.Get("Cookie") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Grok-Conv-Id") != "conv-9" {
		t.Fatalf("forwarded headers: %v", r.Header)
	}
	l := hs.lastLog(1)
	if l.Status != 200 || l.Model != "grok-4.5" || l.KeyID != k.ID || l.TokenID != hs.tok.ID || l.Stream {
		t.Fatalf("log: %+v", l)
	}
	if l.PromptTokens != 10 || l.CompletionTokens != 5 || l.TotalTokens != 15 || l.UpstreamID != "chatcmpl-1" {
		t.Fatalf("usage: %+v", l)
	}
	if want := int64(10*20000 + 5*60000); l.CostTicks != want || l.CostSource != "computed" {
		t.Fatalf("cost %d %s, want %d", l.CostTicks, l.CostSource, want)
	}
	if l.RespText != "Hello!" || l.ReasoningText != "think" || !strings.Contains(l.ReqBody, `"hi"`) {
		t.Fatalf("bodies: text=%q reasoning=%q req=%q", l.RespText, l.ReasoningText, l.ReqBody)
	}
	if strings.Contains(l.ReqHeaders, hs.token) || !strings.Contains(l.RespHeaders, "X-Upstream") {
		t.Fatalf("headers: %s / %s", l.ReqHeaders, l.RespHeaders)
	}
	if len(l.Attempts) != 1 || l.Attempts[0].Action != pool.ActionSuccess || l.Attempts[0].KeyID != k.ID {
		t.Fatalf("attempts: %+v", l.Attempts)
	}
	tok, _ := hs.db.GetToken(context.Background(), hs.tok.ID)
	if tok.UsedTicks != l.CostTicks || tok.Requests != 1 {
		t.Fatalf("token accounting: %+v", tok)
	}
}

func TestLoggingDisabledStillAccounts(t *testing.T) {
	hs := newHarness(t, func(s *config.Settings) { s.LogEnabled = false })
	up := newMockUp(t, jsonReply(200, chatOK))
	hs.addKey("k1", up.srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5"}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_ = hs.h.Logs.Flush(context.Background())
		tok, _ := hs.db.GetToken(context.Background(), hs.tok.ID)
		if tok.Requests == 1 {
			if tok.UsedTicks == 0 {
				t.Fatal("cost not accounted")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("usage never accounted")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, n, _ := hs.db.ListLogs(context.Background(), store.LogFilter{Limit: 10}); n != 0 {
		t.Fatalf("log stored although logging is disabled: %d", n)
	}
}

func TestNoKeysAndBusy(t *testing.T) {
	hs := newHarness(t, nil)
	resp, b := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5"}`)
	if resp.StatusCode != 503 || errCode(t, b) != "no_upstream_keys" {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	up := newMockUp(t, jsonReply(200, chatOK))
	hs.addKey("k1", up.srv.URL, func(k *store.Key) { k.RPMLimit = 1 })
	if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5"}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	resp, b = hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5"}`)
	if resp.StatusCode != 429 || errCode(t, b) != "upstream_keys_busy" {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
}

func TestQuotaReservationRejects(t *testing.T) {
	hs := newHarness(t, nil)
	up := newMockUp(t, jsonReply(200, chatOK))
	hs.addKey("k1", up.srv.URL, nil)
	small, _ := hs.addToken("small", func(t *store.Token) { t.QuotaTicks = 1000 })
	resp, b := hs.doAs(small, http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5"}`)
	if resp.StatusCode != 429 || errCode(t, b) != "insufficient_quota" || !strings.Contains(b, "does not cover") {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	// Unknown models cannot be priced, so limited tokens may not use them.
	resp, b = hs.doAs(small, http.MethodPost, "/v1/chat/completions", `{"model":"mystery-1"}`)
	if resp.StatusCode != 403 || errCode(t, b) != "model_not_priced" {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	if len(up.requests()) != 0 {
		t.Fatal("rejected requests reached the upstream")
	}
}

// keyedUps creates n upstream keys with ascending priority (tried in order),
// each with its own mock server.
func keyedUps(hs *harness, fns ...http.HandlerFunc) ([]*mockUp, []*store.Key) {
	var ups []*mockUp
	var keys []*store.Key
	for i, fn := range fns {
		up := newMockUp(hs.t, fn)
		k := hs.addKey("k"+string(rune('a'+i)), up.srv.URL, func(k *store.Key) { k.Priority = i })
		ups, keys = append(ups, up), append(keys, k)
	}
	return ups, keys
}

func TestFailover(t *testing.T) {
	const body = `{"model":"grok-4.5","messages":[]}`
	t.Run("server error then success", func(t *testing.T) {
		hs := newHarness(t, nil)
		ups, keys := keyedUps(hs, jsonReply(500, `{"error":"boom"}`), jsonReply(200, chatOK))
		resp, b := hs.do(http.MethodPost, "/v1/chat/completions", body)
		if resp.StatusCode != 200 || b != chatOK {
			t.Fatalf("got %d %s", resp.StatusCode, b)
		}
		l := hs.lastLog(1)
		if len(l.Attempts) != 2 || l.Attempts[0].Action != pool.ActionRetry || l.Attempts[0].Status != 500 ||
			l.Attempts[1].Action != pool.ActionSuccess || l.KeyID != keys[1].ID {
			t.Fatalf("attempts: %+v", l.Attempts)
		}
		if len(ups[0].requests()) != 1 || len(ups[1].requests()) != 1 {
			t.Fatal("each key should be tried once")
		}
	})
	t.Run("401 disables key", func(t *testing.T) {
		hs := newHarness(t, nil)
		_, keys := keyedUps(hs, jsonReply(401, `{"error":"Incorrect API key provided"}`), jsonReply(200, chatOK))
		if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", body); resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
		if !hs.pool.State(keys[0].ID).AutoDisabled {
			t.Fatal("key not auto-disabled")
		}
		l := hs.lastLog(1)
		if l.Attempts[0].Action != pool.ActionDisable {
			t.Fatalf("attempts: %+v", l.Attempts)
		}
	})
	t.Run("429 cools down", func(t *testing.T) {
		hs := newHarness(t, nil)
		_, keys := keyedUps(hs, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":"slow down"}`)
		}, jsonReply(200, chatOK))
		if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", body); resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
		if cu := hs.pool.State(keys[0].ID).CooldownUntil; time.Until(cu) < 100*time.Second {
			t.Fatalf("cooldown until %v", cu)
		}
	})
	t.Run("client error returned as-is", func(t *testing.T) {
		hs := newHarness(t, nil)
		ups, _ := keyedUps(hs, jsonReply(400, `{"error":"bad parameter"}`), jsonReply(200, chatOK))
		resp, b := hs.do(http.MethodPost, "/v1/chat/completions", body)
		if resp.StatusCode != 400 || b != `{"error":"bad parameter"}` {
			t.Fatalf("got %d %s", resp.StatusCode, b)
		}
		if len(ups[1].requests()) != 0 {
			t.Fatal("second key must not be tried for a client error")
		}
		l := hs.lastLog(1)
		if l.Status != 400 || len(l.Attempts) != 1 || l.Attempts[0].Action != pool.ActionReturn || l.RespBody != b {
			t.Fatalf("log: %+v %+v", l, l.Attempts)
		}
	})
	t.Run("same client-class error from two keys stops", func(t *testing.T) {
		hs := newHarness(t, nil)
		deny := jsonReply(403, `{"error":"Permission denied for model grok-4.5"}`)
		ups, _ := keyedUps(hs, deny, deny, jsonReply(200, chatOK))
		resp, b := hs.do(http.MethodPost, "/v1/chat/completions", body)
		if resp.StatusCode != 403 || !strings.Contains(b, "Permission denied") {
			t.Fatalf("got %d %s", resp.StatusCode, b)
		}
		if len(ups[2].requests()) != 0 {
			t.Fatal("third key must not be tried after the same error twice")
		}
		l := hs.lastLog(1)
		if len(l.Attempts) != 2 || l.Attempts[0].Action != pool.ActionRetry || l.Attempts[1].Action != pool.ActionReturn {
			t.Fatalf("attempts: %+v", l.Attempts)
		}
	})
	t.Run("different client-class errors keep going", func(t *testing.T) {
		hs := newHarness(t, nil)
		ups, _ := keyedUps(hs, jsonReply(403, `{"error":"forbidden one"}`), jsonReply(403, `{"error":"forbidden two"}`),
			jsonReply(200, chatOK))
		if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", body); resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
		if len(ups[2].requests()) != 1 {
			t.Fatal("third key should be tried")
		}
	})
	t.Run("out of attempts returns last upstream error", func(t *testing.T) {
		hs := newHarness(t, func(s *config.Settings) { s.MaxAttempts = 2 })
		ups, keys := keyedUps(hs, jsonReply(502, `{"error":"a"}`), jsonReply(503, `{"error":"b"}`), jsonReply(200, chatOK))
		resp, b := hs.do(http.MethodPost, "/v1/chat/completions", body)
		if resp.StatusCode != 503 || b != `{"error":"b"}` {
			t.Fatalf("got %d %s", resp.StatusCode, b)
		}
		if len(ups[2].requests()) != 0 {
			t.Fatal("max_attempts exceeded")
		}
		l := hs.lastLog(1)
		if l.KeyID != keys[1].ID || l.Status != 503 || len(l.Attempts) != 2 {
			t.Fatalf("log: %+v", l)
		}
	})
	t.Run("network error", func(t *testing.T) {
		hs := newHarness(t, nil)
		dead := httptest.NewServer(http.NotFoundHandler())
		dead.Close()
		hs.addKey("dead", dead.URL, nil)
		resp, b := hs.do(http.MethodPost, "/v1/chat/completions", body)
		if resp.StatusCode != 502 || errCode(t, b) != "upstream_error" || !strings.Contains(b, "network error") {
			t.Fatalf("got %d %s", resp.StatusCode, b)
		}
		if resp.Header.Get("Content-Type") != "application/json" {
			t.Fatal("generated error must be JSON")
		}
	})
	t.Run("blocked by upstream policy", func(t *testing.T) {
		hs := newHarness(t, func(s *config.Settings) { s.UpstreamAllowedHosts = []string{"api.x.ai"} })
		up := newMockUp(t, jsonReply(200, chatOK))
		hs.addKey("local", up.srv.URL, nil)
		resp, b := hs.do(http.MethodPost, "/v1/chat/completions", body)
		if resp.StatusCode != 502 || !strings.Contains(b, "blocked by policy") {
			t.Fatalf("got %d %s", resp.StatusCode, b)
		}
		if len(up.requests()) != 0 {
			t.Fatal("blocked upstream was contacted")
		}
	})
	t.Run("timeout", func(t *testing.T) {
		hs := newHarness(t, func(s *config.Settings) { s.RequestTimeoutSeconds = 1 })
		hs.addKey("slow", newMockUp(t, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }).srv.URL, nil)
		resp, b := hs.do(http.MethodPost, "/v1/chat/completions", body)
		if resp.StatusCode != 504 || !strings.Contains(b, "upstream timeout") {
			t.Fatalf("got %d %s", resp.StatusCode, b)
		}
	})
}

func TestModelListFiltering(t *testing.T) {
	hs := newHarness(t, nil)
	up := newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"grok-4.5"},{"id":"grok-4.3"},{"id":"other","aliases":["grok-4.5-latest"]},"junk"]}`)
		case "/v1/language-models":
			_, _ = io.WriteString(w, `{"models":[{"id":"grok-4.3"},{"id":"grok-4.5"}]}`)
		default:
			_, _ = io.WriteString(w, `{"id":"`+strings.TrimPrefix(r.URL.Path, "/v1/models/")+`"}`)
		}
	})
	hs.addKey("k1", up.srv.URL, nil)
	limited, _ := hs.addToken("limited", func(t *store.Token) { t.AllowedModels = []string{"grok-4.5"} })

	_, b := hs.doAs(limited, http.MethodGet, "/v1/models", "")
	var list struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := json.Unmarshal([]byte(b), &list); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, d := range list.Data {
		ids = append(ids, d.ID)
	}
	if strings.Join(ids, ",") != "grok-4.5,other" {
		t.Fatalf("filtered list: %s", b)
	}
	_, b = hs.doAs(limited, http.MethodGet, "/v1/language-models", "")
	if strings.Contains(b, "grok-4.3") || !strings.Contains(b, "grok-4.5") {
		t.Fatalf("language models: %s", b)
	}
	// Unrestricted tokens see everything unchanged.
	if _, b = hs.do(http.MethodGet, "/v1/models", ""); !strings.Contains(b, "grok-4.3") {
		t.Fatalf("unrestricted list: %s", b)
	}
	// Model detail endpoints hide disallowed models.
	before := len(up.requests())
	resp, b := hs.doAs(limited, http.MethodGet, "/v1/models/grok-4.3", "")
	if resp.StatusCode != 404 || errCode(t, b) != "model_not_found" || len(up.requests()) != before {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	if resp, _ := hs.doAs(limited, http.MethodGet, "/v1/models/grok-4.5", ""); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
}

func TestFilterModelListPassthrough(t *testing.T) {
	hs := newHarness(t, nil)
	st := &state{h: hs.h, token: &store.Token{AllowedModels: []string{"grok-4.5"}}}
	get := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	for _, body := range []string{`not json`, `{"data":"x"}`} {
		if got := string(st.filterModelList(get, []byte(body))); got != body {
			t.Fatalf("%q changed to %q", body, got)
		}
	}
	other := httptest.NewRequest(http.MethodGet, "/v1/files", nil)
	if got := string(st.filterModelList(other, []byte(`{"data":[{"id":"x"}]}`))); got != `{"data":[{"id":"x"}]}` {
		t.Fatalf("non-list path changed: %s", got)
	}
}

func TestModelDetailID(t *testing.T) {
	cases := []struct{ method, path, want string }{
		{"GET", "/v1/models/grok-4", "grok-4"},
		{"GET", "/v1/language-models/grok-4.5", "grok-4.5"},
		{"GET", "/v1/models", ""},
		{"GET", "/v1/models/", ""},
		{"GET", "/v1/models/a/b", ""},
		{"POST", "/v1/models/grok-4", ""},
		{"GET", "/v1/files/f1", ""},
	}
	for _, c := range cases {
		if got := modelDetailID(c.method, c.path); got != c.want {
			t.Errorf("%s %s: %q want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestAsyncVideoBindingAndBilling(t *testing.T) {
	hs := newHarness(t, nil)
	var done atomic.Bool
	up := newMockUp(t, nil)
	up.set(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"request_id":"vid-1"}`)
		case !done.Load():
			_, _ = io.WriteString(w, `{"status":"pending"}`)
		default:
			_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://x/v.mp4","duration":8}}`)
		}
	})
	keyA := hs.addKey("a", up.srv.URL, nil)
	quotaRaw, quotaTok := hs.addToken("quota", func(t *store.Token) { t.QuotaTicks = billing.USDToTicks(1) })
	other, _ := hs.addToken("other", nil)

	resp, b := hs.doAs(quotaRaw, http.MethodPost, "/v1/videos/generations", `{"model":"grok-imagine-video","prompt":"x","duration":5}`)
	if resp.StatusCode != 200 {
		t.Fatalf("submit: %d %s", resp.StatusCode, b)
	}
	hs.waitLogs(1)
	vt := billing.USDToTicks(0.05)
	if got, want := hs.h.Quota.Remaining(quotaTok), billing.USDToTicks(1)-5*vt; got != want {
		t.Fatalf("hold: remaining %d want %d", got, want)
	}
	bnd, err := hs.db.GetBinding(context.Background(), "vid-1")
	if err != nil || bnd.KeyID != keyA.ID || bnd.TokenID != quotaTok.ID || bnd.Kind != KindVideo || bnd.Model != "grok-imagine-video" {
		t.Fatalf("binding %+v %v", bnd, err)
	}

	// A preferred key added later must not serve the bound job.
	upB := newMockUp(t, jsonReply(500, `{}`))
	hs.addKey("b", upB.srv.URL, func(k *store.Key) { k.Priority = -1 })

	// Another token cannot see the job.
	resp, b = hs.doAs(other, http.MethodGet, "/v1/videos/vid-1", "")
	if resp.StatusCode != 404 || errCode(t, b) != "not_found" {
		t.Fatalf("foreign poll: %d %s", resp.StatusCode, b)
	}
	hs.waitLogs(2)
	// Pending polls are free.
	if resp, _ := hs.doAs(quotaRaw, http.MethodGet, "/v1/videos/vid-1", ""); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	l := hs.lastLog(3)
	if l.CostTicks != 0 || l.KeyID != keyA.ID || l.Model != "grok-imagine-video" {
		t.Fatalf("pending poll log: %+v", l)
	}
	// The finished poll is billed once.
	done.Store(true)
	for i := 0; i < 2; i++ {
		if resp, _ := hs.doAs(quotaRaw, http.MethodGet, "/v1/videos/vid-1", ""); resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
		hs.waitLogs(4 + i)
	}
	logs := hs.waitLogs(5)
	if logs[3].CostTicks != 8*vt || logs[3].VideoSeconds != 8 || logs[4].CostTicks != 0 {
		t.Fatalf("done polls: %d %v / %d", logs[3].CostTicks, logs[3].VideoSeconds, logs[4].CostTicks)
	}
	if got, want := hs.h.Quota.Remaining(quotaTok), billing.USDToTicks(1)-8*vt; got != want {
		t.Fatalf("after billing: remaining %d want %d", got, want)
	}
	if len(upB.requests()) != 0 {
		t.Fatal("bound job polled with the wrong key")
	}
	for _, r := range up.requests() {
		if r.Auth != "Bearer "+keyA.Key {
			t.Fatalf("wrong key used: %s", r.Auth)
		}
	}
	// Once the key is gone the job is unavailable.
	if err := hs.db.DeleteKey(context.Background(), keyA.ID, ""); err != nil {
		t.Fatal(err)
	}
	_ = hs.pool.Reload(context.Background())
	resp, b = hs.doAs(quotaRaw, http.MethodGet, "/v1/videos/vid-1", "")
	if resp.StatusCode != 503 || errCode(t, b) != "upstream_key_unavailable" {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
}

func TestPreviousResponseOwnership(t *testing.T) {
	hs := newHarness(t, nil)
	up := newMockUp(t, jsonReply(200, `{"id":"resp_1","object":"response","model":"grok-4.5","output":[{"type":"message","content":[{"type":"output_text","text":"hey"}]}],"usage":{"input_tokens":3,"output_tokens":4}}`))
	hs.addKey("k1", up.srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/responses", `{"model":"grok-4.5","input":"hi"}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	l := hs.lastLog(1)
	if l.RespText != "hey" || l.PromptTokens != 3 || l.CompletionTokens != 4 {
		t.Fatalf("responses log: %+v", l)
	}
	other, _ := hs.addToken("other", nil)
	resp, b := hs.doAs(other, http.MethodPost, "/v1/responses", `{"model":"grok-4.5","previous_response_id":"resp_1"}`)
	if resp.StatusCode != 404 {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	hs.waitLogs(2)
	if resp, _ := hs.do(http.MethodPost, "/v1/responses", `{"model":"grok-4.5","previous_response_id":"resp_1"}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	hs.waitLogs(3)
	// Retrievals are free even though they echo the usage.
	if resp, _ := hs.do(http.MethodGet, "/v1/responses/resp_1", ""); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	if l := hs.lastLog(4); l.CostTicks != 0 || l.PromptTokens != 0 {
		t.Fatalf("retrieval billed: %+v", l)
	}
}

func TestVoiceREST(t *testing.T) {
	hs := newHarness(t, nil)
	up := newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/tts":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte{0xff, 0xfb, 0x00})
		case "/v1/stt":
			w.Header().Set("Content-Type", "application/json")
			if strings.Contains(r.Header.Get("X-Grok-Case"), "nodur") {
				_, _ = io.WriteString(w, `{"text":"hi"}`)
				return
			}
			_, _ = io.WriteString(w, `{"text":"hi","duration":36}`)
		}
	})
	hs.addKey("k1", up.srv.URL, nil)
	cases := []struct {
		path, body, hdr string
		cost            int64
		source          string
	}{
		{"/v1/tts", `{"text":"héllo"}`, "", 5 * billing.TTSTicksPerChar, "computed"},
		{"/v1/tts", `{"text":""}`, "", 0, "none"},
		{"/v1/stt", `{"x":1}`, "X-Grok-Case: dur", 36 * billing.STTRestTicksPerHour / 3600, "computed"},
		{"/v1/stt", `{"x":1}`, "X-Grok-Case: nodur", 0, "none"},
	}
	for i, c := range cases {
		var hdr []string
		if c.hdr != "" {
			hdr = append(hdr, c.hdr)
		}
		if resp, b := hs.do(http.MethodPost, c.path, c.body, hdr...); resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", c.path, resp.StatusCode, b)
		}
		l := hs.lastLog(i + 1)
		if l.CostTicks != c.cost || l.CostSource != c.source {
			t.Errorf("case %d %s: cost %d %s, want %d %s", i, c.path, l.CostTicks, l.CostSource, c.cost, c.source)
		}
	}
}

func TestBindingCreatedForDeferredChat(t *testing.T) {
	hs := newHarness(t, nil)
	up := newMockUp(t, jsonReply(200, `{"request_id":"def-1"}`))
	k := hs.addKey("k1", up.srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","deferred":true}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	hs.waitLogs(1)
	b, err := hs.db.GetBinding(context.Background(), "def-1")
	if err != nil || b.Kind != KindDeferred || b.KeyID != k.ID {
		t.Fatalf("binding %+v %v", b, err)
	}
}

// Acceptance E2: without an upstream cost, media is priced at the requested
// resolution plus input fees, and the job's quota hold uses the same tier.
func TestMediaTierPricing(t *testing.T) {
	hs := newHarness(t, nil)
	var done atomic.Bool
	up := newMockUp(t, nil)
	up.set(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/images/generations" || r.URL.Path == "/v1/images/edits":
			_, _ = io.WriteString(w, `{"data":[{"url":"https://x/1.png"}]}`)
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"request_id":"vid-720"}`)
		case !done.Load():
			_, _ = io.WriteString(w, `{"status":"pending"}`)
		default:
			_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://x/v.mp4","duration":5}}`)
		}
	})
	hs.addKey("a", up.srv.URL, nil)
	raw, tok := hs.addToken("quota", func(t *store.Token) { t.QuotaTicks = billing.USDToTicks(10) })

	// grok-imagine-image-quality at 2k: $0.07 (not the 1k $0.05).
	if resp, b := hs.doAs(raw, http.MethodPost, "/v1/images/generations",
		`{"model":"grok-imagine-image-quality","prompt":"x","n":1,"resolution":"2k"}`); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if l := hs.lastLog(1); l.CostTicks != 700_000_000 || l.CostSource != "computed" {
		t.Fatalf("2k image: %d %s", l.CostTicks, l.CostSource)
	}
	// Image edit with two input images: $0.02 + 2 × $0.002.
	if resp, b := hs.doAs(raw, http.MethodPost, "/v1/images/edits",
		`{"model":"grok-imagine-image","prompt":"x","images":[{"url":"https://x/a.png"},{"url":"https://x/b.png"}]}`); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if l := hs.lastLog(2); l.CostTicks != 200_000_000+2*20_000_000 {
		t.Fatalf("edit: %d", l.CostTicks)
	}

	// grok-imagine-video-1.5, 720p, 5 s, image to video: 5 × $0.14 + $0.01.
	before := hs.h.Quota.Remaining(tok)
	if resp, b := hs.doAs(raw, http.MethodPost, "/v1/videos/generations",
		`{"model":"grok-imagine-video-1.5","prompt":"x","duration":5,"resolution":"720p","image":{"url":"https://x/i.png"}}`); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	hs.waitLogs(3)
	want := int64(5*1_400_000_000 + 100_000_000)
	if got := before - hs.h.Quota.Remaining(tok); got != want {
		t.Fatalf("hold %d want %d", got, want)
	}
	done.Store(true)
	if resp, _ := hs.doAs(raw, http.MethodGet, "/v1/videos/vid-720", ""); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	if l := hs.lastLog(4); l.CostTicks != want || l.CostSource != "computed" || l.VideoSeconds != 5 {
		t.Fatalf("video poll: %d %s %v", l.CostTicks, l.CostSource, l.VideoSeconds)
	}
	if got := before - hs.h.Quota.Remaining(tok); got != want {
		t.Fatalf("after billing %d want %d", got, want)
	}
}

// Logs keep the token prefix the request was made with, so regenerating
// the token value later does not relabel old requests.
func TestLogStoresTokenPrefix(t *testing.T) {
	hs := newHarness(t, nil)
	hs.addKey("k1", newMockUp(t, jsonReply(200, chatOK)).srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5"}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	if l := hs.lastLog(1); l.TokenPrefix == "" || l.TokenPrefix != hs.tok.Prefix {
		t.Fatalf("token prefix %q, want %q", l.TokenPrefix, hs.tok.Prefix)
	}
}

// Request and response bodies are capped separately in the log.
func TestBodyCapsPerDirection(t *testing.T) {
	hs := newHarness(t, func(s *config.Settings) { s.LogRequestBodyMaxKB, s.LogResponseBodyMaxKB = 4, 8 })
	big := strings.Repeat("hello world ", 40<<10/12) // ordinary text (long single runs are stripped as base64)
	hs.addKey("k1", newMockUp(t, jsonReply(200, `{"id":"c1","model":"grok-4.5","choices":[{"message":{"content":"`+big+`"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)).srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","messages":[{"role":"user","content":"`+big+`"}]}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	l := hs.lastLog(1)
	if n := len(l.ReqBody); n > 4<<10+200 || n < 4<<10 {
		t.Fatalf("request body stored %d bytes, cap 4 KB", n)
	}
	if n := len(l.RespBody); n > 8<<10+200 || n < 8<<10 {
		t.Fatalf("response body stored %d bytes, cap 8 KB", n)
	}
	if !l.Truncated {
		t.Fatal("truncation not flagged")
	}
}
