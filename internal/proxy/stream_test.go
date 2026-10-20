package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/pool"
)

const chatStream = "data: {\"id\":\"c1\",\"model\":\"grok-4.5\",\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"lo\",\"reasoning_content\":\"r\"}}]}\n\n" +
	": keep-alive\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
	"data: [DONE]\n\n"

// sseReply streams the given events, flushing after each one.
func sseReply(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, e := range events {
			_, _ = io.WriteString(w, e)
			w.(http.Flusher).Flush()
		}
	}
}

func TestStreamChatCompletions(t *testing.T) {
	for _, inject := range []bool{true, false} {
		hs := newHarness(t, func(s *config.Settings) { s.InjectStreamUsage = inject })
		parts := strings.SplitAfter(chatStream, "\n\n")
		up := newMockUp(t, sseReply(parts...))
		hs.addKey("k1", up.srv.URL, nil)
		resp, b := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","stream":true}`)
		// Usage is always requested upstream; with injection off, a client
		// that did not ask for it does not see the usage chunk.
		want := chatStream
		if !inject {
			want = strings.Replace(chatStream, parts[3], "", 1)
		}
		if resp.StatusCode != 200 || b != want {
			t.Fatalf("got %d %q", resp.StatusCode, b)
		}
		if resp.Header.Get("X-Accel-Buffering") != "no" || resp.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("stream headers: %v", resp.Header)
		}
		sent := up.requests()[0].Body
		if !strings.Contains(sent, `"include_usage":true`) {
			t.Fatalf("inject=%v but upstream body %s", inject, sent)
		}
		l := hs.lastLog(1)
		if !l.Stream || l.RespText != "Hello" || l.ReasoningText != "r" || l.UpstreamID != "c1" ||
			l.PromptTokens != 7 || l.CompletionTokens != 2 || l.CostTicks == 0 || l.Error != "" {
			t.Fatalf("log: %+v", l)
		}
		if !strings.Contains(l.RespBody, "[DONE]") || l.TTFBMs < 0 {
			t.Fatalf("resp body: %q", l.RespBody)
		}
	}
}

func TestStreamWithoutBodies(t *testing.T) {
	hs := newHarness(t, func(s *config.Settings) { s.LogBodies = false })
	hs.addKey("k1", newMockUp(t, sseReply(chatStream)).srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","stream":true}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	l := hs.lastLog(1)
	if l.HasBodies || l.PromptTokens != 7 {
		t.Fatalf("log: %+v", l)
	}
}

func TestStreamResponsesBindsID(t *testing.T) {
	hs := newHarness(t, nil)
	k := hs.addKey("k1", newMockUp(t, sseReply(
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Yo\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_s\",\"model\":\"grok-4.5\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5}}}\n\n",
	)).srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/responses", `{"model":"grok-4.5","stream":true}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	l := hs.lastLog(1)
	if l.RespText != "Yo" || l.UpstreamID != "resp_s" || l.PromptTokens != 3 || l.CompletionTokens != 5 {
		t.Fatalf("log: %+v", l)
	}
	b, err := hs.db.GetBinding(context.Background(), "resp_s")
	if err != nil || b.Kind != KindResponse || b.KeyID != k.ID || b.TokenID != hs.tok.ID {
		t.Fatalf("binding %+v %v", b, err)
	}
}

func TestStreamMessagesFix(t *testing.T) {
	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"grok-4.5\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}", // unterminated: flushed at EOF
	}
	for _, fix := range []bool{true, false} {
		hs := newHarness(t, func(s *config.Settings) { s.AnthropicStreamFix = fix })
		hs.addKey("k1", newMockUp(t, sseReply(events...)).srv.URL, nil)
		resp, b := hs.do(http.MethodPost, "/v1/messages", `{"model":"grok-4.5","stream":true}`)
		if resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
		if got := strings.Contains(b, `{"index":0,"type":"content_block_delta"`); got != fix {
			t.Fatalf("fix=%v: %s", fix, b)
		}
		if !strings.HasSuffix(b, `{"type":"message_stop"}`) {
			t.Fatalf("trailing event lost: %q", b)
		}
		l := hs.lastLog(1)
		if l.RespText != "Hi" || l.PromptTokens != 4 || l.CompletionTokens != 6 || l.UpstreamID != "msg_1" {
			t.Fatalf("log: %+v", l)
		}
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	hs := newHarness(t, func(s *config.Settings) { s.StreamIdleTimeoutSeconds = 1 })
	hs.addKey("k1", newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		sseReply("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")(w, r)
		<-r.Context().Done()
	}).srv.URL, nil)
	resp, b := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","stream":true}`)
	if resp.StatusCode != 200 || !strings.Contains(b, `"a"`) {
		t.Fatalf("got %d %q", resp.StatusCode, b)
	}
	l := hs.lastLog(1)
	if !strings.Contains(l.Error, "stream idle") || l.Status != 200 || l.RespText != "a" {
		t.Fatalf("log: %+v", l)
	}
}

func TestStreamUpstreamBreaks(t *testing.T) {
	hs := newHarness(t, nil)
	hs.addKey("k1", newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		sseReply("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")(w, r)
		panic(http.ErrAbortHandler)
	}).srv.URL, nil)
	resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	if l := hs.lastLog(1); !strings.Contains(l.Error, "upstream stream error") {
		t.Fatalf("log: %+v", l)
	}
}

// startAndDrop starts a streaming chat request, reads the first line and
// disconnects.
func startAndDrop(t *testing.T, hs *harness) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hs.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"count to sixty"}]}`))
	req.Header.Set("Authorization", "Bearer "+hs.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(resp.Body).ReadString('\n')
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("first line %q", line)
	}
	cancel()
	resp.Body.Close()
}

// A client that disconnects mid-stream is still billed what the upstream
// reports at the end of the stream (acceptance F7).
func TestStreamClientDisconnect(t *testing.T) {
	hs := newHarness(t, nil)
	resume := make(chan struct{})
	upDone := make(chan error, 1)
	hs.addKey("k1", newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		sseReply("data: {\"choices\":[{\"delta\":{\"content\":\"1 2 \"}}]}\n\n")(w, r)
		<-resume
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"3 4\",\"reasoning_content\":\"r\"}}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":191,\"completion_tokens\":120,"+
			"\"completion_tokens_details\":{\"reasoning_tokens\":97},\"cost_in_usd_ticks\":8894000}}\n\ndata: [DONE]\n\n")
		w.(http.Flusher).Flush()
		upDone <- r.Context().Err() // the gateway kept the upstream request open
	}).srv.URL, nil)
	startAndDrop(t, hs)
	time.Sleep(50 * time.Millisecond)
	close(resume)
	if err := <-upDone; err != nil {
		t.Fatalf("upstream request was cancelled: %v", err)
	}
	l := hs.lastLog(1)
	if l.Status != 499 || l.Error != "client went away" || l.RespText != "1 2 3 4" {
		t.Fatalf("log: %+v", l)
	}
	if l.PromptTokens != 191 || l.CompletionTokens != 120 || l.ReasoningTokens != 97 ||
		l.CostTicks != 8894000 || l.CostSource != "upstream" {
		t.Fatalf("usage not billed: %+v", l)
	}
	if len(l.Attempts) != 1 || l.Attempts[0].Action != pool.ActionSuccess {
		t.Fatalf("attempts: %+v", l.Attempts)
	}
	tok, err := hs.h.DB.GetToken(context.Background(), l.TokenID)
	if err != nil || tok.UsedTicks != 8894000 {
		t.Fatalf("token used %+v %v", tok, err)
	}
}

// When the upstream never finishes after the client left, the gateway gives
// up after clientGoneDrain and bills an estimate, never zero.
func TestStreamClientDisconnectEstimated(t *testing.T) {
	old := clientGoneDrain
	clientGoneDrain = 200 * time.Millisecond
	defer func() { clientGoneDrain = old }()
	hs := newHarness(t, nil)
	upDone := make(chan struct{})
	hs.addKey("k1", newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		sseReply("data: {\"choices\":[{\"delta\":{\"content\":\""+strings.Repeat("word ", 40)+"\"}}]}\n\n")(w, r)
		<-r.Context().Done()
		close(upDone)
	}).srv.URL, nil)
	startAndDrop(t, hs)
	<-upDone // abandoned after the drain limit
	l := hs.lastLog(1)
	if l.Status != 499 || !strings.Contains(l.Error, "abandoned") {
		t.Fatalf("log: %+v", l)
	}
	if l.CompletionTokens != 50 || l.PromptTokens == 0 || l.CostTicks <= 0 || l.CostSource != "estimated" {
		t.Fatalf("estimate: %+v", l)
	}
}

// A non-streaming response that arrives after the client left is billed.
func TestNonStreamClientDisconnectBilled(t *testing.T) {
	hs := newHarness(t, nil)
	upDone := make(chan error, 1)
	hs.addKey("k1", newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		jsonReply(200, chatOK)(w, r)
		upDone <- r.Context().Err()
	}).srv.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hs.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"grok-4.5"}`))
	req.Header.Set("Authorization", "Bearer "+hs.token)
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("request should time out")
	}
	if err := <-upDone; err != nil {
		t.Fatalf("upstream cancelled: %v", err)
	}
	l := hs.lastLog(1)
	if l.Status != 499 || l.PromptTokens == 0 || l.CostTicks == 0 {
		t.Fatalf("log: %+v", l)
	}
}

// A body that breaks after 2xx headers is not retried on another key: the
// upstream already ran (and billed) the request, and a POST such as a video
// generation would run twice. The client gets 502 and the attempt is billed
// as an estimate (review A M4).
func TestNonStreamBodyReadErrorNoFailover(t *testing.T) {
	hs := newHarness(t, nil)
	broken := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"partial":`)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	ups, keys := keyedUps(hs, broken, jsonReply(200, chatOK))
	for i, c := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"grok-4.5"}`},
		{"/v1/videos/generations", `{"model":"grok-imagine-video","prompt":"x"}`},
	} {
		resp, b := hs.do(http.MethodPost, c.path, c.body)
		if resp.StatusCode != 502 || !strings.Contains(b, "interrupted") {
			t.Fatalf("%s: got %d %s", c.path, resp.StatusCode, b)
		}
		l := hs.lastLog(i + 1)
		if len(l.Attempts) != 1 || !strings.Contains(l.Attempts[0].Error, "reading upstream body") ||
			l.KeyID != keys[0].ID || l.Status != 502 || l.CostSource != "estimated" || l.CostTicks <= 0 {
			t.Fatalf("%s: log %+v attempts %+v", c.path, l, l.Attempts)
		}
	}
	if n := len(ups[1].requests()); n != 0 {
		t.Fatalf("second key called %d times", n)
	}
}

// A Messages stream cut after message_start (which carries only the prompt)
// is billed the prompt plus an estimate of the output received, and a
// priority stream keeps its multiplier (review H1/H2).
func TestCutMessagesStreamEstimatesOutput(t *testing.T) {
	hs := newHarness(t, nil)
	hs.addKey("k1", newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		sseReply("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"grok-4.5\",\"usage\":{\"input_tokens\":100,\"output_tokens\":1}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\""+strings.Repeat("abcd", 50)+"\"}}\n\n")(w, r)
		panic(http.ErrAbortHandler)
	}).srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/messages", `{"model":"grok-4.5","stream":true,"max_tokens":10}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	l := hs.lastLog(1)
	if l.PromptTokens != 100 || l.CompletionTokens != 50 || l.CostSource != "estimated" || l.CostTicks <= 0 {
		t.Fatalf("log: %+v", l)
	}
	want, _ := hs.h.Prices.Cost("grok-4.5", billing.Usage{Found: true, PromptTokens: 100, CompletionTokens: 50}, billing.Request{})
	if l.CostTicks != want {
		t.Fatalf("cost %d want %d", l.CostTicks, want)
	}
}

// A client that closes the connection right after the last event got the
// whole stream: it is a success, not a 499 (review H3).
func TestCloseAfterLastEventIsSuccess(t *testing.T) {
	hs := newHarness(t, nil)
	parts := strings.SplitAfter(chatStream, "\n\n")
	hs.addKey("k1", newMockUp(t, sseReply(parts...)).srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hs.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"grok-4.5","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+hs.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil || strings.Contains(line, "[DONE]") {
			break
		}
	}
	cancel()
	resp.Body.Close()
	l := hs.lastLog(1)
	if l.Status != 200 || l.Error != "" || l.CostSource != "computed" || l.PromptTokens != 7 {
		t.Fatalf("log: %+v", l)
	}
}
