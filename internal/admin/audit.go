package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Kiyliy/xai-router/internal/netutil"
	"github.com/Kiyliy/xai-router/internal/store"
)

// Every successful write through the admin API is recorded as an operation
// event (see store.Event): who (password session or which admin API token),
// from where, what, on which object, with the request fields (secrets
// redacted). Handlers can name the target or add detail through opFrom;
// a handler that records its own event (atomically with the change, like
// quota adjustments) marks the operation as recorded.

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
	TargetName string
	TargetID   string
	Detail     map[string]any
	Recorded   bool // the handler wrote its own event
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
		return "", ""
	case c.Type == "admin_token":
		return "admin_token:" + c.ID, c.Name
	default:
		return "password", "admin"
	}
}

func (s *Server) clientIP(r *http.Request) string {
	return netutil.ClientIP(r, s.Settings.Get().TrustedProxies)
}

// audit wraps a write handler and records it as an operation event when it
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
		o := &op{TargetID: r.PathValue("id")}
		if o.TargetID != "" {
			o.TargetName = s.targetName(r.Context(), act[1], o.TargetID) // before a delete removes it
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(rec, r.WithContext(context.WithValue(r.Context(), opCtxKey{}, o)))
		if rec.status >= 400 || o.Recorded {
			return
		}
		detail := redactedFields(body)
		if q := r.URL.RawQuery; q != "" {
			detail["query"] = q
		}
		for k, v := range o.Detail {
			detail[k] = v
		}
		if o.TargetID == "" && act[1] != "" { // created: take the id and name from the response
			var created struct {
				ID   json.Number `json:"id"`
				Name string      `json:"name"`
			}
			if json.Unmarshal(rec.body.Bytes(), &created) == nil && created.ID != "" {
				o.TargetID, o.TargetName = created.ID.String(), created.Name
			}
		}
		a, an := actor(credFrom(r))
		e := &store.Event{Kind: store.EventOperation, Action: act[0], Actor: a, ActorName: an,
			TargetType: act[1], TargetID: o.TargetID, TargetName: o.TargetName, IP: s.clientIP(r), Detail: detail}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5e9)
		defer cancel()
		if err := s.DB.AddEvent(ctx, e); err != nil {
			s.Log.Error("record operation", "action", act[0], "err", err)
		}
	}
}

// targetName resolves the display name of an object about to be changed.
func (s *Server) targetName(ctx context.Context, typ, id string) string {
	n, ok := parseID(id)
	switch {
	case typ == "model":
		return id
	case !ok:
		return ""
	case typ == "key":
		if k, err := s.DB.GetKey(ctx, n); err == nil {
			return k.Name
		}
	case typ == "token":
		if t, err := s.DB.GetToken(ctx, n); err == nil {
			return t.Name
		}
	case typ == "admin_token":
		if ts, err := s.DB.ListAdminTokens(ctx); err == nil {
			for _, t := range ts {
				if t.ID == n {
					return t.Name
				}
			}
		}
	}
	return ""
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
		return map[string]any{"body": "(too large to record)", "fields": keysOf(m)}
	}
	return out
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
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

// systemEvent records something the gateway did on its own.
func (s *Server) systemEvent(ctx context.Context, action, targetType, targetID, targetName string, detail map[string]any) {
	e := &store.Event{Kind: store.EventSystem, Action: action, Actor: "system", ActorName: "system",
		TargetType: targetType, TargetID: targetID, TargetName: targetName, Detail: detail}
	if err := s.DB.AddEvent(ctx, e); err != nil {
		s.Log.Error("record system event", "action", action, "err", err)
	}
}

// listEvents returns the event log (operation and system events), newest
// first. Filters: kind, action (exact, or a prefix ending in "."),
// target_type, target_id, from, to, q.
func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := paging(r)
	f := store.EventFilter{Kind: q.Get("kind"), Action: q.Get("action"), TargetType: q.Get("target_type"),
		TargetID: q.Get("target_id"), Q: strings.TrimSpace(q.Get("q")), Limit: p.size, Offset: p.offset()}
	if f.Kind != "" && f.Kind != store.EventOperation && f.Kind != store.EventSystem {
		writeErr(w, http.StatusBadRequest, "invalid_kind", "kind must be operation or system")
		return
	}
	var err error
	if f.From, err = parseTimeParam(q.Get("from")); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_time", err.Error())
		return
	}
	if f.To, err = parseTimeParam(q.Get("to")); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_time", err.Error())
		return
	}
	evs, total, err := s.DB.ListEvents(r.Context(), f)
	if err != nil {
		s.internalErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(evs))
	for _, e := range evs {
		out = append(out, map[string]any{"id": e.ID, "created_at": msTime(e.CreatedAt), "kind": e.Kind, "action": e.Action,
			"actor": e.Actor, "actor_name": e.ActorName, "target_type": e.TargetType, "target_id": e.TargetID,
			"target_name": e.TargetName, "ip": e.IP, "detail": e.Detail})
	}
	writeJSON(w, http.StatusOK, pageJSON(out, int(total), p))
}
