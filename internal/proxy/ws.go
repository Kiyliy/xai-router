package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/netutil"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/reqlog"
	"github.com/Kiyliy/xai-router/internal/store"
)

// WebSocket passthrough (/v1/realtime, streaming /v1/tts and /v1/stt, and
// any other Upgrade request). The handshake goes through the same token
// authentication, quota check and key selection/failover as HTTP requests;
// after 101 the gateway relays raw frames in both directions and observes
// them for the log and for billing. Compression extensions are not
// negotiated so that frames stay inspectable.

const clientSecretProto = "xai-client-secret."

func isWebSocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && headerHasToken(r.Header, "Connection", "upgrade")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, p := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(p), token) {
				return true
			}
		}
	}
	return false
}

// wsProtocols splits Sec-WebSocket-Protocol into the gateway credential (a
// browser cannot set Authorization on a WebSocket, so like xAI the token may
// be sent as the subprotocol "xai-client-secret.<token>") and the rest.
func wsProtocols(h http.Header) (secret, credProto string, rest []string) {
	for _, v := range h.Values("Sec-Websocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			switch {
			case p == "":
			case strings.HasPrefix(p, clientSecretProto):
				secret, credProto = strings.TrimPrefix(p, clientSecretProto), p
			default:
				rest = append(rest, p)
			}
		}
	}
	return
}

// wsRegistry tracks hijacked connections so shutdown can end them and wait
// for their logs.
type wsRegistry struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func (r *wsRegistry) add(c ...net.Conn) {
	r.mu.Lock()
	if r.conns == nil {
		r.conns = map[net.Conn]struct{}{}
	}
	for _, x := range c {
		r.conns[x] = struct{}{}
	}
	r.mu.Unlock()
}

func (r *wsRegistry) remove(c ...net.Conn) {
	r.mu.Lock()
	for _, x := range c {
		delete(r.conns, x)
	}
	r.mu.Unlock()
}

// CloseWebSockets closes all relayed WebSocket connections and waits (up to
// the context deadline) until their sessions have been logged.
func (h *Handler) CloseWebSockets(ctx context.Context) {
	h.ws.mu.Lock()
	for c := range h.ws.conns {
		_ = c.Close()
	}
	h.ws.mu.Unlock()
	done := make(chan struct{})
	go func() { h.ws.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ActiveWebSockets returns the number of relayed connections (both sides).
func (h *Handler) ActiveWebSockets() int {
	h.ws.mu.Lock()
	defer h.ws.mu.Unlock()
	return len(h.ws.conns) / 2
}

// wsStats is stored in the log's extra JSON.
type wsStats struct {
	DurationMs     int64   `json:"duration_ms"`
	ClientMessages int64   `json:"client_messages"`
	ServerMessages int64   `json:"server_messages"`
	ClientBytes    int64   `json:"client_bytes"`
	ServerBytes    int64   `json:"server_bytes"`
	CloseCode      int     `json:"close_code,omitempty"`
	CloseReason    string  `json:"close_reason,omitempty"`
	ClosedBy       string  `json:"closed_by"`
	Subprotocol    string  `json:"subprotocol,omitempty"`
	TextInputs     int64   `json:"text_inputs,omitempty"`
	TTSChars       int64   `json:"tts_chars,omitempty"`
	AudioSeconds   float64 `json:"audio_seconds,omitempty"`
	LoggedMessages int     `json:"logged_messages"`
}

func (st *state) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	h, s := st.h, st.s
	st.log.WS = true
	q := r.URL.Query()
	st.info.Model = q.Get("model")
	st.log.Model = st.info.Model
	if len(q["model"]) > 1 || len(q["call_id"]) > 1 {
		st.fail(w, errf(http.StatusBadRequest, "ambiguous_model", "the model query parameter is given more than once"))
		return
	}
	if st.info.Model == "" && len(st.token.AllowedModels) > 0 && r.URL.Path != "/v1/tts" {
		// Fail closed: the upstream would pick a default model.
		st.fail(w, errf(http.StatusBadRequest, "model_required",
			"this API key is restricted to specific models; set ?model= on the WebSocket URL"))
		return
	}
	if st.info.Model != "" && !h.modelAllowed(st.token, st.info.Model) {
		st.fail(w, errf(http.StatusForbidden, "model_not_allowed", "this token is not allowed to use model %q", st.info.Model))
		return
	}
	if m := h.Prices.Lookup(st.info.Model); m != nil && !m.Enabled {
		st.fail(w, errf(http.StatusForbidden, "model_disabled", "the model %q is disabled on this gateway", st.info.Model))
		return
	}
	if st.res = h.Quota.Reserve(st.token, 0); st.res == nil {
		st.fail(w, errf(http.StatusTooManyRequests, "insufficient_quota", "this API key has no quota left"))
		return
	}
	candidates := h.Pool.Candidates(0)
	if len(candidates) == 0 {
		st.fail(w, errf(http.StatusServiceUnavailable, "no_upstream_keys", "no upstream API keys are available"))
		return
	}
	var lastErr *upstreamError
	tried := 0
	for _, k := range candidates {
		if tried >= s.MaxAttempts {
			break
		}
		lease, _ := h.Pool.Acquire(k.ID)
		if lease == nil {
			continue
		}
		tried++
		up, ue, retry := st.wsDial(r, lease.Key, len(st.attempts)+1)
		if up == nil {
			lease.Release()
			lastErr = ue
			if !retry {
				break
			}
			continue
		}
		st.wsRelay(w, r, up)
		lease.Release()
		return
	}
	if lastErr != nil {
		if n := len(st.attempts); n > 0 {
			st.log.KeyID, st.log.KeyName = st.attempts[n-1].KeyID, st.attempts[n-1].KeyName
		}
		st.writeUpstreamError(w, lastErr)
		return
	}
	st.fail(w, errf(http.StatusTooManyRequests, "upstream_keys_busy",
		"all upstream keys are at their concurrency or rate limit; retry shortly"))
}

// upstreamWS is an established upstream WebSocket connection.
type upstreamWS struct {
	conn net.Conn
	br   *bufio.Reader
	resp *http.Response
	key  *store.Key
	at   store.Attempt
	t0   time.Time
}

// wsDial performs the upstream handshake. On failure it returns the error to
// relay and whether another key may be tried.
func (st *state) wsDial(r *http.Request, k *store.Key, seq int) (*upstreamWS, *upstreamError, bool) {
	s := st.s
	at := store.Attempt{Seq: seq, KeyID: k.ID, KeyName: k.Name, StartedAt: time.Now().UnixMilli()}
	t0 := time.Now()
	record := func(status int, msg, action string) {
		at.Status, at.Error, at.Action = status, truncateStr(msg, 2000), action
		at.LatencyMs = time.Since(t0).Milliseconds()
		st.attempts = append(st.attempts, at)
	}
	fail := func(err error) (*upstreamWS, *upstreamError, bool) {
		v := st.h.Pool.Report(k.ID, pool.Outcome{NetErr: err, RequestID: st.log.RequestID})
		record(0, v.Reason, v.Action)
		return nil, &upstreamError{status: http.StatusBadGateway, err: v.Reason}, true
	}
	base, err := url.Parse(k.BaseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") {
		return fail(fmt.Errorf("invalid base URL"))
	}
	host := base.Host
	if base.Port() == "" {
		if base.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(s.ConnectTimeoutSeconds)*time.Second)
	defer cancel()
	var conn net.Conn
	if err := s.UpstreamPolicy().CheckURL(k.BaseURL); err != nil {
		return fail(err)
	}
	dial := netutil.GuardedDialer(func() time.Duration { return time.Duration(s.ConnectTimeoutSeconds) * time.Second },
		func(string) bool { return s.AllowPrivateUpstreams })
	conn, err = dial(ctx, "tcp", host)
	if err != nil {
		return fail(err)
	}
	if base.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: base.Hostname(), NextProtos: []string{"http/1.1"},
			MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return fail(err)
		}
		conn = tc
	}
	// Build the upstream handshake.
	path := strings.TrimRight(base.Path, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n", path, base.Host)
	fmt.Fprintf(&b, "Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: %s\r\n",
		r.Header.Get("Sec-Websocket-Key"), r.Header.Get("Sec-Websocket-Version"))
	if _, _, rest := wsProtocols(r.Header); len(rest) > 0 {
		fmt.Fprintf(&b, "Sec-WebSocket-Protocol: %s\r\n", strings.Join(rest, ", "))
	}
	fmt.Fprintf(&b, "Authorization: Bearer %s\r\n", k.Key)
	for name, vals := range r.Header {
		if shouldForward(name) && name != "Content-Type" {
			for _, v := range vals {
				if !strings.ContainsAny(v, "\r\n") {
					fmt.Fprintf(&b, "%s: %s\r\n", name, v)
				}
			}
		}
	}
	b.WriteString("\r\n")
	_ = conn.SetDeadline(time.Now().Add(time.Duration(s.ConnectTimeoutSeconds) * time.Second))
	if _, err := io.WriteString(conn, b.String()); err != nil {
		conn.Close()
		return fail(err)
	}
	br := bufio.NewReaderSize(conn, 32<<10)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return fail(err)
	}
	_ = conn.SetDeadline(time.Time{})
	at.TTFBMs = time.Since(t0).Milliseconds()
	if resp.StatusCode == http.StatusSwitchingProtocols {
		st.h.Pool.Report(k.ID, pool.Outcome{Status: resp.StatusCode})
		return &upstreamWS{conn: conn, br: br, resp: resp, key: k, at: at, t0: t0}, nil, false
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	conn.Close()
	v := st.h.Pool.Report(k.ID, pool.Outcome{Status: resp.StatusCode, Body: string(body), Header: resp.Header, RequestID: st.log.RequestID,
		Echo: st.echo()})
	record(resp.StatusCode, v.Reason, v.Action)
	return nil, &upstreamError{status: resp.StatusCode, header: resp.Header, body: body, err: v.Reason}, v.Retry
}

// wsRelay completes the client handshake and relays frames until either side
// closes.
func (st *state) wsRelay(w http.ResponseWriter, r *http.Request, up *upstreamWS) {
	h, s := st.h, st.s
	k := up.key
	st.log.KeyID, st.log.KeyName = k.ID, k.Name
	st.log.Status = http.StatusSwitchingProtocols
	st.log.RespHeaders = reqlog.HeadersJSON(up.resp.Header)

	client, crw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		up.conn.Close()
		st.log.Status, st.log.Error = 500, "hijack failed: "+err.Error()
		return
	}
	_ = client.SetDeadline(time.Time{})
	// Answer the client with the upstream's accept key (computed from the
	// client's own Sec-WebSocket-Key, which was forwarded).
	_, credProto, _ := wsProtocols(r.Header)
	proto := up.resp.Header.Get("Sec-Websocket-Protocol")
	if proto == "" && credProto != "" {
		proto = credProto // the browser requires its offered subprotocol back
	}
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n")
	fmt.Fprintf(&b, "Sec-WebSocket-Accept: %s\r\n", up.resp.Header.Get("Sec-Websocket-Accept"))
	if proto != "" {
		fmt.Fprintf(&b, "Sec-WebSocket-Protocol: %s\r\n", proto)
	}
	fmt.Fprintf(&b, "X-Request-Id: %s\r\n\r\n", st.log.RequestID)
	_, err = crw.WriteString(b.String())
	if err == nil {
		err = crw.Flush()
	}
	st.markTTFB()
	if err != nil {
		client.Close()
		up.conn.Close()
		st.log.Status, st.log.Error = 499, "client went away during handshake"
		return
	}
	h.ws.add(client, up.conn)
	h.ws.wg.Add(1)
	defer h.ws.wg.Done()
	defer h.ws.remove(client, up.conn)

	stats := &wsStats{Subprotocol: proto}
	sess := newWSSession(st, stats, s.WSLogMessages)
	start := time.Now()

	var closeOnce sync.Once
	var closedBy atomic.Value
	shut := func(by string) {
		closeOnce.Do(func() {
			closedBy.Store(by)
			client.Close()
			up.conn.Close()
		})
	}

	// Quota enforcement and running cost for long sessions.
	stop := make(chan struct{})
	var tickerWG sync.WaitGroup
	tickerWG.Add(1)
	go func() {
		defer tickerWG.Done()
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if !sess.charge(time.Since(start)) {
					sess.note("gateway: token quota exhausted, closing session")
					shut("gateway (quota exhausted)")
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // client -> upstream
		defer wg.Done()
		pipe(up.conn, crw.Reader, sess.clientObs)
		shut("client")
	}()
	go func() { // upstream -> client
		defer wg.Done()
		pipe(client, up.br, sess.serverObs)
		shut("upstream")
	}()
	wg.Wait()
	close(stop)
	tickerWG.Wait()

	dur := time.Since(start)
	sess.charge(dur)
	stats.DurationMs = dur.Milliseconds()
	stats.ClientMessages, stats.ClientBytes = sess.clientObs.messages, sess.clientObs.bytes
	stats.ServerMessages, stats.ServerBytes = sess.serverObs.messages, sess.serverObs.bytes
	if v, ok := closedBy.Load().(string); ok && stats.ClosedBy == "" {
		stats.ClosedBy = v
	}
	sess.finish()
	up.at.Status, up.at.Action = http.StatusSwitchingProtocols, pool.ActionSuccess
	up.at.LatencyMs = time.Since(up.t0).Milliseconds()
	st.attempts = append(st.attempts, up.at)
}

// pipe copies src to dst, feeding every chunk to the observer.
func pipe(dst io.Writer, src io.Reader, obs *wsObserver) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			obs.Feed(buf[:n])
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// wsSession accumulates what is learned from the frames.
type wsSession struct {
	st        *state
	stats     *wsStats
	clientObs *wsObserver
	serverObs *wsObserver

	mu         sync.Mutex
	transcript strings.Builder
	logLimit   int
	logged     int
	usage      billing.Usage
	text       strings.Builder
	charged    int64
}

func newWSSession(st *state, stats *wsStats, logLimit int) *wsSession {
	ss := &wsSession{st: st, stats: stats, logLimit: logLimit}
	inspect := 64 << 10
	ss.clientObs = &wsObserver{maxInspect: inspect,
		onMessage: func(op byte, p []byte, size int64, clip bool) { ss.message("client", op, p, size, clip) },
		onClose:   func(code int, reason string) { ss.closed("client", code, reason) }}
	ss.serverObs = &wsObserver{maxInspect: inspect,
		onMessage: func(op byte, p []byte, size int64, clip bool) { ss.message("upstream", op, p, size, clip) },
		onClose:   func(code int, reason string) { ss.closed("upstream", code, reason) }}
	return ss
}

func (ss *wsSession) closed(by string, code int, reason string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.stats.ClosedBy == "" {
		ss.stats.ClosedBy, ss.stats.CloseCode, ss.stats.CloseReason = by, code, reason
	}
	ss.appendLine(by, fmt.Sprintf("[close %d] %s", code, reason))
}

func (ss *wsSession) note(msg string) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.appendLine("gateway", msg)
}

func (ss *wsSession) appendLine(by, text string) {
	arrow := "→"
	switch by {
	case "upstream":
		arrow = "←"
	case "gateway":
		arrow = "•"
	}
	if ss.transcript.Len() < ss.st.s.LogResponseBodyMaxBytes() {
		ss.transcript.WriteString(arrow + " " + by + " " + text + "\n")
	} else {
		ss.st.log.Truncated = true
	}
}

// wsEvent is the subset of realtime / tts / stt JSON events used for billing.
type wsEvent struct {
	Type     string  `json:"type"`
	Text     string  `json:"text"`
	Delta    string  `json:"delta"`
	Duration float64 `json:"duration"`
	Item     *struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	} `json:"item"`
	Response *struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	} `json:"response"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (ss *wsSession) message(by string, op byte, p []byte, size int64, clipped bool) {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if op == opText && !clipped {
		var ev wsEvent
		if json.Unmarshal(p, &ev) == nil {
			ss.account(by, &ev)
		}
	}
	if ss.logged >= ss.logLimit || !ss.st.s.LogBodies {
		return
	}
	ss.logged++
	ss.stats.LoggedMessages = ss.logged
	switch {
	case op == opText:
		body, _ := reqlog.SanitizeBody(p, 4096)
		if clipped {
			body += fmt.Sprintf(" …[%d bytes]", size)
		}
		ss.appendLine(by, body)
	default:
		ss.appendLine(by, fmt.Sprintf("<binary %d bytes>", size))
	}
}

// account updates usage counters from one event (caller holds ss.mu).
func (ss *wsSession) account(by string, ev *wsEvent) {
	path := ss.st.log.Path
	switch {
	case by == "client" && path == "/v1/tts" && ev.Type == "text.delta":
		ss.stats.TTSChars += int64(utf8.RuneCountInString(ev.Delta) + utf8.RuneCountInString(ev.Text))
	case by == "client" && strings.HasPrefix(path, "/v1/realtime") && ev.Type == "conversation.item.create" && ev.Item != nil:
		for _, c := range ev.Item.Content {
			if c.Type == "input_text" {
				ss.stats.TextInputs++
				break
			}
		}
	case by == "upstream" && ev.Type == "response.done" && ev.Response != nil && ev.Response.Usage != nil:
		u := ev.Response.Usage
		ss.usage.Found = true
		ss.usage.PromptTokens += u.InputTokens
		ss.usage.CompletionTokens += u.OutputTokens
		ss.usage.TotalTokens += u.TotalTokens
	case by == "upstream" && ev.Type == "transcript.done":
		ss.stats.AudioSeconds += ev.Duration
		ss.text.WriteString(ev.Text)
	case by == "upstream" && ev.Type == "error" && ev.Error != nil && ss.st.log.Error == "":
		ss.st.log.Error = truncateStr("upstream error event: "+ev.Error.Message, 500)
	case by == "upstream" && (ev.Type == "response.output_audio_transcript.delta" || ev.Type == "response.text.delta" ||
		ev.Type == "response.output_text.delta"):
		if ss.text.Len() < ss.st.s.LogResponseBodyMaxBytes() {
			ss.text.WriteString(ev.Delta)
		}
	}
}

// cost returns the session cost so far.
func (ss *wsSession) cost(elapsed time.Duration) int64 {
	return billing.WSCost(billing.WSUsage{Path: ss.st.log.Path, DurationMs: elapsed.Milliseconds(),
		TextInputs: ss.stats.TextInputs, TTSChars: ss.stats.TTSChars, AudioSecond: ss.stats.AudioSeconds})
}

// charge books the cost accrued since the last call against the token's
// quota and reports whether the session may continue.
func (ss *wsSession) charge(elapsed time.Duration) bool {
	ss.mu.Lock()
	total := ss.cost(elapsed)
	delta := total - ss.charged
	ss.charged = total
	ss.mu.Unlock()
	if delta <= 0 {
		return !ss.st.token.Limited || ss.st.h.Quota.Remaining(ss.st.token) > 0
	}
	return ss.st.h.Quota.Charge(ss.st.token, delta)
}

func (ss *wsSession) finish() {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	l := ss.st.log
	stats := ss.stats
	extra, _ := json.Marshal(stats)
	l.Extra = string(extra)
	l.RespBody = ss.transcript.String()
	l.RespText = ss.text.String()
	u := ss.usage
	u.Finalize()
	l.PromptTokens, l.CompletionTokens, l.TotalTokens = u.PromptTokens, u.CompletionTokens, u.TotalTokens
	l.CostTicks = ss.charged
	l.CostSource = "none"
	if l.CostTicks > 0 {
		l.CostSource = "computed"
	}
	ss.st.preCharged = ss.charged
	if stats.CloseReason != "" && stats.CloseCode != 1000 && l.Error == "" {
		l.Error = fmt.Sprintf("closed by %s: %d %s", stats.ClosedBy, stats.CloseCode, stats.CloseReason)
	}
}
