package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// attempts=final estimates: a retried row is never a success and always an
// error, so the rollup is not scaled by the overall retried share (review
// v0.6.2 B-M1).
func TestEstimateFinalOnlyByStatus(t *testing.T) {
	ctx := context.Background()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := int64(1_700_000_000_000)
	single := func(id string, at int64, status int) *Log {
		return &Log{RequestID: id, CreatedAt: at, Method: "POST", Path: "/v1/chat/completions", Model: "m",
			Status: status, LatencyMs: 10, CostTicks: 1}
	}
	var logs []*Log
	// 600 single successes, 200 chains (a retried 500, then a success), 100 single errors.
	for i := range 600 {
		logs = append(logs, single(fmt.Sprintf("s%d", i), base+int64(i), 200))
	}
	for i := range 200 {
		at := base + int64(i)
		logs = append(logs, &Log{RequestID: fmt.Sprintf("c%d", i), CreatedAt: at, Method: "POST",
			Path: "/v1/chat/completions", Model: "m", Status: 200, KeyID: 0, LatencyMs: 3000, EndAt: at + 3000,
			CostTicks: 10, Attempts: []Attempt{
				{Seq: 1, Status: 500, Action: "retry", Error: "boom", StartedAt: at, LatencyMs: 1000},
				{Seq: 2, Status: 200, Action: "success", StartedAt: at + 1000}}})
	}
	for i := range 100 {
		logs = append(logs, single(fmt.Sprintf("e%d", i), base+int64(i), 502))
	}
	if err := db.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	for st, want := range map[string]int64{"": 900, "success": 800, "error": 100} {
		est, ok := db.estimateCount(ctx, LogFilter{FinalOnly: true, Status: st, From: base - 1})
		if !ok || est != want {
			t.Errorf("final-only status=%q: estimate %d (ok %v), want %d", st, est, ok, want)
		}
	}
	// Within one model, key or token the retried rows are not counted on
	// the partial index: no estimate, so the bounded exact count is used.
	if _, ok := db.estimateCount(ctx, LogFilter{FinalOnly: true, Status: "error", Model: "m", From: base - 1}); ok {
		t.Error("final-only errors of one model: want no estimate")
	}
	if est, ok := db.estimateCount(ctx, LogFilter{FinalOnly: true, Status: "success", Model: "m", From: base - 1}); !ok || est != 800 {
		t.Errorf("final-only successes of one model: estimate %d (ok %v), want 800", est, ok)
	}
}
