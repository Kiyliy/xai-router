package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/store"
)

// LoadSettings reads saved settings on top of the defaults.
func LoadSettings(ctx context.Context, db *store.DB) (config.Settings, error) {
	s := config.DefaultSettings()
	raw, err := db.GetMeta(ctx, MetaSettings)
	if err != nil || raw == "" {
		return s, err
	}
	// Unknown (removed) fields are ignored so old databases keep working.
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return config.DefaultSettings(), err
	}
	if err := s.Validate(); err != nil {
		return config.DefaultSettings(), err
	}
	return s, nil
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Settings.Get())
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		return
	}
	next, err := s.Settings.Get().Merge(body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_settings", err.Error())
		return
	}
	b, _ := json.Marshal(next)
	if err := s.DB.SetMeta(r.Context(), MetaSettings, string(b)); err != nil {
		s.internalErr(w, err)
		return
	}
	s.Settings.Set(next)
	if s.OnSettingsChanged != nil {
		s.OnSettingsChanged(next)
	}
	s.Log.Info("settings updated")
	writeJSON(w, http.StatusOK, s.Settings.Get())
}
