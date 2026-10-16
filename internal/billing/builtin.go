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

var builtinImage = []struct {
	id      string
	aliases []string
	usd     float64
	tiers   []store.ImageTier
}{
	{"grok-imagine-image", []string{"grok-imagine-image-2026-03-02"}, 0.02, nil},
	{"grok-imagine-image-quality", []string{"grok-imagine-image-quality-latest", "grok-imagine-image-pro"}, 0.05, nil},
	{"grok-imagine-image-2.0", nil, 0.04, []store.ImageTier{
		{Quality: "low", Resolution: "1k", Ticks: 400_000_000},
		{Quality: "low", Resolution: "1.5k", Ticks: 500_000_000},
		{Quality: "low", Resolution: "2k", Ticks: 600_000_000},
		{Quality: "medium", Resolution: "1k", Ticks: 600_000_000},
		{Quality: "medium", Resolution: "1.5k", Ticks: 700_000_000},
		{Quality: "medium", Resolution: "2k", Ticks: 800_000_000},
	}},
}

// VideoPrices are USD per second of generated video. The xAI model-list API
// does not expose them, so sync keeps these values.
var VideoPrices = map[string]float64{
	"grok-imagine-video":          0.050,
	"grok-imagine-video-1.5":      0.080,
	"grok-imagine-video-1.5-lite": 0.020,
}

var videoAliases = map[string][]string{
	"grok-imagine-video-1.5": {"grok-imagine-video-1.5-preview", "grok-imagine-video-1.5-2026-05-30"},
}

// BuiltinModels returns the built-in price table.
func BuiltinModels() []*store.Model {
	var out []*store.Model
	for _, p := range builtinLanguage {
		out = append(out, &store.Model{
			ID: p.id, Kind: "language", Aliases: p.aliases, Enabled: true, Source: "builtin", ContextLength: p.ctx,
			InputTicks: USDPerMillionToTicks(p.in), CachedTicks: USDPerMillionToTicks(p.cached),
			ImageInputTicks: USDPerMillionToTicks(p.in), OutputTicks: USDPerMillionToTicks(p.out),
			LongThreshold: p.threshold, LongInputTicks: USDPerMillionToTicks(p.longIn),
			LongCachedTicks: USDPerMillionToTicks(p.longCached), LongOutputTicks: USDPerMillionToTicks(p.longOut),
		})
	}
	for _, p := range builtinImage {
		out = append(out, &store.Model{ID: p.id, Kind: "image", Aliases: p.aliases, Enabled: true, Source: "builtin",
			ImageTicks: USDToTicks(p.usd), ImageTiers: p.tiers})
	}
	for _, id := range []string{"grok-imagine-video", "grok-imagine-video-1.5", "grok-imagine-video-1.5-lite"} {
		out = append(out, &store.Model{ID: id, Kind: "video", Aliases: videoAliases[id], Enabled: true, Source: "builtin",
			VideoTicksPerSec: USDToTicks(VideoPrices[id])})
	}
	return out
}
