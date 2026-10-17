package admin

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

const clientTokenPrefix = "sk-xr-"

func tokenJSON(t *store.Token) map[string]any {
	var remaining any
	if t.QuotaTicks > 0 {
		remaining = usd(max(t.QuotaTicks-t.UsedTicks, 0))
	}
	expired := t.ExpiresAt > 0 && time.Now().UnixMilli() >= t.ExpiresAt
	return map[string]any{
		"id": t.ID, "name": t.Name, "prefix": t.Prefix, "enabled": t.Enabled,
		"quota_usd": usd(t.QuotaTicks), "used_usd": usd(t.UsedTicks), "remaining_usd": remaining,
		"expires_at": msTime(t.ExpiresAt), "expired": expired, "allowed_models": t.AllowedModels,
		"requests": t.Requests, "last_used_at": msTime(t.LastUsedAt), "notes": t.Notes,
		"created_at": msTime(t.CreatedAt), "updated_at": msTime(t.UpdatedAt),
	}
}

type tokenInput struct {
	Name          *string         `json:"name"`
	Enabled       *bool           `json:"enabled"`
	QuotaUSD      *float64        `json:"quota_usd"`
	ExpiresAt     json.RawMessage `json:"expires_at"`
	AllowedModels *[]string       `json:"allowed_models"`
	Notes         *string         `json:"notes"`
	ResetUsage    bool            `json:"reset_usage"`
}

func (in *tokenInput) apply(t *store.Token) string {
	if in.Name != nil {
		t.Name = strings.TrimSpace(*in.Name)
	}
	if in.Enabled != nil {
		t.Enabled = *in.Enabled
	}
	if in.QuotaUSD != nil {
		if *in.QuotaUSD < 0 {
			return "quota_usd must be >= 0"
		}
		t.QuotaTicks = billing.USDToTicks(*in.QuotaUSD)
	}
	if len(in.ExpiresAt) > 0 {
		var v *string
		if err := json.Unmarshal(in.ExpiresAt, &v); err != nil {
			return "expires_at must be an RFC 3339 string or null"
		}
		if v == nil || *v == "" {
			t.ExpiresAt = 0
		} else {
			ms, err := parseTimeParam(*v)
			if err != nil {
				return err.Error()
			}
			t.ExpiresAt = ms
		}
	}
	if in.AllowedModels != nil {
		var ms []string
		for _, m := range *in.AllowedModels {
			if m = strings.TrimSpace(m); m != "" {
				ms = append(ms, m)
			}
		}
		t.AllowedModels = ms
	}
	if in.Notes != nil {
		t.Notes = *in.Notes
	}
	if in.ResetUsage {
		t.UsedTicks = 0
	}
	if t.Name == "" {
		return "name is required"
	}
	return ""
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	p := paging(r)
	status := r.URL.Query().Get("status")
	if !store.ValidTokenStatus(status) {
		writeErr(w, http.StatusBadRequest, "invalid_status", "status must be enabled, disabled, expired or exhausted")
		return
	}
	ts, total, err := s.DB.ListTokensByStatus(r.Context(), r.URL.Query().Get("q"), status, p.size, p.offset())
	if err != nil {
		s.internalErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, tokenJSON(t))
	}
	writeJSON(w, http.StatusOK, pageJSON(out, total, p))
}

func (s *Server) getToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	t, err := s.DB.GetToken(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "token")
		return
	}
	writeJSON(w, http.StatusOK, tokenJSON(t))
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in tokenInput
	if !decode(w, r, &in) {
		return
	}
	t := &store.Token{Enabled: true}
	if msg := in.apply(t); msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_token", msg)
		return
	}
	raw := secret.NewToken(clientTokenPrefix)
	t.Hash, t.Prefix = secret.Hash(raw), secret.Display(raw)
	if err := s.DB.CreateToken(r.Context(), t); err != nil {
		s.storeErr(w, err, "token")
		return
	}
	out := tokenJSON(t)
	out["token"] = raw
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) updateToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in tokenInput
	if !decode(w, r, &in) {
		return
	}
	t, err := s.DB.GetToken(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "token")
		return
	}
	if msg := in.apply(t); msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_token", msg)
		return
	}
	if err := s.DB.UpdateToken(r.Context(), t); err != nil {
		s.storeErr(w, err, "token")
		return
	}
	if in.ResetUsage {
		if err := s.DB.ResetTokenUsage(r.Context(), t.ID); err != nil {
			s.internalErr(w, err)
			return
		}
		s.Quota.Reset(t.ID)
	}
	if fresh, err := s.DB.GetToken(r.Context(), t.ID); err == nil {
		t = fresh
	}
	writeJSON(w, http.StatusOK, tokenJSON(t))
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.DB.DeleteToken(r.Context(), id); err != nil {
		s.storeErr(w, err, "token")
		return
	}
	s.Quota.Invalidate(id)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) regenerateToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	t, err := s.DB.GetToken(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "token")
		return
	}
	raw := secret.NewToken(clientTokenPrefix)
	t.Hash, t.Prefix = secret.Hash(raw), secret.Display(raw)
	if err := s.DB.UpdateToken(r.Context(), t); err != nil {
		s.storeErr(w, err, "token")
		return
	}
	out := tokenJSON(t)
	out["token"] = raw
	writeJSON(w, http.StatusOK, out)
}
