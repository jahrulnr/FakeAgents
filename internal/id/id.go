// Package id generates API-looking identifiers.
package id

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Generator produces opaque identifiers with a provider-style prefix.
type Generator interface {
	// New returns prefix + a random URL-safe identifier.
	New(prefix string) string
	// Raw returns a random identifier without a prefix.
	Raw(length int) string
}

// Crypto is a cryptographically random Generator.
type Crypto struct{}

// New returns a Crypto generator.
func New() *Crypto { return &Crypto{} }

// New implements Generator.
func (*Crypto) New(prefix string) string {
	return prefix + base62(24)
}

// Raw implements Generator.
func (*Crypto) Raw(length int) string {
	if length <= 0 {
		length = 24
	}
	return base62(length)
}

func base62(n int) string {
	buf := make([]byte, n)
	random := make([]byte, n)
	if _, err := rand.Read(random); err != nil {
		panic(fmt.Sprintf("fakeagents: crypto/rand unavailable: %v", err))
	}
	for i, b := range random {
		buf[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(buf)
}

// Fake is a deterministic Generator for tests. It is safe to use from a single
// goroutine only.
type Fake struct {
	Prefix string
	Next   int
}

// NewFake returns a deterministic generator.
func NewFake() *Fake { return &Fake{Prefix: "test"} }

// New implements Generator.
func (f *Fake) New(prefix string) string {
	f.Next++
	return fmt.Sprintf("%s%s%06d", prefix, f.Prefix, f.Next)
}

// Raw implements Generator.
func (f *Fake) Raw(length int) string {
	f.Next++
	value := fmt.Sprintf("%s%06d", f.Prefix, f.Next)
	if len(value) >= length {
		return value[:length]
	}
	return value + strings.Repeat("x", length-len(value))
}

// Decode is a helper that extracts the payload portion of a prefixed id,
// returning the prefix and the remainder.
func Decode(s string) (string, string) {
	i := strings.LastIndex(s, "_")
	if i < 0 || i == len(s)-1 {
		return s, ""
	}
	return s[:i+1], s[i+1:]
}

// EncodeBase64URL is a small helper used by the envelope package.
func EncodeBase64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
