// Package admin implements the /api/v1 management REST API.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/keytest"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/quota"
	"github.com/Kiyliy/xai-router/internal/reqlog"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

// Meta keys.
const (
	MetaPassword          = "admin_password"
	MetaPasswordGenerated = "admin_password_generated"
	MetaSettings          = "settings"
	MetaLastSync          = "last_model_sync"
)

const sessionTTL = 30 * 24 * time.Hour

// BuildInfo describes the running binary.
type BuildInfo struct {
	Version, Commit, Date string
}

// Server is the admin API.
type Server struct {
	DB       *store.DB
	Pool     *pool.Pool
	Prices   *billing.Table
	Syncer   *billing.Syncer
	Tester   *keytest.Tester
	Settings *config.Holder
	Logs     *reqlog.Writer
	Quota    *quota.Ledger
	Log      *slog.Logger
	Build    BuildInfo
	Started  time.Time
	// OnSettingsChanged is invoked after settings are saved.
	OnSettingsChanged func(config.Settings)

	limiter loginLimiter
	syncMu  sync.Mutex
	routes  []Route
	jobs    jobs
}

// Route describes one admin API endpoint ("METHOD /path").
type Route struct {
	Pattern string
	Public  bool // reachable without credentials
}

// Routes lists the endpoints registered by Handler (for tests and tooling).
func (s *Server) Routes() []Route { return append([]Route(nil), s.routes...) }

type ctxKey int

const credKey ctxKey = 0

type credential struct {
	Type      string // session, admin_token
	ID        string // admin token id
	Name      string
	Hash      string
	ExpiresAt int64
}

// Handler returns the router for /api/.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routes = s.routes[:0]
	pub := func(pattern string, h http.HandlerFunc) {
		s.routes = append(s.routes, Route{Pattern: pattern, Public: true})
		mux.HandleFunc(pattern, h)
	}
	priv := func(pattern string, h http.HandlerFunc) {
		s.routes = append(s.routes, Route{Pattern: pattern})
		mux.Handle(pattern, s.requireAuth(s.audit(pattern, h)))
	}

	pub("POST /api/v1/auth/login", s.login)
	priv("POST /api/v1/auth/logout", s.logout)
	priv("GET /api/v1/auth/me", s.me)
	priv("PUT /api/v1/auth/password", s.changePassword)

	priv("GET /api/v1/keys", s.listKeys)
	priv("POST /api/v1/keys", s.createKey)
	priv("POST /api/v1/keys/import", s.importKeys)
	priv("POST /api/v1/keys/test", s.testKeys)
	priv("GET /api/v1/keys/{id}", s.getKey)
	priv("PATCH /api/v1/keys/{id}", s.updateKey)
	priv("DELETE /api/v1/keys/{id}", s.deleteKey)
	priv("POST /api/v1/keys/{id}/test", s.testKey)
	priv("POST /api/v1/keys/{id}/reset", s.resetKey)

	priv("GET /api/v1/tokens", s.listTokens)
	priv("POST /api/v1/tokens", s.createToken)
	priv("GET /api/v1/tokens/{id}", s.getToken)
	priv("PATCH /api/v1/tokens/{id}", s.updateToken)
	priv("DELETE /api/v1/tokens/{id}", s.deleteToken)
	priv("POST /api/v1/tokens/{id}/regenerate", s.regenerateToken)
	priv("POST /api/v1/tokens/{id}/quota", s.adjustQuota)
	priv("GET /api/v1/tokens/{id}/quota-history", s.quotaHistory)

	priv("GET /api/v1/admin-tokens", s.listAdminTokens)
	priv("POST /api/v1/admin-tokens", s.createAdminToken)
	priv("DELETE /api/v1/admin-tokens/{id}", s.deleteAdminToken)

	priv("GET /api/v1/models", s.listModels)
	priv("POST /api/v1/models", s.createModel)
	priv("POST /api/v1/models/sync", s.syncModels)
	priv("PATCH /api/v1/models/{id}", s.updateModel)
	priv("DELETE /api/v1/models/{id}", s.deleteModel)

	priv("GET /api/v1/logs", s.listLogs)
	priv("DELETE /api/v1/logs", s.deleteLogs)
	priv("GET /api/v1/logs/{id}", s.getLog)
	priv("GET /api/v1/logs/{id}/curl", s.logCurl)

	priv("GET /api/v1/stats/overview", s.statsOverview)
	priv("GET /api/v1/stats/timeseries", s.statsTimeseries)
	priv("GET /api/v1/stats/breakdown", s.statsBreakdown)

	priv("GET /api/v1/settings", s.getSettings)
	priv("PUT /api/v1/settings", s.putSettings)
	priv("GET /api/v1/system/info", s.systemInfo)
	priv("POST /api/v1/system/compact", s.compact)
	priv("GET /api/v1/jobs", s.listJobs)
	priv("GET /api/v1/jobs/{id}", s.getJob)
	priv("GET /api/v1/events/{id}", s.getEvent)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	return securityHeaders(mux)
}

// RequireAuth exposes the auth middleware (used for /debug/pprof).
func (s *Server) RequireAuth(h http.Handler) http.Handler { return s.requireAuth(h) }

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := bearer(r)
		if raw == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing bearer token")
			return
		}
		hash := secret.Hash(raw)
		var cred *credential
		if strings.HasPrefix(raw, "xra-") {
			if t, err := s.DB.GetAdminTokenByHash(r.Context(), hash); err == nil && secret.Equal(t.Hash, hash) {
				cred = &credential{Type: "admin_token", ID: strconv.FormatInt(t.ID, 10), Name: t.Name, Hash: hash}
			}
		} else if exp, err := s.DB.GetSession(r.Context(), hash, sessionTTL.Milliseconds()); err == nil {
			cred = &credential{Type: "session", Name: "admin", Hash: hash, ExpiresAt: exp}
		}
		if cred == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "invalid or expired token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), credKey, cred)))
	})
}

func credFrom(r *http.Request) *credential {
	c, _ := r.Context().Value(credKey).(*credential)
	return c
}

func bearer(r *http.Request) string {
	a := r.Header.Get("Authorization")
	if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return ""
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (s *Server) internalErr(w http.ResponseWriter, err error) {
	s.Log.Error("admin api error", "err", err)
	writeErr(w, http.StatusInternalServerError, "internal_error", err.Error())
}

func (s *Server) storeErr(w http.ResponseWriter, err error, what string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", what+" not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", what+" already exists")
	default:
		s.internalErr(w, err)
	}
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "too_large", "request body too large")
		return false
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	return id, err == nil && id > 0
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "invalid_id", "invalid id")
		return 0, false
	}
	return id, true
}

type page struct{ page, size int }

func (p page) offset() int { return (p.page - 1) * p.size }

func paging(r *http.Request) page {
	q := r.URL.Query()
	p, _ := strconv.Atoi(q.Get("page"))
	n, _ := strconv.Atoi(q.Get("page_size"))
	if p < 1 {
		p = 1
	}
	if n < 1 {
		n = 20
	}
	if n > 200 {
		n = 200
	}
	return page{p, n}
}

func pageJSON(data any, total int, p page) map[string]any {
	return map[string]any{"data": data, "total": total, "page": p.page, "page_size": p.size}
}

// msTime converts unix ms to *time.Time for JSON (nil when zero).
func msTime(ms int64) *time.Time {
	if ms <= 0 {
		return nil
	}
	t := time.UnixMilli(ms).UTC()
	return &t
}

func parseTimeParam(v string) (int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n > 1e12 { // already ms
			return n, nil
		}
		return n * 1000, nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("invalid time %q", v)
}

func usd(ticks int64) float64 { return billing.TicksToUSD(ticks) }

// ---- login rate limiting ----

// loginLimiter allows loginMaxFails attempts per client per loginWindow.
// An attempt is counted when it starts (before the password is checked) and
// forgotten only after a successful login, so concurrent attempts cannot
// all pass the check before the first failure is recorded.
type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

const (
	loginWindow   = 10 * time.Minute
	loginMaxFails = 10
	// loginMaxClients bounds the map; past it expired entries are swept.
	loginMaxClients = 10_000
)

// limiterKey groups IPv6 clients by /64 (one host usually has a whole /64).
func limiterKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil || a.Is4() || a.Is4In6() {
		return ip
	}
	p, err := a.Prefix(64)
	if err != nil {
		return ip
	}
	return p.String()
}

// attempt reserves one login attempt for ip. It reports false (and counts
// nothing) when the limit is reached.
func (l *loginLimiter) attempt(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := limiterKey(ip)
	if len(l.prune(k)) >= loginMaxFails {
		return false
	}
	if l.failures == nil {
		l.failures = map[string][]time.Time{}
	}
	if len(l.failures) >= loginMaxClients {
		for other := range l.failures {
			l.prune(other)
		}
	}
	l.failures[k] = append(l.failures[k], time.Now())
	return true
}

func (l *loginLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(limiterKey(ip))) >= loginMaxFails
}

// fail records a failed attempt that was not reserved with attempt.
func (l *loginLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures == nil {
		l.failures = map[string][]time.Time{}
	}
	k := limiterKey(ip)
	l.failures[k] = append(l.prune(k), time.Now())
}

func (l *loginLimiter) reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, limiterKey(ip))
}

func (l *loginLimiter) prune(k string) []time.Time {
	f := l.failures[k]
	cut := time.Now().Add(-loginWindow)
	i := 0
	for i < len(f) && f[i].Before(cut) {
		i++
	}
	f = f[i:]
	if len(f) == 0 {
		delete(l.failures, k)
	} else if l.failures != nil {
		l.failures[k] = f
	}
	return f
}
