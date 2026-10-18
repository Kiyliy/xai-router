package proxy

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/store"
)

// ---- minimal RFC 6455 helpers ----

func wsAcceptKey(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

// encodeFrame builds one frame. A non-nil mask masks the payload.
func encodeFrame(fin bool, op byte, payload []byte, mask []byte) []byte {
	b0 := op
	if fin {
		b0 |= 0x80
	}
	hdr := []byte{b0}
	mbit := byte(0)
	if mask != nil {
		mbit = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, mbit|byte(n))
	case n < 65536:
		hdr = append(hdr, mbit|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, mbit|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	if mask == nil {
		return append(hdr, payload...)
	}
	hdr = append(hdr, mask...)
	for i, c := range payload {
		hdr = append(hdr, c^mask[i%4])
	}
	return hdr
}

func readWSFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := uint64(h[1] & 0x7f)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	var k [4]byte
	masked := h[1]&0x80 != 0
	if masked {
		if _, err := io.ReadFull(r, k[:]); err != nil {
			return 0, nil, err
		}
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	if masked {
		for i := range p {
			p[i] ^= k[i%4]
		}
	}
	return h[0] & 0x0f, p, nil
}

func closeFrame(code int, reason string) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(code)), reason...)
}

// ---- mock upstream ----

type wsUp struct {
	srv    *mockUp
	mu     sync.Mutex
	hdrs   []http.Header
	reject int // when non-zero, answer the handshake with this status
}

func newWSUp(t *testing.T) *wsUp {
	u := &wsUp{}
	u.srv = newMockUp(t, u.serve)
	return u
}

func (u *wsUp) setReject(status int) {
	u.mu.Lock()
	u.reject = status
	u.mu.Unlock()
}

func (u *wsUp) headers() []http.Header {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]http.Header(nil), u.hdrs...)
}

func (u *wsUp) serve(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.hdrs = append(u.hdrs, r.Header.Clone())
	reject := u.reject
	u.mu.Unlock()
	if reject != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reject)
		_, _ = fmt.Fprintf(w, `{"error":"rejected with %d"}`, reject)
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n",
		wsAcceptKey(r.Header.Get("Sec-Websocket-Key")))
	if p := r.Header.Get("Sec-Websocket-Protocol"); p != "" {
		fmt.Fprintf(rw, "Sec-WebSocket-Protocol: %s\r\n", strings.TrimSpace(strings.Split(p, ",")[0]))
	}
	_, _ = rw.WriteString("\r\n")
	_ = rw.Flush()
	send := func(v string) { _, _ = conn.Write(encodeFrame(true, opText, []byte(v), nil)) }
	if r.URL.Path == "/v1/realtime" {
		send(`{"type":"session.created"}`)
	}
	for {
		op, p, err := readWSFrame(rw.Reader)
		if err != nil {
			return
		}
		switch op {
		case opClose:
			_, _ = conn.Write(encodeFrame(true, opClose, p, nil))
			return
		case opPing:
			_, _ = conn.Write(encodeFrame(true, opPong, p, nil))
		case opBinary:
			send(`{"type":"transcript.done","text":"hello world","duration":3.2}`)
			_, _ = conn.Write(encodeFrame(true, opClose, closeFrame(1000, "done"), nil))
			return
		case opText:
			var ev struct{ Type string }
			_ = json.Unmarshal(p, &ev)
			switch ev.Type {
			case "conversation.item.create":
				send(`{"type":"response.output_audio_transcript.delta","delta":"Hi there"}`)
				send(`{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`)
			case "text.delta":
				_, _ = conn.Write(encodeFrame(true, opBinary, make([]byte, 300), nil))
			case "text.done":
				send(`{"type":"audio.done"}`)
				_, _ = conn.Write(encodeFrame(true, opClose, closeFrame(1000, ""), nil))
				return
			case "boom":
				send(`{"type":"error","error":{"message":"something broke"}}`)
				_, _ = conn.Write(encodeFrame(true, opClose, closeFrame(1011, "internal"), nil))
				return
			}
		}
	}
}

// ---- client ----

type wsConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
	resp *http.Response
	key  string
}

func (hs *harness) dialWS(path string, hdr ...string) *wsConn {
	hs.t.Helper()
	u, _ := url.Parse(hs.srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		hs.t.Fatal(err)
	}
	hs.t.Cleanup(func() { conn.Close() })
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: keep-alive, Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", path, u.Host, key)
	for _, h := range hdr {
		b.WriteString(h + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		hs.t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		hs.t.Fatal(err)
	}
	return &wsConn{t: hs.t, conn: conn, br: br, resp: resp, key: key}
}

func (c *wsConn) send(op byte, p []byte) {
	c.t.Helper()
	if _, err := c.conn.Write(encodeFrame(true, op, p, []byte{1, 2, 3, 4})); err != nil {
		c.t.Fatal(err)
	}
}

// readUntil reads frames until one whose text contains sub (or a close).
func (c *wsConn) readUntil(sub string) (byte, []byte) {
	c.t.Helper()
	for {
		op, p, err := readWSFrame(c.br)
		if err != nil {
			c.t.Fatalf("waiting for %q: %v", sub, err)
		}
		if op == opClose || strings.Contains(string(p), sub) {
			return op, p
		}
	}
}

func extraStats(t *testing.T, l *store.Log) wsStats {
	t.Helper()
	var s wsStats
	if err := json.Unmarshal([]byte(l.Extra), &s); err != nil {
		t.Fatalf("extra %q: %v", l.Extra, err)
	}
	return s
}

func TestWebSocketRealtime(t *testing.T) {
	hs := newHarness(t, nil)
	up := newWSUp(t)
	k := hs.addKey("k1", up.srv.srv.URL, nil)
	c := hs.dialWS("/v1/realtime?model=grok-voice", "Authorization: Bearer "+hs.token, "X-Grok-Trace: abc")
	if c.resp.StatusCode != 101 || c.resp.Header.Get("Sec-Websocket-Accept") != wsAcceptKey(c.key) ||
		c.resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("handshake: %d %v", c.resp.StatusCode, c.resp.Header)
	}
	c.readUntil("session.created")
	c.send(opPing, []byte("hi"))
	if op, _ := c.readUntil("hi"); op != opPong {
		t.Fatalf("expected pong, got op %d", op)
	}
	c.send(opText, []byte(`{"type":"conversation.item.create","item":{"content":[{"type":"input_text","text":"hey"}]}}`))
	c.readUntil("response.done")
	c.send(opClose, closeFrame(1000, "bye"))
	if op, p := c.readUntil("\x00"); op != opClose || binary.BigEndian.Uint16(p) != 1000 {
		t.Fatalf("close echo: %d %q", op, p)
	}
	l := hs.lastLog(1)
	if !l.WS || l.Status != 101 || l.KeyID != k.ID || l.Model != "grok-voice" || l.Error != "" {
		t.Fatalf("log: %+v", l)
	}
	if l.PromptTokens != 10 || l.CompletionTokens != 20 || l.RespText != "Hi there" {
		t.Fatalf("usage: %+v", l)
	}
	if l.CostTicks < billing.RealtimeTicksPerText || l.CostSource != "computed" {
		t.Fatalf("cost %d %s", l.CostTicks, l.CostSource)
	}
	s := extraStats(t, l)
	if s.TextInputs != 1 || s.ClientMessages != 1 || s.ServerMessages < 3 || s.ClosedBy != "client" || s.CloseCode != 1000 ||
		s.LoggedMessages == 0 {
		t.Fatalf("stats: %+v", s)
	}
	mustContain(t, "transcript", l.RespBody, "→ client", "← upstream", "[close 1000] bye")
	h := up.headers()[0]
	if h.Get("Authorization") != "Bearer "+k.Key || h.Get("Sec-Websocket-Key") != c.key || h.Get("X-Grok-Trace") != "abc" {
		t.Fatalf("upstream handshake headers: %v", h)
	}
	if len(l.Attempts) != 1 || l.Attempts[0].Status != 101 {
		t.Fatalf("attempts: %+v", l.Attempts)
	}
	tok, _ := hs.db.GetToken(context.Background(), hs.tok.ID)
	if tok.UsedTicks != l.CostTicks {
		t.Fatalf("token charged %d, log %d", tok.UsedTicks, l.CostTicks)
	}
}

func TestWebSocketSubprotocolAuth(t *testing.T) {
	hs := newHarness(t, nil)
	up := newWSUp(t)
	hs.addKey("k1", up.srv.srv.URL, nil)

	c := hs.dialWS("/v1/realtime", "Sec-WebSocket-Protocol: realtime, xai-client-secret."+hs.token)
	if c.resp.StatusCode != 101 || c.resp.Header.Get("Sec-Websocket-Protocol") != "realtime" {
		t.Fatalf("handshake: %d %v", c.resp.StatusCode, c.resp.Header)
	}
	if got := up.headers()[0].Get("Sec-Websocket-Protocol"); got != "realtime" {
		t.Fatalf("credential forwarded upstream: %q", got)
	}
	c.send(opClose, closeFrame(1000, ""))
	c.readUntil("\x00")
	l := hs.lastLog(1)
	if strings.Contains(l.ReqHeaders, hs.token) || !strings.Contains(l.ReqHeaders, "xai-client-secret.") {
		t.Fatalf("credential not masked: %s", l.ReqHeaders)
	}

	// Only the credential offered: the browser needs it echoed back.
	c = hs.dialWS("/v1/realtime", "Sec-WebSocket-Protocol: xai-client-secret."+hs.token)
	if c.resp.StatusCode != 101 || c.resp.Header.Get("Sec-Websocket-Protocol") != "xai-client-secret."+hs.token {
		t.Fatalf("handshake: %d %v", c.resp.StatusCode, c.resp.Header)
	}
	c.send(opClose, closeFrame(1000, ""))
	c.readUntil("\x00")

	c = hs.dialWS("/v1/realtime", "Sec-WebSocket-Protocol: xai-client-secret.wrong")
	if c.resp.StatusCode != 401 {
		t.Fatalf("bad credential: %d", c.resp.StatusCode)
	}
}

func TestWebSocketTTSAndSTT(t *testing.T) {
	hs := newHarness(t, nil)
	hs.addKey("k1", newWSUp(t).srv.srv.URL, nil)

	c := hs.dialWS("/v1/tts", "Authorization: Bearer "+hs.token)
	c.send(opText, []byte(`{"type":"text.delta","delta":"héllo"}`))
	if op, _ := c.readUntil("\x00"); op != opBinary {
		t.Fatalf("expected audio, got op %d", op)
	}
	c.send(opText, []byte(`{"type":"text.done"}`))
	if op, _ := c.readUntil("\xff\xff"); op != opClose {
		t.Fatalf("expected close, got %d", op)
	}
	l := hs.lastLog(1)
	s := extraStats(t, l)
	if s.TTSChars != 5 || l.CostTicks != 5*billing.TTSTicksPerChar || s.ClosedBy != "upstream" || l.Error != "" {
		t.Fatalf("tts: %+v %+v", s, l)
	}
	mustContain(t, "tts transcript", l.RespBody, "<binary 300 bytes>")

	c = hs.dialWS("/v1/stt", "Authorization: Bearer "+hs.token)
	c.send(opBinary, make([]byte, 70000)) // 64-bit length frame
	c.readUntil("transcript.done")
	c.readUntil("\xff\xff")
	l = hs.lastLog(2)
	s = extraStats(t, l)
	if s.AudioSeconds != 3.2 || l.RespText != "hello world" ||
		l.CostTicks != billing.WSCost(billing.WSUsage{Path: "/v1/stt", AudioSecond: 3.2}) || l.CostTicks == 0 {
		t.Fatalf("stt: %+v cost %d", s, l.CostTicks)
	}
}

func TestWebSocketUpstreamErrorEvent(t *testing.T) {
	hs := newHarness(t, nil)
	hs.addKey("k1", newWSUp(t).srv.srv.URL, nil)
	c := hs.dialWS("/v1/realtime", "Authorization: Bearer "+hs.token)
	c.readUntil("session.created")
	c.send(opText, []byte(`{"type":"boom"}`))
	c.readUntil("\xff\xff")
	l := hs.lastLog(1)
	if l.Error != "upstream error event: something broke" {
		t.Fatalf("error: %q", l.Error)
	}
	if s := extraStats(t, l); s.CloseCode != 1011 || s.CloseReason != "internal" || s.ClosedBy != "upstream" {
		t.Fatalf("stats: %+v", s)
	}
}

func TestWebSocketHandshakeFailures(t *testing.T) {
	t.Run("model not allowed", func(t *testing.T) {
		hs := newHarness(t, nil)
		hs.addKey("k1", newWSUp(t).srv.srv.URL, nil)
		limited, _ := hs.addToken("limited", func(t *store.Token) { t.AllowedModels = []string{"grok-voice"} })
		c := hs.dialWS("/v1/realtime?model=grok-other", "Authorization: Bearer "+limited)
		if c.resp.StatusCode != 403 {
			t.Fatalf("status %d", c.resp.StatusCode)
		}
	})
	t.Run("no keys", func(t *testing.T) {
		hs := newHarness(t, nil)
		if c := hs.dialWS("/v1/realtime", "Authorization: Bearer "+hs.token); c.resp.StatusCode != 503 {
			t.Fatalf("status %d", c.resp.StatusCode)
		}
	})
	t.Run("busy", func(t *testing.T) {
		hs := newHarness(t, nil)
		hs.addKey("k1", newWSUp(t).srv.srv.URL, func(k *store.Key) { k.MaxConcurrency = 1 })
		c := hs.dialWS("/v1/realtime", "Authorization: Bearer "+hs.token)
		c.readUntil("session.created")
		if c2 := hs.dialWS("/v1/realtime", "Authorization: Bearer "+hs.token); c2.resp.StatusCode != 429 {
			t.Fatalf("status %d", c2.resp.StatusCode)
		}
	})
	t.Run("failover then success", func(t *testing.T) {
		hs := newHarness(t, nil)
		bad, good := newWSUp(t), newWSUp(t)
		bad.setReject(401)
		kb := hs.addKey("bad", bad.srv.srv.URL, func(k *store.Key) { k.Priority = 0 })
		hs.addKey("good", good.srv.srv.URL, func(k *store.Key) { k.Priority = 1 })
		c := hs.dialWS("/v1/realtime", "Authorization: Bearer "+hs.token)
		if c.resp.StatusCode != 101 {
			t.Fatalf("status %d", c.resp.StatusCode)
		}
		c.send(opClose, closeFrame(1000, ""))
		c.readUntil("\x00")
		l := hs.lastLog(1)
		if len(l.Attempts) != 2 || l.Attempts[0].Status != 401 || l.Attempts[1].Status != 101 {
			t.Fatalf("attempts: %+v", l.Attempts)
		}
		if !hs.pool.State(kb.ID).AutoDisabled {
			t.Fatal("rejected key not disabled")
		}
	})
	t.Run("client error is relayed", func(t *testing.T) {
		hs := newHarness(t, nil)
		a, b := newWSUp(t), newWSUp(t)
		a.setReject(400)
		b.setReject(400)
		hs.addKey("a", a.srv.srv.URL, func(k *store.Key) { k.Priority = 0 })
		hs.addKey("b", b.srv.srv.URL, func(k *store.Key) { k.Priority = 1 })
		c := hs.dialWS("/v1/realtime", "Authorization: Bearer "+hs.token)
		body, _ := io.ReadAll(c.resp.Body)
		if c.resp.StatusCode != 400 || !strings.Contains(string(body), "rejected with 400") {
			t.Fatalf("got %d %s", c.resp.StatusCode, body)
		}
		if len(b.headers()) != 0 {
			t.Fatal("non-retryable rejection should not fail over")
		}
	})
	t.Run("unreachable and bad base URL", func(t *testing.T) {
		hs := newHarness(t, nil)
		gone := httptest.NewServer(http.NotFoundHandler())
		dead := gone.URL
		gone.Close()
		hs.addKey("dead", dead, func(k *store.Key) { k.Priority = 0 })
		hs.addKey("ftp", "ftp://example.com", func(k *store.Key) { k.Priority = 1 })
		c := hs.dialWS("/v1/realtime", "Authorization: Bearer "+hs.token)
		if c.resp.StatusCode != 502 {
			t.Fatalf("status %d", c.resp.StatusCode)
		}
		if l := hs.lastLog(1); len(l.Attempts) != 2 {
			t.Fatalf("attempts: %+v", l.Attempts)
		}
	})
}

func TestCloseWebSockets(t *testing.T) {
	hs := newHarness(t, nil)
	hs.addKey("k1", newWSUp(t).srv.srv.URL, nil)
	c := hs.dialWS("/v1/realtime", "Authorization: Bearer "+hs.token)
	c.readUntil("session.created")
	deadline := time.Now().Add(5 * time.Second)
	for hs.h.ActiveWebSockets() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("session not registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.h.CloseWebSockets(ctx)
	if _, _, err := readWSFrame(c.br); err == nil {
		t.Fatal("client connection still open")
	}
	if n := hs.h.ActiveWebSockets(); n != 0 {
		t.Fatalf("%d sessions still active", n)
	}
	if l := hs.lastLog(1); !l.WS || l.Status != 101 {
		t.Fatalf("log: %+v", l)
	}
}

func TestWSSessionChargeQuota(t *testing.T) {
	hs := newHarness(t, nil)
	tok := &store.Token{ID: 99, QuotaTicks: billing.RealtimeTicksPerMinute / 2, Limited: true}
	st := &state{h: hs.h, s: hs.set.Get(), token: tok, log: &store.Log{Path: "/v1/realtime"}}
	ss := newWSSession(st, &wsStats{}, 10)
	if !ss.charge(10 * time.Second) {
		t.Fatal("within quota")
	}
	if !ss.charge(10 * time.Second) { // nothing new accrued
		t.Fatal("no new cost must not end the session")
	}
	if ss.charge(time.Minute) {
		t.Fatal("quota exceeded but session allowed to continue")
	}
	ss.finish()
	if st.log.CostTicks != billing.RealtimeTicksPerMinute || st.preCharged != st.log.CostTicks {
		t.Fatalf("cost %d precharged %d", st.log.CostTicks, st.preCharged)
	}
}

func TestWSSessionLogging(t *testing.T) {
	hs := newHarness(t, func(s *config.Settings) { s.LogResponseBodyMaxKB = 1 })
	st := &state{h: hs.h, s: hs.set.Get(), token: &store.Token{}, log: &store.Log{Path: "/v1/realtime"}}
	ss := newWSSession(st, &wsStats{}, 2)
	ss.message("upstream", opText, []byte(`{"type":"response.text.delta","delta":"a"}`), 40, false)
	ss.message("upstream", opText, []byte(`{"type":"response.output_text.delta"`), 99999, true)
	ss.message("client", opBinary, nil, 10, false) // over the message log limit
	for i := 0; i < 50; i++ {
		ss.note(strings.Repeat("x", 100))
	}
	ss.finish()
	if st.log.RespText != "a" || !st.log.Truncated {
		t.Fatalf("log: %+v", st.log)
	}
	mustContain(t, "transcript", st.log.RespBody, "…[99999 bytes]", "• gateway")
	if strings.Contains(st.log.RespBody, "<binary") {
		t.Fatal("message beyond the log limit recorded")
	}
	if s := extraStats(t, st.log); s.LoggedMessages != 2 {
		t.Fatalf("stats: %+v", s)
	}
}

func TestWSProtocolHelpers(t *testing.T) {
	h := http.Header{}
	h.Add("Sec-Websocket-Protocol", "a, xai-client-secret.tok1")
	h.Add("Sec-Websocket-Protocol", " ,b")
	sec, cred, rest := wsProtocols(h)
	if sec != "tok1" || cred != "xai-client-secret.tok1" || strings.Join(rest, ",") != "a,b" {
		t.Fatalf("got %q %q %q", sec, cred, rest)
	}
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Upgrade", "WebSocket")
	r.Header.Set("Connection", "keep-alive, Upgrade")
	if !isWebSocket(r) {
		t.Fatal("websocket not detected")
	}
	r.Header.Set("Connection", "keep-alive")
	if isWebSocket(r) {
		t.Fatal("upgrade token required")
	}
}
