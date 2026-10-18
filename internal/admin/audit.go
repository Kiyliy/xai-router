package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/Kiyliy/xai-router/internal/keytest"
	"github.com/Kiyliy/xai-router/internal/netutil"
	"github.com/Kiyliy/xai-router/internal/store"
)

// Every successful write through the admin API is recorded as an audit
// event (see store.Event): who (password session or which admin API token),
// from where, what, on which object, and for updates the fields that
// changed with their old and new values (secrets masked). Handlers can name
// the target or add detail through opFrom; a handler that records its own
// event (atomically with the change, like quota adjustments, or when a
// background job finishes) marks the operation as recorded.

// auditActions maps a route to an action name and the type of its {id}.
var auditActions = map[string][2]string{
	"POST /api/v1/auth/logout":            {"auth.logout", ""},
	"PUT /api/v1/auth/password":           {"auth.password_change", ""},
	"POST /api/v1/keys":                   {"key.create", "key"},
	"POST /api/v1/keys/import":            {"key.import", "key"},
	"POST /api/v1/keys/test":              {"key.test_many", "key"},
	"PATCH /api/v1/keys/{id}":             {"key.update", "key"},
	"DELETE /api/v1/keys/{id}":            {"key.delete", "key"},
	"POST /api/v1/keys/{id}/test":         {"key.test", "key"},
	"POST /api/v1/keys/{id}/reset":        {"key.restore", "key"},
	"POST /api/v1/tokens":                 {"token.create", "token"},
	"PATCH /api/v1/tokens/{id}":           {"token.update", "token"},
	"DELETE /api/v1/tokens/{id}":          {"token.delete", "token"},
	"POST /api/v1/tokens/{id}/regenerate": {"token.regenerate", "token"},
	"POST /api/v1/tokens/{id}/quota":      {"token.quota_adjust", "token"},
	"POST /api/v1/admin-tokens":           {"admin_token.create", "admin_token"},
	"DELETE /api/v1/admin-tokens/{id}":    {"admin_token.revoke", "admin_token"},
	"POST /api/v1/models":                 {"model.create", "model"},
	"POST /api/v1/models/sync":            {"model.sync", "model"},
	"PATCH /api/v1/models/{id}":           {"model.update", "model"},
	"DELETE /api/v1/models/{id}":          {"model.delete", "model"},
	"DELETE /api/v1/logs":                 {"logs.purge", "logs"},
	"PUT /api/v1/settings":                {"settings.update", "settings"},
	"POST /api/v1/system/compact":         {"system.compact", "database"},
}

// redactKeys are request fields never copied into an event.
var redactKeys = map[string]bool{"key": true, "keys": true, "password": true, "old_password": true,
	"new_password": true, "current": true, "new": true, "old": true, "token": true, "secret": true}

const maxAuditBody = 4 << 10

type opCtxKey struct{}

// op is the operation being recorded for the current request.
type op struct {
	Action     string
	TargetID   string
	TargetName string
	TargetHint string
	Level      string
	Detail     map[string]any
	Recorded   bool // the handler wrote (or will write) its own event
}

// opFrom returns the operation of an audited request (nil otherwise).
func opFrom(r *http.Request) *op {
	o, _ := r.Context().Value(opCtxKey{}).(*op)
	return o
}

// actor describes the credential of the request for events.
func actor(c *credential) (string, string) {
	switch {
	case c == nil:
		return "anonymous", ""
	case c.Type == "admin_token":
		return "admin_token:" + c.ID, c.Name
	default:
		return "password", "admin"
	}
}

func (s *Server) clientIP(r *http.Request) string {
	return netutil.ClientIP(r, s.Settings.Get().TrustedProxies)
}

// newAudit starts an audit event for the request (actor, IP).
func (s *Server) newAudit(r *http.Request, action string) *store.Event {
	a, an := actor(credFrom(r))
	return &store.Event{Kind: store.EventAudit, Action: action, Actor: a, ActorName: an, IP: s.clientIP(r)}
}

// audit wraps a write handler and records it as an audit event when it
// succeeds.
func (s *Server) audit(pattern string, next http.HandlerFunc) http.HandlerFunc {
	act, ok := auditActions[pattern]
	if !ok {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var body []byte
		if r.Body != nil && r.ContentLength != 0 {
			body, _ = io.ReadAll(io.LimitReader(r.Body, 64<<20))
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		o := &op{Action: act[0], TargetID: r.PathValue("id")}
		var before map[string]any
		if o.TargetID != "" || act[1] == "settings" {
			// Resolved before the handler runs: a delete removes the object.
			before = s.snapshot(r.Context(), act[1], o.TargetID)
			o.TargetName, o.TargetHint = s.targetLabel(r.Context(), act[1], o.TargetID)
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r.WithContext(context.WithValue(r.Context(), opCtxKey{}, o)))
		if rec.status >= 400 || o.Recorded {
			return
		}
		detail := map[string]any{}
		if before != nil && r.Method != http.MethodDelete {
			after := s.snapshot(r.Context(), act[1], o.TargetID)
			if ch := diff(before, after); len(ch) > 0 {
				detail["changes"] = ch
				o.Action = refineAction(o.Action, ch)
			}
		} else if fields := redactedFields(body); len(fields) > 0 {
			detail["request"] = fields
		}
		if q := r.URL.RawQuery; q != "" {
			detail["query"] = q
		}
		for k, v := range o.Detail {
			detail[k] = v
		}
		if o.TargetID == "" && act[1] != "" && act[1] != "settings" { // created: id and name from the response
			var created struct {
				ID     json.Number `json:"id"`
				Name   string      `json:"name"`
				Prefix string      `json:"prefix"`
				Masked string      `json:"key_masked"`
			}
			if json.Unmarshal(rec.body.Bytes(), &created) == nil && created.ID != "" {
				o.TargetID, o.TargetName, o.TargetHint = created.ID.String(), created.Name, created.Prefix+created.Masked
			}
		}
		e := s.newAudit(r, o.Action)
		e.Level, e.TargetType, e.TargetID, e.TargetName, e.TargetHint, e.Detail = o.Level, act[1], o.TargetID,
			o.TargetName, o.TargetHint, detail
		s.recordEvent(r.Context(), e)
	}
}

// jobAudit records an operation that runs as a background job when the
// job ends (with its result), instead of when the request returns.
func (s *Server) jobAudit(r *http.Request, action, targetType string, request map[string]any) func(result any, err error) {
	e := s.newAudit(r, action)
	e.TargetType = targetType
	if o := opFrom(r); o != nil {
		o.Recorded = true
	}
	return func(result any, err error) {
		e.CreatedAt = 0 // the time the job finished
		e.Detail = map[string]any{"request": request, "result": result}
		if err != nil {
			e.Level, e.Detail["error"] = store.LevelError, err.Error()
		}
		s.recordEvent(context.Background(), e)
	}
}

// testSummary counts test results by status.
func testSummary(res []keytest.Result) map[string]any {
	by := map[string]int{}
	for _, r := range res {
		by[r.Status]++
	}
	return map[string]any{"total": len(res), "by_status": by}
}

// recordEvent appends an event, outliving the request's context.
func (s *Server) recordEvent(ctx context.Context, e *store.Event) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.DB.AddEvent(ctx, e); err != nil {
		s.Log.Error("record event", "action", e.Action, "err", err)
	}
}

// refineAction names an update by what it did when that is clearer: an
// update that only switches "enabled" is an enable or a disable.
func refineAction(action string, ch map[string]any) string {
	if !strings.HasSuffix(action, ".update") || len(ch) != 1 {
		return action
	}
	c, ok := ch["enabled"].(map[string]any)
	if !ok {
		return action
	}
	base := strings.TrimSuffix(action, ".update")
	if c["after"] == true {
		return base + ".enable"
	}
	return base + ".disable"
}

// targetLabel resolves the display name and hint (masked key, token prefix)
// of an object about to be changed.
func (s *Server) targetLabel(ctx context.Context, typ, id string) (string, string) {
	n, ok := parseID(id)
	switch {
	case typ == "model":
		return id, ""
	case !ok:
		return "", ""
	case typ == "key":
		if l, err := s.DB.KeyLabels(ctx, maskKey); err == nil {
			return l[n].Name, l[n].Hint
		}
	case typ == "token":
		if l, err := s.DB.TokenLabels(ctx); err == nil {
			return l[n].Name, l[n].Hint
		}
	case typ == "admin_token":
		if ts, err := s.DB.ListAdminTokens(ctx); err == nil {
			for _, t := range ts {
				if t.ID == n {
					return t.Name, t.Prefix
				}
			}
		}
	}
	return "", ""
}

// snapshot returns the auditable fields of an object (secrets masked), for
// computing what an update changed.
func (s *Server) snapshot(ctx context.Context, typ, id string) map[string]any {
	n, ok := parseID(id)
	var v any
	switch {
	case typ == "settings":
		b, _ := json.Marshal(s.Settings.Get())
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for k, val := range m {
			if str, ok := val.(string); ok {
				m[k] = maskURLCredentials(str)
			}
		}
		return m
	case typ == "model":
		if m, err := s.DB.GetModel(ctx, id); err == nil {
			v = modelJSON(m)
		}
	case !ok:
		return nil
	case typ == "key":
		if k, err := s.DB.GetKey(ctx, n); err == nil {
			v = map[string]any{"name": k.Name, "key": maskKey(k.Key), "base_url": maskURLCredentials(k.BaseURL),
				"priority": k.Priority, "weight": k.Weight, "enabled": k.Enabled, "max_concurrency": k.MaxConcurrency,
				"rpm_limit": k.RPMLimit, "notes": k.Notes}
		}
	case typ == "token":
		if t, err := s.DB.GetToken(ctx, n); err == nil {
			v = map[string]any{"name": t.Name, "prefix": t.Prefix, "enabled": t.Enabled, "expires_at": msTime(t.ExpiresAt),
				"allowed_models": t.AllowedModels, "notes": t.Notes}
		}
	}
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	delete(m, "updated_at")
	delete(m, "synced_at")
	return m
}

// diff lists the fields whose value changed: {field: {before, after}}.
func diff(before, after map[string]any) map[string]any {
	out := map[string]any{}
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	for k := range keys {
		if !reflect.DeepEqual(emptyAsNil(before[k]), emptyAsNil(after[k])) {
			out[k] = map[string]any{"before": before[k], "after": after[k]}
		}
	}
	return out
}

// emptyAsNil treats an empty list or object like a missing one, so a value
// normalised from null to [] is not reported as a change.
func emptyAsNil(v any) any {
	switch t := v.(type) {
	case []any:
		if len(t) == 0 {
			return nil
		}
	case map[string]any:
		if len(t) == 0 {
			return nil
		}
	}
	return v
}

// maskURLCredentials hides a user:password in a URL.
func maskURLCredentials(s string) string {
	if !strings.Contains(s, "@") || !strings.Contains(s, "://") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.User("***")
	return u.String()
}

// redactedFields returns the top-level fields of a JSON object body with
// secrets replaced, or a note when the body is not a small JSON object.
func redactedFields(body []byte) map[string]any {
	out := map[string]any{}
	if len(bytes.TrimSpace(body)) == 0 {
		return out
	}
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		out["body"] = "(not a JSON object)"
		return out
	}
	for k, v := range m {
		if redactKeys[strings.ToLower(k)] {
			out[k] = "[redacted]"
			continue
		}
		out[k] = v
	}
	if b, _ := json.Marshal(out); len(b) > maxAuditBody {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return map[string]any{"body": "(too large to record)", "fields": keys}
	}
	return out
}

// statusRecorder captures the status and (small) body of a response.
type statusRecorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.body.Len() < 64<<10 {
		r.body.Write(b[:min(len(b), 64<<10-r.body.Len())])
	}
	return r.ResponseWriter.Write(b)
}

// eventJSON is the API shape of an event (type audit or system).
func eventJSON(e *store.Event) map[string]any {
	return map[string]any{"type": e.Kind, "id": e.ID, "created_at": msTime(e.CreatedAt), "level": e.Level,
		"action": e.Action, "actor": e.Actor, "actor_name": e.ActorName, "target_type": e.TargetType,
		"target_id": e.TargetID, "target_name": e.TargetName, "target_hint": e.TargetHint, "ip": e.IP,
		"request_id": e.RequestID, "detail": e.Detail}
}

// getEvent returns one event (GET /api/v1/events/{id}).
func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	e, err := s.DB.GetEvent(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "event")
		return
	}
	writeJSON(w, http.StatusOK, eventJSON(e))
}
