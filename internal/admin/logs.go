package admin

import (
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
		"path": l.Path, "model": l.Model, "stream": l.Stream, "status": l.Status, "error": l.Error,
		"client_ip": l.ClientIP, "token_id": tokID, "token_name": l.TokenName, "key_id": keyID,
		"key_name": l.KeyName, "attempts": attempts, "ttfb_ms": l.TTFBMs, "latency_ms": l.LatencyMs,
		"prompt_tokens": l.PromptTokens, "completion_tokens": l.CompletionTokens, "cached_tokens": l.CachedTokens,
		"reasoning_tokens": l.ReasoningTokens, "image_tokens": l.ImageTokens, "total_tokens": l.TotalTokens,
		"images": l.Images, "video_seconds": l.VideoSeconds, "cost_usd": usd(l.CostTicks), "cost_source": l.CostSource,
	}
}

func headersMap(s string) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p := paging(r)
	f := store.LogFilter{Model: q.Get("model"), Status: q.Get("status"), Path: q.Get("path"), Q: strings.TrimSpace(q.Get("q")),
		Limit: p.size, Offset: p.offset()}
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
	logs, total, err := s.DB.ListLogs(r.Context(), f)
	if err != nil {
		s.internalErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		out = append(out, logSummaryJSON(l))
	}
	writeJSON(w, http.StatusOK, pageJSON(out, total, p))
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
	out["upstream_attempts"] = attempts
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteLogs(w http.ResponseWriter, r *http.Request) {
	before, err := parseTimeParam(r.URL.Query().Get("before"))
	if err != nil || before == 0 {
		writeErr(w, http.StatusBadRequest, "invalid_time", "before is required (RFC 3339 or unix seconds)")
		return
	}
	n, err := s.DB.DeleteLogsBefore(r.Context(), before)
	if err != nil {
		s.internalErr(w, err)
		return
	}
	_ = s.DB.Vacuum(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
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
	b.WriteString(" \\\n  -H " + shellQuote("Authorization: Bearer $XR_API_KEY"))
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
