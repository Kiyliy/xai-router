package admin

import (
	"context"
	"net/http"
	"time"

	"github.com/Kiyliy/xai-router/internal/netutil"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

// EnsurePassword makes sure an admin password exists. If none is stored it
// uses envPassword, or generates a random one. It returns the generated
// password ("" when nothing was generated) so the caller can print it once.
func EnsurePassword(ctx context.Context, db *store.DB, envPassword string) (generated string, err error) {
	cur, err := db.GetMeta(ctx, MetaPassword)
	if err != nil || cur != "" {
		return "", err
	}
	pw := envPassword
	if pw == "" {
		pw = secret.Random(20)
		generated = pw
	}
	if err := db.SetMeta(ctx, MetaPassword, secret.HashPassword(pw)); err != nil {
		return "", err
	}
	flag := "0"
	if generated != "" {
		flag = "1"
	}
	return generated, db.SetMeta(ctx, MetaPasswordGenerated, flag)
}

// ResetPassword replaces the admin password with a generated one and revokes
// all sessions. The console then asks for a password change.
func ResetPassword(ctx context.Context, db *store.DB, pw string) error {
	if err := db.SetMeta(ctx, MetaPassword, secret.HashPassword(pw)); err != nil {
		return err
	}
	if err := db.SetMeta(ctx, MetaPasswordGenerated, "1"); err != nil {
		return err
	}
	return db.DeleteSessionsExcept(ctx, "")
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := netutil.ClientIP(r, s.Settings.Get().TrustedProxies)
	loginEvent := func(action, level, reason string) {
		e := &store.Event{Kind: store.EventAudit, Level: level, Action: action, Actor: "anonymous", IP: ip}
		if reason != "" {
			e.Detail = map[string]any{"reason": reason} // never what was typed
		} else {
			e.Actor, e.ActorName = "password", "admin"
		}
		s.recordEvent(r.Context(), e)
	}
	if s.limiter.blocked(ip) {
		loginEvent("auth.login_failed", store.LevelWarn, "locked out after too many failed attempts")
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed logins; try again in a few minutes")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	hash, err := s.DB.GetMeta(r.Context(), MetaPassword)
	if err != nil {
		s.internalErr(w, err)
		return
	}
	if in.Password == "" || !s.checkPassword(r.Context(), in.Password, hash) {
		s.limiter.fail(ip)
		s.Log.Warn("failed admin login", "ip", ip)
		reason := "wrong password"
		if in.Password == "" {
			reason = "empty password"
		}
		loginEvent("auth.login_failed", store.LevelWarn, reason)
		writeErr(w, http.StatusUnauthorized, "invalid_password", "wrong password")
		return
	}
	s.limiter.reset(ip)
	tok := secret.NewToken("xrs-")
	exp := time.Now().Add(sessionTTL)
	if err := s.DB.CreateSession(r.Context(), secret.Hash(tok), exp.UnixMilli()); err != nil {
		s.internalErr(w, err)
		return
	}
	s.Log.Info("admin login", "ip", ip)
	loginEvent("auth.login", store.LevelInfo, "")
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "expires_at": exp.UTC()})
}

// pbkdf2Slots bounds concurrent password checks (each is ~100ms of CPU), so
// a flood of login attempts cannot exhaust the machine.
var pbkdf2Slots = make(chan struct{}, 2)

func (s *Server) checkPassword(ctx context.Context, pw, hash string) bool {
	select {
	case pbkdf2Slots <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-pbkdf2Slots }()
	return secret.CheckPassword(pw, hash)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c := credFrom(r); c != nil && c.Type == "session" {
		_ = s.DB.DeleteSession(r.Context(), c.Hash)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	c := credFrom(r)
	writeJSON(w, http.StatusOK, map[string]any{"type": c.Type, "name": c.Name, "expires_at": msTime(c.ExpiresAt)})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Old          string `json:"old_password"`
		New          string `json:"new_password"`
		RevokeTokens bool   `json:"revoke_admin_tokens"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.New) < 8 {
		writeErr(w, http.StatusBadRequest, "weak_password", "new password must be at least 8 characters")
		return
	}
	hash, err := s.DB.GetMeta(r.Context(), MetaPassword)
	if err != nil {
		s.internalErr(w, err)
		return
	}
	if !s.checkPassword(r.Context(), in.Old, hash) {
		writeErr(w, http.StatusUnauthorized, "invalid_password", "current password is wrong")
		return
	}
	if err := s.DB.SetMeta(r.Context(), MetaPassword, secret.HashPassword(in.New)); err != nil {
		s.internalErr(w, err)
		return
	}
	_ = s.DB.SetMeta(r.Context(), MetaPasswordGenerated, "0")
	keep := ""
	if c := credFrom(r); c.Type == "session" {
		keep = c.Hash
	}
	if err := s.DB.DeleteSessionsExcept(r.Context(), keep); err != nil {
		s.internalErr(w, err)
		return
	}
	if in.RevokeTokens {
		if err := s.DB.DeleteAllAdminTokens(r.Context()); err != nil {
			s.internalErr(w, err)
			return
		}
	}
	s.Log.Info("admin password changed")
	w.WriteHeader(http.StatusNoContent)
}

// ---- admin API tokens ----

func adminTokenJSON(t *store.AdminToken) map[string]any {
	return map[string]any{"id": t.ID, "name": t.Name, "prefix": t.Prefix,
		"last_used_at": msTime(t.LastUsedAt), "created_at": msTime(t.CreatedAt)}
}

func (s *Server) listAdminTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := s.DB.ListAdminTokens(r.Context())
	if err != nil {
		s.internalErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, adminTokenJSON(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out, "total": len(out)})
}

func (s *Server) createAdminToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_name", "name is required")
		return
	}
	tok := secret.NewToken("xra-")
	t := &store.AdminToken{Name: in.Name, Hash: secret.Hash(tok), Prefix: secret.Display(tok)}
	if err := s.DB.CreateAdminToken(r.Context(), t); err != nil {
		s.internalErr(w, err)
		return
	}
	out := adminTokenJSON(t)
	out["token"] = tok
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) deleteAdminToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := s.DB.DeleteAdminToken(r.Context(), id); err != nil {
		s.storeErr(w, err, "admin token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
