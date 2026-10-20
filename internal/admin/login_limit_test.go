package admin

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// M2: concurrent wrong passwords from one IP get at most loginMaxFails
// password checks; the rest are refused before checking.
func TestLoginLimitHoldsUnderConcurrency(t *testing.T) {
	h := newHarness(t)
	const n = 60
	var unauthorized, limited, other atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp, err := http.Post(h.srv.URL+"/api/v1/auth/login", "application/json",
				strings.NewReader(`{"password":"wrong-password"}`))
			if err != nil {
				other.Add(1)
				return
			}
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusUnauthorized:
				unauthorized.Add(1)
			case http.StatusTooManyRequests:
				limited.Add(1)
			default:
				other.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if unauthorized.Load() != loginMaxFails || limited.Load() != n-loginMaxFails || other.Load() != 0 {
		t.Fatalf("401 %d, 429 %d, other %d (want %d password checks)", unauthorized.Load(), limited.Load(),
			other.Load(), loginMaxFails)
	}
	// Even the right password is refused while locked out.
	if r := h.login(testPassword); r.status != http.StatusTooManyRequests {
		t.Fatalf("locked out client logged in: %d", r.status)
	}
}

// A success clears the count; IPv6 clients are grouped by /64.
func TestLoginLimiterKeys(t *testing.T) {
	var l loginLimiter
	for i := 0; i < loginMaxFails; i++ {
		if !l.attempt("2001:db8:1:2::" + itoa(int64(i+1))) {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if l.attempt("2001:db8:1:2:ffff::1") {
		t.Fatal("same /64 not limited")
	}
	if !l.attempt("2001:db8:1:3::1") || !l.attempt("192.0.2.1") {
		t.Fatal("other clients limited")
	}
	l.reset("2001:db8:1:2::99")
	if l.blocked("2001:db8:1:2::1") {
		t.Fatal("reset did not clear the /64")
	}
}
