// Package pool selects upstream keys and tracks their health: priority groups,
// weighted selection, cooldown after rate limiting, a circuit breaker for
// repeated upstream failures, automatic disabling of dead keys and optional
// per-key concurrency and requests-per-minute limits.
package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/store"
)

// Actions recorded on each upstream attempt.
const (
	ActionSuccess  = "success"  // response forwarded to the client
	ActionReturn   = "return"   // client error forwarded as-is (not the key's fault)
	ActionRetry    = "retry"    // try the next key
	ActionCooldown = "cooldown" // key paused (429 or breaker opened); next key
	ActionDisable  = "disable"  // key auto-disabled; next key
	ActionVerify   = "verify"   // looks like a dead key: verifying with GET /v1/api-key; next key
)

type entry struct {
	key           *store.Key
	inflight      int
	consecFails   int
	breakerUntil  time.Time
	cooldownUntil time.Time
	recent        []time.Time // request start times within the last minute (RPM limit)
}

func (e *entry) disabled() bool { return !e.key.Enabled || e.key.AutoDisabled }

// Verifier checks a key out of band (GET /v1/api-key). It returns whether
// the key is unusable and why.
type Verifier func(ctx context.Context, k *store.Key) (dead bool, reason string)

// Pool is safe for concurrent use.
type Pool struct {
	// Verify confirms suspected dead keys before they are disabled. When nil,
	// suspected keys are only skipped for the current request.
	Verify    Verifier
	verifying map[int64]time.Time

	db       *store.DB
	settings *config.Holder
	log      *slog.Logger

	mu      sync.Mutex
	entries map[int64]*entry
	rr      uint64
	now     func() time.Time
}

// New creates a pool; call Reload to load keys.
func New(db *store.DB, settings *config.Holder, log *slog.Logger) *Pool {
	return &Pool{db: db, settings: settings, log: log, entries: map[int64]*entry{}, now: time.Now}
}

// Reload re-reads keys from the database, keeping runtime state.
func (p *Pool) Reload(ctx context.Context) error {
	keys, err := p.db.ListAllKeys(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next := make(map[int64]*entry, len(keys))
	for _, k := range keys {
		e := p.entries[k.ID]
		if e == nil {
			e = &entry{}
		}
		e.key = k
		e.cooldownUntil = time.UnixMilli(k.CooldownUntil)
		if k.CooldownUntil == 0 {
			e.cooldownUntil = time.Time{}
		}
		if !k.AutoDisabled && k.Enabled && k.CooldownUntil == 0 && e.consecFails > 0 && e.breakerUntil.IsZero() {
			e.consecFails = 0
		}
		next[k.ID] = e
	}
	p.entries = next
	return nil
}

// Lease is a reserved slot on a key. Release must be called exactly once.
type Lease struct {
	Key *store.Key
	p   *Pool
	id  int64
}

// Candidates returns the keys to try, in order. When boundKeyID is non-zero
// only that key is returned (async jobs and stored responses must be read
// with the key that created them).
func (p *Pool) Candidates(boundKeyID int64) []*store.Key {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if boundKeyID != 0 {
		if e := p.entries[boundKeyID]; e != nil {
			k := *e.key
			return []*store.Key{&k}
		}
		return nil
	}
	s := p.settings.Get()
	byPrio := map[int][]*entry{}
	var prios []int
	var resting []*entry // in cooldown or breaker: used only as a last resort
	for _, e := range p.entries {
		if e.disabled() {
			continue
		}
		if now.Before(e.cooldownUntil) || now.Before(e.breakerUntil) {
			resting = append(resting, e)
			continue
		}
		if _, ok := byPrio[e.key.Priority]; !ok {
			prios = append(prios, e.key.Priority)
		}
		byPrio[e.key.Priority] = append(byPrio[e.key.Priority], e)
	}
	slices.Sort(prios)
	var out []*store.Key
	for _, pr := range prios {
		group := byPrio[pr]
		slices.SortFunc(group, func(a, b *entry) int { return int(a.key.ID - b.key.ID) })
		switch s.SelectionStrategy {
		case "random":
			rand.Shuffle(len(group), func(i, j int) { group[i], group[j] = group[j], group[i] })
		case "round_robin":
			p.rr++
			off := int(p.rr % uint64(len(group)))
			group = append(group[off:], group[:off]...)
		default:
			weightedShuffle(group)
		}
		for _, e := range group {
			k := *e.key // callers get a snapshot; the pool mutates its own copy
			out = append(out, &k)
		}
	}
	if len(out) == 0 && len(resting) > 0 {
		slices.SortFunc(resting, func(a, b *entry) int {
			return later(a.cooldownUntil, a.breakerUntil).Compare(later(b.cooldownUntil, b.breakerUntil))
		})
		for _, e := range resting {
			k := *e.key
			out = append(out, &k)
		}
	}
	return out
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// weightedShuffle orders entries by weighted random sampling without
// replacement (Efraimidis–Spirakis): key = u^(1/w), sorted descending.
func weightedShuffle(es []*entry) {
	if len(es) < 2 {
		return
	}
	scores := make(map[*entry]float64, len(es))
	for _, e := range es {
		w := float64(max(e.key.Weight, 1))
		scores[e] = math.Pow(rand.Float64(), 1/w)
	}
	slices.SortFunc(es, func(a, b *entry) int {
		switch {
		case scores[a] > scores[b]:
			return -1
		case scores[a] < scores[b]:
			return 1
		}
		return 0
	})
}

// Acquire reserves a slot on key id, honouring max_concurrency and rpm_limit.
// It returns nil (and a reason) when the key is saturated.
func (p *Pool) Acquire(id int64) (*Lease, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[id]
	if e == nil {
		return nil, "key no longer exists"
	}
	if e.key.MaxConcurrency > 0 && e.inflight >= e.key.MaxConcurrency {
		return nil, fmt.Sprintf("concurrency limit %d reached", e.key.MaxConcurrency)
	}
	now := p.now()
	if e.key.RPMLimit > 0 {
		cut := now.Add(-time.Minute)
		i := 0
		for i < len(e.recent) && e.recent[i].Before(cut) {
			i++
		}
		e.recent = e.recent[i:]
		if len(e.recent) >= e.key.RPMLimit {
			return nil, fmt.Sprintf("rpm limit %d reached", e.key.RPMLimit)
		}
		e.recent = append(e.recent, now)
	}
	e.inflight++
	return &Lease{Key: e.key, p: p, id: id}, ""
}

// Release frees the slot.
func (l *Lease) Release() {
	l.p.mu.Lock()
	if e := l.p.entries[l.id]; e != nil && e.inflight > 0 {
		e.inflight--
	}
	l.p.mu.Unlock()
}

// Outcome describes the result of an upstream attempt.
type Outcome struct {
	Status  int    // 0 = network error
	Body    string // error body (may be truncated); empty on success
	Header  http.Header
	NetErr  error
	Timeout bool
	Echo    []string // client-supplied values the upstream may echo (model name…)
}

// Verdict is what the proxy should do after an attempt.
type Verdict struct {
	Action  string
	Reason  string
	Retry   bool // try the next key
	Penalty bool // counts towards the circuit breaker
}

// Report classifies an attempt and updates the key's health.
func (p *Pool) Report(id int64, o Outcome) Verdict {
	s := p.settings.Get()
	v := classify(s, o)
	p.mu.Lock()
	e := p.entries[id]
	if e == nil {
		p.mu.Unlock()
		return v
	}
	now := p.now()
	var persist func()
	switch v.Action {
	case ActionSuccess, ActionReturn:
		e.consecFails = 0
		e.breakerUntil = time.Time{}
		if !e.cooldownUntil.IsZero() {
			e.cooldownUntil = time.Time{}
			persist = p.persistState(e)
		}
	case ActionVerify:
		if p.Verify != nil {
			if p.verifying == nil {
				p.verifying = map[int64]time.Time{}
			}
			if last, busy := p.verifying[id]; !busy || now.Sub(last) > time.Minute {
				p.verifying[id] = now
				k := *e.key
				go p.verify(&k)
			}
		}
	case ActionDisable:
		e.key.AutoDisabled = true
		e.key.StatusReason = v.Reason
		persist = p.persistState(e)
		p.log.Warn("key auto-disabled", "key_id", id, "key", e.key.Name, "reason", v.Reason)
	case ActionCooldown:
		d := time.Duration(s.CooldownSeconds) * time.Second
		if ra := retryAfter(o.Header); ra > 0 {
			d = min(ra, 24*time.Hour)
		}
		e.cooldownUntil = now.Add(d)
		e.key.StatusReason = v.Reason
		persist = p.persistState(e)
		p.log.Info("key cooling down", "key_id", id, "key", e.key.Name, "for", d.String(), "reason", v.Reason)
	case ActionRetry:
		if !v.Penalty {
			break
		}
		e.consecFails++
		if e.consecFails >= s.BreakerThreshold {
			e.breakerUntil = now.Add(time.Duration(s.BreakerSeconds) * time.Second)
			v.Action = ActionCooldown
			v.Reason = fmt.Sprintf("circuit open after %d consecutive failures: %s", e.consecFails, v.Reason)
			// Persist as a cooldown so filters and restarts see it too.
			if e.breakerUntil.After(e.cooldownUntil) {
				e.cooldownUntil = e.breakerUntil
			}
			e.key.StatusReason = v.Reason
			persist = p.persistState(e)
			p.log.Info("key circuit open", "key_id", id, "key", e.key.Name, "reason", v.Reason)
		}
	}
	p.mu.Unlock()
	if persist != nil {
		persist()
	}
	return v
}

func (p *Pool) verify(k *store.Key) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dead, reason := p.Verify(ctx, k)
	if !dead {
		p.log.Info("suspected key verified healthy", "key_id", k.ID, "key", k.Name)
		return
	}
	if err := p.Disable(ctx, k.ID, reason); err != nil {
		p.log.Error("disable key", "key_id", k.ID, "err", err)
		return
	}
	p.log.Warn("key auto-disabled", "key_id", k.ID, "key", k.Name, "reason", reason)
}

func (p *Pool) persistState(e *entry) func() {
	id, auto, reason := e.key.ID, e.key.AutoDisabled, e.key.StatusReason
	var cd int64
	if !e.cooldownUntil.IsZero() {
		cd = e.cooldownUntil.UnixMilli()
	}
	e.key.CooldownUntil = cd
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := p.db.SetKeyState(ctx, id, auto, reason, cd); err != nil {
			p.log.Error("persist key state", "key_id", id, "err", err)
		}
	}
}

func classify(s *config.Settings, o Outcome) Verdict {
	if o.Status == 0 {
		reason := "network error"
		if o.NetErr != nil {
			reason = "network error: " + o.NetErr.Error()
		}
		if o.Timeout {
			reason = "upstream timeout"
		}
		return Verdict{Action: ActionRetry, Reason: reason, Retry: true, Penalty: true}
	}
	if o.Status < 400 {
		return Verdict{Action: ActionSuccess}
	}
	// Upstream errors often echo client input (e.g. "the model <model> does
	// not exist"), so keywords are matched against the error message with
	// the client-supplied values removed, never against the raw body.
	msg := ErrorMessage(o.Body, o.Echo)
	snippet := truncateRunes(msg, 200)
	label := strconv.Itoa(o.Status)
	reason := fmt.Sprintf("HTTP %s: %s", label, snippet)
	switch o.Status {
	case http.StatusUnauthorized:
		return Verdict{Action: ActionDisable, Reason: reason, Retry: true}
	}
	if kw := config.MatchKeyword(msg, s.DisableKeywords); kw != "" {
		// e.g. 400 "Incorrect API key provided" or 403 "no credits". Error
		// text may echo client input, so a key is only disabled after an
		// out-of-band check with the key itself confirms it; only a 401
		// disables directly.
		return Verdict{Action: ActionVerify, Reason: reason, Retry: true}
	}
	if o.Status == http.StatusTooManyRequests {
		if isCapacityError(msg) {
			// The model is overloaded for everyone; the key itself is fine,
			// so try another key without cooling this one down.
			return Verdict{Action: ActionRetry, Reason: "model at capacity (429): " + snippet, Retry: true}
		}
		return Verdict{Action: ActionCooldown, Reason: "rate limited (429): " + snippet, Retry: true}
	}
	if s.IsRetryStatus(o.Status) {
		// Server errors count towards the circuit breaker; client-class
		// statuses (403 etc.) may be caused by the request itself, so they
		// only move on to the next key.
		return Verdict{Action: ActionRetry, Reason: reason, Retry: true, Penalty: o.Status >= 500 || o.Status == 408}
	}
	if o.Status < 500 {
		if kw := config.MatchKeyword(msg, s.RetryKeywords); kw != "" {
			return Verdict{Action: ActionRetry, Reason: fmt.Sprintf("HTTP %s matched %q: %s", label, kw, snippet), Retry: true}
		}
	}
	return Verdict{Action: ActionReturn, Reason: reason}
}

// isCapacityError recognises 429s caused by model-wide load (xAI:
// "resource-exhausted … The model is currently at capacity due to high
// demand") rather than by the key's own rate limit.
func isCapacityError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "at capacity") || strings.Contains(m, "high demand") || strings.Contains(m, "overloaded")
}

// ErrorMessage extracts the human-readable message of an error body
// ({"error":"…"}, {"error":{"message":"…"}}, {"message":"…"}, {"code":"…"}),
// falling back to the raw text, and removes the given client-supplied
// strings (case-insensitively).
func ErrorMessage(body string, echo []string) string {
	msg := strings.TrimSpace(body)
	var v struct {
		Error   any    `json:"error"`
		Message string `json:"message"`
		Code    any    `json:"code"`
	}
	if json.Unmarshal([]byte(msg), &v) == nil {
		var parts []string
		switch e := v.Error.(type) {
		case string:
			parts = append(parts, e)
		case map[string]any:
			for _, k := range []string{"message", "type", "code"} {
				if s, ok := e[k].(string); ok && s != "" {
					parts = append(parts, s)
				}
			}
		}
		if v.Message != "" {
			parts = append(parts, v.Message)
		}
		if c, ok := v.Code.(string); ok && c != "" {
			parts = append(parts, c)
		}
		if len(parts) > 0 {
			msg = strings.Join(parts, " | ")
		}
	}
	for _, e := range echo {
		if len(e) < 2 {
			continue
		}
		msg = replaceFold(msg, e, "<client input>")
	}
	return msg
}

func replaceFold(s, old, repl string) string {
	lo, lold := strings.ToLower(s), strings.ToLower(old)
	if len(lo) != len(s) { // non-ASCII case folding changed lengths; fall back
		return strings.ReplaceAll(s, old, repl)
	}
	var b strings.Builder
	for {
		i := strings.Index(lo, lold)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString(repl)
		s, lo = s[i+len(old):], lo[i+len(old):]
	}
}

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

func retryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// RuntimeState is the in-memory state of one key, for the admin API.
type RuntimeState struct {
	Inflight      int
	CooldownUntil time.Time // the later of rate-limit cooldown and breaker
	AutoDisabled  bool
	Reason        string
}

// State returns the runtime state of a key.
func (p *Pool) State(id int64) RuntimeState {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[id]
	if e == nil {
		return RuntimeState{}
	}
	rs := RuntimeState{Inflight: e.inflight, AutoDisabled: e.key.AutoDisabled, Reason: e.key.StatusReason}
	now := p.now()
	if cu := later(e.cooldownUntil, e.breakerUntil); cu.After(now) {
		rs.CooldownUntil = cu
	}
	return rs
}

// ResetKey clears cooldown, breaker and auto-disable state.
func (p *Pool) ResetKey(ctx context.Context, id int64) error {
	p.mu.Lock()
	if e := p.entries[id]; e != nil {
		e.consecFails, e.breakerUntil, e.cooldownUntil = 0, time.Time{}, time.Time{}
		e.key.AutoDisabled, e.key.StatusReason, e.key.CooldownUntil = false, "", 0
	}
	p.mu.Unlock()
	return p.db.SetKeyState(ctx, id, false, "", 0)
}

// Disable marks a key auto-disabled (used by health checks).
func (p *Pool) Disable(ctx context.Context, id int64, reason string) error {
	p.mu.Lock()
	if e := p.entries[id]; e != nil {
		e.key.AutoDisabled, e.key.StatusReason = true, reason
	}
	p.mu.Unlock()
	return p.db.SetKeyState(ctx, id, true, reason, 0)
}

// AnyKey returns an enabled key for internal calls (model sync), preferring
// healthy ones.
func (p *Pool) AnyKey() *store.Key {
	if c := p.Candidates(0); len(c) > 0 {
		return c[0]
	}
	return nil
}
