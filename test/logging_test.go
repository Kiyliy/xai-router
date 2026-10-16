package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fullLog is a log row with the fields F7 checks.
type fullLog struct {
	ID               int64
	Path             string
	Status           int
	Error            string
	Stream           bool
	Attempts         int
	TokenName        string  `json:"token_name"`
	KeyName          string  `json:"key_name"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CostSource       string  `json:"cost_source"`
	RequestBody      string  `json:"request_body"`
	ResponseBody     string  `json:"response_body"`
	ResponseText     string  `json:"response_text"`
	ClientIP         string  `json:"client_ip"`
}

// logFor returns the detail of the newest log whose request id matches the
// X-Request-Id the gateway returned.
func (e *env) logFor(reqID string) fullLog {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var page struct{ Data []fullLog }
		e.api("GET", "/api/v1/logs?q="+reqID, nil, 200, &page)
		if len(page.Data) == 1 {
			var d fullLog
			e.api("GET", fmt.Sprintf("/api/v1/logs/%d", page.Data[0].ID), nil, 200, &d)
			return d
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no log for request %s", reqID)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (e *env) send(token string, body string, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// The upstream usage of the mock chat completion (see mockXAI).
const mockUsage = "prompt 188 cached 128 completion 9 reasoning 173 total 370 cost 0.0004496"

func usageOf(l fullLog) string {
	return fmt.Sprintf("prompt %d cached %d completion %d reasoning %d total %d cost %v",
		l.PromptTokens, l.CachedTokens, l.CompletionTokens, l.ReasoningTokens, l.TotalTokens, l.CostUSD)
}

// Acceptance F7: every outcome produces exactly one log with the right
// fields, and token counts and cost equal the upstream's values.
func TestEveryOutcomeIsLogged(t *testing.T) {
	e := newEnv(t, "xai-a-500", "xai-b-ok")
	chat := `{"model":"grok-build-0.1","messages":[{"role":"user","content":"hi"}]}`

	t.Run("success", func(t *testing.T) {
		resp, body := e.send(e.client, chat, nil)
		l := e.logFor(resp.Header.Get("X-Request-Id"))
		if l.Status != 200 || l.Attempts != 2 || l.KeyName != "k2" || l.TokenName != "client" ||
			usageOf(l) != mockUsage || l.CostSource != "upstream" || l.ResponseBody != string(body) ||
			l.RequestBody != chat || l.ClientIP == "" {
			t.Fatalf("log %+v", l)
		}
	})

	t.Run("stream success", func(t *testing.T) {
		resp, body := e.send(e.client, `{"model":"grok-build-0.1","stream":true,"messages":[]}`, nil)
		l := e.logFor(resp.Header.Get("X-Request-Id"))
		if l.Status != 200 || !l.Stream || usageOf(l) != mockUsage || l.ResponseText != "Hello world" ||
			l.ResponseBody != string(body) {
			t.Fatalf("log %+v", l)
		}
	})

	t.Run("upstream client error", func(t *testing.T) {
		resp, body := e.send(e.client, `{"model":"bad-model","messages":[]}`, nil)
		l := e.logFor(resp.Header.Get("X-Request-Id"))
		if resp.StatusCode != 400 || l.Status != 400 || l.Error == "" || l.ResponseBody != string(body) ||
			l.CostUSD != 0 || l.CostSource != "none" {
			t.Fatalf("status %d log %+v", resp.StatusCode, l)
		}
	})

	t.Run("upstream server error on every key", func(t *testing.T) {
		e2 := newEnv(t, "xai-a-500", "xai-b-500")
		resp, _ := e2.send(e2.client, chat, nil)
		l := e2.logFor(resp.Header.Get("X-Request-Id"))
		if resp.StatusCode != 500 || l.Status != 500 || l.Attempts != 2 || l.CostUSD != 0 {
			t.Fatalf("status %d log %+v", resp.StatusCode, l)
		}
	})

	t.Run("gateway rejections", func(t *testing.T) {
		var q struct{ Token string }
		e.api("POST", "/api/v1/tokens", map[string]any{"name": "tiny", "quota_usd": 0.0000001, "allowed_models": []string{"grok-build-0.1"}}, 201, &q)
		cases := []struct {
			name, token, body string
			status            int
			code              string
		}{
			{"bad token", "sk-xr-nope", chat, 401, "invalid_api_key"},
			{"no token", "", chat, 401, "missing_api_key"},
			{"model not allowed", q.Token, `{"model":"grok-4.3","messages":[]}`, 403, "model_not_allowed"},
			{"quota", q.Token, chat, 429, "insufficient_quota"},
			{"too large", e.client, `{"model":"grok-build-0.1","x":"` + strings.Repeat("a", 2<<20) + `"}`, 413, "request_too_large"},
		}
		e.api("PUT", "/api/v1/settings", map[string]any{"max_body_mb": 1}, 200, nil)
		for _, c := range cases {
			resp, body := e.send(c.token, c.body, nil)
			l := e.logFor(resp.Header.Get("X-Request-Id"))
			if resp.StatusCode != c.status || l.Status != c.status || !strings.Contains(string(body), c.code) ||
				l.Error == "" || l.Attempts != 0 || l.CostUSD != 0 {
				t.Errorf("%s: status %d log %+v body %.120s", c.name, resp.StatusCode, l, body)
			}
		}
		e.api("PUT", "/api/v1/settings", map[string]any{"max_body_mb": 50}, 200, nil)
	})

	t.Run("client disconnects during a request", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(chat))
		req.Header.Set("Authorization", "Bearer "+e.client)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Grok-Mock-Delay-Ms", "1500")
		marker := "disconnect-" + fmt.Sprint(time.Now().UnixNano())
		req.Header.Set("User-Agent", marker)
		if _, err := http.DefaultClient.Do(req); err == nil {
			t.Fatal("request should have been cancelled")
		}
		l := e.findByUA(marker)
		if l.Status != 499 || l.Error == "" || l.CostUSD != 0 {
			t.Fatalf("log %+v", l)
		}
	})

	t.Run("client disconnects during a stream", func(t *testing.T) {
		e.mock.streamGap = 300 * time.Millisecond
		defer func() { e.mock.streamGap = 40 * time.Millisecond }()
		ctx, cancel := context.WithCancel(context.Background())
		req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"grok-build-0.1","stream":true,"messages":[]}`))
		req.Header.Set("Authorization", "Bearer "+e.client)
		req.Header.Set("Content-Type", "application/json")
		marker := "stream-disconnect-" + fmt.Sprint(time.Now().UnixNano())
		req.Header.Set("User-Agent", marker)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = bufio.NewReader(resp.Body).ReadString('\n') // first chunk arrived
		cancel()
		resp.Body.Close()
		l := e.findByUA(marker)
		if l.Status != 499 || !strings.Contains(l.Error, "client") || !strings.HasPrefix(l.ResponseText, "Hel") {
			t.Fatalf("log %+v", l)
		}
	})

	t.Run("upstream breaks a stream", func(t *testing.T) {
		resp, body := e.send(e.client, `{"model":"grok-build-0.1","stream":true,"messages":[]}`,
			map[string]string{"X-Grok-Mock-Break": "1"})
		l := e.logFor(resp.Header.Get("X-Request-Id"))
		if l.Status != 200 || !strings.Contains(l.Error, "upstream stream error") || l.ResponseText != "Hel" ||
			!bytes.Contains(body, []byte("Hel")) || l.CostUSD != 0 {
			t.Fatalf("log %+v", l)
		}
	})
}

// findByUA finds the newest log whose user agent is marker (for requests
// whose X-Request-Id the client never received).
func (e *env) findByUA(marker string) fullLog {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var page struct{ Data []fullLog }
		e.api("GET", "/api/v1/logs?page_size=50", nil, 200, &page)
		for _, l := range page.Data {
			var d struct {
				fullLog
				UserAgent string `json:"user_agent"`
			}
			e.api("GET", fmt.Sprintf("/api/v1/logs/%d", l.ID), nil, 200, &d)
			if d.UserAgent == marker {
				return d.fullLog
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("no log with user agent %s", marker)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var _ = json.Marshal
