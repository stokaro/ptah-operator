package crdupgrade

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	certificateMutatingWriteGuardNamePrefix   = "ptah-operator-certificate-mutate-guard-v1-"
	certificateValidatingWriteGuardNamePrefix = "ptah-operator-certificate-validate-guard-v1-"
	certificateMutatingWriteGuardComponent    = "certificate-mutating-write-guard"
	certificateValidatingWriteGuardComponent  = "certificate-validating-write-guard"
	certificateMutatingWritePolicyWeight      = "-156"
	certificateMutatingWriteBindingWeight     = "-155"
	certificateValidatingWritePolicyWeight    = "-154"
	certificateValidatingWriteBindingWeight   = "-153"
	maximumCertificateCABundleBytes           = 256 * 1024
	certificateWebhookServicePort             = 443
)

// CertificateMutatingWriteGuardPolicyName returns the stable, versioned name
// of the certificate identity's MutatingWebhookConfiguration write boundary.
func CertificateMutatingWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return certificateWriteGuardPolicyName(certificateMutatingWriteGuardNamePrefix, releaseNamespace, releaseName)
}

// CertificateValidatingWriteGuardPolicyName returns the stable, versioned
// name of the certificate identity's ValidatingWebhookConfiguration boundary.
func CertificateValidatingWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return certificateWriteGuardPolicyName(certificateValidatingWriteGuardNamePrefix, releaseNamespace, releaseName)
}

func certificateWriteGuardPolicyName(prefix, releaseNamespace, releaseName string) string {
	identity := releaseNamespace + "\n" + releaseName
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return prefix + digest[:12]
}

func certificateMutatingWriteGuardDenialMessage() string {
	return "Ptah certificate mutating write guard rejected an unsafe mutation"
}

func certificateValidatingWriteGuardDenialMessage() string {
	return "Ptah certificate validating write guard rejected an unsafe mutation"
}

func certificateMutatingWebhookNames() []string {
	return []string{mutatingApprovalWebhookName, mutatingCertificateCanaryWebhookName}
}

// certificateValidatingWebhookNames is the single ordered admission-entry
// inventory used by the write boundary. Appending a new release-owned entry
// therefore changes one explicit contract instead of duplicating cardinality
// and index assumptions across the policy builder.
func certificateValidatingWebhookNames() []string {
	return []string{
		validatingApprovalWebhookName,
		podIntentWebhookName,
		controllerWriteWebhookName,
		validatingCertificateCanaryWebhookName,
	}
}

type certificateWriteGuardEntry struct {
	name                string
	component           string
	resource            string
	policyWeight        string
	bindingWeight       string
	denialMessage       string
	includeReinvocation bool
	canaryName          string
}

// CertificateWriteGuard confines the certificate ServiceAccount's admission
// singleton updates to bounded, nonempty CA bundles on entries targeting the
// exact release Service and on the one typed canary entry targeting the exact
// candidate Service, all on effective port 443. Every other entry's bundle and
// all behavioral and caller-controlled fields remain immutable.
// Kubernetes field management may still rewrite or reset
// metadata.managedFields before validating admission; that unavoidable
// bookkeeping exception carries no webhook behavior. The mutating and
// validating resources deliberately use distinct policies so each CEL program
// is checked against one concrete Kubernetes API type.
type CertificateWriteGuard struct {
	Policies                      ValidatingAdmissionPolicyReader
	Bindings                      ValidatingAdmissionPolicyBindingReader
	ReleaseName                   string
	ReleaseNamespace              string
	WebhookServiceName            string
	CertificateServiceAccountName string
	PollEvery                     time.Duration
}

// NewCertificateWriteGuard copies the stable release and certificate identity
// from the rollout contract.
func NewCertificateWriteGuard(rollout *RolloutGuard) *CertificateWriteGuard {
	if rollout == nil {
		return nil
	}
	return &CertificateWriteGuard{
		Policies:                      rollout.Policies,
		Bindings:                      rollout.Bindings,
		ReleaseName:                   rollout.ReleaseName,
		ReleaseNamespace:              rollout.ReleaseNamespace,
		WebhookServiceName:            rollout.WebhookServiceName,
		CertificateServiceAccountName: rollout.CertificateDeploymentName,
		PollEvery:                     rollout.PollEvery,
	}
}

// Verify requires both retained policy/binding pairs to match the compiled
// parameterless write contract exactly.
func (g *CertificateWriteGuard) Verify(ctx context.Context) error {
	if err := g.validate(false); err != nil {
		return err
	}
	for _, entry := range g.entries() {
		policy, err := g.Policies.Get(ctx, entry.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s policy: %w", entry.component, err)
		}
		if err := g.verifyPolicy(entry, policy); err != nil {
			return err
		}
		binding, err := g.Bindings.Get(ctx, entry.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s binding: %w", entry.component, err)
		}
		if err := g.verifyBinding(entry, binding); err != nil {
			return err
		}
	}
	return nil
}

// WaitReady verifies both immutable contracts, then waits for observed
// generation and warning-free CEL type checking before privileged workloads
// can be created.
func (g *CertificateWriteGuard) WaitReady(ctx context.Context) error {
	if err := g.validate(true); err != nil {
		return err
	}
	if err := g.Verify(ctx); err != nil {
		return err
	}
	for _, entry := range g.entries() {
		entry := entry
		if err := wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
			policy, err := g.Policies.Get(pollCtx, entry.name, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Errorf("read %s policy status: %w", entry.component, err)
			}
			if err := g.verifyPolicy(entry, policy); err != nil {
				return false, err
			}
			if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
				return false, nil
			}
			if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) != 0 {
				return false, fmt.Errorf("%s policy has CEL type-check warnings: %s", entry.component, warnings[0].Warning)
			}
			return true, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (g *CertificateWriteGuard) entries() []certificateWriteGuardEntry {
	return []certificateWriteGuardEntry{
		{
			name:                CertificateMutatingWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName),
			component:           certificateMutatingWriteGuardComponent,
			resource:            "mutatingwebhookconfigurations",
			policyWeight:        certificateMutatingWritePolicyWeight,
			bindingWeight:       certificateMutatingWriteBindingWeight,
			denialMessage:       certificateMutatingWriteGuardDenialMessage(),
			includeReinvocation: true,
			canaryName:          mutatingCertificateCanaryWebhookName,
		},
		{
			name:          CertificateValidatingWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName),
			component:     certificateValidatingWriteGuardComponent,
			resource:      "validatingwebhookconfigurations",
			policyWeight:  certificateValidatingWritePolicyWeight,
			bindingWeight: certificateValidatingWriteBindingWeight,
			denialMessage: certificateValidatingWriteGuardDenialMessage(),
			canaryName:    validatingCertificateCanaryWebhookName,
		},
	}
}

func (g *CertificateWriteGuard) policy(entry certificateWriteGuardEntry) *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	username := "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.CertificateServiceAccountName
	validations := []admissionregistrationv1.Validation{
		{Expression: certificateMetadataValidation(), Message: entry.denialMessage},
		{Expression: certificateWebhookNamesValidation(), Message: entry.denialMessage},
		{Expression: certificateWebhookEntriesValidation(
			g.ReleaseNamespace,
			g.WebhookServiceName,
			g.candidateServiceName(),
			entry.canaryName,
			entry.includeReinvocation,
		), Message: entry.denialMessage},
	}
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(entry),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(entry.resource),
			// The certificate principal's own ConfigMap writes, the canary
			// marker among them, are judged by the admission canary webhooks
			// and its Role; these validations are written for the webhook
			// configurations and error on any other kind.
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "exact-certificate-service-account",
				Expression: fmt.Sprintf(`request.userInfo.username == %q && request.resource.group == "admissionregistration.k8s.io"`, username),
			}},
			Validations: validations,
		},
	}
	addStableAdmissionConvergenceDependencyProbe(
		policy,
		g.ReleaseNamespace,
		serviceAccountObjectGuardMarkerPattern(g.ReleaseNamespace, g.ReleaseName),
	)
	return policy
}

func (g *CertificateWriteGuard) binding(entry certificateWriteGuardEntry) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(entry),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        entry.name,
			MatchResources:    g.matchResources(entry.resource),
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
	addAdmissionConvergenceProbeMatchResource(binding.Spec.MatchResources, "")
	return binding
}

func (g *CertificateWriteGuard) matchResources(resource string) *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &exact,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{"admissionregistration.k8s.io"},
					APIVersions: []string{"v1"},
					Resources:   []string{resource},
					Scope:       scopePtr(admissionregistrationv1.ClusterScope),
				},
			},
			ResourceNames: []string{AdmissionConfigurationName},
		}},
	}
}

func (g *CertificateWriteGuard) metadata(entry certificateWriteGuardEntry) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: entry.name,
		Annotations: map[string]string{
			rolloutGuardVersionAnnotation: rolloutGuardVersion,
			ReleaseNameAnnotation:         g.ReleaseName,
			ReleaseNamespaceAnnotation:    g.ReleaseNamespace,
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.ReleaseName,
			"app.kubernetes.io/component": entry.component,
		},
	}
}

func (g *CertificateWriteGuard) verifyPolicy(entry certificateWriteGuardEntry, policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	if policy == nil || policy.Name != entry.name {
		return fmt.Errorf("fixed %s policy %s is missing", entry.component, entry.name)
	}
	if err := g.verifyMetadata(entry, "ValidatingAdmissionPolicy", policy.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(policy.Spec, g.policy(entry).Spec) {
		return fmt.Errorf("%s policy spec differs from the immutable contract", entry.component)
	}
	return nil
}

func (g *CertificateWriteGuard) verifyBinding(entry certificateWriteGuardEntry, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	if binding == nil || binding.Name != entry.name {
		return fmt.Errorf("fixed %s binding %s is missing", entry.component, entry.name)
	}
	if err := g.verifyMetadata(entry, "ValidatingAdmissionPolicyBinding", binding.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(binding.Spec, g.binding(entry).Spec) {
		return fmt.Errorf("%s binding spec differs from the immutable contract", entry.component)
	}
	return nil
}

func (g *CertificateWriteGuard) verifyMetadata(entry certificateWriteGuardEntry, kind string, metadata metav1.ObjectMeta) error {
	expected := g.metadata(entry)
	if metadata.Name != expected.Name {
		return fmt.Errorf("fixed certificate write guard %s has an unexpected name", kind)
	}
	for key, value := range expected.Annotations {
		if metadata.Annotations[key] != value {
			return fmt.Errorf("fixed certificate write guard %s has foreign or incomplete ownership", kind)
		}
	}
	for key, value := range expected.Labels {
		if metadata.Labels[key] != value {
			return fmt.Errorf("fixed certificate write guard %s has foreign or incomplete ownership", kind)
		}
	}
	return nil
}

func (g *CertificateWriteGuard) validate(requirePoll bool) error {
	if g == nil || g.Policies == nil || g.Bindings == nil {
		return fmt.Errorf("certificate write guard policy clients are required")
	}
	for description, value := range map[string]string{
		"release name":                        g.ReleaseName,
		"release namespace":                   g.ReleaseNamespace,
		"webhook Service name":                g.WebhookServiceName,
		"certificate ServiceAccount identity": g.CertificateServiceAccountName,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("certificate write guard %s is required and must not contain surrounding whitespace", description)
		}
	}
	if requirePoll && g.PollEvery <= 0 {
		return fmt.Errorf("certificate write guard poll interval must be positive")
	}
	if _, _, err := deriveCertificateCanaryNames(g.CertificateServiceAccountName, g.WebhookServiceName); err != nil {
		return fmt.Errorf("certificate write guard canary identity: %w", err)
	}
	return nil
}

func (g *CertificateWriteGuard) candidateServiceName() string {
	name, _, _ := deriveCertificateCanaryNames(g.CertificateServiceAccountName, g.WebhookServiceName)
	return name
}

func certificateWebhookNamesValidation() string {
	return `dyn(object).webhooks.size() > 0 && dyn(object).webhooks.size() <= 64 && dyn(object).webhooks.map(webhook, webhook.name) == dyn(oldObject).webhooks.map(webhook, webhook.name)`
}

func certificateMetadataValidation() string {
	parts := make([]string, 0, 13)
	for _, field := range []string{
		"name",
		"generateName",
		"namespace",
		"selfLink",
		"uid",
		"resourceVersion",
		"creationTimestamp",
		"deletionTimestamp",
		"deletionGracePeriodSeconds",
		"labels",
		"annotations",
		"ownerReferences",
		"finalizers",
	} {
		parts = append(parts, certificatePresenceEqual("object.metadata."+field, "oldObject.metadata."+field))
	}
	// managedFields is deliberately absent: the API field manager may rewrite it
	// before validating admission, and may turn a caller-requested reset into the
	// same representation. Generation is likewise server-maintained, so bind its
	// only permitted transition to an actual webhook-list change.
	parts = append(parts, `((dyn(object).webhooks == dyn(oldObject).webhooks && object.metadata.generation == oldObject.metadata.generation) || (dyn(object).webhooks != dyn(oldObject).webhooks && object.metadata.generation == oldObject.metadata.generation + 1))`)
	return strings.Join(parts, " && ")
}

func certificateWebhookEntriesValidation(
	serviceNamespace,
	serviceName,
	candidateServiceName,
	canaryName string,
	includeReinvocation bool,
) string {
	newWebhook := "webhook"
	oldWebhook := "previous"
	exactServiceTarget := fmt.Sprintf(
		`has(%[1]s.clientConfig.service) && %[1]s.clientConfig.service.namespace == %[2]q && %[1]s.clientConfig.service.name == %[3]q && (!has(%[1]s.clientConfig.service.port) || %[1]s.clientConfig.service.port == %[4]d)`,
		newWebhook,
		serviceNamespace,
		serviceName,
		certificateWebhookServicePort,
	)
	exactCanaryTarget := fmt.Sprintf(
		`%[1]s.name == %[2]q && has(%[1]s.clientConfig.service) && %[1]s.clientConfig.service.namespace == %[3]q && %[1]s.clientConfig.service.name == %[4]q && (!has(%[1]s.clientConfig.service.port) || %[1]s.clientConfig.service.port == %[5]d)`,
		newWebhook,
		canaryName,
		serviceNamespace,
		candidateServiceName,
		certificateWebhookServicePort,
	)
	mutableTarget := fmt.Sprintf(`((%s) || (%s))`, exactServiceTarget, exactCanaryTarget)
	// The alternative is parenthesized as a whole. CEL binds && tighter than
	// ||, so an unparenthesized alternative in a conjunction splits the whole
	// chain: every field comparison after it would sit in the right branch and
	// be skipped whenever the left one holds, which let a bounded CA-bundle
	// write carry any other webhook change with it.
	mutableCABundle := fmt.Sprintf(
		`(((%[1]s) && has(%[2]s.clientConfig.caBundle) && %[2]s.clientConfig.caBundle.size() > 0 && %[2]s.clientConfig.caBundle.size() <= %[4]d) || (!(%[1]s) && %[3]s))`,
		mutableTarget,
		newWebhook,
		certificatePresenceEqual(newWebhook+".clientConfig.caBundle", oldWebhook+".clientConfig.caBundle"),
		maximumCertificateCABundleBytes,
	)
	parts := []string{
		oldWebhook + ".name == " + newWebhook + ".name",
		certificatePresenceEqual(newWebhook+".clientConfig.service", oldWebhook+".clientConfig.service"),
		certificatePresenceEqual(newWebhook+".clientConfig.url", oldWebhook+".clientConfig.url"),
		mutableCABundle,
	}
	for _, field := range []string{
		"rules",
		"failurePolicy",
		"matchPolicy",
		"namespaceSelector",
		"objectSelector",
		"sideEffects",
		"timeoutSeconds",
		"admissionReviewVersions",
		"matchConditions",
	} {
		parts = append(parts, certificatePresenceEqual(newWebhook+"."+field, oldWebhook+"."+field))
	}
	if includeReinvocation {
		parts = append(parts, certificatePresenceEqual(newWebhook+".reinvocationPolicy", oldWebhook+".reinvocationPolicy"))
	}
	return fmt.Sprintf(
		`dyn(object).webhooks.all(webhook, dyn(oldObject).webhooks.exists(previous, %s))`,
		strings.Join(parts, " && "),
	)
}

func certificatePresenceEqual(newPath, oldPath string) string {
	return fmt.Sprintf(`has(%[1]s) == has(%[2]s) && (!has(%[1]s) || (has(%[2]s) && %[1]s == %[2]s))`, newPath, oldPath)
}
