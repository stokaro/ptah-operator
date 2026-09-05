package certrotation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	StagingSecretLabel      = "operator.ptah.dev/certificate-rotation-staging"
	StagingSecretLabelValue = "true"

	stagingFormat = "v1"

	stagingFormatKey           = "format"
	stagingSourceStateKey      = "source-state"
	stagingSourceUIDKey        = "source-secret.uid"
	stagingSourceDigestKey     = "source-secret.sha256"
	stagingCACertificateKey    = "candidate.ca.crt"
	stagingCAPrivateKeyKey     = "candidate.ca.key"
	stagingServingCertKey      = "candidate.tls.crt"
	stagingServingKeyKey       = "candidate.tls.key"
	stagingCandidateCertKey    = "listener.tls.crt"
	stagingCandidateKeyKey     = "listener.tls.key"
	stagingSourceStatePresent  = "present"
	stagingSourceStateMissing  = "missing"
	stagingSourceDigestHexSize = sha256.Size * 2
)

// CandidateCertificateSink receives the listener-only certificate associated
// with a durable pending CA transition. Implementations must replace the
// currently served certificate atomically and must not retain the input byte
// slices.
type CandidateCertificateSink interface {
	StoreCandidateCertificate(certificatePEM, privateKeyPEM []byte) error
	ClearCandidateCertificate()
}

type stagingSourceState string

const (
	stagingSourcePresent stagingSourceState = stagingSourceStatePresent
	stagingSourceMissing stagingSourceState = stagingSourceStateMissing
)

type pendingCandidate struct {
	sourceState     stagingSourceState
	sourceUID       types.UID
	sourceDigest    string
	material        certificateMaterial
	listenerCertPEM []byte
	listenerKeyPEM  []byte
	listenerLeaf    *x509.Certificate
}

func (r *Rotator) readStagingSecret(ctx context.Context) (*corev1.Secret, *pendingCandidate, error) {
	secret, err := r.client.CoreV1().Secrets(r.config.Namespace).Get(
		ctx,
		r.config.StagingSecretName,
		metav1.GetOptions{},
	)
	if err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return nil, nil, fmt.Errorf("get certificate rotation staging Secret %q: %w", r.config.StagingSecretName, err)
	}
	if err := validateStagingSecretMetadata(secret, r.config); err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return nil, nil, fmt.Errorf("certificate rotation staging Secret %q contract: %w", r.config.StagingSecretName, err)
	}
	if len(secret.Data) == 0 {
		r.candidateSink.ClearCandidateCertificate()
		return secret, nil, nil
	}
	pending, err := decodePendingCandidate(secret.Data, r.config, r.now())
	if err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return nil, nil, fmt.Errorf("certificate rotation staging Secret %q pending material: %w", r.config.StagingSecretName, err)
	}
	return secret, pending, nil
}

func validateStagingSecretMetadata(secret *corev1.Secret, config Config) error {
	if secret == nil {
		return errors.New("staging Secret is nil")
	}
	if secret.Name != config.StagingSecretName || secret.Namespace != config.Namespace || secret.GenerateName != "" {
		return errors.New("name, namespace, or generateName differs from the configured identity")
	}
	if secret.UID == "" || secret.ResourceVersion == "" || secret.DeletionTimestamp != nil {
		return errors.New("live UID and resourceVersion are required and deletion must not be in progress")
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return fmt.Errorf("type is %q, want %q", secret.Type, corev1.SecretTypeOpaque)
	}
	if len(secret.Labels) != 1 || secret.Labels[StagingSecretLabel] != StagingSecretLabelValue {
		return errors.New("labels are not the exact managed staging identity")
	}
	if len(secret.Annotations) != 0 || len(secret.OwnerReferences) != 0 || len(secret.Finalizers) != 0 ||
		secret.Immutable != nil || len(secret.StringData) != 0 {
		return errors.New("annotations, owner references, finalizers, immutable, and stringData must be absent")
	}
	return nil
}

func validatePrimarySecretSource(secret *corev1.Secret, config Config) error {
	if secret == nil {
		return errors.New("generated TLS Secret is nil")
	}
	if secret.Name != config.SecretName || secret.Namespace != config.Namespace || secret.GenerateName != "" ||
		secret.UID == "" || secret.ResourceVersion == "" || secret.DeletionTimestamp != nil {
		return errors.New("name, namespace, generateName, live UID, resourceVersion, or deletion state differs from the generated Secret identity")
	}
	if secret.Type != corev1.SecretTypeTLS && secret.Type != corev1.SecretTypeOpaque && secret.Type != "" {
		return fmt.Errorf("type %q cannot be normalized as a generated TLS Secret", secret.Type)
	}
	if len(secret.Labels) != 1 || secret.Labels[GeneratedSecretLabel] != GeneratedSecretLabelValue {
		return errors.New("labels are not the exact generated certificate identity")
	}
	if len(secret.Annotations) != 0 || len(secret.OwnerReferences) != 0 || len(secret.Finalizers) != 0 ||
		secret.Immutable != nil || len(secret.StringData) != 0 {
		return errors.New("annotations, owner references, finalizers, immutable, and stringData must be absent")
	}
	allowedData := map[string]struct{}{
		CACertificateKey:        {},
		CAPrivateKeyKey:         {},
		corev1.TLSCertKey:       {},
		corev1.TLSPrivateKeyKey: {},
	}
	for key := range secret.Data {
		if _, allowed := allowedData[key]; !allowed {
			return fmt.Errorf("unmanaged data field %q is present", key)
		}
	}
	return nil
}

func generatePendingCandidate(
	reader io.Reader,
	now time.Time,
	config Config,
	source *corev1.Secret,
) (*pendingCandidate, error) {
	material, err := generateMaterial(reader, now, config)
	if err != nil {
		return nil, err
	}
	listenerMaterial, err := generateServingMaterialForService(
		reader,
		now,
		config.ServingCertificateValidity,
		config.CandidateServiceName,
		config.Namespace,
		material,
	)
	if err != nil {
		return nil, fmt.Errorf("generate candidate-listener certificate: %w", err)
	}
	pending := &pendingCandidate{
		material:        material,
		listenerCertPEM: append([]byte(nil), listenerMaterial.certPEM...),
		listenerKeyPEM:  append([]byte(nil), listenerMaterial.keyPEM...),
		listenerLeaf:    listenerMaterial.leaf,
	}
	if source == nil {
		pending.sourceState = stagingSourceMissing
		return pending, nil
	}
	if source.UID == "" {
		return nil, errors.New("source TLS Secret has no live UID")
	}
	pending.sourceState = stagingSourcePresent
	pending.sourceUID = source.UID
	pending.sourceDigest = secretMaterialDigest(source)
	return pending, nil
}

func decodePendingCandidate(data map[string][]byte, config Config, now time.Time) (*pendingCandidate, error) {
	if len(data) != 10 {
		return nil, fmt.Errorf("data has %d fields, want exactly 10", len(data))
	}
	for _, key := range []string{
		stagingFormatKey,
		stagingSourceStateKey,
		stagingSourceUIDKey,
		stagingSourceDigestKey,
		stagingCACertificateKey,
		stagingCAPrivateKeyKey,
		stagingServingCertKey,
		stagingServingKeyKey,
		stagingCandidateCertKey,
		stagingCandidateKeyKey,
	} {
		if _, found := data[key]; !found {
			return nil, fmt.Errorf("required data field %q is missing", key)
		}
	}
	if string(data[stagingFormatKey]) != stagingFormat {
		return nil, fmt.Errorf("format is %q, want %q", data[stagingFormatKey], stagingFormat)
	}

	pending := &pendingCandidate{
		sourceState:     stagingSourceState(data[stagingSourceStateKey]),
		sourceUID:       types.UID(data[stagingSourceUIDKey]),
		sourceDigest:    string(data[stagingSourceDigestKey]),
		listenerCertPEM: append([]byte(nil), data[stagingCandidateCertKey]...),
		listenerKeyPEM:  append([]byte(nil), data[stagingCandidateKeyKey]...),
	}
	switch pending.sourceState {
	case stagingSourcePresent:
		if pending.sourceUID == "" {
			return nil, errors.New("present source state requires a Secret UID")
		}
		if len(pending.sourceDigest) != stagingSourceDigestHexSize {
			return nil, errors.New("present source state requires a canonical SHA-256 digest")
		}
		decoded, err := hex.DecodeString(pending.sourceDigest)
		if err != nil || hex.EncodeToString(decoded) != pending.sourceDigest {
			return nil, errors.New("present source state has a malformed SHA-256 digest")
		}
	case stagingSourceMissing:
		if pending.sourceUID != "" || pending.sourceDigest != "" {
			return nil, errors.New("missing source state must not carry a UID or digest")
		}
	default:
		return nil, fmt.Errorf("source state is %q, want %q or %q", pending.sourceState, stagingSourcePresent, stagingSourceMissing)
	}

	material, err := decodeCandidateMaterial(data, config, now)
	if err != nil {
		return nil, err
	}
	pending.material = material
	listenerLeaf, err := validatePendingServingCertificate(
		pending.listenerCertPEM,
		pending.listenerKeyPEM,
		material.ca,
		candidateServiceDNSNames(config),
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("candidate-listener material: %w", err)
	}
	if certificateRawEqual(material.leaf, listenerLeaf) {
		return nil, errors.New("primary and candidate-listener certificates must be distinct")
	}
	pending.listenerLeaf = listenerLeaf
	return pending, nil
}

func decodeCandidateMaterial(data map[string][]byte, config Config, now time.Time) (certificateMaterial, error) {
	ca, normalizedCA, err := parseSingleCertificate(data[stagingCACertificateKey])
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("candidate CA certificate: %w", err)
	}
	if !bytes.Equal(normalizedCA, data[stagingCACertificateKey]) {
		return certificateMaterial{}, errors.New("candidate CA certificate is not canonical PEM")
	}
	caKey, err := parsePrivateKey(data[stagingCAPrivateKeyKey])
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("candidate CA private key: %w", err)
	}
	if !ca.IsCA || !ca.BasicConstraintsValid || ca.KeyUsage&x509.KeyUsageCertSign == 0 ||
		!publicKeysEqual(ca.PublicKey, signerPublicKey(caKey)) || ca.CheckSignatureFrom(ca) != nil {
		return certificateMaterial{}, errors.New("candidate CA certificate and private key are not an exact self-signed CA")
	}
	if !certificateCurrentlyValid(ca, now) || !certificateLifetimeWithinAbsoluteLimit(ca) {
		return certificateMaterial{}, errors.New("candidate CA certificate is expired or exceeds the absolute validity limit")
	}
	leaf, err := validatePendingServingCertificate(
		data[stagingServingCertKey],
		data[stagingServingKeyKey],
		ca,
		requiredDNSNames(config),
		now,
	)
	if err != nil {
		return certificateMaterial{}, fmt.Errorf("candidate primary serving material: %w", err)
	}
	return certificateMaterial{
		caPEM:    append([]byte(nil), data[stagingCACertificateKey]...),
		caKeyPEM: append([]byte(nil), data[stagingCAPrivateKeyKey]...),
		certPEM:  append([]byte(nil), data[stagingServingCertKey]...),
		keyPEM:   append([]byte(nil), data[stagingServingKeyKey]...),
		ca:       ca,
		caKey:    caKey,
		leaf:     leaf,
	}, nil
}

func validatePendingServingCertificate(
	certificatePEM []byte,
	privateKeyPEM []byte,
	ca *x509.Certificate,
	requiredNames []string,
	now time.Time,
) (*x509.Certificate, error) {
	leaf, normalizedLeaf, err := parseSingleCertificate(certificatePEM)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(normalizedLeaf, certificatePEM) {
		return nil, errors.New("certificate is not canonical PEM")
	}
	privateKey, err := parsePrivateKey(privateKeyPEM)
	if err != nil {
		return nil, err
	}
	if !publicKeysEqual(leaf.PublicKey, signerPublicKey(privateKey)) {
		return nil, errors.New("certificate and private key do not match")
	}
	if !servingCertificateAuthentic(leaf, ca, requiredNames) || !certificateCurrentlyValid(leaf, now) ||
		!certificateLifetimeWithinAbsoluteLimit(leaf) || !leaf.NotAfter.Before(ca.NotAfter) {
		return nil, errors.New("certificate is expired or outside the identity or absolute validity policy")
	}
	return leaf, nil
}

func certificateLifetimeWithinAbsoluteLimit(certificate *x509.Certificate) bool {
	if certificate == nil || !certificate.NotAfter.After(certificate.NotBefore) {
		return false
	}
	return certificate.NotAfter.Sub(certificate.NotBefore) <= maximumValidity+certificateBackdate
}

func encodePendingCandidate(pending *pendingCandidate) map[string][]byte {
	return map[string][]byte{
		stagingFormatKey:        []byte(stagingFormat),
		stagingSourceStateKey:   []byte(pending.sourceState),
		stagingSourceUIDKey:     []byte(pending.sourceUID),
		stagingSourceDigestKey:  []byte(pending.sourceDigest),
		stagingCACertificateKey: append([]byte(nil), pending.material.caPEM...),
		stagingCAPrivateKeyKey:  append([]byte(nil), pending.material.caKeyPEM...),
		stagingServingCertKey:   append([]byte(nil), pending.material.certPEM...),
		stagingServingKeyKey:    append([]byte(nil), pending.material.keyPEM...),
		stagingCandidateCertKey: append([]byte(nil), pending.listenerCertPEM...),
		stagingCandidateKeyKey:  append([]byte(nil), pending.listenerKeyPEM...),
	}
}

func secretMaterialDigest(secret *corev1.Secret) string {
	hash := sha256.New()
	for _, key := range []string{CACertificateKey, CAPrivateKeyKey, corev1.TLSCertKey, corev1.TLSPrivateKeyKey} {
		writeLengthPrefixed(hash, []byte(key))
		writeLengthPrefixed(hash, secret.Data[key])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func writeLengthPrefixed(writer io.Writer, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write(value)
}

func (r *Rotator) stageCandidate(
	ctx context.Context,
	staging *corev1.Secret,
	source *corev1.Secret,
) (*corev1.Secret, *pendingCandidate, error) {
	pending, err := generatePendingCandidate(r.random, r.now(), r.config, source)
	if err != nil {
		return nil, nil, fmt.Errorf("generate durable pending CA transition: %w", err)
	}
	updated, err := r.updateStagingSecretData(ctx, staging, encodePendingCandidate(pending), "persist pending CA transition")
	if err != nil {
		return nil, nil, err
	}
	if err := r.candidateSink.StoreCandidateCertificate(pending.listenerCertPEM, pending.listenerKeyPEM); err != nil {
		return nil, nil, fmt.Errorf("load candidate-listener certificate after durable staging: %w", err)
	}
	return updated, pending, nil
}

func (r *Rotator) loadPendingCandidate(pending *pendingCandidate) error {
	if err := r.candidateSink.StoreCandidateCertificate(pending.listenerCertPEM, pending.listenerKeyPEM); err != nil {
		r.candidateSink.ClearCandidateCertificate()
		return fmt.Errorf("load durable candidate-listener certificate: %w", err)
	}
	return nil
}

func (r *Rotator) clearPendingCandidate(ctx context.Context, staging *corev1.Secret) error {
	if _, err := r.updateStagingSecretData(ctx, staging, nil, "clear completed CA transition"); err != nil {
		return err
	}
	r.candidateSink.ClearCandidateCertificate()
	return nil
}

func (r *Rotator) updateStagingSecretData(
	ctx context.Context,
	previous *corev1.Secret,
	data map[string][]byte,
	operation string,
) (*corev1.Secret, error) {
	updated := previous.DeepCopy()
	updated.Data = cloneBytesMap(data)
	observed, err := r.client.CoreV1().Secrets(r.config.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	if err == nil {
		if stagingSecretHasExactData(observed, r.config, previous.UID, data) {
			return observed, nil
		}
		return nil, fmt.Errorf("%s: update response differs from the exact staging contract", operation)
	}

	// A timeout can hide a successful atomic update. Accept only an exact live
	// read-back of this transition; never adopt a partial or unrelated record.
	readBack, getErr := r.client.CoreV1().Secrets(r.config.Namespace).Get(
		ctx,
		r.config.StagingSecretName,
		metav1.GetOptions{},
	)
	if getErr == nil && stagingSecretHasExactData(readBack, r.config, previous.UID, data) {
		return readBack, nil
	}
	if getErr != nil {
		return nil, fmt.Errorf("%s: %w (read-back failed: %v)", operation, err, getErr)
	}
	return nil, fmt.Errorf("%s: %w (read-back contains different staging data)", operation, err)
}

func stagingSecretHasExactData(
	secret *corev1.Secret,
	config Config,
	expectedUID types.UID,
	want map[string][]byte,
) bool {
	return validateStagingSecretMetadata(secret, config) == nil && secret.UID == expectedUID &&
		maps.EqualFunc(secret.Data, want, bytes.Equal)
}

type pendingRelationship int

const (
	pendingUnrelated pendingRelationship = iota
	pendingBeforePrimaryWrite
	pendingAfterPrimaryWrite
)

func relatePendingCandidate(
	primary *corev1.Secret,
	pending *pendingCandidate,
	config Config,
) pendingRelationship {
	if pending.sourceState == stagingSourceMissing {
		if primary == nil {
			return pendingBeforePrimaryWrite
		}
		if exactGeneratedSecret(primary, config, pending.material) {
			return pendingAfterPrimaryWrite
		}
		return pendingUnrelated
	}
	if primary == nil || primary.UID != pending.sourceUID {
		return pendingUnrelated
	}
	if secretMaterialDigest(primary) == pending.sourceDigest {
		return pendingBeforePrimaryWrite
	}
	if exactGeneratedSecret(primary, config, pending.material) {
		return pendingAfterPrimaryWrite
	}
	return pendingUnrelated
}
