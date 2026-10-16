package netutil

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	trusted := []string{"10.0.0.0/8"}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:5555"
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	if ip := ClientIP(r, trusted); ip != "1.2.3.4" {
		t.Fatalf("untrusted peer must not be able to spoof: %s", ip)
	}
	r.RemoteAddr = "10.1.1.1:5555"
	r.Header.Set("X-Forwarded-For", "8.8.8.8, 7.7.7.7, 10.2.2.2")
	if ip := ClientIP(r, trusted); ip != "7.7.7.7" {
		t.Fatalf("got %s", ip)
	}
}

func TestCheckURL(t *testing.T) {
	def := UpstreamPolicy{AllowedHosts: []string{"api.x.ai", "*.api.x.ai"}}
	for u, ok := range map[string]bool{
		"https://api.x.ai":          true,
		"https://us.api.x.ai/":      true,
		"https://evilapi.x.ai":      false,
		"https://api.x.ai.evil.com": false,
		"http://api.x.ai":           false,
		"https://127.0.0.1":         false,
		"https://user:pw@api.x.ai":  false,
		"file:///etc/passwd":        false,
		"https://[::1]":             false,
		"https://localhost":         false,
	} {
		if err := def.CheckURL(u); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", u, err, ok)
		}
	}
	open := UpstreamPolicy{}
	if open.CheckURL("https://example.com") != nil || open.CheckURL("https://169.254.169.254") == nil {
		t.Fatal("empty host list must allow public hosts but still block metadata addresses")
	}
	if (UpstreamPolicy{AllowPrivate: true, AllowInsecure: true}).CheckURL("http://10.1.2.3:8080") != nil {
		t.Fatal("explicitly allowed private http upstream rejected")
	}
}

func TestIsPublic(t *testing.T) {
	for ip, want := range map[string]bool{"8.8.8.8": true, "2606:4700::1111": true, "10.0.0.1": false,
		"172.16.0.1": false, "192.168.1.1": false, "127.0.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fd00::1": false, "fe80::1": false, "::ffff:10.0.0.1": false} {
		if got := IsPublic(netip.MustParseAddr(ip)); got != want {
			t.Errorf("IsPublic(%s) = %v", ip, got)
		}
	}
}

func TestGuardedDialer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	timeout := func() time.Duration { return time.Second }
	blocked := GuardedDialer(timeout, func(string) bool { return false })
	if _, err := blocked(context.Background(), "tcp", addr); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("loopback dial not blocked: %v", err)
	}
	// "localhost" resolves to loopback: blocked after resolution too.
	if _, err := blocked(context.Background(), "tcp", "localhost:"+addr[strings.LastIndex(addr, ":")+1:]); err == nil {
		t.Fatal("name resolving to loopback not blocked")
	}
	allowed := GuardedDialer(timeout, func(string) bool { return true })
	c, err := allowed(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
