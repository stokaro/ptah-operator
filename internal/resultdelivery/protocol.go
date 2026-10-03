// Package resultdelivery implements authenticated, bounded delivery of an
// already executed operation. It never starts a SQL executor.
package resultdelivery

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stokaro/ptah-operator/internal/dataplane"
	"github.com/stokaro/ptah-operator/internal/plancontract"
	"github.com/stokaro/ptah-operator/internal/resultstore"
	"github.com/stokaro/ptah-operator/internal/runner"
)

const (
	PathPrefix   = "/v1/results/"
	DigestHeader = "X-Ptah-Result-Digest"
	ContentType  = "application/vnd.ptah.result.v1+json"
)

var (
	ErrAuthority = errors.New("result delivery authority is invalid or expired")
	ErrPayload   = errors.New("result delivery payload is invalid")
)

// Identity is carried in the client certificate's sole URI SAN. Engine is the
// target engine fixed by the claim; it is empty only for operations that have
// no database target. The issuer and live authorizer must derive it from the
// claim, never from an upload body or an unverified certificate request.
type Identity struct {
	Binding resultstore.Binding `json:"binding"`
	Engine  string              `json:"engine"`
}

func (i Identity) valid() bool {
	if _, err := resultstore.Name(i.Binding); err != nil {
		return false
	}
	switch i.Engine {
	case "postgresql", "mysql":
		return true
	case "":
		return i.Binding.Operation == "resolve" || i.Binding.Operation == "verify"
	default:
		return false
	}
}

// CertificateURI constructs a SAN for an identity the certificate issuer has
// already authorized. It neither issues a certificate nor verifies authority.
func CertificateURI(identity Identity) (*url.URL, error) {
	if !identity.valid() {
		return nil, ErrAuthority
	}
	b, err := json.Marshal(identity)
	if err != nil {
		return nil, ErrAuthority
	}
	return &url.URL{Scheme: "ptah-result", Host: "operator.ptah.run", Path: "/v1/" + base64.RawURLEncoding.EncodeToString(b)}, nil
}

// ClientIdentity reads the binding from a locally provisioned client
// certificate. It refuses an expired or non-client leaf, but does not authenticate
// its issuer; the receiver's TLS verification establishes that trust.
func ClientIdentity(certificate tls.Certificate) (Identity, error) {
	identity, err := StoredClientIdentity(certificate)
	if err != nil {
		return Identity{}, err
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	now := time.Now()
	if err != nil || now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return Identity{}, ErrAuthority
	}
	return identity, nil
}

// StoredClientIdentity decodes an immutable credential's recorded binding for
// cleanup after expiry or CA retirement. It authenticates neither the issuer
// nor current execution authority and must never authorize delivery. The caller
// must establish the stored object's provenance and check live retention pins.
func StoredClientIdentity(certificate tls.Certificate) (Identity, error) {
	if len(certificate.Certificate) == 0 || certificate.PrivateKey == nil {
		return Identity{}, ErrAuthority
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || leaf.IsCA || len(leaf.URIs) != 1 {
		return Identity{}, ErrAuthority
	}
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageClientAuth {
			return certificateIdentity(leaf.URIs[0])
		}
	}
	return Identity{}, ErrAuthority
}

func certificateIdentity(uri *url.URL) (Identity, error) {
	if uri == nil || len(uri.String()) > 8192 || uri.Scheme != "ptah-result" || uri.Host != "operator.ptah.run" ||
		uri.User != nil || uri.RawQuery != "" || uri.Fragment != "" || !strings.HasPrefix(uri.Path, "/v1/") {
		return Identity{}, ErrAuthority
	}
	encoded := strings.TrimPrefix(uri.Path, "/v1/")
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Identity{}, ErrAuthority
	}
	var identity Identity
	if err := json.Unmarshal(raw, &identity); err != nil {
		return Identity{}, ErrAuthority
	}
	canonical, err := CertificateURI(identity)
	if err != nil || canonical.String() != uri.String() {
		return Identity{}, ErrAuthority
	}
	return identity, nil
}

func authenticatedIdentity(state *tls.ConnectionState, now time.Time) (Identity, error) {
	if state == nil || state.Version < tls.VersionTLS13 || len(state.PeerCertificates) == 0 || len(state.VerifiedChains) == 0 {
		return Identity{}, ErrAuthority
	}
	leaf := state.PeerCertificates[0]
	if len(leaf.URIs) != 1 {
		return Identity{}, ErrAuthority
	}
	valid := false
	for _, chain := range state.VerifiedChains {
		if len(chain) == 0 || !bytes.Equal(chain[0].Raw, leaf.Raw) {
			continue
		}
		current := true
		for _, cert := range chain {
			if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
				current = false
			}
		}
		valid = valid || current
	}
	if !valid {
		return Identity{}, ErrAuthority
	}
	return certificateIdentity(leaf.URIs[0])
}

func payloadDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Encode encodes the durable wire document. For a changed Plan, Stdout must
// contain the exact plaintext plan, not a process-sealed legacy log payload.
// These bytes are only for authenticated TLS delivery and confidential result-record storage.
func Encode(identity Identity, result runner.Result) ([]byte, error) {
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, ErrPayload
	}
	if _, err := Decode(identity, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// Decode refuses noncanonical documents (including duplicate or unknown JSON
// fields), foreign operations, and plans that cannot survive process-key loss.
// The controller must still check policy, target fingerprints, and provenance.
func Decode(identity Identity, payload []byte) (runner.Result, error) {
	if !identity.valid() || len(payload) == 0 || int64(len(payload)) > resultstore.MaxPayloadBytes || !utf8.Valid(payload) {
		return runner.Result{}, ErrPayload
	}
	var result runner.Result
	if err := json.Unmarshal(payload, &result); err != nil {
		return runner.Result{}, ErrPayload
	}
	canonical, err := json.Marshal(result)
	if err != nil || !bytes.Equal(canonical, payload) {
		return runner.Result{}, ErrPayload
	}
	if err := runner.ValidateResultFor(result, runner.Operation(identity.Binding.Operation), identity.Binding.OperationID); err != nil {
		return runner.Result{}, ErrPayload
	}
	if result.Operation == runner.OperationPlan && result.Stdout != "" {
		plan := []byte(result.Stdout)
		if int64(len(plan)) > plancontract.MaxExecutableBytes || payloadDigest(plan) != result.PlanContentDigest {
			return runner.Result{}, ErrPayload
		}
		if _, err := dataplane.DecodePlan(plan, identity.Engine); err != nil {
			return runner.Result{}, ErrPayload
		}
	}
	return result, nil
}
