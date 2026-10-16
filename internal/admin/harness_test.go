package admin

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/keytest"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/quota"
	"github.com/Kiyliy/xai-router/internal/reqlog"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

const testPassword = "correct-horse-battery"

// fakeXAI answers the upstream endpoints used by key tests and model sync.
func fakeXAI(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.Header.Get("Authorization"), "dead") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"Incorrect API key provided"}`)
			return
		}
		switch r.URL.Path {
		case "/v1/api-key":
			_, _ = io.WriteString(w, `{"name":"k","team_id":"team-1","acls":["api-key:model:*"]}`)
		case "/v1/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"grok-4.5"},{"id":"grok-new"}]}`)
		case "/v1/chat/completions":
			_, _ = io.WriteString(w, `{"id":"x","model":"grok-4.5","usage":{"prompt_tokens":5,"completion_tokens":1}}`)
		case "/v1/language-models":
			_, _ = io.WriteString(w, `{"models":[{"id":"grok-new","prompt_text_token_price":100,"completion_text_token_price":200}]}`)
		case "/v1/image-generation-models", "/v1/video-generation-models", "/v1/embedding-models":
			_, _ = io.WriteString(w, `{"models":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type harness struct {
	t     *testing.T
	db    *store.DB
	s     *Server
	srv   *httptest.Server
	up    *httptest.Server
	token string // admin API token
}

func testSettings() config.Settings {
	s := config.DefaultSettings()
	s.AllowPrivateUpstreams = true
	s.AllowInsecureUpstreams = true
	s.UpstreamAllowedHosts = nil
	return s
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := EnsurePassword(ctx, db, testPassword); err != nil {
		t.Fatal(err)
	}
	if err := billing.SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	up := fakeXAI(t)
	set := testSettings()
	set.UpstreamBaseURL = up.URL
	holder := config.NewHolder(set)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	prices := billing.NewTable()
	if err := prices.Reload(ctx, db); err != nil {
		t.Fatal(err)
	}
	p := pool.New(db, holder, log)
	client := &http.Client{}
	tester := &keytest.Tester{DB: db, Pool: p, Prices: prices, Settings: holder, Client: client, Log: log}
	s := &Server{DB: db, Pool: p, Prices: prices, Settings: holder, Logs: reqlog.NewWriter(db, holder, log, 100),
		Log: log, Quota: quota.New(), Syncer: &billing.Syncer{DB: db, Table: prices, Client: client}, Tester: tester,
		Build: BuildInfo{Version: "v-test", Commit: "abc", Date: "today"}, Started: time.Now()}
	h := &harness{t: t, db: db, s: s, up: up}
	h.srv = httptest.NewServer(s.Handler())
	t.Cleanup(h.srv.Close)
	raw := secret.NewToken("xra-")
	if err := db.CreateAdminToken(ctx, &store.AdminToken{Name: "ci", Hash: secret.Hash(raw), Prefix: secret.Display(raw)}); err != nil {
		t.Fatal(err)
	}
	h.token = raw
	return h
}

type result struct {
	status int
	header http.Header
	body   string
}

func (r result) json(t *testing.T) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(r.body), &v); err != nil {
		t.Fatalf("not JSON (%d): %q", r.status, r.body)
	}
	return v
}

func (r result) code(t *testing.T) string {
	t.Helper()
	e, _ := r.json(t)["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// call performs a request with the harness admin token.
func (h *harness) call(method, path, body string) result {
	h.t.Helper()
	return h.callAs(h.token, method, path, body)
}

func (h *harness) callAs(token, method, path, body string, hdr ...string) result {
	h.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, kv := range hdr {
		k, v, _ := strings.Cut(kv, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, resp.Header, string(b)}
}

// must asserts the status and returns the decoded JSON object (if any).
func (h *harness) must(r result, status int) map[string]any {
	h.t.Helper()
	if r.status != status {
		h.t.Fatalf("status %d, want %d: %s", r.status, status, r.body)
	}
	if strings.TrimSpace(r.body) == "" {
		return nil
	}
	return r.json(h.t)
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func list(v map[string]any) []map[string]any {
	raw, _ := v["data"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		m, _ := x.(map[string]any)
		out = append(out, m)
	}
	return out
}
