package admin

import (
	"context"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"time"

	"github.com/Kiyliy/xai-router/internal/store"
)

var ranges = map[string]time.Duration{
	"1h": time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour,
}

// rangeStart returns the hour-aligned start of the requested range.
func rangeStart(r *http.Request) (string, int64, bool) {
	rg := r.URL.Query().Get("range")
	if rg == "" {
		rg = "24h"
	}
	d, ok := ranges[rg]
	if !ok {
		return rg, 0, false
	}
	from := time.Now().Add(-d)
	if d > time.Hour {
		// Include the partial first hour so totals cover the whole window.
		from = from.Truncate(time.Hour).Add(time.Hour)
	} else {
		from = from.Truncate(time.Hour)
	}
	return rg, from.UnixMilli(), true
}

func aggJSON(a store.Agg) map[string]any {
	rate, lat := 0.0, 0.0
	if a.Requests > 0 {
		rate = float64(a.Success) / float64(a.Requests)
		lat = float64(a.LatencySum) / float64(a.Requests)
	}
	return map[string]any{
		"requests": a.Requests, "success": a.Success, "errors": a.Errors, "success_rate": rate,
		"avg_latency_ms": lat, "prompt_tokens": a.PromptTokens, "completion_tokens": a.CompletionTokens,
		"cached_tokens": a.CachedTokens, "reasoning_tokens": a.ReasoningTokens, "total_tokens": a.TotalTokens,
		"cost_usd": usd(a.CostTicks),
	}
}

func (s *Server) statsOverview(w http.ResponseWriter, r *http.Request) {
	rg, from, ok := rangeStart(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_range", "range must be one of 1h, 24h, 7d, 30d, 90d")
		return
	}
	a, err := s.DB.StatsTotal(r.Context(), from)
	if err != nil {
		s.internalErr(w, err)
		return
	}
	kc, err := s.DB.CountKeys(r.Context())
	if err != nil {
		s.internalErr(w, err)
		return
	}
	tt, te, err := s.DB.TokenCounts(r.Context())
	if err != nil {
		s.internalErr(w, err)
		return
	}
	out := aggJSON(a)
	out["range"] = rg
	out["keys"] = map[string]int{"total": kc.Total, "active": kc.Active, "cooldown": kc.Cooldown, "disabled": kc.Disabled}
	out["tokens"] = map[string]int{"total": tt, "enabled": te}
	writeJSON(w, http.StatusOK, out)
}

// names resolves key / token ids to display names.
func (s *Server) names(ctx context.Context, by string) map[string]string {
	m := map[string]string{"0": "(none)"}
	switch by {
	case "key":
		if keys, err := s.DB.ListAllKeys(ctx); err == nil {
			for _, k := range keys {
				m[strconv.FormatInt(k.ID, 10)] = k.Name
			}
		}
	case "token":
		if ts, _, err := s.DB.ListTokens(ctx, "", 100000, 0); err == nil {
			for _, t := range ts {
				m[strconv.FormatInt(t.ID, 10)] = t.Name
			}
		}
	}
	return m
}

func (s *Server) displayName(names map[string]string, by, id string) string {
	if by == "model" {
		if id == "" {
			return "(unknown)"
		}
		return id
	}
	if n, ok := names[id]; ok {
		return n
	}
	return "#" + id + " (deleted)"
}

func (s *Server) statsBreakdown(w http.ResponseWriter, r *http.Request) {
	_, from, ok := rangeStart(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_range", "range must be one of 1h, 24h, 7d, 30d, 90d")
		return
	}
	by := r.URL.Query().Get("by")
	rows, err := s.DB.StatsBreakdown(r.Context(), from, by)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_group", "by must be model, key or token")
		return
	}
	names := s.names(r.Context(), by)
	out := make([]map[string]any, 0, len(rows))
	for _, g := range rows {
		row := aggJSON(g.Agg)
		row["id"] = g.Group
		row["name"] = s.displayName(names, by, g.Group)
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": out})
}

func (s *Server) statsTimeseries(w http.ResponseWriter, r *http.Request) {
	rg, from, ok := rangeStart(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid_range", "range must be one of 1h, 24h, 7d, 30d, 90d")
		return
	}
	q := r.URL.Query()
	interval := q.Get("interval")
	if interval == "" {
		switch rg {
		case "1h":
			interval = "5m"
		case "30d", "90d":
			interval = "day"
		default:
			interval = "hour"
		}
	}
	bucket := int64(3600_000)
	switch interval {
	case "hour":
	case "day":
		bucket = 86400_000
	case "5m":
		// Fine-grained buckets come from raw logs; keep the range short.
		if rg != "1h" && rg != "24h" {
			writeErr(w, http.StatusBadRequest, "invalid_interval", "interval 5m is only available for ranges up to 24h")
			return
		}
		bucket = 300_000
		from = time.Now().Add(-ranges[rg]).UnixMilli() / bucket * bucket
	default:
		writeErr(w, http.StatusBadRequest, "invalid_interval", "interval must be 5m, hour or day")
		return
	}
	group := q.Get("group")
	if group == "" {
		group = "none"
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 6
	}
	var rows []store.SeriesRow
	var err error
	if bucket < 3600_000 {
		rows, err = s.DB.StatsSeriesLogs(r.Context(), from, bucket, group)
	} else {
		rows, err = s.DB.StatsSeries(r.Context(), from, bucket, group)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_group", "group must be none, model, key or token")
		return
	}
	// Buckets from the start of the range to now.
	start := from / bucket * bucket
	now := time.Now().UnixMilli()
	var buckets []int64
	for b := start; b <= now; b += bucket {
		buckets = append(buckets, b)
	}
	idx := make(map[int64]int, len(buckets))
	for i, b := range buckets {
		idx[b] = i
	}
	// Pick the top groups by requests; merge the rest into "other".
	totals := map[string]int64{}
	for _, r := range rows {
		totals[r.Group] += r.Requests
	}
	groups := make([]string, 0, len(totals))
	for g := range totals {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if totals[groups[i]] != totals[groups[j]] {
			return totals[groups[i]] > totals[groups[j]]
		}
		return groups[i] < groups[j]
	})
	keep := map[string]bool{}
	for i, g := range groups {
		if group == "none" || i < limit {
			keep[g] = true
		}
	}
	names := s.names(r.Context(), group)
	type acc struct {
		name string
		aggs []store.Agg
	}
	series := map[string]*acc{}
	var order []string
	get := func(name string) *acc {
		if a, ok := series[name]; ok {
			return a
		}
		a := &acc{name: name, aggs: make([]store.Agg, len(buckets))}
		series[name] = a
		order = append(order, name)
		return a
	}
	for _, g := range groups {
		if keep[g] {
			name := "all"
			if group != "none" {
				name = s.displayName(names, group, g)
			}
			get(name)
		}
	}
	for _, row := range rows {
		i, ok := idx[row.Bucket]
		if !ok {
			continue
		}
		name := "all"
		if group != "none" {
			name = "other"
			if keep[row.Group] {
				name = s.displayName(names, group, row.Group)
			}
		}
		a := &get(name).aggs[i]
		a.Requests += row.Requests
		a.Success += row.Success
		a.Errors += row.Errors
		a.TotalTokens += row.TotalTokens
		a.CostTicks += row.CostTicks
		a.LatencySum += row.LatencySum
	}
	if len(order) == 0 {
		get("all")
	}
	ts := make([]time.Time, len(buckets))
	for i, b := range buckets {
		ts[i] = time.UnixMilli(b).UTC()
	}
	out := make([]map[string]any, 0, len(order))
	for _, name := range order {
		a := series[name]
		pts := make([]map[string]any, len(buckets))
		for i, ag := range a.aggs {
			lat := 0.0
			if ag.Requests > 0 {
				lat = float64(ag.LatencySum) / float64(ag.Requests)
			}
			pts[i] = map[string]any{"t": ts[i], "requests": ag.Requests, "success": ag.Success, "errors": ag.Errors,
				"total_tokens": ag.TotalTokens, "cost_usd": usd(ag.CostTicks), "avg_latency_ms": lat}
		}
		out = append(out, map[string]any{"name": name, "points": pts})
	}
	writeJSON(w, http.StatusOK, map[string]any{"interval": interval, "buckets": ts, "series": out})
}

func (s *Server) systemInfo(w http.ResponseWriter, r *http.Request) {
	gen, _ := s.DB.GetMeta(r.Context(), MetaPasswordGenerated)
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.Build.Version, "commit": s.Build.Commit, "build_date": s.Build.Date,
		"go_version": runtime.Version(), "started_at": s.Started.UTC(),
		"uptime_seconds": int64(time.Since(s.Started).Seconds()), "db_size_bytes": s.DB.Size(),
		"log_queue_length": s.Logs.QueueLen(), "log_dropped": s.Logs.Dropped(),
		"needs_password_change": gen == "1",
	})
}
