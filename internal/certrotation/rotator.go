package certrotation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	CACertificateKey = "ca.crt"
	CAPrivateKeyKey  = "ca.key"

	GeneratedSecretLabel      = "operator.ptah.dev/generated-webhook-certificate"
	GeneratedSecretLabelValue = "true"

	secretCreateGuardDenialMessage = "certificate rotator Secret CREATE is outside its exact recovery contract"
	webhookServicePort             = int32(443)

	defaultAcquireTimeout = 30 * time.Second
	minimumLeaseDuration  = 30 * time.Second
	maximumValidity       = 20 * 365 * 24 * time.Hour
	maximumOperationTime  = 24 * time.Hour
)

// Config names the exact objects in one chart-managed certificate lifecycle.
// It contains no credentials or certificate material.
type Config struct {
	Namespace                      string
	SecretName                     string
	LeaseName                      string
	MutatingWebhookConfiguration   string
	MutatingWebhookNames           []string
	ValidatingWebhookConfiguration string
	ValidatingWebhookNames         []string
	ServiceName                    string
	ServiceNamespace               string
	EndpointPortName               string
	HolderIdentity                 string
	SecretCreatePolicyName         string
	SecretCreatePolicyBindingName  string
	SecretCreateServiceAccountName string
	RecreateMissingSecret          bool

	RenewalThreshold           time.Duration
	ServingCertificateValidity time.Duration
	CACertificateValidity      time.Duration
	ProbeTimeout               time.Duration
	ProbeInterval              time.Duration
	LeaseDuration              time.Duration
	AcquireTimeout             time.Duration
}

// Rotator serializes and performs one fail-closed reconciliation of the
// chart-managed webhook certificate and trust bundles.
type Rotator struct {
	client kubernetes.Interface
	config Config
	now    func() time.Time
	random io.Reader
	probe  certificateProber
}

// New returns a Rotator after validating all names and lifecycle durations.
func New(client kubernetes.Interface, config Config) (*Rotator, error) {
	if client == nil {
		return nil, errors.New("Kubernetes client is required")
	}
	if config.AcquireTimeout == 0 {
		config.AcquireTimeout = defaultAcquireTimeout
	}
	if err := validateConfig(config); err != nil {
		return nil, err
	}
	return &Rotator{
		client: client,
		config: config,
		now:    time.Now,
		random: rand.Reader,
		probe:  tlsCertificateProber{},
	}, nil
}

// Run acquires the task-scoped Lease and reconciles the Secret and both
// webhook configurations. The Lease is released on every return path.
func (r *Rotator) Run(ctx context.Context) (runErr error) {
	guard, err := acquireLease(ctx, r.client, leaseConfig{
		Namespace:      r.config.Namespace,
		Name:           r.config.LeaseName,
		HolderIdentity: r.config.HolderIdentity,
		Duration:       r.config.LeaseDuration,
		AcquireTimeout: r.config.AcquireTimeout,
		Now:            r.now,
	})
	if err != nil {
		return fmt.Errorf("acquire certificate rotation lease: %w", err)
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := guard.Close(releaseCtx); err != nil && runErr == nil {
			runErr = fmt.Errorf("release certificate rotation lease: %w", err)
		}
	}()

	if err := r.reconcile(guard.Context()); err != nil {
		if cause := context.Cause(guard.Context()); cause != nil && ctx.Err() == nil && !errors.Is(cause, context.Canceled) {
			return fmt.Errorf("certificate rotation lease lost: %w", cause)
		}
		return err
	}
	return nil
}

func (r *Rotator) reconcile(ctx context.Context) error {
	secret, err := r.client.CoreV1().Secrets(r.config.Namespace).Get(ctx, r.config.SecretName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			if !r.config.RecreateMissingSecret {
				return fmt.Errorf("generated TLS Secret %q is missing and recreation is disabled", r.config.SecretName)
			}
			return r.recreateMissingSecret(ctx)
		}
		return fmt.Errorf("get generated TLS Secret %q: %w", r.config.SecretName, err)
	}
	state, err := inspectSecret(secret, r.config, r.now())
	if err != nil {
		return fmt.Errorf("inspect generated TLS Secret %q: %w", r.config.SecretName, err)
	}

	mutatingBundles, err := r.readMutatingBundles(ctx)
	if err != nil {
		return err
	}
	validatingBundles, err := r.readValidatingBundles(ctx)
	if err != nil {
		return err
	}

	switch {
	case state.rotateCA:
		return r.rotateCA(ctx, secret, state, mutatingBundles, validatingBundles)
	case state.rotateServing:
		return r.rotateServingCertificate(ctx, secret, state)
	default:
		if state.normalizeSecret {
			if err := r.updateSecret(ctx, secret, state.current); err != nil {
				return fmt.Errorf("normalize generated TLS Secret type and material: %w", err)
			}
		}
		return r.repairTrustBundles(ctx, state.current, mutatingBundles, validatingBundles)
	}
}

func (r *Rotator) rotateCA(
	ctx context.Context,
	secret *corev1.Secret,
	state secretState,
	mutatingBundles observedCABundles,
	validatingBundles observedCABundles,
) error {
	trustedCurrentCA := []byte(nil)
	if state.currentServingChainAuthentic {
		trustedCurrentCA = state.current.caPEM
	} else if state.current.leaf != nil {
		candidateBundle, found, err := authenticServingCABundle(
			mutatingBundles,
			validatingBundles,
			state.current.leaf,
			requiredDNSNames(r.config),
		)
		if err != nil {
			return fmt.Errorf("filter authentic serving-certificate CAs: %w", err)
		}
		if found {
			if err := r.probeCertificateIdentity(ctx, candidateBundle, state.current.leaf); err != nil {
				return fmt.Errorf("prove recovered CA signs the live serving certificate: %w", err)
			}
			trustedCurrentCA = candidateBundle
		}
	}

	next, err := generateMaterial(r.random, r.now(), r.config)
	if err != nil {
		return fmt.Errorf("generate replacement CA and serving certificate: %w", err)
	}
	additions := [][]byte{next.caPEM}
	if len(trustedCurrentCA) != 0 {
		additions = append([][]byte{trustedCurrentCA}, additions...)
	}
	if err := r.publishPerEntryTransition(ctx, additions...); err != nil {
		return fmt.Errorf("publish entry-local overlapping CA trust: %w", err)
	}
	if err := r.updateSecret(ctx, secret, next); err != nil {
		return err
	}
	if err := r.probeCurrentCertificate(ctx, next); err != nil {
		return err
	}
	if err := r.setBothBundles(ctx, next.caPEM); err != nil {
		return fmt.Errorf("contract CA trust after serving-certificate proof: %w", err)
	}
	return nil
}

func (r *Rotator) recreateMissingSecret(ctx context.Context) error {
	// Validate every named webhook before creating new material. A missing or
	// retargeted entry must never be papered over by Secret recovery.
	if _, err := r.readMutatingBundles(ctx); err != nil {
		return err
	}
	if _, err := r.readValidatingBundles(ctx); err != nil {
		return err
	}
	next, err := generateMaterial(r.random, r.now(), r.config)
	if err != nil {
		return fmt.Errorf("generate replacement material for missing TLS Secret: %w", err)
	}
	desired := generatedSecret(r.config, next)
	if err := r.ensureSecretCreateGuard(ctx, desired); err != nil {
		return err
	}
	if err := r.publishPerEntryTransition(ctx, next.caPEM); err != nil {
		return fmt.Errorf("publish replacement trust before recreating TLS Secret: %w", err)
	}
	if err := r.createSecret(ctx, desired, next); err != nil {
		return err
	}
	if err := r.probeCurrentCertificate(ctx, next); err != nil {
		return err
	}
	if err := r.setBothBundles(ctx, next.caPEM); err != nil {
		return fmt.Errorf("contract CA trust after recreated Secret proof: %w", err)
	}
	return nil
}
func (r *Rotator) rotateServingCertificate(
	ctx context.Context,
	secret *corev1.Secret,
	state secretState,
) error {
	// The CA does not change. Append it independently to each entry without
	// dropping existing trust, then prove the current exact leaf before update.
	if err := r.publishPerEntryTransition(ctx, state.current.caPEM); err != nil {
		return fmt.Errorf("publish serving-certificate trust precondition: %w", err)
	}
	if state.currentServingChainAuthentic {
		if err := r.probeCertificateIdentity(ctx, state.current.caPEM, state.current.leaf); err != nil {
			return fmt.Errorf("prove current serving certificate before replacement: %w", err)
		}
	}

	next, err := generateServingMaterial(r.random, r.now(), r.config, state.current)
	if err != nil {
		return fmt.Errorf("generate replacement serving certificate: %w", err)
	}
	if err := r.updateSecret(ctx, secret, next); err != nil {
		return err
	}
	if err := r.probeCurrentCertificate(ctx, next); err != nil {
		return err
	}
	if err := r.setBothBundles(ctx, next.caPEM); err != nil {
		return fmt.Errorf("contract repaired CA trust after serving-certificate proof: %w", err)
	}
	return nil
}

func (r *Rotator) repairTrustBundles(
	ctx context.Context,
	current certificateMaterial,
	mutatingBundles observedCABundles,
	validatingBundles observedCABundles,
) error {
	if mutatingBundles.allEqual(current.caPEM) && validatingBundles.allEqual(current.caPEM) {
		return nil
	}
	// Append the Secret-authoritative CA independently to every entry while
	// retaining that entry's existing trust until exact endpoint proof.
	if err := r.publishPerEntryTransition(ctx, current.caPEM); err != nil {
		return fmt.Errorf("repair interrupted CA trust transition: %w", err)
	}
	if err := r.probeCurrentCertificate(ctx, current); err != nil {
		return err
	}
	if err := r.setBothBundles(ctx, current.caPEM); err != nil {
		return fmt.Errorf("contract interrupted CA trust transition: %w", err)
	}
	return nil
}

func (r *Rotator) updateSecret(ctx context.Context, previous *corev1.Secret, next certificateMaterial) error {
	updated := previous.DeepCopy()
	updated.Type = corev1.SecretTypeTLS
	updated.Data = cloneBytesMap(previous.Data)
	updated.Data[corev1.TLSCertKey] = append([]byte(nil), next.certPEM...)
	updated.Data[corev1.TLSPrivateKeyKey] = append([]byte(nil), next.keyPEM...)
	updated.Data[CACertificateKey] = append([]byte(nil), next.caPEM...)
	updated.Data[CAPrivateKeyKey] = append([]byte(nil), next.caKeyPEM...)

	if _, err := r.client.CoreV1().Secrets(r.config.Namespace).Update(ctx, updated, metav1.UpdateOptions{}); err == nil {
		return nil
	} else {
		// An API timeout may hide a successful write. Read back the exact four
		// fields before classifying the transition as interrupted.
		observed, getErr := r.client.CoreV1().Secrets(r.config.Namespace).Get(ctx, r.config.SecretName, metav1.GetOptions{})
		if getErr == nil && observed.Type == corev1.SecretTypeTLS && secretContainsMaterial(observed, next) {
			return nil
		}
		if getErr != nil {
			return fmt.Errorf("atomically update generated TLS Secret: %w (read-back failed: %v)", err, getErr)
		}
		return fmt.Errorf("atomically update generated TLS Secret: %w", err)
	}
}

func (r *Rotator) createSecret(ctx context.Context, desired *corev1.Secret, material certificateMaterial) error {
	created, err := r.client.CoreV1().Secrets(r.config.Namespace).Create(ctx, desired, metav1.CreateOptions{})
	if err == nil {
		if exactGeneratedSecret(created, r.config, material) {
			return nil
		}
		return errors.New("created generated TLS Secret differs from the exact recovery material")
	}

	// AlreadyExists and transport timeouts are both uncertain CREATE outcomes.
	// Accept only a byte-exact read-back of this attempt. Never update or adopt
	// different material created by another actor or racing rotator.
	observed, getErr := r.client.CoreV1().Secrets(r.config.Namespace).Get(ctx, r.config.SecretName, metav1.GetOptions{})
	if getErr == nil && exactGeneratedSecret(observed, r.config, material) {
		return nil
	}
	if getErr != nil {
		return fmt.Errorf("create missing generated TLS Secret: %w (read-back failed: %v)", err, getErr)
	}
	return fmt.Errorf("create missing generated TLS Secret: %w (read-back contains different material)", err)
}

func (r *Rotator) ensureSecretCreateGuard(ctx context.Context, desired *corev1.Secret) error {
	policy, err := r.client.AdmissionregistrationV1().ValidatingAdmissionPolicies().Get(
		ctx,
		r.config.SecretCreatePolicyName,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("get generated-Secret CREATE guard policy %q: %w", r.config.SecretCreatePolicyName, err)
	}
	if err := VerifySecretCreatePolicyContract(policy, r.config); err != nil {
		return fmt.Errorf("generated-Secret CREATE guard policy contract: %w", err)
	}
	if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
		return errors.New("generated-Secret CREATE guard policy is not established for its current generation")
	}
	if len(policy.Status.TypeChecking.ExpressionWarnings) != 0 {
		return errors.New("generated-Secret CREATE guard policy has type-checking warnings")
	}
	for _, condition := range policy.Status.Conditions {
		if condition.Status != metav1.ConditionTrue {
			return fmt.Errorf(
				"generated-Secret CREATE guard policy condition %q is not true: %s",
				condition.Type,
				condition.Status,
			)
		}
	}

	binding, err := r.client.AdmissionregistrationV1().ValidatingAdmissionPolicyBindings().Get(
		ctx,
		r.config.SecretCreatePolicyBindingName,
		metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("get generated-Secret CREATE guard binding %q: %w", r.config.SecretCreatePolicyBindingName, err)
	}
	if err := VerifySecretCreateBindingContract(binding, r.config); err != nil {
		return fmt.Errorf("generated-Secret CREATE guard binding contract: %w", err)
	}

	attacks := secretCreateGuardAttacks(desired)
	client := r.client.CoreV1().Secrets(r.config.Namespace)
	for _, attack := range attacks {
		_, err := client.Create(ctx, attack.secret, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err == nil {
			return fmt.Errorf("generated-Secret CREATE guard admitted %s", attack.name)
		}
		if !strings.Contains(err.Error(), secretCreateGuardDenialMessage) {
			return fmt.Errorf("generated-Secret CREATE guard returned an unrecognized denial for %s: %w", attack.name, err)
		}
	}
	if _, err := client.Create(ctx, desired, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}); err != nil {
		return fmt.Errorf("generated-Secret CREATE guard rejected the exact recovery object: %w", err)
	}
	return nil
}

func validateSecretCreatePolicyContract(policy *admissionregistrationv1.ValidatingAdmissionPolicy, config Config) error {
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		return errors.New("policy is not fail-closed")
	}
	if policy.Spec.ParamKind != nil || len(policy.Spec.AuditAnnotations) != 0 || len(policy.Spec.Variables) != 0 {
		return errors.New("policy contains unsupported parameters, audit annotations, or variables")
	}
	constraints := policy.Spec.MatchConstraints
	if constraints == nil || !emptyLabelSelector(constraints.NamespaceSelector) ||
		!emptyLabelSelector(constraints.ObjectSelector) ||
		len(constraints.ExcludeResourceRules) != 0 || len(constraints.ResourceRules) != 1 ||
		(constraints.MatchPolicy != nil && *constraints.MatchPolicy != admissionregistrationv1.Equivalent) {
		return errors.New("policy match constraints are not the exact Secret CREATE scope")
	}
	rule := constraints.ResourceRules[0]
	if len(rule.ResourceNames) != 0 ||
		!slices.Equal(rule.Operations, []admissionregistrationv1.OperationType{admissionregistrationv1.Create}) ||
		!slices.Equal(rule.APIGroups, []string{""}) ||
		!slices.Equal(rule.APIVersions, []string{"v1"}) ||
		!slices.Equal(rule.Resources, []string{"secrets"}) ||
		rule.Scope == nil || *rule.Scope != admissionregistrationv1.NamespacedScope {
		return errors.New("policy resource rule is not exactly namespaced core/v1 Secret CREATE")
	}
	wantIdentity := fmt.Sprintf(
		"request.userInfo.username == 'system:serviceaccount:%s:%s'",
		config.Namespace,
		config.SecretCreateServiceAccountName,
	)
	if len(policy.Spec.MatchConditions) != 1 ||
		policy.Spec.MatchConditions[0].Name != "exact-certificate-rotator-service-account" ||
		compactCEL(policy.Spec.MatchConditions[0].Expression) != compactCEL(wantIdentity) {
		return errors.New("policy does not match only the exact certificate rotator ServiceAccount")
	}
	if len(policy.Spec.Validations) != 1 {
		return errors.New("policy must contain exactly one validation")
	}
	validation := policy.Spec.Validations[0]
	if compactCEL(validation.Expression) != compactCEL(secretCreateValidationExpression(config)) ||
		validation.Message != secretCreateGuardDenialMessage || validation.Reason != nil || validation.MessageExpression != "" {
		return errors.New("policy validation is not the exact generated TLS Secret contract")
	}
	return nil
}

// VerifySecretCreatePolicyContract verifies the immutable spec of the
// generated-Secret CREATE admission policy. Ownership metadata is deliberately
// left to the caller because Helm, rather than the certificate runtime, owns
// that metadata lifecycle.
func VerifySecretCreatePolicyContract(policy *admissionregistrationv1.ValidatingAdmissionPolicy, config Config) error {
	if policy == nil {
		return errors.New("generated-Secret CREATE guard policy is nil")
	}
	return validateSecretCreatePolicyContract(policy, config)
}

func validateSecretCreateBindingContract(
	binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	config Config,
) error {
	if binding.Spec.PolicyName != config.SecretCreatePolicyName || binding.Spec.ParamRef != nil ||
		!slices.Equal(binding.Spec.ValidationActions, []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny}) {
		return errors.New("binding does not enforce only Deny for the configured policy")
	}
	resources := binding.Spec.MatchResources
	if resources == nil || resources.NamespaceSelector == nil || !emptyLabelSelector(resources.ObjectSelector) ||
		len(resources.ResourceRules) != 0 || len(resources.ExcludeResourceRules) != 0 ||
		(resources.MatchPolicy != nil && *resources.MatchPolicy != admissionregistrationv1.Equivalent) ||
		!maps.Equal(resources.NamespaceSelector.MatchLabels, map[string]string{"kubernetes.io/metadata.name": config.Namespace}) ||
		len(resources.NamespaceSelector.MatchExpressions) != 0 {
		return errors.New("binding does not select only the exact release namespace")
	}
	return nil
}

// VerifySecretCreateBindingContract verifies the immutable spec of the
// generated-Secret CREATE admission binding. Ownership metadata is
// deliberately left to the caller for the same reason as the policy helper.
func VerifySecretCreateBindingContract(
	binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	config Config,
) error {
	if binding == nil {
		return errors.New("generated-Secret CREATE guard binding is nil")
	}
	return validateSecretCreateBindingContract(binding, config)
}

func emptyLabelSelector(selector *metav1.LabelSelector) bool {
	return selector == nil || len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0
}

func secretCreateValidationExpression(config Config) string {
	return fmt.Sprintf(`
		object.metadata.name == '%s' &&
		object.metadata.namespace == '%s' &&
		(!has(object.metadata.generateName) || object.metadata.generateName == '') &&
		has(object.metadata.labels) &&
		object.metadata.labels == {'%s': '%s'} &&
		(!has(object.metadata.annotations) || object.metadata.annotations.size() == 0) &&
		(!has(object.metadata.ownerReferences) || object.metadata.ownerReferences.size() == 0) &&
		(!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) &&
		object.type == 'kubernetes.io/tls' &&
		!has(object.immutable) &&
		(!has(object.stringData) || object.stringData.size() == 0) &&
		object.data.size() == 4 &&
		'ca.crt' in object.data && object.data['ca.crt'].size() > 0 &&
		'ca.key' in object.data && object.data['ca.key'].size() > 0 &&
		'tls.crt' in object.data && object.data['tls.crt'].size() > 0 &&
		'tls.key' in object.data && object.data['tls.key'].size() > 0
	`, config.SecretName, config.Namespace, GeneratedSecretLabel, GeneratedSecretLabelValue)
}

func compactCEL(expression string) string {
	return strings.Join(strings.Fields(expression), " ")
}

type secretCreateGuardAttack struct {
	name   string
	secret *corev1.Secret
}

func secretCreateGuardAttacks(desired *corev1.Secret) []secretCreateGuardAttack {
	differentName := desired.DeepCopy()
	differentName.Name = "ptah-rotator-guard-probe"
	if differentName.Name == desired.Name {
		differentName.Name = "ptah-rotator-guard-probe-alt"
	}
	extraLabel := desired.DeepCopy()
	extraLabel.Labels["operator.ptah.dev/uncontrolled"] = "true"
	extraData := desired.DeepCopy()
	extraData.Data["uncontrolled"] = []byte("x")
	wrongType := desired.DeepCopy()
	wrongType.Type = corev1.SecretTypeOpaque
	annotation := desired.DeepCopy()
	annotation.Annotations = map[string]string{"operator.ptah.dev/uncontrolled": "true"}
	stringData := desired.DeepCopy()
	stringData.StringData = map[string]string{"uncontrolled": "x"}
	generatedName := desired.DeepCopy()
	generatedName.Name = ""
	generatedName.GenerateName = "ptah-rotator-guard-"
	missingLabel := desired.DeepCopy()
	missingLabel.Labels = nil
	ownerReference := desired.DeepCopy()
	controller := true
	ownerReference.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "v1", Kind: "ConfigMap", Name: "uncontrolled", UID: "uncontrolled", Controller: &controller,
	}}
	finalizer := desired.DeepCopy()
	finalizer.Finalizers = []string{"operator.ptah.dev/uncontrolled"}
	immutable := desired.DeepCopy()
	immutableValue := false
	immutable.Immutable = &immutableValue
	missingKey := desired.DeepCopy()
	delete(missingKey.Data, CACertificateKey)
	emptyKey := desired.DeepCopy()
	emptyKey.Data[CACertificateKey] = nil
	return []secretCreateGuardAttack{
		{name: "an unrelated Secret name", secret: differentName},
		{name: "generateName", secret: generatedName},
		{name: "a missing managed label", secret: missingLabel},
		{name: "an extra label", secret: extraLabel},
		{name: "an owner reference", secret: ownerReference},
		{name: "a finalizer", secret: finalizer},
		{name: "an explicit immutable field", secret: immutable},
		{name: "an extra data field", secret: extraData},
		{name: "a missing data field", secret: missingKey},
		{name: "an empty data field", secret: emptyKey},
		{name: "a non-TLS type", secret: wrongType},
		{name: "an annotation", secret: annotation},
		{name: "stringData", secret: stringData},
	}
}

func generatedSecret(config Config, material certificateMaterial) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.SecretName,
			Namespace: config.Namespace,
			Labels:    map[string]string{GeneratedSecretLabel: GeneratedSecretLabelValue},
		},
		Type: corev1.SecretTypeTLS,
		Data: map[string][]byte{
			CACertificateKey:        append([]byte(nil), material.caPEM...),
			CAPrivateKeyKey:         append([]byte(nil), material.caKeyPEM...),
			corev1.TLSCertKey:       append([]byte(nil), material.certPEM...),
			corev1.TLSPrivateKeyKey: append([]byte(nil), material.keyPEM...),
		},
	}
}

func exactGeneratedSecret(secret *corev1.Secret, config Config, material certificateMaterial) bool {
	return secret != nil &&
		secret.Name == config.SecretName &&
		secret.Namespace == config.Namespace &&
		secret.Type == corev1.SecretTypeTLS &&
		maps.Equal(secret.Labels, map[string]string{GeneratedSecretLabel: GeneratedSecretLabelValue}) &&
		len(secret.Annotations) == 0 &&
		len(secret.OwnerReferences) == 0 &&
		len(secret.Finalizers) == 0 &&
		secret.Immutable == nil &&
		len(secret.StringData) == 0 &&
		len(secret.Data) == 4 &&
		secretContainsMaterial(secret, material)
}

func (r *Rotator) probeCurrentCertificate(ctx context.Context, material certificateMaterial) error {
	return r.probeCertificate(ctx, material.caPEM, material.leaf, false)
}

func (r *Rotator) probeCertificateIdentity(ctx context.Context, caBundle []byte, leaf *x509.Certificate) error {
	return r.probeCertificate(ctx, caBundle, leaf, true)
}

func (r *Rotator) probeCertificate(
	ctx context.Context,
	caBundle []byte,
	leaf *x509.Certificate,
	identityOnly bool,
) error {
	probeCtx, cancel := context.WithTimeout(ctx, r.config.ProbeTimeout)
	defer cancel()
	ticker := time.NewTicker(r.config.ProbeInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		before, err := r.endpointSnapshot(probeCtx)
		if err == nil {
			for _, endpoint := range before {
				err = r.probe.Probe(probeCtx, probeRequest{
					Address:          net.JoinHostPort(endpoint.address, fmt.Sprintf("%d", endpoint.port)),
					ServerName:       fmt.Sprintf("%s.%s.svc", r.config.ServiceName, r.config.ServiceNamespace),
					CACertificatePEM: caBundle,
					LeafCertificate:  leaf,
					IdentityOnly:     identityOnly,
				})
				if err != nil {
					break
				}
			}
		}
		if err == nil {
			after, snapshotErr := r.endpointSnapshot(probeCtx)
			if snapshotErr != nil {
				err = snapshotErr
			} else if !slices.Equal(before, after) {
				err = errors.New("webhook endpoint set changed while certificates were probed")
			} else {
				return nil
			}
		}
		lastErr = err
		select {
		case <-probeCtx.Done():
			return fmt.Errorf("prove every stable webhook endpoint serves the replacement certificate: %w (last attempt: %v)", probeCtx.Err(), lastErr)
		case <-ticker.C:
		}
	}
}

func (r *Rotator) setBothBundles(ctx context.Context, bundle []byte) error {
	if err := r.setMutatingBundle(ctx, bundle); err != nil {
		return err
	}
	if err := r.setValidatingBundle(ctx, bundle); err != nil {
		return err
	}
	mutating, err := r.readMutatingBundles(ctx)
	if err != nil {
		return fmt.Errorf("verify managed mutating CA bundles: %w", err)
	}
	validating, err := r.readValidatingBundles(ctx)
	if err != nil {
		return fmt.Errorf("verify managed validating CA bundles: %w", err)
	}
	if !mutating.allEqual(bundle) || !validating.allEqual(bundle) {
		return errors.New("not every exact-Service webhook contains the published CA bundle")
	}
	return nil
}

// publishPerEntryTransition appends authenticated additions to each exact
// managed entry's own trust. Parseable certificates survive malformed
// neighbors, trust is never copied between entries, and an entry with no
// parseable certificate falls back to the authenticated additions alone.
func (r *Rotator) publishPerEntryTransition(ctx context.Context, additions ...[]byte) error {
	if len(additions) == 0 {
		return errors.New("at least one CA transition addition is required")
	}
	if err := r.setMutatingPerEntryTransition(ctx, additions); err != nil {
		return err
	}
	if err := r.setValidatingPerEntryTransition(ctx, additions); err != nil {
		return err
	}
	mutating, err := r.readMutatingBundles(ctx)
	if err != nil {
		return fmt.Errorf("verify mutating per-entry CA transition: %w", err)
	}
	validating, err := r.readValidatingBundles(ctx)
	if err != nil {
		return fmt.Errorf("verify validating per-entry CA transition: %w", err)
	}
	for _, observed := range []observedCABundles{mutating, validating} {
		if observed.invalidCount != 0 || len(observed.valid) != observed.total {
			return errors.New("per-entry CA transition left a malformed managed bundle")
		}
		for _, bundle := range observed.valid {
			for _, addition := range additions {
				if !caBundleContainsAllCertificates(bundle, addition) {
					return errors.New("per-entry CA transition did not publish every authenticated CA addition")
				}
			}
		}
	}
	return nil
}

func (r *Rotator) setMutatingPerEntryTransition(ctx context.Context, additions [][]byte) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		client := r.client.AdmissionregistrationV1().MutatingWebhookConfigurations()
		configuration, err := client.Get(ctx, r.config.MutatingWebhookConfiguration, metav1.GetOptions{})
		if err != nil {
			return err
		}
		webhooks, err := managedMutatingWebhooks(configuration.Webhooks, r.config)
		if err != nil {
			return err
		}
		changed := false
		for _, webhook := range webhooks {
			transition, err := perEntryTransitionBundle(webhook.ClientConfig.CABundle, additions...)
			if err != nil {
				return err
			}
			if bytes.Equal(webhook.ClientConfig.CABundle, transition) {
				continue
			}
			webhook.ClientConfig.CABundle = transition
			changed = true
		}
		if !changed {
			return nil
		}
		_, err = client.Update(ctx, configuration, metav1.UpdateOptions{})
		return err
	})
}

func (r *Rotator) setValidatingPerEntryTransition(ctx context.Context, additions [][]byte) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		client := r.client.AdmissionregistrationV1().ValidatingWebhookConfigurations()
		configuration, err := client.Get(ctx, r.config.ValidatingWebhookConfiguration, metav1.GetOptions{})
		if err != nil {
			return err
		}
		webhooks, err := managedValidatingWebhooks(configuration.Webhooks, r.config)
		if err != nil {
			return err
		}
		changed := false
		for _, webhook := range webhooks {
			transition, err := perEntryTransitionBundle(webhook.ClientConfig.CABundle, additions...)
			if err != nil {
				return err
			}
			if bytes.Equal(webhook.ClientConfig.CABundle, transition) {
				continue
			}
			webhook.ClientConfig.CABundle = transition
			changed = true
		}
		if !changed {
			return nil
		}
		_, err = client.Update(ctx, configuration, metav1.UpdateOptions{})
		return err
	})
}

func perEntryTransitionBundle(existing []byte, additions ...[]byte) ([]byte, error) {
	preserved := existing
	if _, err := parseCertificateBundle(existing); err != nil {
		candidates := certificateCandidates(existing)
		if len(candidates) == 0 {
			return combineCABundles(additions...)
		}
		preserved, err = encodeCertificateBundle(candidates)
		if err != nil {
			return nil, err
		}
	}
	return combineCABundles(append([][]byte{preserved}, additions...)...)
}

func (r *Rotator) readMutatingBundles(ctx context.Context) (observedCABundles, error) {
	configuration, err := r.client.AdmissionregistrationV1().MutatingWebhookConfigurations().Get(
		ctx, r.config.MutatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		return observedCABundles{}, fmt.Errorf("get MutatingWebhookConfiguration %q: %w", r.config.MutatingWebhookConfiguration, err)
	}
	webhooks, err := managedMutatingWebhooks(configuration.Webhooks, r.config)
	if err != nil {
		return observedCABundles{}, fmt.Errorf("inspect MutatingWebhookConfiguration %q: %w", r.config.MutatingWebhookConfiguration, err)
	}
	bundles := make([][]byte, 0, len(webhooks))
	for _, webhook := range webhooks {
		bundles = append(bundles, webhook.ClientConfig.CABundle)
	}
	return observeCABundles(bundles...), nil
}

func (r *Rotator) readValidatingBundles(ctx context.Context) (observedCABundles, error) {
	configuration, err := r.client.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(
		ctx, r.config.ValidatingWebhookConfiguration, metav1.GetOptions{},
	)
	if err != nil {
		return observedCABundles{}, fmt.Errorf("get ValidatingWebhookConfiguration %q: %w", r.config.ValidatingWebhookConfiguration, err)
	}
	webhooks, err := managedValidatingWebhooks(configuration.Webhooks, r.config)
	if err != nil {
		return observedCABundles{}, fmt.Errorf("inspect ValidatingWebhookConfiguration %q: %w", r.config.ValidatingWebhookConfiguration, err)
	}
	bundles := make([][]byte, 0, len(webhooks))
	for _, webhook := range webhooks {
		bundles = append(bundles, webhook.ClientConfig.CABundle)
	}
	return observeCABundles(bundles...), nil
}

func (r *Rotator) setMutatingBundle(ctx context.Context, bundle []byte) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		client := r.client.AdmissionregistrationV1().MutatingWebhookConfigurations()
		configuration, err := client.Get(ctx, r.config.MutatingWebhookConfiguration, metav1.GetOptions{})
		if err != nil {
			return err
		}
		webhooks, err := managedMutatingWebhooks(configuration.Webhooks, r.config)
		if err != nil {
			return err
		}
		changed := false
		for _, webhook := range webhooks {
			if bytes.Equal(webhook.ClientConfig.CABundle, bundle) {
				continue
			}
			webhook.ClientConfig.CABundle = append([]byte(nil), bundle...)
			changed = true
		}
		if !changed {
			return nil
		}
		_, err = client.Update(ctx, configuration, metav1.UpdateOptions{})
		return err
	})
}

func (r *Rotator) setValidatingBundle(ctx context.Context, bundle []byte) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		client := r.client.AdmissionregistrationV1().ValidatingWebhookConfigurations()
		configuration, err := client.Get(ctx, r.config.ValidatingWebhookConfiguration, metav1.GetOptions{})
		if err != nil {
			return err
		}
		webhooks, err := managedValidatingWebhooks(configuration.Webhooks, r.config)
		if err != nil {
			return err
		}
		changed := false
		for _, webhook := range webhooks {
			if bytes.Equal(webhook.ClientConfig.CABundle, bundle) {
				continue
			}
			webhook.ClientConfig.CABundle = append([]byte(nil), bundle...)
			changed = true
		}
		if !changed {
			return nil
		}
		_, err = client.Update(ctx, configuration, metav1.UpdateOptions{})
		return err
	})
}

func validateConfig(config Config) error {
	for label, value := range map[string]string{
		"Secret name":                         config.SecretName,
		"Lease name":                          config.LeaseName,
		"MutatingWebhookConfiguration name":   config.MutatingWebhookConfiguration,
		"ValidatingWebhookConfiguration name": config.ValidatingWebhookConfiguration,
	} {
		if problems := validation.IsDNS1123Subdomain(value); len(problems) != 0 {
			return fmt.Errorf("%s is invalid: %s", label, problems[0])
		}
	}
	for label, value := range map[string]string{
		"namespace":         config.Namespace,
		"service name":      config.ServiceName,
		"service namespace": config.ServiceNamespace,
	} {
		if problems := validation.IsDNS1123Label(value); len(problems) != 0 {
			return fmt.Errorf("%s is invalid: %s", label, problems[0])
		}
	}
	if config.RecreateMissingSecret {
		for label, value := range map[string]string{
			"Secret CREATE policy name":         config.SecretCreatePolicyName,
			"Secret CREATE policy binding name": config.SecretCreatePolicyBindingName,
		} {
			if problems := validation.IsDNS1123Subdomain(value); len(problems) != 0 {
				return fmt.Errorf("%s is invalid: %s", label, problems[0])
			}
		}
		if problems := validation.IsDNS1123Label(config.SecretCreateServiceAccountName); len(problems) != 0 {
			return fmt.Errorf("Secret CREATE ServiceAccount name is invalid: %s", problems[0])
		}
	} else if config.SecretCreatePolicyName != "" || config.SecretCreatePolicyBindingName != "" ||
		config.SecretCreateServiceAccountName != "" {
		return errors.New("Secret recreation guard names must be empty when missing-Secret recreation is disabled")
	}
	if err := validateWebhookNames("mutating", config.MutatingWebhookNames); err != nil {
		return err
	}
	if err := validateWebhookNames("validating", config.ValidatingWebhookNames); err != nil {
		return err
	}
	if problems := validation.IsValidPortName(config.EndpointPortName); len(problems) != 0 {
		return fmt.Errorf("EndpointSlice port name is invalid: %s", problems[0])
	}
	if config.HolderIdentity == "" || len(config.HolderIdentity) > 128 {
		return errors.New("holder identity must contain 1 to 128 characters")
	}
	if config.RenewalThreshold <= 0 {
		return errors.New("renewal threshold must be positive")
	}
	if config.ServingCertificateValidity <= config.RenewalThreshold || config.ServingCertificateValidity > maximumValidity {
		return errors.New("serving certificate validity must exceed the renewal threshold and be at most 20 years")
	}
	if config.CACertificateValidity <= config.ServingCertificateValidity || config.CACertificateValidity > maximumValidity {
		return errors.New("CA certificate validity must exceed serving certificate validity and be at most 20 years")
	}
	if config.ProbeInterval <= 0 || config.ProbeTimeout <= config.ProbeInterval || config.ProbeTimeout > maximumOperationTime {
		return errors.New("probe timeout must exceed the positive probe interval and be at most 24 hours")
	}
	if config.LeaseDuration < minimumLeaseDuration || config.LeaseDuration > maximumOperationTime {
		return errors.New("Lease duration must be between 30 seconds and 24 hours")
	}
	if config.AcquireTimeout <= 0 || config.AcquireTimeout > maximumOperationTime {
		return errors.New("Lease acquire timeout must be positive and at most 24 hours")
	}
	return nil
}

// managedMutatingWebhooks treats configured names as required identity
// anchors, then includes every entry targeting the exact release Service and
// its supported port. This lets an already-running predecessor carry a
// same-Service entry observed in a narrow partial-apply race. It does not make a
// quiesced predecessor restartable after candidate activation; recovery then
// retries the same candidate. URL, foreign-Service, and other-port entries
// remain outside this rotator's authority.
func managedMutatingWebhooks(webhooks []admissionregistrationv1.MutatingWebhook, config Config) ([]*admissionregistrationv1.MutatingWebhook, error) {
	expected := make(map[string]struct{}, len(config.MutatingWebhookNames))
	for _, name := range config.MutatingWebhookNames {
		expected[name] = struct{}{}
	}
	found := make(map[string]struct{}, len(expected))
	seen := make(map[string]struct{}, len(webhooks))
	managed := make([]*admissionregistrationv1.MutatingWebhook, 0, len(webhooks))
	for i := range webhooks {
		webhook := &webhooks[i]
		if _, duplicate := seen[webhook.Name]; duplicate {
			return nil, fmt.Errorf("webhook %q appears more than once", webhook.Name)
		}
		seen[webhook.Name] = struct{}{}
		_, required := expected[webhook.Name]
		targetsService := webhookTargetsService(webhook.ClientConfig, config)
		if required && !targetsService {
			return nil, fmt.Errorf("webhook %q does not target the configured Service", webhook.Name)
		}
		if required {
			found[webhook.Name] = struct{}{}
		}
		if targetsService {
			managed = append(managed, webhook)
		}
	}
	if missing := missingMapKeys(expected, found); len(missing) != 0 {
		return nil, fmt.Errorf("required mutating webhooks were not found: %v", missing)
	}
	return managed, nil
}

// managedValidatingWebhooks treats configured names as required identity
// anchors, then includes every entry targeting the exact release Service and
// its supported port. This lets an already-running predecessor carry a
// same-Service entry observed in a narrow partial-apply race. It does not make a
// quiesced predecessor restartable after candidate activation; recovery then
// retries the same candidate. URL, foreign-Service, and other-port entries
// remain outside this rotator's authority.
func managedValidatingWebhooks(webhooks []admissionregistrationv1.ValidatingWebhook, config Config) ([]*admissionregistrationv1.ValidatingWebhook, error) {
	expected := make(map[string]struct{}, len(config.ValidatingWebhookNames))
	for _, name := range config.ValidatingWebhookNames {
		expected[name] = struct{}{}
	}
	found := make(map[string]struct{}, len(expected))
	seen := make(map[string]struct{}, len(webhooks))
	managed := make([]*admissionregistrationv1.ValidatingWebhook, 0, len(webhooks))
	for i := range webhooks {
		webhook := &webhooks[i]
		if _, duplicate := seen[webhook.Name]; duplicate {
			return nil, fmt.Errorf("webhook %q appears more than once", webhook.Name)
		}
		seen[webhook.Name] = struct{}{}
		_, required := expected[webhook.Name]
		targetsService := webhookTargetsService(webhook.ClientConfig, config)
		if required && !targetsService {
			return nil, fmt.Errorf("webhook %q does not target the configured Service", webhook.Name)
		}
		if required {
			found[webhook.Name] = struct{}{}
		}
		if targetsService {
			managed = append(managed, webhook)
		}
	}
	if missing := missingMapKeys(expected, found); len(missing) != 0 {
		return nil, fmt.Errorf("required validating webhooks were not found: %v", missing)
	}
	return managed, nil
}

func webhookTargetsService(clientConfig admissionregistrationv1.WebhookClientConfig, config Config) bool {
	return clientConfig.Service != nil &&
		clientConfig.Service.Name == config.ServiceName &&
		clientConfig.Service.Namespace == config.ServiceNamespace &&
		effectiveWebhookServicePort(clientConfig.Service) == webhookServicePort
}

func effectiveWebhookServicePort(service *admissionregistrationv1.ServiceReference) int32 {
	if service.Port == nil {
		return webhookServicePort
	}
	return *service.Port
}

func validateWebhookNames(kind string, names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("at least one %s webhook name is required", kind)
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if problems := validation.IsDNS1123Subdomain(name); len(problems) != 0 {
			return fmt.Errorf("%s webhook name %q is invalid: %s", kind, name, problems[0])
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%s webhook name %q appears more than once", kind, name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func missingMapKeys(expected, found map[string]struct{}) []string {
	keys := make([]string, 0, len(expected)-len(found))
	for key := range expected {
		if _, ok := found[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	return keys
}

func cloneBytesMap(source map[string][]byte) map[string][]byte {
	cloned := make(map[string][]byte, len(source)+4)
	for key, value := range source {
		cloned[key] = append([]byte(nil), value...)
	}
	return cloned
}

func secretContainsMaterial(secret *corev1.Secret, material certificateMaterial) bool {
	return bytes.Equal(secret.Data[corev1.TLSCertKey], material.certPEM) &&
		bytes.Equal(secret.Data[corev1.TLSPrivateKeyKey], material.keyPEM) &&
		bytes.Equal(secret.Data[CACertificateKey], material.caPEM) &&
		bytes.Equal(secret.Data[CAPrivateKeyKey], material.caKeyPEM)
}

func certificateRawEqual(left, right *x509.Certificate) bool {
	return left != nil && right != nil && bytes.Equal(left.Raw, right.Raw)
}
