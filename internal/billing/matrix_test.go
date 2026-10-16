package billing

import (
	"fmt"
	"math"
	"testing"
)

// Acceptance E5: every model class × billing scenario has a case.
//
// The expected values are computed from an independent table typed from
// docs.x.ai/developers/pricing (2026-10-16), in USD, so the test does not
// trust the gateway's own price table. Results are logged as a matrix
// (go test -v -run TestBillingMatrix ./internal/billing/).

type officialText struct {
	in, cached, out             float64 // USD per 1M tokens, prompt < 200k
	longIn, longCached, longOut float64 // prompt >= 200k
}

var officialTextPrices = map[string]officialText{
	"grok-4.7":                     {2.00, 0.50, 6.00, 4.00, 1.00, 12.00},
	"grok-4.6":                     {2.00, 0.50, 6.00, 4.00, 1.00, 12.00},
	"grok-4.5":                     {2.00, 0.30, 6.00, 4.00, 0.60, 12.00},
	"grok-4.3":                     {1.25, 0.20, 2.50, 2.50, 0.40, 5.00},
	"grok-4.20-0309-reasoning":     {1.25, 0.20, 2.50, 2.50, 0.40, 5.00},
	"grok-4.20-0309-non-reasoning": {1.25, 0.20, 2.50, 2.50, 0.40, 5.00},
	"grok-4.20-multi-agent-0309":   {1.25, 0.20, 2.50, 2.50, 0.40, 5.00},
	"grok-build-0.1":               {1.00, 0.20, 2.00, 2.00, 0.40, 4.00},
}

var officialImagePrices = map[string]float64{ // USD per image (default tier)
	"grok-imagine-image":         0.02,
	"grok-imagine-image-quality": 0.05,
	"grok-imagine-image-2.0":     0.04, // low quality, 1k (the default for generations)
}

var officialImage20Tiers = map[[2]string]float64{ // quality, resolution
	{"low", "1k"}: 0.04, {"low", "1.5k"}: 0.05, {"low", "2k"}: 0.06,
	{"medium", "1k"}: 0.06, {"medium", "1.5k"}: 0.07, {"medium", "2k"}: 0.08,
}

var officialVideoPrices = map[string]float64{ // USD per second
	"grok-imagine-video":          0.050,
	"grok-imagine-video-1.5":      0.080,
	"grok-imagine-video-1.5-lite": 0.020,
}

const usdPerTick = 1e-10

func usdOf(ticks int64) float64 { return float64(ticks) * usdPerTick }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestBillingMatrix(t *testing.T) {
	tb := table()
	type scenario struct {
		name string
		u    Usage
		usd  func(p officialText) float64
	}
	const M = 1e6
	scenarios := []scenario{
		{"plain", Usage{Found: true, PromptTokens: 1000, CompletionTokens: 200},
			func(p officialText) float64 { return 1000*p.in/M + 200*p.out/M }},
		{"cached", Usage{Found: true, PromptTokens: 1000, CachedTokens: 600, CompletionTokens: 200},
			func(p officialText) float64 { return 400*p.in/M + 600*p.cached/M + 200*p.out/M }},
		{"reasoning", Usage{Found: true, PromptTokens: 1000, CompletionTokens: 100, ReasoningTokens: 300},
			func(p officialText) float64 { return 1000*p.in/M + 400*p.out/M }},
		{"cached+reasoning", Usage{Found: true, PromptTokens: 188, CachedTokens: 128, CompletionTokens: 9, ReasoningTokens: 173},
			func(p officialText) float64 { return 60*p.in/M + 128*p.cached/M + 182*p.out/M }},
		{"long context (>=200k)", Usage{Found: true, PromptTokens: 250_000, CachedTokens: 50_000, CompletionTokens: 1000},
			func(p officialText) float64 { return 200_000*p.longIn/M + 50_000*p.longCached/M + 1000*p.longOut/M }},
		{"exactly 200k prompt", Usage{Found: true, PromptTokens: 200_000, CompletionTokens: 10},
			func(p officialText) float64 { return 200_000*p.longIn/M + 10*p.longOut/M }},
		{"image input tokens", Usage{Found: true, PromptTokens: 1500, ImageTokens: 1000, CompletionTokens: 50},
			func(p officialText) float64 { return 1500*p.in/M + 50*p.out/M }},
		{"priority tier (x2)", Usage{Found: true, PromptTokens: 1000, CompletionTokens: 200, ServiceTier: "priority"},
			func(p officialText) float64 { return 2 * (1000*p.in/M + 200*p.out/M) }},
	}
	t.Log("| model | scenario | expected USD | gateway USD |")
	for model, p := range officialTextPrices {
		if tb.Lookup(model) == nil {
			t.Fatalf("%s missing from the built-in price table", model)
		}
		for _, sc := range scenarios {
			got, src := tb.Cost(model, sc.u, Request{})
			want := sc.usd(p)
			t.Logf("| %s | %s | %.10f | %.10f |", model, sc.name, want, usdOf(got))
			if src != "computed" || !near(usdOf(got), want) {
				t.Errorf("%s / %s: got $%.10f (%s), want $%.10f", model, sc.name, usdOf(got), src, want)
			}
		}
		// Upstream cost always wins.
		u := Usage{Found: true, PromptTokens: 1000, CompletionTokens: 200, CostTicks: 123456, HasCost: true}
		if got, src := tb.Cost(model, u, Request{}); got != 123456 || src != "upstream" {
			t.Errorf("%s: upstream cost not used: %d %s", model, got, src)
		}
	}
	for model, price := range officialImagePrices {
		for _, n := range []int64{1, 3, 10} {
			got, src := tb.Cost(model, Usage{Found: true, Images: n}, Request{})
			want := float64(n) * price
			t.Logf("| %s | %d image(s) | %.10f | %.10f |", model, n, want, usdOf(got))
			if src != "computed" || !near(usdOf(got), want) {
				t.Errorf("%s × %d: got $%.10f, want $%.10f", model, n, usdOf(got), want)
			}
		}
		if got, src := tb.Cost(model, Usage{Found: true, Images: 2, CostTicks: 7, HasCost: true}, Request{}); got != 7 || src != "upstream" {
			t.Errorf("%s: upstream cost not used", model)
		}
	}
	for tier, price := range officialImage20Tiers {
		got, _ := tb.Cost("grok-imagine-image-2.0", Usage{Found: true, Images: 2}, Request{Quality: tier[0], Resolution: tier[1]})
		t.Logf("| grok-imagine-image-2.0 | 2 images, %s/%s | %.10f | %.10f |", tier[0], tier[1], 2*price, usdOf(got))
		if !near(usdOf(got), 2*price) {
			t.Errorf("image-2.0 %v: got $%.10f, want $%.10f", tier, usdOf(got), 2*price)
		}
	}
	for model, price := range officialVideoPrices {
		for _, secs := range []float64{1, 6, 15} {
			got, src := tb.Cost(model, Usage{Found: true, VideoSeconds: secs}, Request{})
			t.Logf("| %s | %g s video | %.10f | %.10f |", model, secs, secs*price, usdOf(got))
			if src != "computed" || !near(usdOf(got), secs*price) {
				t.Errorf("%s %gs: got $%.10f, want $%.10f", model, secs, usdOf(got), secs*price)
			}
		}
		if got, src := tb.Cost(model, Usage{Found: true, VideoSeconds: 6, CostTicks: 9, HasCost: true}, Request{}); got != 9 || src != "upstream" {
			t.Errorf("%s: upstream cost not used", model)
		}
	}
	// Voice (no upstream cost exists for these): published rates.
	for _, c := range []struct {
		u    WSUsage
		want float64
	}{
		{WSUsage{Path: "/v1/realtime", DurationMs: 90_000, TextInputs: 2}, 1.5*0.08 + 2*0.004},
		{WSUsage{Path: "/v1/tts", TTSChars: 1_000_000}, 15},
		{WSUsage{Path: "/v1/stt", AudioSecond: 3600}, 0.20},
		{WSUsage{Path: "/v1/stt", DurationMs: 1_800_000}, 0.10},
	} {
		got := usdOf(WSCost(c.u))
		t.Logf("| voice %s | %+v | %.10f | %.10f |", c.u.Path, c.u, c.want, got)
		if !near(got, c.want) {
			t.Errorf("%+v: got $%.10f, want $%.10f", c.u, got, c.want)
		}
	}
	if n := len(officialTextPrices); n != 8 {
		t.Fatal(fmt.Sprint("text model table size ", n))
	}
}
