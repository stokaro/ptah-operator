package planseal_test

import (
	"strings"
	"testing"

	"github.com/stokaro/ptah-operator/internal/planseal"
)

func TestSealOpenRoundTrip(t *testing.T) {
	t.Parallel()
	key, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	message := []byte("declared row value: alice@example.com")
	sealed, err := planseal.Seal(message, key.PublicKey())
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if strings.Contains(sealed, "alice@example.com") {
		t.Fatalf("sealed payload leaks plaintext: %s", sealed)
	}
	opened, err := key.Open(sealed)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if string(opened) != string(message) {
		t.Fatalf("Open() = %q, want %q", opened, message)
	}
}

func TestOpenRefusesTheWrongKey(t *testing.T) {
	t.Parallel()
	sealedTo, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	other, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	sealed, err := planseal.Seal([]byte("plan bytes"), sealedTo.PublicKey())
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("Open() with the wrong key pair succeeded")
	}
}

func TestOpenRefusesTamperedCiphertext(t *testing.T) {
	t.Parallel()
	key, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	sealed, err := planseal.Seal([]byte("plan bytes"), key.PublicKey())
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	tampered := []byte(sealed)
	// Flip a bit deep in the ciphertext, past the prepended ephemeral public
	// key, so the change lands inside the authenticated payload rather than
	// in the sender's own key material.
	index := len(tampered) - 5
	if tampered[index] == 'A' {
		tampered[index] = 'B'
	} else {
		tampered[index] = 'A'
	}
	if _, err := key.Open(string(tampered)); err == nil {
		t.Fatal("Open() accepted a tampered payload")
	}
}

func TestOpenRefusesMalformedBase64(t *testing.T) {
	t.Parallel()
	key, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if _, err := key.Open("not base64!!"); err == nil {
		t.Fatal("Open() accepted malformed base64")
	}
}

func TestPublicKeyEncodeDecodeRoundTrip(t *testing.T) {
	t.Parallel()
	key, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	encoded := key.PublicKey().Encode()
	decoded, err := planseal.DecodePublicKey(encoded)
	if err != nil {
		t.Fatalf("DecodePublicKey() error = %v", err)
	}
	if decoded != key.PublicKey() {
		t.Fatalf("DecodePublicKey() = %v, want %v", decoded, key.PublicKey())
	}
}

func TestDecodePublicKeyRefusesEmpty(t *testing.T) {
	t.Parallel()
	if _, err := planseal.DecodePublicKey(""); err == nil {
		t.Fatal("DecodePublicKey(\"\") succeeded")
	}
}

func TestDecodePublicKeyRefusesWrongLength(t *testing.T) {
	t.Parallel()
	// 16 raw bytes, base64-encoded: valid base64, wrong length.
	if _, err := planseal.DecodePublicKey("AAAAAAAAAAAAAAAAAAAAAA=="); err == nil {
		t.Fatal("DecodePublicKey() accepted a short key")
	}
}

func TestDecodePublicKeyRefusesMalformedBase64(t *testing.T) {
	t.Parallel()
	if _, err := planseal.DecodePublicKey("not base64!!"); err == nil {
		t.Fatal("DecodePublicKey() accepted malformed base64")
	}
}

func TestGenerateProducesDistinctKeyPairs(t *testing.T) {
	t.Parallel()
	first, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	second, err := planseal.Generate()
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if first.PublicKey() == second.PublicKey() {
		t.Fatal("Generate() produced the same public key twice")
	}
}
