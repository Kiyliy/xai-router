// Package keytest runs health checks on upstream keys.
package keytest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/store"
)

// APIKeyInfo is the subset of GET /v1/api-key the gateway shows.
type APIKeyInfo struct {
	Name           string   `json:"name"`
	TeamID         string   `json:"team_id"`
	ACLs           []string `json:"acls"`
	APIKeyBlocked  bool     `json:"api_key_blocked"`
	APIKeyDisabled bool     `json:"api_key_disabled"`
	TeamBlocked    bool     `json:"team_blocked"`
}

// Probe is the result of a tiny completion request.
type Probe struct {
	Model     string  `json:"model"`
	Status    int     `json:"status"`
	LatencyMs int64   `json:"latency_ms"`
	CostUSD   float64 `json:"cost_usd"`
	Error     string  `json:"error,omitempty"`
}

// Result is a key health check result (JSON shape is part of the admin API).
type Result struct {
	KeyID     int64       `json:"key_id"`
	KeyName   string      `json:"key_name"`
	OK        bool        `json:"ok"`
	Status    string      `json:"status"` // ok, invalid, blocked, no_credits, rate_limited, error
	Message   string      `json:"message"`
	LatencyMs int64       `json:"latency_ms"`
	APIKey    *APIKeyInfo `json:"api_key"`
	Models    []string    `json:"models"`
	Probe     *Probe      `json:"probe"`
	TestedAt  time.Time   `json:"tested_at"`
}

// Tester runs checks and applies their outcome to the pool.
type Tester struct {
	DB       *store.DB
	Pool     *pool.Pool
	Prices   *billing.Table
	Settings *config.Holder
	Client   *http.Client
	Log      *slog.Logger
}

// Test checks one key. It never returns an error; failures are in Result.
func (t *Tester) Test(ctx context.Context, k *store.Key, probe bool) Result {
	res := Result{KeyID: k.ID, KeyName: k.Name, TestedAt: time.Now().UTC(), Models: []string{}}
	base := strings.TrimRight(k.BaseURL, "/")
	if err := t.Settings.Get().UpstreamPolicy().CheckURL(base); err != nil {
		res.Status, res.Message = "error", "blocked by upstream policy: "+err.Error()
		return res
	}

	// 1. Key status (free).
	t0 := time.Now()
	status, body, err := t.do(ctx, http.MethodGet, base+"/v1/api-key", k.Key, nil)
	res.LatencyMs = time.Since(t0).Milliseconds()
	switch {
	case err != nil:
		res.Status, res.Message = "error", "network error: "+err.Error()
	case status == http.StatusOK:
		var info APIKeyInfo
		if json.Unmarshal(body, &info) == nil {
			res.APIKey = &info
		}
	default:
		res.Status, res.Message = classify(status, body)
	}
	if res.APIKey != nil {
		switch {
		case res.APIKey.TeamBlocked:
			res.Status, res.Message = "blocked", "team is blocked"
		case res.APIKey.APIKeyBlocked:
			res.Status, res.Message = "blocked", "API key is blocked"
		case res.APIKey.APIKeyDisabled:
			res.Status, res.Message = "blocked", "API key is disabled"
		}
	}

	// 2. Model list (free).
	if res.Status == "" {
		if st, b, err := t.do(ctx, http.MethodGet, base+"/v1/models", k.Key, nil); err == nil && st == http.StatusOK {
			var ml struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if json.Unmarshal(b, &ml) == nil {
				for _, m := range ml.Data {
					res.Models = append(res.Models, m.ID)
				}
				sort.Strings(res.Models)
			}
		}
	}

	// 3. Optional 1-token completion with the cheapest model.
	if res.Status == "" && probe {
		model := t.Settings.Get().KeyTestModel
		if model == "" {
			model = t.Prices.Cheapest()
		}
		if model == "" {
			model = "grok-build-0.1"
		}
		p := &Probe{Model: model}
		payload, _ := json.Marshal(map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": "Reply with: ok"}},
			"max_tokens": 1,
		})
		t1 := time.Now()
		st, b, err := t.do(ctx, http.MethodPost, base+"/v1/chat/completions", k.Key, payload)
		p.LatencyMs = time.Since(t1).Milliseconds()
		p.Status = st
		switch {
		case err != nil:
			p.Error = err.Error()
			res.Status, res.Message = "error", "probe failed: "+err.Error()
		case st == http.StatusOK:
			u, env := billing.ParseBody(b)
			ticks, _ := t.Prices.Cost(firstNonEmpty(env.Model, model), u, billing.Request{})
			p.CostUSD = billing.TicksToUSD(ticks)
		default:
			p.Error = truncate(string(b), 300)
			res.Status, res.Message = classify(st, b)
		}
		res.Probe = p
	}
	if res.Status == "" {
		res.Status, res.OK = "ok", true
		res.Message = "key is healthy"
	}
	t.apply(ctx, k, res)
	return res
}

// Verify is a cheap, free check used by the pool to confirm a key that
// looked dead during a request (GET /v1/api-key only).
func (t *Tester) Verify(ctx context.Context, k *store.Key) (bool, string) {
	if t.Settings.Get().UpstreamPolicy().CheckURL(k.BaseURL) != nil {
		return false, ""
	}
	st, body, err := t.do(ctx, http.MethodGet, strings.TrimRight(k.BaseURL, "/")+"/v1/api-key", k.Key, nil)
	if err != nil {
		return false, ""
	}
	if st == http.StatusOK {
		var info APIKeyInfo
		if json.Unmarshal(body, &info) == nil && (info.TeamBlocked || info.APIKeyBlocked || info.APIKeyDisabled) {
			return true, "verified: API key or team is blocked/disabled"
		}
		// The key itself is valid; check whether it can still pay.
		return t.verifyWithProbe(ctx, k)
	}
	status, msg := classify(st, body)
	switch status {
	case "invalid", "blocked", "no_credits":
		return true, "verified: " + msg
	}
	return false, ""
}

// verifyWithProbe follows a healthy GET /v1/api-key with a 1-token request,
// because an exhausted balance or spending limit only shows up on real
// requests. Used when the triggering error looked like a billing problem.
func (t *Tester) verifyWithProbe(ctx context.Context, k *store.Key) (bool, string) {
	model := t.Settings.Get().KeyTestModel
	if model == "" {
		model = t.Prices.Cheapest()
	}
	if model == "" {
		model = "grok-build-0.1"
	}
	payload, _ := json.Marshal(map[string]any{"model": model, "max_tokens": 1,
		"messages": []map[string]string{{"role": "user", "content": "ok"}}})
	st, body, err := t.do(ctx, http.MethodPost, strings.TrimRight(k.BaseURL, "/")+"/v1/chat/completions", k.Key, payload)
	if err != nil || st == http.StatusOK {
		return false, ""
	}
	switch status, msg := classify(st, body); status {
	case "invalid", "blocked", "no_credits":
		return true, "verified by probe: " + msg
	}
	return false, ""
}

// apply stores the result and updates pool state: dead keys are disabled,
// healthy keys that had been auto-disabled are re-enabled.
func (t *Tester) apply(ctx context.Context, k *store.Key, res Result) {
	b, _ := json.Marshal(res)
	if err := t.DB.SetKeyTest(ctx, k.ID, string(b)); err != nil {
		t.Log.Error("store key test", "key_id", k.ID, "err", err)
	}
	switch res.Status {
	case "invalid", "blocked", "no_credits":
		if err := t.Pool.Disable(ctx, k.ID, "health check: "+res.Message); err != nil {
			t.Log.Error("disable key", "key_id", k.ID, "err", err)
		}
	case "ok":
		if k.AutoDisabled || k.CooldownUntil > 0 {
			if err := t.Pool.ResetKey(ctx, k.ID); err != nil {
				t.Log.Error("reset key", "key_id", k.ID, "err", err)
			}
		}
	}
}

// TestMany checks keys concurrently (bounded) and returns results in order.
func (t *Tester) TestMany(ctx context.Context, keys []*store.Key, probe bool) []Result {
	return t.TestManyProgress(ctx, keys, probe, nil)
}

// TestManyProgress is TestMany reporting the number of finished keys.
func (t *Tester) TestManyProgress(ctx context.Context, keys []*store.Key, probe bool, progress func(done int)) []Result {
	var done atomic.Int64
	out := make([]Result, len(keys))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = t.Test(ctx, k, probe)
			if n := done.Add(1); progress != nil {
				progress(int(n))
			}
		}()
	}
	wg.Wait()
	return out
}

func classify(status int, body []byte) (string, string) {
	msg := extractMessage(body)
	lb := strings.ToLower(msg)
	label := fmt.Sprintf("HTTP %d: %s", status, msg)
	switch {
	case strings.Contains(lb, "credit") || strings.Contains(lb, "spending limit"):
		return "no_credits", label
	case strings.Contains(lb, "blocked") || strings.Contains(lb, "suspended") || strings.Contains(lb, "disabled"):
		return "blocked", label
	case status == http.StatusUnauthorized || strings.Contains(lb, "incorrect api key") || strings.Contains(lb, "invalid api key"):
		return "invalid", label
	case status == http.StatusTooManyRequests:
		return "rate_limited", label
	case status == http.StatusForbidden:
		return "blocked", label
	}
	return "error", label
}

func extractMessage(body []byte) string {
	var e struct {
		Error any    `json:"error"`
		Code  string `json:"code"`
		Msg   string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil {
		switch v := e.Error.(type) {
		case string:
			return truncate(v, 300)
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				return truncate(m, 300)
			}
		}
		if e.Msg != "" {
			return truncate(e.Msg, 300)
		}
	}
	return truncate(strings.TrimSpace(string(body)), 300)
}

func (t *Tester) do(ctx context.Context, method, url, key string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
