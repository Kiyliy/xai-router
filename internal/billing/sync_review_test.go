package billing

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/store"
)

func syncOnce(t *testing.T, db *store.DB, f *fakeXAI, overwrite bool) (SyncResult, *Table) {
	t.Helper()
	srv := httptest.NewServer(f)
	defer srv.Close()
	tbl := NewTable()
	res := (&Syncer{DB: db, Table: tbl, Client: srv.Client()}).Sync(context.Background(), srv.URL, "k", overwrite)
	if !res.OK {
		t.Fatalf("sync failed: %+v", res)
	}
	return res, tbl
}

// H1 and M4: a new model without a usable price is added disabled and
// reported; an embedding price from upstream is kept.
func TestSyncUnpricedModelsAddedDisabled(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	// An existing unpriced row keeps its state.
	if err := db.UpsertModel(ctx, &store.Model{ID: "vid-old", Kind: "video", Enabled: true, Source: "sync"}); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	f.set("/v1/language-models", `{"models":[{"id":"lang-free"},{"id":"lang-paid","prompt_text_token_price":5,"completion_text_token_price":6}]}`)
	f.set("/v1/image-generation-models", `{"models":[{"id":"img-free"},{"id":"img-paid","image_price":300000000}]}`)
	f.set("/v1/video-generation-models", `{"models":[{"id":"vid-new"},{"id":"vid-old"},{"id":"grok-imagine-video"}]}`)
	f.set("/v1/embedding-models", `{"models":[{"id":"emb-free"},{"id":"emb-paid","prompt_text_token_price":100,"prompt_image_token_price":7}]}`)
	res, tbl := syncOnce(t, db, f, false)
	if got := strings.Join(res.Unpriced, ","); got != "emb-free,img-free,lang-free,vid-new,vid-old" {
		t.Fatalf("unpriced %s", got)
	}
	for _, id := range []string{"emb-free", "img-free", "lang-free", "vid-new"} {
		if m := tbl.Lookup(id); m == nil || m.Enabled || m.Source != "sync" {
			t.Errorf("%s should be added disabled: %+v", id, m)
		}
	}
	for _, id := range []string{"emb-paid", "img-paid", "lang-paid", "grok-imagine-video", "vid-old"} {
		if m := tbl.Lookup(id); m == nil || !m.Enabled {
			t.Errorf("%s should be enabled: %+v", id, m)
		}
	}
	if m := tbl.Lookup("emb-paid"); m.InputTicks != 100 || m.ImageTokenTicks != 7 || m.Kind != "embedding" {
		t.Fatalf("embedding prices: %+v", m)
	}
	if HasPrice(&store.Model{Kind: "image", ImageTiers: []store.ImageTier{{Ticks: 1}}}) != true ||
		HasPrice(&store.Model{Kind: "video", VideoTiers: []store.ImageTier{{Ticks: 0}}}) != false ||
		HasPrice(&store.Model{Kind: "other"}) != true {
		t.Fatal("HasPrice")
	}
}

// H2: a changed upstream image_price without tiers reaches the table, and
// the built-in tier ratios follow it.
func TestSyncImagePriceChangeWithoutTiers(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	f := newFake()
	f.set("/v1/image-generation-models", `{"models":[{"id":"grok-imagine-image-quality","image_price":500000000}]}`)
	syncOnce(t, db, f, false) // built-in rows become source=sync
	res, _ := syncOnce(t, db, f, false)
	if strings.Contains(strings.Join(res.Updated, ","), "grok-imagine-image-quality") {
		t.Fatalf("unchanged price reported as updated: %v", res.Updated)
	}
	// $0.05 -> $0.10: the base doubles, and so do the 1.5k / 2k tiers.
	f.set("/v1/image-generation-models", `{"models":[{"id":"grok-imagine-image-quality","image_price":1000000000}]}`)
	res, tbl := syncOnce(t, db, f, false)
	if !strings.Contains(strings.Join(res.Updated, ","), "grok-imagine-image-quality") {
		t.Fatalf("price change not reported: %+v", res)
	}
	m := tbl.Lookup("grok-imagine-image-quality")
	if m.ImageTicks != 1_000_000_000 || len(m.ImageTiers) != 3 || m.ImageTiers[0].Ticks != 1_000_000_000 ||
		m.ImageTiers[1].Ticks != 1_200_000_000 || m.ImageTiers[2].Ticks != 1_400_000_000 {
		t.Fatalf("image prices: %+v", m)
	}
	if c, _ := tbl.Cost("grok-imagine-image-quality", Usage{Found: true, Images: 1}, Request{}); c != 1_000_000_000 {
		t.Fatalf("cost %d", c)
	}
	// The built-in table itself is not changed by scaling.
	if b := builtinModel("grok-imagine-image-quality"); b.ImageTiers[2].Ticks != 700_000_000 {
		t.Fatalf("built-in tiers mutated: %+v", b.ImageTiers)
	}
}

// M1: models upstream drops are disabled and reported (manual ones are
// kept), come back when listed again, and lose aliases upstream moved.
func TestSyncRetiresMissingModels(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	for _, m := range []*store.Model{
		{ID: "old-lang", Kind: "language", Enabled: true, Source: "sync", InputTicks: 1, Aliases: []string{"moved", "kept"}},
		{ID: "manual-lang", Kind: "language", Enabled: true, Source: "manual", InputTicks: 1, Aliases: []string{"moved"}},
		{ID: "off-lang", Kind: "language", Enabled: false, Source: "sync", InputTicks: 1},
		{ID: "zzz-stale", Kind: "language", Enabled: true, Source: "builtin", InputTicks: 1, Aliases: []string{"code"}},
	} {
		if err := db.UpsertModel(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	f := newFake()
	f.set("/v1/language-models", `{"models":[{"id":"new-lang","aliases":["moved","code"],"prompt_text_token_price":2,"completion_text_token_price":2}]}`)
	f.set("/v1/image-generation-models", `{"models":[]}`) // an empty list retires nothing
	res, tbl := syncOnce(t, db, f, false)
	if got := strings.Join(res.Retired, ","); got != "old-lang,zzz-stale" {
		t.Fatalf("retired %s", got)
	}
	old, _ := db.GetModel(ctx, "old-lang")
	if old.Enabled || strings.Join(old.Aliases, ",") != "kept" || old.InputTicks != 1 {
		t.Fatalf("old-lang: %+v", old)
	}
	if m, _ := db.GetModel(ctx, "manual-lang"); !m.Enabled || len(m.Aliases) != 1 {
		t.Fatalf("manual model touched: %+v", m)
	}
	if m, _ := db.GetModel(ctx, "grok-imagine-image"); m != nil {
		t.Fatalf("unexpected row %+v", m)
	}
	// The alias resolves to the model upstream assigns it to, although the
	// stale row sorts later.
	if m := tbl.Lookup("code"); m == nil || m.ID != "new-lang" {
		t.Fatalf("alias code -> %+v", m)
	}
	// Retired once: a second sync reports nothing new.
	if res, _ = syncOnce(t, db, f, false); len(res.Retired) != 0 {
		t.Fatalf("retired again: %v", res.Retired)
	}
	// Listed again: re-enabled, but a model the admin disabled stays off.
	f.set("/v1/language-models", `{"models":[{"id":"old-lang","prompt_text_token_price":1},{"id":"off-lang","prompt_text_token_price":1}]}`)
	res, _ = syncOnce(t, db, f, false)
	if m, _ := db.GetModel(ctx, "old-lang"); !m.Enabled || !strings.Contains(strings.Join(res.Updated, ","), "old-lang") {
		t.Fatalf("old-lang not restored: %+v %+v", m, res)
	}
	if m, _ := db.GetModel(ctx, "off-lang"); m.Enabled {
		t.Fatal("admin-disabled model enabled by sync")
	}
	if raw, _ := db.GetMeta(ctx, MetaRetiredModels); strings.Contains(raw, "old-lang") || !strings.Contains(raw, "zzz-stale") {
		t.Fatalf("retired set %s", raw)
	}
}

// L6: an id in two lists is synced once, with the first list's kind.
func TestSyncDuplicateIDs(t *testing.T) {
	db := openDB(t)
	f := newFake()
	f.set("/v1/language-models", `{"models":[{"id":"dup","prompt_text_token_price":1}]}`)
	f.set("/v1/embedding-models", `{"models":[{"id":"dup","prompt_text_token_price":9}]}`)
	res, tbl := syncOnce(t, db, f, false)
	if strings.Count(strings.Join(res.Added, ","), "dup") != 1 || tbl.Lookup("dup").Kind != "language" {
		t.Fatalf("dup: %+v", res)
	}
}

// M5: a built-in model the admin deleted is not seeded again.
func TestSeedBuiltinRemembersDeletions(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteModel(ctx, "grok-4.6"); err != nil {
		t.Fatal(err)
	}
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetModel(ctx, "grok-4.6"); err == nil {
		t.Fatal("deleted built-in model came back")
	}
	all, _ := db.ListModels(ctx, "")
	if len(all) != len(BuiltinModels())-1 {
		t.Fatalf("%d models", len(all))
	}
	// A database seeded by an older version (no record yet) gets the
	// missing built-ins once, then remembers them.
	if err := db.SetMeta(ctx, MetaBuiltinSeen, ""); err != nil {
		t.Fatal(err)
	}
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	if m, err := db.GetModel(ctx, "grok-4.6"); err != nil || !m.Enabled {
		t.Fatalf("upgrade seed: %+v %v", m, err)
	}
}

// L4: corrected built-in video prices reach existing non-manual rows.
func TestSeedBuiltinRefreshesVideoPrices(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"grok-imagine-video", "grok-imagine-video-1.5"} {
		m, _ := db.GetModel(ctx, id)
		m.VideoTicksPerSec, m.VideoTiers, m.InputImageTicks = 1, []store.ImageTier{{Resolution: "480p", Ticks: 1}}, 1
		if id == "grok-imagine-video-1.5" {
			m.Source = "manual"
		}
		if err := db.UpsertModel(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if err := SeedBuiltin(ctx, db); err != nil {
		t.Fatal(err)
	}
	b := builtinModel("grok-imagine-video")
	if m, _ := db.GetModel(ctx, "grok-imagine-video"); m.VideoTicksPerSec != b.VideoTicksPerSec ||
		!sameTiers(m.VideoTiers, b.VideoTiers) || m.InputImageTicks != b.InputImageTicks {
		t.Fatalf("video prices not refreshed: %+v", m)
	}
	if m, _ := db.GetModel(ctx, "grok-imagine-video-1.5"); m.VideoTicksPerSec != 1 {
		t.Fatalf("manual video prices overwritten: %+v", m)
	}
}

// L1: aliases from docs.x.ai resolve before the first sync.
func TestBuiltinAliases(t *testing.T) {
	tbl := NewTable()
	tbl.Set(BuiltinModels())
	for alias, id := range map[string]string{
		"grok-4.20-beta":                      "grok-4.20-0309-reasoning",
		"grok-4.20-reasoning-gv2":             "grok-4.20-0309-reasoning",
		"grok-4.20-experimental-beta-0304":    "grok-4.20-0309-reasoning",
		"grok-imagine-image-quality-20260403": "grok-imagine-image-quality",
	} {
		if m := tbl.Lookup(alias); m == nil || m.ID != id {
			t.Errorf("%s -> %+v", alias, m)
		}
	}
	for _, m := range BuiltinModels() {
		if !HasPrice(m) {
			t.Errorf("built-in %s has no price", m.ID)
		}
	}
}
