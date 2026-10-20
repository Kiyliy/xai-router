package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/store"
)

// In-stream errors as xAI sends them while a model is overloaded (round 7,
// N1): HTTP 200, then an error event instead of content.
const (
	capacityMsg     = "The model is currently at capacity due to high demand. Please try again later."
	chatErrEvent    = `data: {"error":{"message":"` + capacityMsg + `","type":"server_error","code":"resource-exhausted"}}` + "\n\n"
	chatXAIErrEvent = `data: {"code":"resource-exhausted","error":"` + capacityMsg + `"}` + "\n\n"
	respCreated     = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_e\",\"model\":\"grok-4.5\",\"status\":\"in_progress\",\"error\":null}}\n\n"
	respErrEvent    = "event: error\ndata: {\"type\":\"error\",\"code\":\"resource-exhausted\",\"message\":\"" + capacityMsg + "\",\"param\":null}\n\n"
	respFailedEvent = "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_e\",\"status\":\"failed\",\"error\":{\"code\":\"resource-exhausted\",\"message\":\"" + capacityMsg + "\"}}}\n\n"
	msgStart        = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_e\",\"model\":\"grok-4.5\",\"usage\":{\"input_tokens\":40,\"output_tokens\":1}}}\n\n"
	msgErrEvent     = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"" + capacityMsg + "\"}}\n\n"
	doneEvent       = "data: [DONE]\n\n"
)

// checkStreamFailed asserts the log of a stream that failed in-stream after
// answering 200: logged as a failed request with the upstream error.
func checkStreamFailed(t *testing.T, hs *harness, l *store.Log, wantInError string) {
	t.Helper()
	if l.Status != streamErrorLogStatus || l.Success() {
		t.Fatalf("status %d success=%v; want a failed request", l.Status, l.Success())
	}
	mustContain(t, "error", l.Error, "upstream error in stream (HTTP 200 sent)", wantInError)
	// The 200 the client saw is kept in extra.
	var extra struct {
		StreamError struct {
			ClientStatus int `json:"client_status"`
		} `json:"stream_error"`
	}
	if json.Unmarshal([]byte(l.Extra), &extra) != nil || extra.StreamError.ClientStatus != 200 {
		t.Fatalf("extra should keep the 200 the client saw: %s", l.Extra)
	}
	// Counted by the "problems" filter and as an error in the stats.
	list, _, err := hs.db.ListLogs(context.Background(), store.LogFilter{Problems: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range list {
		found = found || x.ID == l.ID
	}
	if !found {
		t.Fatal("not listed by the problems filter")
	}
}

func TestStreamErrorOnlyIsFailedAndFree(t *testing.T) {
	cases := []struct {
		name, path, body string
		events           []string
		want             string // in the logged error
	}{
		{"chat openai shape", "/v1/chat/completions", `{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"hi there"}]}`,
			[]string{chatErrEvent, doneEvent}, "resource-exhausted: server_error: " + capacityMsg},
		{"chat xai shape", "/v1/chat/completions", `{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"hi there"}]}`,
			[]string{chatXAIErrEvent, doneEvent}, "resource-exhausted: " + capacityMsg},
		{"chat role chunk first", "/v1/chat/completions", `{"model":"grok-4.5","stream":true}`,
			[]string{"data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", chatErrEvent, doneEvent}, "resource-exhausted"},
		{"responses error event", "/v1/responses", `{"model":"grok-4.5","stream":true,"input":"hi there"}`,
			[]string{respCreated, respErrEvent}, "resource-exhausted: " + capacityMsg},
		{"responses failed", "/v1/responses", `{"model":"grok-4.5","stream":true,"input":"hi there"}`,
			[]string{respCreated, respFailedEvent}, "resource-exhausted: " + capacityMsg},
		{"messages error", "/v1/messages", `{"model":"grok-4.5","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`,
			[]string{msgStart, msgErrEvent}, "overloaded_error: " + capacityMsg},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hs := newHarness(t, nil)
			up := newMockUp(t, sseReply(c.events...))
			k := hs.addKey("k1", up.srv.URL, nil)
			resp, b := hs.do(http.MethodPost, c.path, c.body)
			// The only key: the error is passed through as the upstream sent it.
			if resp.StatusCode != 200 || !strings.Contains(b, capacityMsg) {
				t.Fatalf("client got %d %q", resp.StatusCode, b)
			}
			if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
				t.Fatalf("content type %q", ct)
			}
			l := hs.lastLog(1)
			checkStreamFailed(t, hs, l, c.want)
			// Nothing generated, no usage: not billed, with its own source and
			// the estimate kept for audit.
			if l.CostTicks != 0 || l.CostSource != "upstream_error" || l.PromptTokens != 0 || l.CompletionTokens != 0 {
				t.Fatalf("billing: %+v", l)
			}
			var extra struct {
				StreamError struct {
					Code         string
					Message      string
					ClientStatus int   `json:"client_status"`
					Estimated    int64 `json:"estimated_cost_ticks"`
				} `json:"stream_error"`
			}
			if err := json.Unmarshal([]byte(l.Extra), &extra); err != nil || extra.StreamError.Estimated <= 0 ||
				extra.StreamError.ClientStatus != 200 || extra.StreamError.Message != capacityMsg {
				t.Fatalf("extra %q: %v", l.Extra, err)
			}
			tok, err := hs.db.GetToken(context.Background(), hs.tok.ID)
			if err != nil || tok.UsedTicks != 0 {
				t.Fatalf("token used %+v %v", tok, err)
			}
			// resource-exhausted at capacity: the key is not cooled down (as
			// for an HTTP 429 "at capacity"), and the attempt says retry.
			if a := l.Attempts[0]; a.Action != pool.ActionRetry {
				t.Fatalf("attempt %+v", a)
			}
			if st := hs.pool.State(k.ID); !st.CooldownUntil.IsZero() {
				t.Fatalf("key cooled down: %+v", st)
			}
		})
	}
}

func TestStreamErrorAfterContentBillsContent(t *testing.T) {
	text := strings.Repeat("abcd", 30) // 120 bytes: 30 tokens estimated
	cases := []struct {
		name, path, body string
		events           []string
		prompt           int64 // 0: estimated from the request
	}{
		{"chat", "/v1/chat/completions", `{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			[]string{"data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"" + text + "\"}}]}\n\n", chatErrEvent, doneEvent}, 0},
		{"responses", "/v1/responses", `{"model":"grok-4.5","stream":true,"input":"hi"}`,
			[]string{respCreated, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + text + "\"}\n\n", respErrEvent}, 0},
		{"messages", "/v1/messages", `{"model":"grok-4.5","stream":true,"max_tokens":100}`,
			[]string{msgStart, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n", msgErrEvent}, 40},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hs := newHarness(t, nil)
			ups, _ := keyedUps(hs, sseReply(c.events...), sseReply(chatStream))
			resp, b := hs.do(http.MethodPost, c.path, c.body)
			// Content reached the client: no retry, the error is passed through.
			if resp.StatusCode != 200 || !strings.Contains(b, text) || !strings.Contains(b, capacityMsg) {
				t.Fatalf("client got %d %q", resp.StatusCode, b)
			}
			if n := len(ups[1].requests()); n != 0 {
				t.Fatalf("retried after content was sent (%d)", n)
			}
			l := hs.lastLog(1)
			checkStreamFailed(t, hs, l, capacityMsg)
			if l.RespText != text || l.CompletionTokens != 30 || l.CostSource != "estimated" || l.CostTicks <= 0 {
				t.Fatalf("billing: %+v", l)
			}
			if c.prompt != 0 && l.PromptTokens != c.prompt {
				t.Fatalf("prompt %d want %d", l.PromptTokens, c.prompt)
			}
			want, _ := hs.h.Prices.Cost("grok-4.5", billing.Usage{Found: true, PromptTokens: l.PromptTokens, CompletionTokens: 30}, billing.Request{})
			if l.CostTicks != want {
				t.Fatalf("cost %d want %d", l.CostTicks, want)
			}
		})
	}
}

// Usage the upstream reported alongside the error is billed as usual.
func TestStreamErrorWithUsageUsesUsage(t *testing.T) {
	hs := newHarness(t, nil)
	hs.addKey("k1", newMockUp(t, sseReply(
		"data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
		chatErrEvent,
		"data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"cost_in_usd_ticks\":777000}}\n\n",
		doneEvent)).srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","stream":true}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	l := hs.lastLog(1)
	checkStreamFailed(t, hs, l, "resource-exhausted")
	if l.PromptTokens != 12 || l.CompletionTokens != 3 || l.CostTicks != 777000 || l.CostSource != "upstream" {
		t.Fatalf("billing: %+v", l)
	}
}

// An error before any byte reached the client is retried on the next key,
// like an HTTP error; the client sees only the successful stream.
func TestStreamErrorBeforeFirstByteRetries(t *testing.T) {
	cases := []struct {
		name     string
		event    string
		cooldown bool
		action   string
	}{
		// Model-wide capacity: the next key, no cooldown (as an HTTP 429 "at capacity").
		{"capacity", chatErrEvent, false, pool.ActionRetry},
		// The key's own rate limit: cooled down (as an HTTP 429).
		{"rate limited", `data: {"code":"resource-exhausted","error":"Rate limit exceeded for this team"}` + "\n\n", true, pool.ActionCooldown},
		// Overloaded upstream: a 5xx-style retry that counts towards the breaker.
		{"overloaded", `data: {"error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n", false, pool.ActionRetry},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hs := newHarness(t, nil)
			ups, keys := keyedUps(hs, sseReply(c.event, doneEvent), sseReply(chatStream))
			resp, b := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","stream":true,"stream_options":{"include_usage":true}}`)
			if resp.StatusCode != 200 || b != chatStream {
				t.Fatalf("client got %d %q", resp.StatusCode, b)
			}
			if len(ups[0].requests()) != 1 || len(ups[1].requests()) != 1 {
				t.Fatal("expected one request per key")
			}
			l := hs.lastLog(1)
			if l.Status != 200 || l.Error != "" || l.KeyID != keys[1].ID || l.CostSource != "computed" || l.PromptTokens != 7 {
				t.Fatalf("log: %+v", l)
			}
			if len(l.Attempts) != 2 {
				t.Fatalf("attempts: %+v", l.Attempts)
			}
			// The retried attempt is its own row, failed with the status its
			// in-stream error stands for.
			a := l.Attempts[0]
			if a.Status < 400 || a.Action != c.action || !strings.Contains(a.Error, "before any byte was sent") {
				t.Fatalf("first attempt: %+v", a)
			}
			if st := hs.pool.State(keys[0].ID); st.CooldownUntil.IsZero() == c.cooldown {
				t.Fatalf("cooldown=%v want %v", !st.CooldownUntil.IsZero(), c.cooldown)
			}
		})
	}
}

// A stream held back for its first content is released after firstByteHold,
// so a slow model still gets its headers and preamble early.
func TestStreamHoldReleasesPreamble(t *testing.T) {
	old := firstByteHold
	firstByteHold = 50 * time.Millisecond
	defer func() { firstByteHold = old }()
	hs := newHarness(t, nil)
	release := make(chan struct{})
	hs.addKey("k1", newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		sseReply(respCreated)(w, r)
		<-release
		sseReply("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")(w, r)
	}).srv.URL, nil)
	req, _ := http.NewRequest(http.MethodPost, hs.srv.URL+"/v1/responses", strings.NewReader(`{"model":"grok-4.5","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+hs.token)
	got := make(chan *http.Response, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			close(release)
			return
		}
		got <- resp
	}()
	select {
	case resp := <-got:
		buf := make([]byte, len(respCreated))
		_, err := resp.Body.Read(buf)
		close(release)
		resp.Body.Close()
		if err != nil || !strings.Contains(string(buf), "response.created") {
			t.Fatalf("preamble %q %v", buf, err)
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("headers held while waiting for content")
	}
}

func TestStreamErrorStatus(t *testing.T) {
	for _, c := range []struct {
		e    billing.StreamError
		want int
	}{
		{billing.StreamError{Code: "resource-exhausted", Message: capacityMsg}, 429},
		{billing.StreamError{Type: "rate_limit_error"}, 429},
		{billing.StreamError{Type: "overloaded_error", Message: "Overloaded"}, 503},
		{billing.StreamError{Type: "invalid_request_error", Message: "bad"}, 400},
		{billing.StreamError{Message: "boom"}, 500},
	} {
		if got := streamErrorStatus(&c.e); got != c.want {
			t.Errorf("%+v: %d want %d", c.e, got, c.want)
		}
	}
}
