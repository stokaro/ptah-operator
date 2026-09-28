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
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
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

// Envelope binds a sealed plan to the exact operation and Job it was
// computed for.
//
// A NaCl sealed box carries no associated data: any plaintext sealed to a
// given public key opens the same way regardless of what it is, so opening
// successfully proves only that the manager's own key sealed it, never that
// it was sealed for the harvest now reading it. A validly sealed plan from
// one operation, with a self-consistent content digest, would otherwise open
// and validate as another's. The envelope closes that: it travels inside the
// sealed plaintext, where nothing that lacks the public key can forge it
// undetected, and the harvest path refuses a mismatch instead of trusting
// that the frame it read named the right operation.
//
// JobName rather than a Job UID, because the runner seals before the Job it
// runs in exists as an API object: the UID is assigned on create, but the
// deterministic name is computed from the claim before dispatch and is
// already what the runner is given. It changes on every retry of the same
// operation, so it also tells apart two attempts the OperationID alone
// would not.
type Envelope struct {
	OperationID string `json:"operationID"`
	JobName     string `json:"jobName"`
}

// SealPlan seals plan for recipient, wrapped in an envelope binding it to
// envelope's operation and Job. The wrapping is a compact JSON header,
// followed by one newline the header itself cannot contain, followed by plan
// unchanged: encoding/json never emits a literal newline for a struct of
// plain strings, so the split before decoding it back is unambiguous however
// many newlines the plan itself carries.
func SealPlan(plan []byte, envelope Envelope, recipient PublicKey) (string, error) {
	header, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("marshal seal envelope: %w", err)
	}
	message := make([]byte, 0, len(header)+1+len(plan))
	message = append(message, header...)
	message = append(message, '\n')
	message = append(message, plan...)
	return Seal(message, recipient)
}

// OpenPlan opens a payload SealPlan produced and refuses one whose envelope
// is not exactly want: a plan sealed for a different operation or a
// different attempt of the same one, however validly it opens.
func (k KeyPair) OpenPlan(sealed string, want Envelope) ([]byte, error) {
	message, err := k.Open(sealed)
	if err != nil {
		return nil, err
	}
	newline := bytes.IndexByte(message, '\n')
	if newline < 0 {
		return nil, errors.New("sealed plan payload carries no envelope")
	}
	var got Envelope
	if err := json.Unmarshal(message[:newline], &got); err != nil {
		return nil, fmt.Errorf("decode sealed plan envelope: %w", err)
	}
	if got != want {
		return nil, errors.New("sealed plan envelope does not match the operation being harvested")
	}
	return message[newline+1:], nil
}
