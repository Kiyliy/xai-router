package store

import (
	"context"
	"fmt"
	"strconv"
)

// Agg is an aggregate over stats_hourly rows.
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
	err := db.R.QueryRowContext(ctx, `SELECT `+aggCols+` FROM stats_hourly WHERE hour >= ?`, from).Scan(a.dest()...)
	return a, err
}

// GroupedAgg is an aggregate for one group value.
type GroupedAgg struct {
	Group string
	Agg
}

func groupColumn(by string) (string, error) {
	switch by {
	case "model":
		return "model", nil
	case "key":
		return "CAST(key_id AS TEXT)", nil
	case "token":
		return "CAST(token_id AS TEXT)", nil
	}
	return "", fmt.Errorf("invalid group %q", by)
}

// StatsBreakdown aggregates by model, key or token since `from`.
func (db *DB) StatsBreakdown(ctx context.Context, from int64, by string) ([]GroupedAgg, error) {
	col, err := groupColumn(by)
	if err != nil {
		return nil, err
	}
	rows, err := db.R.QueryContext(ctx, `SELECT `+col+` AS g, `+aggCols+` FROM stats_hourly WHERE hour >= ?
GROUP BY g ORDER BY SUM(requests) DESC`, from)
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

// SeriesRow is one time bucket of one group.
type SeriesRow struct {
	Bucket int64
	Group  string
	Agg
}

// StatsSeries aggregates per bucket (bucketMs = 3600000 or 86400000) and
// optional group ("" = none). Day buckets are aligned to UTC midnight.
func (db *DB) StatsSeries(ctx context.Context, from, bucketMs int64, by string) ([]SeriesRow, error) {
	col := "''"
	if by != "" && by != "none" {
		c, err := groupColumn(by)
		if err != nil {
			return nil, err
		}
		col = c
	}
	q := `SELECT (hour / ` + strconv.FormatInt(bucketMs, 10) + `) * ` + strconv.FormatInt(bucketMs, 10) + ` AS b, ` + col +
		` AS g, ` + aggCols + ` FROM stats_hourly WHERE hour >= ? GROUP BY b, g ORDER BY b`
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
