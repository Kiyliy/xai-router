package proxy

// Regression tests for code review A of v0.5.6 (forwarding and billing path).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/store"
)

// H1: a client's include_usage:false is overridden upstream, and the usage
// chunk it did not ask for is dropped before it reaches the client.
func TestStreamUsageForcedAndStripped(t *testing.T) {
	hs := newHarness(t, nil)
	parts := strings.SplitAfter(chatStream, "\n\n")
	up := newMockUp(t, sseReply(parts...))
	hs.addKey("k1", up.srv.URL, nil)
	resp, b := hs.do(http.MethodPost, "/v1/chat/completions",
		`{"model":"grok-4.5","stream":true,"stream_options":{"include_usage":false}}`)
	if resp.StatusCode != 200 || strings.Contains(b, `"usage"`) || !strings.Contains(b, "[DONE]") {
		t.Fatalf("client stream: %d %q", resp.StatusCode, b)
	}
	if sent := up.requests()[0].Body; !strings.Contains(sent, `"include_usage":true`) || strings.Contains(sent, "false") {
		t.Fatalf("upstream body: %s", sent)
	}
	l := hs.lastLog(1)
	if l.PromptTokens != 7 || l.CompletionTokens != 2 || l.CostSource != "computed" {
		t.Fatalf("log: %+v", l)
	}
}

// H1: streams that end cleanly without their usage event are estimated, not
// billed $0: chat without a usage chunk, Responses without
// response.completed (also after response.failed), Messages without
// message_delta.
func TestStreamCleanEOFWithoutUsageEstimated(t *testing.T) {
	text := strings.Repeat("abcd", 25) // 100 bytes, about 25 tokens
	var chat []string
	for i := 0; i < 50; i++ {
		chat = append(chat, `data: {"id":"c1","model":"grok-4.5","choices":[{"delta":{"content":"`+text+`"}}]}`+"\n\n")
	}
	chat = append(chat, "data: [DONE]\n\n")
	cases := []struct {
		name, path string
		events     []string
		minOut     int64
	}{
		{"chat", "/v1/chat/completions", chat, 50 * 25},
		{"responses", "/v1/responses", []string{
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"grok-4.5\"}}\n\n",
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + text + "\"}\n\n",
		}, 25},
		{"responses failed", "/v1/responses", []string{
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"" + text + "\"}\n\n",
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r2\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n",
		}, 25},
		{"messages", "/v1/messages", []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"grok-4.5\",\"usage\":{\"input_tokens\":40,\"output_tokens\":1}}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n",
		}, 25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hs := newHarness(t, nil)
			hs.addKey("k1", newMockUp(t, sseReply(c.events...)).srv.URL, nil)
			if resp, _ := hs.do(http.MethodPost, c.path, `{"model":"grok-4.5","stream":true}`); resp.StatusCode != 200 {
				t.Fatal(resp.Status)
			}
			l := hs.lastLog(1)
			if l.Status != 200 || l.CostSource != "estimated" || l.CostTicks <= 0 || l.CompletionTokens < c.minOut || l.PromptTokens == 0 {
				t.Fatalf("log: status=%d source=%s cost=%d prompt=%d completion=%d", l.Status, l.CostSource,
					l.CostTicks, l.PromptTokens, l.CompletionTokens)
			}
		})
	}
}

// videoUp is an upstream that creates job vid1 and answers polls with the
// current status.
func videoUp(t *testing.T, status *atomic.Value) *mockUp {
	return newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"request_id":"vid1"}`)
			return
		}
		switch s := status.Load().(string); s {
		case "done":
			_, _ = io.WriteString(w, `{"status":"done","video":{"url":"https://x/v.mp4","duration":8}}`)
		default:
			_, _ = io.WriteString(w, `{"status":"`+s+`"}`)
		}
	})
}

// M1: a job whose hold fills the quota can still be polled (and so
// delivered and billed); so can a token whose stored spend reached its quota.
func TestPollNotBlockedByOwnHold(t *testing.T) {
	hs := newHarness(t, nil)
	var status atomic.Value
	status.Store("pending")
	hs.addKey("k1", videoUp(t, &status).srv.URL, nil)
	raw, tok := hs.addToken("q", func(t *store.Token) { t.QuotaTicks = billing.USDToTicks(0.40) })
	if resp, b := hs.doAs(raw, http.MethodPost, "/v1/videos/generations", `{"model":"grok-imagine-video","prompt":"x"}`); resp.StatusCode != 200 {
		t.Fatalf("create: %d %s", resp.StatusCode, b)
	}
	hs.waitLogs(1)
	if left := hs.h.Quota.Remaining(tok); left != 0 {
		t.Fatalf("hold should fill the quota, %d left", left)
	}
	if resp, b := hs.doAs(raw, http.MethodGet, "/v1/videos/vid1", ""); resp.StatusCode != 200 {
		t.Fatalf("pending poll: %d %s", resp.StatusCode, b)
	}
	// Other requests are still refused.
	if resp, _ := hs.doAs(raw, http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5"}`); resp.StatusCode != 429 {
		t.Fatalf("chat with a full quota: %d", resp.StatusCode)
	}
	status.Store("done")
	if resp, b := hs.doAs(raw, http.MethodGet, "/v1/videos/vid1", ""); resp.StatusCode != 200 {
		t.Fatalf("done poll: %d %s", resp.StatusCode, b)
	}
	l := hs.lastLog(4)
	if l.CostTicks != billing.USDToTicks(0.40) {
		t.Fatalf("done poll billed %d", l.CostTicks)
	}

	// A token whose stored spend already reached the quota polls its own job.
	raw2, tok2 := hs.addToken("spent", func(t *store.Token) { t.QuotaTicks = 100 })
	if _, err := hs.db.W.ExecContext(context.Background(), `UPDATE tokens SET used_ticks = 100 WHERE id = ?`, tok2.ID); err != nil {
		t.Fatal(err)
	}
	if resp, _ := hs.doAs(raw2, http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5"}`); resp.StatusCode != 429 {
		t.Fatalf("exhausted token admitted: %d", resp.StatusCode)
	}
	k := hs.pool.Candidates(0)[0]
	if err := hs.db.PutBinding(context.Background(), &store.Binding{ResourceID: "vid2", Kind: KindVideo,
		KeyID: k.ID, TokenID: tok2.ID, Model: "grok-imagine-video", ExpiresAt: 1 << 62}); err != nil {
		t.Fatal(err)
	}
	status.Store("pending")
	if resp, b := hs.doAs(raw2, http.MethodGet, "/v1/videos/vid2", ""); resp.StatusCode != 200 {
		t.Fatalf("exhausted token's own poll: %d %s", resp.StatusCode, b)
	}
	if resp, _ := hs.doAs(raw2, http.MethodGet, "/v1/videos/vid1", ""); resp.StatusCode != 429 {
		t.Fatalf("exhausted token polling another token's job: %d", resp.StatusCode)
	}
}

// M2: a poll that reports the job failed, expired or cancelled frees the
// job's hold at once.
func TestFailedJobReleasesHold(t *testing.T) {
	for _, final := range []string{"failed", "expired", "cancelled"} {
		t.Run(final, func(t *testing.T) {
			hs := newHarness(t, nil)
			var status atomic.Value
			status.Store("pending")
			hs.addKey("k1", videoUp(t, &status).srv.URL, nil)
			raw, tok := hs.addToken("q", func(t *store.Token) { t.QuotaTicks = billing.USDToTicks(1) })
			if resp, _ := hs.doAs(raw, http.MethodPost, "/v1/videos/generations", `{"model":"grok-imagine-video","prompt":"x"}`); resp.StatusCode != 200 {
				t.Fatal(resp.Status)
			}
			hs.waitLogs(1)
			if left := hs.h.Quota.Remaining(tok); left != billing.USDToTicks(0.60) {
				t.Fatalf("after create: %d", left)
			}
			hs.doAs(raw, http.MethodGet, "/v1/videos/vid1", "")
			hs.waitLogs(2)
			if left := hs.h.Quota.Remaining(tok); left != billing.USDToTicks(0.60) {
				t.Fatalf("pending poll released the hold: %d", left)
			}
			status.Store(final)
			hs.doAs(raw, http.MethodGet, "/v1/videos/vid1", "")
			if l := hs.lastLog(3); l.CostTicks != 0 {
				t.Fatalf("failed job billed %d", l.CostTicks)
			}
			if left := hs.h.Quota.Remaining(tok); left != billing.USDToTicks(1) {
				t.Fatalf("hold kept after %s: %d left", final, left)
			}
		})
	}
}

// M5: a request with previous_response_id reserves for the stored
// conversation it pulls in, whose size is recorded on the binding.
func TestPreviousResponseContextReserved(t *testing.T) {
	hs := newHarness(t, nil)
	up := newMockUp(t, jsonReply(200, `{"id":"resp1","object":"response","model":"grok-4.5","usage":{"input_tokens":100,"output_tokens":20}}`))
	k := hs.addKey("k1", up.srv.URL, nil)
	if resp, _ := hs.do(http.MethodPost, "/v1/responses", `{"model":"grok-4.5","input":"hi"}`); resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	hs.waitLogs(1)
	b, err := hs.db.GetBinding(context.Background(), "resp1")
	if err != nil {
		t.Fatal(err)
	}
	if got := contextTokens(b); got != 120 {
		t.Fatalf("recorded context %d (meta %q)", got, b.PriceMeta)
	}
	if got := contextTokens(nil); got != defaultContextTokens {
		t.Fatalf("unknown context %d", got)
	}

	m := hs.h.Prices.Lookup("grok-4.5")
	body := `{"model":"grok-4.5","input":"more","previous_response_id":"big"}`
	small := languageEstimate(m, int64(len(body)), 0, 0, "")
	raw, tok := hs.addToken("q", func(t *store.Token) { t.QuotaTicks = 2 * small })
	if err := hs.db.PutBinding(context.Background(), &store.Binding{ResourceID: "big", Kind: KindResponse, KeyID: k.ID,
		TokenID: tok.ID, PriceMeta: `{"context_tokens":300000}`, ExpiresAt: 1 << 62}); err != nil {
		t.Fatal(err)
	}
	resp, out := hs.doAs(raw, http.MethodPost, "/v1/responses", body)
	if resp.StatusCode != 429 || errCode(t, out) != "insufficient_quota" {
		t.Fatalf("300k-token context admitted: %d %s", resp.StatusCode, out)
	}
	var e struct{ Error struct{ Message string } }
	_ = json.Unmarshal([]byte(out), &e)
	if want := languageEstimate(m, int64(len(body)), 300000, 0, ""); !strings.Contains(e.Error.Message, "up to $") || want <= 2*small {
		t.Fatalf("estimate %d not above quota %d: %s", want, 2*small, e.Error.Message)
	}
}
