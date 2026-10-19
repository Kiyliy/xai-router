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
	"math"
	"math/rand/v2"
	"slices"
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
	table := billing.NewTable()
	if err := table.Reload(ctx, db); err != nil {
		log.Fatal(err)
	}
	// Realistic traffic: each endpoint with the models it serves, its streaming
	// share and the shape of its usage. Costs come from the price table, the
	// same way the gateway prices a request.
	type endpoint struct {
		path, method string
		weight       int
		models       []string
		stream       float64 // share of streamed requests
		kind         string  // text, image, video, list
	}
	endpoints := []endpoint{
		{"/v1/chat/completions", "POST", 50, []string{"grok-4.3", "grok-4.3", "grok-4.20-0309-non-reasoning", "grok-4.20-0309-reasoning", "grok-4.6", "grok-4.7", "grok-build-0.1"}, 0.45, "text"},
		{"/v1/responses", "POST", 14, []string{"grok-4.3", "grok-4.20-0309-reasoning", "grok-4.6"}, 0.3, "text"},
		{"/v1/messages", "POST", 10, []string{"grok-4.3", "grok-4.20-0309-non-reasoning", "grok-4.5"}, 0.5, "text"},
		{"/v1/images/generations", "POST", 6, []string{"grok-imagine-image", "grok-imagine-image-2.0"}, 0, "image"},
		{"/v1/videos/generations", "POST", 2, []string{"grok-imagine-video-1.5", "grok-imagine-video-1.5-lite"}, 0, "video"},
		{"/v1/models", "GET", 8, nil, 0, "list"},
	}
	totalW := 0
	for _, e := range endpoints {
		totalW += e.weight
	}
	pickEndpoint := func(r *rand.Rand) endpoint {
		x := r.IntN(totalW)
		for _, e := range endpoints {
			if x -= e.weight; x < 0 {
				return e
			}
		}
		return endpoints[0]
	}
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
	// A daily curve (busy afternoons, quiet nights) with some noise; the times
	// are drawn from it and sorted, so the newest log is a few seconds old.
	times := make([]int64, *n)
	for i := range times {
		for {
			t := start + r.Int64N(end-start)
			h := float64(time.UnixMilli(t).Hour()) + float64(time.UnixMilli(t).Minute())/60
			w := 0.35 + 0.65*(0.5+0.5*math.Sin((h-9)/24*2*math.Pi))
			if r.Float64() < w {
				times[i] = t
				break
			}
		}
	}
	slices.Sort(times)
	times[len(times)-1] = end - 3000
	t0 := time.Now()
	buf := make([]*store.Log, 0, *batch)
	for i := 0; i < *n; i++ {
		created := times[i]
		ki, ti := r.IntN(len(keyIDs)), r.IntN(len(tokIDs))
		ep := pickEndpoint(r)
		l := &store.Log{
			RequestID: secret.Random(24), CreatedAt: created, Method: ep.method, Path: ep.path,
			ClientIP:  fmt.Sprintf("203.0.113.%d", r.IntN(250)+1),
			UserAgent: "OpenAI/Python 3.31.0", TokenID: tokIDs[ti], TokenName: tokNames[ti],
			KeyID: keyIDs[ki], KeyName: keyNames[ki], CostSource: "none", HasBodies: true,
			ReqHeaders: `{"Content-Type":"application/json"}`, RespHeaders: `{"Content-Type":"application/json"}`,
		}
		if len(ep.models) > 0 {
			l.Model = ep.models[r.IntN(len(ep.models))]
		}
		l.Stream = r.Float64() < ep.stream
		switch ep.kind {
		case "list":
			l.TTFBMs = int64(80 + r.IntN(200))
			l.LatencyMs = l.TTFBMs + int64(r.IntN(40))
		case "image":
			l.TTFBMs = int64(4000 + r.IntN(8000))
			l.LatencyMs = l.TTFBMs + int64(r.IntN(500))
		case "video":
			l.TTFBMs = int64(300 + r.IntN(600))
			l.LatencyMs = l.TTFBMs + int64(r.IntN(200))
		default:
			l.TTFBMs = int64(300 + r.IntN(1500))
			l.LatencyMs = l.TTFBMs + int64(r.IntN(6000))
		}
		switch p := r.IntN(100); {
		case p < 92 || ep.kind == "list":
			l.Status = 200
		case p < 95:
			l.Status, l.Error = 429, "rate limited (429): slow down"
		case p < 97:
			l.Status, l.Error = 400, "HTTP 400: invalid request"
		case p < 99:
			l.Status, l.Error = 500, "HTTP 500: internal"
		default:
			l.Status, l.Error = 499, "client closed request"
		}
		if ep.kind == "video" && l.Status == 200 {
			l.Status = 202
		}
		var u billing.Usage
		var req billing.Request
		ok := l.Status == 200 || l.Status == 202
		switch {
		case ep.kind == "list":
			l.RespBody = `{"object":"list","data":[{"id":"grok-4.3","object":"model"}]}`
		case !ok:
			l.RespBody = `{"error":"` + l.Error + `"}`
		case ep.kind == "image":
			u = billing.Usage{Found: true, Images: int64(1 + r.IntN(3))}
			req.Resolution = []string{"1k", "1k", "2k"}[r.IntN(3)]
			l.ReqBody = `{"model":"` + l.Model + `","prompt":"` + text(r, 120) + `","n":` + fmt.Sprint(u.Images) + `}`
			l.RespBody = `{"data":[{"url":"https://example.invalid/image.png"}]}`
		case ep.kind == "video":
			u = billing.Usage{Found: true, VideoSeconds: float64(4 + r.IntN(7))}
			req.Resolution = []string{"480p", "480p", "720p"}[r.IntN(3)]
			l.ReqBody = `{"model":"` + l.Model + `","prompt":"` + text(r, 120) + `"}`
			l.RespBody = `{"request_id":"` + secret.Random(16) + `"}`
		default:
			u = billing.Usage{Found: true, PromptTokens: int64(200 + r.IntN(4000))}
			if r.IntN(3) > 0 { // most repeated prompts hit the cache
				u.CachedTokens = u.PromptTokens * int64(40+r.IntN(50)) / 100
			}
			u.CompletionTokens = int64(20 + r.IntN(900))
			if strings.Contains(l.Model, "reasoning") || l.Model == "grok-4.6" || l.Model == "grok-4.7" {
				u.ReasoningTokens = int64(50 + r.IntN(1200))
			}
			if ep.path == "/v1/messages" {
				zero := int64(0)
				u.CacheWriteTokens = &zero
			}
			u.TotalTokens = u.PromptTokens + u.CompletionTokens + u.ReasoningTokens
			l.ReqBody = `{"model":"` + l.Model + `","messages":[{"role":"user","content":"` + text(r, 500+r.IntN(2500)) + `"}]}`
			size := 500 + r.IntN(3500)
			if r.Float64()*100 < *bigPct {
				size = 100_000 + r.IntN(100_000)
			}
			l.RespText = text(r, size/2)
			l.RespBody = `{"choices":[{"message":{"content":"` + l.RespText + `"}}]}`
		}
		if u.Found {
			l.PromptTokens, l.CompletionTokens, l.CachedTokens = u.PromptTokens, u.CompletionTokens, u.CachedTokens
			l.ReasoningTokens, l.TotalTokens, l.Images, l.VideoSeconds = u.ReasoningTokens, u.TotalTokens, u.Images, u.VideoSeconds
			l.CacheWriteTokens = u.CacheWriteTokens
			l.CostTicks, l.CostSource = table.Cost(l.Model, u, req)
			l.PriceSnapshot = table.PriceSnapshot(l.Model, u, req)
		}
		attempts := 1
		if r.IntN(12) == 0 && ep.kind != "list" {
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
