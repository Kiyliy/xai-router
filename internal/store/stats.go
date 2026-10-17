package store

import (
	"context"
	"fmt"
	"strconv"
)

// Agg is an aggregate over hourly rollup rows (stats_dim).
type Agg struct {
	Requests, Success, Errors                           int64
	PromptTokens, CompletionTokens, CachedTokens        int64
	ReasoningTokens, TotalTokens, CostTicks, LatencySum int64
}

const aggCols = `COALESCE(SUM(requests),0), COALESCE(SUM(success),0), COALESCE(SUM(errors),0),
COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_ticks),0),
COALESCE(SUM(latency_ms_sum),0)`

func (a *Agg) dest() []any {
	return []any{&a.Requests, &a.Success, &a.Errors, &a.PromptTokens, &a.CompletionTokens, &a.CachedTokens,
		&a.ReasoningTokens, &a.TotalTokens, &a.CostTicks, &a.LatencySum}
}

// StatsTotal aggregates everything since `from` (unix ms, hour aligned).
func (db *DB) StatsTotal(ctx context.Context, from int64) (Agg, error) {
	var a Agg
	err := db.R.QueryRowContext(ctx, `SELECT `+aggCols+` FROM stats_dim WHERE dim = 'all' AND hour >= ?`, from).Scan(a.dest()...)
	return a, err
}

// GroupedAgg is an aggregate for one group value.
type GroupedAgg struct {
	Group string
	Agg
}

func checkDim(by string) error {
	switch by {
	case "model", "key", "token":
		return nil
	}
	return fmt.Errorf("invalid group %q", by)
}

// StatsBreakdown aggregates by model, key or token since `from`.
func (db *DB) StatsBreakdown(ctx context.Context, from int64, by string) ([]GroupedAgg, error) {
	if err := checkDim(by); err != nil {
		return nil, err
	}
	rows, err := db.R.QueryContext(ctx, `SELECT id AS g, `+aggCols+` FROM stats_dim WHERE dim = ? AND hour >= ?
GROUP BY g ORDER BY SUM(requests) DESC`, by, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupedAgg
	for rows.Next() {
		var g GroupedAgg
		if err := rows.Scan(append([]any{&g.Group}, g.dest()...)...); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// StatsSeriesLogs is StatsSeries computed from raw request logs, for
// sub-hour buckets over short ranges.
func (db *DB) StatsSeriesLogs(ctx context.Context, from, bucketMs int64, by string) ([]SeriesRow, error) {
	col := "''"
	switch by {
	case "", "none":
	case "model":
		col = "model"
	case "key":
		col = "CAST(COALESCE(key_id,0) AS TEXT)"
	case "token":
		col = "CAST(COALESCE(token_id,0) AS TEXT)"
	default:
		return nil, fmt.Errorf("invalid group %q", by)
	}
	b := strconv.FormatInt(bucketMs, 10)
	q := `SELECT (created_at / ` + b + `) * ` + b + ` AS bk, ` + col + ` AS g, COUNT(*),
COALESCE(SUM(CASE WHEN ` + successSQL + ` THEN 1 ELSE 0 END),0),
COALESCE(SUM(CASE WHEN ` + successSQL + ` THEN 0 ELSE 1 END),0),
COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(cost_ticks),0),
COALESCE(SUM(latency_ms),0) FROM request_logs WHERE created_at >= ? GROUP BY bk, g ORDER BY bk`
	rows, err := db.R.QueryContext(ctx, q, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesRow
	for rows.Next() {
		var r SeriesRow
		if err := rows.Scan(append([]any{&r.Bucket, &r.Group}, r.dest()...)...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeriesRow is one time bucket of one group.
type SeriesRow struct {
	Bucket int64
	Group  string
	Agg
}

// StatsSeries aggregates per bucket (bucketMs = 3600000 or 86400000) and
// optional group ("" = none). Buckets are aligned to midnight in the zone
// offsetMs east of UTC (e.g. +8h for China).
func (db *DB) StatsSeries(ctx context.Context, from, bucketMs, offsetMs int64, by string) ([]SeriesRow, error) {
	dim := "all"
	if by != "" && by != "none" {
		if err := checkDim(by); err != nil {
			return nil, err
		}
		dim = by
	}
	bs, off := strconv.FormatInt(bucketMs, 10), strconv.FormatInt(offsetMs, 10)
	q := `SELECT ((hour + ` + off + `) / ` + bs + `) * ` + bs + ` - ` + off + ` AS b, id AS g, ` + aggCols +
		` FROM stats_dim WHERE dim = ? AND hour >= ? GROUP BY b, g ORDER BY b`
	rows, err := db.R.QueryContext(ctx, q, dim, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SeriesRow
	for rows.Next() {
		var r SeriesRow
		if err := rows.Scan(append([]any{&r.Bucket, &r.Group}, r.dest()...)...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
