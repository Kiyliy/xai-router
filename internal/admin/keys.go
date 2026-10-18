package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/keytest"
	"github.com/Kiyliy/xai-router/internal/reqlog"
	"github.com/Kiyliy/xai-router/internal/store"
)

func (s *Server) keyJSON(k *store.Key) map[string]any {
	rs := s.Pool.State(k.ID)
	status := "active"
	reason := k.StatusReason
	var cooldown *time.Time
	switch {
	case !k.Enabled:
		status = "disabled"
		reason = "disabled by admin"
	case k.AutoDisabled || rs.AutoDisabled:
		status = "disabled"
		if rs.Reason != "" {
			reason = rs.Reason
		}
	case !rs.CooldownUntil.IsZero():
		status = "cooldown"
		t := rs.CooldownUntil.UTC()
		cooldown = &t
		if rs.Reason != "" {
			reason = rs.Reason
		}
	default:
		reason = ""
	}
	var lastTest any
	if k.LastTest != "" {
		var lt keytest.Result
		if json.Unmarshal([]byte(k.LastTest), &lt) == nil {
			lt.KeyName = k.Name // the key may have been renamed since
			lastTest = lt
		}
	}
	return map[string]any{
		"id": k.ID, "name": k.Name, "key_masked": reqlog.MaskSecret(k.Key), "base_url": k.BaseURL,
		"priority": k.Priority, "weight": k.Weight, "enabled": k.Enabled, "status": status,
		"status_reason": reason, "auto_disabled": k.AutoDisabled || rs.AutoDisabled, "cooldown_until": cooldown,
		"max_concurrency": k.MaxConcurrency, "rpm_limit": k.RPMLimit, "inflight": rs.Inflight,
		// requests/errors are kept for compatibility; they count upstream
		// attempts, like attempts/attempt_errors.
		"requests": k.Attempts, "errors": k.AttemptErrors, "attempts": k.Attempts, "attempt_errors": k.AttemptErrors,
		"cost_usd":     usd(k.CostTicks),
		"last_used_at": msTime(k.LastUsedAt), "last_error": k.LastError, "last_test": lastTest,
		"notes": k.Notes, "created_at": msTime(k.CreatedAt), "updated_at": msTime(k.UpdatedAt),
	}
}

type keyInput struct {
	Name           *string `json:"name"`
	Key            *string `json:"key"`
	BaseURL        *string `json:"base_url"`
	Priority       *int    `json:"priority"`
	Weight         *int    `json:"weight"`
	Enabled        *bool   `json:"enabled"`
	MaxConcurrency *int    `json:"max_concurrency"`
	RPMLimit       *int    `json:"rpm_limit"`
	Notes          *string `json:"notes"`
}

func (in *keyInput) apply(k *store.Key, s *config.Settings) string {
	if in.Name != nil {
		k.Name = strings.TrimSpace(*in.Name)
	}
	if in.Key != nil {
		k.Key = strings.TrimSpace(*in.Key)
	}
	if in.BaseURL != nil {
		k.BaseURL = strings.TrimRight(strings.TrimSpace(*in.BaseURL), "/")
	}
	if in.Priority != nil {
		k.Priority = *in.Priority
	}
	if in.Weight != nil {
		k.Weight = *in.Weight
	}
	if in.Enabled != nil {
		k.Enabled = *in.Enabled
	}
	if in.MaxConcurrency != nil {
		k.MaxConcurrency = *in.MaxConcurrency
	}
	if in.RPMLimit != nil {
		k.RPMLimit = *in.RPMLimit
	}
	if in.Notes != nil {
		k.Notes = *in.Notes
	}
	switch {
	case k.Key == "" || strings.ContainsAny(k.Key, " \t\r\n"):
		return "key is required and must not contain whitespace"
	case k.Weight < 1 || k.Weight > 1000:
		return "weight must be between 1 and 1000"
	case k.MaxConcurrency < 0 || k.RPMLimit < 0:
		return "limits must be >= 0"
	}
	if u, err := url.Parse(k.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "base_url must be an http(s) URL"
	}
	if err := s.UpstreamPolicy().CheckURL(k.BaseURL); err != nil {
		return "base_url: " + err.Error()
	}
	if k.Name == "" {
		k.Name = "key-" + lastN(k.Key, 4)
	}
	return ""
}

func lastN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	p := paging(r)
	q := r.URL.Query()
	keys, total, err := s.DB.ListKeys(r.Context(), store.KeyFilter{Q: q.Get("q"), Status: q.Get("status"),
		Limit: p.size, Offset: p.offset()})
	if err != nil {
		s.internalErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.keyJSON(k))
	}
	writeJSON(w, http.StatusOK, pageJSON(out, total, p))
}

func (s *Server) getKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	k, err := s.DB.GetKey(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "key")
		return
	}
	writeJSON(w, http.StatusOK, s.keyJSON(k))
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var in keyInput
	if !decode(w, r, &in) {
		return
	}
	k := &store.Key{BaseURL: s.Settings.Get().UpstreamBaseURL, Weight: 1, Enabled: true}
	if msg := in.apply(k, s.Settings.Get()); msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_key", msg)
		return
	}
	if err := s.DB.CreateKey(r.Context(), k); err != nil {
		s.storeErr(w, err, "key")
		return
	}
	s.reloadPool(r)
	writeJSON(w, http.StatusCreated, s.keyJSON(k))
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in keyInput
	if !decode(w, r, &in) {
		return
	}
	k, err := s.DB.GetKey(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "key")
		return
	}
	wasEnabled := k.Enabled
	if msg := in.apply(k, s.Settings.Get()); msg != "" {
		writeErr(w, http.StatusBadRequest, "invalid_key", msg)
		return
	}
	if !wasEnabled && k.Enabled { // re-enabling by hand also clears automatic state
		k.AutoDisabled, k.StatusReason, k.CooldownUntil = false, "", 0
	}
	if err := s.DB.UpdateKey(r.Context(), k); err != nil {
		s.storeErr(w, err, "key")
		return
	}
	if !wasEnabled && k.Enabled {
		_ = s.Pool.ResetKey(r.Context(), k.ID)
	}
	s.reloadPool(r)
	writeJSON(w, http.StatusOK, s.keyJSON(k))
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	k, err := s.DB.GetKey(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "key")
		return
	}
	if err := s.DB.DeleteKey(r.Context(), id, reqlog.MaskSecret(k.Key)); err != nil {
		s.storeErr(w, err, "key")
		return
	}
	s.reloadPool(r)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) resetKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	k, err := s.DB.GetKey(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "key")
		return
	}
	if err := s.Pool.ResetKey(r.Context(), id); err != nil {
		s.internalErr(w, err)
		return
	}
	if !k.Enabled {
		k.Enabled = true
		k.AutoDisabled, k.StatusReason, k.CooldownUntil = false, "", 0
		if err := s.DB.UpdateKey(r.Context(), k); err != nil {
			s.internalErr(w, err)
			return
		}
	}
	s.reloadPool(r)
	k, _ = s.DB.GetKey(r.Context(), id)
	writeJSON(w, http.StatusOK, s.keyJSON(k))
}

func (s *Server) testKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	in := struct {
		Probe *bool `json:"probe"`
	}{}
	if !decode(w, r, &in) {
		return
	}
	k, err := s.DB.GetKey(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "key")
		return
	}
	full := in.Probe == nil || *in.Probe
	res := s.Tester.Test(r.Context(), k, full)
	s.reloadPool(r)
	if o := opFrom(r); o != nil {
		mode := "quick"
		if full {
			mode = "full"
		}
		o.Detail = map[string]any{"mode": mode, "result": map[string]any{"ok": res.OK, "status": res.Status, "message": res.Message}}
		if !res.OK {
			o.Level = store.LevelWarn
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) testKeys(w http.ResponseWriter, r *http.Request) {
	in := struct {
		IDs   []int64 `json:"ids"`
		Probe *bool   `json:"probe"`
		Async bool    `json:"async"`
	}{}
	if !decode(w, r, &in) {
		return
	}
	all, err := s.DB.ListAllKeys(r.Context())
	if err != nil {
		s.internalErr(w, err)
		return
	}
	keys := all
	if len(in.IDs) > 0 {
		want := map[int64]bool{}
		for _, id := range in.IDs {
			want[id] = true
		}
		keys = keys[:0:0]
		for _, k := range all {
			if want[k.ID] {
				keys = append(keys, k)
			}
		}
	}
	probe := in.Probe == nil || *in.Probe
	if in.Async {
		done := s.jobAudit(r, "key.test_many", "key", map[string]any{"ids": in.IDs, "full": probe})
		j, _ := s.jobs.start("test_keys", func(ctx context.Context, j *Job) (any, error) {
			s.jobs.update(j, func(j *Job) { j.Total = int64(len(keys)) })
			res := s.Tester.TestManyProgress(ctx, keys, probe, func(done int) {
				s.jobs.update(j, func(j *Job) { j.Progress = int64(done) })
			})
			if err := s.Pool.Reload(ctx); err != nil {
				s.Log.Error("reload pool", "err", err)
			}
			done(testSummary(res), nil)
			return map[string]any{"results": res}, nil
		})
		writeJSON(w, http.StatusAccepted, j)
		return
	}
	res := s.Tester.TestMany(r.Context(), keys, probe)
	s.reloadPool(r)
	if o := opFrom(r); o != nil {
		o.Detail = map[string]any{"result": testSummary(res)}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": res})
}

func (s *Server) importKeys(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Keys       string  `json:"keys"`
		BaseURL    string  `json:"base_url"`
		Priority   int     `json:"priority"`
		Weight     int     `json:"weight"`
		NamePrefix string  `json:"name_prefix"`
		Test       *bool   `json:"test"`
		Probe      bool    `json:"probe"`
		Notes      *string `json:"notes"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.BaseURL == "" {
		in.BaseURL = s.Settings.Get().UpstreamBaseURL
	}
	if in.Weight == 0 {
		in.Weight = 1
	}
	if in.NamePrefix == "" {
		in.NamePrefix = "key"
	}
	type skipped struct {
		Line      int    `json:"line"`
		KeyMasked string `json:"key_masked"`
		Reason    string `json:"reason"`
	}
	created := []*store.Key{}
	skips := []skipped{}
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(in.Keys, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, key := "", line
		if j := strings.LastIndexAny(line, ",\t"); j >= 0 {
			name, key = strings.TrimSpace(line[:j]), strings.TrimSpace(line[j+1:])
		}
		if seen[key] {
			skips = append(skips, skipped{i + 1, reqlog.MaskSecret(key), "duplicate in input"})
			continue
		}
		seen[key] = true
		if name == "" {
			name = in.NamePrefix + "-" + lastN(key, 4)
		}
		k := &store.Key{Enabled: true}
		ki := keyInput{Name: &name, Key: &key, BaseURL: &in.BaseURL, Priority: &in.Priority, Weight: &in.Weight, Notes: in.Notes}
		if msg := ki.apply(k, s.Settings.Get()); msg != "" {
			skips = append(skips, skipped{i + 1, reqlog.MaskSecret(key), msg})
			continue
		}
		if err := s.DB.CreateKey(r.Context(), k); err != nil {
			reason := err.Error()
			if err == store.ErrConflict {
				reason = "already in the pool"
			}
			skips = append(skips, skipped{i + 1, reqlog.MaskSecret(key), reason})
			continue
		}
		created = append(created, k)
	}
	s.reloadPool(r)
	tests := []keytest.Result{}
	if (in.Test == nil || *in.Test) && len(created) > 0 {
		tests = s.Tester.TestMany(r.Context(), created, in.Probe)
		s.reloadPool(r)
	}
	out := make([]map[string]any, 0, len(created))
	for _, k := range created {
		if fresh, err := s.DB.GetKey(r.Context(), k.ID); err == nil {
			k = fresh
		}
		out = append(out, s.keyJSON(k))
	}
	if o := opFrom(r); o != nil { // one summary event for the whole import
		o.Detail = map[string]any{"result": map[string]any{"created": len(out), "skipped": len(skips)}}
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": out, "skipped": skips, "tests": tests})
}

func (s *Server) reloadPool(r *http.Request) {
	if err := s.Pool.Reload(r.Context()); err != nil {
		s.Log.Error("reload pool", "err", err)
	}
}

// maskKey masks an upstream key for display and events.
func maskKey(k string) string { return reqlog.MaskSecret(k) }
