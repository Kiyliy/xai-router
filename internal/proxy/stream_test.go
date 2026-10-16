package proxy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

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
		if resp.StatusCode != 200 || b != chatStream {
			t.Fatalf("got %d %q", resp.StatusCode, b)
		}
		if resp.Header.Get("X-Accel-Buffering") != "no" || resp.Header.Get("Cache-Control") != "no-cache" {
			t.Fatalf("stream headers: %v", resp.Header)
		}
		sent := up.requests()[0].Body
		if got := strings.Contains(sent, `"include_usage":true`); got != inject {
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

func TestStreamClientDisconnect(t *testing.T) {
	hs := newHarness(t, nil)
	upDone := make(chan struct{})
	hs.addKey("k1", newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		sseReply("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")(w, r)
		<-r.Context().Done()
		close(upDone)
	}).srv.URL, nil)
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hs.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"grok-4.5","stream":true}`))
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
	<-upDone // the upstream request is cancelled too
	l := hs.lastLog(1)
	if l.Status != 499 || l.Error != "client went away" {
		t.Fatalf("log: %+v", l)
	}
	if len(l.Attempts) != 1 || l.Attempts[0].Action != pool.ActionSuccess {
		t.Fatalf("attempts: %+v", l.Attempts)
	}
}

func TestNonStreamBodyReadErrorFailsOver(t *testing.T) {
	hs := newHarness(t, nil)
	_, keys := keyedUps(hs, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"partial":`)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}, jsonReply(200, chatOK))
	resp, b := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5"}`)
	if resp.StatusCode != 200 || b != chatOK {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	l := hs.lastLog(1)
	if len(l.Attempts) != 2 || !strings.Contains(l.Attempts[0].Error, "reading upstream body") || l.KeyID != keys[1].ID {
		t.Fatalf("attempts: %+v", l.Attempts)
	}
}
