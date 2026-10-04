package resultcredentials

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// EnrollmentPolicy fences new credential issuance across replicas. The exact
// ConfigMap contains public digests only and is owned by the installation's
// certificate rotator. Its Reader must bypass informer caches. An old mounted
// signer cannot authorize a new credential after this policy changes.
// Existing canonical credentials are validated separately against overlap trust.
type EnrollmentPolicy struct {
	reader client.Reader
	key    client.ObjectKey
}

func NewEnrollmentPolicy(reader client.Reader, namespace, name string) (*EnrollmentPolicy, error) {
	if reader == nil || len(validation.IsDNS1123Label(namespace)) != 0 || len(validation.IsDNS1123Subdomain(name)) != 0 {
		return nil, errors.New("invalid result enrollment policy reference")
	}
	return &EnrollmentPolicy{reader: reader, key: client.ObjectKey{Namespace: namespace, Name: name}}, nil
}

// EnrollmentData is the public policy contract. clientCA is the selected
// signer's DER certificate; serverTrust is the exact PEM bundle new runners pin.
// Comparing whole certificates, not just key IDs, also binds their validity.
func EnrollmentData(clientCA, serverTrust []byte) map[string]string {
	return map[string]string{
		"version":     "1",
		"clientCA":    fmt.Sprintf("sha256:%x", sha256.Sum256(clientCA)),
		"serverTrust": fmt.Sprintf("sha256:%x", sha256.Sum256(serverTrust)),
	}
}

func (p *EnrollmentPolicy) check(ctx context.Context, ca, trust []byte) error {
	policy := &corev1.ConfigMap{}
	if err := p.reader.Get(ctx, p.key, policy); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if policy.UID == "" || policy.ResourceVersion == "" || policy.DeletionTimestamp != nil ||
		policy.Namespace != p.key.Namespace || policy.Name != p.key.Name || len(policy.BinaryData) != 0 ||
		!maps.Equal(policy.Data, EnrollmentData(ca, trust)) {
		return ErrCredential
	}
	return nil
}

// WithEnrollmentPolicy returns an independent issuer. Its signing material is
// immutable; installing a policy never changes another replica's issuer object.
func (i *Issuer) WithEnrollmentPolicy(policy *EnrollmentPolicy) *Issuer {
	next := *i
	next.enrollment = policy
	return &next
}

// CheckEnrollment reports whether this snapshot may issue new credentials.
// It reads the public policy directly and grants no permission to execute SQL.
func (i *Issuer) CheckEnrollment(ctx context.Context) error {
	if i.enrollment == nil {
		return ctx.Err()
	}
	return i.enrollment.check(ctx, i.ca.Raw, i.serverTrust)
}
