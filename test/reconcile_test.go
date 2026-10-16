package test

import (
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

func ticks(usd float64) int64 { return int64(math.Round(usd * 1e10)) }

// reconcile returns the four totals acceptance F8 compares, in ticks.
func (e *env) reconcile() (overview, logs, tokens, keys int64) {
	e.t.Helper()
	var ov struct {
		CostUSD float64 `json:"cost_usd"`
	}
	e.api("GET", "/api/v1/stats/overview?range=24h", nil, 200, &ov)
	overview = ticks(ov.CostUSD)
	for page := 1; ; page++ {
		var p struct {
			Data []struct {
				CostUSD float64 `json:"cost_usd"`
			}
			Total int
		}
		e.api("GET", fmt.Sprintf("/api/v1/logs?page_size=200&page=%d", page), nil, 200, &p)
		for _, l := range p.Data {
			logs += ticks(l.CostUSD)
		}
		if page*200 >= p.Total {
			break
		}
	}
	var tp struct {
		Data []struct {
			UsedUSD float64 `json:"used_usd"`
		}
	}
	e.api("GET", "/api/v1/tokens?page_size=200", nil, 200, &tp)
	for _, t := range tp.Data {
		tokens += ticks(t.UsedUSD)
	}
	var kp struct {
		Data []struct {
			CostUSD float64 `json:"cost_usd"`
		}
	}
	e.api("GET", "/api/v1/keys?page_size=200", nil, 200, &kp)
	for _, k := range kp.Data {
		keys += ticks(k.CostUSD)
	}
	return
}

// Acceptance F8: overview total = sum of log costs = sum of token spend =
// sum of key spend, to the tick, after a mixed workload.
func TestCostsReconcile(t *testing.T) {
	e := newEnv(t, "xai-a-429", "xai-b-500", "xai-c-ok", "xai-d-nocost")
	var second struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "second", "quota_usd": 5}, 201, &second)
	chat := map[string]any{"model": "grok-build-0.1", "messages": []any{}}
	for _, tok := range []string{e.client, second.Token} {
		e.do("POST", "/v1/chat/completions", tok, chat, 200, nil)
		e.do("POST", "/v1/chat/completions", tok, map[string]any{"model": "grok-build-0.1", "stream": true, "messages": []any{}}, 200, nil)
		e.do("POST", "/v1/responses", tok, map[string]any{"model": "grok-4.3", "input": "x"}, 200, nil)
		e.do("POST", "/v1/images/generations", tok, map[string]any{"model": "grok-imagine-image", "prompt": "p", "n": 2}, 200, nil)
		e.do("POST", "/v1/tts", tok, map[string]any{"text": "hello", "language": "en"}, 200, nil)
		e.do("POST", "/v1/chat/completions", tok, map[string]any{"model": "bad-model"}, 0, nil) // 400 upstream or 403 unpriced
		e.do("POST", "/v1/chat/completions", "sk-xr-bad", chat, 401, nil)
		var sub struct {
			RequestID string `json:"request_id"`
		}
		e.do("POST", "/v1/videos/generations", tok, map[string]any{"model": "grok-imagine-video", "prompt": "p"}, 200, &sub)
		e.do("GET", "/v1/videos/"+sub.RequestID, tok, nil, 202, nil)
		e.do("GET", "/v1/videos/"+sub.RequestID, tok, nil, 200, nil)
		e.do("GET", "/v1/videos/"+sub.RequestID, tok, nil, 200, nil) // second done poll: not billed again
	}
	// A WebSocket session (priced from published rates).
	c, err := e.dialWS("/v1/tts?language=en", http.Header{"Authorization": {"Bearer " + second.Token}})
	if err != nil || c.resp.StatusCode != 101 {
		t.Fatalf("ws: %v", err)
	}
	_ = c.send(0x1, []byte(`{"type":"text.delta","delta":"hello there"}`))
	c.recv(t)
	_ = c.send(0x8, closePayload(1000, ""))
	c.recv(t)
	c.conn.Close()

	time.Sleep(1500 * time.Millisecond) // let the log writer flush
	ov, logs, toks, keys := e.reconcile()
	t.Logf("overview %d, logs %d, tokens %d, keys %d ticks", ov, logs, toks, keys)
	if logs == 0 || ov != logs || logs != toks || toks != keys {
		t.Fatalf("totals differ: overview %d, logs %d, tokens %d, keys %d ticks", ov, logs, toks, keys)
	}
}
