package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

const clientTokenPrefix = "sk-xr-"

func tokenJSON(t *store.Token) map[string]any {
	var remaining any
	if t.Limited {
		remaining = usd(max(t.QuotaTicks-t.UsedTicks, 0))
	}
	expired := t.ExpiresAt > 0 && time.Now().UnixMilli() >= t.ExpiresAt
	return map[string]any{
		"id": t.ID, "name": t.Name, "prefix": t.Prefix, "enabled": t.Enabled,
		// unlimited is its own state; when false, quota_usd applies even at 0
		// (a $0 quota blocks every request).
		"unlimited": !t.Limited, "quota_usd": usd(t.QuotaTicks), "used_usd": usd(t.UsedTicks), "remaining_usd": remaining,
		"exhausted":  t.Limited && t.UsedTicks >= t.QuotaTicks,
		"expires_at": msTime(t.ExpiresAt), "expired": expired, "allowed_models": t.AllowedModels,
		"requests": t.Requests, "last_used_at": msTime(t.LastUsedAt), "notes": t.Notes,
		"created_at": msTime(t.CreatedAt), "updated_at": msTime(t.UpdatedAt),
		"spent_usd": usd(t.UsedTicks), // deprecated: equals used_usd since usage can no longer be reset
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
	// The quota given at creation is the token's first quota record; without
	// one the token is unlimited. A limited quota of 0 is allowed (it blocks).
	if in.QuotaUSD != nil {
		if *in.QuotaUSD < 0 {
			writeErr(w, http.StatusBadRequest, "invalid_token", "quota_usd must be >= 0")
			return
		}
		t.QuotaTicks, t.Limited = billing.USDToTicks(*in.QuotaUSD), true
	}
	raw := secret.NewToken(clientTokenPrefix)
	t.Hash, t.Prefix = secret.Hash(raw), secret.Display(raw)
	e := s.newAudit(r, "token.quota_initial")
	if err := s.DB.CreateTokenRecorded(r.Context(), t, e); err != nil {
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
	// The quota is set when a token is created and afterwards only adjusted
	// (POST /tokens/{id}/quota), which keeps a record of every change; usage
	// is never reset.
	if in.QuotaUSD != nil || in.ResetUsage {
		writeErr(w, http.StatusBadRequest, "quota_is_adjustment_only",
			"the quota can only be increased or decreased (with a reason, and a record) through POST /api/v1/tokens/{id}/quota; usage cannot be reset")
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
	if fresh, err := s.DB.GetToken(r.Context(), t.ID); err == nil {
		t = fresh
	}
	writeJSON(w, http.StatusOK, tokenJSON(t))
}

// adjustQuota applies one quota adjustment (increase, decrease, switch to
// limited or unlimited) with a required reason. The ledger's lock is held
// across the change so no request is admitted or billed against the old
// quota, and the change and its audit record are one transaction.
func (s *Server) adjustQuota(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in struct {
		Action    string   `json:"action"`
		AmountUSD *float64 `json:"amount_usd"`
		Reason    string   `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	in.Reason = strings.TrimSpace(in.Reason)
	switch {
	case in.Reason == "":
		writeErr(w, http.StatusBadRequest, "reason_required", "a reason is required for every quota adjustment")
		return
	case utf8.RuneCountInString(in.Reason) > 200:
		writeErr(w, http.StatusBadRequest, "invalid_quota", "the reason must be at most 200 characters")
		return
	case in.Action != store.QuotaSetUnlimited && in.AmountUSD == nil:
		writeErr(w, http.StatusBadRequest, "invalid_quota", "amount_usd is required for "+in.Action)
		return
	}
	var amount int64
	if in.AmountUSD != nil {
		if v := *in.AmountUSD * 1e4; v != float64(int64(v)) {
			writeErr(w, http.StatusBadRequest, "invalid_quota", "amount_usd has at most 4 decimal places")
			return
		}
		amount = billing.USDToTicks(*in.AmountUSD)
	}
	e := s.newAudit(r, "token.quota_"+in.Action)
	e.Detail = map[string]any{"reason": in.Reason}
	var qc store.QuotaChange
	err := s.Quota.Adjust(id, func(used func(int64) int64) (int64, bool, error) {
		var err error
		qc, err = s.DB.AdjustTokenQuota(r.Context(), id, in.Action, amount, used, e)
		return qc.AfterTicks, qc.AfterUnlimited, err
	})
	var qerr store.ErrQuotaInvalid
	switch {
	case errors.As(err, &qerr):
		msg := qerr.Reason
		if in.Action == store.QuotaDecrease && !qc.BeforeUnlimited && amount > 0 {
			msg += fmt.Sprintf(" (at most $%.4f can be removed; $%.4f is used)",
				billing.TicksToUSD(max(qc.BeforeTicks-qc.UsedTicks, 0)), billing.TicksToUSD(qc.UsedTicks))
		}
		writeErr(w, http.StatusBadRequest, "invalid_quota", msg)
		return
	case err != nil:
		s.storeErr(w, err, "token")
		return
	}
	if o := opFrom(r); o != nil {
		o.Recorded = true
	}
	t, err := s.DB.GetToken(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "token")
		return
	}
	out := tokenJSON(t)
	out["record"] = quotaRecordJSON(e)
	writeJSON(w, http.StatusOK, out)
}

// quotaHistory lists a token's quota records, newest first.
func (s *Server) quotaHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	p := paging(r)
	f := store.EventFilter{Kinds: []string{store.EventAudit}, Action: "token.quota_", TargetType: "token",
		TargetID: strconv.FormatInt(id, 10), Limit: p.size, Offset: p.offset()}
	total, err := s.DB.CountEvents(r.Context(), f)
	if err != nil {
		s.internalErr(w, err)
		return
	}
	evs, err := s.DB.ListEvents(r.Context(), f)
	if err != nil {
		s.internalErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		out = append(out, quotaRecordJSON(e))
	}
	resp := pageJSON(out, int(total), p)
	// The amounts of all records add up to the stored quota.
	var sum int64
	all, _ := s.DB.ListEvents(r.Context(), store.EventFilter{Kinds: f.Kinds, Action: f.Action, TargetType: f.TargetType,
		TargetID: f.TargetID, Limit: 1 << 30})
	for _, e := range all {
		sum += ticksOf(e.Detail["amount_ticks"])
	}
	resp["granted_total_usd"] = usd(sum)
	writeJSON(w, http.StatusOK, resp)
}

// quotaRecordJSON is the API shape of one quota record.
func quotaRecordJSON(e *store.Event) map[string]any {
	d := e.Detail
	return map[string]any{"id": e.ID, "created_at": msTime(e.CreatedAt), "action": strings.TrimPrefix(e.Action, "token.quota_"),
		"amount_usd": usd(ticksOf(d["amount_ticks"])), "before_usd": usd(ticksOf(d["before_ticks"])),
		"after_usd": usd(ticksOf(d["after_ticks"])), "used_usd": usd(ticksOf(d["used_ticks"])),
		"unlimited": d["unlimited"] == true, "before_unlimited": d["before_unlimited"] == true, "reason": d["reason"],
		"reason_code": d["reason_code"], "actor": e.Actor, "actor_name": e.ActorName, "ip": e.IP}
}

// ticksOf reads a tick amount from decoded JSON (float64) or Go values.
func ticksOf(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
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
