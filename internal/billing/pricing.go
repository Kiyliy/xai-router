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
	Quality    string
	Resolution string
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
	case u.VideoSeconds > 0 && m.VideoTicksPerSec > 0:
		cost = int64(math.Round(u.VideoSeconds * float64(m.VideoTicksPerSec)))
	case u.Images > 0 && (m.ImageTicks > 0 || len(m.ImageTiers) > 0):
		cost = u.Images * imagePrice(m, req)
	default:
		cost = tokenCost(m, u)
	}
	if cost == 0 {
		return 0, "none"
	}
	return cost, "computed"
}

func imagePrice(m *store.Model, req Request) int64 {
	if req.Quality != "" || req.Resolution != "" {
		for _, tr := range m.ImageTiers {
			if (req.Quality == "" || strings.EqualFold(tr.Quality, req.Quality)) &&
				(req.Resolution == "" || strings.EqualFold(tr.Resolution, req.Resolution)) {
				return tr.Ticks
			}
		}
	}
	return m.ImageTicks
}

func tokenCost(m *store.Model, u Usage) int64 {
	in, cached, out := m.InputTicks, m.CachedTicks, m.OutputTicks
	imgIn := m.ImageInputTicks
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
