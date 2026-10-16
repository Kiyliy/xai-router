package test

import (
	"bufio"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Every endpoint in the xAI REST reference (docs.x.ai/openapi.json and the
// voice/files/collections/batches references, 2026-10-16) plus unknown
// paths is relayed with its status code, headers and complete body.
func TestDocumentedEndpointsPassThrough(t *testing.T) {
	e := newEnv(t, "xai-ok")
	endpoints := []struct {
		method, path, body string
		status             int
	}{
		{"GET", "/v1/api-key", "", 200},
		{"GET", "/v1/me", "", 200},
		{"GET", "/v1/models/grok-4.3", "", 200},
		{"GET", "/v1/language-models/grok-4.3", "", 200},
		{"GET", "/v1/image-generation-models", "", 200},
		{"GET", "/v1/image-generation-models/grok-imagine-image", "", 200},
		{"GET", "/v1/video-generation-models", "", 200},
		{"GET", "/v1/video-generation-models/grok-imagine-video", "", 200},
		{"GET", "/v1/embedding-models", "", 200},
		{"POST", "/v1/tokenize-text", `{"model":"grok-4.3","text":"hello"}`, 200},
		{"POST", "/v1/embeddings", `{"model":"grok-4.3","input":"hello"}`, 200},
		{"POST", "/v1/completions", `{"model":"grok-4.3","prompt":"hi"}`, 200},
		{"POST", "/v1/complete", `{"model":"grok-4.3","prompt":"hi"}`, 200},
		{"POST", "/v1/responses/compact", `{"model":"grok-4.3","input":[]}`, 200},
		{"GET", "/v1/responses/unknown-id/input_items", "", 404},
		{"DELETE", "/v1/responses/unknown-id", "", 200},
		{"GET", "/v1/chat/deferred-completion/req-1", "", 202},
		{"GET", "/v1/images/req-1", "", 202},
		{"POST", "/v1/documents/search", `{"query":"q","source":{"collection_ids":["c1"]}}`, 200},
		{"GET", "/v1/files?limit=5", "", 200},
		{"POST", "/v1/files", `{"purpose":"assistants"}`, 200},
		{"GET", "/v1/files/f1", "", 200},
		{"GET", "/v1/files/f1/content", "", 200},
		{"POST", "/v1/files/f1/public-url", "", 200},
		{"POST", "/v1/files/f1/public-url/revoke", "", 200},
		{"DELETE", "/v1/files/f1", "", 200},
		{"POST", "/v1/batches", `{"name":"b"}`, 200},
		{"GET", "/v1/batches", "", 200},
		{"GET", "/v1/batches/b1", "", 200},
		{"GET", "/v1/batches/b1/requests", "", 200},
		{"POST", "/v1/batches/b1/requests", `{"requests":[]}`, 200},
		{"GET", "/v1/batches/b1/results", "", 200},
		{"POST", "/v1/batches/b1:cancel", "", 200},
		{"POST", "/v1/collections", `{"name":"c"}`, 201},
		{"GET", "/v1/collections", "", 200},
		{"GET", "/v1/collections/c1", "", 200},
		{"PUT", "/v1/collections/c1", `{"name":"c2"}`, 200},
		{"POST", "/v1/collections/c1/documents/f1", "", 200},
		{"GET", "/v1/collections/c1/documents", "", 200},
		{"GET", "/v1/collections/c1/documents/f1", "", 200},
		{"PATCH", "/v1/collections/c1/documents/f1", `{}`, 200},
		{"GET", "/v1/collections/c1/documents:batchGet?file_ids=f1", "", 200},
		{"DELETE", "/v1/collections/c1/documents/f1", "", 204},
		{"DELETE", "/v1/collections/c1", "", 200},
		{"GET", "/v1/skills", "", 200},
		{"POST", "/v1/skills", `{"name":"s"}`, 200},
		{"GET", "/v1/skills/s1", "", 200},
		{"GET", "/v1/skills/s1/content", "", 200},
		{"DELETE", "/v1/skills/s1", "", 200},
		{"GET", "/v1/tts/voices", "", 200},
		{"GET", "/v1/tts/voices/eve", "", 200},
		{"POST", "/v1/custom-voices", `{"name":"v"}`, 200},
		{"GET", "/v1/custom-voices", "", 200},
		{"GET", "/v1/custom-voices/v1", "", 200},
		{"PATCH", "/v1/custom-voices/v1", `{"name":"v2"}`, 200},
		{"GET", "/v1/custom-voices/v1/audio", "", 200},
		{"DELETE", "/v1/custom-voices/v1", "", 200},
		{"POST", "/v1/realtime/client_secrets", `{"expires_after":{"seconds":60}}`, 200},
		{"POST", "/v1/realtime/calls/c1/refer", `{"target_uri":"sip:x"}`, 200},
		{"POST", "/v1/realtime/calls/c1/hangup", "", 200},
		{"POST", "/v2/phone-numbers", `{"number":"x"}`, 200},
		{"GET", "/v2/phone-numbers", "", 200},
		{"DELETE", "/v2/phone-numbers/p1", "", 200},
		{"GET", "/v1/some/future/endpoint?x=1", "", 418},
		{"POST", "/v1/another-future", `{"a":1}`, 422},
	}
	for _, c := range endpoints {
		req, _ := http.NewRequest(c.method, e.srv.URL+c.path, strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+e.client)
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("X-Grok-Mock-Echo", "1")
		req.Header.Set("X-Grok-Mock-Status", strconv.Itoa(c.status))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("%s %s: status %d, want %d (%.200s)", c.method, c.path, resp.StatusCode, c.status, raw)
			continue
		}
		if c.status == http.StatusNoContent {
			continue
		}
		var got struct {
			Method, Path, Query, Auth, Padding string
			BodyLen                            int `json:"body_len"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Errorf("%s %s: body not passed through intact (%d bytes): %v", c.method, c.path, len(raw), err)
			continue
		}
		u, _ := url.Parse(c.path)
		if got.Method != c.method || got.Path != u.Path || got.Query != u.RawQuery || got.BodyLen != len(c.body) ||
			len(got.Padding) != 100_000 || got.Auth != "Bearer xai-ok" || resp.Header.Get("X-Upstream-Custom") != "kept" {
			t.Errorf("%s %s: upstream saw %s %s?%s body %d auth %q", c.method, c.path, got.Method, got.Path, got.Query,
				got.BodyLen, got.Auth)
		}
	}
}

// Client headers that identify the client or its network are not forwarded;
// the upstream only ever sees a pool key.
func TestClientHeadersNotForwarded(t *testing.T) {
	e := newEnv(t, "xai-ok")
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"grok-4.3","messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+e.client)
	req.Header.Set("Content-Type", "application/json")
	for _, h := range []string{"Cookie", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip",
		"Forwarded", "Via", "X-Api-Key", "Proxy-Authorization", "X-Custom-Secret", "Origin", "Referer"} {
		req.Header.Set(h, "client-value")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h := e.mock.lastHdr["/v1/chat/completions"]
	for name, vals := range h {
		for _, v := range vals {
			if strings.Contains(v, "client-value") || strings.Contains(v, e.client) {
				t.Errorf("client header leaked upstream: %s: %s", name, v)
			}
		}
	}
	if h.Get("Authorization") != "Bearer xai-ok" {
		t.Fatalf("upstream authorization %q", h.Get("Authorization"))
	}
}

// Malformed framing is rejected with 4xx, never 500.
func TestMalformedContentLength(t *testing.T) {
	e := newEnv(t, "xai-ok")
	u, _ := url.Parse(e.srv.URL)
	send := func(raw string, closeWrite bool) (int, error) {
		conn, err := net.Dial("tcp", u.Host)
		if err != nil {
			return 0, err
		}
		defer conn.Close()
		_, _ = conn.Write([]byte(raw))
		if closeWrite {
			_ = conn.(*net.TCPConn).CloseWrite()
		}
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	head := "POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + e.client +
		"\r\nContent-Type: application/json\r\n"
	for _, cl := range []string{"abc", "-5", "1, 2", "99999999999999999999"} {
		status, err := send(head+"Content-Length: "+cl+"\r\n\r\n{}", false)
		if err != nil {
			t.Fatalf("Content-Length %q: %v", cl, err)
		}
		if status < 400 || status >= 500 {
			t.Errorf("Content-Length %q: status %d, want 4xx", cl, status)
		}
	}
	// A body shorter than its Content-Length is a client error too.
	if status, err := send(head+"Content-Length: 100\r\n\r\n{\"model\":", true); err == nil && status >= 500 {
		t.Errorf("truncated body: status %d", status)
	}
}
