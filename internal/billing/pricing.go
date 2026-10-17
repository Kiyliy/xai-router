package billing

import (
	"context"
	"math"
	"strings"
	"sync"

	"github.com/Kiyliy/xai-router/internal/store"
)

// Server-side tool prices in ticks per invocation (docs.x.ai/developers/pricing).
const (
	ticksWebSearch   = 50_000_000  // $5 / 1k calls
	ticksXPost       = 50_000_000  // $5 / 1k posts
	ticksXUser       = 100_000_000 // $10 / 1k profiles
	ticksCodeExec    = 50_000_000  // $5 / 1k calls
	ticksAttachment  = 50_000_000  // $5 / 1k calls
	ticksCollections = 25_000_000  // $2.50 / 1k calls
)

// USDPerMillionToTicks converts a "USD per 1M tokens" price to ticks per token.
func USDPerMillionToTicks(usd float64) int64 { return int64(math.Round(usd * 1e4)) }

// TicksToUSDPerMillion is the inverse of USDPerMillionToTicks.
func TicksToUSDPerMillion(t int64) float64 { return float64(t) / 1e4 }

// USDToTicks converts dollars to ticks.
func USDToTicks(usd float64) int64 { return int64(math.Round(usd * TicksPerUSD)) }

// TicksToUSD converts ticks to dollars.
func TicksToUSD(t int64) float64 { return float64(t) / TicksPerUSD }

// Table is an in-memory, alias-aware view of the models table.
type Table struct {
	mu     sync.RWMutex
	byName map[string]*store.Model
	all    []*store.Model
}

// NewTable returns an empty table.
func NewTable() *Table { return &Table{byName: map[string]*store.Model{}} }

// Reload replaces the table contents from the database.
func (t *Table) Reload(ctx context.Context, db *store.DB) error {
	models, err := db.ListModels(ctx, "")
	if err != nil {
		return err
	}
	t.Set(models)
	return nil
}

// Set replaces the table contents.
func (t *Table) Set(models []*store.Model) {
	by := make(map[string]*store.Model, len(models)*3)
	for _, m := range models {
		for _, a := range m.Aliases {
			by[strings.ToLower(a)] = m
		}
	}
	for _, m := range models { // canonical ids win over aliases
		by[strings.ToLower(m.ID)] = m
	}
	t.mu.Lock()
	t.byName, t.all = by, models
	t.mu.Unlock()
}

// Lookup finds a model by id or alias.
func (t *Table) Lookup(name string) *store.Model {
	if name == "" {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byName[strings.ToLower(name)]
}

// All returns the models (shared; do not mutate).
func (t *Table) All() []*store.Model {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.all
}

// Cheapest returns the cheapest enabled language model, used for key probes.
func (t *Table) Cheapest() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	best, bestCost := "", int64(math.MaxInt64)
	for _, m := range t.all {
		if m.Kind != "language" || !m.Enabled || m.InputTicks == 0 || strings.Contains(m.ID, "multi-agent") {
			continue
		}
		if c := m.InputTicks + m.OutputTicks; c < bestCost {
			best, bestCost = m.ID, c
		}
	}
	return best
}

// Request carries request parameters that influence media prices.
type Request struct {
	Quality    string `json:"quality,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	Edit       bool   `json:"edit,omitempty"` // image edit (default quality differs from generation)
	// VideoEdit marks a video edit: its input video is as long as the output,
	// so InputVideoSeconds defaults to the generated seconds.
	VideoEdit bool `json:"video_edit,omitempty"`
	// Media inputs: images attached to an image edit or video request, and
	// seconds of input video (video edits).
	InputImages       int64   `json:"input_images,omitempty"`
	InputVideoSeconds float64 `json:"input_video_seconds,omitempty"`
}

// Cost prices usage. Upstream-reported cost always wins; otherwise the price
// table is used. Returns the cost in ticks and its source.
func (t *Table) Cost(model string, u Usage, req Request) (int64, string) {
	if u.HasCost {
		return u.CostTicks, "upstream"
	}
	m := t.Lookup(model)
	if m == nil {
		return 0, "none"
	}
	var cost int64
	switch {
	case u.VideoSeconds > 0 && (m.VideoTicksPerSec > 0 || len(m.VideoTiers) > 0):
		if req.VideoEdit && req.InputVideoSeconds == 0 {
			req.InputVideoSeconds = u.VideoSeconds
		}
		cost = int64(math.Round(u.VideoSeconds*float64(VideoPrice(m, req.Resolution)))) + inputCost(m, req)
	case u.Images > 0 && (m.ImageTicks > 0 || len(m.ImageTiers) > 0):
		cost = u.Images*imagePrice(m, req) + inputCost(m, req)
	default:
		cost = tokenCost(m, u)
	}
	if cost == 0 {
		return 0, "none"
	}
	return cost, "computed"
}

// inputCost is the fee for media inputs (per input image, per second of
// input video).
func inputCost(m *store.Model, req Request) int64 {
	return req.InputImages*m.InputImageTicks + int64(math.Round(req.InputVideoSeconds*float64(m.InputVideoTicksPerSec)))
}

// Default resolutions when a request sets none (docs.x.ai).
const (
	DefaultImageResolution = "1k"
	DefaultVideoResolution = "480p"
)

// normRes normalises a resolution parameter: "2K" → "2k", "720" → "720p".
func normRes(res string, video bool) string {
	res = strings.ToLower(strings.TrimSpace(res))
	if video && res != "" && strings.Trim(res, "0123456789") == "" {
		res += "p"
	}
	return res
}

// VideoPrice is the per-second price for the requested resolution: the
// matching tier, else the highest tier.
func VideoPrice(m *store.Model, resolution string) int64 {
	res := normRes(resolution, true)
	if res == "" {
		res = DefaultVideoResolution
	}
	for _, tr := range m.VideoTiers {
		if normRes(tr.Resolution, true) == res {
			return tr.Ticks
		}
	}
	// A resolution without its own price is billed at the highest one, the
	// same bound the quota reservation uses.
	return MaxVideoPrice(m)
}

// MaxVideoPrice is the highest per-second price of a model (reservations).
func MaxVideoPrice(m *store.Model) int64 {
	p := m.VideoTicksPerSec
	for _, tr := range m.VideoTiers {
		p = max(p, tr.Ticks)
	}
	return p
}

// imagePrice picks the per-image price. Tiers match on resolution (default
// 1k) and, when a tier names one, quality: grok-imagine-image-2.0 defaults to
// quality "auto", which xAI serves as low for generations and medium for
// edits.
func imagePrice(m *store.Model, req Request) int64 {
	if len(m.ImageTiers) == 0 {
		return m.ImageTicks
	}
	q, res := strings.ToLower(req.Quality), normRes(req.Resolution, false)
	if q == "" || q == "auto" {
		q = "low"
		if req.Edit {
			q = "medium"
		}
	}
	if res == "" {
		res = DefaultImageResolution
	}
	for _, tr := range m.ImageTiers {
		if (tr.Quality == "" || strings.EqualFold(tr.Quality, q)) && normRes(tr.Resolution, false) == res {
			return tr.Ticks
		}
	}
	// No matching tier: the highest tier (what the reservation assumed).
	best := m.ImageTicks
	for _, tr := range m.ImageTiers {
		best = max(best, tr.Ticks)
	}
	return best
}

func tokenCost(m *store.Model, u Usage) int64 {
	in, cached, out := m.InputTicks, m.CachedTicks, m.OutputTicks
	imgIn := m.ImageTokenTicks
	if m.LongThreshold > 0 && u.PromptTokens >= m.LongThreshold {
		in, cached, out = m.LongInputTicks, m.LongCachedTicks, m.LongOutputTicks
		if imgIn == m.InputTicks { // image tokens follow the text input rate
			imgIn = in
		}
	}
	if imgIn == 0 {
		imgIn = in
	}
	text := max(u.PromptTokens-u.ImageTokens, 0)
	uncached := max(text-u.CachedTokens, 0)
	cost := uncached*in + u.CachedTokens*cached + u.ImageTokens*imgIn + (u.CompletionTokens+u.ReasoningTokens)*out
	if u.ServiceTier == "priority" {
		cost *= 2
	}
	tl := u.Tools
	cost += tl.WebSearch*ticksWebSearch + tl.XPosts*ticksXPost + tl.XUsers*ticksXUser +
		tl.CodeInterpreter*ticksCodeExec + tl.Attachment*ticksAttachment +
		(tl.FileSearch+tl.DocumentSearch)*ticksCollections
	return cost
}
