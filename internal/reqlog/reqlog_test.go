package reqlog

import (
	"net/http"
	"strings"
	"testing"
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
