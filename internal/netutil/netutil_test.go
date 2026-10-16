package netutil

import (
	"net/http/httptest"
	"testing"
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
