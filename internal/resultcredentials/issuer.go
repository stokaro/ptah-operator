// Package resultcredentials issues one immutable delivery credential per
// operation attempt. It never grants Kubernetes API access or permission to SQL.
package resultcredentials

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/stokaro/ptah-operator/api/v1alpha1"
	"github.com/stokaro/ptah-operator/internal/resultauthority"
	"github.com/stokaro/ptah-operator/internal/resultdelivery"
	"github.com/stokaro/ptah-operator/internal/resultdelivery/jobconfig"
)

var ErrCredential = errors.New("result delivery credential is absent, invalid, or bound to another attempt")

type Issuer struct {
	writer      client.Client
	reader      client.Reader
	ca          *x509.Certificate
	signer      crypto.Signer
	roots       *x509.CertPool
	serverTrust []byte
}

// New requires a direct API reader, a dedicated client signer, the complete
// currently trusted client CA pool, and the server CA bundle mounted by runners.
// The trust pool may include a previous client signer during rotation overlap.
// Server trust is pinned per credential; changing it needs an installation
// transition that preserves old runners' trust, not replacement of their Secret.
func New(writer client.Client, reader client.Reader, ca tls.Certificate, clientTrust *x509.CertPool, serverTrust []byte) (*Issuer, error) {
	if writer == nil || reader == nil || len(ca.Certificate) != 1 || clientTrust == nil || len(serverTrust) == 0 || len(serverTrust) > 64<<10 {
		return nil, ErrCredential
	}
	parsed, err := x509.ParseCertificate(ca.Certificate[0])
	if err != nil || !parsed.IsCA || parsed.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, ErrCredential
	}
	signer, ok := ca.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, ErrCredential
	}
	public, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil || !bytes.Equal(public, parsed.RawSubjectPublicKeyInfo) {
		return nil, ErrCredential
	}
	if _, err := parsed.Verify(x509.VerifyOptions{Roots: clientTrust, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, ErrCredential
	}
	if !x509.NewCertPool().AppendCertsFromPEM(serverTrust) {
		return nil, ErrCredential
	}
	return &Issuer{writer: writer, reader: reader, ca: parsed, signer: signer, roots: clientTrust.Clone(), serverTrust: bytes.Clone(serverTrust)}, nil
}

// Credential carries no private key. Success means the exact immutable Secret
// was read back through the direct API reader and authority still held.
type Credential struct {
	Name     string
	UID      types.UID
	NotAfter time.Time
}

func (i *Issuer) Ensure(ctx context.Context, identity resultdelivery.Identity) (Credential, error) {
	authority := resultauthority.Authorizer{Reader: i.reader}
	if err := authority.Check(ctx, identity); err != nil {
		return Credential{}, err
	}
	b := identity.Binding
	job := &batchv1.Job{}
	if err := i.reader.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.JobName}, job); err != nil {
		return Credential{}, err
	}
	projection, err := jobconfig.Read(job, b.UID, b.OperationID)
	if err != nil || job.UID != b.JobUID || projection.Generation != b.Generation || job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds < 1 || *job.Spec.ActiveDeadlineSeconds > 86460 {
		return Credential{}, ErrCredential
	}
	key := client.ObjectKey{Namespace: b.Namespace, Name: projection.SecretName}
	existing := &corev1.Secret{}
	err = i.reader.Get(ctx, key, existing)
	if apierrors.IsNotFound(err) {
		// Cover the supported Job horizon, termination grace, and bounded delivery.
		// An operation may still refuse its own elapsed execution deadline; issuing
		// a reporting credential never changes that deadline or restarts its SQL.
		now := time.Now()
		notAfter := now.Add(time.Duration(*job.Spec.ActiveDeadlineSeconds)*time.Second + 10*time.Minute)
		if notAfter.After(i.ca.NotAfter) || now.Before(i.ca.NotBefore) {
			return Credential{}, ErrCredential
		}
		secret, err := i.issue(identity, projection.SecretName, now, notAfter)
		if err != nil {
			return Credential{}, err
		}
		if err := authority.Check(ctx, identity); err != nil {
			return Credential{}, err
		}
		if err := i.writer.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
			return Credential{}, err
		}
		// Losing a write acknowledgment is safe: the next call reads the winner.
		// Concurrent issuers accept only the same binding, not the same random key.
		if err := i.reader.Get(ctx, key, existing); err != nil {
			return Credential{}, err
		}
	} else if err != nil {
		return Credential{}, err
	}
	credential, err := i.validate(existing, identity)
	if err != nil {
		return Credential{}, err
	}
	if err := authority.Check(ctx, identity); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

func (i *Issuer) issue(identity resultdelivery.Identity, name string, now, notAfter time.Time) (*corev1.Secret, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	serial.Add(serial, big.NewInt(1))
	uri, err := resultdelivery.CertificateURI(identity)
	if err != nil {
		return nil, err
	}
	leaf := &x509.Certificate{SerialNumber: serial, NotBefore: now.Add(-time.Minute), NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{uri}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, i.ca, &key.PublicKey, i.signer)
	if err != nil {
		return nil, err
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	b := identity.Binding
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: b.Namespace, Name: name, Labels: map[string]string{"app.kubernetes.io/managed-by": "ptah-operator", "app.kubernetes.io/component": "result-credential"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: b.Kind, Name: b.Name, UID: b.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}, Immutable: ptr.To(true), Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), "ca.crt": bytes.Clone(i.serverTrust)}}, nil
}

func (i *Issuer) validate(secret *corev1.Secret, identity resultdelivery.Identity) (Credential, error) {
	b := identity.Binding
	if secret.Name != jobconfig.CredentialName(b.UID, b.OperationID, b.JobName) || secret.Namespace != b.Namespace || secret.UID == "" || !secret.DeletionTimestamp.IsZero() || secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretTypeTLS || len(secret.Data) != 3 || len(secret.OwnerReferences) != 1 {
		return Credential{}, ErrCredential
	}
	owner := secret.OwnerReferences[0]
	if owner.APIVersion != api.GroupVersion.String() || owner.Kind != b.Kind || owner.Name != b.Name || owner.UID != b.UID || owner.Controller == nil || !*owner.Controller || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		return Credential{}, ErrCredential
	}
	if secret.Labels["app.kubernetes.io/managed-by"] != "ptah-operator" || secret.Labels["app.kubernetes.io/component"] != "result-credential" || !bytes.Equal(secret.Data["ca.crt"], i.serverTrust) {
		return Credential{}, ErrCredential
	}
	for _, data := range secret.Data {
		if len(data) == 0 || len(data) > 64<<10 {
			return Credential{}, ErrCredential
		}
	}
	certificate, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
	if err != nil || len(certificate.Certificate) != 1 {
		return Credential{}, ErrCredential
	}
	bound, err := resultdelivery.ClientIdentity(certificate)
	if err != nil || bound != identity {
		return Credential{}, ErrCredential
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return Credential{}, ErrCredential
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: i.roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return Credential{}, ErrCredential
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > 24*time.Hour+12*time.Minute || leaf.KeyUsage != x509.KeyUsageDigitalSignature || len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		return Credential{}, ErrCredential
	}
	return Credential{Name: secret.Name, UID: secret.UID, NotAfter: leaf.NotAfter}, nil
}
