package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/store"
)

func modelJSON(m *store.Model) map[string]any {
	tiers := make([]map[string]any, 0, len(m.ImageTiers))
	for _, t := range m.ImageTiers {
		tiers = append(tiers, map[string]any{"quality": t.Quality, "resolution": t.Resolution, "price": usd(t.Ticks)})
	}
	pm := billing.TicksToUSDPerMillion
	return map[string]any{
		"id": m.ID, "kind": m.Kind, "aliases": m.Aliases, "enabled": m.Enabled, "source": m.Source,
		"context_length": m.ContextLength,
		"input_price":    pm(m.InputTicks), "cached_input_price": pm(m.CachedTicks),
		"image_input_price": pm(m.ImageInputTicks), "output_price": pm(m.OutputTicks),
		"long_context_threshold": m.LongThreshold, "long_input_price": pm(m.LongInputTicks),
		"long_cached_input_price": pm(m.LongCachedTicks), "long_output_price": pm(m.LongOutputTicks),
		"image_price": usd(m.ImageTicks), "image_tiers": tiers, "video_price_per_second": usd(m.VideoTicksPerSec),
		"synced_at": msTime(m.SyncedAt), "updated_at": msTime(m.UpdatedAt),
	}
}

type modelInput struct {
	ID                   *string   `json:"id"`
	Kind                 *string   `json:"kind"`
	Aliases              *[]string `json:"aliases"`
	Enabled              *bool     `json:"enabled"`
	Source               *string   `json:"source"` // ignored; edits always become manual
	ContextLength        *int64    `json:"context_length"`
	InputPrice           *float64  `json:"input_price"`
	CachedInputPrice     *float64  `json:"cached_input_price"`
	ImageInputPrice      *float64  `json:"image_input_price"`
	OutputPrice          *float64  `json:"output_price"`
	LongContextThreshold *int64    `json:"long_context_threshold"`
	LongInputPrice       *float64  `json:"long_input_price"`
	LongCachedInputPrice *float64  `json:"long_cached_input_price"`
	LongOutputPrice      *float64  `json:"long_output_price"`
	ImagePrice           *float64  `json:"image_price"`
	ImageTiers           *[]struct {
		Quality    string  `json:"quality"`
		Resolution string  `json:"resolution"`
		Price      float64 `json:"price"`
	} `json:"image_tiers"`
	VideoPricePerSecond *float64        `json:"video_price_per_second"`
	SyncedAt            json.RawMessage `json:"synced_at"`  // read-only, ignored
	UpdatedAt           json.RawMessage `json:"updated_at"` // read-only, ignored
}

func (in *modelInput) apply(m *store.Model) string {
	pt := billing.USDPerMillionToTicks
	setP := func(dst *int64, v *float64) {
		if v != nil {
			*dst = pt(*v)
		}
	}
	if in.Kind != nil {
		m.Kind = *in.Kind
	}
	if in.Aliases != nil {
		m.Aliases = *in.Aliases
	}
	if in.Enabled != nil {
		m.Enabled = *in.Enabled
	}
	if in.ContextLength != nil {
		m.ContextLength = *in.ContextLength
	}
	setP(&m.InputTicks, in.InputPrice)
	setP(&m.CachedTicks, in.CachedInputPrice)
	setP(&m.ImageInputTicks, in.ImageInputPrice)
	setP(&m.OutputTicks, in.OutputPrice)
	setP(&m.LongInputTicks, in.LongInputPrice)
	setP(&m.LongCachedTicks, in.LongCachedInputPrice)
	setP(&m.LongOutputTicks, in.LongOutputPrice)
	if in.LongContextThreshold != nil {
		m.LongThreshold = *in.LongContextThreshold
	}
	if in.ImagePrice != nil {
		m.ImageTicks = billing.USDToTicks(*in.ImagePrice)
	}
	if in.ImageTiers != nil {
		m.ImageTiers = m.ImageTiers[:0:0]
		for _, t := range *in.ImageTiers {
			m.ImageTiers = append(m.ImageTiers, store.ImageTier{Quality: t.Quality, Resolution: t.Resolution,
				Ticks: billing.USDToTicks(t.Price)})
		}
	}
	if in.VideoPricePerSecond != nil {
		m.VideoTicksPerSec = billing.USDToTicks(*in.VideoPricePerSecond)
	}
	switch m.Kind {
	case "language", "image", "video", "embedding":
	default:
		return "kind must be language, image, video or embedding"
	}
	for _, v := range []int64{m.InputTicks, m.CachedTicks, m.ImageInputTicks, m.OutputTicks, m.LongInputTicks,
		m.LongCachedTicks, m.LongOutputTicks, m.ImageTicks, m.VideoTicksPerSec, m.LongThreshold} {
		if v < 0 {
			return "prices must be >= 0"
		}
	}
	m.Source = "manual"
	return ""
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	ms, err := s.DB.ListModels(r.Context(), r.URL.Query().Get("kind"))
	if err != nil {
		s.internalErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(ms))
	for _, m := range ms {
		out = append(out, modelJSON(m))
	}
	var last any
	if v, _ := s.DB.GetMeta(r.Context(), MetaLastSync); v != "" {
		var sr billing.SyncResult
		if json.Unmarshal([]byte(v), &sr) == nil {
			last = sr
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "total": len(out), "last_sync": last})
}

func (s *Server) createModel(w http.ResponseWriter, r *http.Request) {
	var in modelInput
	if !decode(w, r, &in) {
		return
	}
	if in.ID == nil || strings.TrimSpace(*in.ID) == "" {
		writeErr(w, http.StatusBadRequest, "invalid_model", "id is required")
		return
	}
	m := &store.Model{ID: strings.TrimSpace(*in.ID), Kind: "language", Enabled: true}
	if msg := in.apply(m); msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_model", msg)
		return
	}
	if err := s.DB.UpsertModel(r.Context(), m); err != nil {
		s.internalErr(w, err)
		return
	}
	s.reloadPrices(r)
	writeJSON(w, http.StatusCreated, modelJSON(m))
}

func (s *Server) updateModel(w http.ResponseWriter, r *http.Request) {
	var in modelInput
	if !decode(w, r, &in) {
		return
	}
	m, err := s.DB.GetModel(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeErr(w, err, "model")
		return
	}
	if msg := in.apply(m); msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_model", msg)
		return
	}
	if err := s.DB.UpsertModel(r.Context(), m); err != nil {
		s.internalErr(w, err)
		return
	}
	s.reloadPrices(r)
	writeJSON(w, http.StatusOK, modelJSON(m))
}

func (s *Server) deleteModel(w http.ResponseWriter, r *http.Request) {
	if err := s.DB.DeleteModel(r.Context(), r.PathValue("id")); err != nil {
		s.storeErr(w, err, "model")
		return
	}
	s.reloadPrices(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) syncModels(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OverwriteManual bool `json:"overwrite_manual"`
	}
	if !decode(w, r, &in) {
		return
	}
	res, err := s.RunSync(r.Context(), in.OverwriteManual)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "no_upstream_keys", err.Error())
		return
	}
	if !res.OK {
		writeErr(w, http.StatusBadGateway, "sync_failed", res.Error)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// RunSync syncs models using a pool key and records the result.
func (s *Server) RunSync(ctx context.Context, overwriteManual bool) (billing.SyncResult, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	k := s.Pool.AnyKey()
	if k == nil {
		return billing.SyncResult{}, errNoKey
	}
	if err := s.Settings.Get().UpstreamPolicy().CheckURL(k.BaseURL); err != nil {
		return billing.SyncResult{}, fmt.Errorf("key %q: %v", k.Name, err)
	}
	res := s.Syncer.Sync(ctx, strings.TrimRight(k.BaseURL, "/"), k.Key, overwriteManual)
	b, _ := json.Marshal(res)
	_ = s.DB.SetMeta(ctx, MetaLastSync, string(b))
	s.Log.Info("model sync", "ok", res.OK, "added", len(res.Added), "updated", len(res.Updated), "error", res.Error)
	return res, nil
}

type constErr string

func (e constErr) Error() string { return string(e) }

const errNoKey = constErr("no enabled upstream key available to sync with")

func (s *Server) reloadPrices(r *http.Request) {
	if err := s.Prices.Reload(r.Context(), s.DB); err != nil {
		s.Log.Error("reload prices", "err", err)
	}
}
