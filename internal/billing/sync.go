package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/Kiyliy/xai-router/internal/store"
)

// SyncResult reports a model sync.
type SyncResult struct {
	At      time.Time `json:"at"`
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Added   []string  `json:"added"`
	Updated []string  `json:"updated"`
	Total   int       `json:"total"`
}

// xAI model-list payloads. All prices are in ticks (per token / per image).
type xaiLanguageModel struct {
	ID                     string   `json:"id"`
	Aliases                []string `json:"aliases"`
	ContextLength          int64    `json:"context_length"`
	PromptTextTokenPrice   int64    `json:"prompt_text_token_price"`
	CachedPromptTokenPrice int64    `json:"cached_prompt_text_token_price"`
	PromptImageTokenPrice  int64    `json:"prompt_image_token_price"`
	CompletionTokenPrice   int64    `json:"completion_text_token_price"`
	PromptPriceLong        int64    `json:"prompt_text_token_price_long_context"`
	CachedPriceLong        int64    `json:"cached_prompt_text_token_price_long_context"`
	CompletionPriceLong    int64    `json:"completion_text_token_price_long_context"`
	LongContextThreshold   int64    `json:"long_context_threshold"`
}

type xaiImageModel struct {
	ID         string   `json:"id"`
	Aliases    []string `json:"aliases"`
	ImagePrice int64    `json:"image_price"`
	Pricing    []struct {
		Quality       string `json:"quality"`
		Resolution    string `json:"resolution"`
		PricePerImage int64  `json:"price_per_image"`
	} `json:"pricing"`
}

type xaiPlainModel struct {
	ID      string   `json:"id"`
	Aliases []string `json:"aliases"`
}

// Syncer pulls models and prices from xAI.
type Syncer struct {
	DB     *store.DB
	Table  *Table
	Client *http.Client
}

// Sync fetches the model lists with the given key and updates the price table.
// Models edited by hand (source=manual) are kept unless overwriteManual is set.
func (s *Syncer) Sync(ctx context.Context, baseURL, apiKey string, overwriteManual bool) SyncResult {
	res := SyncResult{At: time.Now().UTC(), Added: []string{}, Updated: []string{}}
	var lang struct{ Models []xaiLanguageModel }
	var img struct{ Models []xaiImageModel }
	var vid, emb struct{ Models []xaiPlainModel }
	var all struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	for _, f := range []struct {
		path string
		dst  any
	}{
		{"/v1/language-models", &lang},
		{"/v1/image-generation-models", &img},
		{"/v1/video-generation-models", &vid},
		{"/v1/embedding-models", &emb},
		{"/v1/models", &all},
	} {
		if err := s.get(ctx, baseURL+f.path, apiKey, f.dst); err != nil {
			res.Error = err.Error()
			return res
		}
	}
	existing := map[string]*store.Model{}
	if all, err := s.DB.ListModels(ctx, ""); err == nil {
		for _, m := range all {
			existing[m.ID] = m
		}
	}
	now := store.NowMs()
	var incoming []*store.Model
	ctxLen := map[string]int64{}
	for _, m := range all.Data {
		ctxLen[m.ID] = m.ContextLength
	}
	for _, m := range lang.Models {
		if m.ContextLength == 0 {
			m.ContextLength = ctxLen[m.ID]
		}
		raw, _ := json.Marshal(m)
		incoming = append(incoming, &store.Model{ID: m.ID, Kind: "language", Aliases: m.Aliases, ContextLength: m.ContextLength,
			InputTicks: m.PromptTextTokenPrice, CachedTicks: m.CachedPromptTokenPrice, ImageInputTicks: m.PromptImageTokenPrice,
			OutputTicks: m.CompletionTokenPrice, LongThreshold: m.LongContextThreshold, LongInputTicks: m.PromptPriceLong,
			LongCachedTicks: m.CachedPriceLong, LongOutputTicks: m.CompletionPriceLong, Raw: string(raw)})
	}
	for _, m := range img.Models {
		raw, _ := json.Marshal(m)
		tiers := []store.ImageTier{}
		for _, p := range m.Pricing {
			tiers = append(tiers, store.ImageTier{Quality: p.Quality, Resolution: p.Resolution, Ticks: p.PricePerImage})
		}
		incoming = append(incoming, &store.Model{ID: m.ID, Kind: "image", Aliases: m.Aliases, ImageTicks: m.ImagePrice,
			ImageTiers: tiers, Raw: string(raw)})
	}
	for _, m := range vid.Models {
		raw, _ := json.Marshal(m)
		v := &store.Model{ID: m.ID, Kind: "video", Aliases: m.Aliases, Raw: string(raw)}
		if old := existing[m.ID]; old != nil && builtinModel(m.ID) == nil {
			// Prices of a video model unknown to the built-in table are kept.
			v.VideoTicksPerSec, v.VideoTiers = old.VideoTicksPerSec, old.VideoTiers
			v.InputImageTicks, v.InputVideoTicksPerSec = old.InputImageTicks, old.InputVideoTicksPerSec
		}
		incoming = append(incoming, v)
	}
	for _, m := range emb.Models {
		raw, _ := json.Marshal(m)
		incoming = append(incoming, &store.Model{ID: m.ID, Kind: "embedding", Aliases: m.Aliases, Raw: string(raw)})
	}
	for _, m := range incoming {
		FillMediaPrices(m)
	}
	for _, m := range incoming {
		old := existing[m.ID]
		if old != nil && old.Source == "manual" && !overwriteManual {
			continue
		}
		m.Enabled, m.Source, m.SyncedAt = true, "sync", now
		if m.Aliases == nil {
			m.Aliases = []string{}
		}
		if old != nil {
			m.Enabled = old.Enabled
			if samePrices(old, m) && old.Source == "sync" {
				// Only refresh the sync timestamp.
				old.SyncedAt = now
				m = old
			} else {
				res.Updated = append(res.Updated, m.ID)
			}
		} else {
			res.Added = append(res.Added, m.ID)
		}
		if err := s.DB.UpsertModel(ctx, m); err != nil {
			res.Error = err.Error()
			return res
		}
	}
	sort.Strings(res.Added)
	sort.Strings(res.Updated)
	res.Total = len(incoming)
	res.OK = true
	if err := s.Table.Reload(ctx, s.DB); err != nil {
		res.OK, res.Error = false, err.Error()
	}
	return res
}

func samePrices(a, b *store.Model) bool {
	if a.Kind != b.Kind || a.InputTicks != b.InputTicks || a.CachedTicks != b.CachedTicks ||
		a.ImageInputTicks != b.ImageInputTicks || a.OutputTicks != b.OutputTicks || a.LongThreshold != b.LongThreshold ||
		a.LongInputTicks != b.LongInputTicks || a.LongCachedTicks != b.LongCachedTicks ||
		a.LongOutputTicks != b.LongOutputTicks || a.ImageTicks != b.ImageTicks ||
		a.VideoTicksPerSec != b.VideoTicksPerSec || a.ContextLength != b.ContextLength ||
		a.InputImageTicks != b.InputImageTicks || a.InputVideoTicksPerSec != b.InputVideoTicksPerSec ||
		!sameTiers(a.ImageTiers, b.ImageTiers) || !sameTiers(a.VideoTiers, b.VideoTiers) ||
		len(a.Aliases) != len(b.Aliases) {
		return false
	}
	for i := range a.Aliases {
		if a.Aliases[i] != b.Aliases[i] {
			return false
		}
	}
	return true
}

func sameTiers(a, b []store.ImageTier) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Syncer) get(ctx context.Context, url, apiKey string, dst any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d: %s", url, resp.StatusCode, truncate(string(body), 300))
	}
	return json.Unmarshal(body, dst)
}

// SeedBuiltin inserts built-in models that are not in the database yet, and
// completes synced or built-in media models with prices added in later
// versions (resolution tiers, input prices). Manual edits are kept.
func SeedBuiltin(ctx context.Context, db *store.DB) error {
	for _, m := range BuiltinModels() {
		old, err := db.GetModel(ctx, m.ID)
		if err == nil {
			if old.Source == "manual" {
				continue
			}
			before := *old
			FillMediaPrices(old)
			if samePrices(&before, old) {
				continue
			}
			m = old
		}
		if err := db.UpsertModel(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
