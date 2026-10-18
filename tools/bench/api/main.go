// Command api measures admin API latency on a large database (acceptance
// P1/P2): log listing with filters and deep pages, log detail, and every
// statistics endpoint. Prints a Markdown table of p50/p95/max per query.
//
//	go run ./tools/bench/api -base http://127.0.0.1:18090 -password ... -n 20
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

var (
	base   = flag.String("base", "http://127.0.0.1:18090", "gateway URL")
	pw     = flag.String("password", os.Getenv("XR_ADMIN_PASSWORD"), "admin password")
	n      = flag.Int("n", 20, "runs per query")
	limit  = flag.Duration("p95", 500*time.Millisecond, "p95 limit for list/stats queries")
	dlimit = flag.Duration("detail", 200*time.Millisecond, "p95 limit for log detail")
	token  string
)

func call(path string) (int, []byte, time.Duration) {
	req, _ := http.NewRequest(http.MethodGet, *base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	t0 := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, b, time.Since(t0)
}

func pct(d []time.Duration, p float64) time.Duration {
	s := append([]time.Duration(nil), d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(float64(len(s))*p+0.999999) - 1
	return s[max(i, 0)]
}

func main() {
	flag.Parse()
	body, _ := json.Marshal(map[string]string{"password": *pw})
	resp, err := http.Post(*base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatal(err)
	}
	var lg struct{ Token string }
	_ = json.NewDecoder(resp.Body).Decode(&lg)
	resp.Body.Close()
	if lg.Token == "" {
		log.Fatal("login failed")
	}
	token = lg.Token

	// Discover ids to filter on.
	_, kb, _ := call("/api/v1/keys?page_size=200")
	_, tb, _ := call("/api/v1/tokens?page_size=200")
	var keys, toks struct{ Data []struct{ ID int64 } }
	_ = json.Unmarshal(kb, &keys)
	_ = json.Unmarshal(tb, &toks)
	_, lb, _ := call("/api/v1/logs?page_size=1")
	var first struct {
		Data []struct {
			ID        int64  `json:"id"`
			RequestID string `json:"request_id"`
		}
		Total int64
	}
	_ = json.Unmarshal(lb, &first)
	if len(first.Data) == 0 || len(keys.Data) == 0 || len(toks.Data) == 0 {
		log.Fatal("database has no logs/keys/tokens")
	}
	maxID := first.Data[0].ID
	k1, t1 := keys.Data[0].ID, toks.Data[0].ID
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).Unix()
	twoDays := time.Now().Add(-48 * time.Hour).Unix()

	type q struct{ name, path string }
	list := []q{
		{"logs page 1", "/api/v1/logs"},
		{"logs page 1, 200/page", "/api/v1/logs?page_size=200"},
		{"model", "/api/v1/logs?model=grok-4.3"},
		{"status=error", "/api/v1/logs?status=error"},
		{"status=success", "/api/v1/logs?status=success"},
		{"status=429", "/api/v1/logs?status=429"},
		{"status=5xx", "/api/v1/logs?status=5xx"},
		{"key", fmt.Sprintf("/api/v1/logs?key_id=%d", k1)},
		{"token", fmt.Sprintf("/api/v1/logs?token_id=%d", t1)},
		{"token+success", fmt.Sprintf("/api/v1/logs?token_id=%d&status=success", t1)},
		{"key+error", fmt.Sprintf("/api/v1/logs?key_id=%d&status=error", k1)},
		{"token+5xx", fmt.Sprintf("/api/v1/logs?token_id=%d&status=5xx", t1)},
		{"model+429", "/api/v1/logs?model=grok-4.5&status=429"},
		// Acceptance round 2 (P1): sparse / missing model with status, and deep pages under filters.
		{"missing model+4xx+token", fmt.Sprintf("/api/v1/logs?model=grok-nope&status=4xx&token_id=%d", t1)},
		{"missing model+error", "/api/v1/logs?model=grok-nope&status=error"},
		{"model+success+token, page 300", fmt.Sprintf("/api/v1/logs?model=grok-4.6&status=success&token_id=%d&page=300", t1)},
		{"status=200+key, page 5000", fmt.Sprintf("/api/v1/logs?status=200&key_id=%d&page=5000", k1)},
		{"model+200+key, page 500", fmt.Sprintf("/api/v1/logs?model=grok-4.6&status=200&key_id=%d&page=500", k1)},
		{"status=200, page 20000", "/api/v1/logs?status=200&page=20000"},
		{"path", "/api/v1/logs?path=/v1/messages"},
		{"q=request id", "/api/v1/logs?q=" + first.Data[0].RequestID},
		{"q=error text", "/api/v1/logs?q=internal"},
		{"q=text, no match", "/api/v1/logs?q=hello"},
		{"q=text+model", "/api/v1/logs?q=timeout&model=grok-4.5"},
		{"q=text+key", fmt.Sprintf("/api/v1/logs?q=rate&key_id=%d", k1)},
		{"q=text matching 60k", "/api/v1/logs?q=rate"},
		{"model+status=error", "/api/v1/logs?model=grok-4.7&status=error"},
		{"key+token+model", fmt.Sprintf("/api/v1/logs?key_id=%d&token_id=%d&model=grok-4.3", k1, t1)},
		{"7d+model+status=429", fmt.Sprintf("/api/v1/logs?from=%d&model=grok-4.5&status=429", weekAgo)},
		{"2d window+token+path", fmt.Sprintf("/api/v1/logs?from=%d&token_id=%d&path=/v1/responses", twoDays, t1)},
		{"page 50,000 (offset 1M)", "/api/v1/logs?page=50000"},
		{"page 149,000 (near end)", "/api/v1/logs?page=149000"},
		{"model, page 10,000", "/api/v1/logs?model=grok-4.3&page=10000"},
		{"status=error, page 5,000", "/api/v1/logs?status=error&page=5000"},
		{"keys list", "/api/v1/keys"},
		{"tokens list", "/api/v1/tokens"},
	}
	// The unified log (v0.4.1, acceptance U27): requests merged with audit
	// and system events. Deep pages use a cursor 20 days back.
	deep := fmt.Sprintf("%d.0.%d", time.Now().Add(-20*24*time.Hour).UnixMilli(), maxID)
	mixed := []q{
		{"type=audit", "/api/v1/logs?type=audit"},
		{"type=system", "/api/v1/logs?type=system"},
		{"type=audit,system", "/api/v1/logs?type=audit,system"},
		{"type=audit, action prefix", "/api/v1/logs?type=audit&action=token.quota_"},
		{"mixed page 1", "/api/v1/logs?type=request,audit,system"},
		{"mixed page 1, 200/page", "/api/v1/logs?type=request,audit,system&page_size=200"},
		{"mixed problems", "/api/v1/logs?type=request,audit,system&problems=1"},
		{"mixed deep cursor (20 d back)", "/api/v1/logs?type=request,audit,system&cursor=" + deep},
		{"mixed problems, deep cursor", "/api/v1/logs?type=request,audit,system&problems=1&cursor=" + deep},
		{"mixed+key", fmt.Sprintf("/api/v1/logs?type=request,audit,system&key_id=%d", k1)},
		{"mixed+key, deep cursor", fmt.Sprintf("/api/v1/logs?type=request,audit,system&key_id=%d&cursor=%s", k1, deep)},
		{"mixed+token", fmt.Sprintf("/api/v1/logs?type=request,audit,system&token_id=%d", t1)},
		{"mixed+key+problems", fmt.Sprintf("/api/v1/logs?type=request,audit,system&key_id=%d&problems=1", k1)},
		{"system+key, deep cursor", fmt.Sprintf("/api/v1/logs?type=system&key_id=%d&cursor=%s", k1, deep)},
		{"mixed q=text", "/api/v1/logs?type=request,audit,system&q=invalid"},
	}
	stats := []q{}
	for _, rg := range []string{"7d", "30d"} {
		stats = append(stats,
			q{"overview " + rg, "/api/v1/stats/overview?range=" + rg},
			q{"timeseries " + rg, "/api/v1/stats/timeseries?range=" + rg},
			q{"timeseries " + rg + " by model", "/api/v1/stats/timeseries?group=model&range=" + rg},
			q{"timeseries " + rg + " by key", "/api/v1/stats/timeseries?group=key&range=" + rg},
			q{"timeseries " + rg + " by token", "/api/v1/stats/timeseries?group=token&range=" + rg},
			q{"breakdown " + rg + " by model", "/api/v1/stats/breakdown?by=model&range=" + rg},
			q{"breakdown " + rg + " by key", "/api/v1/stats/breakdown?by=key&range=" + rg},
			q{"breakdown " + rg + " by token", "/api/v1/stats/breakdown?by=token&range=" + rg},
		)
	}
	stats = append(stats, q{"overview 24h", "/api/v1/stats/overview?range=24h"},
		q{"timeseries 24h", "/api/v1/stats/timeseries?range=24h"}, q{"timeseries 1h (5 min)", "/api/v1/stats/timeseries?range=1h"},
		q{"system info", "/api/v1/system/info"})

	fails := 0
	run := func(section string, qs []q, lim time.Duration) {
		fmt.Printf("\n### %s\n\n| query | total | p50 | p95 | max | ok |\n|---|---|---|---|---|---|\n", section)
		for _, x := range qs {
			var ds []time.Duration
			var total string
			for i := 0; i < *n; i++ {
				st, b, d := call(x.path)
				if st != 200 {
					log.Fatalf("%s: HTTP %d %s", x.path, st, b)
				}
				ds = append(ds, d)
				if i == 0 {
					var t struct {
						Total      *int64 `json:"total"`
						TotalExact *bool  `json:"total_exact"`
					}
					_ = json.Unmarshal(b, &t)
					if t.Total != nil {
						total = fmt.Sprint(*t.Total)
						if t.TotalExact != nil && !*t.TotalExact {
							total = "~" + total
						}
					}
				}
			}
			p95 := pct(ds, 0.95)
			ok := p95 < lim
			if !ok {
				fails++
			}
			fmt.Printf("| %s | %s | %v | %v | %v | %s |\n", x.name, total, pct(ds, 0.5).Round(time.Millisecond),
				p95.Round(time.Millisecond), pct(ds, 1).Round(time.Millisecond), map[bool]string{true: "✅", false: "❌"}[ok])
		}
	}
	run("Log list (P1)", list, *limit)
	run("Unified log (U27)", mixed, *limit)
	run("Statistics (P2)", stats, *limit)

	// Log detail: random ids, plus the largest bodies.
	var ds []time.Duration
	r := rand.New(rand.NewPCG(7, 8))
	for i := 0; i < *n*5; i++ {
		id := 1 + r.Int64N(maxID)
		st, _, d := call(fmt.Sprintf("/api/v1/logs/%d", id))
		if st != 200 && st != 404 {
			log.Fatalf("detail %d: %d", id, st)
		}
		ds = append(ds, d)
	}
	// Event detail.
	_, eb, _ := call("/api/v1/logs?type=audit,system&page_size=1")
	var ev struct{ Data []struct{ ID int64 } }
	_ = json.Unmarshal(eb, &ev)
	var eds []time.Duration
	if len(ev.Data) > 0 {
		for i := 0; i < *n*5; i++ {
			id := 1 + r.Int64N(ev.Data[0].ID)
			st, _, d := call(fmt.Sprintf("/api/v1/events/%d", id))
			if st != 200 && st != 404 {
				log.Fatalf("event %d: %d", id, st)
			}
			eds = append(eds, d)
		}
	}
	p95 := pct(ds, 0.95)
	if p95 >= *dlimit {
		fails++
	}
	fmt.Printf("\n### Log detail (P1)\n\n| query | runs | p50 | p95 | max | ok |\n|---|---|---|---|---|---|\n")
	fmt.Printf("| random log detail | %d | %v | %v | %v | %s |\n", len(ds), pct(ds, 0.5).Round(time.Millisecond),
		p95.Round(time.Millisecond), pct(ds, 1).Round(time.Millisecond), map[bool]string{true: "✅", false: "❌"}[p95 < *dlimit])
	if len(eds) > 0 {
		ep := pct(eds, 0.95)
		if ep >= *dlimit {
			fails++
		}
		fmt.Printf("| random event detail | %d | %v | %v | %v | %s |\n", len(eds), pct(eds, 0.5).Round(time.Millisecond),
			ep.Round(time.Millisecond), pct(eds, 1).Round(time.Millisecond), map[bool]string{true: "✅", false: "❌"}[ep < *dlimit])
	}
	_ = url.QueryEscape
	_ = strings.Contains
	fmt.Printf("\n%d queries over the limit\n", fails)
	if fails > 0 {
		os.Exit(1)
	}
}
