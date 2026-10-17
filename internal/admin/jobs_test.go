package admin

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/store"
)

// waitJob polls a job until it finishes.
func (h *harness) waitJob(id int64) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j := h.must(h.call(http.MethodGet, "/api/v1/jobs/"+itoa(id), ""), 200)
		if j["state"] != "running" {
			return j
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("job %d still running: %v", id, j)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPurgeRunsAsJob(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	now := store.NowMs()
	var logs []*store.Log
	for i := 0; i < 2500; i++ { // more than two purge chunks
		logs = append(logs, &store.Log{RequestID: "r", CreatedAt: now - 10*86400_000 + int64(i), Method: "POST",
			Path: "/v1/chat/completions", Status: 200, CostTicks: 1})
	}
	logs = append(logs, &store.Log{RequestID: "keep", CreatedAt: now, Method: "POST", Path: "/v1/x", Status: 200})
	if err := h.db.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	j := h.must(h.call(http.MethodDelete, "/api/v1/logs?before="+itoa((now-86400_000)/1000), ""), 202)
	if j["kind"] != "purge_logs" || j["state"] != "running" {
		t.Fatalf("job %v", j)
	}
	done := h.waitJob(num(j["id"]))
	res, _ := done["result"].(map[string]any)
	if done["state"] != "done" || num(res["deleted"]) != 2500 || num(done["total"]) != 2500 || num(done["progress"]) != 2500 {
		t.Fatalf("purge job %v", done)
	}
	if _, n, _ := h.db.ListLogs(ctx, store.LogFilter{Limit: 10}); n != 1 {
		t.Fatalf("%d logs left", n)
	}
	// Stats survive the purge.
	if a, _ := h.db.StatsTotal(ctx, 0); a.Requests != 2501 {
		t.Fatalf("stats %+v", a)
	}
	jobs := h.must(h.call(http.MethodGet, "/api/v1/jobs", ""), 200)
	if num(jobs["total"]) != 1 {
		t.Fatalf("jobs %v", jobs)
	}
	h.must(h.call(http.MethodGet, "/api/v1/jobs/999", ""), 404)
	h.must(h.call(http.MethodGet, "/api/v1/jobs/x", ""), 400)
}

func TestAsyncKeyTestsAndCompact(t *testing.T) {
	h := newHarness(t)
	h.must(h.call(http.MethodPost, "/api/v1/keys", `{"key":"xai-ok-async"}`), 201)
	if err := h.s.Pool.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	j := h.must(h.call(http.MethodPost, "/api/v1/keys/test", `{"async":true,"probe":false}`), 202)
	done := h.waitJob(num(j["id"]))
	res, _ := done["result"].(map[string]any)
	results, _ := res["results"].([]any)
	if done["state"] != "done" || len(results) != 1 || num(done["progress"]) != 1 {
		t.Fatalf("test job %v", done)
	}
	c := h.must(h.call(http.MethodPost, "/api/v1/system/compact", ""), 202)
	if d := h.waitJob(num(c["id"])); d["state"] != "done" {
		t.Fatalf("compact %v", d)
	}
	info := h.must(h.call(http.MethodGet, "/api/v1/system/info", ""), 200)
	for _, k := range []string{"log_count", "db_free_bytes", "go_heap_bytes", "goroutines", "db_auto_vacuum"} {
		if _, ok := info[k]; !ok {
			t.Errorf("system info lacks %s", k)
		}
	}
}

func TestOnlyOneJobPerKind(t *testing.T) {
	var js jobs
	release := make(chan struct{})
	a, started := js.start("x", func(ctx context.Context, j *Job) (any, error) { <-release; return nil, nil })
	b, again := js.start("x", func(ctx context.Context, j *Job) (any, error) { return nil, nil })
	if !started || again || a.ID != b.ID {
		t.Fatalf("second start must return the running job: %v %v", a, b)
	}
	close(release)
}
