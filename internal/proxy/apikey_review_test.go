package proxy

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// /v1/api-key describes the upstream key; a client token must not reach it
// (acceptance round 6, B3).
func TestAPIKeyEndpointNotForwarded(t *testing.T) {
	hs := newHarness(t, nil)
	var calls atomic.Int32
	up := newMockUp(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"team_id":"secret-team","redacted_api_key":"xai-...abcd"}`))
	})
	hs.addKey("k1", up.srv.URL, nil)
	tok, _ := hs.addToken("client", nil)
	for _, p := range []string{"/v1/api-key", "/v1/api-key/"} {
		resp, b := hs.doAs(tok, http.MethodGet, p, "")
		if resp.StatusCode != http.StatusNotFound || strings.Contains(b, "secret-team") {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, b)
		}
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("upstream called %d times", n)
	}
}
