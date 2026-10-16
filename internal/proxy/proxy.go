// Package proxy implements the /v1/* forwarding gateway.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/reqlog"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

const (
	maxNonStreamBody = 512 << 20 // upstream non-streaming responses are buffered up to this size
	maxErrorBody     = 4 << 20
)

// Handler forwards client requests to xAI.
type Handler struct {
	DB       *store.DB
	Pool     *pool.Pool
	Prices   *billing.Table
	Logs     *reqlog.Writer
	Settings *config.Holder
	Client   *http.Client
	Log      *slog.Logger
}

// NewTransport returns the upstream HTTP transport.
func NewTransport(connectTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   connectTimeout,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 0, // bounded by the per-request timer instead
	}
}

// reqInfo is what the gateway reads from a client request body.
type reqInfo struct {
	Model              string
	Stream             bool
	PreviousResponseID string
	Deferred           bool
	Quality            string
	Resolution         string
}

// state collects everything about one client request for the log.
type state struct {
	h        *Handler
	s        *config.Settings
	start    time.Time
	log      *store.Log
	token    *store.Token
	info     reqInfo
	reqBody  []byte
	ttfbSet  bool
	attempts []store.Attempt
}

func (st *state) markTTFB() {
	if !st.ttfbSet {
		st.ttfbSet = true
		st.log.TTFBMs = time.Since(st.start).Milliseconds()
	}
}

// ServeHTTP handles one client request.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s := h.Settings.Get()
	setCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	st := &state{h: h, s: s, start: time.Now()}
	st.log = &store.Log{
		RequestID: secret.Random(24),
		CreatedAt: st.start.UnixMilli(),
		Method:    r.Method,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
		ClientIP:  clientIP(r, s.TrustedProxies),
		UserAgent: truncateStr(r.UserAgent(), 300),
	}
	w.Header().Set("X-Request-Id", st.log.RequestID)
	defer st.finish(r)

	// 1. Authenticate the client token.
	tok, err := h.authenticate(r)
	if err != nil {
		st.fail(w, err.(*apiError))
		return
	}
	st.token = tok
	st.log.TokenID, st.log.TokenName = tok.ID, tok.Name

	// 2. Read the body with a hard limit (applies to chunked bodies too).
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = http.MaxBytesReader(w, r.Body, s.MaxBodyBytes())
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				st.fail(w, errf(http.StatusRequestEntityTooLarge, "request_too_large",
					"request body exceeds the %d MB limit", s.MaxBodyMB))
				return
			}
			st.fail(w, errf(http.StatusBadRequest, "invalid_request", "failed to read request body: %v", err))
			return
		}
		st.reqBody = body
	}
	st.info = parseRequest(r.Header.Get("Content-Type"), st.reqBody)
	st.log.Model, st.log.Stream = st.info.Model, st.info.Stream

	// 3. Token policy: model allow-list.
	if st.info.Model != "" && !h.modelAllowed(tok, st.info.Model) {
		st.fail(w, errf(http.StatusForbidden, "model_not_allowed", "this token is not allowed to use model %q", st.info.Model))
		return
	}

	// 4. Ask the upstream to report usage on streaming chat requests.
	outBody := st.reqBody
	if st.info.Stream && s.InjectStreamUsage && r.URL.Path == "/v1/chat/completions" {
		outBody = injectStreamUsage(outBody)
	}

	// 5. Pick keys: resources created by a key must be read with that key.
	var bound *store.Binding
	if kind, id := referencedResource(r.Method, r.URL.Path); kind != "" {
		bound, _ = h.DB.GetBinding(r.Context(), id)
	}
	if bound == nil && st.info.PreviousResponseID != "" {
		bound, _ = h.DB.GetBinding(r.Context(), st.info.PreviousResponseID)
	}
	var boundKey int64
	if bound != nil {
		boundKey = bound.KeyID
	}
	candidates := h.Pool.Candidates(boundKey)
	if len(candidates) == 0 {
		if bound != nil {
			st.fail(w, errf(http.StatusServiceUnavailable, "upstream_key_unavailable",
				"the key that created this resource is no longer in the pool"))
			return
		}
		st.fail(w, errf(http.StatusServiceUnavailable, "no_upstream_keys", "no upstream API keys are available"))
		return
	}

	// 6. Try keys in order.
	var lastErr *upstreamError
	tried, busy := 0, 0
	for _, k := range candidates {
		if tried >= s.MaxAttempts {
			break
		}
		lease, why := h.Pool.Acquire(k.ID)
		if lease == nil {
			busy++
			h.Log.Debug("skip saturated key", "key_id", k.ID, "reason", why)
			continue
		}
		tried++
		done, uerr := st.attempt(w, r, lease, outBody, len(st.attempts)+1, tried < s.MaxAttempts)
		lease.Release()
		if done {
			if bound != nil && st.log.Model == "" {
				st.log.Model = bound.Model
			}
			return
		}
		lastErr = uerr
		if r.Context().Err() != nil {
			st.log.Status, st.log.Error = 499, "client closed request"
			return
		}
	}
	switch {
	case lastErr != nil:
		if n := len(st.attempts); n > 0 {
			st.log.KeyID, st.log.KeyName = st.attempts[n-1].KeyID, st.attempts[n-1].KeyName
		}
		st.writeUpstreamError(w, lastErr)
	case busy > 0:
		st.fail(w, errf(http.StatusTooManyRequests, "upstream_keys_busy",
			"all upstream keys are at their concurrency or rate limit; retry shortly"))
	default:
		st.fail(w, errf(http.StatusServiceUnavailable, "no_upstream_keys", "no upstream API keys are available"))
	}
}

// upstreamError is a failed attempt whose response may be returned to the
// client when no key succeeds.
type upstreamError struct {
	status int
	header http.Header
	body   []byte
	err    string
}

// attempt performs one upstream request with key k. It returns done=true when
// a response has been (or started being) written to the client.
func (st *state) attempt(w http.ResponseWriter, r *http.Request, lease *pool.Lease, body []byte, seq int, canRetry bool) (bool, *upstreamError) {
	k := lease.Key
	s := st.s
	at := store.Attempt{Seq: seq, KeyID: k.ID, KeyName: k.Name, StartedAt: time.Now().UnixMilli()}
	t0 := time.Now()
	record := func(status int, errMsg, action string) {
		at.Status, at.Error, at.Action = status, truncateStr(errMsg, 2000), action
		at.LatencyMs = time.Since(t0).Milliseconds()
		st.attempts = append(st.attempts, at)
	}

	ctx, cancel := context.WithCancelCause(r.Context())
	defer cancel(nil)
	timer := time.AfterFunc(s.RequestTimeout(), func() { cancel(errUpstreamTimeout) })
	defer timer.Stop()

	url := strings.TrimRight(k.BaseURL, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		url += "?" + r.URL.RawQuery
	}
	var rdr io.Reader
	if len(body) > 0 {
		rdr = bytes.NewReader(body)
	}
	ureq, err := http.NewRequestWithContext(ctx, r.Method, url, rdr)
	if err != nil {
		record(0, err.Error(), pool.ActionRetry)
		return false, &upstreamError{status: http.StatusBadGateway, err: err.Error()}
	}
	for name, vals := range r.Header {
		if shouldForward(name) {
			ureq.Header[name] = vals
		}
	}
	ureq.Header.Set("Authorization", "Bearer "+k.Key)
	ureq.ContentLength = int64(len(body))

	resp, err := st.h.Client.Do(ureq)
	if err != nil {
		if r.Context().Err() != nil {
			record(0, "client closed request", pool.ActionReturn)
			return false, nil
		}
		timeout := errors.Is(context.Cause(ctx), errUpstreamTimeout)
		v := st.h.Pool.Report(k.ID, pool.Outcome{NetErr: err, Timeout: timeout})
		record(0, v.Reason, v.Action)
		status := http.StatusBadGateway
		if timeout {
			status = http.StatusGatewayTimeout
		}
		return false, &upstreamError{status: status, err: v.Reason}
	}
	defer resp.Body.Close()
	at.TTFBMs = time.Since(t0).Milliseconds()

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		v := st.h.Pool.Report(k.ID, pool.Outcome{Status: resp.StatusCode, Body: string(errBody), Header: resp.Header})
		if v.Retry && canRetry {
			record(resp.StatusCode, v.Reason, v.Action)
			return false, &upstreamError{status: resp.StatusCode, header: resp.Header, body: errBody, err: v.Reason}
		}
		// Not retryable (a client error) or out of attempts: return it as-is.
		record(resp.StatusCode, v.Reason, v.Action)
		st.log.KeyID, st.log.KeyName = k.ID, k.Name
		ue := &upstreamError{status: resp.StatusCode, header: resp.Header, body: errBody, err: v.Reason}
		if !v.Retry {
			st.writeUpstreamError(w, ue)
			return true, nil
		}
		return false, ue
	}

	st.h.Pool.Report(k.ID, pool.Outcome{Status: resp.StatusCode})
	st.log.KeyID, st.log.KeyName = k.ID, k.Name
	isSSE := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	copyResponseHeaders(w.Header(), resp.Header)
	st.log.RespHeaders = reqlog.HeadersJSON(resp.Header)
	st.log.Status = resp.StatusCode
	if isSSE {
		st.log.Stream = true
		timer.Stop()
		record(resp.StatusCode, st.streamBody(ctx, cancel, w, resp), pool.ActionSuccess)
		return true, nil
	}
	// Non-streaming: buffer the body so usage can be read, then forward it.
	full, rerr := io.ReadAll(io.LimitReader(resp.Body, maxNonStreamBody))
	timer.Stop()
	if rerr != nil {
		if r.Context().Err() != nil {
			record(resp.StatusCode, "client closed request", pool.ActionSuccess)
			st.log.Status, st.log.Error = 499, "client closed request"
			return true, nil
		}
		// Headers are not sent yet, so another key can still be tried.
		v := st.h.Pool.Report(k.ID, pool.Outcome{NetErr: rerr, Timeout: errors.Is(context.Cause(ctx), errUpstreamTimeout)})
		record(resp.StatusCode, "reading upstream body: "+v.Reason, v.Action)
		st.log.KeyID, st.log.KeyName, st.log.Status = 0, "", 0
		return false, &upstreamError{status: http.StatusBadGateway, err: v.Reason}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(full)))
	w.WriteHeader(resp.StatusCode)
	st.markTTFB()
	if _, err := w.Write(full); err != nil {
		st.log.Error = "client write failed: " + err.Error()
	}
	record(resp.StatusCode, "", pool.ActionSuccess)
	st.handleBody(r, full)
	return true, nil
}

var errUpstreamTimeout = errors.New("upstream timeout")

// streamBody forwards an SSE response chunk by chunk, flushing each one, while
// parsing events for usage and assembling the text for the log.
func (st *state) streamBody(ctx context.Context, cancel context.CancelCauseFunc, w http.ResponseWriter, resp *http.Response) string {
	s := st.s
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	maxLog := s.LogBodyMaxBytes()
	capt := newCapture(maxLog)
	ss := &billing.StreamState{MaxText: maxLog}
	parser := &sseParser{onEvent: ss.AddEvent}

	idle := s.StreamIdleTimeout()
	watchdog := time.AfterFunc(idle, func() { cancel(errStreamIdle) })
	defer watchdog.Stop()

	buf := make([]byte, 32<<10)
	var errMsg string
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			watchdog.Reset(idle)
			chunk := buf[:n]
			st.markTTFB()
			if _, werr := w.Write(chunk); werr != nil {
				errMsg = "client went away"
				st.log.Status = 499
				break
			}
			_ = rc.Flush()
			parser.Write(chunk)
			if s.LogBodies {
				capt.Write(chunk)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				switch {
				case errors.Is(context.Cause(ctx), errStreamIdle):
					errMsg = fmt.Sprintf("stream idle for %s; aborted", idle)
				case ctx.Err() != nil:
					errMsg = "client went away"
					st.log.Status = 499
				default:
					errMsg = "upstream stream error: " + err.Error()
				}
			}
			break
		}
	}
	parser.Close()
	if errMsg != "" {
		st.log.Error = errMsg
	}
	st.log.UpstreamID = ss.ID
	if ss.Model != "" && st.log.Model == "" {
		st.log.Model = ss.Model
	}
	st.log.RespText = ss.Text.String()
	st.log.ReasoningText = ss.Reasoning.String()
	if s.LogBodies {
		st.log.RespBody = string(capt.Bytes())
		st.log.Truncated = st.log.Truncated || capt.truncated || ss.Truncated
	}
	st.applyUsage(ss.Usage)
	if ss.ID != "" && st.log.Path == "/v1/responses" {
		st.bind(KindResponse, ss.ID)
	}
	return errMsg
}

var errStreamIdle = errors.New("stream idle timeout")

// handleBody extracts usage, ids and bindings from a non-streaming body.
func (st *state) handleBody(r *http.Request, body []byte) {
	u, env := billing.ParseBody(body)
	if env.Model != "" && st.log.Model == "" {
		st.log.Model = env.Model
	}
	st.log.UpstreamID = firstNonEmpty(env.ID, env.RequestID)
	if st.s.LogBodies {
		st.log.RespBody, st.log.Truncated = reqlog.SanitizeBody(body, st.s.LogBodyMaxBytes())
		if u.Found && len(body) <= 8<<20 {
			t, rs := billing.ExtractText(body)
			limit := st.s.LogBodyMaxBytes()
			st.log.RespText, st.log.ReasoningText = truncateStr(t, limit), truncateStr(rs, limit)
		}
	}
	// Async jobs: the poll that returns the finished job carries the cost of
	// the whole job. Charge it once, to whoever polls first.
	if kind, id, ok := isAsyncPoll(r.Method, r.URL.Path); ok && u.HasCost {
		if b, err := st.h.DB.GetBinding(r.Context(), id); err == nil {
			if st.log.Model == "" {
				st.log.Model = b.Model
			}
			first, err := st.h.DB.MarkBindingBilled(r.Context(), id)
			if err == nil && !first {
				u.HasCost, u.CostTicks = false, 0
				u.VideoSeconds, u.Images = 0, 0
				st.log.Error = kind + " job already billed by an earlier poll"
			}
		}
	}
	if !u.HasCost && (env.Status == "pending" || r.Method == http.MethodGet) {
		// Polls of pending jobs and plain GETs are free; don't price them.
		u.VideoSeconds, u.Images = 0, 0
	}
	st.applyUsage(u)
	if kind := createdResourceKind(r.Method, r.URL.Path, st.info.Deferred); kind != "" {
		id := env.ID
		if kind == KindVideo || kind == KindImage || kind == KindDeferred {
			id = firstNonEmpty(env.RequestID, env.ID)
		}
		st.bind(kind, id)
	}
}

func (st *state) applyUsage(u billing.Usage) {
	u.Finalize()
	l := st.log
	l.PromptTokens, l.CompletionTokens, l.CachedTokens = u.PromptTokens, u.CompletionTokens, u.CachedTokens
	l.ReasoningTokens, l.ImageTokens, l.TotalTokens = u.ReasoningTokens, u.ImageTokens, u.TotalTokens
	l.Images, l.VideoSeconds = u.Images, u.VideoSeconds
	l.CostTicks, l.CostSource = st.h.Prices.Cost(l.Model, u, billing.Request{
		Quality: st.info.Quality, Resolution: st.info.Resolution, Edit: st.log.Path == "/v1/images/edits"})
}

func (st *state) bind(kind, id string) {
	if id == "" || st.log.KeyID == 0 {
		return
	}
	ttl := time.Duration(st.s.TaskBindingTTLHours) * time.Hour
	b := &store.Binding{ResourceID: id, Kind: kind, KeyID: st.log.KeyID, TokenID: st.log.TokenID,
		Model: st.log.Model, ExpiresAt: time.Now().Add(ttl).UnixMilli()}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := st.h.DB.PutBinding(ctx, b); err != nil {
		st.h.Log.Error("store binding", "kind", kind, "id", id, "err", err)
	}
}

// writeUpstreamError returns an upstream error response to the client
// unmodified (full body, original status).
func (st *state) writeUpstreamError(w http.ResponseWriter, ue *upstreamError) {
	st.log.Status = ue.status
	st.log.Error = ue.err
	if ue.header != nil {
		copyResponseHeaders(w.Header(), ue.header)
		st.log.RespHeaders = reqlog.HeadersJSON(ue.header)
	}
	body := ue.body
	if len(body) == 0 {
		body = errorJSON("upstream_error", ue.err)
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(ue.status)
	st.markTTFB()
	_, _ = w.Write(body)
	if st.s.LogBodies {
		st.log.RespBody, _ = reqlog.SanitizeBody(body, st.s.LogBodyMaxBytes())
	}
}

// fail writes a gateway-generated error.
func (st *state) fail(w http.ResponseWriter, e *apiError) {
	st.log.Status, st.log.Error = e.status, e.message
	body := errorJSON(e.code, e.message)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(e.status)
	st.markTTFB()
	_, _ = w.Write(body)
	if st.s.LogBodies {
		st.log.RespBody = string(body)
	}
}

// finish completes and enqueues the log entry.
func (st *state) finish(r *http.Request) {
	s := st.s
	if !s.LogEnabled {
		return
	}
	l := st.log
	l.LatencyMs = time.Since(st.start).Milliseconds()
	l.Attempts = st.attempts
	if l.Status == 0 {
		l.Status = 499
		if l.Error == "" {
			l.Error = "client closed request"
		}
	}
	if s.LogBodies {
		l.HasBodies = true
		l.ReqHeaders = reqlog.HeadersJSON(r.Header)
		var trunc bool
		l.ReqBody, trunc = reqlog.SanitizeBody(st.reqBody, s.LogBodyMaxBytes())
		l.Truncated = l.Truncated || trunc
		if l.RespHeaders == "" {
			l.RespHeaders = "{}"
		}
	}
	st.h.Logs.Enqueue(l)
}

// ---- helpers ----

type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func errf(status int, code, format string, a ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, a...)}
}

func errorJSON(code, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{
		"message": msg, "type": "xai_router_error", "code": code}})
	return b
}

func (h *Handler) authenticate(r *http.Request) (*store.Token, error) {
	raw := ""
	if a := r.Header.Get("Authorization"); a != "" {
		if len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
			raw = strings.TrimSpace(a[7:])
		}
	}
	if raw == "" {
		raw = strings.TrimSpace(r.Header.Get("X-Api-Key"))
	}
	if raw == "" {
		return nil, errf(http.StatusUnauthorized, "missing_api_key",
			"missing API key: send 'Authorization: Bearer <token>' (or 'x-api-key')")
	}
	hash := secret.Hash(raw)
	tok, err := h.DB.GetTokenByHash(r.Context(), hash)
	if err != nil || !secret.Equal(tok.Hash, hash) {
		return nil, errf(http.StatusUnauthorized, "invalid_api_key", "invalid API key")
	}
	if !tok.Enabled {
		return nil, errf(http.StatusForbidden, "token_disabled", "this API key is disabled")
	}
	if tok.ExpiresAt > 0 && time.Now().UnixMilli() >= tok.ExpiresAt {
		return nil, errf(http.StatusForbidden, "token_expired", "this API key has expired")
	}
	if tok.QuotaTicks > 0 && tok.UsedTicks >= tok.QuotaTicks {
		return nil, errf(http.StatusTooManyRequests, "insufficient_quota",
			"this API key has used its quota of $%.2f", billing.TicksToUSD(tok.QuotaTicks))
	}
	return tok, nil
}

// modelAllowed checks the token's allow-list, resolving aliases.
func (h *Handler) modelAllowed(tok *store.Token, model string) bool {
	if len(tok.AllowedModels) == 0 {
		return true
	}
	names := []string{strings.ToLower(model)}
	if m := h.Prices.Lookup(model); m != nil {
		names = append(names, strings.ToLower(m.ID))
	}
	for _, pat := range tok.AllowedModels {
		pat = strings.ToLower(strings.TrimSpace(pat))
		for _, n := range names {
			if pat == n || pat == "*" || (strings.HasSuffix(pat, "*") && strings.HasPrefix(n, strings.TrimSuffix(pat, "*"))) {
				return true
			}
		}
	}
	return false
}

func parseRequest(contentType string, body []byte) reqInfo {
	var ri reqInfo
	if len(body) == 0 || !strings.Contains(strings.ToLower(contentType), "json") && body[0] != '{' {
		return ri
	}
	var v struct {
		Model              string          `json:"model"`
		Stream             bool            `json:"stream"`
		PreviousResponseID string          `json:"previous_response_id"`
		Deferred           bool            `json:"deferred"`
		Quality            string          `json:"quality"`
		Resolution         json.RawMessage `json:"resolution"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ri
	}
	ri = reqInfo{Model: v.Model, Stream: v.Stream, PreviousResponseID: v.PreviousResponseID, Deferred: v.Deferred, Quality: v.Quality}
	_ = json.Unmarshal(v.Resolution, &ri.Resolution)
	return ri
}

// injectStreamUsage sets stream_options.include_usage=true unless the client
// already chose a value.
func injectStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body
	}
	opts := map[string]json.RawMessage{}
	if raw, ok := m["stream_options"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &opts) != nil {
			return body
		}
		if _, set := opts["include_usage"]; set {
			return body
		}
	}
	opts["include_usage"] = json.RawMessage("true")
	ob, _ := json.Marshal(opts)
	m["stream_options"] = ob
	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if dropResponseHeaders[k] {
			continue
		}
		dst[k] = append([]string(nil), vv...)
	}
}

func setCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, Anthropic-Version, Anthropic-Beta, X-Grok-Conv-Id, Idempotency-Key, OpenAI-Beta, X-Stainless-Arch, X-Stainless-Lang, X-Stainless-OS, X-Stainless-Package-Version, X-Stainless-Runtime, X-Stainless-Runtime-Version, X-Stainless-Retry-Count, X-Stainless-Timeout")
	h.Set("Access-Control-Expose-Headers", "X-Request-Id")
	h.Set("Access-Control-Max-Age", "86400")
}

// clientIP returns the client address, honouring X-Forwarded-For / X-Real-IP
// only when the direct peer is a trusted proxy.
func clientIP(r *http.Request, trusted []string) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil || !ipTrusted(peer, trusted) {
		return host
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		// Walk from the right, skipping trusted proxies.
		for i := len(parts) - 1; i >= 0; i-- {
			ip := net.ParseIP(strings.TrimSpace(parts[i]))
			if ip == nil {
				break
			}
			if !ipTrusted(ip, trusted) || i == 0 {
				return ip.String()
			}
		}
	}
	if xr := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-Ip"))); xr != nil {
		return xr.String()
	}
	return host
}

func ipTrusted(ip net.IP, trusted []string) bool {
	for _, t := range trusted {
		if strings.Contains(t, "/") {
			if _, n, err := net.ParseCIDR(t); err == nil && n.Contains(ip) {
				return true
			}
		} else if p := net.ParseIP(t); p != nil && p.Equal(ip) {
			return true
		}
	}
	return false
}

// truncateStr cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }
