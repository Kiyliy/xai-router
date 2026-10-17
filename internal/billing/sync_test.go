package billing

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kiyliy/xai-router/internal/store"
)

func openDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fakeXAI serves the model-list endpoints from a map of path -> body.
type fakeXAI struct {
	mu     sync.Mutex
	bodies map[string]string
	status map[string]int
	auth   []string
}

func (f *fakeXAI) set(path, body string) {
	f.mu.Lock()
	f.bodies[path] = body
	f.mu.Unlock()
}

func (f *fakeXAI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	body, ok := f.bodies[r.URL.Path]
	status := f.status[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if status == 0 {
		status = 200
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

const (
	langV1 = `{"models":[
{"id":"grok-4.5","aliases":["grok-4.5-latest","grok-build-latest"],"context_length":500000,
 "prompt_text_token_price":20000,"cached_prompt_text_token_price":3000,"prompt_image_token_price":20000,
 "completion_text_token_price":60000,"prompt_text_token_price_long_context":40000,
 "cached_prompt_text_token_price_long_context":6000,"completion_text_token_price_long_context":120000,
 "long_context_threshold":200000},
{"id":"grok-new","prompt_text_token_price":1000,"completion_text_token_price":2000},
{"id":"grok-4.3","prompt_text_token_price":1,"completion_text_token_price":1}]}`
	imgV1   = `{"models":[{"id":"grok-imagine-image-2.0","image_price":400000000,"pricing":[{"quality":"low","resolution":"1k","price_per_image":400000000}]}]}`
	vidV1   = `{"models":[{"id":"grok-imagine-video","aliases":[]},{"id":"vid-unpriced"},{"id":"vid-manual"}]}`
	embV1   = `{"models":[{"id":"emb-1","aliases":["emb"]}]}`
	modelV1 = `{"object":"list","data":[{"id":"grok-new","context_length":131072},{"id":"grok-4.5","context_length":1}]}`
)

func newFake() *fakeXAI {
	return &fakeXAI{bodies: map[string]string{
		"/v1/language-models":         langV1,
		"/v1/image-generation-models": imgV1,
		"/v1/video-generation-models": vidV1,
		"/v1/embedding-models":        embV1,
		"/v1/models":                  modelV1,
	}, status: map[string]int{}}
}

func TestSyncAddsUpdatesAndPreserves(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	// Hand-edited models survive a sync.
	manual := &store.Model{ID: "grok-4.3", Kind: "language", Enabled: true, Source: "manual", InputTicks: 77, OutputTicks: 88}
	if err := db.UpsertModel(ctx, manual); err != nil {
		t.Fatal(err)
	}
	mv := &store.Model{ID: "vid-manual", Kind: "video", Enabled: true, Source: "manual", VideoTicksPerSec: 12345}
	if err := db.UpsertModel(ctx, mv); err != nil {
		t.Fatal(err)
	}
	// A disabled model stays disabled.
	g45, _ := db.GetModel(ctx, "grok-4.5")
	g45.Enabled = false
	if err := db.UpsertModel(ctx, g45); err != nil {
		t.Fatal(err)
	}

	fake := newFake()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	tbl := NewTable()
	s := &Syncer{DB: db, Table: tbl, Client: srv.Client()}

	res := s.Sync(ctx, srv.URL, "xai-key", false)
	if !res.OK || res.Error != "" || res.Total != 8 {
		t.Fatalf("sync: %+v", res)
	}
	if strings.Join(res.Added, ",") != "emb-1,grok-new,vid-unpriced" {
		t.Fatalf("added %v", res.Added)
	}
	if strings.Join(res.Updated, ",") != "grok-4.5,grok-imagine-image-2.0,grok-imagine-video" {
		t.Fatalf("updated %v", res.Updated)
	}
	for _, a := range fake.auth {
		if a != "Bearer xai-key" {
			t.Fatalf("auth header %q", a)
		}
	}
	got, _ := db.GetModel(ctx, "grok-new")
	if got.ContextLength != 131072 || got.Source != "sync" || !got.Enabled || got.Aliases == nil || got.SyncedAt == 0 {
		t.Fatalf("grok-new: %+v", got)
	}
	if got, _ = db.GetModel(ctx, "grok-4.5"); got.Enabled || got.CachedTicks != 3000 || got.ContextLength != 500000 {
		t.Fatalf("grok-4.5: %+v", got)
	}
	if got, _ = db.GetModel(ctx, "grok-4.3"); got.Source != "manual" || got.InputTicks != 77 {
		t.Fatalf("manual model overwritten: %+v", got)
	}
	if got, _ = db.GetModel(ctx, "grok-imagine-video"); got.VideoTicksPerSec != 500_000_000 || len(got.VideoTiers) != 2 || got.InputVideoTicksPerSec != 100_000_000 {
		t.Fatalf("video price: %+v", got)
	}
	if got, _ = db.GetModel(ctx, "vid-unpriced"); got.VideoTicksPerSec != 0 || got.Kind != "video" {
		t.Fatalf("unpriced video: %+v", got)
	}
	if got, _ = db.GetModel(ctx, "grok-imagine-image-2.0"); len(got.ImageTiers) != 1 || got.ImageTicks != 400000000 {
		t.Fatalf("image: %+v", got)
	}
	if tbl.Lookup("emb") == nil || tbl.Lookup("grok-new") == nil || len(tbl.All()) == 0 {
		t.Fatal("table not reloaded")
	}

	// Same data again: nothing changes.
	res = s.Sync(ctx, srv.URL, "xai-key", false)
	if !res.OK || len(res.Added) != 0 || len(res.Updated) != 0 {
		t.Fatalf("idempotent sync: %+v", res)
	}

	// A price change, and overwriting manual models.
	fake.set("/v1/language-models", strings.Replace(langV1, `"prompt_text_token_price":1000`, `"prompt_text_token_price":1500`, 1))
	res = s.Sync(ctx, srv.URL, "xai-key", true)
	if !res.OK || strings.Join(res.Updated, ",") != "grok-4.3,grok-new,vid-manual" {
		t.Fatalf("overwrite sync: %+v", res)
	}
	if got, _ = db.GetModel(ctx, "grok-4.3"); got.Source != "sync" || got.InputTicks != 1 {
		t.Fatalf("manual model not overwritten: %+v", got)
	}
	if got, _ = db.GetModel(ctx, "vid-manual"); got.VideoTicksPerSec != 12345 {
		t.Fatalf("manual video price lost: %+v", got)
	}
}

func TestSyncErrors(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		mutate func(f *fakeXAI)
		want   string
	}{
		{"http error", func(f *fakeXAI) {
			f.status["/v1/embedding-models"] = 500
			f.bodies["/v1/embedding-models"] = strings.Repeat("e", 400)
		}, "HTTP 500: eee"},
		{"bad json", func(f *fakeXAI) { f.bodies["/v1/models"] = "{" }, "unexpected end of JSON"},
		{"missing endpoint", func(f *fakeXAI) { delete(f.bodies, "/v1/language-models") }, "HTTP 404"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			c.mutate(f)
			srv := httptest.NewServer(f)
			defer srv.Close()
			res := (&Syncer{DB: openDB(t), Table: NewTable(), Client: srv.Client()}).Sync(ctx, srv.URL, "k", false)
			if res.OK || !strings.Contains(res.Error, c.want) {
				t.Fatalf("got %+v", res)
			}
		})
	}
	t.Run("network", func(t *testing.T) {
		srv := httptest.NewServer(newFake())
		url := srv.URL
		srv.Close()
		res := (&Syncer{DB: openDB(t), Table: NewTable(), Client: &http.Client{}}).Sync(ctx, url, "k", false)
		if res.OK || res.Error == "" {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("bad url", func(t *testing.T) {
		res := (&Syncer{DB: openDB(t), Table: NewTable(), Client: &http.Client{}}).Sync(ctx, "http://bad host", "k", false)
		if res.OK || res.Error == "" {
			t.Fatalf("got %+v", res)
		}
	})
	t.Run("database closed", func(t *testing.T) {
		srv := httptest.NewServer(newFake())
		defer srv.Close()
		db := openDB(t)
		db.Close()
		res := (&Syncer{DB: db, Table: NewTable(), Client: srv.Client()}).Sync(ctx, srv.URL, "k", false)
		if res.OK || res.Error == "" {
			t.Fatalf("got %+v", res)
		}
		if err := NewTable().Reload(ctx, db); err == nil {
			t.Fatal("reload from a closed database should fail")
		}
		if err := SeedBuiltin(ctx, db); err == nil {
			t.Fatal("seed into a closed database should fail")
		}
	})
}

func TestSeedBuiltinIdempotent(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	m, _ := db.GetModel(ctx, "grok-4.5")
	m.InputTicks, m.Source = 1, "manual"
	if err := db.UpsertModel(ctx, m); err != nil {
		t.Fatal(err)
	}
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	all, _ := db.ListModels(ctx, "")
	if len(all) != len(BuiltinModels()) {
		t.Fatalf("%d models after reseed", len(all))
	}
	if m, _ = db.GetModel(ctx, "grok-4.5"); m.InputTicks != 1 {
		t.Fatal("seed overwrote an edited model")
	}
	tbl := NewTable()
	if err := tbl.Reload(ctx, db); err != nil || tbl.Lookup("grok-4.5").InputTicks != 1 {
		t.Fatalf("reload: %v", err)
	}
}

func TestSamePrices(t *testing.T) {
	base := func() *store.Model {
		return &store.Model{Kind: "image", ImageTicks: 5, Aliases: []string{"a", "b"},
			ImageTiers: []store.ImageTier{{Quality: "low", Resolution: "1k", Ticks: 1}}}
	}
	if !samePrices(base(), base()) {
		t.Fatal("identical models differ")
	}
	for name, mut := range map[string]func(m *store.Model){
		"kind":      func(m *store.Model) { m.Kind = "video" },
		"price":     func(m *store.Model) { m.ImageTicks = 6 },
		"alias":     func(m *store.Model) { m.Aliases[1] = "c" },
		"alias len": func(m *store.Model) { m.Aliases = m.Aliases[:1] },
		"tier":      func(m *store.Model) { m.ImageTiers[0].Ticks = 2 },
	} {
		m := base()
		mut(m)
		if samePrices(base(), m) {
			t.Errorf("%s: change not detected", name)
		}
	}
	if truncate("abc", 5) != "abc" || truncate("abcdef", 3) != "abc…" {
		t.Fatal("truncate")
	}
}

func TestWSCost(t *testing.T) {
	cases := []struct {
		u    WSUsage
		want int64
	}{
		{WSUsage{Path: "/v1/realtime", DurationMs: 60_000, TextInputs: 2}, RealtimeTicksPerMinute + 2*RealtimeTicksPerText},
		{WSUsage{Path: "/v1/realtime/x", DurationMs: 30_000}, RealtimeTicksPerMinute / 2},
		{WSUsage{Path: "/v1/tts", TTSChars: 10, DurationMs: 99}, 10 * TTSTicksPerChar},
		{WSUsage{Path: "/v1/stt", AudioSecond: 3600, DurationMs: 1}, STTStreamTicksPerHour},
		{WSUsage{Path: "/v1/stt", DurationMs: 1_800_000}, STTStreamTicksPerHour / 2},
		{WSUsage{Path: "/v1/other", DurationMs: 1_000_000}, 0},
	}
	for _, c := range cases {
		if got := WSCost(c.u); got != c.want {
			t.Errorf("%+v: %d want %d", c.u, got, c.want)
		}
	}
}

func TestExtractTextShapes(t *testing.T) {
	cases := []struct{ body, text, reasoning string }{
		{`{"content":[{"type":"thinking","thinking":"hmm"},{"type":"text","text":"Hi"},{"type":"tool_use"}]}`, "Hi", "hmm"},
		{`{"choices":[{"message":{"content":[{"type":"text"}]}}]}`, "", ""},
		{`not json`, "", ""},
		{`{"output":[{"type":"message","content":[{"type":"refusal","text":"no"},{"type":"output_text","text":"ok"}]}]}`, "ok", ""},
	}
	for _, c := range cases {
		text, reasoning := ExtractText([]byte(c.body))
		if text != c.text || reasoning != c.reasoning {
			t.Errorf("%s: %q %q", c.body, text, reasoning)
		}
	}
}

func TestUsageMergeAndFinalize(t *testing.T) {
	u := Usage{PromptTokens: 1, ServiceTier: "default"}
	u.Merge(Usage{PromptTokens: 9}) // not Found: ignored
	if u.PromptTokens != 1 {
		t.Fatal("merge of empty usage changed values")
	}
	u.Merge(Usage{Found: true, CompletionTokens: 3, CostTicks: 5, HasCost: true, VideoSeconds: 2, ServiceTier: "priority",
		Tools: Tools{WebSearch: 1}, Images: 2, CachedTokens: 1, ReasoningTokens: 4, ImageTokens: 1, TotalTokens: 0})
	if !u.Found || u.PromptTokens != 1 || u.CompletionTokens != 3 || u.CostTicks != 5 || !u.HasCost ||
		u.VideoSeconds != 2 || u.ServiceTier != "priority" || u.Tools.WebSearch != 1 || u.Images != 2 {
		t.Fatalf("merge: %+v", u)
	}
	u.Finalize()
	if u.TotalTokens != 1+3+4 {
		t.Fatalf("total %d", u.TotalTokens)
	}
	u.TotalTokens = 100
	u.Finalize()
	if u.TotalTokens != 100 {
		t.Fatal("reported total overwritten")
	}
}

func TestCostPaths(t *testing.T) {
	tbl := table()
	if tbl.Lookup("") != nil {
		t.Fatal("empty name must not match")
	}
	if c, src := tbl.Cost("grok-imagine-video-1.5", Usage{Found: true, VideoSeconds: 2.5}, Request{}); c != int64(2.5*float64(USDToTicks(0.08))) || src != "computed" {
		t.Fatalf("video: %d %s", c, src)
	}
	// Tiered image prices: defaults per generation/edit, explicit tier, unknown tier.
	for _, c := range []struct {
		req  Request
		want int64
	}{
		{Request{}, 400_000_000},
		{Request{Quality: "auto", Edit: true}, 600_000_000},
		{Request{Quality: "MEDIUM", Resolution: "2K"}, 800_000_000},
		{Request{Quality: "ultra"}, 800_000_000}, // unknown tier: the highest one, as reserved
	} {
		if got, _ := tbl.Cost("grok-imagine-image-2.0", Usage{Found: true, Images: 1}, c.req); got != c.want {
			t.Errorf("image %+v: %d want %d", c.req, got, c.want)
		}
	}
	// No usage at all prices to zero.
	if c, src := tbl.Cost("grok-4.5", Usage{}, Request{}); c != 0 || src != "none" {
		t.Fatalf("empty usage: %d %s", c, src)
	}
	// Long context with image tokens follows the long input rate.
	m := tbl.Lookup("grok-4.5")
	u := Usage{Found: true, PromptTokens: 300_000, ImageTokens: 1000, CompletionTokens: 10}
	got, _ := tbl.Cost("grok-4.5", u, Request{})
	want := (300_000-1000)*m.LongInputTicks + 1000*m.LongInputTicks + 10*m.LongOutputTicks
	if got != want {
		t.Fatalf("long context: %d want %d", got, want)
	}
}

func TestStreamStateEdgeCases(t *testing.T) {
	s := &StreamState{MaxText: 4}
	for _, d := range []string{
		``, `[DONE]`, `{bad`,
		`{"type":"response.output_text.delta","delta":{"not":"string"}}`,
		`{"type":"response.reasoning_text.delta","delta":"why"}`,
		`{"id":"c","service_tier":"priority","choices":[{"delta":{"content":"héllo"}}]}`,
	} {
		s.AddEvent("", []byte(d))
	}
	if s.Reasoning.String() != "why" || s.Text.String() != "hél" || !s.Truncated || s.Usage.ServiceTier != "priority" {
		t.Fatalf("state: text=%q reasoning=%q trunc=%v tier=%q", s.Text.String(), s.Reasoning.String(), s.Truncated, s.Usage.ServiceTier)
	}
	// Anthropic message_delta with an upstream cost.
	s = &StreamState{}
	s.AddEvent("message_delta", []byte(`{"type":"message_delta","usage":{"output_tokens":3,"cost_in_usd_ticks":42}}`))
	if s.Usage.CompletionTokens != 3 || s.Usage.CostTicks != 42 || !s.Usage.HasCost {
		t.Fatalf("message_delta: %+v", s.Usage)
	}
}

func TestParseBodyShapes(t *testing.T) {
	cases := []struct {
		body   string
		images int64
		found  bool
	}{
		{`{"data":[{"url":"https://x/1.png"},{"b64_json":"QUJD"}]}`, 2, true},
		{`{"object":"list","data":[{"url":"x"}]}`, 0, false},
		{`{"data":["x"]}`, 0, false},
		{`{"data":[{"id":"m"}]}`, 0, false},
		{`{"usage":"n/a"}`, 0, false},
		{`{"usage":{"input_tokens":"x"}}`, 0, false},
		{`{"usage":{"image_cost":7}}`, 0, true},
	}
	for _, c := range cases {
		u, _ := ParseBody([]byte(c.body))
		if u.Images != c.images || u.Found != c.found {
			t.Errorf("%s: %+v", c.body, u)
		}
	}
	if u, _ := ParseBody([]byte(`{"usage":{"image_cost":7}}`)); u.CostTicks != 7 || !u.HasCost {
		t.Fatalf("image_cost: %+v", u)
	}
}
