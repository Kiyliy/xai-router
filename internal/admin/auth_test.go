package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

func (h *harness) login(pw string) result {
	h.t.Helper()
	return h.callAs("", http.MethodPost, "/api/v1/auth/login", `{"password":"`+pw+`"}`)
}

func TestRequireAuth(t *testing.T) {
	h := newHarness(t)
	for _, tok := range []string{"", "nope", "xra-nope"} {
		r := h.callAs(tok, http.MethodGet, "/api/v1/keys", "")
		if r.status != 401 || r.code(t) != "unauthorized" {
			t.Fatalf("token %q: %d %s", tok, r.status, r.body)
		}
	}
	r := h.callAs("", http.MethodGet, "/api/v1/keys", "", "Authorization: Basic abc")
	if r.status != 401 {
		t.Fatalf("basic auth accepted: %d", r.status)
	}
	me := h.must(h.call(http.MethodGet, "/api/v1/auth/me", ""), 200)
	if me["type"] != "admin_token" || me["name"] != "ci" || me["expires_at"] != nil {
		t.Fatalf("me: %v", me)
	}
}

func TestErrorFormatAndRoutes(t *testing.T) {
	h := newHarness(t)
	r := h.call(http.MethodGet, "/api/v1/does-not-exist", "")
	if r.status != 404 || r.code(t) != "not_found" || !strings.Contains(r.body, "GET /api/v1/does-not-exist") {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	if r.header.Get("Cache-Control") != "no-store" || r.header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(r.header.Get("Content-Type"), "application/json") {
		t.Fatalf("headers: %v", r.header)
	}
	public := 0
	for _, rt := range h.s.Routes() {
		if rt.Public {
			public++
			if rt.Pattern != "POST /api/v1/auth/login" {
				t.Fatalf("unexpected public route %s", rt.Pattern)
			}
		}
	}
	if public != 1 || len(h.s.Routes()) < 30 {
		t.Fatalf("routes: %+v", h.s.Routes())
	}
	if r := h.call(http.MethodPost, "/api/v1/tokens", `{"name":"x","bogus":1}`); r.status != 400 || r.code(t) != "invalid_json" {
		t.Fatalf("unknown field: %d %s", r.status, r.body)
	}
	if r := h.call(http.MethodPost, "/api/v1/tokens", `{"name":`); r.status != 400 || r.code(t) != "invalid_json" {
		t.Fatalf("bad json: %d %s", r.status, r.body)
	}
	big := `{"name":"` + strings.Repeat("a", 17<<20) + `"}`
	if r := h.call(http.MethodPost, "/api/v1/tokens", big); r.status != 413 || r.code(t) != "too_large" {
		t.Fatalf("large body: %d", r.status)
	}
}

func TestLoginLogoutAndLimiter(t *testing.T) {
	h := newHarness(t)
	if r := h.login("wrong-password"); r.status != 401 || r.code(t) != "invalid_password" {
		t.Fatalf("wrong password: %d %s", r.status, r.body)
	}
	if r := h.login(""); r.status != 401 {
		t.Fatalf("empty password: %d", r.status)
	}
	if r := h.callAs("", http.MethodPost, "/api/v1/auth/login", `{"pw":1}`); r.status != 400 {
		t.Fatalf("bad body: %d", r.status)
	}
	out := h.must(h.login(testPassword), 200)
	sess, _ := out["token"].(string)
	if !strings.HasPrefix(sess, "xrs-") || out["expires_at"] == nil {
		t.Fatalf("login: %v", out)
	}
	me := h.must(h.callAs(sess, http.MethodGet, "/api/v1/auth/me", ""), 200)
	if me["type"] != "session" || me["expires_at"] == nil {
		t.Fatalf("me: %v", me)
	}
	h.must(h.callAs(sess, http.MethodPost, "/api/v1/auth/logout", ""), 204)
	if r := h.callAs(sess, http.MethodGet, "/api/v1/auth/me", ""); r.status != 401 {
		t.Fatalf("session survived logout: %d", r.status)
	}
	// Logging out with an admin token is a no-op.
	h.must(h.call(http.MethodPost, "/api/v1/auth/logout", ""), 204)
	h.must(h.call(http.MethodGet, "/api/v1/auth/me", ""), 200)

	for i := 0; i < loginMaxFails; i++ {
		h.s.limiter.fail("127.0.0.1")
	}
	if r := h.login(testPassword); r.status != 429 || r.code(t) != "too_many_attempts" {
		t.Fatalf("limiter: %d %s", r.status, r.body)
	}
	h.s.limiter.reset("127.0.0.1")
	if h.s.limiter.blocked("127.0.0.1") {
		t.Fatal("reset did not unblock")
	}
}

func TestChangePassword(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	cur := h.must(h.login(testPassword), 200)["token"].(string)
	other := secret.NewToken("xrs-")
	if err := h.db.CreateSession(ctx, secret.Hash(other), store.NowMs()+3600_000); err != nil {
		t.Fatal(err)
	}
	if r := h.callAs(cur, http.MethodPut, "/api/v1/auth/password", `{"old_password":"x","new_password":"short"}`); r.status != 400 ||
		r.code(t) != "weak_password" {
		t.Fatalf("weak: %d %s", r.status, r.body)
	}
	if r := h.callAs(cur, http.MethodPut, "/api/v1/auth/password", `{"old_password":"nope","new_password":"long-enough-1"}`); r.status != 401 {
		t.Fatalf("wrong old: %d %s", r.status, r.body)
	}
	h.must(h.callAs(cur, http.MethodPut, "/api/v1/auth/password",
		`{"old_password":"`+testPassword+`","new_password":"long-enough-1","revoke_admin_tokens":true}`), 204)
	if r := h.callAs(other, http.MethodGet, "/api/v1/auth/me", ""); r.status != 401 {
		t.Fatal("other sessions must be revoked")
	}
	h.must(h.callAs(cur, http.MethodGet, "/api/v1/auth/me", ""), 200)
	if r := h.call(http.MethodGet, "/api/v1/auth/me", ""); r.status != 401 {
		t.Fatal("admin tokens must be revoked")
	}
	hash, _ := h.db.GetMeta(ctx, MetaPassword)
	if !secret.CheckPassword("long-enough-1", hash) {
		t.Fatal("password not changed")
	}
	if flag, _ := h.db.GetMeta(ctx, MetaPasswordGenerated); flag != "0" {
		t.Fatalf("generated flag %q", flag)
	}
}

func TestEnsureAndResetPassword(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	gen, err := EnsurePassword(ctx, db, "")
	if err != nil || len(gen) < 16 {
		t.Fatalf("generated %q %v", gen, err)
	}
	if flag, _ := db.GetMeta(ctx, MetaPasswordGenerated); flag != "1" {
		t.Fatalf("flag %q", flag)
	}
	if again, err := EnsurePassword(ctx, db, "ignored"); err != nil || again != "" {
		t.Fatalf("second call: %q %v", again, err)
	}
	if err := db.CreateSession(ctx, "s1", store.NowMs()+1000_000); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateAdminToken(ctx, &store.AdminToken{Name: "leaked", Hash: "ah-leaked"}); err != nil {
		t.Fatal(err)
	}
	if err := ResetPassword(ctx, db, "pw-keep-tokens", true); err != nil {
		t.Fatal(err)
	}
	if ts, _ := db.ListAdminTokens(ctx); len(ts) != 1 {
		t.Fatalf("--keep-admin-tokens revoked tokens: %d left", len(ts))
	}
	if err := ResetPassword(ctx, db, "new-password-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetSession(ctx, "s1", 1000); err == nil {
		t.Fatal("sessions survived a reset")
	}
	if ts, _ := db.ListAdminTokens(ctx); len(ts) != 0 {
		t.Fatalf("admin tokens survived a reset: %d", len(ts))
	}
	hash, _ := db.GetMeta(ctx, MetaPassword)
	if !secret.CheckPassword("new-password-1", hash) {
		t.Fatal("reset password not stored")
	}
	db.Close()
	if _, err := EnsurePassword(ctx, db, "x"); err == nil {
		t.Fatal("closed database should fail")
	}
	if err := ResetPassword(ctx, db, "x", false); err == nil {
		t.Fatal("closed database should fail")
	}
}

func TestAdminTokens(t *testing.T) {
	h := newHarness(t)
	if r := h.call(http.MethodPost, "/api/v1/admin-tokens", `{"name":""}`); r.status != 400 || r.code(t) != "invalid_name" {
		t.Fatalf("got %d %s", r.status, r.body)
	}
	out := h.must(h.call(http.MethodPost, "/api/v1/admin-tokens", `{"name":"deploy"}`), 201)
	raw, _ := out["token"].(string)
	if !strings.HasPrefix(raw, "xra-") || out["prefix"] == "" || out["created_at"] == nil {
		t.Fatalf("created: %v", out)
	}
	ls := h.must(h.call(http.MethodGet, "/api/v1/admin-tokens", ""), 200)
	if num(ls["total"]) != 2 {
		t.Fatalf("list: %v", ls)
	}
	for _, x := range list(ls) {
		if _, leaked := x["token"]; leaked {
			t.Fatal("secret listed")
		}
	}
	me := h.must(h.callAs(raw, http.MethodGet, "/api/v1/auth/me", ""), 200)
	if me["name"] != "deploy" {
		t.Fatalf("me: %v", me)
	}
	id := num(out["id"])
	h.must(h.call(http.MethodDelete, "/api/v1/admin-tokens/"+itoa(id), ""), 204)
	if r := h.callAs(raw, http.MethodGet, "/api/v1/auth/me", ""); r.status != 401 {
		t.Fatal("revoked token still works")
	}
	if r := h.call(http.MethodDelete, "/api/v1/admin-tokens/"+itoa(id), ""); r.status != 404 {
		t.Fatalf("second delete: %d", r.status)
	}
	if r := h.call(http.MethodDelete, "/api/v1/admin-tokens/zero", ""); r.status != 400 || r.code(t) != "invalid_id" {
		t.Fatalf("bad id: %d", r.status)
	}
}
