package billing

import (
	"testing"

	"github.com/Kiyliy/xai-router/internal/store"
)

// A long-context price of 0 means "not set": the base price applies instead
// of the request being free (code review v0.5.6, C-M3).
func TestLongContextZeroPriceFallsBack(t *testing.T) {
	tb := NewTable()
	tb.Set([]*store.Model{{ID: "m", Kind: "language", Enabled: true, InputTicks: 10, CachedTicks: 2, OutputTicks: 30,
		LongThreshold: 100, LongInputTicks: 20, LongCachedTicks: 0, LongOutputTicks: 0}})
	u := Usage{Found: true, PromptTokens: 200, CachedTokens: 50, CompletionTokens: 10}
	cost, _ := tb.Cost("m", u, Request{})
	// 150 uncached at the long input price 20, 50 cached at the base 2, 10 out at the base 30.
	if want := int64(150*20 + 50*2 + 10*30); cost != want {
		t.Fatalf("cost %d, want %d", cost, want)
	}
	if snap := tb.PriceSnapshot("m", u, Request{}); snap == "" || !contains(snap, `"cached_input":2`) || !contains(snap, `"output":30`) {
		t.Fatalf("snapshot %s", snap)
	}
}

// A disabled (retired) model's id must not capture a name that upstream now
// gives to an enabled model as an alias (code review v0.5.6, C follow-up).
func TestDisabledIDDoesNotShadowEnabledAlias(t *testing.T) {
	tb := NewTable()
	tb.Set([]*store.Model{
		{ID: "grok-code-fast-1", Kind: "language", Enabled: false},
		{ID: "grok-build-0.1", Kind: "language", Enabled: true, Aliases: []string{"grok-code-fast-1"}},
	})
	if m := tb.Lookup("grok-code-fast-1"); m == nil || m.ID != "grok-build-0.1" {
		t.Fatalf("lookup %+v", m)
	}
	// An enabled model's own id still beats another enabled model's alias.
	tb.Set([]*store.Model{
		{ID: "a", Kind: "language", Enabled: true},
		{ID: "b", Kind: "language", Enabled: true, Aliases: []string{"a"}},
	})
	if m := tb.Lookup("a"); m == nil || m.ID != "a" {
		t.Fatalf("lookup a %+v", m)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
