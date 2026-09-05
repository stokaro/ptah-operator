package crdupgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	serviceAccountObjectGuardNamePrefix = "ptah-operator-service-account-object-guard-v1-"
	serviceAccountObjectGuardComponent  = "service-account-object-guard"
	serviceAccountObjectPolicyWeight    = "-164"
	serviceAccountObjectBindingWeight   = "-163"
	serviceAccountObjectContractVersion = "2"

	serviceAccountObjectProbeFieldManagerPrefix  = "ptah-service-account-object-probe-v1-"
	serviceAccountObjectProbeDenialMessagePrefix = "Ptah admission convergence confirmed exact service account object guard "
)

var (
	managedControllerServiceAccountPattern  = regexp.MustCompile(`^(.*)-v([1-9][0-9]*)-[0-9a-f]{12}$`)
	externalControllerServiceAccountPattern = regexp.MustCompile(`^(.*)-v([1-9][0-9]*)$`)
)

// ServiceAccountObjectIdentityContract is the immutable naming input shared
// by the release-stable ServiceAccount object guard and its readiness marker.
// The current candidate names are deliberately excluded.
type ServiceAccountObjectIdentityContract struct {
	ControllerServiceAccountBase    string
	ControllerServiceAccountManaged bool
	HookServiceAccountBase          string
	CertificateServiceAccountName   string
}

// MarkerData returns the exact canonical readiness-marker payload for this
// identity contract. Callers must compare the complete map, not a subset.
func (c ServiceAccountObjectIdentityContract) MarkerData() map[string]string {
	return map[string]string{
		"contract-version":                   serviceAccountObjectContractVersion,
		"controller-service-account-base":    c.ControllerServiceAccountBase,
		"controller-service-account-managed": strconv.FormatBool(c.ControllerServiceAccountManaged),
		"hook-service-account-base":          c.HookServiceAccountBase,
		"certificate-service-account-name":   c.CertificateServiceAccountName,
	}
}

// ExternalControllerServiceAccountName returns the non-reusable external
// controller identity for one release epoch.
func ExternalControllerServiceAccountName(base string, releaseSequence int32) (string, error) {
	if releaseSequence < 1 {
		return "", errors.New("external controller ServiceAccount release sequence must be positive")
	}
	if err := validateServiceAccountIdentityBase(base, len("-v2147483647")); err != nil {
		return "", fmt.Errorf("external controller ServiceAccount base: %w", err)
	}
	name := fmt.Sprintf("%s-v%d", base, releaseSequence)
	if problems := utilvalidation.IsDNS1123Subdomain(name); len(problems) != 0 {
		return "", fmt.Errorf("external controller ServiceAccount name %q is invalid: %s", name, strings.Join(problems, "; "))
	}
	return name, nil
}

// ServiceAccountObjectIdentityContractForRollout derives and validates the
// frozen identity inputs from one rollout. Epoch-zero predecessors belong to
// the separately trusted legacy bootstrap and do not expand this contract.
func ServiceAccountObjectIdentityContractForRollout(rollout *RolloutGuard) (ServiceAccountObjectIdentityContract, error) {
	if rollout == nil {
		return ServiceAccountObjectIdentityContract{}, errors.New("service account object identity rollout is required")
	}
	if rollout.ReleaseSequence < 1 {
		return ServiceAccountObjectIdentityContract{}, errors.New("service account object identity release sequence must be positive")
	}
	controllerBase, err := controllerServiceAccountIdentityBase(
		rollout.ControllerServiceAccountName,
		rollout.ControllerServiceAccountManaged,
		rollout.ReleaseSequence,
	)
	if err != nil {
		return ServiceAccountObjectIdentityContract{}, err
	}

	hookParts := teardownHookIdentityPattern.FindStringSubmatch(rollout.HookServiceAccountName)
	if len(hookParts) != 4 || hookParts[1] == "" {
		return ServiceAccountObjectIdentityContract{}, errors.New("hook ServiceAccount does not encode the candidate release identity")
	}
	hookSequence, err := strconv.ParseInt(hookParts[2], 10, 32)
	if err != nil || int32(hookSequence) != rollout.ReleaseSequence {
		return ServiceAccountObjectIdentityContract{}, fmt.Errorf("hook ServiceAccount %q does not encode release sequence %d", rollout.HookServiceAccountName, rollout.ReleaseSequence)
	}
	if err := validateServiceAccountIdentityBase(hookParts[1], len("-cleanup-v2147483647-0123456789ab")); err != nil {
		return ServiceAccountObjectIdentityContract{}, fmt.Errorf("hook ServiceAccount base: %w", err)
	}
	if rollout.CertificateDeploymentName == "" {
		return ServiceAccountObjectIdentityContract{}, errors.New("certificate ServiceAccount name is required")
	}
	if problems := utilvalidation.IsDNS1123Subdomain(rollout.CertificateDeploymentName); len(problems) != 0 {
		return ServiceAccountObjectIdentityContract{}, fmt.Errorf("certificate ServiceAccount name %q is invalid: %s", rollout.CertificateDeploymentName, strings.Join(problems, "; "))
	}

	if rollout.PreviousControllerReleaseSequence < 0 {
		return ServiceAccountObjectIdentityContract{}, errors.New("predecessor controller release sequence must not be negative")
	}
	if rollout.PreviousControllerServiceAccountName == "" {
		if rollout.PreviousControllerReleaseSequence != 0 {
			return ServiceAccountObjectIdentityContract{}, errors.New("predecessor controller release sequence requires a ServiceAccount name")
		}
	} else if rollout.PreviousControllerReleaseSequence > 0 {
		if rollout.PreviousControllerServiceAccountManaged != rollout.ControllerServiceAccountManaged {
			return ServiceAccountObjectIdentityContract{}, errors.New("candidate and predecessor controller ServiceAccounts must use the same managed identity mode")
		}
		previousBase, previousErr := controllerServiceAccountIdentityBase(
			rollout.PreviousControllerServiceAccountName,
			rollout.PreviousControllerServiceAccountManaged,
			rollout.PreviousControllerReleaseSequence,
		)
		if previousErr != nil {
			return ServiceAccountObjectIdentityContract{}, fmt.Errorf("derive predecessor controller ServiceAccount identity: %w", previousErr)
		}
		if previousBase != controllerBase {
			return ServiceAccountObjectIdentityContract{}, errors.New("candidate and predecessor controller ServiceAccounts must share one stable identity base")
		}
	}

	return ServiceAccountObjectIdentityContract{
		ControllerServiceAccountBase:    controllerBase,
		ControllerServiceAccountManaged: rollout.ControllerServiceAccountManaged,
		HookServiceAccountBase:          hookParts[1],
		CertificateServiceAccountName:   rollout.CertificateDeploymentName,
	}, nil
}

func controllerServiceAccountIdentityBase(name string, managed bool, expectedSequence int32) (string, error) {
	pattern := externalControllerServiceAccountPattern
	description := "external"
	maxSuffixLength := len("-v2147483647")
	if managed {
		pattern = managedControllerServiceAccountPattern
		description = "managed"
		maxSuffixLength = len("-v2147483647-0123456789ab")
	}
	parts := pattern.FindStringSubmatch(name)
	if len(parts) != 3 || parts[1] == "" {
		return "", fmt.Errorf("%s controller ServiceAccount %q does not have a stable epoch-qualified name", description, name)
	}
	sequence, err := strconv.ParseInt(parts[2], 10, 32)
	if err != nil || int32(sequence) != expectedSequence {
		return "", fmt.Errorf("%s controller ServiceAccount %q does not encode release sequence %d", description, name, expectedSequence)
	}
	if err := validateServiceAccountIdentityBase(parts[1], maxSuffixLength); err != nil {
		return "", fmt.Errorf("%s controller ServiceAccount base: %w", description, err)
	}
	return parts[1], nil
}

func validateServiceAccountIdentityBase(base string, maxSuffixLength int) error {
	if base == "" || base != strings.TrimSpace(base) {
		return errors.New("must be nonempty and have no surrounding whitespace")
	}
	if len(base)+maxSuffixLength > 253 {
		return errors.New("does not reserve space for every positive int32 release sequence")
	}
	if problems := utilvalidation.IsDNS1123Subdomain(base); len(problems) != 0 {
		return fmt.Errorf("%q is invalid: %s", base, strings.Join(problems, "; "))
	}
	return nil
}

// ServiceAccountObjectGuardPolicyName returns the release-stable name of the
// admission boundary around every ServiceAccount identity that can receive
// operator privileges. Candidate sequence and image are deliberately absent.
func ServiceAccountObjectGuardPolicyName(releaseNamespace, releaseName string) string {
	identity := releaseNamespace + "\n" + releaseName
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return serviceAccountObjectGuardNamePrefix + digest[:12]
}

// ServiceAccountObjectGuardBindingName returns the release-stable binding
// name. Policy and binding intentionally share one deterministic identity.
func ServiceAccountObjectGuardBindingName(releaseNamespace, releaseName string) string {
	return ServiceAccountObjectGuardPolicyName(releaseNamespace, releaseName)
}

func serviceAccountObjectGuardDenialMessage() string {
	return "Ptah service account object guard rejected an unsafe identity lifecycle request"
}

func serviceAccountObjectGuardProbe(releaseNamespace, releaseName string) admissionConvergenceDependencyProbe {
	policyName := ServiceAccountObjectGuardPolicyName(releaseNamespace, releaseName)
	markerPattern := serviceAccountObjectGuardMarkerPattern(releaseNamespace, releaseName)
	digest := sha256.Sum256([]byte("1\n" + policyName + "\n" + markerPattern))
	fieldManager := serviceAccountObjectProbeFieldManagerPrefix + fmt.Sprintf("%x", digest)
	return admissionConvergenceDependencyProbe{
		PolicyName:   policyName,
		FieldManager: fieldManager,
		Message:      serviceAccountObjectProbeDenialMessagePrefix + fieldManager,
	}
}

func serviceAccountObjectGuardProbeRequestExpression(releaseNamespace, releaseName string) string {
	probe := serviceAccountObjectGuardProbe(releaseNamespace, releaseName)
	return fmt.Sprintf(
		`request.operation == "UPDATE" && request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "configmaps" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && request.name.matches(%q) && has(request.options) && has(request.options.fieldManager) && request.options.fieldManager == %q`,
		releaseNamespace,
		serviceAccountObjectGuardMarkerPattern(releaseNamespace, releaseName),
		probe.FieldManager,
	)
}

func serviceAccountObjectGuardMarkerPattern(releaseNamespace, releaseName string) string {
	return "^" + regexp.QuoteMeta(admissionConvergenceMarkerPrefix) + `[1-9][0-9]*-` + admissionConvergenceReleaseDigest(releaseNamespace, releaseName) + `$`
}

// ServiceAccountObjectGuard prevents namespace-scoped writers from creating,
// replacing, or deleting a ServiceAccount that can inherit release privileges.
// Its name patterns are stable across candidate attempts, so an attacker cannot
// pre-stage the next hook or managed controller identity between upgrades.
type ServiceAccountObjectGuard struct {
	rollout *RolloutGuard
}

// NewServiceAccountObjectGuard derives the stable object boundary from the
// already validated rollout identity.
func NewServiceAccountObjectGuard(rollout *RolloutGuard) *ServiceAccountObjectGuard {
	return &ServiceAccountObjectGuard{rollout: rollout}
}

// ServiceAccountObjectGuardInventoryNames returns the policy/binding identity
// that convergence and retirement inventories must include. Both Kubernetes
// object kinds use this same name.
func ServiceAccountObjectGuardInventoryNames(releaseNamespace, releaseName string) []string {
	return []string{ServiceAccountObjectGuardPolicyName(releaseNamespace, releaseName)}
}

// ExpectedPolicy constructs the immutable, release-stable policy contract.
func (g *ServiceAccountObjectGuard) ExpectedPolicy() (*admissionregistrationv1.ValidatingAdmissionPolicy, error) {
	if err := g.validate(false); err != nil {
		return nil, err
	}
	patterns, err := g.patterns()
	if err != nil {
		return nil, err
	}
	fail := admissionregistrationv1.Fail
	name := ServiceAccountObjectGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	message := serviceAccountObjectGuardDenialMessage()
	protectedName := patterns.protectedNameExpression("object.metadata.name")
	protectedOldName := patterns.protectedNameExpression("oldObject.metadata.name")
	protectedRequestName := patterns.protectedNameExpression("request.name")
	managedLabels := fmt.Sprintf(
		`has(object.metadata.labels) && object.metadata.labels[%q] == %q && object.metadata.labels[%q] == %q && object.metadata.labels[%q] != "" && object.metadata.labels[%q] != "" && object.metadata.labels[%q] != ""`,
		managedByLabel, "Helm",
		"app.kubernetes.io/instance", g.rollout.ReleaseName,
		"helm.sh/chart",
		"app.kubernetes.io/version",
		"app.kubernetes.io/name",
	)
	helmOwnership := fmt.Sprintf(
		`has(object.metadata.annotations) && object.metadata.annotations[%q] == %q && object.metadata.annotations[%q] == %q`,
		"meta.helm.sh/release-name", g.rollout.ReleaseName,
		"meta.helm.sh/release-namespace", g.rollout.ReleaseNamespace,
	)
	hookInstallAnnotations := `object.metadata.annotations["helm.sh/hook"] == "pre-install,pre-upgrade" && object.metadata.annotations["helm.sh/hook-weight"] == "-110" && object.metadata.annotations["helm.sh/hook-delete-policy"] == "before-hook-creation,hook-succeeded,hook-failed"`
	hookDeleteAnnotations := `object.metadata.annotations["helm.sh/hook"] == "pre-delete" && object.metadata.annotations["helm.sh/hook-weight"] == "-110" && object.metadata.annotations["helm.sh/hook-delete-policy"] == "before-hook-creation,hook-succeeded"`
	cleanupAnnotations := `object.metadata.annotations["helm.sh/hook"] == "pre-delete" && object.metadata.annotations["helm.sh/hook-weight"] == "-109" && object.metadata.annotations["helm.sh/hook-delete-policy"] == "before-hook-creation,hook-succeeded"`
	bootstrapAnnotations := `object.metadata.annotations["helm.sh/hook"] == "pre-delete" && object.metadata.annotations["helm.sh/hook-weight"] == "-327" && object.metadata.annotations["helm.sh/hook-delete-policy"] == "before-hook-creation,hook-succeeded"`
	quiesceAnnotations := `object.metadata.annotations["helm.sh/hook"] == "pre-delete" && object.metadata.annotations["helm.sh/hook-weight"] == "-110" && object.metadata.annotations["helm.sh/hook-delete-policy"] == "before-hook-creation,hook-succeeded"`
	bootstrapOwnership := fmt.Sprintf(
		`has(object.metadata.annotations) && ((!(%q in object.metadata.annotations) && !(%q in object.metadata.annotations)) || (%s))`,
		"meta.helm.sh/release-name", "meta.helm.sh/release-namespace", helmOwnership,
	)
	annotations := fmt.Sprintf(
		`(variables.isExternalController && (!has(object.metadata.annotations) || !("helm.sh/hook" in object.metadata.annotations) && !("helm.sh/hook-weight" in object.metadata.annotations) && !("helm.sh/hook-delete-policy" in object.metadata.annotations))) || ((variables.isManagedController || variables.isCertificate) && (%s) && !("helm.sh/hook" in object.metadata.annotations) && !("helm.sh/hook-weight" in object.metadata.annotations) && !("helm.sh/hook-delete-policy" in object.metadata.annotations)) || (variables.isHook && (%s) && ((%s) || (%s))) || (variables.isCleanup && (%s) && (%s)) || (variables.isQuiesce && (%s) && (%s)) || (variables.isBootstrap && (%s) && (%s))`,
		helmOwnership,
		helmOwnership, hookInstallAnnotations, hookDeleteAnnotations,
		helmOwnership, cleanupAnnotations,
		helmOwnership, quiesceAnnotations,
		bootstrapOwnership, bootstrapAnnotations,
	)
	labels := fmt.Sprintf(
		`variables.isExternalController || ((%s) && ((!variables.isCertificate || object.metadata.labels[%q] == %q) && (!variables.isHook || object.metadata.labels[%q] == %q) && (!variables.isCleanup || object.metadata.labels[%q] == %q) && (!variables.isQuiesce || object.metadata.labels[%q] == %q) && (!variables.isBootstrap || object.metadata.labels[%q] == %q)))`,
		managedLabels,
		"app.kubernetes.io/component", "certificate-rotation",
		"app.kubernetes.io/component", "crd-manager",
		"app.kubernetes.io/component", "crd-manager-teardown",
		"app.kubernetes.io/component", "crd-manager-teardown-quiesce",
		"app.kubernetes.io/component", "teardown-retirement-bootstrap",
	)
	objectShape := fmt.Sprintf(
		`object != null && object.apiVersion == "v1" && object.kind == "ServiceAccount" && object.metadata.namespace == %q && object.metadata.name == variables.name && (!has(object.metadata.generateName) || object.metadata.generateName == "") && (!has(object.metadata.ownerReferences) || object.metadata.ownerReferences.size() == 0) && (variables.isExternalController || !has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && (!has(object.metadata.deletionTimestamp)) && (%s) && (%s) && (variables.isExternalController || ((variables.isManagedController || variables.isCertificate) ? (has(object.automountServiceAccountToken) && object.automountServiceAccountToken) : (has(object.automountServiceAccountToken) && !object.automountServiceAccountToken))) && ((variables.isManagedController || variables.isExternalController || variables.isCertificate) ? (!has(object.secrets) || object.secrets.all(secret, secret.name != "" && (!has(secret.namespace) || secret.namespace in ["", %q]) && (!has(secret.apiVersion) || secret.apiVersion in ["", "v1"]) && (!has(secret.kind) || secret.kind in ["", "Secret"]) && (!has(secret.uid) || secret.uid == "") && (!has(secret.resourceVersion) || secret.resourceVersion == "") && (!has(secret.fieldPath) || secret.fieldPath == ""))) : (!has(object.secrets) || object.secrets.size() == 0)) && ((variables.isManagedController || variables.isExternalController) ? (!has(object.imagePullSecrets) || object.imagePullSecrets.all(secret, secret.name != "")) : (!has(object.imagePullSecrets) || object.imagePullSecrets.size() == 0))`,
		g.rollout.ReleaseNamespace,
		labels,
		annotations,
		g.rollout.ReleaseNamespace,
	)
	oldIdentity := fmt.Sprintf(
		`oldObject != null && has(oldObject.apiVersion) && oldObject.apiVersion == "v1" && has(oldObject.kind) && oldObject.kind == "ServiceAccount" && has(oldObject.metadata) && has(oldObject.metadata.namespace) && oldObject.metadata.namespace == %q && has(oldObject.metadata.name) && oldObject.metadata.name == variables.name && has(oldObject.metadata.uid) && oldObject.metadata.uid != "" && has(oldObject.metadata.resourceVersion) && oldObject.metadata.resourceVersion != ""`,
		g.rollout.ReleaseNamespace,
	)
	updateIdentity := `object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && has(object.metadata.uid) && object.metadata.uid == oldObject.metadata.uid && has(object.metadata.resourceVersion) && object.metadata.resourceVersion == oldObject.metadata.resourceVersion && has(object.metadata.creationTimestamp) == has(oldObject.metadata.creationTimestamp) && (!has(object.metadata.creationTimestamp) || object.metadata.creationTimestamp == oldObject.metadata.creationTimestamp)`
	namespaceCleanup := serviceAccountNamespaceControllerCleanupExpression(g.rollout.ReleaseNamespace)
	authority := parentHookAdmissionAuthorityExpression(NamespaceDeletionGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName))

	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name, serviceAccountObjectPolicyWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(),
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "protected-service-account-object",
				Expression: fmt.Sprintf(
					`request.namespace == %q && (!has(request.subResource) || request.subResource == "") && ((object != null && has(object.metadata) && has(object.metadata.name) && (%s)) || (oldObject != null && has(oldObject.metadata) && has(oldObject.metadata.name) && (%s)) || (request.operation == "DELETE" && request.name != "" && (%s)))`,
					g.rollout.ReleaseNamespace, protectedName, protectedOldName, protectedRequestName,
				),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isCreate", Expression: `request.operation == "CREATE"`},
				{Name: "isUpdate", Expression: `request.operation == "UPDATE"`},
				{Name: "isDelete", Expression: `request.operation == "DELETE"`},
				{Name: "name", Expression: `request.operation == "DELETE" ? (oldObject != null && has(oldObject.metadata) && has(oldObject.metadata.name) ? oldObject.metadata.name : request.name) : object.metadata.name`},
				{Name: "isManagedController", Expression: patterns.managedControllerExpression("variables.name")},
				{Name: "isExternalController", Expression: patterns.externalControllerExpression("variables.name")},
				{Name: "isCertificate", Expression: fmt.Sprintf(`variables.name == %q`, g.rollout.CertificateDeploymentName)},
				{Name: "isHook", Expression: fmt.Sprintf(`variables.name.matches(%q)`, patterns.hook)},
				{Name: "isCleanup", Expression: fmt.Sprintf(`variables.name.matches(%q)`, patterns.cleanup)},
				{Name: "isQuiesce", Expression: fmt.Sprintf(`variables.name.matches(%q)`, patterns.quiesce)},
				{Name: "isBootstrap", Expression: fmt.Sprintf(`variables.name == %q`, patterns.bootstrap)},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `variables.isCreate || variables.isUpdate || variables.isDelete`, Message: message},
				{Expression: `variables.isManagedController || variables.isExternalController || variables.isCertificate || variables.isHook || variables.isCleanup || variables.isQuiesce || variables.isBootstrap`, Message: message},
				{Expression: fmt.Sprintf(`(!variables.isDelete && (%s)) || (variables.isDelete && ((%s) || (%s)))`, authority, authority, namespaceCleanup), Message: message},
				{Expression: `!variables.isDelete || (request.name == "" || request.name == variables.name)`, Message: message},
				{Expression: `variables.isCreate || (` + oldIdentity + `)`, Message: message},
				{Expression: `variables.isDelete || (` + objectShape + `)`, Message: message},
				{Expression: `!variables.isUpdate || (` + updateIdentity + `)`, Message: message},
			},
		},
	}
	addServiceAccountObjectConvergenceProbe(policy, g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	return policy, nil
}

// ExpectedBinding constructs the exact deny-only enforcement binding.
func (g *ServiceAccountObjectGuard) ExpectedBinding() (*admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	if err := g.validate(false); err != nil {
		return nil, err
	}
	name := ServiceAccountObjectGuardBindingName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(name, serviceAccountObjectBindingWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        ServiceAccountObjectGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName),
			MatchResources:    g.matchResources(),
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
	addAdmissionConvergenceProbeMatchResource(binding.Spec.MatchResources)
	return binding, nil
}

// ExpectedObjects returns both immutable objects for wiring into convergence
// and retirement state machines without reconstructing either contract.
func (g *ServiceAccountObjectGuard) ExpectedObjects() (*admissionregistrationv1.ValidatingAdmissionPolicy, *admissionregistrationv1.ValidatingAdmissionPolicyBinding, error) {
	policy, err := g.ExpectedPolicy()
	if err != nil {
		return nil, nil, err
	}
	binding, err := g.ExpectedBinding()
	if err != nil {
		return nil, nil, err
	}
	return policy, binding, nil
}

// Verify requires the retained policy and binding to match the compiled
// release-stable contract exactly.
func (g *ServiceAccountObjectGuard) Verify(ctx context.Context) error {
	if err := g.validate(true); err != nil {
		return err
	}
	expectedPolicy, expectedBinding, err := g.ExpectedObjects()
	if err != nil {
		return err
	}
	policy, err := g.rollout.Policies.Get(ctx, expectedPolicy.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get service account object guard policy: %w", err)
	}
	if err := g.verifyPolicy(policy, expectedPolicy); err != nil {
		return err
	}
	binding, err := g.rollout.Bindings.Get(ctx, expectedBinding.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get service account object guard binding: %w", err)
	}
	return g.verifyBinding(binding, expectedBinding)
}

// WaitReady verifies the retained contract and waits for warning-free CEL
// type checking before any protected ServiceAccount can be created.
func (g *ServiceAccountObjectGuard) WaitReady(ctx context.Context) error {
	if err := g.Verify(ctx); err != nil {
		return err
	}
	name := ServiceAccountObjectGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	return wait.PollUntilContextCancel(ctx, g.rollout.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		policy, err := g.rollout.Policies.Get(pollCtx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("read service account object guard policy status: %w", err)
		}
		expected, err := g.ExpectedPolicy()
		if err != nil {
			return false, err
		}
		if err := g.verifyPolicy(policy, expected); err != nil {
			return false, err
		}
		if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
			return false, nil
		}
		if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) != 0 {
			return false, fmt.Errorf("service account object guard policy has CEL type-check warnings: %s", warnings[0].Warning)
		}
		return true, nil
	})
}

// Probe proves that one directly addressed API server observes both the exact
// retained policy and binding. The unchanged dry-run update uses the current
// exact convergence marker, which Helm creates before this guard; any admission
// or marker shape other than the single compiled denial is inconclusive or
// fatal.
func (g *ServiceAccountObjectGuard) Probe(ctx context.Context, client AdmissionConvergenceMarkerClient) (bool, error) {
	if ctx == nil {
		return false, errors.New("service account object guard probe context is nil")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if client == nil {
		return false, errors.New("service account object guard marker client is nil")
	}
	if err := g.validate(true); err != nil {
		return false, err
	}
	markerName := AdmissionConvergenceMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence)
	marker, err := client.Get(ctx, markerName, metav1.GetOptions{})
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if err != nil {
		if retryableAdmissionConvergenceError(err) {
			return false, nil
		}
		return false, fmt.Errorf("get direct service account object guard convergence marker: %w", err)
	}
	convergence := NewAdmissionConvergenceGuard(g.rollout)
	unsealedErr := convergence.verifyUnsealedMarker(marker)
	_, sealedErr := convergence.verifySealedMarker(marker)
	if unsealedErr != nil && sealedErr != nil {
		return false, fmt.Errorf("service account object guard convergence marker is neither exact unsealed nor sealed state: unsealed: %v; sealed: %v", unsealedErr, sealedErr)
	}
	probe := serviceAccountObjectGuardProbe(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	_, err = client.Update(ctx, marker.DeepCopy(), metav1.UpdateOptions{
		DryRun:       []string{metav1.DryRunAll},
		FieldManager: probe.FieldManager,
	})
	if contextErr := ctx.Err(); contextErr != nil {
		return false, contextErr
	}
	if err == nil {
		return false, nil
	}
	if hasExactValidatingAdmissionPolicyDenial(err, probe.PolicyName, probe.PolicyName, probe.Message) {
		return true, nil
	}
	if retryableAdmissionConvergenceError(err) {
		return false, nil
	}
	return false, fmt.Errorf("direct service account object guard probe returned an unexpected response: %w", err)
}

func addServiceAccountObjectConvergenceProbe(
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	releaseNamespace,
	releaseName string,
) {
	if policy == nil {
		return
	}
	expression := serviceAccountObjectGuardProbeRequestExpression(releaseNamespace, releaseName)
	addAdmissionConvergenceProbeMatchResource(policy.Spec.MatchConstraints)
	for index := range policy.Spec.MatchConditions {
		policy.Spec.MatchConditions[index].Expression = "(" + expression + ") || (" + policy.Spec.MatchConditions[index].Expression + ")"
	}
	policy.Spec.Variables = append([]admissionregistrationv1.Variable{{
		Name:       "isServiceAccountObjectConvergenceProbe",
		Expression: expression,
	}}, policy.Spec.Variables...)
	for index := range policy.Spec.Validations {
		policy.Spec.Validations[index].Expression = "variables.isServiceAccountObjectConvergenceProbe || (" + policy.Spec.Validations[index].Expression + ")"
	}
	probe := serviceAccountObjectGuardProbe(releaseNamespace, releaseName)
	policy.Spec.Validations = append(policy.Spec.Validations,
		admissionregistrationv1.Validation{
			Expression: `!variables.isServiceAccountObjectConvergenceProbe || request.dryRun == true`,
			Message:    admissionConvergenceProbePersistenceMessage,
		},
		admissionregistrationv1.Validation{
			Expression: `!variables.isServiceAccountObjectConvergenceProbe`,
			Message:    probe.Message,
		},
	)
}

type serviceAccountObjectPatterns struct {
	managedController  string
	externalController string
	hook               string
	cleanup            string
	quiesce            string
	bootstrap          string
	certificate        string
}

func (g *ServiceAccountObjectGuard) patterns() (serviceAccountObjectPatterns, error) {
	contract, err := ServiceAccountObjectIdentityContractForRollout(g.rollout)
	if err != nil {
		return serviceAccountObjectPatterns{}, err
	}
	result := serviceAccountObjectPatterns{
		hook:        "^" + regexp.QuoteMeta(contract.HookServiceAccountBase+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`,
		cleanup:     "^" + regexp.QuoteMeta(contract.HookServiceAccountBase+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`,
		quiesce:     "^" + regexp.QuoteMeta(contract.HookServiceAccountBase+"-quiesce-v") + `[1-9][0-9]*-[0-9a-f]{12}$`,
		bootstrap:   teardownRetirementBootstrapPrefix + teardownRetirementReleaseDigest(g.rollout.ReleaseNamespace, g.rollout.ReleaseName),
		certificate: contract.CertificateServiceAccountName,
	}
	if contract.ControllerServiceAccountManaged {
		result.managedController = "^" + regexp.QuoteMeta(contract.ControllerServiceAccountBase+"-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
	} else {
		result.externalController = "^" + regexp.QuoteMeta(contract.ControllerServiceAccountBase+"-v") + `[1-9][0-9]*$`
	}
	return result, nil
}

func (p serviceAccountObjectPatterns) managedControllerExpression(name string) string {
	if p.managedController == "" {
		return "false"
	}
	return fmt.Sprintf(`%s.matches(%q)`, name, p.managedController)
}

func (p serviceAccountObjectPatterns) externalControllerExpression(name string) string {
	if p.externalController == "" {
		return "false"
	}
	return fmt.Sprintf(`%s.matches(%q)`, name, p.externalController)
}

func (p serviceAccountObjectPatterns) protectedNameExpression(name string) string {
	return fmt.Sprintf(
		`(%s) || (%s) || %s == %q || %s.matches(%q) || %s.matches(%q) || %s.matches(%q) || %s == %q`,
		p.managedControllerExpression(name),
		p.externalControllerExpression(name),
		name, p.certificate,
		name, p.hook,
		name, p.cleanup,
		name, p.quiesce,
		name, p.bootstrap,
	)
}

func serviceAccountNamespaceControllerCleanupExpression(namespace string) string {
	legacyPrincipal := `request.userInfo.username == "system:kube-controller-manager" && request.userInfo.groups.size() == 1 && "system:authenticated" in request.userInfo.groups`
	serviceAccountPrincipal := `request.userInfo.username == "system:serviceaccount:kube-system:namespace-controller" && request.userInfo.groups.size() == 3 && "system:serviceaccounts" in request.userInfo.groups && "system:serviceaccounts:kube-system" in request.userInfo.groups && "system:authenticated" in request.userInfo.groups`
	return fmt.Sprintf(
		`namespaceObject != null && namespaceObject.metadata.name == %q && has(namespaceObject.metadata.deletionTimestamp) && ((%s) || (%s))`,
		namespace, legacyPrincipal, serviceAccountPrincipal,
	)
}

func (g *ServiceAccountObjectGuard) matchResources() *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &exact,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{
					admissionregistrationv1.Create,
					admissionregistrationv1.Update,
					admissionregistrationv1.Delete,
				},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"serviceaccounts"},
					Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
				},
			},
		}},
	}
}

func (g *ServiceAccountObjectGuard) metadata(name, weight string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Annotations: map[string]string{
			"helm.sh/hook":                "pre-install,pre-upgrade",
			"helm.sh/hook-weight":         weight,
			"helm.sh/resource-policy":     "keep",
			rolloutGuardVersionAnnotation: rolloutGuardVersion,
			ReleaseNameAnnotation:         g.rollout.ReleaseName,
			ReleaseNamespaceAnnotation:    g.rollout.ReleaseNamespace,
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			"app.kubernetes.io/instance":  g.rollout.ReleaseName,
			"app.kubernetes.io/component": serviceAccountObjectGuardComponent,
		},
	}
}

func (g *ServiceAccountObjectGuard) verifyPolicy(actual, expected *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	if actual == nil {
		return errors.New("service account object guard policy is missing")
	}
	if actual.APIVersion != expected.APIVersion || actual.Kind != expected.Kind {
		return errors.New("service account object guard policy has an unexpected API identity")
	}
	if err := verifyServiceAccountObjectGuardMetadata("policy", actual.ObjectMeta, expected.ObjectMeta); err != nil {
		return err
	}
	if mismatch := serviceAccountObjectPolicySpecMismatch(actual.Spec, expected.Spec); mismatch != "" {
		return fmt.Errorf("service account object guard policy %s does not match its immutable contract: %s", expected.Name, mismatch)
	}
	return nil
}

func serviceAccountObjectPolicySpecMismatch(actual, expected admissionregistrationv1.ValidatingAdmissionPolicySpec) string {
	if reflect.DeepEqual(actual, expected) {
		return ""
	}
	if !reflect.DeepEqual(actual.ParamKind, expected.ParamKind) {
		return "parameter kind differs"
	}
	if !reflect.DeepEqual(actual.FailurePolicy, expected.FailurePolicy) {
		return "failure policy differs"
	}
	if !reflect.DeepEqual(actual.MatchConstraints, expected.MatchConstraints) {
		return "match constraints differ"
	}
	if !reflect.DeepEqual(actual.MatchConditions, expected.MatchConditions) {
		return "match conditions differ"
	}
	if !reflect.DeepEqual(actual.Variables, expected.Variables) {
		for index := 0; index < len(actual.Variables) && index < len(expected.Variables); index++ {
			if !reflect.DeepEqual(actual.Variables[index], expected.Variables[index]) {
				return fmt.Sprintf("variable %d (%s) differs: got %q, want %q", index, expected.Variables[index].Name, actual.Variables[index].Expression, expected.Variables[index].Expression)
			}
		}
		return fmt.Sprintf("variable count differs: got %d, want %d", len(actual.Variables), len(expected.Variables))
	}
	if !reflect.DeepEqual(actual.Validations, expected.Validations) {
		for index := 0; index < len(actual.Validations) && index < len(expected.Validations); index++ {
			if !reflect.DeepEqual(actual.Validations[index], expected.Validations[index]) {
				return fmt.Sprintf("validation %d differs: got %q, want %q", index, actual.Validations[index].Expression, expected.Validations[index].Expression)
			}
		}
		return fmt.Sprintf("validation count differs: got %d, want %d", len(actual.Validations), len(expected.Validations))
	}
	if !reflect.DeepEqual(actual.AuditAnnotations, expected.AuditAnnotations) {
		return "audit annotations differ"
	}
	return "unknown field differs"
}

func (g *ServiceAccountObjectGuard) verifyBinding(actual, expected *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	if actual == nil {
		return errors.New("service account object guard binding is missing")
	}
	if actual.APIVersion != expected.APIVersion || actual.Kind != expected.Kind {
		return errors.New("service account object guard binding has an unexpected API identity")
	}
	if err := verifyServiceAccountObjectGuardMetadata("binding", actual.ObjectMeta, expected.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual.Spec, expected.Spec) {
		return fmt.Errorf("service account object guard binding %s does not match its immutable contract", expected.Name)
	}
	return nil
}

func verifyServiceAccountObjectGuardMetadata(kind string, actual, expected metav1.ObjectMeta) error {
	if actual.Name != expected.Name || actual.Namespace != "" || actual.GenerateName != "" {
		return fmt.Errorf("service account object guard %s has an unexpected name", kind)
	}
	if actual.UID == "" || actual.ResourceVersion == "" {
		return fmt.Errorf("service account object guard %s has no persisted identity", kind)
	}
	if !reflect.DeepEqual(actual.Annotations, expected.Annotations) || !reflect.DeepEqual(actual.Labels, expected.Labels) {
		return fmt.Errorf("service account object guard %s has foreign or incomplete ownership", kind)
	}
	if len(actual.OwnerReferences) != 0 || len(actual.Finalizers) != 0 || actual.DeletionTimestamp != nil || actual.DeletionGracePeriodSeconds != nil {
		return fmt.Errorf("service account object guard %s has unsafe lifecycle metadata", kind)
	}
	return nil
}

func (g *ServiceAccountObjectGuard) validate(requireReaders bool) error {
	if g == nil || g.rollout == nil {
		return errors.New("service account object guard rollout identity is required")
	}
	if err := g.rollout.validateIdentity(); err != nil {
		return fmt.Errorf("validate service account object guard identity: %w", err)
	}
	if requireReaders && (g.rollout.Policies == nil || g.rollout.Bindings == nil) {
		return errors.New("service account object guard policy and binding readers are required")
	}
	if g.rollout.PollEvery <= 0 {
		return errors.New("service account object guard poll interval must be positive")
	}
	if _, err := g.patterns(); err != nil {
		return err
	}
	return nil
}
