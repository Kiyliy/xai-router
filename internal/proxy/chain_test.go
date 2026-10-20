package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/store"
)

// v0.6.0: two retried attempts and a success are three log rows sharing the
// request id, numbered 1..3. Only the last carries usage and cost; the
// request body is stored on the first row only; each retried row keeps its
// own key, status, error, action and response. Stats, key and token
// counters count rows (acceptance F5).
func TestRetryChainIsOneRowPerAttempt(t *testing.T) {
	ctx := context.Background()
	hs := newHarness(t, func(s *config.Settings) { s.LogBodies = true })
	_, keys := keyedUps(hs, jsonReply(500, `{"error":"boom a"}`), jsonReply(503, `{"error":"boom b"}`), jsonReply(200, chatOK))
	const body = `{"model":"grok-4.5","messages":[]}`
	resp, b := hs.do(http.MethodPost, "/v1/chat/completions", body)
	if resp.StatusCode != 200 || b != chatOK {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
	final := hs.lastLog(1)
	chain, err := hs.db.LogChain(ctx, final.RequestID)
	if err != nil || len(chain) != 3 {
		t.Fatalf("chain: %d rows %v", len(chain), err)
	}
	wantStatus := []int{500, 503, 200}
	for i, r := range chain {
		if r.RequestID != resp.Header.Get("X-Request-Id") || r.AttemptIndex != i+1 || r.AttemptTotal != 3 ||
			r.AttemptFinal != (i == 2) || r.KeyID != keys[i].ID || r.Status != wantStatus[i] {
			t.Fatalf("row %d: %+v", i+1, r)
		}
		if i < 2 {
			if r.AttemptState() != "retried" || r.AttemptAction != pool.ActionRetry || r.CostTicks != 0 ||
				r.TotalTokens != 0 || r.PriceSnapshot != "" || r.CostSource != "none" || r.Error == "" {
				t.Fatalf("retried row %d: %+v", i+1, r)
			}
		} else if r.AttemptState() != "final" || r.AttemptAction != pool.ActionSuccess || r.CostTicks <= 0 ||
			r.TotalTokens != 15 || r.PriceSnapshot == "" {
			t.Fatalf("final row: %+v", r)
		}
		if i > 0 && (r.StartedAt < chain[i-1].StartedAt || r.CreatedAt != chain[0].CreatedAt) {
			t.Fatalf("rows out of attempt order: %+v", chain)
		}
	}
	// Bodies: the request body on row 1 only (GetLog fills it in for the
	// others), each row its own response.
	for i, r := range chain {
		d, err := hs.db.GetLog(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if d.ReqBody != body {
			t.Fatalf("row %d request body %q", i+1, d.ReqBody)
		}
		if wantFrom := map[bool]int64{true: 0, false: chain[0].ID}[i == 0]; d.ReqBodyFrom != wantFrom {
			t.Fatalf("row %d body from %d, want %d", i+1, d.ReqBodyFrom, wantFrom)
		}
		if len(d.Attempts) != 0 {
			t.Fatalf("row %d has nested attempts: %+v", i+1, d.Attempts)
		}
		want := []string{"boom a", "boom b", "Hello!"}[i]
		if !strings.Contains(d.RespBody, want) || d.RespHeaders == "{}" {
			t.Fatalf("row %d response %q %s", i+1, d.RespBody, d.RespHeaders)
		}
	}
	var stored int
	_ = hs.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM request_log_bodies b JOIN request_logs l ON l.id = b.log_id
WHERE l.request_id = ? AND b.req_body <> ''`, final.RequestID).Scan(&stored)
	if stored != 1 {
		t.Fatalf("request body stored %d times", stored)
	}
	var attemptsRows int
	_ = hs.db.R.QueryRowContext(ctx, `SELECT COUNT(*) FROM log_attempts`).Scan(&attemptsRows)
	if attemptsRows != 0 {
		t.Fatalf("log_attempts written for new data: %d", attemptsRows)
	}

	// Stats count rows: 3 requests, 1 success, 2 errors; the cost once.
	a, _ := hs.db.StatsTotal(ctx, 0)
	if a.Requests != 3 || a.Success != 1 || a.Errors != 2 || a.CostTicks != chain[2].CostTicks || a.TotalTokens != 15 {
		t.Fatalf("stats: %+v", a)
	}
	byKey, _ := hs.db.StatsBreakdown(ctx, 0, "key")
	if len(byKey) != 3 {
		t.Fatalf("stats by key: %+v", byKey)
	}
	for _, g := range byKey {
		if g.Requests != 1 {
			t.Fatalf("stats by key: %+v", byKey)
		}
	}
	for i, k := range keys {
		got, _ := hs.db.GetKey(ctx, k.ID)
		wantErr, wantCost := int64(1), int64(0)
		if i == 2 {
			wantErr, wantCost = 0, chain[2].CostTicks
		}
		if got.Attempts != 1 || got.AttemptErrors != wantErr || got.CostTicks != wantCost {
			t.Fatalf("key %d: %+v", i, got)
		}
	}
	tok, _ := hs.db.GetToken(ctx, hs.tok.ID)
	if tok.Requests != 3 || tok.UsedTicks != chain[2].CostTicks {
		t.Fatalf("token: requests %d used %d", tok.Requests, tok.UsedTicks)
	}
	// attempts=final keeps the last row only.
	if _, n, _ := hs.db.ListLogs(ctx, store.LogFilter{Limit: 10}); n != 3 {
		t.Fatalf("all rows: %d", n)
	}
	if l, n, _ := hs.db.ListLogs(ctx, store.LogFilter{Limit: 10, FinalOnly: true}); n != 1 || l[0].ID != chain[2].ID {
		t.Fatalf("final rows: %d", n)
	}
}
