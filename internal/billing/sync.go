package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
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
	// Unpriced lists upstream models that came without a usable price.
	// New ones are added disabled; existing rows keep their state.
	Unpriced []string `json:"unpriced"`
	// Retired lists non-manual models that upstream no longer lists and
	// that this sync disabled.
	Retired []string `json:"retired"`
	// Errors holds per-endpoint failures (path -> error). The sync goes on
	// with the endpoints that answered; it fails only when no model list
	// could be read.
	Errors map[string]string `json:"errors,omitempty"`
}

// Meta keys used by sync and seeding.
const (
	// MetaRetiredModels holds the ids (JSON array) that a sync disabled
	// because upstream stopped listing them. They are re-enabled when they
	// come back.
	MetaRetiredModels = "models_retired"
	// MetaBuiltinSeen holds the built-in ids (JSON array) that were ever
	// seeded, so a built-in model the admin deleted is not added again.
	MetaBuiltinSeen = "builtin_models_seen"
)

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

type xaiEmbeddingModel struct {
	ID                    string   `json:"id"`
	Aliases               []string `json:"aliases"`
	PromptTextTokenPrice  int64    `json:"prompt_text_token_price"`
	PromptImageTokenPrice int64    `json:"prompt_image_token_price"`
}

// HasPrice reports whether m has a usable price for its kind: token prices
// for language and embedding models, a per-image price for image models and
// a per-second price for video models. A model without one is billed at $0
// when upstream reports no cost, so it needs a price before it is enabled.
func HasPrice(m *store.Model) bool {
	anyTier := func(ts []store.ImageTier) bool {
		for _, t := range ts {
			if t.Ticks > 0 {
				return true
			}
		}
		return false
	}
	switch m.Kind {
	case "language":
		return m.InputTicks > 0 || m.OutputTicks > 0
	case "embedding":
		return m.InputTicks > 0 || m.ImageTokenTicks > 0
	case "image":
		return m.ImageTicks > 0 || anyTier(m.ImageTiers)
	case "video":
		return m.VideoTicksPerSec > 0 || anyTier(m.VideoTiers)
	}
	return true
}

// Syncer pulls models and prices from xAI.
type Syncer struct {
	DB     *store.DB
	Table  *Table
	Client *http.Client
}

// Sync fetches the model lists with the given key and updates the price table.
// Models edited by hand (source=manual) are kept unless overwriteManual is set.
// Non-manual models that upstream no longer lists are disabled (never
// deleted, so logs still resolve) and reported in Retired; aliases that
// upstream now assigns to another model are removed from them.
func (s *Syncer) Sync(ctx context.Context, baseURL, apiKey string, overwriteManual bool) SyncResult {
	res := SyncResult{At: time.Now().UTC(), Added: []string{}, Updated: []string{}, Unpriced: []string{}, Retired: []string{}}
	var lang struct{ Models []xaiLanguageModel }
	var img struct{ Models []xaiImageModel }
	var vid struct{ Models []xaiPlainModel }
	var emb struct{ Models []xaiEmbeddingModel }
	var all struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int64  `json:"context_length"`
		} `json:"data"`
	}
	// listed[kind] is true when that kind's list was read and is not empty:
	// only then can a model of that kind be missing upstream.
	fetched, listed := map[string]bool{}, map[string]bool{}
	firstErr := ""
	for _, f := range []struct {
		path, kind string
		dst        any
		n          func() int
		reset      func()
	}{
		{"/v1/language-models", "language", &lang, func() int { return len(lang.Models) }, func() { lang.Models = nil }},
		{"/v1/image-generation-models", "image", &img, func() int { return len(img.Models) }, func() { img.Models = nil }},
		{"/v1/video-generation-models", "video", &vid, func() int { return len(vid.Models) }, func() { vid.Models = nil }},
		{"/v1/embedding-models", "embedding", &emb, func() int { return len(emb.Models) }, func() { emb.Models = nil }},
		{"/v1/models", "", &all, func() int { return len(all.Data) }, func() { all.Data = nil }},
	} {
		if err := s.get(ctx, baseURL+f.path, apiKey, f.dst); err != nil {
			f.reset() // a partly decoded list is not trusted
			if res.Errors == nil {
				res.Errors = map[string]string{}
			}
			res.Errors[f.path] = err.Error()
			if firstErr == "" {
				firstErr = err.Error()
			}
			continue
		}
		if f.kind != "" {
			fetched[f.kind] = true
			listed[f.kind] = f.n() > 0
		}
	}
	if len(fetched) == 0 {
		res.Error = firstErr
		return res
	}
	rows, err := s.DB.ListModels(ctx, "")
	if err != nil {
		// Without the current rows every manual model would look new.
		res.Error = "read models: " + err.Error()
		return res
	}
	existing := map[string]*store.Model{}
	for _, m := range rows {
		existing[m.ID] = m
	}
	retired, _, err := loadMetaSet(ctx, s.DB, MetaRetiredModels)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	retiredChanged := false
	now := store.NowMs()
	var incoming []*store.Model
	seen := map[string]bool{}
	add := func(m *store.Model) {
		if m.ID == "" || seen[m.ID] { // the first list naming an id wins
			return
		}
		seen[m.ID] = true
		incoming = append(incoming, m)
	}
	ctxLen := map[string]int64{}
	for _, m := range all.Data {
		ctxLen[m.ID] = m.ContextLength
	}
	for _, m := range lang.Models {
		if m.ContextLength == 0 {
			m.ContextLength = ctxLen[m.ID]
		}
		raw, _ := json.Marshal(m)
		add(&store.Model{ID: m.ID, Kind: "language", Aliases: m.Aliases, ContextLength: m.ContextLength,
			InputTicks: m.PromptTextTokenPrice, CachedTicks: m.CachedPromptTokenPrice, ImageTokenTicks: m.PromptImageTokenPrice,
			OutputTicks: m.CompletionTokenPrice, LongThreshold: m.LongContextThreshold, LongInputTicks: m.PromptPriceLong,
			LongCachedTicks: m.CachedPriceLong, LongOutputTicks: m.CompletionPriceLong, Raw: string(raw)})
	}
	for _, m := range img.Models {
		raw, _ := json.Marshal(m)
		tiers := []store.ImageTier{}
		for _, p := range m.Pricing {
			tiers = append(tiers, store.ImageTier{Quality: p.Quality, Resolution: p.Resolution, Ticks: p.PricePerImage})
		}
		add(&store.Model{ID: m.ID, Kind: "image", Aliases: m.Aliases, ImageTicks: m.ImagePrice,
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
		add(v)
	}
	for _, m := range emb.Models {
		raw, _ := json.Marshal(m)
		add(&store.Model{ID: m.ID, Kind: "embedding", Aliases: m.Aliases, InputTicks: m.PromptTextTokenPrice,
			ImageTokenTicks: m.PromptImageTokenPrice, Raw: string(raw)})
	}
	// claimed maps each alias upstream lists to the model it assigns it to.
	claimed := map[string]string{}
	for _, m := range incoming {
		FillMediaPrices(m)
		for _, a := range m.Aliases {
			if _, ok := claimed[strings.ToLower(a)]; !ok {
				claimed[strings.ToLower(a)] = m.ID
			}
		}
	}
	for _, m := range incoming {
		old := existing[m.ID]
		wasRetired := retired[m.ID]
		if wasRetired { // listed again
			delete(retired, m.ID)
			retiredChanged = true
		}
		if old != nil && old.Source == "manual" && !overwriteManual {
			continue
		}
		priced := HasPrice(m)
		if !priced {
			res.Unpriced = append(res.Unpriced, m.ID)
		}
		// A new model without a price starts disabled: enabled at $0 it
		// would be free for spending-limited tokens.
		m.Enabled, m.Source, m.SyncedAt = priced, "sync", now
		if m.Aliases == nil {
			m.Aliases = []string{}
		}
		// Built-in aliases upstream does not list (legacy ids it resolves
		// silently) are kept, after upstream's own, as SeedBuiltin adds them.
		for _, a := range builtinAliases[m.ID] {
			if !slices.Contains(m.Aliases, a) {
				m.Aliases = append(m.Aliases, a)
			}
		}
		if old != nil {
			m.Enabled = old.Enabled
			restore := wasRetired && !old.Enabled && priced
			if restore { // disabled by an earlier sync, not by the admin
				m.Enabled = true
			}
			if samePrices(old, m) && old.Source == "sync" && !restore {
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
	// Models upstream no longer lists: drop the aliases it now assigns to
	// another model, and disable them unless their list could not be read.
	for _, old := range rows {
		if seen[old.ID] || old.Source == "manual" {
			continue
		}
		changed := false
		if kept := withoutClaimed(old.Aliases, claimed, old.ID); len(kept) != len(old.Aliases) {
			old.Aliases, changed = kept, true
		}
		if listed[old.Kind] && old.Enabled {
			old.Enabled, changed = false, true
			retired[old.ID], retiredChanged = true, true
			res.Retired = append(res.Retired, old.ID)
		}
		if !changed {
			continue
		}
		if err := s.DB.UpsertModel(ctx, old); err != nil {
			res.Error = err.Error()
			return res
		}
	}
	if retiredChanged {
		if err := saveMetaSet(ctx, s.DB, MetaRetiredModels, retired); err != nil {
			res.Error = err.Error()
			return res
		}
	}
	sort.Strings(res.Added)
	sort.Strings(res.Updated)
	sort.Strings(res.Unpriced)
	sort.Strings(res.Retired)
	res.Total = len(incoming)
	res.OK = true
	if err := s.Table.Reload(ctx, s.DB); err != nil {
		res.OK, res.Error = false, err.Error()
	}
	return res
}

// withoutClaimed returns aliases minus those that upstream assigns to a
// model other than id.
func withoutClaimed(aliases []string, claimed map[string]string, id string) []string {
	out := make([]string, 0, len(aliases))
	for _, a := range aliases {
		if owner, ok := claimed[strings.ToLower(a)]; ok && owner != id {
			continue
		}
		out = append(out, a)
	}
	return out
}

// loadMetaSet reads a JSON array of ids from the meta table. found is false
// when the key was never written.
func loadMetaSet(ctx context.Context, db *store.DB, key string) (set map[string]bool, found bool, err error) {
	raw, err := db.GetMeta(ctx, key)
	if err != nil {
		return nil, false, err
	}
	set = map[string]bool{}
	if raw == "" {
		return set, false, nil
	}
	var ids []string
	if json.Unmarshal([]byte(raw), &ids) != nil {
		return set, true, nil // a damaged value starts over
	}
	for _, id := range ids {
		set[id] = true
	}
	return set, true, nil
}

func saveMetaSet(ctx context.Context, db *store.DB, key string, set map[string]bool) error {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	b, _ := json.Marshal(ids)
	return db.SetMeta(ctx, key, string(b))
}

func samePrices(a, b *store.Model) bool {
	if a.Kind != b.Kind || a.InputTicks != b.InputTicks || a.CachedTicks != b.CachedTicks ||
		a.ImageTokenTicks != b.ImageTokenTicks || a.OutputTicks != b.OutputTicks || a.LongThreshold != b.LongThreshold ||
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

// SeedBuiltin inserts built-in models that were never seeded before, and
// completes synced or built-in media models with prices added in later
// versions (resolution tiers, input prices). Video prices of non-manual rows
// follow the built-in table, since upstream does not publish them. Manual
// edits are kept, and a built-in model the admin deleted is not added again.
// builtinAliases maps a built-in model id to its built-in aliases.
var builtinAliases = func() map[string][]string {
	out := map[string][]string{}
	for _, m := range BuiltinModels() {
		out[m.ID] = m.Aliases
	}
	return out
}()

func SeedBuiltin(ctx context.Context, db *store.DB) error {
	seen, _, err := loadMetaSet(ctx, db, MetaBuiltinSeen)
	if err != nil {
		return err
	}
	seenBefore := len(seen)
	for _, m := range BuiltinModels() {
		old, err := db.GetModel(ctx, m.ID)
		switch {
		case err == nil:
			seen[m.ID] = true
			if old.Source == "manual" {
				continue
			}
			prev := *old
			// Built-in aliases missing from the row are added (for example
			// legacy ids xAI resolves to a current model); existing ones stay.
			aliasAdded := false
			for _, a := range m.Aliases {
				if !slices.Contains(old.Aliases, a) {
					old.Aliases = append(old.Aliases, a)
					aliasAdded = true
				}
			}
			if old.Kind == "video" && m.Kind == "video" {
				old.VideoTicksPerSec, old.VideoTiers = m.VideoTicksPerSec, m.VideoTiers
				old.InputImageTicks, old.InputVideoTicksPerSec = m.InputImageTicks, m.InputVideoTicksPerSec
			}
			FillMediaPrices(old)
			if !aliasAdded && samePrices(&prev, old) {
				continue
			}
			m = old
		case errors.Is(err, store.ErrNotFound):
			if seen[m.ID] {
				continue // deleted by the admin
			}
			seen[m.ID] = true
			m.Enabled = HasPrice(m)
		default:
			return err
		}
		if err := db.UpsertModel(ctx, m); err != nil {
			return err
		}
	}
	if len(seen) != seenBefore {
		return saveMetaSet(ctx, db, MetaBuiltinSeen, seen)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
