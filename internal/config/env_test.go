package config

import (
	"log/slog"
	"testing"
	"time"
)

func TestLoadEnv(t *testing.T) {
	for _, k := range []string{"XR_LISTEN", "XR_DATA_DIR", "XR_ADMIN_PASSWORD", "XR_LOG_LEVEL", "XR_LOG_FORMAT"} {
		t.Setenv(k, "")
	}
	e, err := LoadEnv()
	if err != nil || e.Listen != ":8080" || e.DataDir != "./data" || e.LogLevel != slog.LevelInfo || e.LogFormat != "text" {
		t.Fatalf("defaults: %+v %v", e, err)
	}
	t.Setenv("XR_LISTEN", " 127.0.0.1:9 ")
	t.Setenv("XR_ADMIN_PASSWORD", "pw")
	t.Setenv("XR_LOG_LEVEL", "debug")
	t.Setenv("XR_LOG_FORMAT", "JSON")
	if e, err = LoadEnv(); err != nil || e.Listen != "127.0.0.1:9" || e.AdminPassword != "pw" ||
		e.LogLevel != slog.LevelDebug || e.LogFormat != "json" {
		t.Fatalf("overrides: %+v %v", e, err)
	}
	t.Setenv("XR_LOG_LEVEL", "loud")
	if _, err = LoadEnv(); err == nil {
		t.Fatal("invalid level accepted")
	}
	t.Setenv("XR_LOG_LEVEL", "info")
	t.Setenv("XR_LOG_FORMAT", "xml")
	if _, err = LoadEnv(); err == nil {
		t.Fatal("invalid format accepted")
	}
}

func TestValidateNormalises(t *testing.T) {
	s := DefaultSettings()
	s.UpstreamBaseURL = " https://api.x.ai/// "
	s.UnpricedModels = ""
	s.KeyTestModel = "  grok-4  "
	s.UpstreamAllowedHosts = []string{" API.X.AI ", "api.x.ai", ""}
	s.TrustedProxies = []string{" 10.0.0.1 ", "10.0.0.1"}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.UpstreamBaseURL != "https://api.x.ai" || s.UnpricedModels != "block_quota_tokens" || s.KeyTestModel != "grok-4" ||
		len(s.UpstreamAllowedHosts) != 1 || len(s.TrustedProxies) != 1 {
		t.Fatalf("normalised: %+v", s)
	}
	for name, mut := range map[string]func(*Settings){
		"unpriced":      func(s *Settings) { s.UnpricedModels = "maybe" },
		"host policy":   func(s *Settings) { s.UpstreamBaseURL = "https://evil.example" },
		"insecure":      func(s *Settings) { s.UpstreamBaseURL = "http://api.x.ai" },
		"private":       func(s *Settings) { s.UpstreamAllowedHosts = nil; s.UpstreamBaseURL = "https://127.0.0.1" },
		"no host":       func(s *Settings) { s.UpstreamBaseURL = "https://" },
		"ws log range":  func(s *Settings) { s.WSLogMessages = -1 },
		"stream idle":   func(s *Settings) { s.StreamIdleTimeoutSeconds = 1 },
		"retention max": func(s *Settings) { s.LogRetentionDays = 4000 },
	} {
		c := DefaultSettings()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	ok := DefaultSettings()
	ok.UpstreamAllowedHosts = nil
	ok.AllowPrivateUpstreams, ok.AllowInsecureUpstreams = true, true
	ok.UpstreamBaseURL = "http://127.0.0.1:8080"
	if err := ok.Validate(); err != nil {
		t.Fatalf("local upstream with policy relaxed: %v", err)
	}
}

func TestHelpersAndHolder(t *testing.T) {
	s := DefaultSettings()
	if s.RequestTimeout() != 600*time.Second || s.StreamIdleTimeout() != 300*time.Second ||
		s.MaxBodyBytes() != 50<<20 || s.LogBodyMaxBytes() != 256<<10 {
		t.Fatal("duration/size helpers")
	}
	if !s.IsRetryStatus(503) || s.IsRetryStatus(400) {
		t.Fatal("IsRetryStatus")
	}
	p := s.UpstreamPolicy()
	if p.AllowPrivate || p.AllowInsecure || len(p.AllowedHosts) != 2 {
		t.Fatalf("policy: %+v", p)
	}
	h := NewHolder(s)
	s.RetryKeywords[0] = "mutated"
	if h.Get().RetryKeywords[0] == "mutated" {
		t.Fatal("holder shares slices with the caller")
	}
	s.MaxAttempts = 9
	h.Set(s)
	if h.Get().MaxAttempts != 9 {
		t.Fatal("Set not applied")
	}
}
