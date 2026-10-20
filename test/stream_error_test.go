package test

import (
	"fmt"
	"strings"
	"testing"
)

// Round 7 N1: a stream that answers 200 and then only an in-stream error is
// a failed request in the logs (level error, problems filter, failure
// stats) and is not billed; with another key available it is retried there.
func TestInStreamErrorLoggedAsFailure(t *testing.T) {
	e := newEnv(t, "xai-a-streamerr")
	body := map[string]any{"model": "grok-build-0.1", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	b := e.proxy("POST", "/v1/chat/completions", body, 200, nil)
	if !strings.Contains(string(b), "resource-exhausted") {
		t.Fatalf("client body %q", b)
	}
	e.waitLogs(1)
	var page struct {
		Data []struct {
			ID         int64
			Status     int
			Level      string
			Error      string
			CostUSD    float64 `json:"cost_usd"`
			CostSource string  `json:"cost_source"`
		}
	}
	e.api("GET", "/api/v1/logs?problems=1", nil, 200, &page)
	if len(page.Data) != 1 {
		t.Fatalf("problems filter: %+v", page.Data)
	}
	l := page.Data[0]
	if l.Status != 502 || l.Level != "error" || !strings.Contains(l.Error, "resource-exhausted") ||
		!strings.Contains(l.Error, "HTTP 200 sent") || l.CostUSD != 0 || l.CostSource != "upstream_error" {
		t.Fatalf("log: %+v", l)
	}
	// The 200 the client saw is in the detail's extra.
	var detail struct {
		Extra struct {
			StreamError struct {
				ClientStatus int    `json:"client_status"`
				Code         string `json:"code"`
			} `json:"stream_error"`
		}
	}
	e.api("GET", fmt.Sprintf("/api/v1/logs/%d", l.ID), nil, 200, &detail)
	if detail.Extra.StreamError.ClientStatus != 200 || detail.Extra.StreamError.Code == "" {
		t.Fatalf("extra: %+v", detail.Extra)
	}
	var ov struct{ Requests, Success, Errors int }
	e.api("GET", "/api/v1/stats/overview", nil, 200, &ov)
	if ov.Requests != 1 || ov.Success != 0 {
		t.Fatalf("overview: %+v", ov)
	}
}

func TestInStreamErrorRetriedOnNextKey(t *testing.T) {
	e := newEnv(t, "xai-a-streamerr", "xai-b-ok")
	body := map[string]any{"model": "grok-build-0.1", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	b := e.proxy("POST", "/v1/chat/completions", body, 200, nil)
	if strings.Contains(string(b), "resource-exhausted") || !strings.Contains(string(b), "[DONE]") {
		t.Fatalf("client body %q", b)
	}
	l := e.waitLogs(1)[0]
	if l.Status != 200 || l.AttemptTotal != 2 || l.KeyName != "k2" || l.Error != "" {
		t.Fatalf("log: %+v", l)
	}
	// The abandoned stream is a retried row of its own: failed (429, the
	// status its capacity error stands for), on the first key, free.
	r := e.waitRows(2, "")[1]
	if r.AttemptState != "retried" || r.KeyName != "k1" || r.Status != 429 || r.CostUSD != 0 || r.Level != "warn" ||
		!strings.Contains(r.Error, "before any byte was sent") {
		t.Fatalf("retried row: %+v", r)
	}
}
