package secret

import (
	"strings"
	"testing"
)

func TestPassword(t *testing.T) {
	h := HashPassword("correct horse")
	if !strings.HasPrefix(h, "pbkdf2-sha256$") || !CheckPassword("correct horse", h) {
		t.Fatal("password round trip failed")
	}
	if CheckPassword("wrong", h) || CheckPassword("correct horse", "garbage") {
		t.Fatal("wrong password accepted")
	}
	if a, b := HashPassword("x"), HashPassword("x"); a == b {
		t.Fatal("salt not random")
	}
}

func TestTokens(t *testing.T) {
	tok := NewToken("sk-xr-")
	if len(tok) != 46 || !strings.HasPrefix(tok, "sk-xr-") {
		t.Fatalf("token %q", tok)
	}
	if Display(tok) != tok[:10] {
		t.Fatalf("display %q", Display(tok))
	}
	if Hash(tok) == Hash(tok+"x") || len(Hash(tok)) != 64 {
		t.Fatal("hash")
	}
	if !Equal("a", "a") || Equal("a", "b") {
		t.Fatal("equal")
	}
}
