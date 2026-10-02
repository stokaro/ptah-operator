package certrotation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	"github.com/stokaro/ptah-operator/internal/resultcredentials"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const ResultTrustLabel = "operator.ptah.run/result-trust"
const resultJournalKey = "rotation.json"

// ResultConfig names precreated installation objects. The projection contains
// the manager's six mounted files; the journal also holds private server CAs.
// The public policy contains only enrollment digests. No CREATE grant is needed.
type ResultConfig struct {
	Config
	PolicyName string
}

type ResultRotator struct {
	client kubernetes.Interface
	config ResultConfig
	now    func() time.Time
	random io.Reader
	probe  func(context.Context, map[string][]byte, []certificateMaterial) error
}

type resultKeys struct {
	ServerCA, ServerKey, ServerCertificate, ServerCertificateKey []byte
	ClientCA, ClientKey                                          []byte
}
type resultJournal struct {
	Version       int         `json:"version"`
	ProjectionUID types.UID   `json:"projectionUID"`
	PolicyUID     types.UID   `json:"policyUID"`
	Phase         string      `json:"phase"`
	Current       resultKeys  `json:"current"`
	Next          *resultKeys `json:"next,omitempty"`
	// FencedAt is written only after the public policy was read back. It is
	// cleared between waits, never inferred from a pre-write client timestamp.
	FencedAt *time.Time `json:"fencedAt,omitempty"`
}

func NewResultRotator(api kubernetes.Interface, config ResultConfig) (*ResultRotator, error) {
	if nilDependency(api) {
		return nil, errors.New("Kubernetes client is required")
	}
	if config.ServiceNamespace == "" {
		config.ServiceNamespace = config.Namespace
	}
	for _, name := range []string{config.SecretName, config.StagingSecretName, config.PolicyName, config.LeaseName, config.ReleaseName} {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return nil, errors.New("invalid result rotation object name")
		}
	}
	if len(validation.IsDNS1123Label(config.Namespace)) != 0 || config.Namespace != config.ServiceNamespace || len(validation.IsDNS1123Label(config.ServiceName)) != 0 || len(validation.IsValidPortName(config.EndpointPortName)) != 0 || config.SecretName == config.StagingSecretName || config.HolderIdentity == "" || len(config.HolderIdentity) > 128 {
		return nil, errors.New("invalid result rotation scope")
	}
	if config.AcquireTimeout == 0 {
		config.AcquireTimeout = defaultAcquireTimeout
	}
	wait := resultcredentials.MaxCredentialLifetime + maximumCertificatePolicyClockSkew
	if config.RenewalThreshold <= 3*wait || config.ServingCertificateValidity <= config.RenewalThreshold || config.CACertificateValidity <= config.ServingCertificateValidity || config.CACertificateValidity > maximumValidity || config.ProbeInterval <= 0 || config.ProbeTimeout <= config.ProbeInterval || config.ProbeTimeout > maximumOperationTime || config.LeaseDuration < minimumLeaseDuration || config.LeaseDuration > maximumOperationTime || config.AcquireTimeout <= 0 || config.AcquireTimeout > config.LeaseDuration {
		return nil, errors.New("invalid result rotation lifetime or operation bounds")
	}
	r := &ResultRotator{client: api, config: config, now: time.Now, random: rand.Reader}
	r.probe = r.probeResultProjection
	return r, nil
}

func (r *ResultRotator) Run(ctx context.Context) (_ Result, runErr error) {
	guard, err := acquireLease(ctx, r.client, leaseConfig{Namespace: r.config.Namespace, Name: r.config.LeaseName, HolderIdentity: r.config.HolderIdentity, Duration: r.config.LeaseDuration, AcquireTimeout: r.config.AcquireTimeout, Now: r.now})
	if err != nil {
		return Result{}, err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := guard.Close(c); runErr == nil {
			runErr = err
		}
	}()
	return r.step(guard.Context())
}

func (r *ResultRotator) step(ctx context.Context) (Result, error) {
	journal, projection, policy, err := r.objects(ctx)
	if err != nil {
		return Result{}, err
	}
	if len(journal.Data) == 0 {
		if len(projection.Data) != 0 || len(policy.Data) != 0 {
			return Result{}, errors.New("result rotation journal is missing for existing trust")
		}
		keys, err := r.generateKeys()
		if err != nil {
			return Result{}, err
		}
		st := resultJournal{Version: 1, ProjectionUID: projection.UID, PolicyUID: policy.UID, Phase: "bootstrap", Current: keys}
		return Result{Pending: true, RequeueAfter: time.Second}, r.saveJournal(ctx, journal, st)
	}
	st, err := r.decodeJournal(journal.Data)
	if err != nil {
		return Result{}, err
	}
	if st.ProjectionUID != projection.UID || st.PolicyUID != policy.UID {
		return Result{}, errors.New("result rotation object identity changed")
	}
	if st.Phase == "stable" {
		current, clientCA, err := r.inspectKeys(st.Current)
		if err != nil {
			return Result{}, err
		}
		if !equalBytes(projection.Data, st.Current.projection()) || !maps.Equal(policy.Data, enrollment(st.Current.projection())) {
			return Result{}, errors.New("stable result trust differs from its journal")
		}
		renewCA := !r.now().Add(r.config.RenewalThreshold).Before(current.ca.NotAfter) || !r.now().Add(r.config.RenewalThreshold).Before(clientCA.ca.NotAfter) || !r.now().Add(r.config.ServingCertificateValidity).Before(current.ca.NotAfter)
		renewLeaf := !r.now().Add(r.config.RenewalThreshold).Before(current.leaf.NotAfter)
		// Repair an expired leaf under a still-usable CA before distributing trust.
		// An expired CA has no valid old clients left and is handled by expiry bounds
		// in the prepare wait instead.
		firstWait := resultcredentials.MaxCredentialLifetime + maximumCertificatePolicyClockSkew
		leafNeededUntil := minTime(r.now().Add(firstWait+r.config.ProbeTimeout+time.Minute), current.ca.NotAfter.Add(-2*time.Second))
		repairLeaf := current.ca.NotAfter.Sub(r.now()) > time.Minute && (!certificateCurrentlyValid(current.leaf, r.now()) || (renewCA && !current.leaf.NotAfter.After(leafNeededUntil)))
		if renewCA && !repairLeaf {
			next, err := r.generateKeys()
			if err != nil {
				return Result{}, err
			}
			st.Next, st.Phase = &next, "prepare"
		} else if renewLeaf || repairLeaf {
			validity := min(r.config.ServingCertificateValidity, current.ca.NotAfter.Sub(r.now())-time.Second)
			nextMaterial, err := generateServingMaterialForService(r.random, r.now(), validity, r.config.ServiceName, r.config.Namespace, current)
			if err != nil {
				return Result{}, err
			}
			next := st.Current
			next.ServerCertificate, next.ServerCertificateKey = nextMaterial.certPEM, nextMaterial.keyPEM
			st.Next, st.Phase = &next, "leaf"
		} else {
			return Result{}, r.probeState(ctx, st, st.Current.projection())
		}
		return Result{Pending: true, RequeueAfter: time.Second}, r.saveJournal(ctx, journal, st)
	}
	desired, prior := st.projections()
	// Refuse foreign state rather than overwrite it. Once a wait has started,
	// accepting a rolled-back enrollment policy would revive old issuance while
	// retaining an earlier retirement deadline.
	desiredPolicy := enrollment(desired)
	if !maps.Equal(policy.Data, desiredPolicy) && (st.FencedAt != nil || !maps.Equal(policy.Data, enrollment(prior))) {
		return Result{}, errors.New("result enrollment policy moved outside the pending transition")
	}
	if !equalBytes(projection.Data, desired) && !equalBytes(projection.Data, prior) {
		return Result{}, errors.New("result trust projection moved outside the pending transition")
	}
	if err := r.writePolicy(ctx, policy, desiredPolicy); err != nil {
		return Result{}, err
	}
	if (st.Phase == "prepare" || st.Phase == "switch") && st.FencedAt == nil {
		now := r.now().UTC()
		st.FencedAt = &now
		// Finish this pass here: the next pass resumes from the persisted timestamp
		// even if the write response is lost. No Secret is switched before this write.
		return Result{Pending: true, RequeueAfter: time.Second}, r.saveJournal(ctx, journal, st)
	}
	if err := r.writeProjection(ctx, projection, desired); err != nil {
		return Result{}, err
	}
	if st.Phase == "prepare" || st.Phase == "switch" {
		deadline := r.retirementDeadline(st)
		if remaining := deadline.Sub(r.now()); remaining > 0 {
			if err := r.probeState(ctx, st, desired); err != nil {
				return Result{}, err
			}
			return Result{RequeueAfter: remaining}, nil
		}
		// After old server CA expiry, requiring its expired leaf to pass TLS would
		// prevent recovery. Its authenticated journal still identifies the old keys.
		current, _, _ := r.inspectKeys(st.Current)
		if st.Phase != "prepare" || r.now().Before(current.ca.NotAfter.Add(maximumCertificatePolicyClockSkew)) {
			if err := r.probeState(ctx, st, desired); err != nil {
				return Result{}, err
			}
		}
		if st.Phase == "prepare" {
			st.Phase = "switch"
		} else {
			st.Phase = "retire"
		}
		st.FencedAt = nil
		return Result{Pending: true, RequeueAfter: time.Second}, r.saveJournal(ctx, journal, st)
	}
	if err := r.probeState(ctx, st, desired); err != nil {
		return Result{}, err
	}
	if st.Next != nil {
		st.Current = *st.Next
	}
	st.Next, st.FencedAt, st.Phase = nil, nil, "stable"
	return Result{Pending: true, RequeueAfter: time.Second}, r.saveJournal(ctx, journal, st)
}

func (r *ResultRotator) retirementDeadline(st resultJournal) time.Time {
	server, client, _ := r.inspectKeys(st.Current)
	expires := server.ca.NotAfter
	if st.Phase == "switch" {
		expires = client.ca.NotAfter
	}
	return minTime(st.FencedAt.Add(resultcredentials.MaxCredentialLifetime+maximumCertificatePolicyClockSkew), expires.Add(maximumCertificatePolicyClockSkew))
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func equalBytes(a, b map[string][]byte) bool { return maps.EqualFunc(a, b, bytes.Equal) }
func enrollment(projection map[string][]byte) map[string]string {
	if len(projection) == 0 {
		return nil
	}
	ca, _, err := parseSingleCertificate(projection["client-ca.crt"])
	if err != nil {
		return nil
	}
	return resultcredentials.EnrollmentData(ca.Raw, projection["ca.crt"])
}
func (k resultKeys) projection() map[string][]byte {
	return map[string][]byte{"tls.crt": k.ServerCertificate, "tls.key": k.ServerCertificateKey, "ca.crt": k.ServerCA, "client-ca.crt": k.ClientCA, "client-ca.key": k.ClientKey, "client-trust.crt": k.ClientCA}
}
func (st resultJournal) projections() (desired, prior map[string][]byte) {
	base := st.Current.projection()
	if st.Phase == "bootstrap" {
		return base, nil
	}
	if st.Phase == "leaf" {
		return st.Next.projection(), base
	}
	expanded := st.Current.projection()
	expanded["ca.crt"] = append(bytes.Clone(st.Current.ServerCA), st.Next.ServerCA...)
	expanded["client-trust.crt"] = append(bytes.Clone(st.Current.ClientCA), st.Next.ClientCA...)
	if st.Phase == "prepare" {
		return expanded, base
	}
	switched := st.Next.projection()
	switched["ca.crt"], switched["client-trust.crt"] = expanded["ca.crt"], expanded["client-trust.crt"]
	if st.Phase == "switch" {
		return switched, expanded
	}
	return st.Next.projection(), switched
}

func (r *ResultRotator) generateKeys() (resultKeys, error) {
	server, err := generateMaterial(r.random, r.now(), r.config.Config)
	if err != nil {
		return resultKeys{}, err
	}
	clientConfig := r.config.Config
	clientConfig.ServiceName += "-client"
	clientCA, err := generateMaterial(r.random, r.now(), clientConfig)
	if err != nil {
		return resultKeys{}, err
	}
	return resultKeys{server.caPEM, server.caKeyPEM, server.certPEM, server.keyPEM, clientCA.caPEM, clientCA.caKeyPEM}, nil
}
func (r *ResultRotator) inspectKeys(k resultKeys) (certificateMaterial, certificateMaterial, error) {
	invalid := errors.New("invalid result rotation key material")
	parse := func(cert, key []byte) (certificateMaterial, error) {
		ca, _, err := parseSingleCertificate(cert)
		if err != nil || !selfSignedCertificateAuthority(ca) {
			return certificateMaterial{}, invalid
		}
		signer, err := parsePrivateKey(key)
		if err != nil || !publicKeysEqual(ca.PublicKey, signerPublicKey(signer)) {
			return certificateMaterial{}, invalid
		}
		return certificateMaterial{caPEM: cert, caKeyPEM: key, ca: ca, caKey: signer}, nil
	}
	server, err := parse(k.ServerCA, k.ServerKey)
	if err != nil {
		return certificateMaterial{}, certificateMaterial{}, err
	}
	clientCA, err := parse(k.ClientCA, k.ClientKey)
	if err != nil {
		return certificateMaterial{}, certificateMaterial{}, err
	}
	if publicKeysEqual(server.ca.PublicKey, clientCA.ca.PublicKey) {
		return certificateMaterial{}, certificateMaterial{}, invalid
	}
	leaf, err := parseLeafAndKey(k.ServerCertificate, k.ServerCertificateKey)
	if err != nil || !servingCertificateAuthentic(leaf, server.ca, requiredDNSNames(r.config.Config)) {
		return certificateMaterial{}, certificateMaterial{}, invalid
	}
	server.certPEM, server.keyPEM, server.leaf = k.ServerCertificate, k.ServerCertificateKey, leaf
	return server, clientCA, nil
}
func (r *ResultRotator) decodeJournal(data map[string][]byte) (resultJournal, error) {
	invalid := errors.New("invalid result rotation journal")
	var st resultJournal
	if len(data) != 1 || len(data[resultJournalKey]) == 0 || len(data[resultJournalKey]) > 128<<10 {
		return st, invalid
	}
	dec := json.NewDecoder(bytes.NewReader(data[resultJournalKey]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return st, invalid
	}
	encoded, _ := json.Marshal(st)
	if !bytes.Equal(encoded, data[resultJournalKey]) || st.Version != 1 || st.ProjectionUID == "" || st.PolicyUID == "" {
		return st, invalid
	}
	switch st.Phase {
	case "stable", "bootstrap":
		if st.Next != nil || st.FencedAt != nil {
			return st, invalid
		}
	case "leaf", "retire":
		if st.Next == nil || st.FencedAt != nil {
			return st, invalid
		}
	case "prepare", "switch":
		if st.Next == nil {
			return st, invalid
		}
	default:
		return st, invalid
	}
	if st.FencedAt != nil && (st.FencedAt.IsZero() || st.FencedAt.After(r.now().Add(maximumCertificatePolicyClockSkew))) {
		return st, invalid
	}
	if _, _, err := r.inspectKeys(st.Current); err != nil {
		return st, invalid
	}
	if st.Next != nil {
		if _, _, err := r.inspectKeys(*st.Next); err != nil {
			return st, invalid
		}
	}
	return st, nil
}

func (r *ResultRotator) metadata(o metav1.Object, name, role string) bool {
	return o.GetName() == name && o.GetNamespace() == r.config.Namespace && o.GetUID() != "" && o.GetResourceVersion() != "" && o.GetDeletionTimestamp() == nil && o.GetGenerateName() == "" && len(o.GetOwnerReferences()) == 0 && len(o.GetFinalizers()) == 0 &&
		maps.Equal(o.GetLabels(), map[string]string{ResultTrustLabel: role, HelmManagedByLabel: HelmManagedByLabelValue}) && maps.Equal(o.GetAnnotations(), helmOwnershipAnnotations(r.config.Config))
}
func (r *ResultRotator) objects(ctx context.Context) (*corev1.Secret, *corev1.Secret, *corev1.ConfigMap, error) {
	secrets := r.client.CoreV1().Secrets(r.config.Namespace)
	journal, err := secrets.Get(ctx, r.config.StagingSecretName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, err
	}
	projection, err := secrets.Get(ctx, r.config.SecretName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, err
	}
	policy, err := r.client.CoreV1().ConfigMaps(r.config.Namespace).Get(ctx, r.config.PolicyName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, nil, err
	}
	if !r.validSecret(journal, r.config.StagingSecretName, "journal") || !r.validSecret(projection, r.config.SecretName, "projection") || !r.metadata(policy, r.config.PolicyName, "enrollment") || len(policy.BinaryData) != 0 || policy.Immutable != nil {
		return nil, nil, nil, errors.New("result rotation objects violate their installation contract")
	}
	return journal, projection, policy, nil
}
func (r *ResultRotator) validSecret(s *corev1.Secret, name, role string) bool {
	return r.metadata(s, name, role) && s.Type == corev1.SecretTypeOpaque && s.Immutable == nil && len(s.StringData) == 0
}
func (r *ResultRotator) saveJournal(ctx context.Context, old *corev1.Secret, st resultJournal) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return errors.New("encode result rotation journal")
	}
	return r.writeSecret(ctx, old, map[string][]byte{resultJournalKey: raw}, "journal")
}
func (r *ResultRotator) writeProjection(ctx context.Context, old *corev1.Secret, data map[string][]byte) error {
	return r.writeSecret(ctx, old, data, "projection")
}
func (r *ResultRotator) writeSecret(ctx context.Context, old *corev1.Secret, data map[string][]byte, role string) error {
	if equalBytes(old.Data, data) {
		return ctx.Err()
	}
	next := old.DeepCopy()
	next.Data = data
	_, writeErr := r.client.CoreV1().Secrets(old.Namespace).Update(ctx, next, metav1.UpdateOptions{})
	got, err := r.client.CoreV1().Secrets(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
	if err == nil && got.UID == old.UID && r.validSecret(got, old.Name, role) && equalBytes(got.Data, data) {
		return ctx.Err()
	}
	if writeErr != nil {
		return fmt.Errorf("result %s write was not confirmed: %w", role, writeErr)
	}
	return fmt.Errorf("result %s readback differs from the intended write", role)
}
func (r *ResultRotator) writePolicy(ctx context.Context, old *corev1.ConfigMap, data map[string]string) error {
	if maps.Equal(old.Data, data) {
		return ctx.Err()
	}
	next := old.DeepCopy()
	next.Data = data
	_, writeErr := r.client.CoreV1().ConfigMaps(old.Namespace).Update(ctx, next, metav1.UpdateOptions{})
	got, err := r.client.CoreV1().ConfigMaps(old.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
	if err == nil && got.UID == old.UID && r.metadata(got, old.Name, "enrollment") && len(got.BinaryData) == 0 && got.Immutable == nil && maps.Equal(got.Data, data) {
		return ctx.Err()
	}
	if writeErr != nil {
		return fmt.Errorf("result enrollment write was not confirmed: %w", writeErr)
	}
	return errors.New("result enrollment readback differs from the intended write")
}
