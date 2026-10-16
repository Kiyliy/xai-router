// Package secret generates and hashes credentials.
package secret

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Random returns n random characters from [a-zA-Z0-9].
func Random(n int) string {
	b := make([]byte, n)
	out := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		// 62 does not divide 256; the bias is negligible for credentials of
		// 40+ characters but reject-sample anyway for uniformity.
		for b[i] >= 248 {
			var c [1]byte
			_, _ = rand.Read(c[:])
			b[i] = c[0]
		}
		out[i] = alphabet[int(b[i])%62]
	}
	return string(out)
}

// NewToken returns prefix + 40 random characters.
func NewToken(prefix string) string { return prefix + Random(40) }

// Hash returns the hex SHA-256 of a token. Tokens are high-entropy, so a fast
// hash is appropriate (unlike passwords).
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Equal compares two strings in constant time.
func Equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Display returns the visible prefix of a token, e.g. "sk-xr-a1b2".
func Display(token string) string {
	if i := strings.LastIndexByte(token, '-'); i >= 0 && len(token) > i+5 {
		return token[:i+5]
	}
	if len(token) > 8 {
		return token[:8]
	}
	return token
}

const pbkdf2Iter = 210_000

// HashPassword returns "pbkdf2-sha256$iter$salt$hash".
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iter, 32)
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// CheckPassword verifies pw against a HashPassword result in constant time.
func CheckPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[2])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}
