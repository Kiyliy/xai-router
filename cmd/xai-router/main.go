// Command xai-router is a gateway for the xAI (Grok) API.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kiyliy/xai-router/internal/admin"
	"github.com/Kiyliy/xai-router/internal/app"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
	"github.com/Kiyliy/xai-router/web"
)

// Set by -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

const usage = `xai-router — a gateway for the xAI (Grok) API

Usage:
  xai-router                    start the server
  xai-router reset-password     set a new random admin password and print it
  xai-router version            print version information
  xai-router healthcheck        probe /healthz of a running server (for containers)

Environment:
  XR_LISTEN          listen address (default ":8080")
  XR_DATA_DIR        data directory for the SQLite database (default "./data")
  XR_ADMIN_PASSWORD  initial admin password (only used on first start)
  XR_LOG_LEVEL       debug, info, warn or error (default "info")
  XR_LOG_FORMAT      text or json (default "text")

Everything else is configured in the web console (Settings).
`

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Printf("xai-router %s (commit %s, built %s)\n", version, commit, date)
			return
		case "reset-password":
			if err := resetPassword(); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				os.Exit(1)
			}
			return
		case "healthcheck":
			os.Exit(healthcheck())
		case "help", "--help", "-h":
			fmt.Print(usage)
			return
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
			os.Exit(2)
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newLogger(env config.Env) *slog.Logger {
	opts := &slog.HandlerOptions{Level: env.LogLevel}
	if env.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

func run() error {
	env, err := config.LoadEnv()
	if err != nil {
		return err
	}
	log := newLogger(env)
	slog.SetDefault(log)

	ctx := context.Background()
	a, err := app.New(ctx, app.Options{
		DBPath:        filepath.Join(env.DataDir, "xai-router.db"),
		AdminPassword: env.AdminPassword,
		Log:           log,
		Build:         admin.BuildInfo{Version: version, Commit: commit, Date: date},
		StaticFS:      staticFS(log),
	})
	if err != nil {
		return err
	}
	if a.GeneratedPassword != "" {
		banner := "\n" +
			"  ┌──────────────────────────────────────────────────────────┐\n" +
			"  │ xai-router: generated an admin password on first start   │\n" +
			"  │                                                          │\n" +
			fmt.Sprintf("  │     %-52s │\n", a.GeneratedPassword) +
			"  │                                                          │\n" +
			"  │ Log in to the console and change it under Settings.      │\n" +
			"  └──────────────────────────────────────────────────────────┘\n"
		fmt.Fprint(os.Stderr, banner)
	}
	a.Start()

	srv := &http.Server{
		Addr:              env.Listen,
		Handler:           a.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("xai-router listening", "addr", env.Listen, "version", version, "data_dir", env.DataDir)
		errCh <- srv.ListenAndServe()
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			a.Close()
			return err
		}
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	}
	// Stop accepting connections and let in-flight requests (including long
	// streams) finish, then flush logs and close the database.
	// Longer than the proxy's drain of abandoned streams (60 s), so their
	// usage is still logged and billed.
	shutCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Warn("graceful shutdown timed out", "err", err)
	}
	// WebSocket sessions are hijacked connections that Shutdown does not
	// track: close them and wait for their logs.
	a.Proxy.CloseWebSockets(shutCtx)
	if err := a.Close(); err != nil {
		return err
	}
	log.Info("stopped")
	return nil
}

func resetPassword() error {
	env, err := config.LoadEnv()
	if err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(env.DataDir, "xai-router.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	pw := secret.Random(20)
	if err := admin.ResetPassword(context.Background(), db, pw); err != nil {
		return err
	}
	fmt.Println("New admin password:", pw)
	fmt.Println("All sessions were signed out. Change the password in the console under Settings.")
	return nil
}

// healthcheck probes the local server; used by the container HEALTHCHECK
// because the runtime image has no shell or curl.
func healthcheck() int {
	env, err := config.LoadEnv()
	if err != nil {
		return 1
	}
	addr := env.Listen
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// staticFS returns the embedded console, or the files under XR_STATIC_DIR
// when set (console development: edits show on reload without a rebuild).
func staticFS(log *slog.Logger) fs.FS {
	if dir := os.Getenv("XR_STATIC_DIR"); dir != "" {
		log.Warn("serving the console from disk (XR_STATIC_DIR)", "dir", dir)
		return os.DirFS(dir)
	}
	return web.FS()
}
