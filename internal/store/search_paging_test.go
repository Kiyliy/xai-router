package store

import (
	"context"
	"strconv"
	"testing"
)

// M1: the search mode of an id-like query is decided from the query and the
// filters, not from the page, so offset and cursor pages agree with page 1.
func TestLogSearchModeSameOnEveryPage(t *testing.T) {
	ctx := context.Background()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var logs []*Log
	base := NowMs() - 100_000
	for i := 0; i < 30; i++ {
		logs = append(logs, &Log{RequestID: "e" + strconv.Itoa(i), CreatedAt: base + int64(i), Method: "POST",
			Path: "/v1/chat/completions", Status: 504, Error: "upstream timeout"})
	}
	for i := 0; i < 12; i++ {
		logs = append(logs, &Log{RequestID: "u" + strconv.Itoa(i), CreatedAt: base + 1000 + int64(i), Method: "POST",
			Path: "/v1/responses", Status: 200, UpstreamID: "timeout"}) // an id that also appears in error texts
	}
	if err := db.InsertLogs(ctx, logs); err != nil {
		t.Fatal(err)
	}
	// "rate_limit" matches no id: every page searches error texts.
	if err := db.InsertLogs(ctx, []*Log{{RequestID: "x1", CreatedAt: base, Status: 429, Error: "rate_limit hit"},
		{RequestID: "x2", CreatedAt: base, Status: 429, Error: "rate_limit hit"},
		{RequestID: "x3", CreatedAt: base, Status: 429, Error: "rate_limit hit"}}); err != nil {
		t.Fatal(err)
	}
	p1, err := db.ListLogsPage(ctx, LogFilter{Q: "rate_limit", Limit: 2})
	if err != nil || len(p1.Logs) != 2 || p1.Total != 3 {
		t.Fatalf("page 1: %+v %v", p1, err)
	}
	p2, err := db.ListLogsPage(ctx, LogFilter{Q: "rate_limit", Limit: 2, Offset: 2})
	if err != nil || len(p2.Logs) != 1 || p2.Total != 3 {
		t.Fatalf("page 2 (offset): rows %d total %d %v", len(p2.Logs), p2.Total, err)
	}
	last := p1.Logs[1]
	pc, err := db.ListLogsPage(ctx, LogFilter{Q: "rate_limit", Limit: 2, NoCount: true, BeforeAt: last.CreatedAt, BeforeID: last.ID})
	if err != nil || len(pc.Logs) != 1 || pc.Logs[0].Error != "rate_limit hit" {
		t.Fatalf("page 2 (cursor): %+v %v", pc, err)
	}

	// "timeout" is an upstream id: every page, offset or cursor, is the id search.
	seen := 0
	f := LogFilter{Q: "timeout", Limit: 5}
	for page := 0; page < 10; page++ {
		p, err := db.ListLogsPage(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range p.Logs {
			if l.UpstreamID != "timeout" {
				t.Fatalf("page %d: error-text row %s in an id search", page, l.RequestID)
			}
			seen++
		}
		if len(p.Logs) < f.Limit {
			break
		}
		l := p.Logs[len(p.Logs)-1]
		f.NoCount, f.BeforeAt, f.BeforeID = true, l.CreatedAt, l.ID
	}
	if seen != 12 {
		t.Fatalf("cursor walk saw %d id matches", seen)
	}
}
