package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Kiyliy/xai-router/internal/store"
)

func logSummaryJSON(l *store.Log) map[string]any {
	var tokID, keyID any
	if l.TokenID != 0 {
		tokID = l.TokenID
	}
	if l.KeyID != 0 {
		keyID = l.KeyID
	}
	attempts := l.AttemptCount
	if len(l.Attempts) > attempts {
		attempts = len(l.Attempts)
	}
	return map[string]any{
		"id": l.ID, "request_id": l.RequestID, "created_at": msTime(l.CreatedAt), "method": l.Method,
		"path": l.Path, "model": l.Model, "stream": l.Stream, "started_at": msTime(l.StartedAt), "status": l.Status, "error": l.Error,
		"client_ip": l.ClientIP, "token_id": tokID, "token_name": l.TokenName, "key_id": keyID,
		"key_name": l.KeyName, "attempts": attempts, "ttfb_ms": l.TTFBMs, "latency_ms": l.LatencyMs,
		"prompt_tokens": l.PromptTokens, "completion_tokens": l.CompletionTokens, "cached_tokens": l.CachedTokens,
		"reasoning_tokens": l.ReasoningTokens, "image_tokens": l.ImageTokens, "total_tokens": l.TotalTokens,
		"images": l.Images, "video_seconds": l.VideoSeconds, "cost_usd": usd(l.CostTicks), "cost_source": l.CostSource,
		"websocket": l.WS, "type": "request", "level": logLevel(l),
		"cache_write_tokens": l.CacheWriteTokens, // null: the upstream did not report it
		"attempt_index":      max(l.AttemptIndex, 1), "attempt_total": max(l.AttemptTotal, 1),
		"attempt_final": l.AttemptFinal || l.AttemptTotal <= 1, "attempt_state": l.AttemptState(),
	}
}

// logLevel is a log row's level: an attempt that failed and was retried is
// a warning, the failed last attempt of a retried request an error, and a
// single attempt follows its status (requestLevel).
func logLevel(l *store.Log) string {
	switch l.AttemptState() {
	case "retried":
		return store.LevelWarn
	case "final":
		if !l.Success() {
			return store.LevelError
		}
	}
	return requestLevel(l.Status)
}

// chainEntryJSON summarises one row of a retry chain.
func chainEntryJSON(l *store.Log) map[string]any {
	var keyID any
	if l.KeyID != 0 {
		keyID = l.KeyID
	}
	return map[string]any{"id": l.ID, "attempt_index": l.AttemptIndex, "attempt_total": l.AttemptTotal,
		"attempt_state": l.AttemptState(), "key_id": keyID, "key_name": l.KeyName, "status": l.Status,
		"error": l.Error, "action": l.AttemptAction, "ttfb_ms": l.TTFBMs, "latency_ms": l.LatencyMs,
		"created_at": msTime(l.CreatedAt), "started_at": msTime(l.StartedAt), "cost_usd": usd(l.CostTicks)}
}

func headersMap(s string) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

// requestLevel is a request's level for the unified log: 2xx/3xx/101 are
// info, 4xx (including the gateway's own refusals) and 499 are warnings,
// 5xx and failures without a status are errors.
func requestLevel(status int) string {
	switch {
	case status == 101 || status >= 200 && status < 400:
		return store.LevelInfo
	case status >= 400 && status < 500:
		return store.LevelWarn
	default:
		return store.LevelError
	}
}

// listLogs serves the unified log: requests (request_logs) and audit and
// system events (events), selected with type=request,audit,system.
//
// type=request alone (the default, as before v0.4.1) pages by number.
// Any other selection merges the sources newest first and pages with a
// cursor (next_cursor), which stays cheap however deep the page.
func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := paging(r)
	types := map[string]bool{}
	for _, t := range strings.Split(q.Get("type"), ",") {
		switch t = strings.TrimSpace(t); t {
		case "":
		case "request", store.EventAudit, store.EventSystem:
			types[t] = true
		default:
			writeErr(w, http.StatusBadRequest, "invalid_type", "type must be a list of request, audit and system")
			return
		}
	}
	if len(types) == 0 {
		types["request"] = true
	}
	f := store.LogFilter{Model: q.Get("model"), Status: q.Get("status"), Path: q.Get("path"), Q: strings.TrimSpace(q.Get("q")),
		Problems: q.Get("problems") == "1" || q.Get("problems") == "true", Limit: p.size, Offset: p.offset()}
	switch q.Get("attempts") {
	case "", "all":
	case "final":
		f.FinalOnly = true // the last row of each client request
	default:
		writeErr(w, http.StatusBadRequest, "invalid_attempts", "attempts must be all or final")
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
	f.KeyID, _ = strconv.ParseInt(q.Get("key_id"), 10, 64)
	f.TokenID, _ = strconv.ParseInt(q.Get("token_id"), 10, 64)
	if len(types) == 1 && types["request"] {
		s.listRequests(w, r, f, p)
		return
	}

	// Filters that only apply to one source exclude the other; key and
	// token filters apply to both (a key's requests and its events).
	ef := store.EventFilter{ProblemsOnly: f.Problems, Action: q.Get("action"), TargetType: q.Get("target_type"),
		TargetID: q.Get("target_id"), Actor: q.Get("actor"), From: f.From, To: f.To, Q: f.Q, Limit: p.size}
	// Event-only filters (action, object, actor) leave requests out.
	wantRequests := types["request"] && ef.Action == "" && ef.TargetType == "" && ef.TargetID == "" && ef.Actor == ""
	for _, k := range []string{store.EventAudit, store.EventSystem} {
		if types[k] {
			ef.Kinds = append(ef.Kinds, k)
		}
	}
	switch {
	case f.KeyID != 0 && f.TokenID != 0:
		ef.Kinds = nil // an event concerns one object
	case f.KeyID != 0:
		ef.TargetType, ef.TargetID = "key", strconv.FormatInt(f.KeyID, 10)
	case f.TokenID != 0:
		ef.TargetType, ef.TargetID = "token", strconv.FormatInt(f.TokenID, 10)
	}
	if f.Model != "" || f.Status != "" || f.Path != "" || f.FinalOnly { // request-only filters leave events out
		ef.Kinds = nil
	}
	wantEvents := len(ef.Kinds) > 0

	// Cursor: "<created_at ms>.<0 request | 1 event>.<id>" of the last row
	// shown. Order: created_at DESC, events before requests at the same
	// millisecond, id DESC.
	var curAt, curKind, curID int64
	if c := q.Get("cursor"); c != "" {
		parts := strings.Split(c, ".")
		if len(parts) != 3 {
			writeErr(w, http.StatusBadRequest, "invalid_cursor", "invalid cursor")
			return
		}
		curAt, _ = strconv.ParseInt(parts[0], 10, 64)
		curKind, _ = strconv.ParseInt(parts[1], 10, 64)
		curID, _ = strconv.ParseInt(parts[2], 10, 64)
		if curAt <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid_cursor", "invalid cursor")
			return
		}
	}
	limit := p.size
	var reqs []*store.Log
	var evs []*store.Event
	var total int64
	totalKind := "exact"
	if wantRequests {
		rf := f
		rf.Limit, rf.Offset = limit+1, 0
		if curAt > 0 {
			rf.NoCount = true
			rf.BeforeAt, rf.BeforeID = curAt, curID
			if curKind == 1 { // the cursor is an event: requests at that millisecond are all still to come
				rf.BeforeAt, rf.BeforeID = curAt+1, 0
			}
		}
		pg, err := s.DB.ListLogsPage(r.Context(), rf)
		if err != nil {
			s.internalErr(w, err)
			return
		}
		reqs = pg.Logs
		total += pg.Total
		if pg.TotalKind != "" && pg.TotalKind != "exact" {
			totalKind = pg.TotalKind
		}
	}
	if wantEvents {
		ef2 := ef
		ef2.Limit = limit + 1
		if curAt > 0 {
			ef2.BeforeAt, ef2.BeforeID = curAt, curID
			if curKind == 0 { // the cursor is a request: events at that millisecond were shown first
				ef2.BeforeAt, ef2.BeforeID = curAt, 0
			}
		} else {
			n, err := s.DB.CountEvents(r.Context(), ef)
			if err != nil {
				s.internalErr(w, err)
				return
			}
			total += n
		}
		evs, err = s.DB.ListEvents(r.Context(), ef2)
		if err != nil {
			s.internalErr(w, err)
			return
		}
	}

	// Merge newest first.
	labels := s.hints(r.Context(), "token")
	out := make([]map[string]any, 0, limit)
	var next string
	i, j := 0, 0
	for len(out) < limit && (i < len(reqs) || j < len(evs)) {
		takeEvent := j < len(evs) && (i >= len(reqs) || evs[j].CreatedAt >= reqs[i].CreatedAt)
		if takeEvent {
			e := evs[j]
			j++
			out = append(out, eventJSON(e))
			next = strconv.FormatInt(e.CreatedAt, 10) + ".1." + strconv.FormatInt(e.ID, 10)
		} else {
			l := reqs[i]
			i++
			row := logSummaryJSON(l)
			row["token_prefix"] = firstNonEmpty(l.TokenPrefix, labels[strconv.FormatInt(l.TokenID, 10)])
			out = append(out, row)
			next = strconv.FormatInt(l.CreatedAt, 10) + ".0." + strconv.FormatInt(l.ID, 10)
		}
	}
	if i >= len(reqs) && j >= len(evs) {
		next = "" // nothing after this page
	}
	resp := map[string]any{"data": out, "page_size": limit, "next_cursor": next, "types": typesList(types)}
	if curAt == 0 {
		resp["total"], resp["total_kind"], resp["total_exact"] = total, totalKind, totalKind == "exact"
	}
	writeJSON(w, http.StatusOK, resp)
}

func typesList(t map[string]bool) []string {
	var out []string
	for _, k := range []string{"request", store.EventAudit, store.EventSystem} {
		if t[k] {
			out = append(out, k)
		}
	}
	return out
}

// listRequests is the request-only view (paged by number).
func (s *Server) listRequests(w http.ResponseWriter, r *http.Request, f store.LogFilter, p page) {
	pg, err := s.DB.ListLogsPage(r.Context(), f)
	if err != nil {
		s.internalErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(pg.Logs))
	prefixes := s.hints(r.Context(), "token")
	for _, l := range pg.Logs {
		row := logSummaryJSON(l)
		row["token_prefix"] = firstNonEmpty(l.TokenPrefix, prefixes[strconv.FormatInt(l.TokenID, 10)])
		out = append(out, row)
	}
	resp := pageJSON(out, int(pg.Total), p)
	resp["total_kind"] = pg.TotalKind // exact, approx or at_least
	resp["total_exact"] = pg.TotalKind == "exact"
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getLog(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	l, err := s.DB.GetLog(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "log")
		return
	}
	out := logSummaryJSON(l)
	out["token_prefix"] = firstNonEmpty(l.TokenPrefix, s.hints(r.Context(), "token")[strconv.FormatInt(l.TokenID, 10)])
	attempts := make([]map[string]any, 0, len(l.Attempts))
	for _, a := range l.Attempts {
		var kid any
		if a.KeyID != 0 {
			kid = a.KeyID
		}
		attempts = append(attempts, map[string]any{"seq": a.Seq, "key_id": kid, "key_name": a.KeyName,
			"status": a.Status, "error": a.Error, "action": a.Action, "ttfb_ms": a.TTFBMs,
			"latency_ms": a.LatencyMs, "started_at": msTime(a.StartedAt)})
	}
	// The rows of the same client request (its upstream attempts), when
	// there are several; a single attempt has none.
	chain := []map[string]any{}
	if l.AttemptTotal > 1 {
		rows, err := s.DB.LogChain(r.Context(), l.RequestID)
		if err != nil {
			s.internalErr(w, err)
			return
		}
		for _, c := range rows {
			chain = append(chain, chainEntryJSON(c))
		}
	}
	out["chain"] = chain
	if l.ReqBodyFrom != 0 && l.ReqBodyFrom != l.ID {
		out["request_body_from"] = l.ReqBodyFrom
	} else {
		out["request_body_from"] = nil
	}
	var extra map[string]any
	if l.Extra != "" && json.Unmarshal([]byte(l.Extra), &extra) == nil {
		out["extra"] = extra
	} else {
		out["extra"] = nil
	}
	out["query"] = l.Query
	out["user_agent"] = l.UserAgent
	out["upstream_id"] = l.UpstreamID
	out["request_headers"] = headersMap(l.ReqHeaders)
	out["request_body"] = l.ReqBody
	out["response_headers"] = headersMap(l.RespHeaders)
	out["response_body"] = l.RespBody
	out["response_text"] = l.RespText
	out["reasoning_text"] = l.ReasoningText
	out["truncated"] = l.Truncated
	// Unit prices the request was billed at (absent before v0.4.1).
	var snap map[string]any
	if l.PriceSnapshot != "" && json.Unmarshal([]byte(l.PriceSnapshot), &snap) == nil {
		out["price_snapshot"] = snap
	} else {
		out["price_snapshot"] = nil
	}
	out["upstream_attempts"] = attempts
	if l.WS && l.Extra != "" {
		var ws map[string]any
		if json.Unmarshal([]byte(l.Extra), &ws) == nil {
			out["websocket_session"] = ws
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteLogs(w http.ResponseWriter, r *http.Request) {
	before, err := parseTimeParam(r.URL.Query().Get("before"))
	if err != nil || before == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_time", "before is required (RFC 3339 or unix seconds)")
		return
	}
	// Both paths record one audit event when the deletion ends, with the
	// cutoff and the number of request logs deleted. (A purge does not
	// return space to the disk; that is the compact job.)
	finish := s.jobAudit(r, "logs.purge", "logs", map[string]any{"before": msTime(before)})
	result := func(n int64) map[string]any { return map[string]any{"deleted": n, "before": msTime(before)} }
	if r.URL.Query().Get("wait") == "true" {
		n, err := s.DB.DeleteLogsBefore(r.Context(), before)
		finish(result(n), err)
		if err != nil {
			s.internalErr(w, err)
			return
		}
		_ = s.DB.Vacuum(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
		return
	}
	// Purging millions of rows takes minutes: run it in the background in
	// small chunks (requests keep flowing) and report progress as a job.
	j, _ := s.jobs.start("purge_logs", func(ctx context.Context, j *Job) (any, error) {
		total := s.DB.CountLogsBefore(ctx, before)
		s.jobs.update(j, func(j *Job) { j.Phase, j.Total = "delete", total })
		n, err := s.DB.DeleteLogsBeforeProgress(ctx, before, func(done int64) {
			s.jobs.update(j, func(j *Job) { j.Progress = done })
		})
		if err != nil {
			finish(result(n), err)
			return map[string]any{"deleted": n}, err
		}
		// The freed pages are not returned to the disk here: new logs reuse
		// them, and incremental_vacuum after a mass delete moves pages one
		// by one (measured ~25 pages/s, a day for 13 GB). Returning space to
		// the OS is the explicit, paced "compact" job.
		_ = s.DB.Vacuum(ctx) // WAL checkpoint
		finish(result(n), nil)
		return map[string]any{"deleted": n}, nil
	})
	writeJSON(w, http.StatusAccepted, j)
}

// Headers worth repeating in a reproduced request.
var curlHeaders = []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta", "X-Grok-Conv-Id", "Openai-Beta"}

func (s *Server) logCurl(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	l, err := s.DB.GetLog(r.Context(), id)
	if err != nil {
		s.storeErr(w, err, "log")
		return
	}
	base := strings.TrimRight(r.URL.Query().Get("base_url"), "/")
	if base == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	url := base + l.Path
	if l.Query != "" {
		url += "?" + l.Query
	}
	var b strings.Builder
	b.WriteString("curl")
	if l.Method != http.MethodGet {
		b.WriteString(" -X " + l.Method)
	}
	b.WriteString(" " + shellQuote(url))
	if l.Stream {
		b.WriteString(" -N")
	}
	// Double quotes so the shell expands $XR_API_KEY.
	b.WriteString(" \\\n  -H \"Authorization: Bearer $XR_API_KEY\"")
	hdrs := headersMap(l.ReqHeaders)
	keys := make([]string, 0, len(hdrs))
	for k := range hdrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, want := range curlHeaders {
		for _, k := range keys {
			if strings.EqualFold(k, want) {
				b.WriteString(" \\\n  -H " + shellQuote(k+": "+hdrs[k]))
			}
		}
	}
	if l.ReqBody != "" {
		b.WriteString(" \\\n  --data-raw " + shellQuote(l.ReqBody))
	}
	out := map[string]any{"curl": b.String()}
	if l.Truncated {
		out["warning"] = "the stored request body was truncated or had base64 content removed; the command may not reproduce the request exactly"
	}
	writeJSON(w, http.StatusOK, out)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
