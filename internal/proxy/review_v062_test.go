package proxy

// Regression tests for code review A of v0.6.2 (findings A1 to A3).

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/pool"
)

// costErrBody is an upstream error that nevertheless reports what the
// attempt cost.
const costErrBody = `{"error":{"message":"server exploded","type":"server_error"},"usage":{"prompt_tokens":10,"completion_tokens":0,"total_tokens":10,"cost_in_usd_ticks":7000}}`

func tokenUsed(t *testing.T, hs *harness) int64 {
	t.Helper()
	tok, err := hs.db.GetToken(context.Background(), hs.tok.ID)
	if err != nil {
		t.Fatal(err)
	}
	return tok.UsedTicks
}

// A1: when every attempt fails and each reports a cost, each row is billed
// its own cost exactly once: the retried row from the attempt, the final row
// from its error's usage.
func TestFinalErrorCostBilledOnce(t *testing.T) {
	hs := newHarness(t, func(s *config.Settings) { s.MaxAttempts = 2 })
	keyedUps(hs, jsonReply(500, costErrBody), jsonReply(500, costErrBody))
	if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","messages":[]}`); resp.StatusCode != 500 {
		t.Fatal(resp.Status)
	}
	final := hs.lastLog(1)
	chain, err := hs.db.LogChain(context.Background(), final.RequestID)
	if err != nil || len(chain) != 2 {
		t.Fatalf("chain: %d rows %v", len(chain), err)
	}
	for _, r := range chain {
		if r.CostTicks != 7000 || r.CostSource != "upstream" {
			t.Fatalf("row %d/%d: cost %d source %s", r.AttemptIndex, r.AttemptTotal, r.CostTicks, r.CostSource)
		}
	}
	if f := chain[1]; !f.AttemptFinal || f.PromptTokens != 10 {
		t.Fatalf("final row: %+v", f)
	}
	if used := tokenUsed(t, hs); used != 14000 {
		t.Fatalf("token used %d, want 14000", used)
	}
}

// A1: a non-retryable error (an invalid request) that reports a cost is
// billed it.
func TestNonRetryableErrorCostBilled(t *testing.T) {
	hs := newHarness(t, nil)
	errBody := strings.Replace(costErrBody, "server_error", "invalid_request_error", 1)
	ups, _ := keyedUps(hs, jsonReply(400, errBody), jsonReply(200, chatOK))
	if resp, _ := hs.do(http.MethodPost, "/v1/chat/completions", `{"model":"grok-4.5","messages":[]}`); resp.StatusCode != 400 {
		t.Fatal(resp.Status)
	}
	if n := len(ups[1].requests()); n != 0 {
		t.Fatalf("an invalid request was failed over (%d)", n)
	}
	l := hs.lastLog(1)
	if len(l.Attempts) != 1 || l.Status != 400 || l.CostTicks != 7000 || l.CostSource != "upstream" || l.PromptTokens != 10 {
		t.Fatalf("log: attempts %d status %d cost %d source %s prompt %d", len(l.Attempts), l.Status, l.CostTicks, l.CostSource, l.PromptTokens)
	}
	if used := tokenUsed(t, hs); used != 7000 {
		t.Fatalf("token used %d, want 7000", used)
	}
}

// A2: an in-stream error that arrives with usage carrying a cost, before
// any content, is not failed over: the upstream billed the attempt, so it
// is final and billed, as an error after content is.
func TestStreamErrorWithCostNotRetried(t *testing.T) {
	ev := `data: {"id":"c1","model":"grok-4.5","choices":[],"error":{"message":"` + capacityMsg +
		`","type":"server_error","code":"resource-exhausted"},"usage":{"prompt_tokens":100,"completion_tokens":0,"total_tokens":100,"cost_in_usd_ticks":9000}}` + "\n\n"
	hs := newHarness(t, nil)
	ups, keys := keyedUps(hs, sseReply(ev, doneEvent), sseReply(chatStream))
	resp, b := hs.do(http.MethodPost, "/v1/chat/completions",
		`{"model":"grok-4.5","stream":true,"stream_options":{"include_usage":true},"messages":[]}`)
	if resp.StatusCode != 200 || !strings.Contains(b, capacityMsg) {
		t.Fatalf("client got %d %q", resp.StatusCode, b)
	}
	if n := len(ups[1].requests()); n != 0 {
		t.Fatalf("a billed attempt was failed over (%d requests to the next key)", n)
	}
	l := hs.lastLog(1)
	checkStreamFailed(t, hs, l, capacityMsg)
	if len(l.Attempts) != 1 || l.KeyID != keys[0].ID || l.CostTicks != 9000 || l.CostSource != "upstream" || l.PromptTokens != 100 {
		t.Fatalf("log: attempts %d key %d cost %d source %s prompt %d", len(l.Attempts), l.KeyID, l.CostTicks, l.CostSource, l.PromptTokens)
	}
	if used := tokenUsed(t, hs); used != 9000 {
		t.Fatalf("token used %d, want 9000", used)
	}
}

// A3: a key whose streams answer 200 and then fail with a 5xx-class
// in-stream error opens its breaker after BreakerThreshold attempts, as the
// same error answered as an HTTP 500 does. The 200 headers no longer count
// as a success that resets the failure count first.
func TestBreakerOpensOnInStreamErrors(t *testing.T) {
	const body = `{"model":"grok-4.5","stream":true,"messages":[]}`
	internal := `data: {"error":{"message":"internal failure","type":"server_error","code":"internal"}}` + "\n\n"
	for _, c := range []struct {
		name string
		fn   http.HandlerFunc
	}{
		{"http 500", jsonReply(500, `{"error":{"message":"internal failure","type":"server_error"}}`)},
		{"in-stream 500", sseReply(internal, doneEvent)},
	} {
		t.Run(c.name, func(t *testing.T) {
			hs := newHarness(t, func(s *config.Settings) { s.BreakerThreshold = 2; s.MaxAttempts = 1 })
			_, keys := keyedUps(hs, c.fn)
			hs.do(http.MethodPost, "/v1/chat/completions", body)
			if st := hs.pool.State(keys[0].ID); !st.CooldownUntil.IsZero() {
				t.Fatalf("breaker open after one failure: %+v", st)
			}
			hs.do(http.MethodPost, "/v1/chat/completions", body)
			st := hs.pool.State(keys[0].ID)
			if st.CooldownUntil.IsZero() || !strings.Contains(st.Reason, "circuit open") {
				t.Fatalf("breaker not open after %d consecutive failures: %+v", 2, st)
			}
		})
	}
}

// A3: the deferred success report still resets the failure count once a
// stream delivers content, and a capacity error in the stream (a 429 "at
// capacity") neither counts towards the breaker nor cools the key down.
func TestStreamOutcomeReportedOnce(t *testing.T) {
	const body = `{"model":"grok-4.5","stream":true,"messages":[]}`
	internal := `data: {"error":{"message":"internal failure","type":"server_error","code":"internal"}}` + "\n\n"
	t.Run("content resets", func(t *testing.T) {
		hs := newHarness(t, func(s *config.Settings) { s.BreakerThreshold = 2; s.MaxAttempts = 1 })
		ups, keys := keyedUps(hs, sseReply(internal, doneEvent))
		hs.do(http.MethodPost, "/v1/chat/completions", body)
		ups[0].set(sseReply(chatStream))
		if resp, b := hs.do(http.MethodPost, "/v1/chat/completions", body); resp.StatusCode != 200 || !strings.Contains(b, "Hel") {
			t.Fatalf("client got %d %q", resp.StatusCode, b)
		}
		ups[0].set(sseReply(internal, doneEvent))
		hs.do(http.MethodPost, "/v1/chat/completions", body)
		if st := hs.pool.State(keys[0].ID); !st.CooldownUntil.IsZero() {
			t.Fatalf("breaker opened though a success came between the failures: %+v", st)
		}
	})
	t.Run("capacity", func(t *testing.T) {
		hs := newHarness(t, func(s *config.Settings) { s.BreakerThreshold = 2; s.MaxAttempts = 1 })
		_, keys := keyedUps(hs, sseReply(chatErrEvent, doneEvent))
		for i := 0; i < 4; i++ {
			hs.do(http.MethodPost, "/v1/chat/completions", body)
		}
		l := hs.lastLog(4)
		if a := l.Attempts[0]; a.Action != pool.ActionRetry {
			t.Fatalf("attempt %+v", a)
		}
		if st := hs.pool.State(keys[0].ID); !st.CooldownUntil.IsZero() {
			t.Fatalf("capacity errors cooled the key down: %+v", st)
		}
	})
}
