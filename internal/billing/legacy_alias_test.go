package billing

import (
	"context"
	"slices"
	"testing"

	"github.com/Kiyliy/xai-router/internal/store"
)

// Legacy ids that xAI resolves to grok-4.3 are aliases of it, so tokens
// with a spending limit can still use them (acceptance round 6, B4).
func TestLegacyIDsResolveToGrok43(t *testing.T) {
	tbl := NewTable()
	tbl.Set(BuiltinModels())
	for _, id := range []string{"grok-3", "grok-3-mini", "grok-4", "grok-4-latest", "grok-4-0709", "grok-4-fast",
		"grok-4-fast-reasoning", "grok-4-1-fast", "grok-4-1-fast-reasoning"} {
		if m := tbl.Lookup(id); m == nil || m.ID != "grok-4.3" {
			t.Errorf("%s -> %+v", id, m)
		}
	}
}

// An existing database learns new built-in aliases on start, without
// touching the row's prices.
func TestSeedBuiltinAddsMissingAliases(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	old := &store.Model{ID: "grok-4.3", Kind: "language", Enabled: true, Source: "sync", Aliases: []string{"grok-4.3-latest"},
		InputTicks: 99, CachedTicks: 1, OutputTicks: 2}
	if err := db.UpsertModel(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	m, err := db.GetModel(ctx, "grok-4.3")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(m.Aliases, "grok-3") || !slices.Contains(m.Aliases, "grok-4.3-latest") || m.InputTicks != 99 {
		t.Fatalf("row after seed: aliases %v input %d", m.Aliases, m.InputTicks)
	}
}

// A sync keeps built-in aliases upstream does not list, and a second sync
// with the same upstream reports nothing updated.
func TestSyncKeepsBuiltinAliases(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	f.set("/v1/language-models", `{"models":[{"id":"grok-4.3","aliases":["grok-4.3-latest"],"prompt_text_token_price":12500,"cached_prompt_text_token_price":2000,"completion_text_token_price":25000}]}`)
	syncOnce(t, db, f, false)
	m, err := db.GetModel(ctx, "grok-4.3")
	if err != nil || !slices.Contains(m.Aliases, "grok-3") || m.Aliases[0] != "grok-4.3-latest" {
		t.Fatalf("aliases after sync: %+v %v", m, err)
	}
	if res, _ := syncOnce(t, db, f, false); slices.Contains(res.Updated, "grok-4.3") {
		t.Fatalf("second sync updated grok-4.3 again: %v", res.Updated)
	}
}
