// Package config holds the bootstrap configuration (environment variables, read
// once at startup) and the runtime settings (stored in the database, editable
// from the admin UI and applied without a restart).
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// Env is the bootstrap configuration. Everything else lives in Settings.
type Env struct {
	Listen        string // XR_LISTEN, default ":8080"
	DataDir       string // XR_DATA_DIR, default "./data"
	AdminPassword string // XR_ADMIN_PASSWORD, only used when no password is set yet
	LogLevel      slog.Level
	LogFormat     string // "text" or "json"
}

// LoadEnv reads the bootstrap configuration from the environment.
func LoadEnv() (Env, error) {
	e := Env{
		Listen:        getenv("XR_LISTEN", ":8080"),
		DataDir:       getenv("XR_DATA_DIR", "./data"),
		AdminPassword: os.Getenv("XR_ADMIN_PASSWORD"),
		LogFormat:     strings.ToLower(getenv("XR_LOG_FORMAT", "text")),
	}
	if err := e.LogLevel.UnmarshalText([]byte(getenv("XR_LOG_LEVEL", "info"))); err != nil {
		return e, fmt.Errorf("XR_LOG_LEVEL: %w", err)
	}
	if e.LogFormat != "text" && e.LogFormat != "json" {
		return e, fmt.Errorf("XR_LOG_FORMAT must be text or json, got %q", e.LogFormat)
	}
	return e, nil
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}
