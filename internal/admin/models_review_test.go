package admin

import (
	"net/http"
	"testing"
)

// Switching a synced model on or off keeps it synced; editing what sync
// manages makes it manual (code review v0.5.6, C-M2).
func TestModelToggleKeepsSyncSource(t *testing.T) {
	h := newHarness(t)
	const id = "grok-4.3" // a built-in model, not manual
	get := func() map[string]any {
		for _, m := range list(h.must(h.call(http.MethodGet, "/api/v1/models?page_size=500", ""), 200)) {
			if m["id"] == id {
				return m
			}
		}
		t.Fatalf("%s not listed", id)
		return nil
	}
	if src := get()["source"]; src == "manual" {
		t.Fatalf("built-in model starts manual: %v", src)
	}
	h.must(h.call(http.MethodPatch, "/api/v1/models/"+id, `{"enabled":false}`), 200)
	h.must(h.call(http.MethodPatch, "/api/v1/models/"+id, `{"enabled":true}`), 200)
	if m := get(); m["source"] == "manual" || m["enabled"] != true {
		t.Fatalf("toggled model: %v", m)
	}
	if m := get(); m["needs_price"] != false {
		t.Fatalf("priced built-in flagged as needing a price: %v", m["needs_price"])
	}
	h.must(h.call(http.MethodPatch, "/api/v1/models/"+id, `{"input_price":1.5}`), 200)
	if src := get()["source"]; src != "manual" {
		t.Fatalf("price edit did not make it manual: %v", src)
	}
}
