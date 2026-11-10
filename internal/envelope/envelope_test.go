package envelope_test

import (
	"strings"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/envelope"
)

func TestSealOpenRoundTrip(t *testing.T) {
	sealer, err := envelope.New("test-passphrase")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	token, err := sealer.Seal("reasoning", map[string]any{"text": "secret", "n": 1})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !envelope.IsSealed(token) {
		t.Fatalf("sealed token has no prefix: %s", token)
	}
	payload, err := sealer.Open("reasoning", token)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if payload["text"] != "secret" {
		t.Fatalf("payload = %v", payload)
	}
	if _, err := sealer.Open("other-kind", token); err == nil {
		t.Fatal("kind binding must be enforced")
	}
}

func TestTamperedTokenRejected(t *testing.T) {
	sealer, err := envelope.New("key")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	token, err := sealer.Seal("reasoning", map[string]any{"text": "secret"})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	replacement := "A"
	if strings.HasSuffix(token, "A") {
		replacement = "B"
	}
	tampered := token[:len(token)-1] + replacement
	if _, err := sealer.Open("reasoning", tampered); err == nil {
		t.Fatal("tampered token must be rejected")
	}
}

func TestSignVerify(t *testing.T) {
	sealer, err := envelope.New("key")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	signature := sealer.Sign([]byte("payload"))
	if !envelope.IsSigned(signature) {
		t.Fatalf("signature prefix missing: %s", signature)
	}
	if !sealer.Verify([]byte("payload"), signature) {
		t.Fatal("valid signature rejected")
	}
	if sealer.Verify([]byte("other"), signature) {
		t.Fatal("signature accepted for different payload")
	}
}

func TestEphemeralKeyIsUsable(t *testing.T) {
	sealer, err := envelope.New("")
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	token, err := sealer.Seal("x", map[string]any{"ok": true})
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := sealer.Open("x", token); err != nil {
		t.Fatalf("open: %v", err)
	}
}
