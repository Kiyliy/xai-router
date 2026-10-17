package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// ImageTier is a quality/resolution specific price: per image for image
// models (Quality may be empty when only the resolution matters), per second
// for video models (VideoTiers; Quality unused).
type ImageTier struct {
	Quality    string `json:"quality"`
	Resolution string `json:"resolution"`
	Ticks      int64  `json:"ticks"`
}

// Model is a price-table entry. Token prices are ticks per token, image prices
// ticks per image and video prices ticks per second (1 USD = 1e10 ticks).
type Model struct {
	ID               string
	Kind             string // language, image, video, embedding
	Aliases          []string
	Enabled          bool
	Source           string // sync, manual, builtin
	ContextLength    int64
	InputTicks       int64
	CachedTicks      int64
	ImageTokenTicks  int64
	OutputTicks      int64
	LongThreshold    int64
	LongInputTicks   int64
	LongCachedTicks  int64
	LongOutputTicks  int64
	ImageTicks       int64
	ImageTiers       []ImageTier
	VideoTicksPerSec int64
	VideoTiers       []ImageTier // per-second price by resolution
	// Media input prices: per input image, per second of input video.
	InputImageTicks       int64
	InputVideoTicksPerSec int64
	Raw                   string
	SyncedAt              int64
	UpdatedAt             int64
}

const modelCols = `id, kind, aliases, enabled, source, context_length, input_ticks, cached_ticks, image_token_ticks,
output_ticks, long_threshold, long_input_ticks, long_cached_ticks, long_output_ticks, image_ticks, image_tiers,
video_ticks_per_sec, raw, synced_at, updated_at, video_tiers, input_image_ticks, input_video_ticks_per_sec`

func scanModel(s interface{ Scan(...any) error }) (*Model, error) {
	var m Model
	var aliases, tiers, vtiers string
	var en int
	err := s.Scan(&m.ID, &m.Kind, &aliases, &en, &m.Source, &m.ContextLength, &m.InputTicks, &m.CachedTicks,
		&m.ImageTokenTicks, &m.OutputTicks, &m.LongThreshold, &m.LongInputTicks, &m.LongCachedTicks, &m.LongOutputTicks,
		&m.ImageTicks, &tiers, &m.VideoTicksPerSec, &m.Raw, &m.SyncedAt, &m.UpdatedAt, &vtiers,
		&m.InputImageTicks, &m.InputVideoTicksPerSec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.Enabled = en == 1
	_ = json.Unmarshal([]byte(aliases), &m.Aliases)
	_ = json.Unmarshal([]byte(tiers), &m.ImageTiers)
	_ = json.Unmarshal([]byte(vtiers), &m.VideoTiers)
	if m.Aliases == nil {
		m.Aliases = []string{}
	}
	if m.ImageTiers == nil {
		m.ImageTiers = []ImageTier{}
	}
	if m.VideoTiers == nil {
		m.VideoTiers = []ImageTier{}
	}
	return &m, nil
}

// ListModels returns all models (optionally of one kind) ordered by kind and id.
func (db *DB) ListModels(ctx context.Context, kind string) ([]*Model, error) {
	q, args := `SELECT `+modelCols+` FROM models`, []any{}
	if kind != "" {
		q += ` WHERE kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY CASE kind WHEN 'language' THEN 0 WHEN 'image' THEN 1 WHEN 'video' THEN 2 ELSE 3 END, id`
	rows, err := db.R.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetModel returns one model by id.
func (db *DB) GetModel(ctx context.Context, id string) (*Model, error) {
	return scanModel(db.R.QueryRowContext(ctx, `SELECT `+modelCols+` FROM models WHERE id = ?`, id))
}

// UpsertModel inserts or replaces m.
func (db *DB) UpsertModel(ctx context.Context, m *Model) error {
	m.UpdatedAt = NowMs()
	aliases, _ := json.Marshal(nonNil(m.Aliases))
	tiers := m.ImageTiers
	if tiers == nil {
		tiers = []ImageTier{}
	}
	tj, _ := json.Marshal(tiers)
	vt := m.VideoTiers
	if vt == nil {
		vt = []ImageTier{}
	}
	vj, _ := json.Marshal(vt)
	_, err := db.W.ExecContext(ctx, `INSERT INTO models (`+modelCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET kind=excluded.kind, aliases=excluded.aliases, enabled=excluded.enabled,
source=excluded.source, context_length=excluded.context_length, input_ticks=excluded.input_ticks,
cached_ticks=excluded.cached_ticks, image_token_ticks=excluded.image_token_ticks, output_ticks=excluded.output_ticks,
long_threshold=excluded.long_threshold, long_input_ticks=excluded.long_input_ticks,
long_cached_ticks=excluded.long_cached_ticks, long_output_ticks=excluded.long_output_ticks,
image_ticks=excluded.image_ticks, image_tiers=excluded.image_tiers, video_ticks_per_sec=excluded.video_ticks_per_sec,
raw=excluded.raw, synced_at=excluded.synced_at, updated_at=excluded.updated_at, video_tiers=excluded.video_tiers,
input_image_ticks=excluded.input_image_ticks, input_video_ticks_per_sec=excluded.input_video_ticks_per_sec`,
		m.ID, m.Kind, string(aliases), boolInt(m.Enabled), m.Source, m.ContextLength, m.InputTicks, m.CachedTicks,
		m.ImageTokenTicks, m.OutputTicks, m.LongThreshold, m.LongInputTicks, m.LongCachedTicks, m.LongOutputTicks,
		m.ImageTicks, string(tj), m.VideoTicksPerSec, m.Raw, m.SyncedAt, m.UpdatedAt, string(vj),
		m.InputImageTicks, m.InputVideoTicksPerSec)
	return err
}

// DeleteModel removes a model.
func (db *DB) DeleteModel(ctx context.Context, id string) error {
	res, err := db.W.ExecContext(ctx, `DELETE FROM models WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
