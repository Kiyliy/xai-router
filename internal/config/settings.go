package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Kiyliy/xai-router/internal/netutil"
)

// Settings are the runtime settings. JSON field names are part of the admin API.
type Settings struct {
	UpstreamBaseURL          string   `json:"upstream_base_url"`
	SelectionStrategy        string   `json:"selection_strategy"`
	RetryStatusCodes         []int    `json:"retry_status_codes"`
	RetryKeywords            []string `json:"retry_keywords"`
	DisableKeywords          []string `json:"disable_keywords"`
	MaxAttempts              int      `json:"max_attempts"`
	CooldownSeconds          int      `json:"cooldown_seconds"`
	BreakerThreshold         int      `json:"breaker_threshold"`
	BreakerSeconds           int      `json:"breaker_seconds"`
	RequestTimeoutSeconds    int      `json:"request_timeout_seconds"`
	ConnectTimeoutSeconds    int      `json:"connect_timeout_seconds"`
	StreamIdleTimeoutSeconds int      `json:"stream_idle_timeout_seconds"`
	MaxBodyMB                int      `json:"max_body_mb"`
	LogEnabled               bool     `json:"log_enabled"`
	LogBodies                bool     `json:"log_bodies"`
	LogRequestBodyMaxKB      int      `json:"log_request_body_max_kb"`  // per stored request body
	LogResponseBodyMaxKB     int      `json:"log_response_body_max_kb"` // per stored response body (and assembled text)
	DiskWarnPercent          int      `json:"disk_warn_percent"`        // warn when the data volume is this full
	LogRetentionDays         int      `json:"log_retention_days"`
	ModelSyncIntervalHours   int      `json:"model_sync_interval_hours"`
	TaskBindingTTLHours      int      `json:"task_binding_ttl_hours"`
	KeyTestModel             string   `json:"key_test_model"`
	TrustedProxies           []string `json:"trusted_proxies"`
	ExposeDebug              bool     `json:"expose_debug"`
	InjectStreamUsage        bool     `json:"inject_stream_usage"`
	AnthropicStreamFix       bool     `json:"anthropic_stream_fix"`
	WSLogMessages            int      `json:"ws_log_messages"`
	UnpricedModels           string   `json:"unpriced_models"`
	UpstreamAllowedHosts     []string `json:"upstream_allowed_hosts"`
	AllowInsecureUpstreams   bool     `json:"allow_insecure_upstreams"`
	AllowPrivateUpstreams    bool     `json:"allow_private_upstreams"`
}

// UpstreamPolicy returns the SSRF policy for upstream base URLs.
func (s *Settings) UpstreamPolicy() netutil.UpstreamPolicy {
	return netutil.UpstreamPolicy{AllowedHosts: s.UpstreamAllowedHosts, AllowInsecure: s.AllowInsecureUpstreams,
		AllowPrivate: s.AllowPrivateUpstreams}
}

// DefaultSettings returns the factory defaults.
func DefaultSettings() Settings {
	return Settings{
		UpstreamBaseURL:   "https://api.x.ai",
		SelectionStrategy: "weighted",
		RetryStatusCodes:  []int{401, 403, 408, 429, 500, 502, 503, 504},
		RetryKeywords: []string{
			"api key", "api_key", "invalid key", "incorrect key", "unauthorized",
			"unauthenticated", "permission denied", "rate limit", "rate_limit",
			"too many requests", "quota", "credits", "spending limit", "billing",
			"insufficient", "blocked", "suspended", "disabled",
		},
		DisableKeywords: []string{
			"incorrect api key", "invalid api key", "api key is blocked", "api key is disabled",
			"team is blocked", "account suspended", "has been blocked",
			"does not have any credits", "doesn't have any credits", "no credits",
			"used all available credits", "exhausted its spending limit", "reached its monthly spending limit",
		},
		MaxAttempts:              5,
		CooldownSeconds:          60,
		BreakerThreshold:         3,
		BreakerSeconds:           30,
		RequestTimeoutSeconds:    600,
		ConnectTimeoutSeconds:    15,
		StreamIdleTimeoutSeconds: 300,
		MaxBodyMB:                50,
		LogEnabled:               true,
		LogBodies:                true,
		LogRequestBodyMaxKB:      32,
		LogResponseBodyMaxKB:     32,
		DiskWarnPercent:          85,
		LogRetentionDays:         30,
		ModelSyncIntervalHours:   24,
		TaskBindingTTLHours:      720,
		TrustedProxies:           []string{"127.0.0.1/32", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"},
		InjectStreamUsage:        true,
		AnthropicStreamFix:       true,
		WSLogMessages:            50,
		UnpricedModels:           "block_quota_tokens",
		UpstreamAllowedHosts:     []string{"api.x.ai", "*.api.x.ai"},
	}
}

// Validate normalises and checks the settings.
func (s *Settings) Validate() error {
	s.UpstreamBaseURL = strings.TrimRight(strings.TrimSpace(s.UpstreamBaseURL), "/")
	s.UpstreamAllowedHosts = cleanList(s.UpstreamAllowedHosts, true)
	if u, err := url.Parse(s.UpstreamBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("upstream_base_url must be an http(s) URL")
	}
	if err := s.UpstreamPolicy().CheckURL(s.UpstreamBaseURL); err != nil {
		return fmt.Errorf("upstream_base_url: %v", err)
	}
	switch s.UnpricedModels {
	case "":
		s.UnpricedModels = "block_quota_tokens"
	case "block_quota_tokens", "allow":
	default:
		return fmt.Errorf("unpriced_models must be block_quota_tokens or allow")
	}
	switch s.SelectionStrategy {
	case "weighted", "random", "round_robin":
	default:
		return fmt.Errorf("selection_strategy must be weighted, random or round_robin")
	}
	for _, c := range s.RetryStatusCodes {
		if c < 100 || c > 599 {
			return fmt.Errorf("retry_status_codes: %d is not an HTTP status", c)
		}
	}
	s.RetryKeywords = cleanList(s.RetryKeywords, true)
	s.DisableKeywords = cleanList(s.DisableKeywords, true)
	s.TrustedProxies = cleanList(s.TrustedProxies, false)
	for _, p := range s.TrustedProxies {
		if _, _, err := net.ParseCIDR(p); err != nil {
			if net.ParseIP(p) == nil {
				return fmt.Errorf("trusted_proxies: %q is not an IP or CIDR", p)
			}
		}
	}
	checks := []struct {
		name     string
		v        *int
		min, max int
	}{
		{"max_attempts", &s.MaxAttempts, 1, 50},
		{"cooldown_seconds", &s.CooldownSeconds, 1, 86400},
		{"breaker_threshold", &s.BreakerThreshold, 1, 100},
		{"breaker_seconds", &s.BreakerSeconds, 1, 86400},
		{"request_timeout_seconds", &s.RequestTimeoutSeconds, 5, 7200},
		{"connect_timeout_seconds", &s.ConnectTimeoutSeconds, 1, 300},
		{"stream_idle_timeout_seconds", &s.StreamIdleTimeoutSeconds, 5, 7200},
		{"max_body_mb", &s.MaxBodyMB, 1, 1024},
		{"log_request_body_max_kb", &s.LogRequestBodyMaxKB, 1, 65536},
		{"log_response_body_max_kb", &s.LogResponseBodyMaxKB, 1, 65536},
		{"disk_warn_percent", &s.DiskWarnPercent, 50, 99},
		{"log_retention_days", &s.LogRetentionDays, 0, 3650},
		{"model_sync_interval_hours", &s.ModelSyncIntervalHours, 0, 8760},
		{"task_binding_ttl_hours", &s.TaskBindingTTLHours, 1, 8760},
		{"ws_log_messages", &s.WSLogMessages, 0, 10000},
	}
	for _, c := range checks {
		if *c.v < c.min || *c.v > c.max {
			return fmt.Errorf("%s must be between %d and %d", c.name, c.min, c.max)
		}
	}
	s.KeyTestModel = strings.TrimSpace(s.KeyTestModel)
	return nil
}

// Merge applies a partial JSON object on top of s and validates the result.
func (s Settings) Merge(patch []byte) (Settings, error) {
	out := s.clone()
	dec := json.NewDecoder(strings.NewReader(string(patch)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return s, fmt.Errorf("invalid settings: %w", err)
	}
	if err := out.Validate(); err != nil {
		return s, err
	}
	return out, nil
}

func (s Settings) clone() Settings {
	c := s
	c.RetryStatusCodes = append([]int(nil), s.RetryStatusCodes...)
	c.RetryKeywords = append([]string(nil), s.RetryKeywords...)
	c.DisableKeywords = append([]string(nil), s.DisableKeywords...)
	c.TrustedProxies = append([]string(nil), s.TrustedProxies...)
	return c
}

// Helpers used on hot paths.

func (s *Settings) RequestTimeout() time.Duration {
	return time.Duration(s.RequestTimeoutSeconds) * time.Second
}
func (s *Settings) StreamIdleTimeout() time.Duration {
	return time.Duration(s.StreamIdleTimeoutSeconds) * time.Second
}
func (s *Settings) MaxBodyBytes() int64 { return int64(s.MaxBodyMB) << 20 }

// LogRequestBodyMaxBytes caps a stored request body.
func (s *Settings) LogRequestBodyMaxBytes() int { return s.LogRequestBodyMaxKB << 10 }

// LogResponseBodyMaxBytes caps a stored response body and assembled text.
func (s *Settings) LogResponseBodyMaxBytes() int { return s.LogResponseBodyMaxKB << 10 }

// IsRetryStatus reports whether status should trigger trying the next key.
func (s *Settings) IsRetryStatus(status int) bool {
	for _, c := range s.RetryStatusCodes {
		if c == status {
			return true
		}
	}
	return false
}

// MatchKeyword returns the first keyword contained in body (case-insensitive).
func MatchKeyword(body string, keywords []string) string {
	if body == "" {
		return ""
	}
	lb := strings.ToLower(body)
	for _, k := range keywords {
		if k != "" && strings.Contains(lb, k) {
			return k
		}
	}
	return ""
}

func cleanList(in []string, lower bool) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if lower {
			v = strings.ToLower(v)
		}
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// Holder gives lock-free access to the current settings.
type Holder struct{ p atomic.Pointer[Settings] }

// NewHolder returns a holder initialised with s.
func NewHolder(s Settings) *Holder {
	h := &Holder{}
	h.Set(s)
	return h
}

// Get returns the current settings. Callers must not mutate the result.
func (h *Holder) Get() *Settings { return h.p.Load() }

// Set replaces the current settings.
func (h *Holder) Set(s Settings) { c := s.clone(); h.p.Store(&c) }
