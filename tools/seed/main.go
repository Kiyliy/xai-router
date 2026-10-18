// Command seed fills an xai-router database with synthetic request logs for
// load and performance testing. It writes through store.InsertLogs, so key and
// token counters and the hourly statistics stay consistent with the logs.
//
//	go run ./tools/seed -db ./data/xai-router.db -n 3000000 -days 30
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

func main() {
	dbPath := flag.String("db", "./data/xai-router.db", "database file (created if missing)")
	n := flag.Int("n", 3_000_000, "number of logs")
	days := flag.Int("days", 30, "spread logs over this many days, ending now")
	keys := flag.Int("keys", 10, "number of keys")
	tokens := flag.Int("tokens", 30, "number of client tokens")
	bigPct := flag.Float64("big", 1, "percent of logs with a 100-200 KB response body")
	batch := flag.Int("batch", 1000, "logs per transaction")
	events := flag.Int("events", 0, "only add this many audit and system events to an existing database (no logs)")
	flag.Parse()

	ctx := context.Background()
	db, err := store.Open(*dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if *events > 0 {
		seedEvents(ctx, db, *events, *days)
		return
	}
	if err := billing.SeedBuiltin(ctx, db); err != nil {
		log.Fatal(err)
	}
	var keyIDs []int64
	var keyNames []string
	for i := 0; i < *keys; i++ {
		k := &store.Key{Name: fmt.Sprintf("seed-key-%02d", i), Key: "xai-seed-" + secret.Random(32),
			BaseURL: "https://api.x.ai", Weight: 1, Enabled: true}
		if err := db.CreateKey(ctx, k); err != nil {
			log.Fatal(err)
		}
		keyIDs, keyNames = append(keyIDs, k.ID), append(keyNames, k.Name)
	}
	var tokIDs []int64
	var tokNames []string
	for i := 0; i < *tokens; i++ {
		raw := secret.NewToken("sk-xr-")
		t := &store.Token{Name: fmt.Sprintf("seed-token-%02d", i), Hash: secret.Hash(raw), Prefix: secret.Display(raw), Enabled: true}
		if err := db.CreateToken(ctx, t); err != nil {
			log.Fatal(err)
		}
		tokIDs, tokNames = append(tokIDs, t.ID), append(tokNames, t.Name)
	}
	models := []string{"grok-4.3", "grok-build-0.1", "grok-4.20-0309-non-reasoning", "grok-4.7", "grok-4.5",
		"grok-4.20-0309-reasoning", "grok-imagine-image", "grok-4.6"}
	paths := []string{"/v1/chat/completions", "/v1/chat/completions", "/v1/chat/completions", "/v1/responses",
		"/v1/messages", "/v1/images/generations", "/v1/models"}
	words := strings.Fields("the quick brown fox jumps over the lazy dog gateway router stream token model price " +
		"reasoning cache answer question hello world summary report data value request response log")
	text := func(r *rand.Rand, bytes int) string {
		var b strings.Builder
		for b.Len() < bytes {
			b.WriteString(words[r.IntN(len(words))])
			b.WriteByte(' ')
		}
		return b.String()
	}
	r := rand.New(rand.NewPCG(1, 2))
	end := time.Now().UnixMilli()
	start := end - int64(*days)*86400_000
	step := float64(end-start) / float64(*n)
	t0 := time.Now()
	buf := make([]*store.Log, 0, *batch)
	for i := 0; i < *n; i++ {
		created := start + int64(float64(i)*step)
		ki, ti := r.IntN(len(keyIDs)), r.IntN(len(tokIDs))
		l := &store.Log{
			RequestID: secret.Random(24), CreatedAt: created, Method: "POST", Path: paths[r.IntN(len(paths))],
			Model: models[r.IntN(len(models))], ClientIP: fmt.Sprintf("203.0.113.%d", r.IntN(250)+1),
			UserAgent: "OpenAI/Python 3.31.0", TokenID: tokIDs[ti], TokenName: tokNames[ti],
			KeyID: keyIDs[ki], KeyName: keyNames[ki], Stream: r.IntN(10) < 4, CostSource: "upstream",
			TTFBMs: int64(200 + r.IntN(2000)), HasBodies: true, ReqHeaders: `{"Content-Type":"application/json"}`,
			RespHeaders: `{"Content-Type":"application/json"}`,
		}
		l.LatencyMs = l.TTFBMs + int64(r.IntN(5000))
		switch p := r.IntN(100); {
		case p < 90:
			l.Status = 200
		case p < 93:
			l.Status, l.Error = 429, "rate limited (429): slow down"
		case p < 96:
			l.Status, l.Error = 400, "HTTP 400: invalid request"
		case p < 98:
			l.Status, l.Error = 500, "HTTP 500: internal"
		case p < 99:
			l.Status, l.Error = 499, "client closed request"
		default:
			l.Status, l.Error = 401, "invalid API key"
		}
		if l.Path == "/v1/models" {
			l.Method, l.Model = "GET", ""
		}
		l.ReqBody = `{"model":"` + l.Model + `","messages":[{"role":"user","content":"` + text(r, 500+r.IntN(2500)) + `"}]}`
		if l.Status == 200 {
			l.PromptTokens = int64(100 + r.IntN(3000))
			l.CachedTokens = l.PromptTokens / int64(1+r.IntN(4))
			l.CompletionTokens = int64(10 + r.IntN(800))
			l.ReasoningTokens = int64(r.IntN(600))
			l.TotalTokens = l.PromptTokens + l.CompletionTokens + l.ReasoningTokens
			l.CostTicks = l.PromptTokens*12500 + l.CompletionTokens*25000
			size := 500 + r.IntN(3500)
			if r.Float64()*100 < *bigPct {
				size = 100_000 + r.IntN(100_000)
			}
			l.RespText = text(r, size/2)
			l.RespBody = `{"choices":[{"message":{"content":"` + l.RespText + `"}}]}`
		} else {
			l.CostSource = "none"
			l.RespBody = `{"error":"` + l.Error + `"}`
		}
		attempts := 1
		if r.IntN(10) == 0 {
			attempts = 2 + r.IntN(2)
		}
		for a := 1; a <= attempts; a++ {
			act, st := "success", l.Status
			if a < attempts {
				act, st = "retry", 503
			} else if l.Status >= 400 {
				act = "return"
			}
			l.Attempts = append(l.Attempts, store.Attempt{Seq: a, KeyID: l.KeyID, KeyName: l.KeyName, Status: st,
				Action: act, TTFBMs: l.TTFBMs, LatencyMs: l.LatencyMs, StartedAt: created})
		}
		buf = append(buf, l)
		if len(buf) == *batch || i == *n-1 {
			if err := db.InsertLogs(ctx, buf); err != nil {
				log.Fatal(err)
			}
			buf = buf[:0]
			if (i+1)%100_000 == 0 || i == *n-1 {
				el := time.Since(t0)
				log.Printf("%d/%d logs (%.0f/s), db %.1f GB", i+1, *n, float64(i+1)/el.Seconds(), float64(db.Size())/1e9)
			}
		}
	}
}

// seedEvents adds n audit and system events spread over the last days,
// about the database's existing keys and tokens.
func seedEvents(ctx context.Context, db *store.DB, n, days int) {
	keys, _, err := db.ListKeys(ctx, store.KeyFilter{Limit: 1000})
	if err != nil {
		log.Fatal(err)
	}
	toks, _, err := db.ListTokens(ctx, "", 1000, 0)
	if err != nil {
		log.Fatal(err)
	}
	if len(keys) == 0 || len(toks) == 0 {
		log.Fatal("seed logs (keys and tokens) first")
	}
	r := rand.New(rand.NewPCG(3, 4))
	end := time.Now().UnixMilli()
	start := end - int64(days)*86400_000
	step := float64(end-start) / float64(n)
	buf := make([]*store.Event, 0, 1000)
	for i := 0; i < n; i++ {
		e := &store.Event{CreatedAt: start + int64(float64(i)*step), Level: store.LevelInfo, IP: "198.51.100.7",
			Actor: "password", ActorName: "admin", Kind: store.EventAudit}
		k, t := keys[r.IntN(len(keys))], toks[r.IntN(len(toks))]
		key := func() { e.TargetType, e.TargetID, e.TargetName = "key", fmt.Sprint(k.ID), k.Name }
		tok := func() { e.TargetType, e.TargetID, e.TargetName = "token", fmt.Sprint(t.ID), t.Name }
		switch p := r.IntN(100); {
		case p < 30:
			e.Action = "key.update"
			key()
			e.Detail = map[string]any{"changes": map[string]any{"priority": map[string]any{"before": r.IntN(5), "after": r.IntN(5)}}}
		case p < 45:
			e.Action = "token.quota_increase"
			tok()
			e.Detail = map[string]any{"amount_ticks": 10_000_000_000, "reason": "top-up"}
		case p < 55:
			e.Action = "key.test"
			key()
			e.Detail = map[string]any{"result": map[string]any{"ok": true, "status": "ok"}}
		case p < 60:
			e.Action, e.Level = "auth.login_failed", store.LevelWarn
			e.Actor, e.ActorName = "anonymous", ""
		case p < 80:
			e.Kind, e.Actor, e.ActorName, e.Action, e.Level = store.EventSystem, "system", "system", "key.cooldown", store.LevelWarn
			key()
			e.RequestID = secret.Random(24)
			e.Detail = map[string]any{"reason": "rate limited", "seconds": 60}
		case p < 85:
			e.Kind, e.Actor, e.ActorName, e.Action, e.Level = store.EventSystem, "system", "system", "key.auto_disable", store.LevelError
			key()
			e.RequestID = secret.Random(24)
			e.Detail = map[string]any{"reason": "invalid API key"}
		default:
			e.Kind, e.Actor, e.ActorName, e.Action = store.EventSystem, "system", "system", "model.sync"
			e.TargetType = "model"
			e.Detail = map[string]any{"scheduled": true, "result": map[string]any{"ok": true, "total": 40}}
		}
		buf = append(buf, e)
		if len(buf) == cap(buf) || i == n-1 {
			if err := db.AddEvents(ctx, buf); err != nil {
				log.Fatal(err)
			}
			buf = buf[:0]
		}
	}
	log.Printf("added %d events", n)
}
