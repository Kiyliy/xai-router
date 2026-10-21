// Package test contains end-to-end tests that run the whole gateway against a
// mock xAI upstream.
package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/app"
	"github.com/Kiyliy/xai-router/web"
)

// ---- mock xAI ----

type mockXAI struct {
	mu        sync.Mutex
	seq       int
	owner     map[string]string // resource id -> key that created it
	polls     map[string]int
	lastBody  map[string][]byte
	lastHdr   map[string]http.Header
	hits      map[string]int // key -> requests
	streamGap atomic.Int64   // time.Duration between stream chunks
	wsSeen    []wsSeen
}

func newMock() *mockXAI {
	m := &mockXAI{owner: map[string]string{}, polls: map[string]int{}, lastBody: map[string][]byte{},
		lastHdr: map[string]http.Header{}, hits: map[string]int{}}
	m.streamGap.Store(int64(40 * time.Millisecond))
	return m
}

func (m *mockXAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.hits[key]++
	m.lastBody[r.URL.Path] = body
	m.lastHdr[r.URL.Path] = r.Header.Clone()
	m.mu.Unlock()

	switch {
	case strings.HasSuffix(key, "-429"):
		w.Header().Set("Retry-After", "30")
		jsonResp(w, 429, `{"error":"Too many requests"}`)
		return
	case strings.HasSuffix(key, "-401"):
		jsonResp(w, 400, `{"code":"Client specified an invalid argument","error":"Incorrect API key provided: xa***01. You can obtain an API key from https://console.x.ai."}`)
		return
	case strings.HasSuffix(key, "-500"):
		jsonResp(w, 500, `{"error":"internal"}`)
		return
	case strings.HasSuffix(key, "-500cost"):
		// A failure the upstream nevertheless reports a cost for.
		jsonResp(w, 500, `{"error":"internal after the prompt was read","usage":{"cost_in_usd_ticks":1230000}}`)
		return
	case strings.HasSuffix(key, "-partialerr"):
		// Content, then an in-stream error: too late to fail over.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `data: {"id":"chat-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello there, partial"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, `data: {"code":"resource-exhausted","error":"The model is currently at capacity due to high demand."}`+"\n\ndata: [DONE]\n\n")
		return
	case strings.HasSuffix(key, "-streamerr"):
		// Overloaded: 200, then an in-stream error instead of content.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `data: {"code":"resource-exhausted","error":"The model is currently at capacity due to high demand."}`+"\n\ndata: [DONE]\n\n")
		return
	}
	if r.Header.Get("X-Grok-Mock-Echo") != "" {
		// Echo mode: report what arrived, with a large padding so truncation
		// would be visible.
		status, _ := strconv.Atoi(r.Header.Get("X-Grok-Mock-Status"))
		if status == 0 {
			status = 200
		}
		out, _ := json.Marshal(map[string]any{"method": r.Method, "path": r.URL.Path, "query": r.URL.RawQuery,
			"body_len": len(body), "auth": r.Header.Get("Authorization"), "padding": strings.Repeat("p", 100_000)})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Custom", "kept")
		w.WriteHeader(status)
		if status != http.StatusNoContent {
			_, _ = w.Write(out)
		}
		return
	}
	if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		m.serveMockWS(w, r, key)
		return
	}
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	withCost := !strings.HasSuffix(key, "-nocost")

	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/chat/completions":
		if c, _ := req["model"].(string); c == "echo-403" {
			// A 403 that repeats the user's message text back.
			msgs, _ := json.Marshal(req["messages"])
			jsonResp(w, 403, `{"error":"content not permitted: `+strings.ReplaceAll(string(msgs), `"`, `'`)+`"}`)
			return
		}
		if m, _ := req["model"].(string); strings.HasPrefix(m, "echo ") {
			// Like xAI, unknown models are echoed back in the error.
			jsonResp(w, 404, `{"code":"not-found","error":"The model `+m+` does not exist or your team does not have access to it."}`)
			return
		}
		if req["model"] == "bad-model" {
			jsonResp(w, 400, `{"error":"The model bad-model does not exist. `+strings.Repeat("x", 1500)+`"}`)
			return
		}
		if d := r.Header.Get("X-Grok-Mock-Delay-Ms"); d != "" {
			ms, _ := strconv.Atoi(d)
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		usage := `"usage":{"prompt_tokens":188,"completion_tokens":9,"total_tokens":370,"prompt_tokens_details":{"text_tokens":188,"cached_tokens":128},"completion_tokens_details":{"reasoning_tokens":173}`
		if c := r.Header.Get("X-Grok-Mock-Cost"); c != "" {
			usage += `,"cost_in_usd_ticks":` + c
		} else if withCost {
			usage += `,"cost_in_usd_ticks":4496000`
		}
		usage += "}"
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			for _, piece := range []string{"Hel", "lo", " wor", "ld"} {
				fmt.Fprintf(w, "data: {\"id\":\"chat-1\",\"object\":\"chat.completion.chunk\",\"model\":\"grok-build-0.1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%q}}]}\n\n", piece)
				fl.Flush()
				if r.Header.Get("X-Grok-Mock-Break") != "" {
					// Upstream dies mid-stream: drop the connection.
					conn, _, _ := w.(http.Hijacker).Hijack()
					conn.Close()
					return
				}
				time.Sleep(time.Duration(m.streamGap.Load()))
			}
			opts, _ := req["stream_options"].(map[string]any)
			if opts["include_usage"] == true {
				fmt.Fprintf(w, "data: {\"id\":\"chat-1\",\"object\":\"chat.completion.chunk\",\"choices\":[],%s}\n\n", usage)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		jsonResp(w, 200, `{"id":"chat-1","object":"chat.completion","model":"grok-build-0.1","choices":[{"index":0,"message":{"role":"assistant","content":"Hello world"}}],`+usage+`}`)
	case r.Method == "POST" && r.URL.Path == "/v1/responses":
		id := m.newID("resp", key)
		jsonResp(w, 200, fmt.Sprintf(`{"id":%q,"object":"response","model":"grok-4.3","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1000,"output_tokens":100,"total_tokens":1100}}`, id))
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/responses/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
		if !m.owns(id, key) {
			jsonResp(w, 404, `{"error":"response not found"}`)
			return
		}
		// Like xAI, a retrieval echoes the original usage including its cost.
		jsonResp(w, 200, fmt.Sprintf(`{"id":%q,"object":"response","status":"completed","usage":{"input_tokens":1000,"output_tokens":100,"cost_in_usd_ticks":15000000}}`, id))
	case r.Method == "POST" && r.URL.Path == "/v1/videos/generations":
		jsonResp(w, 200, fmt.Sprintf(`{"request_id":%q}`, m.newID("vid", key)))
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/videos/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/videos/")
		if !m.owns(id, key) {
			jsonResp(w, 404, `{"error":"No deferred video could be found"}`)
			return
		}
		m.mu.Lock()
		m.polls[id]++
		n := m.polls[id]
		m.mu.Unlock()
		if n == 1 {
			jsonResp(w, 202, `{"status":"pending","progress":40}`)
			return
		}
		jsonResp(w, 200, `{"status":"done","video":{"url":"https://vidgen.x.ai/v.mp4","duration":6},"model":"grok-imagine-video","usage":{"cost_in_usd_ticks":3000000000}}`)
	case r.Method == "POST" && r.URL.Path == "/v1/images/generations":
		jsonResp(w, 200, `{"data":[{"url":"https://img/1.png"},{"url":"https://img/2.png"}]}`)
	case r.URL.Path == "/v1/api-key":
		jsonResp(w, 200, `{"name":"k","team_id":"t","acls":["api-key:model:*"],"api_key_blocked":false,"api_key_disabled":false,"team_blocked":false}`)
	case r.Method == "POST" && r.URL.Path == "/v1/files":
		jsonResp(w, 200, fmt.Sprintf(`{"id":%q,"object":"file","filename":"a.txt"}`, m.newID("file", key)))
	case r.Method == "POST" && r.URL.Path == "/v1/batches":
		jsonResp(w, 200, fmt.Sprintf(`{"batch_id":%q,"name":"b"}`, m.newID("batch", key)))
	case r.Method == "GET" && (r.URL.Path == "/v1/files" || r.URL.Path == "/v1/batches"):
		// The team sees everything, including resources created elsewhere.
		m.mu.Lock()
		var items []string
		for id := range m.owner {
			if strings.HasPrefix(id, "file_") && r.URL.Path == "/v1/files" {
				items = append(items, fmt.Sprintf(`{"id":%q}`, id))
			}
			if strings.HasPrefix(id, "batch_") && r.URL.Path == "/v1/batches" {
				items = append(items, fmt.Sprintf(`{"batch_id":%q}`, id))
			}
		}
		m.mu.Unlock()
		items = append(items, `{"id":"file_console","batch_id":"batch_console"}`)
		field := "data"
		if r.URL.Path == "/v1/batches" && r.URL.Query().Get("shape") != "data" {
			field = "batches"
		}
		jsonResp(w, 200, fmt.Sprintf(`{%q:[%s]}`, field, strings.Join(items, ",")))
	case r.Method == "POST" && r.URL.Path == "/v1/tts":
		jsonResp(w, 200, `{"audio":"AAAA","content_type":"audio/mpeg","duration":1.2}`)
	case r.Method == "POST" && r.URL.Path == "/v1/images/edits":
		jsonResp(w, 200, `{"data":[{"url":"https://img/e.png"}]}`)
	case r.URL.Path == "/v1/language-models":
		jsonResp(w, 200, `{"models":[{"id":"grok-4.3","aliases":["grok-4.3-latest"]},{"id":"grok-build-0.1","aliases":["grok-code-fast-1"]}]}`)
	case r.URL.Path == "/v1/models":
		jsonResp(w, 200, `{"object":"list","data":[{"id":"grok-4.3","object":"model"},{"id":"grok-build-0.1","object":"model"}]}`)
	default:
		jsonResp(w, 404, `{"error":"unknown path `+r.URL.Path+`"}`)
	}
}

func (m *mockXAI) newID(prefix, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("%s_%d", prefix, m.seq)
	m.owner[id] = key
	return id
}

func (m *mockXAI) owns(id, key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.owner[id] == key
}

func jsonResp(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// ---- harness ----

type env struct {
	t      *testing.T
	app    *app.App
	srv    *httptest.Server
	mock   *mockXAI
	up     *httptest.Server
	admin  string // session token
	client string // client token
}

const adminPW = "integration-test-pw"

func newEnv(t *testing.T, keys ...string) *env {
	t.Helper()
	return newEnvDB(t, ":memory:", keys...)
}

// newEnvDB is newEnv with a database path (a file enables WAL concurrency).
func newEnvDB(t *testing.T, dbPath string, keys ...string) *env {
	t.Helper()
	e := &env{t: t, mock: newMock()}
	e.up = httptest.NewServer(e.mock)
	t.Cleanup(e.up.Close)
	a, err := app.New(context.Background(), app.Options{DBPath: dbPath, AdminPassword: adminPW,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), StaticFS: web.FS()})
	if err != nil {
		t.Fatal(err)
	}
	e.app = a
	a.Start()
	e.srv = httptest.NewServer(a.Handler())
	t.Cleanup(func() { e.srv.Close(); a.Close() })

	var login struct{ Token string }
	e.do("POST", "/api/v1/auth/login", "", map[string]any{"password": adminPW}, 200, &login)
	e.admin = login.Token
	// The mock upstream is plain HTTP on localhost: relax the SSRF policy.
	e.api("PUT", "/api/v1/settings", map[string]any{"allow_private_upstreams": true, "allow_insecure_upstreams": true,
		"upstream_allowed_hosts": []string{}}, 200, nil)
	for i, k := range keys {
		e.api("POST", "/api/v1/keys", map[string]any{"name": fmt.Sprintf("k%d", i+1), "key": k, "base_url": e.up.URL,
			"priority": i}, 201, nil)
	}
	var tok struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "client"}, 201, &tok)
	e.client = tok.Token
	return e
}

func (e *env) do(method, path, bearer string, body any, want int, out any) []byte {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rdr)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if want != 0 && resp.StatusCode != want {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, b)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			e.t.Fatalf("decode %s: %v: %s", path, err, b)
		}
	}
	return b
}

func (e *env) api(method, path string, body any, want int, out any) []byte {
	e.t.Helper()
	return e.do(method, path, e.admin, body, want, out)
}

func (e *env) proxy(method, path string, body any, want int, out any) []byte {
	e.t.Helper()
	return e.do(method, path, e.client, body, want, out)
}

type logRow struct {
	ID               int64   `json:"id"`
	Path             string  `json:"path"`
	Model            string  `json:"model"`
	Status           int     `json:"status"`
	Stream           bool    `json:"stream"`
	Attempts         int     `json:"attempts"`
	AttemptIndex     int     `json:"attempt_index"`
	AttemptTotal     int     `json:"attempt_total"`
	AttemptState     string  `json:"attempt_state"`
	RequestID        string  `json:"request_id"`
	Level            string  `json:"level"`
	KeyName          string  `json:"key_name"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CostSource       string  `json:"cost_source"`
	Error            string  `json:"error"`
	VideoSeconds     float64 `json:"video_seconds"`
	Images           int64   `json:"images"`
	TTFBMs           int64   `json:"ttfb_ms"`
	LatencyMs        int64   `json:"latency_ms"`
}

// waitLogs polls until at least n client requests are logged and returns
// their final rows (attempts=final), newest first.
func (e *env) waitLogs(n int) []logRow {
	e.t.Helper()
	return e.waitRows(n, "attempts=final")
}

// waitRows polls until at least n log rows match the filter and returns
// them newest first.
func (e *env) waitRows(n int, filter string) []logRow {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var page struct {
			Data  []logRow
			Total int
		}
		e.api("GET", "/api/v1/logs?page_size=100&"+filter, nil, 200, &page)
		if page.Total >= n {
			return page.Data
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %d logs (have %d)", n, page.Total)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---- tests ----

func TestFailoverAcrossKeys(t *testing.T) {
	e := newEnv(t, "xai-a-429", "xai-b-401", "xai-c-500", "xai-d-ok")
	var resp map[string]any
	e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "grok-build-0.1",
		"messages": []map[string]string{{"role": "user", "content": "hi"}}}, 200, &resp)

	// v0.6.0: one log row per upstream attempt, sharing the request id.
	rows := e.waitRows(4, "")
	l := rows[0]
	if l.Status != 200 || l.Attempts != 1 || l.KeyName != "k4" || l.AttemptIndex != 4 || l.AttemptTotal != 4 ||
		l.AttemptState != "final" || l.Level != "info" {
		t.Fatalf("log: %+v", l)
	}
	if l.CostUSD != 0.0004496 || l.CostSource != "upstream" {
		t.Fatalf("cost: %+v", l)
	}
	for i, r := range rows[1:] {
		if r.RequestID != l.RequestID || r.AttemptState != "retried" || r.AttemptIndex != 3-i || r.Level != "warn" ||
			r.CostUSD != 0 || r.CostSource != "none" {
			t.Fatalf("retried row %d: %+v", i, r)
		}
	}
	var detail struct {
		UpstreamAttempts []any `json:"upstream_attempts"`
		Chain            []struct {
			KeyName      string `json:"key_name"`
			Action       string
			Status       int
			AttemptIndex int `json:"attempt_index"`
		}
	}
	e.api("GET", fmt.Sprintf("/api/v1/logs/%d", l.ID), nil, 200, &detail)
	got := []string{}
	for _, a := range detail.Chain {
		got = append(got, fmt.Sprintf("%d:%s:%d:%s", a.AttemptIndex, a.KeyName, a.Status, a.Action))
	}
	// k2 is checked in the background: "disable" when the check had
	// already disabled it by the time the request finished, else "verify".
	want := "1:k1:429:cooldown 2:k2:400:verify 3:k3:500:retry 4:k4:200:success"
	if len(got) > 1 && got[1] == "2:k2:400:disable" {
		got[1] = "2:k2:400:verify"
	}
	if strings.Join(got, " ") != want || len(detail.UpstreamAttempts) != 0 {
		t.Fatalf("chain = %s\nwant    %s", strings.Join(got, " "), want)
	}
	// Key states are visible in the admin API.
	var keys struct {
		Data []struct {
			Name         string
			Status       string
			StatusReason string `json:"status_reason"`
			AutoDisabled bool   `json:"auto_disabled"`
		}
	}
	// k2 looked dead (400 "Incorrect API key"); it is disabled only after
	// GET /v1/api-key confirms it, which happens asynchronously.
	states := map[string]string{}
	for i := 0; i < 50; i++ {
		e.api("GET", "/api/v1/keys", nil, 200, &keys)
		for _, k := range keys.Data {
			states[k.Name] = k.Status
		}
		if states["k2"] == "disabled" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if states["k1"] != "cooldown" || states["k2"] != "disabled" || states["k4"] != "active" {
		t.Fatalf("key states: %v", states)
	}
	// Acceptance F5: stats count log rows. Three retries then a success are
	// four requests, one success and three failures.
	var ov struct {
		Requests, Success, Errors int
		SuccessRate               float64 `json:"success_rate"`
	}
	e.api("GET", "/api/v1/stats/overview", nil, 200, &ov)
	if ov.Requests != 4 || ov.Success != 1 || ov.Errors != 3 || ov.SuccessRate != 0.25 {
		t.Fatalf("overview: %+v", ov)
	}
	// Second request skips the cooling and disabled keys entirely.
	e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "grok-build-0.1", "messages": []any{}}, 200, nil)
	if l := e.waitLogs(2)[0]; l.AttemptTotal > 2 {
		t.Fatalf("second request made %d attempts", l.AttemptTotal)
	}
}

func TestStreamingIsIncrementalAndBilled(t *testing.T) {
	e := newEnv(t, "xai-ok")
	e.mock.streamGap.Store(int64(150 * time.Millisecond))
	b, _ := json.Marshal(map[string]any{"model": "grok-build-0.1", "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}}})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+e.client)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", "session=secret")
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var firstChunk time.Duration
	var text strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		if firstChunk == 0 {
			firstChunk = time.Since(start)
		}
		var ch struct {
			Choices []struct{ Delta struct{ Content string } }
		}
		_ = json.Unmarshal([]byte(line[6:]), &ch)
		if len(ch.Choices) > 0 {
			text.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	total := time.Since(start)
	if text.String() != "Hello world" {
		t.Fatalf("text %q", text.String())
	}
	// 4 chunks 150ms apart: the first must arrive long before the stream ends.
	if firstChunk > 300*time.Millisecond || total < 500*time.Millisecond {
		t.Fatalf("not incremental: first chunk %v, total %v", firstChunk, total)
	}
	// The gateway asked for usage and did not forward the cookie.
	var sent map[string]any
	_ = json.Unmarshal(e.mock.lastBody["/v1/chat/completions"], &sent)
	if opts, _ := sent["stream_options"].(map[string]any); opts["include_usage"] != true {
		t.Fatalf("include_usage not injected: %v", sent)
	}
	h := e.mock.lastHdr["/v1/chat/completions"]
	if h.Get("Cookie") != "" || h.Get("Authorization") != "Bearer xai-ok" {
		t.Fatalf("headers leaked or auth wrong: %v", h)
	}
	l := e.waitLogs(1)[0]
	if !l.Stream || l.CostUSD != 0.0004496 || l.ReasoningTokens != 173 || l.TTFBMs > 300 || l.LatencyMs < 500 {
		t.Fatalf("stream log: %+v", l)
	}
	var detail struct {
		ResponseText string            `json:"response_text"`
		ResponseBody string            `json:"response_body"`
		RequestHdrs  map[string]string `json:"request_headers"`
	}
	e.api("GET", fmt.Sprintf("/api/v1/logs/%d", l.ID), nil, 200, &detail)
	if detail.ResponseText != "Hello world" || !strings.Contains(detail.ResponseBody, "data: [DONE]") {
		t.Fatalf("detail: %+v", detail)
	}
	if strings.Contains(detail.RequestHdrs["Authorization"], e.client[10:30]) || strings.Contains(detail.RequestHdrs["Cookie"], "secret") {
		t.Fatalf("secrets stored in log: %v", detail.RequestHdrs)
	}
}

func TestComputedCostWhenUpstreamOmitsIt(t *testing.T) {
	e := newEnv(t, "xai-x-nocost")
	e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "grok-build-0.1", "messages": []any{}}, 200, nil)
	e.proxy("POST", "/v1/images/generations", map[string]any{"model": "grok-imagine-image", "prompt": "cat", "n": 2}, 200, nil)
	logs := e.waitLogs(2)
	img, chat := logs[0], logs[1]
	if chat.CostSource != "computed" || chat.CostUSD != 0.0004496 {
		t.Fatalf("chat cost: %+v", chat)
	}
	if img.CostSource != "computed" || img.Images != 2 || img.CostUSD != 0.04 {
		t.Fatalf("image cost: %+v", img)
	}
}

func TestClientErrorReturnedVerbatim(t *testing.T) {
	e := newEnv(t, "xai-a-ok", "xai-b-ok")
	body := e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "bad-model"}, 400, nil)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil || len(body) < 1500 {
		t.Fatalf("error body truncated or invalid (%d bytes): %v", len(body), err)
	}
	if l := e.waitLogs(1)[0]; l.Attempts != 1 || l.Status != 400 || l.CostSource != "none" {
		t.Fatalf("client error must not be retried: %+v", l)
	}
}

func TestAsyncVideoStaysOnKeyAndBillsOnce(t *testing.T) {
	e := newEnv(t, "xai-a-ok", "xai-b-ok", "xai-c-ok")
	var sub struct {
		RequestID string `json:"request_id"`
	}
	e.proxy("POST", "/v1/videos/generations", map[string]any{"model": "grok-imagine-video", "prompt": "cat", "duration": 6}, 200, &sub)
	if sub.RequestID == "" {
		t.Fatal("no request id")
	}
	path := "/v1/videos/" + sub.RequestID
	e.proxy("GET", path, nil, 202, nil)
	for i := 0; i < 3; i++ {
		e.proxy("GET", path, nil, 200, nil)
	}
	logs := e.waitLogs(5)
	var billed, costed int
	for _, l := range logs {
		if l.Path == path && l.Status == 200 {
			if l.CostUSD > 0 {
				costed++
				if l.CostUSD != 0.3 || l.CostSource != "upstream" || l.Model != "grok-imagine-video" {
					t.Fatalf("video cost log: %+v", l)
				}
			}
			billed++
		}
	}
	if billed != 3 || costed != 1 {
		t.Fatalf("video billed %d times out of %d done polls", costed, billed)
	}
	var tok struct {
		Data []struct {
			UsedUSD float64 `json:"used_usd"`
		}
	}
	e.api("GET", "/api/v1/tokens", nil, 200, &tok)
	if tok.Data[0].UsedUSD != 0.3 {
		t.Fatalf("token used %v", tok.Data[0].UsedUSD)
	}
}

func TestResponsesBinding(t *testing.T) {
	e := newEnv(t, "xai-a-ok", "xai-b-ok", "xai-c-ok", "xai-d-ok")
	for i := 0; i < 6; i++ {
		var r struct{ ID string }
		e.proxy("POST", "/v1/responses", map[string]any{"model": "grok-4.3", "input": "hi"}, 200, &r)
		e.proxy("GET", "/v1/responses/"+r.ID, nil, 200, nil)
		e.proxy("POST", "/v1/responses", map[string]any{"model": "grok-4.3", "input": "more", "previous_response_id": r.ID}, 200, nil)
	}
	logs := e.waitLogs(18)
	for _, l := range logs {
		if strings.HasPrefix(l.Path, "/v1/responses/") {
			if l.CostUSD != 0 || l.CostSource != "none" || l.PromptTokens != 0 {
				t.Fatalf("retrieving a stored response must be free: %+v", l)
			}
			continue
		}
		if l.Path == "/v1/responses" && l.Model == "grok-4.3" && l.CostSource != "computed" {
			t.Fatalf("responses cost: %+v", l)
		}
		if l.Path == "/v1/responses" && l.CostUSD != 0.0015 { // 1000*1.25/1M + 100*2.5/1M
			t.Fatalf("responses cost value: %+v", l)
		}
	}
}

func TestTokenPolicies(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var lim struct {
		ID    int64
		Token string
	}
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "limited", "allowed_models": []string{"grok-4*"},
		"quota_usd": 0.0001}, 201, &lim)
	// Strict quota: the request's worst-case cost is reserved up front and a
	// $0.0001 quota cannot cover it.
	e.do("POST", "/v1/chat/completions", lim.Token, map[string]any{"model": "grok-4.3", "messages": []any{}}, 429, nil)
	e.api("POST", fmt.Sprintf("/api/v1/tokens/%d/quota", lim.ID), map[string]any{"action": "increase", "amount_usd": 1, "reason": "test"}, 200, nil)
	req := map[string]any{"model": "grok-build-0.1", "messages": []any{}}
	e.do("POST", "/v1/chat/completions", lim.Token, req, 403, nil)
	// Aliases resolve to the canonical id; wildcard allows grok-4.3.
	e.do("POST", "/v1/chat/completions", lim.Token, map[string]any{"model": "grok-4.3-latest", "messages": []any{}}, 200, nil)
	e.waitLogs(3)
	// Disable and expire.
	e.api("PATCH", fmt.Sprintf("/api/v1/tokens/%d", lim.ID), map[string]any{"enabled": false}, 200, nil)
	e.do("POST", "/v1/chat/completions", lim.Token, map[string]any{"model": "grok-4.3"}, 403, nil)
	e.api("PATCH", fmt.Sprintf("/api/v1/tokens/%d", lim.ID), map[string]any{"enabled": true,
		"expires_at": time.Now().Add(-time.Minute).Format(time.RFC3339)}, 200, nil)
	e.do("POST", "/v1/chat/completions", lim.Token, map[string]any{"model": "grok-4.3"}, 403, nil)
	e.do("POST", "/v1/chat/completions", "sk-xr-wrong", req, 401, nil)
	e.do("POST", "/v1/chat/completions", "", req, 401, nil)
	// Admin tokens cannot call the proxy and client tokens cannot call the admin API.
	e.do("GET", "/api/v1/keys", e.client, nil, 401, nil)
}

func TestBodyLimitAppliesToChunkedRequests(t *testing.T) {
	e := newEnv(t, "xai-ok")
	e.api("PUT", "/api/v1/settings", map[string]any{"max_body_mb": 1}, 200, nil)
	pr, pw := io.Pipe()
	go func() {
		chunk := bytes.Repeat([]byte("a"), 64<<10)
		for i := 0; i < 40; i++ { // 2.5 MB, no Content-Length
			if _, err := pw.Write(chunk); err != nil {
				break
			}
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", pr)
	req.Header.Set("Authorization", "Bearer "+e.client)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestNoKeysAndUnknownPath(t *testing.T) {
	e := newEnv(t)
	body := e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "grok-4.3"}, 503, nil)
	if !strings.Contains(string(body), "no_upstream_keys") {
		t.Fatalf("body %s", body)
	}
	e.api("POST", "/api/v1/keys", map[string]any{"key": "xai-ok", "base_url": e.up.URL}, 201, nil)
	// Unknown paths are passed through untouched.
	body = e.proxy("GET", "/v1/some/future/endpoint", nil, 404, nil)
	if !strings.Contains(string(body), "unknown path /v1/some/future/endpoint") {
		t.Fatalf("not passed through: %s", body)
	}
}

func TestAdminAPI(t *testing.T) {
	e := newEnv(t, "xai-ok")
	// Unified error format.
	var errResp struct {
		Error struct{ Code, Message string }
	}
	e.do("GET", "/api/v1/keys", "", nil, 401, &errResp)
	if errResp.Error.Code != "unauthorized" {
		t.Fatalf("error format: %+v", errResp)
	}
	e.api("GET", "/api/v1/keys/999", nil, 404, &errResp)
	e.api("PUT", "/api/v1/settings", map[string]any{"max_attempts": 0}, 400, &errResp)
	if errResp.Error.Code != "invalid_settings" {
		t.Fatalf("settings error: %+v", errResp)
	}
	e.do("POST", "/api/v1/auth/login", "", map[string]any{"password": "nope"}, 401, nil)

	// Bulk import with test.
	var imp struct {
		Created []struct{ ID int64 }
		Skipped []struct{ Reason string }
		Tests   []struct {
			OK     bool
			Status string
			Models []string
		}
	}
	e.api("POST", "/api/v1/keys/import", map[string]any{"keys": "xai-new-1\nnamed,xai-new-2\nxai-new-1\nxai-ok\n",
		"base_url": e.up.URL, "test": true, "probe": true}, 200, &imp)
	if len(imp.Created) != 2 || len(imp.Skipped) != 2 || len(imp.Tests) != 2 || !imp.Tests[0].OK || len(imp.Tests[0].Models) != 2 {
		t.Fatalf("import: %+v", imp)
	}
	// Key test endpoint.
	var tr struct {
		OK     bool
		Status string
		Probe  struct {
			Model   string
			Status  int
			CostUSD float64 `json:"cost_usd"`
		}
	}
	e.api("POST", fmt.Sprintf("/api/v1/keys/%d/test", imp.Created[0].ID), map[string]any{"probe": true}, 200, &tr)
	if !tr.OK || tr.Probe.Model != "grok-build-0.1" || tr.Probe.Status != 200 || tr.Probe.CostUSD <= 0 {
		t.Fatalf("key test: %+v", tr)
	}
	// Pagination totals.
	var page struct {
		Data     []any
		Total    int
		PageSize int `json:"page_size"`
	}
	e.api("GET", "/api/v1/keys?page_size=2", nil, 200, &page)
	if page.Total != 3 || len(page.Data) != 2 || page.PageSize != 2 {
		t.Fatalf("page: %+v", page)
	}
	// Admin API token works as bearer.
	var at struct{ Token string }
	e.api("POST", "/api/v1/admin-tokens", map[string]any{"name": "ci"}, 201, &at)
	e.do("GET", "/api/v1/system/info", at.Token, nil, 200, nil)
	// Models are seeded with official prices.
	var models struct {
		Data []struct {
			ID          string
			InputPrice  float64 `json:"input_price"`
			OutputPrice float64 `json:"output_price"`
		}
	}
	e.api("GET", "/api/v1/models?kind=language", nil, 200, &models)
	found := false
	for _, m := range models.Data {
		if m.ID == "grok-4.3" && m.InputPrice == 1.25 && m.OutputPrice == 2.5 {
			found = true
		}
	}
	if !found {
		t.Fatalf("grok-4.3 price missing: %+v", models.Data)
	}
	// Copy as curl.
	e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "grok-4.3", "messages": []any{}}, 200, nil)
	l := e.waitLogs(1)[0]
	var c struct{ Curl string }
	e.api("GET", fmt.Sprintf("/api/v1/logs/%d/curl?base_url=https://gw.example.com", l.ID), nil, 200, &c)
	if !strings.HasPrefix(c.Curl, "curl -X POST 'https://gw.example.com/v1/chat/completions'") ||
		!strings.Contains(c.Curl, `--data-raw '{"messages":[],"model":"grok-4.3"}'`) || strings.Contains(c.Curl, e.client) {
		t.Fatalf("curl: %s", c.Curl)
	}
	// Timeseries and breakdown respond.
	var ts struct {
		Series []struct {
			Name   string
			Points []struct{ Requests int }
		}
	}
	e.api("GET", "/api/v1/stats/timeseries?range=24h&group=model", nil, 200, &ts)
	if len(ts.Series) == 0 || len(ts.Series[0].Points) < 24 {
		t.Fatalf("timeseries: %+v", ts)
	}
	e.api("GET", "/api/v1/stats/breakdown?by=key", nil, 200, nil)
	// Password change revokes other sessions.
	var other struct{ Token string }
	e.do("POST", "/api/v1/auth/login", "", map[string]any{"password": adminPW}, 200, &other)
	e.api("PUT", "/api/v1/auth/password", map[string]any{"old_password": adminPW, "new_password": "brand-new-pass"}, 204, nil)
	e.do("GET", "/api/v1/auth/me", other.Token, nil, 401, nil)
	e.api("GET", "/api/v1/auth/me", nil, 200, nil)
}

func TestDebugEndpointHiddenByDefault(t *testing.T) {
	e := newEnv(t)
	e.api("GET", "/debug/pprof/", nil, 404, nil)
	e.api("PUT", "/api/v1/settings", map[string]any{"expose_debug": true}, 200, nil)
	e.api("GET", "/debug/pprof/", nil, 200, nil)
	e.do("GET", "/debug/pprof/", "", nil, 401, nil)
}

func TestUsageAccountedWithLoggingDisabled(t *testing.T) {
	e := newEnv(t, "xai-ok")
	e.api("PUT", "/api/v1/settings", map[string]any{"log_enabled": false}, 200, nil)
	e.proxy("POST", "/v1/chat/completions", map[string]any{"model": "grok-build-0.1", "messages": []any{}}, 200, nil)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var tok struct {
			Data []struct {
				UsedUSD float64 `json:"used_usd"`
			}
		}
		e.api("GET", "/api/v1/tokens", nil, 200, &tok)
		if tok.Data[0].UsedUSD == 0.0004496 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("token usage not accounted: %v", tok.Data[0].UsedUSD)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var page struct{ Total int }
	e.api("GET", "/api/v1/logs", nil, 200, &page)
	var ov struct{ Requests int }
	e.api("GET", "/api/v1/stats/overview", nil, 200, &ov)
	if page.Total != 0 || ov.Requests != 1 {
		t.Fatalf("logs=%d (want 0) stats requests=%d (want 1)", page.Total, ov.Requests)
	}
}

// A client must not be able to disable keys or open breakers by putting
// "dead key" words into its request (the upstream echoes the model name).
func TestHostileModelNameCannotHurtPool(t *testing.T) {
	e := newEnv(t, "xai-a-ok", "xai-b-ok", "xai-c-ok")
	for i := 0; i < 5; i++ {
		e.proxy("POST", "/v1/chat/completions", map[string]any{
			"model": "echo Incorrect API key provided, team is blocked, no credits, quota exceeded, rate limit"}, 404, nil)
	}
	time.Sleep(200 * time.Millisecond)
	var keys struct {
		Data []struct {
			Name   string
			Status string
		}
	}
	e.api("GET", "/api/v1/keys", nil, 200, &keys)
	for _, k := range keys.Data {
		if k.Status != "active" {
			t.Fatalf("key %s is %s after hostile requests", k.Name, k.Status)
		}
	}
	for _, l := range e.waitLogs(5) {
		if l.AttemptTotal > 2 {
			t.Fatalf("hostile request tried %d keys", l.AttemptTotal)
		}
	}
}

func TestUpstreamPolicyBlocksSSRF(t *testing.T) {
	a, err := app.New(context.Background(), app.Options{DBPath: ":memory:", AdminPassword: adminPW,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	e := &env{t: t, srv: srv, app: a}
	var login struct{ Token string }
	e.do("POST", "/api/v1/auth/login", "", map[string]any{"password": adminPW}, 200, &login)
	e.admin = login.Token
	// Defaults: https only, api.x.ai hosts only, no private addresses.
	for _, u := range []string{"http://api.x.ai", "https://evil.example.com", "https://169.254.169.254",
		"https://127.0.0.1:8080", "https://user:pw@api.x.ai", "ftp://api.x.ai"} {
		var er struct {
			Error struct{ Code, Message string }
		}
		e.api("POST", "/api/v1/keys", map[string]any{"key": "xai-test", "base_url": u}, 400, &er)
	}
	e.api("POST", "/api/v1/keys", map[string]any{"key": "xai-test", "base_url": "https://us.api.x.ai"}, 201, nil)
	// Allowing every host still refuses private and metadata addresses...
	e.api("PUT", "/api/v1/settings", map[string]any{"upstream_allowed_hosts": []string{}}, 200, nil)
	e.api("POST", "/api/v1/keys", map[string]any{"key": "xai-t2", "base_url": "https://10.0.0.5"}, 400, nil)
	e.api("PUT", "/api/v1/settings", map[string]any{"upstream_base_url": "https://192.168.1.1"}, 400, nil)
	e.api("PUT", "/api/v1/settings", map[string]any{"allow_private_upstreams": true}, 200, nil)
	e.api("POST", "/api/v1/keys", map[string]any{"key": "xai-t2", "base_url": "https://10.0.0.5"}, 201, nil)
	// Name resolution to private addresses is blocked at dial time; see
	// netutil.TestGuardedDialer.
}
