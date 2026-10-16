package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// sendChat posts a chat request with mock controls and returns the status.
func (e *env) sendChat(token string, maxTokens int, delayMs int, costTicks int64) int {
	b, _ := json.Marshal(map[string]any{"model": "grok-4.3", "max_tokens": maxTokens,
		"messages": []map[string]string{{"role": "user", "content": "hi"}}})
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Grok-Mock-Delay-Ms", strconv.Itoa(delayMs))
	req.Header.Set("X-Grok-Mock-Cost", strconv.FormatInt(costTicks, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Error(err)
		return 0
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func (e *env) tokenUsed(id int64) float64 {
	var tok struct {
		UsedUSD float64 `json:"used_usd"`
	}
	e.api("GET", fmt.Sprintf("/api/v1/tokens/%d", id), nil, 200, &tok)
	return tok.UsedUSD
}

// waitUsed waits until all logs are written (the writer flushes every
// 500ms) and returns the token's recorded spend.
func (e *env) waitUsed(id int64) float64 {
	time.Sleep(1200 * time.Millisecond)
	return e.tokenUsed(id)
}

// The reservation for grok-4.3 with max_tokens=100 is about $0.0106
// (prompt bytes/2 at $1.25/M plus 100 + 4096 output tokens at $2.50/M), so
// at most four requests fit in $0.05 at any moment.
func TestQuotaStrictUnderConcurrency(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var tok struct {
		ID    int64
		Token string
	}
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "q", "quota_usd": 0.05}, 201, &tok)
	const n = 40
	var ok, rejected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each request really costs $0.01 and is slow, so they are all in
			// flight at the same time.
			switch e.sendChat(tok.Token, 100, 300, 100_000_000) {
			case 200:
				ok.Add(1)
			case 429:
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok.Load() == 0 || ok.Load() > 4 || ok.Load()+rejected.Load() != n {
		t.Fatalf("admitted %d, rejected %d of %d (at most 4 fit in $0.05)", ok.Load(), rejected.Load(), n)
	}
	if used := e.waitUsed(tok.ID); used != float64(ok.Load())*0.01 {
		t.Fatalf("used $%v after %d admitted requests", used, ok.Load())
	}
	// Keep sending until the quota is spent: spend never exceeds it.
	for i := 0; i < 20; i++ {
		e.sendChat(tok.Token, 100, 0, 100_000_000)
	}
	used := e.waitUsed(tok.ID)
	if used > 0.05 {
		t.Fatalf("overspent: used $%v of $0.05", used)
	}
	if used < 0.03 {
		t.Fatalf("quota under-used: $%v", used)
	}
}

// Reset usage frees the quota immediately.
func TestQuotaReset(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var tok struct {
		ID    int64
		Token string
	}
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "q", "quota_usd": 0.02}, 201, &tok)
	if st := e.sendChat(tok.Token, 100, 0, 150_000_000); st != 200 {
		t.Fatalf("first request: %d", st)
	}
	if st := e.sendChat(tok.Token, 100, 0, 1); st != 429 {
		t.Fatalf("over-quota request: %d", st)
	}
	e.waitUsed(tok.ID)
	e.api("PATCH", fmt.Sprintf("/api/v1/tokens/%d", tok.ID), map[string]any{"reset_usage": true}, 200, nil)
	if st := e.sendChat(tok.Token, 100, 0, 1); st != 200 {
		t.Fatalf("after reset: %d", st)
	}
}
