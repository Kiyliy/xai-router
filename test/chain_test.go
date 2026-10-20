package test

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// v0.6.0 reconciliation with retry chains (acceptance F5 and F8): whatever
// the chain, token used = sum of log row costs = sum of key costs = stats
// cost, and the stats count one request per row.
func TestRetryChainsReconcile(t *testing.T) {
	chat := map[string]any{"model": "grok-build-0.1", "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	stream := map[string]any{"model": "grok-build-0.1", "stream": true, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
	type keyRow struct {
		Name    string
		CostUSD float64 `json:"cost_usd"`
	}
	keyCosts := func(e *env) map[string]int64 {
		var kp struct{ Data []keyRow }
		e.api("GET", "/api/v1/keys?page_size=200", nil, 200, &kp)
		out := map[string]int64{}
		for _, k := range kp.Data {
			out[k.Name] = ticks(k.CostUSD)
		}
		return out
	}
	check := func(t *testing.T, e *env, rows int, wantSuccess, wantErrors int) []logRow {
		t.Helper()
		all := e.waitRows(rows, "")
		time.Sleep(600 * time.Millisecond) // let the counters settle
		ov, logs, toks, keys := e.reconcile()
		if ov != logs || logs != toks || toks != keys {
			t.Fatalf("totals differ: overview %d, logs %d, tokens %d, keys %d ticks", ov, logs, toks, keys)
		}
		var o struct{ Requests, Success, Errors int }
		e.api("GET", "/api/v1/stats/overview", nil, 200, &o)
		if o.Requests != rows || o.Success != wantSuccess || o.Errors != wantErrors {
			t.Fatalf("overview %+v, want %d requests, %d success, %d errors", o, rows, wantSuccess, wantErrors)
		}
		var tp struct {
			Data []struct{ Requests int }
		}
		e.api("GET", "/api/v1/tokens?page_size=200", nil, 200, &tp)
		if len(tp.Data) == 0 || tp.Data[0].Requests != rows {
			t.Fatalf("token requests: %+v, want %d rows", tp.Data, rows)
		}
		return all
	}

	t.Run("two retries then success", func(t *testing.T) {
		e := newEnv(t, "xai-a-500", "xai-b-500", "xai-c-ok")
		resp, _ := e.send(e.client, `{"model":"grok-build-0.1","messages":[]}`, nil)
		if resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
		rows := check(t, e, 3, 1, 2) // F5: N+1 requests, 1 success, N failures
		for i, r := range rows {
			if r.RequestID != resp.Header.Get("X-Request-Id") || r.AttemptIndex != 3-i || r.AttemptTotal != 3 {
				t.Fatalf("row %d: %+v", i, r)
			}
			if billed := r.CostUSD > 0; billed != (i == 0) {
				t.Fatalf("row %d billed=%v: %+v", i, billed, r)
			}
		}
	})

	t.Run("every attempt failed, one with an upstream cost", func(t *testing.T) {
		e := newEnv(t, "xai-a-500cost", "xai-b-500")
		resp, _ := e.send(e.client, `{"model":"grok-build-0.1","messages":[]}`, nil)
		if resp.StatusCode != 500 {
			t.Fatal(resp.Status)
		}
		rows := check(t, e, 2, 0, 2)
		if rows[0].AttemptState != "final" || rows[0].Level != "error" || rows[0].CostUSD != 0 ||
			rows[1].AttemptState != "retried" || ticks(rows[1].CostUSD) != 1230000 || rows[1].CostSource != "upstream" {
			t.Fatalf("rows: %+v", rows)
		}
		if kc := keyCosts(e); kc["k1"] != 1230000 || kc["k2"] != 0 {
			t.Fatalf("key costs: %v", kc)
		}
	})

	t.Run("error after partial content", func(t *testing.T) {
		e := newEnv(t, "xai-a-partialerr", "xai-b-ok")
		b := e.proxy("POST", "/v1/chat/completions", stream, 200, nil)
		if !strings.Contains(string(b), "partial") {
			t.Fatalf("client body %q", b)
		}
		rows := check(t, e, 1, 0, 1) // not retried: content had been sent
		l := rows[0]
		if l.AttemptTotal != 1 || l.Status != 502 || l.KeyName != "k1" || l.CostUSD <= 0 || l.CostSource != "estimated" {
			t.Fatalf("log: %+v", l)
		}
		var d struct {
			Extra struct {
				StreamError struct {
					ClientStatus int    `json:"client_status"`
					Message      string `json:"message"`
					AfterContent bool   `json:"after_content"`
				} `json:"stream_error"`
			}
		}
		e.api("GET", fmt.Sprintf("/api/v1/logs/%d", l.ID), nil, 200, &d)
		if se := d.Extra.StreamError; se.ClientStatus != 200 || !se.AfterContent || !strings.Contains(se.Message, "capacity") {
			t.Fatalf("extra: %+v", d.Extra)
		}
	})

	t.Run("key switch", func(t *testing.T) {
		e := newEnv(t, "xai-a-429", "xai-b-ok")
		e.proxy("POST", "/v1/chat/completions", chat, 200, nil)
		rows := check(t, e, 2, 1, 1)
		if rows[0].KeyName != "k2" || rows[1].KeyName != "k1" || rows[1].Status != 429 || rows[1].CostUSD != 0 {
			t.Fatalf("rows: %+v", rows)
		}
		kc := keyCosts(e)
		if kc["k1"] != 0 || kc["k2"] != ticks(rows[0].CostUSD) || kc["k2"] == 0 {
			t.Fatalf("key costs: %v", kc)
		}
		// Each row counts under its own key.
		var br struct {
			Data []struct {
				Name     string
				Requests int
				Errors   int
			}
		}
		e.api("GET", "/api/v1/stats/breakdown?by=key", nil, 200, &br)
		got := map[string]string{}
		for _, g := range br.Data {
			got[g.Name] = fmt.Sprintf("%d/%d", g.Requests, g.Errors)
		}
		if got["k1"] != "1/1" || got["k2"] != "1/0" {
			t.Fatalf("by key: %v", got)
		}
	})
}

// Design handoff §10: newest first, a chain is contiguous with its final row
// on top, and the cooldown event its first attempt caused (written while
// the chain was still running) comes after the whole chain, on one page and
// across cursor pages.
func TestChainContiguousWithEvents(t *testing.T) {
	e := newEnv(t, "xai-a-429", "xai-b-500", "xai-c-ok")
	resp, _ := e.send(e.client, `{"model":"grok-build-0.1","messages":[]}`, nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	e.waitRows(3, "")
	type row struct {
		Type         string
		ID           int64
		Action       string
		RequestID    string    `json:"request_id"`
		AttemptIndex int       `json:"attempt_index"`
		CreatedAt    time.Time `json:"created_at"`
		StartedAt    time.Time `json:"started_at"`
	}
	label := func(r row) string {
		if r.Type == "request" {
			return fmt.Sprintf("attempt%d", r.AttemptIndex)
		}
		return r.Action
	}
	var page struct {
		Data       []row
		NextCursor string `json:"next_cursor"`
	}
	e.api("GET", "/api/v1/logs?type=request,system&page_size=50", nil, 200, &page)
	var got []string
	for _, r := range page.Data {
		if r.Type == "request" || r.Action == "key.cooldown" {
			got = append(got, label(r))
		}
	}
	if want := "attempt3 attempt2 attempt1 key.cooldown"; strings.Join(got, " ") != want {
		t.Fatalf("order %v, want %s", got, want)
	}
	// The rows share the list time; each keeps its own start.
	reqs := page.Data[:3]
	if !reqs[0].CreatedAt.Equal(reqs[2].CreatedAt) || reqs[2].StartedAt.After(reqs[0].StartedAt) || reqs[2].StartedAt.IsZero() ||
		reqs[0].StartedAt.After(reqs[0].CreatedAt) {
		t.Fatalf("times: %+v", reqs)
	}
	// Page by one: the same order.
	got = nil
	cursor := ""
	for i := 0; i < 20; i++ {
		var p struct {
			Data       []row
			NextCursor string `json:"next_cursor"`
		}
		e.api("GET", "/api/v1/logs?type=request,system&page_size=1&cursor="+cursor, nil, 200, &p)
		for _, r := range p.Data {
			if r.Type == "request" || r.Action == "key.cooldown" {
				got = append(got, label(r))
			}
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if want := "attempt3 attempt2 attempt1 key.cooldown"; strings.Join(got, " ") != want {
		t.Fatalf("cursor order %v, want %s", got, want)
	}
}
