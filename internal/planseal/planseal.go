// Package planseal seals the Plan payload to a per-process manager key.
//
// The runner holds the public half only: it seals the plan bytes it read and
// never needs to open anything. The manager holds the key pair and never
// persists the private half, so a restarted manager cannot open a Plan frame
// it sealed before the restart -- it re-plans instead, which is safe because
// Plan is read-only. See docs/site/src/content/docs/use/security.md, "Pod
// logs carry plans".
//
// The primitive is NaCl's anonymous sealed box (X25519, XSalsa20,
// Poly1305 -- golang.org/x/crypto/nacl/box), the same construction libsodium
// calls crypto_box_seal. It needs no sender identity: box.SealAnonymous
// generates its own single-use ephemeral key pair internally and prepends the
// ephemeral public key to the ciphertext, which is exactly the runner's
// position -- it holds no private key and never will.
package planseal

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"

	"golang.org/x/crypto/nacl/box"
)

// PublicKeySize is the raw byte length of an X25519 public key.
const PublicKeySize = 32

// PublicKey is the half of a KeyPair that travels in the Plan Job's
// environment. The zero value decodes and encodes like any other key; it is
// not a sentinel for "no key".
type PublicKey [PublicKeySize]byte

// Encode is the base64 form carried in the Job's environment.
func (p PublicKey) Encode() string {
	return base64.StdEncoding.EncodeToString(p[:])
}

// DecodePublicKey reads the base64 form the Plan Job's environment carries.
// It refuses anything that does not decode to exactly PublicKeySize bytes, so
// a missing or truncated environment variable is refused here rather than
// silently treated as a key that seals to nobody.
func DecodePublicKey(encoded string) (PublicKey, error) {
	if encoded == "" {
		return PublicKey{}, errors.New("plan seal public key is empty")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return PublicKey{}, fmt.Errorf("decode plan seal public key: %w", err)
	}
	if len(raw) != PublicKeySize {
		return PublicKey{}, fmt.Errorf("plan seal public key must be %d bytes, got %d", PublicKeySize, len(raw))
	}
	var key PublicKey
	copy(key[:], raw)
	return key, nil
}

// Seal encrypts message so that only the holder of the matching private key
// can read it back, using a fresh single-use ephemeral key pair the box
// package generates for this call alone. The result is the base64 encoding of
// the sealed box, safe to carry in a JSON string field or a frame written to
// standard output.
func Seal(message []byte, recipient PublicKey) (string, error) {
	pub := [PublicKeySize]byte(recipient)
	sealed, err := box.SealAnonymous(nil, message, &pub, rand.Reader)
	if err != nil {
		return "", fmt.Errorf("seal plan payload: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// KeyPair is a per-process X25519 key pair. Generate creates one; the zero
// value opens nothing (OpenAnonymous refuses a box addressed to the zero
// public key the same as any other key it does not hold).
//
// The private half is never persisted, logged, or sent anywhere. It exists
// only in this process's memory for as long as the process runs.
type KeyPair struct {
	public  PublicKey
	private [PublicKeySize]byte
}

// Generate creates a fresh key pair from a cryptographically secure random
// source. Call it once per manager process at startup; never persist the
// result.
func Generate() (KeyPair, error) {
	pub, priv, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return KeyPair{}, fmt.Errorf("generate plan seal key pair: %w", err)
	}
	return KeyPair{public: PublicKey(*pub), private: *priv}, nil
}

// PublicKey is the half that travels in the Plan Job's environment.
func (k KeyPair) PublicKey() PublicKey { return k.public }

// Open decrypts a payload Seal produced for this pair's public half. A
// payload sealed to a different key, or corrupted in transit, is refused
// without distinguishing which -- either way this process cannot read it.
func (k KeyPair) Open(sealed string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return nil, fmt.Errorf("decode sealed plan payload: %w", err)
	}
	pub := [PublicKeySize]byte(k.public)
	message, ok := box.OpenAnonymous(nil, raw, &pub, &k.private)
	if !ok {
		return nil, errors.New("sealed payload could not be opened with this key pair")
	}
	return message, nil
}
