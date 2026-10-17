package billing

import "github.com/Kiyliy/xai-router/internal/store"

// Built-in official prices, verified against docs.x.ai/developers/pricing and
// GET /v1/language-models on 2026-10-16. They seed an empty database and fill
// in what the model-list endpoints do not expose (video prices). A sync from
// xAI overwrites token and image prices with live values.

type langPrice struct {
	id                          string
	aliases                     []string
	ctx                         int64
	in, cached, out             float64 // USD per 1M tokens
	longIn, longCached, longOut float64
	threshold                   int64
}

var builtinLanguage = []langPrice{
	{"grok-4.7", nil, 500_000, 2.00, 0.50, 6.00, 4.00, 1.00, 12.00, 200_000},
	{"grok-4.6", nil, 500_000, 2.00, 0.50, 6.00, 4.00, 1.00, 12.00, 200_000},
	{"grok-4.5", []string{"grok-4.5-latest", "grok-build-latest"}, 500_000, 2.00, 0.30, 6.00, 4.00, 0.60, 12.00, 200_000},
	{"grok-4.3", []string{"grok-4.3-latest"}, 1_000_000, 1.25, 0.20, 2.50, 2.50, 0.40, 5.00, 200_000},
	{"grok-4.20-0309-reasoning", []string{"grok-4.20", "grok-4.20-reasoning", "grok-4.20-reasoning-latest", "grok-4.20-0309"}, 1_000_000, 1.25, 0.20, 2.50, 2.50, 0.40, 5.00, 200_000},
	{"grok-4.20-0309-non-reasoning", []string{"grok-4.20-non-reasoning", "grok-4.20-non-reasoning-latest"}, 1_000_000, 1.25, 0.20, 2.50, 2.50, 0.40, 5.00, 200_000},
	{"grok-4.20-multi-agent-0309", []string{"grok-4.20-multi-agent", "grok-4.20-multi-agent-latest"}, 1_000_000, 1.25, 0.20, 2.50, 2.50, 0.40, 5.00, 200_000},
	{"grok-build-0.1", []string{"grok-code-fast-1", "grok-code-fast", "grok-code-fast-1-0825"}, 256_000, 1.00, 0.20, 2.00, 2.00, 0.40, 4.00, 200_000},
}

// res builds resolution-only tiers from (resolution, USD) pairs.
func res(pairs ...any) []store.ImageTier {
	var out []store.ImageTier
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, store.ImageTier{Resolution: pairs[i].(string), Ticks: USDToTicks(pairs[i+1].(float64))})
	}
	return out
}

// Image prices are USD per generated image; inputUSD is per input image
// (edits). docs.x.ai/developers/pricing, 2026-10-10.
var builtinImage = []struct {
	id       string
	aliases  []string
	usd      float64
	tiers    []store.ImageTier
	inputUSD float64
}{
	{"grok-imagine-image", []string{"grok-imagine-image-2026-03-02"}, 0.02, res("1k", 0.02, "2k", 0.02), 0.002},
	{"grok-imagine-image-quality", []string{"grok-imagine-image-quality-latest", "grok-imagine-image-pro"}, 0.05,
		res("1k", 0.05, "1.5k", 0.06, "2k", 0.07), 0.01},
	{"grok-imagine-image-2.0", nil, 0.04, []store.ImageTier{
		{Quality: "low", Resolution: "1k", Ticks: 400_000_000},
		{Quality: "low", Resolution: "1.5k", Ticks: 500_000_000},
		{Quality: "low", Resolution: "2k", Ticks: 600_000_000},
		{Quality: "medium", Resolution: "1k", Ticks: 600_000_000},
		{Quality: "medium", Resolution: "1.5k", Ticks: 700_000_000},
		{Quality: "medium", Resolution: "2k", Ticks: 800_000_000},
	}, 0.01},
}

// builtinVideo holds video prices: USD per second of output by resolution
// (the first tier, 480p, is the default and base price), per input image and
// per second of input video. The xAI model-list API does not expose them, so
// sync keeps these values.
var builtinVideo = []struct {
	id                  string
	aliases             []string
	tiers               []store.ImageTier
	inputImageUSD       float64
	inputVideoUSDPerSec float64
}{
	{"grok-imagine-video", nil, res("480p", 0.05, "720p", 0.07), 0.002, 0.01},
	{"grok-imagine-video-1.5", []string{"grok-imagine-video-1.5-preview", "grok-imagine-video-1.5-2026-05-30"},
		res("480p", 0.08, "720p", 0.14, "1080p", 0.25), 0.01, 0},
	{"grok-imagine-video-1.5-lite", nil, res("480p", 0.02, "720p", 0.03, "1080p", 0.14), 0.01, 0},
}

// BuiltinModels returns the built-in price table.
func BuiltinModels() []*store.Model {
	var out []*store.Model
	for _, p := range builtinLanguage {
		out = append(out, &store.Model{
			ID: p.id, Kind: "language", Aliases: p.aliases, Enabled: true, Source: "builtin", ContextLength: p.ctx,
			InputTicks: USDPerMillionToTicks(p.in), CachedTicks: USDPerMillionToTicks(p.cached),
			ImageTokenTicks: USDPerMillionToTicks(p.in), OutputTicks: USDPerMillionToTicks(p.out),
			LongThreshold: p.threshold, LongInputTicks: USDPerMillionToTicks(p.longIn),
			LongCachedTicks: USDPerMillionToTicks(p.longCached), LongOutputTicks: USDPerMillionToTicks(p.longOut),
		})
	}
	for _, p := range builtinImage {
		out = append(out, &store.Model{ID: p.id, Kind: "image", Aliases: p.aliases, Enabled: true, Source: "builtin",
			ImageTicks: USDToTicks(p.usd), ImageTiers: p.tiers, InputImageTicks: USDToTicks(p.inputUSD)})
	}
	for _, p := range builtinVideo {
		out = append(out, &store.Model{ID: p.id, Kind: "video", Aliases: p.aliases, Enabled: true, Source: "builtin",
			VideoTicksPerSec: p.tiers[0].Ticks, VideoTiers: p.tiers, InputImageTicks: USDToTicks(p.inputImageUSD),
			InputVideoTicksPerSec: USDToTicks(p.inputVideoUSDPerSec)})
	}
	return out
}

// builtinModel returns the built-in entry for id, or nil.
func builtinModel(id string) *store.Model {
	for _, m := range BuiltinModels() {
		if m.ID == id {
			return m
		}
	}
	return nil
}

// FillMediaPrices completes a synced media model with the built-in prices
// the xAI model-list API does not expose: resolution tiers, video prices and
// media input prices. A model with resolution/quality tiers is listed at its
// default tier (1k, quality auto→low), so the base price is the same whether
// it came from sync or from the built-in table.
func FillMediaPrices(m *store.Model) {
	b := builtinModel(m.ID)
	if b == nil || b.Kind != m.Kind {
		return
	}
	if len(m.ImageTiers) == 0 {
		m.ImageTiers = b.ImageTiers
	}
	if len(m.VideoTiers) == 0 {
		m.VideoTiers = b.VideoTiers
	}
	if m.VideoTicksPerSec == 0 {
		m.VideoTicksPerSec = b.VideoTicksPerSec
	}
	if m.InputImageTicks == 0 {
		m.InputImageTicks = b.InputImageTicks
	}
	if m.InputVideoTicksPerSec == 0 {
		m.InputVideoTicksPerSec = b.InputVideoTicksPerSec
	}
	if m.Kind == "image" && len(m.ImageTiers) > 0 {
		if p := imagePrice(&store.Model{ImageTiers: m.ImageTiers}, Request{}); p > 0 {
			m.ImageTicks = p
		}
	}
}
