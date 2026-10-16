package proxy

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSSEParserSplitChunks(t *testing.T) {
	stream := "event: response.created\ndata: {\"a\":1}\n\n: keep-alive\n\ndata: line1\ndata: line2\n\r\ndata: [DONE]\n\n"
	var got []string
	for size := 1; size <= len(stream); size++ {
		got = got[:0]
		p := &sseParser{onEvent: func(ev string, data []byte) { got = append(got, ev+"|"+string(data)) }}
		for i := 0; i < len(stream); i += size {
			p.Write([]byte(stream[i:min(i+size, len(stream))]))
		}
		p.Close()
		want := []string{"response.created|{\"a\":1}", "|line1\nline2", "|[DONE]"}
		if strings.Join(got, ";") != strings.Join(want, ";") {
			t.Fatalf("chunk size %d: got %q", size, got)
		}
	}
}

func TestSSEParserUnterminated(t *testing.T) {
	var got []string
	p := &sseParser{onEvent: func(_ string, data []byte) { got = append(got, string(data)) }}
	p.Write([]byte("data: x\n\ndata: tail"))
	p.Close()
	if strings.Join(got, ",") != "x,tail" {
		t.Fatalf("got %q", got)
	}
}

func TestCaptureHeadAndTail(t *testing.T) {
	c := newCapture(100)
	c.Write([]byte(strings.Repeat("a", 50)))
	if c.truncated || string(c.Bytes()) != strings.Repeat("a", 50) {
		t.Fatal("small stream should be kept whole")
	}
	for i := 0; i < 100; i++ {
		c.Write([]byte("bbbbbbbbbb"))
	}
	c.Write([]byte("THE-END"))
	out := string(c.Bytes())
	if !c.truncated || !strings.HasPrefix(out, strings.Repeat("a", 50)) || !strings.HasSuffix(out, "THE-END") {
		t.Fatalf("unexpected capture: %q", out)
	}
	if !strings.Contains(out, "1057 bytes total") {
		t.Fatalf("missing total marker: %q", out)
	}
}

func TestReferencedResource(t *testing.T) {
	cases := []struct{ method, path, kind, id string }{
		{"GET", "/v1/videos/abc-123", KindVideo, "abc-123"},
		{"POST", "/v1/videos/generations", "", ""},
		{"GET", "/v1/images/req1", KindImage, "req1"},
		{"GET", "/v1/chat/deferred-completion/r9", KindDeferred, "r9"},
		{"GET", "/v1/responses/resp_1/input_items", KindResponse, "resp_1"},
		{"POST", "/v1/responses/compact", "", ""},
		{"POST", "/v1/batches/b1:cancel", KindBatch, "b1"},
		{"GET", "/v1/files/f1/content", KindFile, "f1"},
		{"GET", "/v1/models", "", ""},
	}
	for _, c := range cases {
		k, id := referencedResource(c.method, c.path)
		if k != c.kind || id != c.id {
			t.Errorf("%s %s: got (%q,%q) want (%q,%q)", c.method, c.path, k, id, c.kind, c.id)
		}
	}
}

func TestInjectStreamUsage(t *testing.T) {
	out := string(injectStreamUsage([]byte(`{"model":"m","stream":true}`)))
	if !strings.Contains(out, `"stream_options":{"include_usage":true}`) {
		t.Fatalf("not injected: %s", out)
	}
	keep := `{"model":"m","stream":true,"stream_options":{"include_usage":false}}`
	if string(injectStreamUsage([]byte(keep))) != keep {
		t.Fatal("client choice overwritten")
	}
	out = string(injectStreamUsage([]byte(`{"stream":true,"stream_options":{"foo":1}}`)))
	if !strings.Contains(out, `"include_usage":true`) || !strings.Contains(out, `"foo":1`) {
		t.Fatalf("merge failed: %s", out)
	}
}

func TestShouldForward(t *testing.T) {
	for h, want := range map[string]bool{"Content-Type": true, "Cookie": false, "X-Forwarded-For": false,
		"X-Grok-Conv-Id": true, "Authorization": false, "Anthropic-Version": true, "X-Real-Ip": false} {
		if shouldForward(h) != want {
			t.Errorf("shouldForward(%s) != %v", h, want)
		}
	}
}

func TestClientIP(t *testing.T) {
	trusted := []string{"10.0.0.0/8"}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "1.2.3.4:5555"
	r.Header.Set("X-Forwarded-For", "9.9.9.9")
	if ip := clientIP(r, trusted); ip != "1.2.3.4" {
		t.Fatalf("untrusted peer must not be able to spoof: %s", ip)
	}
	r.RemoteAddr = "10.1.1.1:5555"
	r.Header.Set("X-Forwarded-For", "8.8.8.8, 7.7.7.7, 10.2.2.2")
	if ip := clientIP(r, trusted); ip != "7.7.7.7" {
		t.Fatalf("got %s", ip)
	}
}

func TestTruncateStr(t *testing.T) {
	if got := truncateStr("héllo", 2); got != "h" {
		t.Fatalf("got %q", got)
	}
}
