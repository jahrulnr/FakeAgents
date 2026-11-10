// Package envelope implements the opaque encrypted payloads and signatures used
// to emulate OpenAI encrypted reasoning content and Anthropic thinking
// signatures.
//
// The format is intentionally self-describing and authenticated:
//
//	fa1.<base64url(nonce || ciphertext || tag)>
//
// It is not wire-compatible with either provider; both treat these values as
// opaque blobs that must be replayed verbatim. FakeAgents can therefore choose
// its own authenticated format while remaining faithful to the client contract.
package envelope

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	prefix       = "fa1."
	signaturePfx = "fas1."
)

// ErrInvalid is returned when a sealed value cannot be authenticated.
var ErrInvalid = errors.New("invalid or tampered encrypted content")

// Sealer encrypts and signs opaque payloads.
type Sealer interface {
	Seal(kind string, payload map[string]any) (string, error)
	Open(kind, token string) (map[string]any, error)
	Sign(data []byte) string
	Verify(data []byte, signature string) bool
}

// AESGCM is the default Sealer implementation.
type AESGCM struct {
	aead cipher.AEAD
	mac  []byte
}

// New builds a Sealer from a base64 encoded key, a raw passphrase, or an empty
// string (which generates an ephemeral random key).
func New(key string) (*AESGCM, error) {
	keyBytes, err := deriveKey(key)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create gcm: %w", err)
	}
	mac := sha256.Sum256(append([]byte("fakeagents-signature:"), keyBytes...))
	return &AESGCM{aead: aead, mac: mac[:]}, nil
}

func deriveKey(key string) ([]byte, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		return buf, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(key); err == nil && (len(decoded) == 16 || len(decoded) == 24 || len(decoded) == 32) {
		return decoded, nil
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(key); err == nil && (len(decoded) == 16 || len(decoded) == 24 || len(decoded) == 32) {
		return decoded, nil
	}
	sum := sha256.Sum256([]byte(key))
	return sum[:], nil
}

// Seal implements Sealer.
func (s *AESGCM) Seal(kind string, payload map[string]any) (string, error) {
	plain, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nil, nonce, plain, []byte(kind))
	out := append(nonce, sealed...)
	return prefix + base64.RawURLEncoding.EncodeToString(out), nil
}

// Open implements Sealer.
func (s *AESGCM) Open(kind, token string) (map[string]any, error) {
	if !strings.HasPrefix(token, prefix) {
		return nil, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, prefix))
	if err != nil {
		return nil, ErrInvalid
	}
	nonceSize := s.aead.NonceSize()
	if len(raw) < nonceSize+s.aead.Overhead() {
		return nil, ErrInvalid
	}
	plain, err := s.aead.Open(nil, raw[:nonceSize], raw[nonceSize:], []byte(kind))
	if err != nil {
		return nil, ErrInvalid
	}
	var payload map[string]any
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, ErrInvalid
	}
	return payload, nil
}

// Sign implements Sealer.
func (s *AESGCM) Sign(data []byte) string {
	mac := hmac.New(sha256.New, s.mac)
	mac.Write(data)
	return signaturePfx + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify implements Sealer.
func (s *AESGCM) Verify(data []byte, signature string) bool {
	if !strings.HasPrefix(signature, signaturePfx) {
		return false
	}
	mac := hmac.New(sha256.New, s.mac)
	mac.Write(data)
	expected := signaturePfx + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

// IsSealed reports whether the value looks like a FakeAgents encrypted blob.
func IsSealed(value string) bool { return strings.HasPrefix(value, prefix) }

// IsSigned reports whether the value looks like a FakeAgents signature.
func IsSigned(value string) bool { return strings.HasPrefix(value, signaturePfx) }
