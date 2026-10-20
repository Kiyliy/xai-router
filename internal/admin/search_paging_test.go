package admin

import (
	"context"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/store"
)

func insertSearchLogs(t *testing.T, h *harness) {
	t.Helper()
	base := store.NowMs() - 100_000
	var logs []*store.Log
	for i := 0; i < 30; i++ { // matched by the error text "timeout"
		logs = append(logs, &store.Log{RequestID: "e" + itoa(int64(i)), CreatedAt: base + int64(i)*100, Method: "POST",
			Path: "/v1/chat/completions", Status: 504, Error: "upstream timeout (resp_abc)"})
	}
	for i := 0; i < 12; i++ { // share one upstream id
		logs = append(logs, &store.Log{RequestID: "u" + itoa(int64(i)), CreatedAt: base + 5000 + int64(i)*100,
			Method: "POST", Path: "/v1/responses", Status: 200, UpstreamID: "resp_abc"})
	}
	if err := h.db.InsertLogs(context.Background(), logs); err != nil {
		t.Fatal(err)
	}
}

// M1: in the number-paged request-only view, an id-like query that falls
// back to the error-text search keeps that mode on later pages.
func TestRequestViewSearchPagesKeepMode(t *testing.T) {
	h := newHarness(t)
	insertSearchLogs(t, h)
	seen := map[int64]bool{}
	for page := 1; page <= 3; page++ {
		p := logsOf(t, h, "?type=request&q=timeout&page_size=10&page="+itoa(int64(page)))
		rows := list(p)
		if num(p["total"]) != 30 || len(rows) != 10 {
			t.Fatalf("page %d: total %v rows %d", page, p["total"], len(rows))
		}
		for _, r := range rows {
			if seen[num(r["id"])] || !strings.Contains(r["error"].(string), "timeout") {
				t.Fatalf("page %d: row %v", page, r)
			}
			seen[num(r["id"])] = true
		}
	}
	// An id search pages through the id matches only.
	p := logsOf(t, h, "?type=request&q=resp_abc&page_size=5&page=3")
	if num(p["total"]) != 12 || len(list(p)) != 2 {
		t.Fatalf("id search page 3: total %v rows %d", p["total"], len(list(p)))
	}
}

// M1: in the cursor view, a request/upstream id search stays an id search
// on later pages (it used to switch to the error-text search).
func TestCursorViewIDSearchKeepsMode(t *testing.T) {
	h := newHarness(t)
	insertSearchLogs(t, h)
	for _, c := range []struct {
		q    string
		want int
		path string
	}{{"resp_abc", 12, "/v1/responses"}, {"timeout", 30, "/v1/chat/completions"}} {
		seen := map[int64]bool{}
		cur := ""
		for pages := 0; ; pages++ {
			if pages > 20 {
				t.Fatal("cursor never ends")
			}
			q := "?type=request,system&q=" + c.q + "&page_size=5"
			if cur != "" {
				q += "&cursor=" + cur
			}
			p := logsOf(t, h, q)
			for _, r := range list(p) {
				if r["type"] != "request" {
					continue
				}
				if r["path"] != c.path || seen[num(r["id"])] {
					t.Fatalf("q=%s page %d: unexpected row %v", c.q, pages, r)
				}
				seen[num(r["id"])] = true
			}
			if cur, _ = p["next_cursor"].(string); cur == "" {
				break
			}
		}
		if len(seen) != c.want {
			t.Fatalf("q=%s: walked %d requests, want %d", c.q, len(seen), c.want)
		}
	}
}
