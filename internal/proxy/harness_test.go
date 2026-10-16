package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/quota"
	"github.com/Kiyliy/xai-router/internal/reqlog"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

// seen is one request observed by a mock upstream.
type seen struct {
	Method, Path, Query, Auth string
	Header                    http.Header
	Body                      string
}

// mockUp is a configurable upstream server that records what it receives.
type mockUp struct {
	srv *httptest.Server
	mu  sync.Mutex
	got []seen
	fn  http.HandlerFunc
}

func newMockUp(t *testing.T, fn http.HandlerFunc) *mockUp {
	t.Helper()
	m := &mockUp{fn: fn}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.got = append(m.got, seen{Method: r.Method, Path: r.URL.Path, Query: r.URL.RawQuery,
			Auth: r.Header.Get("Authorization"), Header: r.Header.Clone(), Body: string(b)})
		fn := m.fn
		m.mu.Unlock()
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		fn(w, r)
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockUp) requests() []seen {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]seen(nil), m.got...)
}

func (m *mockUp) set(fn http.HandlerFunc) {
	m.mu.Lock()
	m.fn = fn
	m.mu.Unlock()
}

type harness struct {
	t     *testing.T
	db    *store.DB
	set   *config.Holder
	pool  *pool.Pool
	h     *Handler
	srv   *httptest.Server
	token string // secret of the default token
	tok   *store.Token
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// testSettings returns defaults that allow local httptest upstreams.
func testSettings() config.Settings {
	s := config.DefaultSettings()
	s.AllowPrivateUpstreams = true
	s.AllowInsecureUpstreams = true
	s.UpstreamAllowedHosts = nil
	return s
}

func newHarness(t *testing.T, mutate func(*config.Settings)) *harness {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := testSettings()
	if mutate != nil {
		mutate(&s)
	}
	set := config.NewHolder(s)
	log := discardLog()
	p := pool.New(db, set, log)
	prices := billing.NewTable()
	prices.Set(append(billing.BuiltinModels(),
		&store.Model{ID: "test-embed", Kind: "embedding", Enabled: true, InputTicks: 10},
		&store.Model{ID: "off-model", Kind: "language", Enabled: false, InputTicks: 10, OutputTicks: 10}))
	hs := &harness{t: t, db: db, set: set, pool: p}
	hs.h = &Handler{DB: db, Pool: p, Prices: prices, Logs: reqlog.NewWriter(db, set, log, 1000), Settings: set,
		Client: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Log:    log, Quota: quota.New()}
	hs.srv = httptest.NewServer(hs.h)
	t.Cleanup(hs.srv.Close)
	hs.token, hs.tok = hs.addToken("default", nil)
	return hs
}

// addKey creates an upstream key pointing at base and reloads the pool.
func (hs *harness) addKey(name, base string, mutate func(*store.Key)) *store.Key {
	hs.t.Helper()
	k := &store.Key{Name: name, Key: "xai-" + name + "-secret", BaseURL: base, Weight: 1, Enabled: true}
	if mutate != nil {
		mutate(k)
	}
	if err := hs.db.CreateKey(context.Background(), k); err != nil {
		hs.t.Fatal(err)
	}
	if err := hs.pool.Reload(context.Background()); err != nil {
		hs.t.Fatal(err)
	}
	return k
}

// addToken creates a client token and returns its secret.
func (hs *harness) addToken(name string, mutate func(*store.Token)) (string, *store.Token) {
	hs.t.Helper()
	raw := secret.NewToken("sk-test-")
	tok := &store.Token{Name: name, Hash: secret.Hash(raw), Prefix: raw[:10], Enabled: true}
	if mutate != nil {
		mutate(tok)
	}
	if err := hs.db.CreateToken(context.Background(), tok); err != nil {
		hs.t.Fatal(err)
	}
	return raw, tok
}

// do sends a request to the gateway. hdr is "Name: value" pairs.
func (hs *harness) do(method, path, body string, hdr ...string) (*http.Response, string) {
	hs.t.Helper()
	return hs.doAs(hs.token, method, path, body, hdr...)
}

func (hs *harness) doAs(token, method, path, body string, hdr ...string) (*http.Response, string) {
	hs.t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, hs.srv.URL+path, rdr)
	if err != nil {
		hs.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, h := range hdr {
		k, v, _ := strings.Cut(h, ":")
		req.Header.Set(strings.TrimSpace(k), strings.TrimSpace(v))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		hs.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// waitLogs flushes the log writer until at least n logs are stored and
// returns them oldest first, with bodies and attempts.
func (hs *harness) waitLogs(n int) []*store.Log {
	hs.t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := hs.h.Logs.Flush(ctx); err != nil {
			hs.t.Fatal(err)
		}
		list, total, err := hs.db.ListLogs(ctx, store.LogFilter{Limit: 1000})
		if err != nil {
			hs.t.Fatal(err)
		}
		if total >= n {
			out := make([]*store.Log, 0, len(list))
			for i := len(list) - 1; i >= 0; i-- {
				l, err := hs.db.GetLog(ctx, list[i].ID)
				if err != nil {
					hs.t.Fatal(err)
				}
				out = append(out, l)
			}
			return out
		}
		if time.Now().After(deadline) {
			hs.t.Fatalf("timed out waiting for %d logs (have %d)", n, total)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lastLog waits for n logs and returns the newest.
func (hs *harness) lastLog(n int) *store.Log {
	hs.t.Helper()
	l := hs.waitLogs(n)
	return l[len(l)-1]
}

func jsonReply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func mustContain(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Fatalf("%s: missing %q in %q", what, sub, s)
		}
	}
}
