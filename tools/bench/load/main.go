// Command load runs a sustained load test against a gateway (acceptance
// P3/P4) using a built-in mock xAI upstream, so no real money is spent.
//
// It configures the gateway through the admin API (a key pointing at the
// mock, a client token), drives -c concurrent clients for -d with a mix of
// streaming and non-streaming chat, responses and messages requests, and
// reports:
//   - errors (any non-2xx or broken stream)
//   - gateway overhead: client latency minus the mock's own handling time
//   - gateway memory over time (from /api/v1/system/info)
//   - that every request was logged and that token spend, log costs and
//     key spend reconcile to the tick.
//
// With -purge-before it starts a background log purge after -purge-at.
//
//	go run ./tools/bench/load -base http://127.0.0.1:18090 -password ... -c 20 -d 10m
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	base        = flag.String("base", "http://127.0.0.1:18090", "gateway URL")
	pw          = flag.String("password", os.Getenv("XR_ADMIN_PASSWORD"), "admin password")
	conc        = flag.Int("c", 20, "concurrent clients")
	dur         = flag.Duration("d", 10*time.Minute, "test duration")
	mockAddr    = flag.String("mock", "127.0.0.1:0", "listen address of the mock upstream")
	mockHost    = flag.String("mock-host", "", "host the gateway uses to reach the mock (default: the listen address)")
	upLatency   = flag.Duration("upstream-latency", 50*time.Millisecond, "mock time to first byte")
	chunks      = flag.Int("chunks", 8, "chunks per streamed response")
	chunkGap    = flag.Duration("chunk-gap", 20*time.Millisecond, "time between streamed chunks")
	purgeBefore = flag.Duration("purge-before", 0, "if set, purge logs older than this (e.g. 600h) during the test")
	purgeAt     = flag.Duration("purge-at", time.Minute, "when to start the purge")
	purgeWait   = flag.Duration("purge-wait", 10*time.Minute, "how long to wait after the load for an unfinished purge before failing")
	p99Limit    = flag.Duration("p99", 20*time.Millisecond, "gateway overhead p99 limit")
	token       string
)

const costTicks = 1_234_567 // every mock response costs this much

// ---- mock upstream ----

func mock(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	time.Sleep(*upLatency)
	usage := fmt.Sprintf(`{"prompt_tokens":120,"completion_tokens":40,"total_tokens":160,"cost_in_usd_ticks":%d}`, costTicks)
	elapsed := func() string { return strconv.FormatInt(time.Since(t0).Microseconds(), 10) }
	switch {
	case r.URL.Path == "/v1/messages" && req.Stream:
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":%q,\"usage\":{\"input_tokens\":120,\"output_tokens\":0}}}\n\n", req.Model)
		for i := 0; i < *chunks; i++ {
			fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"word%d \"}}\n\n", i)
			fl.Flush()
			time.Sleep(*chunkGap)
		}
		fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":40,\"cost_in_usd_ticks\":%d}}\n\n", costTicks)
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\",\"mock_us\":%s}\n\n", elapsed())
	case req.Stream:
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < *chunks; i++ {
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"word%d \"}}]}\n\n", i)
			fl.Flush()
			time.Sleep(*chunkGap)
		}
		fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":%s}\n\n", usage)
		fmt.Fprintf(w, "data: {\"mock_us\":%s}\n\ndata: [DONE]\n\n", elapsed())
	case r.URL.Path == "/v1/responses":
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Mock-Us", elapsed())
		fmt.Fprintf(w, `{"id":"resp_%d","object":"response","model":%q,"output":[{"type":"message","content":[{"type":"output_text","text":"%s"}]}],"usage":{"input_tokens":120,"output_tokens":40,"total_tokens":160,"cost_in_usd_ticks":%d}}`,
			rand.Int64(), req.Model, strings.Repeat("lorem ipsum ", 100), costTicks)
	default:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Mock-Us", elapsed())
		fmt.Fprintf(w, `{"id":"c1","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"%s"}}],"usage":%s}`,
			req.Model, strings.Repeat("lorem ipsum ", 100), usage)
	}
}

// ---- admin helpers ----

func api(method, path string, body any, out any) int {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, *base+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if out != nil {
		_ = json.Unmarshal(b, out)
	}
	if resp.StatusCode >= 300 {
		log.Printf("%s %s: HTTP %d %s", method, path, resp.StatusCode, b)
	}
	return resp.StatusCode
}

type result struct {
	at       time.Time // when the request was sent
	overhead time.Duration
	total    time.Duration
	err      string
	kind     string
}

func main() {
	flag.Parse()
	ln, err := net.Listen("tcp", *mockAddr)
	if err != nil {
		log.Fatal(err)
	}
	go func() { log.Fatal(http.Serve(ln, http.HandlerFunc(mock))) }()
	host := *mockHost
	if host == "" {
		host = ln.Addr().String()
	}

	var lg struct{ Token string }
	api("POST", "/api/v1/auth/login", map[string]string{"password": *pw}, &lg)
	if lg.Token == "" {
		log.Fatal("login failed")
	}
	token = lg.Token
	api("PUT", "/api/v1/settings", map[string]any{"allow_private_upstreams": true, "allow_insecure_upstreams": true,
		"upstream_allowed_hosts": []string{}, "log_enabled": true, "log_bodies": true}, nil)
	stamp := time.Now().Format("150405")
	var key struct{ ID int64 }
	api("POST", "/api/v1/keys", map[string]any{"name": "load-" + stamp, "key": "xai-load-" + stamp, "base_url": "http://" + host,
		"priority": -100}, &key)
	var tok struct {
		ID    int64
		Token string
	}
	api("POST", "/api/v1/tokens", map[string]any{"name": "load-" + stamp, "quota_usd": 100000}, &tok)
	if tok.Token == "" || key.ID == 0 {
		log.Fatal("setup failed")
	}
	logsBefore := logCount()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: *conc * 2}}
	var sent atomic.Int64
	results := make(chan result, 100000)
	stop := time.Now().Add(*dur)
	var wg sync.WaitGroup
	for i := 0; i < *conc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(i), 99))
			for time.Now().Before(stop) {
				sent.Add(1)
				at := time.Now()
				res := one(client, tok.Token, r)
				res.at = at
				results <- res
			}
		}(i)
	}

	// Memory and purge monitoring.
	type memSample struct {
		at          time.Duration
		heap, sys   int64
		gor         int
		queue, logs int64
	}
	var mem []memSample
	var purgeJob struct {
		ID       int64
		State    string
		Progress int64
		Total    int64
		Result   map[string]any
	}
	start := time.Now()
	var purgeStarted bool
	var purgeFrom, purgeTo time.Time // window in which the purge job ran (as observed every 10 s)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for now := range t.C {
			var info struct {
				Heap  int64 `json:"go_heap_bytes"`
				Sys   int64 `json:"go_sys_bytes"`
				Gor   int   `json:"goroutines"`
				Queue int64 `json:"log_queue_length"`
				Logs  int64 `json:"log_count"`
			}
			api("GET", "/api/v1/system/info", nil, &info)
			mem = append(mem, memSample{now.Sub(start).Round(time.Second), info.Heap, info.Sys, info.Gor, info.Queue, info.Logs})
			if *purgeBefore > 0 && !purgeStarted && now.Sub(start) >= *purgeAt {
				purgeStarted = true
				before := time.Now().Add(-*purgeBefore).Unix()
				api("DELETE", fmt.Sprintf("/api/v1/logs?before=%d", before), nil, &purgeJob)
				purgeFrom = time.Now()
				log.Printf("purge job %d started (logs before %s)", purgeJob.ID, time.Unix(before, 0).Format(time.RFC3339))
			} else if purgeStarted && purgeJob.ID != 0 && purgeJob.State != "done" && purgeJob.State != "failed" {
				api("GET", fmt.Sprintf("/api/v1/jobs/%d", purgeJob.ID), nil, &purgeJob)
				log.Printf("purge: %s %d/%d", purgeJob.State, purgeJob.Progress, purgeJob.Total)
				if purgeJob.State != "running" {
					purgeTo = time.Now()
				}
			}
			if now.After(stop) {
				return
			}
		}
	}()

	go func() { wg.Wait(); close(results) }()
	var all []result
	kinds := map[string]int{}
	errs := map[string]int{}
	lastReport := time.Now()
	for res := range results {
		all = append(all, res)
		kinds[res.kind]++
		if res.err != "" {
			errs[res.err]++
		}
		if time.Since(lastReport) > 30*time.Second {
			lastReport = time.Now()
			log.Printf("%d requests, %d errors", len(all), len(all)-countOK(all))
		}
	}
	<-monitorDone
	elapsed := time.Since(start)

	var over []time.Duration
	for _, r := range all {
		if r.err == "" {
			over = append(over, r.overhead)
		}
	}
	sort.Slice(over, func(i, j int) bool { return over[i] < over[j] })
	p := func(q float64) time.Duration {
		if len(over) == 0 {
			return 0
		}
		return over[min(len(over)-1, int(float64(len(over))*q))]
	}

	// Wait (bounded) for the purge and the log writer to finish.
	waitUntil := time.Now().Add(*purgeWait)
	for purgeJob.ID != 0 && purgeJob.State == "running" && time.Now().Before(waitUntil) {
		time.Sleep(5 * time.Second)
		api("GET", fmt.Sprintf("/api/v1/jobs/%d", purgeJob.ID), nil, &purgeJob)
		log.Printf("waiting for purge: %d/%d", purgeJob.Progress, purgeJob.Total)
	}
	time.Sleep(3 * time.Second)
	var logged struct{ Total int64 }
	api("GET", fmt.Sprintf("/api/v1/logs?token_id=%d&page_size=1", tok.ID), nil, &logged)
	var tokNow struct {
		UsedUSD  float64 `json:"used_usd"`
		Requests int64
	}
	api("GET", fmt.Sprintf("/api/v1/tokens/%d", tok.ID), nil, &tokNow)
	var keyNow struct {
		CostUSD float64 `json:"cost_usd"`
	}
	api("GET", fmt.Sprintf("/api/v1/keys/%d", key.ID), nil, &keyNow)
	ok := countOK(all)
	wantTicks := int64(ok) * costTicks
	usedTicks := int64(tokNow.UsedUSD*1e10 + 0.5)
	keyTicks := int64(keyNow.CostUSD*1e10 + 0.5)

	fmt.Printf("\n## Load test: %d clients for %v against a mock upstream (TTFB %v, %d chunks × %v)\n\n",
		*conc, *dur, *upLatency, *chunks, *chunkGap)
	fmt.Printf("| metric | value |\n|---|---|\n")
	fmt.Printf("| requests sent | %d (%.0f/s) |\n", len(all), float64(len(all))/elapsed.Seconds())
	fmt.Printf("| mix | %v |\n", kinds)
	fmt.Printf("| errors | %d %v |\n", len(all)-ok, errs)
	fmt.Printf("| gateway overhead p50 / p95 / p99 / max | %v / %v / %v / %v |\n", p(0.50).Round(time.Microsecond),
		p(0.95).Round(time.Microsecond), p(0.99).Round(time.Microsecond), p(1).Round(time.Microsecond))
	fmt.Printf("| logs written for the test token | %d (requests %d) |\n", logged.Total, len(all))
	fmt.Printf("| token spend / expected / key spend (ticks) | %d / %d / %d |\n", usedTicks, wantTicks, keyTicks)
	fmt.Printf("| total logs before → after | %d → %d |\n", logsBefore, logCount())
	if purgeJob.ID != 0 {
		fmt.Printf("| background purge | %s, %d rows deleted, result %v |\n", purgeJob.State, purgeJob.Progress, purgeJob.Result)
	}
	fmt.Printf("\n### Gateway memory\n\n| t | heap MB | sys MB | goroutines | log queue | logs |\n|---|---|---|---|---|---|\n")
	for _, m := range mem {
		fmt.Printf("| %v | %.1f | %.1f | %d | %d | %d |\n", m.at, float64(m.heap)/1e6, float64(m.sys)/1e6, m.gor, m.queue, m.logs)
	}
	// Overhead of the requests sent while the purge ran.
	purgeOK := true
	if purgeJob.ID != 0 {
		if purgeTo.IsZero() {
			purgeTo = start.Add(*dur)
		}
		var po []time.Duration
		for _, r := range all {
			if r.err == "" && !r.at.Before(purgeFrom) && r.at.Before(purgeTo) {
				po = append(po, r.overhead)
			}
		}
		sort.Slice(po, func(i, j int) bool { return po[i] < po[j] })
		q := func(f float64) time.Duration {
			if len(po) == 0 {
				return 0
			}
			return po[min(len(po)-1, int(float64(len(po))*f))]
		}
		fmt.Printf("\n### While the purge ran (%v, %d requests)\n\n| p50 | p95 | p99 | max |\n|---|---|---|---|\n| %v | %v | %v | %v |\n",
			purgeTo.Sub(purgeFrom).Round(time.Second), len(po), q(0.5).Round(time.Microsecond), q(0.95).Round(time.Microsecond),
			q(0.99).Round(time.Microsecond), q(1).Round(time.Microsecond))
		purgeOK = purgeJob.State == "done" && q(0.99) < *p99Limit
		if purgeJob.State == "running" {
			fmt.Printf("\npurge still running after the load and %v of waiting: %d/%d\n", *purgeWait, purgeJob.Progress, purgeJob.Total)
		}
	}
	pass := len(all) == ok && p(0.99) < *p99Limit && logged.Total == int64(len(all)) && usedTicks == wantTicks && keyTicks == wantTicks && purgeOK
	fmt.Printf("\nRESULT: %s\n", map[bool]string{true: "PASS", false: "FAIL"}[pass])
	if !pass {
		os.Exit(1)
	}
}

func countOK(all []result) int {
	n := 0
	for _, r := range all {
		if r.err == "" {
			n++
		}
	}
	return n
}

func logCount() int64 {
	var info struct {
		Logs int64 `json:"log_count"`
	}
	api("GET", "/api/v1/system/info", nil, &info)
	return info.Logs
}

var mockUsRe = []byte(`"mock_us":`)

// one sends one request and measures the gateway's added latency.
func one(c *http.Client, tok string, r *rand.Rand) result {
	var path, body, kind string
	switch x := r.IntN(100); {
	case x < 40:
		path, kind = "/v1/chat/completions", "chat"
		body = `{"model":"grok-4.3","messages":[{"role":"user","content":"` + strings.Repeat("hello ", 50+r.IntN(300)) + `"}]}`
	case x < 75:
		path, kind = "/v1/chat/completions", "chat-stream"
		body = `{"model":"grok-4.3","stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("hello ", 50+r.IntN(300)) + `"}]}`
	case x < 90:
		path, kind = "/v1/responses", "responses"
		body = `{"model":"grok-4.3","input":"` + strings.Repeat("hello ", 50+r.IntN(300)) + `"}`
	default:
		path, kind = "/v1/messages", "messages-stream"
		body = `{"model":"grok-4.3","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`
	}
	req, _ := http.NewRequest("POST", *base+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return result{err: "transport: " + err.Error(), kind: kind}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return result{err: fmt.Sprintf("HTTP %d %.80s", resp.StatusCode, b), kind: kind}
	}
	var mockUs int64
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		done := false
		for sc.Scan() {
			line := sc.Bytes()
			if i := bytes.Index(line, mockUsRe); i >= 0 {
				v := line[i+len(mockUsRe):]
				v = v[:bytes.IndexAny(v, "},")]
				mockUs, _ = strconv.ParseInt(string(v), 10, 64)
			}
			if bytes.Contains(line, []byte("[DONE]")) || bytes.Contains(line, []byte("message_stop")) {
				done = true
			}
		}
		if !done || sc.Err() != nil {
			return result{err: "stream incomplete", kind: kind}
		}
	} else {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return result{err: "body: " + err.Error(), kind: kind}
		}
		mockUs, _ = strconv.ParseInt(resp.Header.Get("X-Mock-Us"), 10, 64)
	}
	total := time.Since(t0)
	if mockUs == 0 {
		return result{err: "no mock timing", kind: kind}
	}
	return result{total: total, overhead: total - time.Duration(mockUs)*time.Microsecond, kind: kind}
}
