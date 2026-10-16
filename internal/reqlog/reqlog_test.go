package reqlog

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/Kiyliy/xai-router/internal/config"
	"github.com/Kiyliy/xai-router/internal/store"
)

func TestSanitizeBase64(t *testing.T) {
	b64 := strings.Repeat("QUJD", 600) // 2400 chars
	body := `{"image":"` + b64 + `","url":"data:image/png;base64,` + b64[:400] + `","short":"QUJD"}`
	out, changed := SanitizeBody([]byte(body), 0)
	if !changed || strings.Contains(out, b64[:300]) {
		t.Fatalf("base64 not removed: %s", out)
	}
	if !strings.Contains(out, `"image":"<base64 2400 bytes omitted>"`) ||
		!strings.Contains(out, `data:image/png;base64,<base64 400 bytes omitted>"`) ||
		!strings.Contains(out, `"short":"QUJD"`) {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestSanitizeKeepsNormalText(t *testing.T) {
	text := `{"content":"` + strings.Repeat("hello world ", 200) + `"}`
	out, changed := SanitizeBody([]byte(text), 0)
	if changed || out != text {
		t.Fatal("normal text must be untouched")
	}
}

func TestSanitizeTruncate(t *testing.T) {
	out, changed := SanitizeBody([]byte(strings.Repeat("é", 100)), 11)
	if !changed || !strings.HasPrefix(out, "ééééé\n…[truncated, 200 bytes total]") {
		t.Fatalf("got %q", out)
	}
	if out, _ := SanitizeBody([]byte{0xff, 0xfe, 0x00}, 100); !strings.HasPrefix(out, "<binary body") {
		t.Fatalf("binary: %q", out)
	}
}

func TestHeadersJSONMasksSecrets(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer sk-xr-abcdefghijklmnopqrstuvwxyz")
	h.Set("X-Api-Key", "xai-fake-fake-fake-fake")
	h.Set("Cookie", "session=supersecretvalue123")
	h.Set("Content-Type", "application/json")
	out := HeadersJSON(h)
	for _, leak := range []string{"ghijklmnopqrstu", "fake-fake-fake", "supersecretvalue"} {
		if strings.Contains(out, leak) {
			t.Fatalf("secret leaked: %s", out)
		}
	}
	if !strings.Contains(out, `"Authorization":"Bearer sk-xr-…wxyz"`) || !strings.Contains(out, "application/json") {
		t.Fatalf("unexpected: %s", out)
	}
	if MaskSecret("short") != "****" {
		t.Fatal("short secrets must be fully masked")
	}
}

func TestWriterKeepsAccountingWhenQueueFull(t *testing.T) {
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	tok := &store.Token{Name: "t", Hash: "h", Prefix: "p", Enabled: true}
	if err := db.CreateToken(ctx, tok); err != nil {
		t.Fatal(err)
	}
	w := NewWriter(db, config.NewHolder(config.DefaultSettings()), slog.New(slog.NewTextHandler(io.Discard, nil)), 2)
	for i := 0; i < 50; i++ { // the channel holds 2; the rest overflow
		w.Enqueue(&store.Log{RequestID: "r", CreatedAt: store.NowMs(), Method: "POST", Path: "/v1/chat/completions",
			Status: 200, TokenID: tok.ID, CostTicks: 10, HasBodies: true, ReqBody: strings.Repeat("x", 100)})
	}
	if w.Dropped() == 0 {
		t.Fatal("expected bodies to be discarded under pressure")
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetToken(ctx, tok.ID)
	if got.UsedTicks != 500 || got.Requests != 50 {
		t.Fatalf("accounting lost: used %d requests %d", got.UsedTicks, got.Requests)
	}
	_, n, _ := db.ListLogs(ctx, store.LogFilter{Limit: 100})
	if n != 50 {
		t.Fatalf("records lost: %d", n)
	}
}
