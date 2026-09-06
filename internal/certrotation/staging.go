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

	stagingFormat = "v2"

	stagingFormatKey             = "format"
	stagingOperationKey          = "operation"
	stagingPhaseKey              = "phase"
	stagingTransitionDigestKey   = "transition.sha256"
	stagingSourceStateKey        = "source-state"
	stagingSourceUIDKey          = "source-secret.uid"
	stagingSourceDigestKey       = "source-secret.sha256"
	stagingCACertificateKey      = "candidate.ca.crt"
	stagingCAPrivateKeyKey       = "candidate.ca.key"
	stagingServingCertKey        = "candidate.tls.crt"
	stagingServingKeyKey         = "candidate.tls.key"
	stagingCandidateCertKey      = "listener.tls.crt"
	stagingCandidateKeyKey       = "listener.tls.key"
	stagingProofCACertificateKey = "proof.ca.crt"
	stagingProofListenerCertKey  = "proof.listener.tls.crt"
	stagingProofListenerKeyKey   = "proof.listener.tls.key"
	stagingSourceStatePresent    = "present"
	stagingSourceStateMissing    = "missing"
	stagingOperationCATransition = "ca-transition"
	stagingSourceDigestHexSize   = sha256.Size * 2
	stagingTransitionHexSize     = sha256.Size * 2
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

type stagingOperation string

type stagingPhase string

const (
	stagingSourcePresent stagingSourceState = stagingSourceStatePresent
	stagingSourceMissing stagingSourceState = stagingSourceStateMissing

	stagingOperationCA stagingOperation = stagingOperationCATransition

	stagingPhasePrepared                  stagingPhase = "prepared"
	stagingPhaseExpansionMutatingStored   stagingPhase = "expansion-mutating-stored"
	stagingPhaseExpansionBothStored       stagingPhase = "expansion-both-stored"
	stagingPhaseExpansionProven           stagingPhase = "expansion-proven"
	stagingPhasePrimaryWritten            stagingPhase = "primary-written"
	stagingPhasePrimaryServed             stagingPhase = "primary-served"
	stagingPhaseContractionMutatingStored stagingPhase = "contraction-mutating-stored"
	stagingPhaseContractionBothStored     stagingPhase = "contraction-both-stored"
	stagingPhaseContractionProven         stagingPhase = "contraction-proven"
	stagingPhaseMutatingParked            stagingPhase = "mutating-parked"
	stagingPhaseBothParked                stagingPhase = "both-parked"
)

type pendingCandidate struct {
	operation            stagingOperation
	phase                stagingPhase
	transitionDigest     string
	sourceState          stagingSourceState
	sourceUID            types.UID
	sourceDigest         string
	material             certificateMaterial
	listenerCertPEM      []byte
	listenerKeyPEM       []byte
	listenerLeaf         *x509.Certificate
	proofCACertPEM       []byte
	proofCA              *x509.Certificate
	proofListenerCertPEM []byte
	proofListenerKeyPEM  []byte
	proofListenerLeaf    *x509.Certificate
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
	pending, err := decodePendingCandidate(secret.Data, r.config)
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
	if !maps.Equal(secret.Labels, stagingSecretLabels()) {
		return errors.New("labels are not the exact managed staging identity")
	}
	if !maps.Equal(secret.Annotations, helmOwnershipAnnotations(config)) ||
		len(secret.OwnerReferences) != 0 || len(secret.Finalizers) != 0 ||
		secret.Immutable != nil || len(secret.StringData) != 0 {
		return errors.New("annotations are not the exact Helm ownership identity or unsupported lifecycle metadata is present")
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
	if !maps.Equal(secret.Labels, generatedSecretLabels()) {
		return errors.New("labels are not the exact generated certificate identity")
	}
	if !maps.Equal(secret.Annotations, helmOwnershipAnnotations(config)) ||
		len(secret.OwnerReferences) != 0 || len(secret.Finalizers) != 0 ||
		secret.Immutable != nil || len(secret.StringData) != 0 {
		return errors.New("annotations are not the exact Helm ownership identity or unsupported lifecycle metadata is present")
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
	proofConfig := config
	proofConfig.ServiceName = config.CandidateServiceName
	proofConfig.ServiceNamespace = config.Namespace
	proofMaterial, err := generateMaterial(reader, now, proofConfig)
	if err != nil {
		return nil, fmt.Errorf("generate contraction-proof certificate: %w", err)
	}
	pending := &pendingCandidate{
		operation:            stagingOperationCA,
		phase:                stagingPhasePrepared,
		material:             material,
		listenerCertPEM:      append([]byte(nil), listenerMaterial.certPEM...),
		listenerKeyPEM:       append([]byte(nil), listenerMaterial.keyPEM...),
		listenerLeaf:         listenerMaterial.leaf,
		proofCACertPEM:       append([]byte(nil), proofMaterial.caPEM...),
		proofCA:              proofMaterial.ca,
		proofListenerCertPEM: append([]byte(nil), proofMaterial.certPEM...),
		proofListenerKeyPEM:  append([]byte(nil), proofMaterial.keyPEM...),
		proofListenerLeaf:    proofMaterial.leaf,
	}
	if source == nil {
		pending.sourceState = stagingSourceMissing
		pending.transitionDigest = pendingCandidateDigest(pending)
		return pending, nil
	}
	if source.UID == "" {
		return nil, errors.New("source TLS Secret has no live UID")
	}
	pending.sourceState = stagingSourcePresent
	pending.sourceUID = source.UID
	pending.sourceDigest = secretMaterialDigest(source)
	pending.transitionDigest = pendingCandidateDigest(pending)
	return pending, nil
}

func decodePendingCandidate(data map[string][]byte, config Config) (*pendingCandidate, error) {
	if len(data) != 16 {
		return nil, fmt.Errorf("data has %d fields, want exactly 16", len(data))
	}
	for _, key := range []string{
		stagingFormatKey,
		stagingOperationKey,
		stagingPhaseKey,
		stagingTransitionDigestKey,
		stagingSourceStateKey,
		stagingSourceUIDKey,
		stagingSourceDigestKey,
		stagingCACertificateKey,
		stagingCAPrivateKeyKey,
		stagingServingCertKey,
		stagingServingKeyKey,
		stagingCandidateCertKey,
		stagingCandidateKeyKey,
		stagingProofCACertificateKey,
		stagingProofListenerCertKey,
		stagingProofListenerKeyKey,
	} {
		if _, found := data[key]; !found {
			return nil, fmt.Errorf("required data field %q is missing", key)
		}
	}
	if string(data[stagingFormatKey]) != stagingFormat {
		return nil, fmt.Errorf("format is %q, want %q", data[stagingFormatKey], stagingFormat)
	}

	pending := &pendingCandidate{
		operation:            stagingOperation(data[stagingOperationKey]),
		phase:                stagingPhase(data[stagingPhaseKey]),
		transitionDigest:     string(data[stagingTransitionDigestKey]),
		sourceState:          stagingSourceState(data[stagingSourceStateKey]),
		sourceUID:            types.UID(data[stagingSourceUIDKey]),
		sourceDigest:         string(data[stagingSourceDigestKey]),
		listenerCertPEM:      append([]byte(nil), data[stagingCandidateCertKey]...),
		listenerKeyPEM:       append([]byte(nil), data[stagingCandidateKeyKey]...),
		proofCACertPEM:       append([]byte(nil), data[stagingProofCACertificateKey]...),
		proofListenerCertPEM: append([]byte(nil), data[stagingProofListenerCertKey]...),
		proofListenerKeyPEM:  append([]byte(nil), data[stagingProofListenerKeyKey]...),
	}
	if pending.operation != stagingOperationCA {
		return nil, fmt.Errorf("operation is %q, want %q", pending.operation, stagingOperationCA)
	}
	if !validStagingPhase(pending.phase) {
		return nil, fmt.Errorf("phase %q is not a supported CA-transition cursor", pending.phase)
	}
	if len(pending.transitionDigest) != stagingTransitionHexSize {
		return nil, errors.New("transition digest is not a canonical SHA-256 digest")
	}
	decodedTransitionDigest, err := hex.DecodeString(pending.transitionDigest)
	if err != nil || hex.EncodeToString(decodedTransitionDigest) != pending.transitionDigest {
		return nil, errors.New("transition digest is not a canonical SHA-256 digest")
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

	material, err := decodeCandidateMaterial(data, config)
	if err != nil {
		return nil, err
	}
	pending.material = material
	listenerLeaf, err := decodePendingServingCertificate(
		pending.listenerCertPEM,
		pending.listenerKeyPEM,
		material.ca,
		candidateServiceDNSNames(config),
	)
	if err != nil {
		return nil, fmt.Errorf("candidate-listener material: %w", err)
	}
	if certificateRawEqual(material.leaf, listenerLeaf) {
		return nil, errors.New("primary and candidate-listener certificates must be distinct")
	}
	pending.listenerLeaf = listenerLeaf

	proofCA, normalizedProofCA, err := parseSingleCertificate(pending.proofCACertPEM)
	if err != nil {
		return nil, fmt.Errorf("contraction-proof CA certificate: %w", err)
	}
	if !bytes.Equal(normalizedProofCA, pending.proofCACertPEM) {
		return nil, errors.New("contraction-proof CA certificate is not canonical PEM")
	}
	if !selfSignedCertificateAuthority(proofCA) || !certificateLifetimeWithinAbsoluteLimit(proofCA) {
		return nil, errors.New("contraction-proof CA certificate is not an exact self-signed CA within the absolute validity limit")
	}
	if publicKeysEqual(material.ca.PublicKey, proofCA.PublicKey) {
		return nil, errors.New("candidate and contraction-proof CAs must be distinct")
	}
	proofLeaf, err := decodePendingServingCertificate(
		pending.proofListenerCertPEM,
		pending.proofListenerKeyPEM,
		proofCA,
		candidateServiceDNSNames(config),
	)
	if err != nil {
		return nil, fmt.Errorf("contraction-proof listener material: %w", err)
	}
	if certificateRawEqual(material.leaf, proofLeaf) || certificateRawEqual(listenerLeaf, proofLeaf) {
		return nil, errors.New("candidate primary, expansion listener, and contraction-proof listener certificates must be distinct")
	}
	pending.proofCA = proofCA
	pending.proofListenerLeaf = proofLeaf
	if pendingCandidateDigest(pending) != pending.transitionDigest {
		return nil, errors.New("transition digest does not match the exact staged material")
	}
	return pending, nil
}

func decodeCandidateMaterial(data map[string][]byte, config Config) (certificateMaterial, error) {
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
	if !selfSignedCertificateAuthority(ca) || !publicKeysEqual(ca.PublicKey, signerPublicKey(caKey)) {
		return certificateMaterial{}, errors.New("candidate CA certificate and private key are not an exact self-signed CA")
	}
	if !certificateLifetimeWithinAbsoluteLimit(ca) {
		return certificateMaterial{}, errors.New("candidate CA certificate exceeds the absolute validity limit")
	}
	leaf, err := decodePendingServingCertificate(
		data[stagingServingCertKey],
		data[stagingServingKeyKey],
		ca,
		requiredDNSNames(config),
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

func decodePendingServingCertificate(
	certificatePEM []byte,
	privateKeyPEM []byte,
	ca *x509.Certificate,
	requiredNames []string,
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
	if !servingCertificateAuthentic(leaf, ca, requiredNames) ||
		!certificateLifetimeWithinAbsoluteLimit(leaf) || !leaf.NotAfter.Before(ca.NotAfter) {
		return nil, errors.New("certificate is outside the identity or absolute validity policy")
	}
	return leaf, nil
}

func pendingCandidateTemporalUsability(pending *pendingCandidate, now time.Time) error {
	if pending == nil {
		return errors.New("durable pending CA transition is nil")
	}
	for _, certificate := range []struct {
		name  string
		value *x509.Certificate
	}{
		{name: "candidate CA", value: pending.material.ca},
		{name: "candidate primary serving", value: pending.material.leaf},
		{name: "candidate listener", value: pending.listenerLeaf},
		{name: "contraction-proof CA", value: pending.proofCA},
		{name: "contraction-proof listener", value: pending.proofListenerLeaf},
	} {
		if !certificateCurrentlyValid(certificate.value, now) {
			return fmt.Errorf("%s certificate is not currently valid", certificate.name)
		}
	}
	return nil
}

func certificateLifetimeWithinAbsoluteLimit(certificate *x509.Certificate) bool {
	if certificate == nil || !certificate.NotAfter.After(certificate.NotBefore) {
		return false
	}
	return certificate.NotAfter.Sub(certificate.NotBefore) <= maximumValidity+certificateBackdate
}

func validStagingPhase(phase stagingPhase) bool {
	switch phase {
	case stagingPhasePrepared,
		stagingPhaseExpansionMutatingStored,
		stagingPhaseExpansionBothStored,
		stagingPhaseExpansionProven,
		stagingPhasePrimaryWritten,
		stagingPhasePrimaryServed,
		stagingPhaseContractionMutatingStored,
		stagingPhaseContractionBothStored,
		stagingPhaseContractionProven,
		stagingPhaseMutatingParked,
		stagingPhaseBothParked:
		return true
	default:
		return false
	}
}

func nextStagingPhase(phase stagingPhase) (stagingPhase, bool) {
	switch phase {
	case stagingPhasePrepared:
		return stagingPhaseExpansionMutatingStored, true
	case stagingPhaseExpansionMutatingStored:
		return stagingPhaseExpansionBothStored, true
	case stagingPhaseExpansionBothStored:
		return stagingPhaseExpansionProven, true
	case stagingPhaseExpansionProven:
		return stagingPhasePrimaryWritten, true
	case stagingPhasePrimaryWritten:
		return stagingPhasePrimaryServed, true
	case stagingPhasePrimaryServed:
		return stagingPhaseContractionMutatingStored, true
	case stagingPhaseContractionMutatingStored:
		return stagingPhaseContractionBothStored, true
	case stagingPhaseContractionBothStored:
		return stagingPhaseContractionProven, true
	case stagingPhaseContractionProven:
		return stagingPhaseMutatingParked, true
	case stagingPhaseMutatingParked:
		return stagingPhaseBothParked, true
	default:
		return "", false
	}
}

func (r *Rotator) advancePendingPhase(
	ctx context.Context,
	staging *corev1.Secret,
	pending *pendingCandidate,
	next stagingPhase,
) (*corev1.Secret, error) {
	if pending == nil || !validStagingPhase(pending.phase) || !validStagingPhase(next) {
		return nil, errors.New("advance durable CA transition: current and next phases must be valid")
	}
	if pending.phase == next {
		return staging, nil
	}
	want, ok := nextStagingPhase(pending.phase)
	if !ok || next != want {
		return nil, fmt.Errorf("advance durable CA transition: phase %q cannot move directly to %q", pending.phase, next)
	}
	advanced := *pending
	advanced.phase = next
	updated, err := r.updateStagingSecretData(
		ctx,
		staging,
		encodePendingCandidate(&advanced),
		fmt.Sprintf("advance durable CA transition to %s", next),
	)
	if err != nil {
		return nil, err
	}
	pending.phase = next
	return updated, nil
}

func encodePendingCandidate(pending *pendingCandidate) map[string][]byte {
	return map[string][]byte{
		stagingFormatKey:             []byte(stagingFormat),
		stagingOperationKey:          []byte(pending.operation),
		stagingPhaseKey:              []byte(pending.phase),
		stagingTransitionDigestKey:   []byte(pending.transitionDigest),
		stagingSourceStateKey:        []byte(pending.sourceState),
		stagingSourceUIDKey:          []byte(pending.sourceUID),
		stagingSourceDigestKey:       []byte(pending.sourceDigest),
		stagingCACertificateKey:      append([]byte(nil), pending.material.caPEM...),
		stagingCAPrivateKeyKey:       append([]byte(nil), pending.material.caKeyPEM...),
		stagingServingCertKey:        append([]byte(nil), pending.material.certPEM...),
		stagingServingKeyKey:         append([]byte(nil), pending.material.keyPEM...),
		stagingCandidateCertKey:      append([]byte(nil), pending.listenerCertPEM...),
		stagingCandidateKeyKey:       append([]byte(nil), pending.listenerKeyPEM...),
		stagingProofCACertificateKey: append([]byte(nil), pending.proofCACertPEM...),
		stagingProofListenerCertKey:  append([]byte(nil), pending.proofListenerCertPEM...),
		stagingProofListenerKeyKey:   append([]byte(nil), pending.proofListenerKeyPEM...),
	}
}

func pendingCandidateDigest(pending *pendingCandidate) string {
	hash := sha256.New()
	for _, field := range []struct {
		key   string
		value []byte
	}{
		{key: stagingFormatKey, value: []byte(stagingFormat)},
		{key: stagingOperationKey, value: []byte(pending.operation)},
		{key: stagingSourceStateKey, value: []byte(pending.sourceState)},
		{key: stagingSourceUIDKey, value: []byte(pending.sourceUID)},
		{key: stagingSourceDigestKey, value: []byte(pending.sourceDigest)},
		{key: stagingCACertificateKey, value: pending.material.caPEM},
		{key: stagingCAPrivateKeyKey, value: pending.material.caKeyPEM},
		{key: stagingServingCertKey, value: pending.material.certPEM},
		{key: stagingServingKeyKey, value: pending.material.keyPEM},
		{key: stagingCandidateCertKey, value: pending.listenerCertPEM},
		{key: stagingCandidateKeyKey, value: pending.listenerKeyPEM},
		{key: stagingProofCACertificateKey, value: pending.proofCACertPEM},
		{key: stagingProofListenerCertKey, value: pending.proofListenerCertPEM},
		{key: stagingProofListenerKeyKey, value: pending.proofListenerKeyPEM},
	} {
		writeLengthPrefixed(hash, []byte(field.key))
		writeLengthPrefixed(hash, field.value)
	}
	return hex.EncodeToString(hash.Sum(nil))
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
	return updated, pending, nil
}

func (r *Rotator) clearPendingCandidate(ctx context.Context, staging *corev1.Secret) error {
	if _, err := r.updateStagingSecretData(ctx, staging, nil, "clear durable CA transition"); err != nil {
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
