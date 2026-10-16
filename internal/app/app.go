// Package app wires the gateway together.
package app

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kiyliy/xai-router/internal/admin"
	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/keytest"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/proxy"
	"github.com/Kiyliy/xai-router/internal/quota"
	"github.com/Kiyliy/xai-router/internal/reqlog"
	"github.com/Kiyliy/xai-router/internal/store"
)

// Options configure an App.
type Options struct {
	DBPath        string // ":memory:" for tests
	AdminPassword string
	Log           *slog.Logger
	Build         admin.BuildInfo
	StaticFS      fs.FS // admin console files; nil disables the UI
	// HTTPClient overrides the upstream client (tests).
	HTTPClient *http.Client
}

// App is a running gateway instance.
type App struct {
	DB       *store.DB
	Settings *config.Holder
	Pool     *pool.Pool
	Prices   *billing.Table
	Logs     *reqlog.Writer
	Quota    *quota.Ledger
	Admin    *admin.Server
	Proxy    *proxy.Handler
	Log      *slog.Logger
	// GeneratedPassword is set when a random admin password was created.
	GeneratedPassword string

	handler http.Handler
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

// New opens the database and builds all components. Call Start to run the
// background workers and Close to shut down.
func New(ctx context.Context, o Options) (*App, error) {
	log := o.Log
	if log == nil {
		log = slog.Default()
	}
	db, err := store.Open(o.DBPath)
	if err != nil {
		return nil, err
	}
	a := &App{DB: db, Log: log}
	settings, err := admin.LoadSettings(ctx, db)
	if err != nil {
		log.Warn("stored settings are invalid; using defaults", "err", err)
	}
	a.Settings = config.NewHolder(settings)

	if a.GeneratedPassword, err = admin.EnsurePassword(ctx, db, o.AdminPassword); err != nil {
		db.Close()
		return nil, err
	}
	if err := billing.SeedBuiltin(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	a.Prices = billing.NewTable()
	if err := a.Prices.Reload(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	a.Pool = pool.New(db, a.Settings, log)
	if err := a.Pool.Reload(ctx); err != nil {
		db.Close()
		return nil, err
	}

	client := o.HTTPClient
	if client == nil {
		client = &http.Client{
			Transport: newTransport(a.Settings),
			// The gateway is transparent: hand redirects back to the client.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	a.Logs = reqlog.NewWriter(db, a.Settings, log, 10000)
	a.Quota = quota.New()
	a.Proxy = &proxy.Handler{DB: db, Pool: a.Pool, Prices: a.Prices, Logs: a.Logs, Settings: a.Settings,
		Client: client, Log: log, Quota: a.Quota}
	tester := &keytest.Tester{DB: db, Pool: a.Pool, Prices: a.Prices, Settings: a.Settings, Client: client, Log: log}
	a.Pool.Verify = tester.Verify
	a.Admin = &admin.Server{DB: db, Pool: a.Pool, Prices: a.Prices, Settings: a.Settings, Logs: a.Logs, Log: log,
		Quota:  a.Quota,
		Syncer: &billing.Syncer{DB: db, Table: a.Prices, Client: client}, Tester: tester,
		Build: o.Build, Started: time.Now()}
	a.handler = a.routes(o.StaticFS)
	return a, nil
}

func newTransport(h *config.Holder) *http.Transport {
	t := proxy.NewTransport(15 * time.Second)
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := net.Dialer{Timeout: time.Duration(h.Get().ConnectTimeoutSeconds) * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, network, addr)
	}
	return t
}

// Handler returns the root HTTP handler.
func (a *App) Handler() http.Handler { return a.handler }

func (a *App) routes(static fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("/api/", a.Admin.Handler())
	mux.Handle("/v1/", a.Proxy)
	mux.Handle("/v2/", a.Proxy)

	pp := http.NewServeMux()
	pp.HandleFunc("/debug/pprof/", pprof.Index)
	pp.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	pp.HandleFunc("/debug/pprof/profile", pprof.Profile)
	pp.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	pp.HandleFunc("/debug/pprof/trace", pprof.Trace)
	protected := a.Admin.RequireAuth(pp)
	mux.HandleFunc("/debug/", func(w http.ResponseWriter, r *http.Request) {
		if !a.Settings.Get().ExposeDebug {
			http.NotFound(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})

	if static != nil {
		files := http.FileServerFS(static)
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// No directory listings (only "/" serves index.html).
			if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
				http.NotFound(w, r)
				return
			}
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "same-origin")
			if r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, ".html") {
				h.Set("Cache-Control", "no-cache")
			}
			h.Set("Content-Security-Policy", consoleCSP)
			// Set types explicitly: the OS MIME table (e.g. the Windows
			// registry) may map .js to text/plain, which breaks ES modules.
			if ct, ok := staticTypes[filepath.Ext(r.URL.Path)]; ok {
				h.Set("Content-Type", ct)
			}
			files.ServeHTTP(w, r)
		})
	}
	return a.accessLog(mux)
}

// The console only loads its own files (Swagger UI is vendored).
const consoleCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; " +
	"base-uri 'self'; form-action 'self'; object-src 'none'"

var staticTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".js":   "text/javascript; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".yaml": "application/yaml; charset=utf-8",
	".yml":  "application/yaml; charset=utf-8",
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (a *App) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		p := r.URL.Path
		lvl := slog.LevelDebug
		if strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/v2/") {
			lvl = slog.LevelInfo
		}
		a.Log.Log(r.Context(), lvl, "http", "method", r.Method, "path", p, "status", sw.status,
			"duration_ms", time.Since(start).Milliseconds(), "request_id", w.Header().Get("X-Request-Id"))
	})
}

// Start runs the background workers.
func (a *App) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.wg.Add(3)
	go func() { defer a.wg.Done(); a.Logs.Run(ctx) }()
	go func() {
		defer a.wg.Done()
		(&reqlog.Janitor{DB: a.DB, Settings: a.Settings, Log: a.Log}).Run(ctx)
	}()
	go func() { defer a.wg.Done(); a.syncLoop(ctx) }()
}

// syncLoop syncs models and prices from xAI on the configured interval.
func (a *App) syncLoop(ctx context.Context) {
	t := time.NewTimer(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		a.Quota.ExpireHolds(time.Now())
		if h := a.Settings.Get().ModelSyncIntervalHours; h > 0 && a.syncDue(ctx, time.Duration(h)*time.Hour) {
			if _, err := a.Admin.RunSync(ctx, false); err != nil {
				a.Log.Debug("scheduled model sync skipped", "reason", err)
			}
		}
		t.Reset(10 * time.Minute)
	}
}

func (a *App) syncDue(ctx context.Context, every time.Duration) bool {
	raw, _ := a.DB.GetMeta(ctx, admin.MetaLastSync)
	if raw == "" {
		return true
	}
	var last billing.SyncResult
	if json.Unmarshal([]byte(raw), &last) != nil {
		return true
	}
	if !last.OK { // retry failed syncs sooner
		return time.Since(last.At) > 30*time.Minute
	}
	return time.Since(last.At) >= every
}

// Close stops workers, flushes logs and closes the database.
func (a *App) Close() error {
	if a.cancel != nil {
		a.cancel()
		a.wg.Wait()
	} else {
		_ = a.Logs.Flush(context.Background())
	}
	return a.DB.Close()
}
