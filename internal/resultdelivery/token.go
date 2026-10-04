package resultdelivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// TokenAudience is deliberately distinct from the Kubernetes API audience.
// Installations must not add it to the API server's accepted audiences.
const TokenAudience = "operator.ptah.run/results"

const identityHeader = "X-Ptah-Result-Identity"

// AuthenticateToken verifies a Pod-bound token against an untrusted identity
// claim. Authorize must still verify the operation and its immutable Pod pin.
type AuthenticateToken func(context.Context, string, Identity) error

// TokenSource reads the current kubelet projection for every HTTP attempt.
// Neither Sender nor the durable store retains tokens between attempts.
type TokenSource func() (string, error)

// ValidToken bounds header memory and refuses whitespace and non-ASCII bytes.
// Cryptographic verification belongs to TokenReview, never to this predicate.
func ValidToken(token string) bool {
	if len(token) == 0 || len(token) > 8<<10 {
		return false
	}
	for i := range len(token) {
		c := token[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func tokenIdentity(request *http.Request) (Identity, string, error) {
	if request.TLS == nil || request.TLS.Version < tls.VersionTLS13 {
		return Identity{}, "", ErrAuthority
	}
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return Identity{}, "", ErrAuthority
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if !ValidToken(token) {
		return Identity{}, "", ErrAuthority
	}
	values = request.Header.Values(identityHeader)
	if len(values) != 1 || len(values[0]) == 0 || len(values[0]) > 4<<10 {
		return Identity{}, "", ErrAuthority
	}
	data, err := base64.RawURLEncoding.Strict().DecodeString(values[0])
	if err != nil {
		return Identity{}, "", ErrAuthority
	}
	var identity Identity
	if err := json.Unmarshal(data, &identity); err != nil || !identity.valid() {
		return Identity{}, "", ErrAuthority
	}
	canonical, _ := json.Marshal(identity)
	if !bytes.Equal(data, canonical) {
		return Identity{}, "", ErrAuthority
	}
	return identity, token, nil
}

func tokenIdentityHeader(identity Identity) (string, error) {
	data, err := json.Marshal(identity)
	if err != nil || !identity.valid() {
		return "", ErrAuthority
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	if len(encoded) > 4<<10 {
		return "", ErrAuthority
	}
	return encoded, nil
}

func (s *Sender) authenticate(request *http.Request) error {
	if s.token == nil {
		return nil
	}
	token, err := s.token()
	if err != nil || !ValidToken(token) {
		return ErrAuthority
	}
	identity, err := tokenIdentityHeader(s.identity)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(identityHeader, identity)
	return nil
}
