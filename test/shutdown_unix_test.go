//go:build unix

package test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/store"
)

// SIGTERM lets in-flight streams finish and flushes their logs before the
// process exits (acceptance J3).
func TestGracefulShutdownCompletesStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "xai-router")
	build := exec.Command("go", "build", "-o", bin, "./cmd/xai-router")
	build.Dir = ".."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	// Upstream that streams 6 chunks, 500 ms apart.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 6; i++ {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"c%d \"}}]}\n\n", i)
			fl.Flush()
			time.Sleep(500 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":6,\"cost_in_usd_ticks\":1000}}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "XR_LISTEN="+addr, "XR_DATA_DIR="+dir, "XR_ADMIN_PASSWORD="+adminPW)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	base := "http://" + addr
	for i := 0; ; i++ {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		if i > 100 {
			t.Fatalf("server did not start:\n%s", logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	call := func(method, path, tok string, body any) map[string]any {
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, base+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	adm := call("POST", "/api/v1/auth/login", "", map[string]any{"password": adminPW})["token"].(string)
	call("PUT", "/api/v1/settings", adm, map[string]any{"allow_private_upstreams": true, "allow_insecure_upstreams": true,
		"upstream_allowed_hosts": []string{}})
	call("POST", "/api/v1/keys", adm, map[string]any{"key": "xai-test", "base_url": up.URL})
	client := call("POST", "/api/v1/tokens", adm, map[string]any{"name": "c"})["token"].(string)

	req, _ := http.NewRequestWithContext(context.Background(), "POST", base+"/v1/chat/completions",
		strings.NewReader(`{"model":"grok-4.3","stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+client)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	rd := bufio.NewReader(resp.Body)
	if line, _ := rd.ReadString('\n'); !strings.Contains(line, "c0") {
		t.Fatalf("first chunk %q", line)
	}

	// Shut down while the stream is in flight.
	sigAt := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), "c5") || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("stream cut short by shutdown: %q", rest)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v\n%s", err, logs.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("process still running 30s after SIGTERM\n%s", logs.String())
	}
	t.Logf("stream finished and process exited %v after SIGTERM", time.Since(sigAt).Round(time.Millisecond))

	// The stream's log was written before exit.
	db, err := store.Open(filepath.Join(dir, "xai-router.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ls, n, err := db.ListLogs(context.Background(), store.LogFilter{Path: "/v1/chat/completions", Limit: 10})
	if err != nil || n != 1 || ls[0].Status != 200 || ls[0].CostTicks != 1000 {
		t.Fatalf("stream log after shutdown: n=%d %+v %v\n%s", n, ls, err, logs.String())
	}
}
