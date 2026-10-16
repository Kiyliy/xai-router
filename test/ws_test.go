package test

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// ---- minimal WebSocket helpers (RFC 6455) ----

func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

func writeFrame(w io.Writer, op byte, payload []byte, mask bool) error {
	hdr := []byte{0x80 | op}
	mbit := byte(0)
	if mask {
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
	body := payload
	if mask {
		var k [4]byte
		_, _ = rand.Read(k[:])
		hdr = append(hdr, k[:]...)
		body = make([]byte, len(payload))
		for i := range payload {
			body[i] = payload[i] ^ k[i%4]
		}
	}
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func readFrame(r *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	op := h[0] & 0x0f
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
	return op, p, nil
}

func closePayload(code int, reason string) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(code)), reason...)
}

// ---- mock upstream WebSocket endpoints ----

type wsSeen struct {
	auth, protocol, extensions string
}

// serveMockWS implements tiny versions of /v1/realtime, /v1/tts and /v1/stt.
func (m *mockXAI) serveMockWS(w http.ResponseWriter, r *http.Request, key string) {
	m.mu.Lock()
	m.wsSeen = append(m.wsSeen, wsSeen{auth: r.Header.Get("Authorization"),
		protocol: r.Header.Get("Sec-Websocket-Protocol"), extensions: r.Header.Get("Sec-Websocket-Extensions")})
	m.mu.Unlock()
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
		wsAccept(r.Header.Get("Sec-Websocket-Key")))
	_ = rw.Flush()
	send := func(v string) { _ = writeFrame(conn, 0x1, []byte(v), false) }
	switch r.URL.Path {
	case "/v1/realtime":
		send(`{"type":"session.created","session":{"model":"grok-voice-latest"}}`)
	case "/v1/stt":
		send(`{"type":"transcript.created"}`)
	}
	for {
		op, p, err := readFrame(rw.Reader)
		if err != nil {
			return
		}
		switch op {
		case 0x8:
			_ = writeFrame(conn, 0x8, p, false) // echo close
			return
		case 0x2: // stt audio
			send(`{"type":"transcript.partial","text":"hel","is_final":false}`)
		case 0x1:
			var ev struct{ Type, Delta string }
			_ = json.Unmarshal(p, &ev)
			switch ev.Type {
			case "conversation.item.create":
				send(`{"type":"response.output_audio_transcript.delta","delta":"Hi there"}`)
				send(`{"type":"response.done","response":{"id":"r1","status":"completed","usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`)
			case "text.delta":
				_ = writeFrame(conn, 0x2, make([]byte, 3000), false)
			case "text.done":
				send(`{"type":"audio.done"}`)
			case "audio.done":
				send(`{"type":"transcript.done","text":"hello world","duration":3.2}`)
				_ = writeFrame(conn, 0x8, closePayload(1000, "done"), false)
				return
			}
		}
	}
}

// ---- client ----

type wsClient struct {
	conn net.Conn
	br   *bufio.Reader
	resp *http.Response
}

func (e *env) dialWS(path string, hdr http.Header) (*wsClient, error) {
	u, _ := url.Parse(e.srv.URL)
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Extensions: permessage-deflate\r\n", path, u.Host, key)
	for k, vs := range hdr {
		for _, v := range vs {
			fmt.Fprintf(&b, "%s: %s\r\n", k, v)
		}
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(conn, b.String()); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, err
	}
	c := &wsClient{conn: conn, br: br, resp: resp}
	if resp.StatusCode == 101 && resp.Header.Get("Sec-Websocket-Accept") != wsAccept(key) {
		return nil, fmt.Errorf("bad accept key %q", resp.Header.Get("Sec-Websocket-Accept"))
	}
	return c, nil
}

func (c *wsClient) send(op byte, p []byte) error { return writeFrame(c.conn, op, p, true) }

func (c *wsClient) recv(t *testing.T) (byte, string) {
	t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	op, p, err := readFrame(c.br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return op, string(p)
}

type wsLog struct {
	ID               int64
	Status           int
	Attempts         int
	KeyName          string  `json:"key_name"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	CostSource       string  `json:"cost_source"`
	Error            string
	WebSocket        bool `json:"websocket"`
	Session          struct {
		DurationMs     int64   `json:"duration_ms"`
		ClientMessages int64   `json:"client_messages"`
		ServerMessages int64   `json:"server_messages"`
		ClientBytes    int64   `json:"client_bytes"`
		ServerBytes    int64   `json:"server_bytes"`
		CloseCode      int     `json:"close_code"`
		CloseReason    string  `json:"close_reason"`
		ClosedBy       string  `json:"closed_by"`
		TextInputs     int64   `json:"text_inputs"`
		TTSChars       int64   `json:"tts_chars"`
		AudioSeconds   float64 `json:"audio_seconds"`
	} `json:"websocket_session"`
	ResponseBody string            `json:"response_body"`
	ResponseText string            `json:"response_text"`
	ReqHeaders   map[string]string `json:"request_headers"`
}

func (e *env) lastWSLog(t *testing.T) wsLog {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var page struct{ Data []wsLog }
		e.api("GET", "/api/v1/logs?path=/v1/", nil, 200, &page)
		for _, l := range page.Data {
			if l.WebSocket {
				var d wsLog
				e.api("GET", fmt.Sprintf("/api/v1/logs/%d", l.ID), nil, 200, &d)
				return d
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no websocket log written")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ---- tests ----

func TestWebSocketRealtime(t *testing.T) {
	e := newEnv(t, "xai-a-401", "xai-b-ok")
	c, err := e.dialWS("/v1/realtime?model=grok-voice-latest", http.Header{"Authorization": {"Bearer " + e.client}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	if c.resp.StatusCode != 101 {
		t.Fatalf("handshake status %d", c.resp.StatusCode)
	}
	if _, msg := c.recv(t); !strings.Contains(msg, "session.created") {
		t.Fatalf("first message %q", msg)
	}
	_ = c.send(0x1, []byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"Hello"}]}}`))
	c.recv(t)
	if _, msg := c.recv(t); !strings.Contains(msg, "response.done") {
		t.Fatalf("expected response.done, got %q", msg)
	}
	_ = c.send(0x8, closePayload(1000, "bye"))
	if op, _ := c.recv(t); op != 0x8 {
		t.Fatalf("expected close echo, got op %d", op)
	}
	c.conn.Close()

	// The upstream got the pool key (after failing over), no compression, no
	// client token.
	e.mock.mu.Lock()
	seen := e.mock.wsSeen
	e.mock.mu.Unlock()
	if len(seen) != 1 || seen[0].auth != "Bearer xai-b-ok" || seen[0].extensions != "" {
		t.Fatalf("upstream handshakes: %+v", seen)
	}
	l := e.lastWSLog(t)
	s := l.Session
	if l.Status != 101 || l.Attempts != 2 || l.KeyName != "k2" {
		t.Fatalf("log: status %d attempts %d key %s", l.Status, l.Attempts, l.KeyName)
	}
	if s.ClientMessages != 1 || s.ServerMessages != 3 || s.CloseCode != 1000 || s.CloseReason != "bye" ||
		s.ClosedBy != "client" || s.TextInputs != 1 || s.ClientBytes == 0 || s.ServerBytes == 0 {
		t.Fatalf("session stats: %+v", s)
	}
	if l.PromptTokens != 10 || l.CompletionTokens != 20 || l.CostSource != "computed" || l.CostUSD < 0.004 {
		t.Fatalf("usage/cost: %+v", l)
	}
	if !strings.Contains(l.ResponseBody, "conversation.item.create") || !strings.Contains(l.ResponseBody, "response.done") ||
		l.ResponseText != "Hi there" {
		t.Fatalf("transcript: %q text %q", l.ResponseBody, l.ResponseText)
	}
}

func TestWebSocketSubprotocolAuthAndTTS(t *testing.T) {
	e := newEnv(t, "xai-ok")
	c, err := e.dialWS("/v1/tts?language=en", http.Header{"Sec-WebSocket-Protocol": {"xai-client-secret." + e.client}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	if c.resp.StatusCode != 101 || c.resp.Header.Get("Sec-Websocket-Protocol") != "xai-client-secret."+e.client {
		t.Fatalf("status %d protocol %q", c.resp.StatusCode, c.resp.Header.Get("Sec-Websocket-Protocol"))
	}
	_ = c.send(0x1, []byte(`{"type":"text.delta","delta":"héllo"}`))
	if op, _ := c.recv(t); op != 0x2 {
		t.Fatalf("expected binary audio, got op %d", op)
	}
	_ = c.send(0x1, []byte(`{"type":"text.done"}`))
	c.recv(t)
	_ = c.send(0x8, closePayload(1000, ""))
	c.recv(t)
	c.conn.Close()

	e.mock.mu.Lock()
	seen := e.mock.wsSeen[0]
	e.mock.mu.Unlock()
	if seen.protocol != "" || seen.auth != "Bearer xai-ok" {
		t.Fatalf("client secret leaked upstream: %+v", seen)
	}
	l := e.lastWSLog(t)
	if l.Session.TTSChars != 5 || l.CostUSD != 5*0.000015 {
		t.Fatalf("tts chars %d cost %v", l.Session.TTSChars, l.CostUSD)
	}
	if p := l.ReqHeaders["Sec-Websocket-Protocol"]; strings.Contains(p, e.client[10:]) {
		t.Fatalf("token stored in log: %s", p)
	}
}

func TestWebSocketSTTAndUpstreamClose(t *testing.T) {
	e := newEnv(t, "xai-ok")
	c, err := e.dialWS("/v1/stt", http.Header{"X-Api-Key": {e.client}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.conn.Close()
	c.recv(t)
	_ = c.send(0x2, make([]byte, 70000)) // 64-bit length frame
	c.recv(t)
	_ = c.send(0x1, []byte(`{"type":"audio.done"}`))
	if _, msg := c.recv(t); !strings.Contains(msg, "transcript.done") {
		t.Fatalf("got %q", msg)
	}
	if op, _ := c.recv(t); op != 0x8 {
		t.Fatalf("expected close")
	}
	l := e.lastWSLog(t)
	if l.Session.AudioSeconds != 3.2 || l.Session.ClosedBy != "upstream" || l.Session.CloseCode != 1000 ||
		l.Session.ClientBytes < 70000 || l.ResponseText != "hello world" {
		t.Fatalf("stt log: %+v text %q", l.Session, l.ResponseText)
	}
	if want := 3.2 * 0.20 / 3600; l.CostUSD < want*0.99 || l.CostUSD > want*1.01 {
		t.Fatalf("stt cost %v want %v", l.CostUSD, want)
	}
}

func TestWebSocketRejections(t *testing.T) {
	e := newEnv(t, "xai-a-401")
	c, err := e.dialWS("/v1/realtime", http.Header{"Authorization": {"Bearer sk-xr-wrong"}})
	if err != nil || c.resp.StatusCode != 401 {
		t.Fatalf("bad token: %v %v", err, c.resp.StatusCode)
	}
	// Only key is dead: the upstream error is returned as an HTTP response.
	c, err = e.dialWS("/v1/realtime", http.Header{"Authorization": {"Bearer " + e.client}})
	if err != nil || c.resp.StatusCode != 400 {
		t.Fatalf("dead key: %v %v", err, c.resp.StatusCode)
	}
	// Model allow-list applies to ?model=.
	var tok struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "t", "allowed_models": []string{"grok-4*"}}, 201, &tok)
	c, err = e.dialWS("/v1/realtime?model=grok-voice-latest", http.Header{"Authorization": {"Bearer " + tok.Token}})
	if err != nil || c.resp.StatusCode != 403 {
		t.Fatalf("model not allowed: %v %v", err, c.resp.StatusCode)
	}
}

func TestWebSocketQuotaClosesSession(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var tok struct {
		ID    int64
		Token string
	}
	// $0.001 buys 0.75 s of realtime ($0.08/min); the session is cut at the
	// next 2-second accounting tick.
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "t", "quota_usd": 0.001}, 201, &tok)
	c, err := e.dialWS("/v1/realtime", http.Header{"Authorization": {"Bearer " + tok.Token}})
	if err != nil || c.resp.StatusCode != 101 {
		t.Fatalf("dial: %v", err)
	}
	defer c.conn.Close()
	c.recv(t)
	_ = c.conn.SetReadDeadline(time.Now().Add(6 * time.Second))
	if _, _, err := readFrame(c.br); err == nil {
		t.Fatal("session not closed on quota exhaustion")
	}
	l := e.lastWSLog(t)
	if !strings.Contains(l.Session.ClosedBy, "quota") {
		t.Fatalf("closed_by %q", l.Session.ClosedBy)
	}
	// Further sessions are refused.
	time.Sleep(700 * time.Millisecond)
	c2, err := e.dialWS("/v1/realtime", http.Header{"Authorization": {"Bearer " + tok.Token}})
	if err != nil || c2.resp.StatusCode != 429 {
		t.Fatalf("expected 429 after quota spent: %v %v", err, c2.resp.StatusCode)
	}
}

func TestModelListsFilteredByToken(t *testing.T) {
	e := newEnv(t, "xai-ok")
	var tok struct{ Token string }
	e.api("POST", "/api/v1/tokens", map[string]any{"name": "t", "allowed_models": []string{"grok-4.3"}}, 201, &tok)
	var list struct {
		Data []struct{ ID string }
	}
	e.do("GET", "/v1/models", tok.Token, nil, 200, &list)
	if len(list.Data) != 1 || list.Data[0].ID != "grok-4.3" {
		t.Fatalf("filtered list: %+v", list.Data)
	}
	var lang struct {
		Models []struct{ ID string }
	}
	e.do("GET", "/v1/language-models", tok.Token, nil, 200, &lang)
	if len(lang.Models) != 1 || lang.Models[0].ID != "grok-4.3" {
		t.Fatalf("language models: %+v", lang.Models)
	}
	e.do("GET", "/v1/models/grok-build-0.1", tok.Token, nil, 404, nil)
	// Unrestricted tokens see everything.
	e.do("GET", "/v1/models", e.client, nil, 200, &list)
	if len(list.Data) != 2 {
		t.Fatalf("unfiltered list: %+v", list.Data)
	}
}
