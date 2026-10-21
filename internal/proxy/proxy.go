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
	"math"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Kiyliy/xai-router/internal/billing"
	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/netutil"
	"github.com/Kiyliy/xai-router/internal/pool"
	"github.com/Kiyliy/xai-router/internal/quota"
	"github.com/Kiyliy/xai-router/internal/reqlog"
	"github.com/Kiyliy/xai-router/internal/secret"
	"github.com/Kiyliy/xai-router/internal/store"
)

const (
	maxNonStreamBody = 64 << 20 // non-streaming responses up to this size are buffered and inspected
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
	Quota    *quota.Ledger
	ws       wsRegistry
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
	MaxTokens          int64
	TextChars          int64  // characters of "text" (text to speech)
	Invalid            string // why the body cannot be trusted (bad JSON or field types)
	Ambiguous          bool   // the model could not be determined reliably
	N                  int64
	Duration           int64
	InputImages        int64 // images attached to an image edit or video request
	ServiceTier        string
}

// state collects everything about one client request for the log.
type state struct {
	h          *Handler
	s          *config.Settings
	start      time.Time
	log        *store.Log
	token      *store.Token
	info       reqInfo
	reqBody    []byte
	ttfbSet    bool
	attempts   []store.Attempt
	res        *quota.Reservation
	jobID      string           // async job created by this request (quota hold)
	priceReq   *billing.Request // pricing parameters of the job a poll finishes
	preCharged int64            // cost already booked against the quota (WebSocket sessions)
	lastErrSig string           // last client-class upstream error (status + message)
	// releaseJob is the async job whose hold this request frees when it is
	// settled (the job finished, failed or expired).
	releaseJob string
	// pollBinding is the binding of the async job a GET poll refers to, when
	// this token created the job (its quota hold covers the poll).
	pollBinding *store.Binding
	// prevContextTokens is the size of the stored conversation a
	// previous_response_id pulls in (an estimate when it is unknown).
	prevContextTokens int64
	// stripUsage drops the usage-only chat chunk before it reaches a client
	// that did not ask for it (usage is always requested upstream).
	stripUsage bool
	// moreKeys is set while an attempt is not on the last candidate key: a
	// stream that failed before its first byte is then retried.
	moreKeys bool
}

// echo lists client-supplied values that upstream errors may repeat back;
// they are ignored when classifying errors.
func (st *state) echo() []string {
	return []string{st.info.Model, st.info.PreviousResponseID}
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
		ClientIP:  netutil.ClientIP(r, s.TrustedProxies),
		UserAgent: truncateStr(r.UserAgent(), 300),
	}
	w.Header().Set("X-Request-Id", st.log.RequestID)
	defer st.finish(r)

	// 1. Authenticate the client token. A token whose quota is used up may
	// still poll the async jobs it created: their cost is covered by the
	// job's hold, and only the poll can deliver, bill and release them.
	tok, err := h.authenticate(r)
	if tok != nil && !isWebSocket(r) {
		st.pollBinding = h.ownedPoll(r, tok)
	}
	if err != nil && (tok == nil || st.pollBinding == nil) {
		st.fail(w, err.(*apiError))
		return
	}
	st.token = tok
	st.log.TokenID, st.log.TokenName, st.log.TokenPrefix = tok.ID, tok.Name, tok.Prefix

	if isWebSocket(r) {
		st.serveWebSocket(w, r)
		return
	}
	// Model detail endpoints for models outside the token's allow-list look
	// like unknown models.
	if id := modelDetailID(r.Method, r.URL.Path); id != "" && !h.modelAllowed(tok, id) {
		st.fail(w, errf(http.StatusNotFound, "model_not_found", "the model %s does not exist", id))
		return
	}
	// /v1/api-key describes the upstream key (team, user, ACLs, the last
	// characters): never forward it on behalf of a client token.
	if strings.TrimRight(r.URL.Path, "/") == "/v1/api-key" {
		st.fail(w, errf(http.StatusNotFound, "not_found", "this endpoint is not available through the gateway"))
		return
	}

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

	// 3. Token policy: model allow-list, disabled and unpriced models.
	if e := h.admit(st, r); e != nil {
		st.fail(w, e)
		return
	}

	// 4. Resources created by a key must be read with that key.
	bound := st.pollBinding
	if bound == nil {
		if kind, id := referencedResource(r.Method, r.URL.Path); kind != "" {
			bound, _ = h.DB.GetBinding(r.Context(), id)
		}
	}
	if st.info.PreviousResponseID != "" {
		prev, _ := h.DB.GetBinding(r.Context(), st.info.PreviousResponseID)
		st.prevContextTokens = contextTokens(prev)
		if bound == nil {
			bound = prev
		}
	}
	if bound != nil && !ownsResource(tok.ID, bound.TokenID) {
		// Another token's response, job or file: indistinguishable from a
		// missing one.
		st.fail(w, errf(http.StatusNotFound, "not_found", "resource not found"))
		return
	}

	// 5. Reserve the request's worst-case cost against the token's quota, so
	// concurrent requests cannot overspend it. The poll of a job this token
	// created is covered by the job's hold and is never refused.
	if st.pollBinding != nil {
		st.res = h.Quota.Admit(tok)
	} else {
		est := st.estimate(r.Method, r.URL.Path)
		if st.res = h.Quota.Reserve(tok, est); st.res == nil {
			left := h.Quota.Remaining(tok)
			st.fail(w, errf(http.StatusTooManyRequests, "insufficient_quota",
				"this API key's remaining quota ($%.6f) does not cover this request (up to $%.6f reserved); quota is $%.2f",
				billing.TicksToUSD(left), billing.TicksToUSD(est), billing.TicksToUSD(tok.QuotaTicks)))
			return
		}
	}

	// 6. Streaming chat requests always ask the upstream for usage, whatever
	// the client chose: a stream without usage could only be estimated. The
	// usage chunk is passed to clients that asked for it (or, with
	// inject_stream_usage on, did not say) and dropped for the others.
	outBody := st.reqBody
	if st.info.Stream && r.URL.Path == "/v1/chat/completions" {
		var choice int
		outBody, choice = injectStreamUsage(outBody)
		st.stripUsage = choice == usageFalse || (choice == usageUnset && !s.InjectStreamUsage)
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

	// 7. Try keys in order.
	var lastErr *upstreamError
	tried, busy := 0, 0
	for i, k := range candidates {
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
		st.moreKeys = i < len(candidates)-1
		done, uerr := st.attempt(w, r, lease, outBody, len(st.attempts)+1, tried < s.MaxAttempts)
		lease.Release()
		if done {
			if bound != nil && st.log.Model == "" {
				st.log.Model = bound.Model
			}
			return
		}
		lastErr = uerr
		st.keepAttempt(uerr)
		if r.Context().Err() != nil {
			st.log.Status, st.log.Error = 499, "client closed request"
			return
		}
	}
	switch {
	case lastErr != nil:
		if n := len(st.attempts); n > 0 {
			st.log.KeyID, st.log.KeyName = st.attempts[n-1].KeyID, st.attempts[n-1].KeyName
			// The last attempt is the final row: a cost it reported is
			// billed there (below), not as a retried attempt.
			st.attempts[n-1].CostTicks = 0
		}
		st.billUpstreamError(lastErr)
		st.writeUpstreamError(w, lastErr)
	case busy > 0:
		st.fail(w, errf(http.StatusTooManyRequests, "upstream_keys_busy",
			"all upstream keys are at their concurrency or rate limit; retry shortly"))
	default:
		st.fail(w, errf(http.StatusServiceUnavailable, "no_upstream_keys", "no upstream API keys are available"))
	}
}

// keepAttempt keeps what a failed attempt answered for its own log row (it
// becomes a "retried" row when another attempt follows): the status it
// stands for, the response headers and body, and a cost the upstream may
// have reported in the error. Only the cost is kept: a retried row the
// upstream billed shows cost source "upstream" with no tokens or price
// snapshot behind it (store.Attempt has no usage fields). The final row is
// billed from the full usage instead (billUpstreamError).
func (st *state) keepAttempt(ue *upstreamError) {
	n := len(st.attempts)
	if ue == nil || n == 0 {
		return
	}
	a := &st.attempts[n-1]
	a.LogStatus = ue.status
	if u, _ := billing.ParseBody(ue.body); u.HasCost && u.CostTicks > 0 {
		a.CostTicks = u.CostTicks
	}
	if !st.s.LogEnabled || !st.s.LogBodies {
		return
	}
	if ue.header != nil {
		a.RespHeaders = reqlog.HeadersJSON(ue.header)
	}
	body := ue.body
	if len(body) == 0 {
		body = errorJSON("upstream_error", ue.err) // what the client would have been sent
	}
	a.RespBody, _ = reqlog.SanitizeBody(body, st.s.LogResponseBodyMaxBytes())
}

// billUpstreamError bills the final failed attempt with the usage the
// upstream reported in its error response, when that usage carries a cost:
// the upstream billed the attempt, so it is charged (once, on the final
// row, with cost source "upstream"). An error without a cost stays free.
func (st *state) billUpstreamError(ue *upstreamError) {
	if ue == nil {
		return
	}
	if u, _ := billing.ParseBody(ue.body); u.HasCost && u.CostTicks > 0 {
		st.applyUsage(u)
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

	// The upstream request outlives the client: xAI bills a request it has
	// started, so after a disconnect the response is still read (not
	// forwarded) to learn its usage. A non-streaming response stays bounded
	// by the request timeout; a stream by clientGoneDrain (see streamBody).
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(r.Context()))
	defer cancel(nil)
	timer := time.AfterFunc(s.RequestTimeout(), func() { cancel(errUpstreamTimeout) })
	defer timer.Stop()

	if err := s.UpstreamPolicy().CheckURL(k.BaseURL); err != nil {
		// The key's URL is not allowed by the current upstream policy.
		record(0, "blocked by upstream policy: "+err.Error(), pool.ActionRetry)
		return false, &upstreamError{status: http.StatusBadGateway, err: "upstream blocked by policy: " + err.Error()}
	}
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
			// The client left and the upstream never answered within the
			// request timeout: it may still have processed the prompt.
			record(0, "client closed request; "+err.Error(), pool.ActionReturn)
			if errors.Is(context.Cause(ctx), errUpstreamTimeout) {
				st.log.KeyID, st.log.KeyName = k.ID, k.Name
				st.applyEstimatedUsage(&billing.StreamState{})
			}
			return false, nil
		}
		timeout := errors.Is(context.Cause(ctx), errUpstreamTimeout)
		v := st.h.Pool.Report(k.ID, pool.Outcome{NetErr: err, Timeout: timeout, RequestID: st.log.RequestID})
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
		v := st.h.Pool.Report(k.ID, pool.Outcome{Status: resp.StatusCode, Body: string(errBody), Header: resp.Header, RequestID: st.log.RequestID,
			Echo: st.echo()})
		// The same client-class error from two keys is the request's fault,
		// not the keys': stop and return it. Errors the pool attributes to
		// the key (dead-key keywords, 401, cooldowns) never stop failover:
		// keys sharing an exhausted team all answer the same way.
		if v.Action == pool.ActionRetry && !v.Penalty && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			sig := strconv.Itoa(resp.StatusCode) + " " + pool.ErrorMessage(string(errBody), st.echo())
			if sig == st.lastErrSig {
				v.Retry, v.Action = false, pool.ActionReturn
			}
			st.lastErrSig = sig
		}
		if v.Retry && canRetry {
			record(resp.StatusCode, v.Reason, v.Action)
			return false, &upstreamError{status: resp.StatusCode, header: resp.Header, body: errBody, err: v.Reason}
		}
		// Not retryable (a client error) or out of attempts: return it as-is.
		record(resp.StatusCode, v.Reason, v.Action)
		st.log.KeyID, st.log.KeyName = k.ID, k.Name
		ue := &upstreamError{status: resp.StatusCode, header: resp.Header, body: errBody, err: v.Reason}
		if !v.Retry {
			st.billUpstreamError(ue)
			st.writeUpstreamError(w, ue)
			return true, nil
		}
		return false, ue
	}

	isSSE := strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	if !isSSE || !estimatedPaths[st.log.Path] {
		// A token-billed stream reports its own outcome (see streamBody):
		// its 200 is not a success until content or a clean end arrives.
		st.h.Pool.Report(k.ID, pool.Outcome{Status: resp.StatusCode})
	}
	st.log.KeyID, st.log.KeyName = k.ID, k.Name
	st.log.RespHeaders = reqlog.HeadersJSON(resp.Header)
	st.log.Status = resp.StatusCode
	if isSSE {
		// The headers are copied when the stream is committed to the client.
		st.log.Stream = true
		timer.Stop()
		sr := st.streamBody(ctx, r.Context(), cancel, w, resp, k.ID, canRetry && st.moreKeys)
		if sr.retry != nil {
			record(resp.StatusCode, sr.retry.err, sr.action)
			return false, sr.retry
		}
		action := pool.ActionSuccess
		if sr.action != "" {
			action = sr.action
		}
		record(resp.StatusCode, sr.errMsg, action)
		return true, nil
	}
	copyResponseHeaders(w.Header(), resp.Header)
	// Non-streaming: buffer the body so usage can be read, then forward it.
	full, rerr := io.ReadAll(io.LimitReader(resp.Body, maxNonStreamBody+1))
	if rerr == nil && len(full) > maxNonStreamBody {
		// Too large to inspect (e.g. a file download): pass it through
		// unbuffered instead of truncating it. Usage is not read. The request
		// timeout keeps bounding the transfer.
		w.WriteHeader(resp.StatusCode)
		st.markTTFB()
		n, err := w.Write(full)
		if err == nil {
			var m int64
			m, err = io.Copy(w, resp.Body)
			n += int(m)
		}
		if err != nil {
			st.log.Error = "relaying large response: " + err.Error()
		}
		record(resp.StatusCode, "", pool.ActionSuccess)
		st.log.RespBody = fmt.Sprintf("<%d+ byte response passed through without inspection>", n)
		st.log.Truncated = true
		return true, nil
	}
	timer.Stop()
	if rerr != nil {
		if r.Context().Err() != nil {
			record(resp.StatusCode, "client closed request", pool.ActionSuccess)
			st.log.Status, st.log.Error = 499, "client closed request"
			st.applyEstimatedUsage(&billing.StreamState{})
			return true, nil
		}
		// The upstream accepted the request (2xx headers) and then the body
		// failed: it has run, and billed, the request. Retrying on another
		// key would run it twice (a second video job, a second charge) with
		// the first never billed, so this is treated as an interrupted
		// stream: 502 to the client, cost estimated.
		v := st.h.Pool.Report(k.ID, pool.Outcome{NetErr: rerr, Timeout: errors.Is(context.Cause(ctx), errUpstreamTimeout), RequestID: st.log.RequestID})
		record(resp.StatusCode, "reading upstream body: "+v.Reason, pool.ActionReturn)
		st.writeUpstreamError(w, &upstreamError{status: http.StatusBadGateway,
			err: fmt.Sprintf("upstream response interrupted after %d headers: %s", resp.StatusCode, v.Reason)})
		st.applyEstimatedUsage(&billing.StreamState{})
		if st.log.CostTicks == 0 && st.res.Amount() > 0 {
			// Media and other requests not billed by tokens: bill the
			// reservation, the worst case the request was admitted for.
			st.log.CostTicks, st.log.CostSource = st.res.Amount(), "estimated"
		}
		return true, nil
	}
	full = st.filterModelList(r, full)
	full = st.filterOwnedList(r.Context(), r, full)
	w.Header().Set("Content-Length", strconv.Itoa(len(full)))
	w.WriteHeader(resp.StatusCode)
	st.markTTFB()
	if _, err := w.Write(full); err != nil {
		st.log.Error = "client write failed: " + err.Error()
	}
	record(resp.StatusCode, "", pool.ActionSuccess)
	st.handleBody(r, full)
	if r.Context().Err() != nil {
		// Billed from the body read after the client left.
		st.log.Status, st.log.Error = 499, "client closed request"
	}
	return true, nil
}

var (
	errUpstreamTimeout = errors.New("upstream timeout")
	errClientGone      = errors.New("client gone; drain limit reached")
)

// clientGoneDrain bounds how long an upstream response is still read after
// the client disconnected.
var clientGoneDrain = 60 * time.Second

// streamBody forwards an SSE response chunk by chunk, flushing each one, while
// parsing events for usage and assembling the text for the log.
//
// If the client disconnects, the rest of the stream is still read (but not
// forwarded) so the usage at its end is billed; the log keeps status 499.
// When no usage arrives at all, it is estimated from the content received.
//
// Token-billed streams are held back until their first generated content (at
// most firstByteHold): an upstream that answers 200 and then only reports an
// error (resource-exhausted while overloaded) is then retried on the next key,
// as nothing has reached the client yet. The pool classifies such an error
// like the HTTP status it matches (see streamErrorStatus): a capacity error
// counts as a 429 "at capacity", which by design does not cool the key down
// (the whole model is short of capacity, not the key); the key's own rate
// limit does. Once anything
// was sent, the error event is passed through and the request is logged as
// failed (see streamErrorLogStatus). So is an error that arrives with usage
// carrying a cost before any content: the upstream billed that attempt, so
// it is never failed over (a billed attempt is not retried).
//
// A token-billed stream tells the pool its outcome itself: success once
// content arrives or the stream ends cleanly, else only the classified
// in-stream error. Reporting the 200 headers as a success first would reset
// the key's failure count, breaker and cooldown before every in-stream
// error, and the breaker would never open for a key whose streams keep
// failing that way.
func (st *state) streamBody(ctx, clientCtx context.Context, cancel context.CancelCauseFunc, w http.ResponseWriter,
	resp *http.Response, keyID int64, canRetry bool) streamResult {
	s := st.s
	rc := http.NewResponseController(w)

	maxLog := s.LogResponseBodyMaxBytes()
	capt := newCapture(maxLog)
	ss := &billing.StreamState{MaxText: maxLog}
	parser := &sseParser{onEvent: ss.AddEvent}
	tokenStream := estimatedPaths[st.log.Path]

	idle := s.StreamIdleTimeout()
	watchdog := time.AfterFunc(idle, func() { cancel(errStreamIdle) })
	defer watchdog.Stop()

	// After a disconnect the stream is read for at most clientGoneDrain.
	var drainMu sync.Mutex
	var drain *time.Timer
	done := false
	stopWatch := context.AfterFunc(clientCtx, func() {
		drainMu.Lock()
		if !done {
			drain = time.AfterFunc(clientGoneDrain, func() { cancel(errClientGone) })
		}
		drainMu.Unlock()
	})
	defer func() {
		stopWatch()
		drainMu.Lock()
		done = true
		if drain != nil {
			drain.Stop()
		}
		drainMu.Unlock()
	}()

	// Writes to the client. Until the stream is committed, bytes are held
	// and the status line is not sent; the hold timer commits from its own
	// goroutine, so everything that touches w goes through wmu.
	var wmu sync.Mutex
	committed, finished, gone := false, false, false
	var held []byte
	commitLocked := func() {
		if committed || finished {
			return
		}
		committed = true
		copyResponseHeaders(w.Header(), resp.Header)
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		st.markTTFB()
		if len(held) > 0 {
			if _, err := w.Write(held); err != nil {
				gone = true
			}
			held = nil
		}
		if !gone {
			_ = rc.Flush()
		}
	}
	commit := func() {
		wmu.Lock()
		commitLocked()
		wmu.Unlock()
	}
	// Registered first, run last: no write to w after streamBody returns.
	defer func() {
		wmu.Lock()
		finished = true
		wmu.Unlock()
	}()
	if tokenStream {
		hold := time.AfterFunc(firstByteHold, commit)
		defer hold.Stop()
	} else {
		commit()
	}
	toClient := func(b []byte) {
		if len(b) == 0 {
			return
		}
		wmu.Lock()
		defer wmu.Unlock()
		if gone || finished {
			return
		}
		if !committed {
			held = append(held, b...)
			if len(held) > maxHeldBytes {
				commitLocked()
			}
			return
		}
		st.markTTFB()
		if _, werr := w.Write(b); werr != nil {
			gone = true
		} else {
			_ = rc.Flush()
		}
	}

	var fixer *messagesFixer
	if s.AnthropicStreamFix && st.log.Path == "/v1/messages" {
		fixer = &messagesFixer{}
	}
	var strip *usageStripper
	if st.stripUsage {
		strip = &usageStripper{}
	}
	var res streamResult
	reported, succeeded := false, false
	// reportSuccess tells the pool the attempt succeeded, once, unless its
	// in-stream error was reported (token-billed streams only; the others
	// were reported with their headers).
	reportSuccess := func() {
		if !tokenStream || reported || succeeded {
			return
		}
		succeeded = true
		st.h.Pool.Report(keyID, pool.Outcome{Status: resp.StatusCode})
	}
	// report tells the pool about the in-stream error as if the upstream
	// had answered it with the matching HTTP status (cooldown, breaker).
	report := func() pool.Verdict {
		reported = true
		body, _ := json.Marshal(map[string]any{"error": ss.Err})
		return st.h.Pool.Report(keyID, pool.Outcome{Status: streamErrorStatus(ss.Err), Body: string(body),
			RequestID: st.log.RequestID, Echo: st.echo()})
	}
	// decide runs after each upstream chunk while nothing was sent: it
	// commits once content (or a final error) arrives, and abandons the
	// attempt for the next key when the stream opened with a retryable error.
	decide := func() (retry bool) {
		wmu.Lock()
		open := !committed && !finished
		wmu.Unlock()
		if !open {
			return false
		}
		switch {
		case ss.Err != nil && !ss.Output:
			v := report()
			res.action = v.Action
			// Usage with a cost came with the error: the upstream billed
			// the attempt, which is then final, like an error after content.
			billed := ss.Usage.HasCost && ss.Usage.CostTicks > 0
			if v.Retry && canRetry && !billed {
				wmu.Lock()
				if !committed {
					finished = true // the hold timer must not commit any more
					wmu.Unlock()
					body, _ := json.Marshal(map[string]any{"error": ss.Err})
					res.retry = &upstreamError{status: streamErrorStatus(ss.Err), body: body,
						header: http.Header{"Content-Type": {"application/json"}},
						err:    "upstream error in stream before any byte was sent: " + v.Reason}
					return true
				}
				wmu.Unlock()
			}
			commit()
		case ss.Output || ss.Err != nil:
			commit()
		}
		return false
	}
	forward := func(chunk []byte) {
		if len(chunk) == 0 {
			return
		}
		if clientCtx.Err() != nil {
			wmu.Lock()
			gone = true
			wmu.Unlock()
		}
		if strip != nil {
			toClient(strip.Feed(chunk))
		} else {
			toClient(chunk)
		}
		parser.Write(chunk) // the whole upstream stream, usage included
		if s.LogBodies {
			capt.Write(chunk)
		}
	}

	buf := make([]byte, 32<<10)
	var errMsg string
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			watchdog.Reset(idle)
			chunk := buf[:n]
			if fixer != nil {
				chunk = fixer.Feed(chunk)
			}
			forward(chunk)
			if tokenStream && decide() {
				return res // retried on the next key; nothing reached the client
			}
			if ss.Output {
				reportSuccess() // content arrived: an error after it is reported too
			}
		}
		if err != nil {
			if fixer != nil {
				forward(fixer.Flush())
			}
			if strip != nil {
				toClient(strip.Flush())
			}
			parser.Close()
			if tokenStream && decide() {
				return res
			}
			commit()
			if !errors.Is(err, io.EOF) {
				switch {
				case errors.Is(context.Cause(ctx), errStreamIdle):
					errMsg = fmt.Sprintf("stream idle for %s; aborted", idle)
				case errors.Is(context.Cause(ctx), errClientGone):
					errMsg = fmt.Sprintf("client went away; upstream abandoned after %s", clientGoneDrain)
				default:
					errMsg = "upstream stream error: " + err.Error()
				}
			}
			break
		}
	}
	wmu.Lock()
	wasGone := gone
	wmu.Unlock()
	// Only a disconnect seen while data was still arriving counts: clients
	// often close right after the last event, before the handler returns.
	if wasGone || errors.Is(context.Cause(ctx), errClientGone) {
		st.log.Status = 499
		if errMsg == "" {
			errMsg = "client went away"
		}
	}
	streamFailed := tokenStream && ss.Err != nil
	switch {
	case streamFailed && !reported:
		res.action = report().Action
	case !streamFailed && (errMsg == "" || ss.Output):
		// Ended cleanly (or after content): the attempt succeeded. A stream
		// cut short before any content reports nothing either way.
		reportSuccess()
	}
	if streamFailed {
		// The client saw 200 and then the error event; the log records the
		// request as failed so it counts in failure stats and filters.
		msg := fmt.Sprintf("upstream error in stream (HTTP %d sent): %s", resp.StatusCode, ss.Err)
		if errMsg != "" {
			msg += "; " + errMsg
		}
		errMsg = msg
		if st.log.Status != 499 {
			st.log.Status = streamErrorLogStatus
		}
		if ss.Final || ss.Output {
			// Content (or usage) arrived before the error: billed below.
			// The error and the status the client saw stay in extra.
			extra, _ := json.Marshal(map[string]any{"stream_error": map[string]any{
				"code": ss.Err.Code, "type": ss.Err.Type, "message": ss.Err.Message,
				"client_status": resp.StatusCode, "after_content": true}})
			st.log.Extra = string(extra)
		}
	}
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
	switch {
	case streamFailed && !ss.Final && !ss.Output:
		// The upstream failed before generating anything and reported no
		// usage: xAI does not bill such a request. Bill 0 and keep the
		// estimate the gateway would otherwise have charged, for audit.
		st.applyEstimatedUsage(ss)
		est, estPrompt := st.log.CostTicks, st.log.PromptTokens
		st.applyUsage(billing.Usage{})
		st.log.CostSource = "upstream_error"
		extra, _ := json.Marshal(map[string]any{"stream_error": map[string]any{
			"code": ss.Err.Code, "type": ss.Err.Type, "message": ss.Err.Message,
			"client_status": resp.StatusCode, "estimated_cost_ticks": est, "estimated_prompt_tokens": estPrompt}})
		st.log.Extra = string(extra)
	case !ss.Final && (errMsg != "" || tokenStream):
		// Cut short, or ended without the closing usage event (no chat
		// usage chunk, no response.completed, no message_delta): never bill
		// such a stream as free (or prompt-only); estimate what was received.
		if errMsg == "" {
			st.h.Log.Info("stream ended without usage; cost estimated", "path", st.log.Path, "request_id", st.log.RequestID)
		}
		st.applyEstimatedUsage(ss)
	default:
		st.applyUsage(ss.Usage)
	}
	if ss.ID != "" && st.log.Path == "/v1/responses" {
		st.bind(KindResponse, ss.ID)
	}
	res.errMsg = errMsg
	return res
}

// streamResult is how a stream attempt ended.
type streamResult struct {
	errMsg string         // for the log and the attempt
	action string         // pool action for an in-stream error ("" = success)
	retry  *upstreamError // set when the attempt was abandoned for the next key
}

// firstByteHold bounds how long a token-billed stream is held back waiting
// for its first content (see streamBody); maxHeldBytes bounds what is held.
var firstByteHold = 3 * time.Second

const maxHeldBytes = 256 << 10

// streamErrorLogStatus is the log status of a stream that answered 200 and
// then failed with an in-stream error: the upstream's response was an
// error, so the request counts as failed (level error, "xAI server error"
// in the console). The 200 the client saw stays on the attempt and in the
// error text.
const streamErrorLogStatus = http.StatusBadGateway

// streamErrorStatus maps an in-stream error to the HTTP status the upstream
// would have answered it with, so the pool classifies it the same way:
// resource-exhausted is a 429 (capacity or rate limit), overload a 503.
func streamErrorStatus(e *billing.StreamError) int {
	if e == nil {
		return http.StatusBadGateway
	}
	m := strings.ToLower(e.Code + " " + e.Type + " " + e.Message)
	has := func(subs ...string) bool {
		for _, sub := range subs {
			if strings.Contains(m, sub) {
				return true
			}
		}
		return false
	}
	switch {
	case has("resource-exhausted", "resource_exhausted", "rate_limit", "rate limit", "too many requests", "at capacity", "high demand"):
		return http.StatusTooManyRequests
	case has("overloaded", "unavailable"):
		return http.StatusServiceUnavailable
	case has("invalid_request", "invalid-argument", "invalid_argument", "bad_request"):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

var errStreamIdle = errors.New("stream idle timeout")

// estimatedPaths are the token-billed streaming endpoints whose streams are
// estimated when they end cleanly without usage.
var estimatedPaths = map[string]bool{"/v1/chat/completions": true, "/v1/responses": true, "/v1/messages": true}

// handleBody extracts usage, ids and bindings from a non-streaming body.
func (st *state) handleBody(r *http.Request, body []byte) {
	u, env := billing.ParseBody(body)
	if env.Model != "" && st.log.Model == "" {
		st.log.Model = env.Model
	}
	st.log.UpstreamID = firstNonEmpty(env.ID, env.RequestID)
	if st.s.LogBodies {
		st.log.RespBody, st.log.Truncated = reqlog.SanitizeBody(body, st.s.LogResponseBodyMaxBytes())
		if u.Found && len(body) <= 8<<20 {
			t, rs := billing.ExtractText(body)
			limit := st.s.LogResponseBodyMaxBytes()
			st.log.RespText, st.log.ReasoningText = truncateStr(t, limit), truncateStr(rs, limit)
		}
	}
	kind, id, poll := isAsyncPoll(r.Method, r.URL.Path)
	switch {
	case r.Method == http.MethodGet && !poll:
		// Retrievals (GET /v1/responses/{id}, files, batches…) echo the usage
		// of the original request; they are free.
		u = billing.Usage{}
	case poll:
		// The poll that returns a finished job carries the cost of the whole
		// job. Pending polls are free, and a job is charged once, to the
		// first poll that sees it finished.
		done := st.log.Status == http.StatusOK && env.Status != "pending" &&
			(u.HasCost || u.VideoSeconds > 0 || u.Images > 0 || u.PromptTokens > 0)
		if !done {
			u = billing.Usage{}
			if st.log.Status == http.StatusOK && jobFailed(env.Status) {
				// A failed, expired or cancelled job costs nothing: free its
				// hold now instead of keeping it until the TTL.
				st.releaseJob = id
			}
			break
		}
		if b, err := st.h.DB.GetBinding(r.Context(), id); err == nil {
			if st.log.Model == "" {
				st.log.Model = b.Model
			}
			var pr billing.Request
			if b.PriceMeta != "" && json.Unmarshal([]byte(b.PriceMeta), &pr) == nil {
				st.priceReq = &pr // priced by the submission's resolution and inputs
			}
			first, err := st.h.DB.MarkBindingBilled(r.Context(), id)
			switch {
			case err != nil:
				// Whether the job is now marked billed is unknown: do not
				// charge it here (a later poll retries) and keep its hold.
				st.h.Log.Error("mark async job billed", "kind", kind, "id", id, "err", err)
				u = billing.Usage{}
				st.log.Error = "finished " + kind + " job not billed: " + err.Error()
			case !first:
				st.h.Log.Debug("async job already billed", "kind", kind, "id", id)
				u = billing.Usage{}
			default:
				// The hold is freed together with settling the job's cost
				// (see finish), so the quota is never briefly oversubscribed.
				st.releaseJob = id
			}
		} else {
			// Without the binding (expired, or the job was not submitted
			// through this gateway) "bill once" cannot be guaranteed, so the
			// job is not charged here.
			st.h.Log.Info("finished async job without binding not billed", "kind", kind, "id", id)
			u = billing.Usage{}
			st.log.Error = "finished " + kind + " job has no binding (expired or not created here); not billed again"
		}
	}
	st.applyUsage(u)
	st.applyVoiceREST(r, body)
	if kind := createdResourceKind(r.Method, r.URL.Path, st.info.Deferred); kind != "" {
		id := resourceID(body) // id, batch_id, collection_id, file_id or request_id
		if kind == KindVideo || kind == KindImage || kind == KindDeferred {
			id = firstNonEmpty(env.RequestID, env.ID)
		}
		st.bind(kind, id)
	}
}

// applyVoiceREST prices the REST voice endpoints, which report no cost:
// text to speech per input character, speech to text per audio hour.
func (st *state) applyVoiceREST(r *http.Request, body []byte) {
	if r.Method != http.MethodPost || st.log.Status != http.StatusOK || st.log.CostSource == "upstream" {
		return
	}
	switch r.URL.Path {
	case "/v1/tts":
		st.log.CostTicks = st.info.TextChars * billing.TTSTicksPerChar
	case "/v1/stt":
		var v struct {
			Duration float64 `json:"duration"`
		}
		if json.Unmarshal(body, &v) != nil || v.Duration <= 0 {
			return
		}
		st.log.CostTicks = int64(v.Duration * billing.STTRestTicksPerHour / 3600)
	default:
		return
	}
	st.log.CostSource = "computed"
	if st.log.CostTicks == 0 {
		st.log.CostSource = "none"
	}
}

func (st *state) applyUsage(u billing.Usage) {
	u.Finalize()
	l := st.log
	l.PromptTokens, l.CompletionTokens, l.CachedTokens = u.PromptTokens, u.CompletionTokens, u.CachedTokens
	l.ReasoningTokens, l.ImageTokens, l.TotalTokens = u.ReasoningTokens, u.ImageTokens, u.TotalTokens
	l.Images, l.VideoSeconds = u.Images, u.VideoSeconds
	l.CostTicks, l.CostSource = st.h.Prices.Cost(l.Model, u, st.pricingRequest())
	l.CacheWriteTokens = u.CacheWriteTokens
	l.PriceSnapshot = st.h.Prices.PriceSnapshot(l.Model, u, st.pricingRequest())
}

// pricingRequest returns the parameters media prices depend on: those of an
// async job's submission when billing its finished poll, else this request's.
func (st *state) pricingRequest() billing.Request {
	if st.priceReq != nil {
		return *st.priceReq
	}
	return billing.Request{Quality: st.info.Quality, Resolution: st.info.Resolution,
		Edit: st.log.Path == "/v1/images/edits", VideoEdit: st.log.Path == "/v1/videos/edits",
		InputImages: st.info.InputImages}
}

// applyEstimatedUsage bills a response that ended without its final usage:
// what the upstream already reported (e.g. the prompt in message_start, the
// service tier, tool calls), completed with estimates at roughly four bytes
// per token: the prompt from the request without embedded media, the output
// from the text received. The cost source is "estimated".
func (st *state) applyEstimatedUsage(ss *billing.StreamState) {
	u := ss.Usage
	u.Found = true
	if u.PromptTokens == 0 {
		body, _ := reqlog.SanitizeBody(st.reqBody, len(st.reqBody)+1) // base64 media stripped
		u.PromptTokens = max(int64(len(body))/4, 1)
	}
	u.CompletionTokens = max(u.CompletionTokens, (ss.TextBytes+3)/4)
	u.ReasoningTokens = max(u.ReasoningTokens, (ss.ReasoningBytes+3)/4)
	u.ServiceTier = firstNonEmpty(u.ServiceTier, st.info.ServiceTier)
	u.TotalTokens = 0
	st.applyUsage(u)
	if st.log.CostTicks > 0 {
		st.log.CostSource = "estimated"
	}
}

func (st *state) bind(kind, id string) {
	if id == "" || st.log.KeyID == 0 {
		return
	}
	if kind == KindVideo || kind == KindImage || kind == KindDeferred {
		st.jobID = id
	}
	ttl := time.Duration(st.s.TaskBindingTTLHours) * time.Hour
	b := &store.Binding{ResourceID: id, Kind: kind, KeyID: st.log.KeyID, TokenID: st.log.TokenID,
		Model: st.log.Model, ExpiresAt: time.Now().Add(ttl).UnixMilli()}
	switch kind {
	case KindVideo, KindImage:
		meta, _ := json.Marshal(st.pricingRequest())
		b.PriceMeta = string(meta)
	case KindResponse:
		// The stored conversation a later previous_response_id pulls in:
		// this response's prompt (which includes its own context) and output.
		meta, _ := json.Marshal(map[string]int64{"context_tokens": st.log.PromptTokens + st.log.CompletionTokens + st.log.ReasoningTokens})
		b.PriceMeta = string(meta)
	}
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
		st.log.RespBody, _ = reqlog.SanitizeBody(body, st.s.LogResponseBodyMaxBytes())
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
	l := st.log
	l.LatencyMs = time.Since(st.start).Milliseconds()
	l.EndAt = time.Now().UnixMilli()
	l.Attempts = st.attempts
	if l.CostSource == "" {
		l.CostSource = "none"
	}
	// A job the upstream accepted is held even if the client left before
	// reading its id: the finishing poll bills it against this hold.
	if st.jobID != "" && (l.Success() || l.Status == 499) {
		ttl := time.Duration(s.TaskBindingTTLHours) * time.Hour
		st.res.Hold(st.jobID, l.CostTicks, ttl)
	} else {
		// Retried attempts normally cost nothing; one the upstream reported
		// a cost for is billed on its own row, within the same settlement.
		var retried int64
		for i := 0; i+1 < len(st.attempts); i++ {
			retried += st.attempts[i].CostTicks
		}
		st.res.SettleReleasing(st.releaseJob, l.CostTicks+retried-st.preCharged)
	}
	if l.Status == 0 {
		l.Status = 499
		if l.Error == "" {
			l.Error = "client closed request"
		}
	}
	if !s.LogEnabled {
		// Usage must still be accounted (quotas, key spend, stats).
		l.NoRecord = true
	} else if s.LogBodies {
		l.HasBodies = true
		l.ReqHeaders = reqlog.HeadersJSON(r.Header)
		var trunc bool
		l.ReqBody, trunc = reqlog.SanitizeBody(st.reqBody, s.LogRequestBodyMaxBytes())
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
	if raw == "" && isWebSocket(r) {
		raw, _, _ = wsProtocols(r.Header)
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
	if tok.Limited && tok.UsedTicks >= tok.QuotaTicks {
		// The token is returned too: polls of its own jobs stay allowed.
		return tok, errf(http.StatusTooManyRequests, "insufficient_quota",
			"this API key has used its quota of $%.4f", billing.TicksToUSD(tok.QuotaTicks))
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
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 {
		return ri
	}
	if mt, params, err := mime.ParseMediaType(contentType); err == nil && strings.HasPrefix(mt, "multipart/") {
		return parseMultipart(params["boundary"], body)
	}
	if trimmed[0] != '{' {
		return ri
	}
	body = trimmed
	// Go's JSON decoding matches field names case-insensitively and keeps
	// the last match, while xAI reads the exact lowercase name. Any second
	// spelling (duplicate, "MODEL", "Model"...) would let the gateway and
	// the upstream disagree about the model, so such bodies are refused.
	for _, k := range readFields {
		if keyAmbiguous(body, k) {
			ri.Ambiguous = true
			break
		}
	}
	// Decode field by field: one wrongly typed field must not make the
	// gateway lose the others (the upstream may still accept the body).
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		ri.Invalid = "the request body is not valid JSON"
		return ri
	}
	str := func(k string) string {
		raw, ok := m[k]
		if !ok || string(raw) == "null" {
			return ""
		}
		var v string
		if json.Unmarshal(raw, &v) != nil {
			ri.Invalid = fmt.Sprintf("%q must be a string", k)
		}
		return v
	}
	boolean := func(k string) bool {
		raw, ok := m[k]
		if !ok || string(raw) == "null" {
			return false
		}
		var v bool
		if json.Unmarshal(raw, &v) != nil {
			ri.Invalid = fmt.Sprintf("%q must be a boolean", k)
		}
		return v
	}
	number := func(k string) int64 {
		raw, ok := m[k]
		if !ok || string(raw) == "null" {
			return 0
		}
		n := flexInt(raw) // JSON number in any form, or a numeric string
		if n == 0 && strings.Trim(string(raw), `"0. `) != "" {
			ri.Invalid = fmt.Sprintf("%q must be a number", k)
		}
		return n
	}
	ri.Model = str("model")
	ri.PreviousResponseID = str("previous_response_id")
	ri.Quality = str("quality")
	ri.Resolution = str("resolution")
	ri.ServiceTier = str("service_tier")
	ri.TextChars = int64(utf8.RuneCountInString(str("text")))
	ri.Stream = boolean("stream")
	ri.Deferred = boolean("deferred")
	ri.N = number("n")
	ri.MaxTokens = max(number("max_tokens"), number("max_completion_tokens"), number("max_output_tokens"))
	ri.Duration = number("duration")
	if ri.Duration == 0 {
		ri.Duration = number("seconds")
	}
	ri.InputImages = countInputImages(m)
	return ri
}

// parseMultipart reads the "model" form field of a multipart body (image
// edits, speech to text) without buffering file parts.
func parseMultipart(boundary string, body []byte) reqInfo {
	var ri reqInfo
	if boundary == "" {
		ri.Ambiguous = true
		return ri
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	models := 0
	seen := map[string]int{}
	for {
		p, err := mr.NextPart()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				ri.Ambiguous = true
			}
			break
		}
		name := p.FormName()
		for _, k := range readFields {
			if strings.EqualFold(name, k) {
				seen[strings.ToLower(name)]++
				if name != k {
					ri.Ambiguous = true
				}
			}
		}
		if p.FileName() != "" && strings.HasPrefix(strings.ToLower(name), "image") {
			ri.InputImages++
		}
		if p.FileName() == "" {
			switch name {
			case "model":
				v, _ := io.ReadAll(io.LimitReader(p, 256))
				ri.Model = strings.TrimSpace(string(v))
				models++
			case "deferred":
				v, _ := io.ReadAll(io.LimitReader(p, 16))
				ri.Deferred, _ = strconv.ParseBool(strings.TrimSpace(string(v)))
			case "n", "quality", "resolution":
				v, _ := io.ReadAll(io.LimitReader(p, 32))
				val := strings.TrimSpace(string(v))
				switch name {
				case "n":
					ri.N, _ = strconv.ParseInt(val, 10, 64)
				case "quality":
					ri.Quality = val
				case "resolution":
					ri.Resolution = val
				}
			}
		}
		p.Close()
	}
	for _, n := range seen {
		if n > 1 {
			ri.Ambiguous = true
		}
	}
	if models > 1 {
		ri.Ambiguous = true
	}
	return ri
}

// countTopLevelKey counts occurrences of key among the top-level members of
// a JSON object (duplicates are resolved differently by different parsers).
func countTopLevelKey(body []byte, key string) int {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return 0
	}
	n := 0
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return n
		}
		if k, ok := t.(string); ok && k == key {
			n++
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return n
		}
	}
	return n
}

// readFields are the request fields the gateway acts on (policy, billing,
// bindings, quota reservations). Each must appear at most once and only in
// its exact lowercase spelling, otherwise gateway and upstream could read
// different values.
var readFields = []string{"model", "previous_response_id", "deferred", "stream", "stream_options",
	"max_tokens", "max_completion_tokens", "max_output_tokens", "n", "duration", "seconds",
	"service_tier", "quality", "resolution", "text"}

// keyAmbiguous reports whether a top-level member name matches key
// case-insensitively more than once, or only with a different case.
func keyAmbiguous(body []byte, key string) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return false
	}
	exact, fold := 0, 0
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return false // invalid JSON: the upstream rejects it anyway
		}
		if k, ok := t.(string); ok && strings.EqualFold(k, key) {
			fold++
			if k == key {
				exact++
			}
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return false
		}
	}
	return fold > 1 || (fold == 1 && exact == 0)
}

// flexInt reads a JSON number or numeric string ("8").
func flexInt(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return int64(math.Ceil(f))
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
			return int64(math.Ceil(f))
		}
	}
	return 0
}

// What a client asked for in stream_options.include_usage.
const (
	usageUnset = iota
	usageFalse
	usageTrue
)

// injectStreamUsage sets stream_options.include_usage=true, overriding a
// client's false, and reports what the client had chosen. A body that is not
// a JSON object, or whose stream_options is not an object, is returned
// unchanged.
func injectStreamUsage(body []byte) ([]byte, int) {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return body, usageUnset
	}
	opts := map[string]json.RawMessage{}
	choice := usageUnset
	if raw, ok := m["stream_options"]; ok && string(raw) != "null" {
		if json.Unmarshal(raw, &opts) != nil {
			return body, usageUnset
		}
		if v, set := opts["include_usage"]; set {
			if string(bytes.TrimSpace(v)) == "true" {
				return body, usageTrue
			}
			choice = usageFalse
		}
	}
	opts["include_usage"] = json.RawMessage("true")
	ob, _ := json.Marshal(opts)
	m["stream_options"] = ob
	out, err := json.Marshal(m)
	if err != nil {
		return body, usageUnset
	}
	return out, choice
}

// jobFailed reports whether an async job's status says it ended without a
// result, and so without cost.
func jobFailed(status string) bool {
	switch strings.ToLower(status) {
	case "failed", "failure", "error", "expired", "cancelled", "canceled", "rejected":
		return true
	}
	return false
}

// ownedPoll returns the binding of the async job a GET poll refers to when
// tok created that job, else nil.
func (h *Handler) ownedPoll(r *http.Request, tok *store.Token) *store.Binding {
	_, id, ok := isAsyncPoll(r.Method, r.URL.Path)
	if !ok {
		return nil
	}
	b, err := h.DB.GetBinding(r.Context(), id)
	if err != nil || b == nil || b.TokenID != tok.ID {
		return nil
	}
	return b
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

// countInputImages counts the media inputs of an image edit or video
// request that xAI bills per image: image / images, reference_images,
// last_frame and keyframes.
func countInputImages(m map[string]json.RawMessage) int64 {
	count := func(k string) int64 {
		raw := bytes.TrimSpace(m[k])
		if len(raw) == 0 || string(raw) == "null" || string(raw) == `""` {
			return 0
		}
		if raw[0] == '[' {
			var a []json.RawMessage
			if json.Unmarshal(raw, &a) != nil {
				return 0
			}
			return int64(len(a))
		}
		return 1
	}
	return count("image") + count("images") + count("reference_images") + count("last_frame") + count("keyframes")
}
