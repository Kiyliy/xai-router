package test

import (
	"context"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Writing logs must not slow forwarding down: with the database writer
// stalled (a long write transaction holds SQLite's write lock), request
// latency must stay the same. Acceptance F6: p99 grows by less than 10 ms.
func TestStalledLogWriterDoesNotBlockForwarding(t *testing.T) {
	e := newEnvDB(t, filepath.Join(t.TempDir(), "xr.db"), "xai-ok")
	body := `{"model":"grok-4.3","messages":[{"role":"user","content":"hi"}]}`
	measure := func(n int) time.Duration {
		lat := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+e.client)
			req.Header.Set("Content-Type", "application/json")
			t0 := time.Now()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			lat = append(lat, time.Since(t0))
		}
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		return lat[n*99/100]
	}
	measure(30) // warm up connections and caches
	base := measure(300)

	// Stall the writer: hold the single write connection in a transaction.
	ctx := context.Background()
	tx, err := e.app.DB.W.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta (k, v) VALUES ('stall', '1')`); err != nil {
		t.Fatal(err)
	}
	stalled := measure(300)
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	t.Logf("p99 latency: baseline %v, with the log writer stalled %v", base, stalled)
	if stalled-base > 10*time.Millisecond {
		t.Fatalf("p99 grew by %v with a stalled log writer (baseline %v, stalled %v)", stalled-base, base, stalled)
	}
	// Once the writer is free again, every request is logged.
	e.waitLogs(630)
}
