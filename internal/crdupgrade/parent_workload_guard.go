package crdupgrade

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	parentReplicaSetGuardPrefix = "ptah-operator-runtime-parent-guard-v2-"
	parentHookOriginGuardPrefix = "ptah-operator-hook-parent-origin-guard-v2-"
	parentHookPodOriginPrefix   = "ptah-operator-hook-pod-origin-guard-v2-"
	parentHookContractPrefix    = "ptah-operator-hook-parent-contract-v"
	parentOriginReadyPrefix     = "ptah-operator-parent-origin-ready-v2-"
	parentWorkloadComponent     = "parent-workload-guard"
	parentOriginReadyComponent  = "parent-workload-guard-readiness"
	parentOriginReadyManagedBy  = "Helm"

	parentOriginReadyVersionAnnotation = "operator.ptah.dev/parent-origin-ready-version"
	parentOriginReadyVersion           = "2"

	parentReplicaSetPolicyWeight            = "-139"
	parentReplicaSetBindingWeight           = "-138"
	parentHookOriginPolicyWeight            = "-137"
	parentHookOriginBindingWeight           = "-136"
	parentHookPodOriginPolicyWeight         = "-135"
	parentHookPodOriginBindingWeight        = "-134"
	parentHookContractPolicyWeight          = "-133"
	parentHookContractBindingWeight         = "-132"
	legacyHookOriginRetiredBindingWeight    = "9"
	legacyHookOriginRetiredPolicyWeight     = "10"
	legacyHookPodOriginRetiredBindingWeight = "11"
	legacyHookPodOriginRetiredPolicyWeight  = "12"

	legacyParentWorkloadOriginTeardownGroup = "parent-workload-origin-v1"
)

// ParentReplicaSetGuardPolicyName returns the append-only release-owned
// boundary around ReplicaSets that can mint Pods for this runtime principal.
func ParentReplicaSetGuardPolicyName(releaseNamespace, releaseName string, releaseSequence int32, managerImage string) string {
	return parentReplicaSetGuardPrefix + controllerPrincipalGuardDigest(releaseNamespace, releaseName, releaseSequence, managerImage)
}

// ParentHookJobOriginGuardPolicyName returns the stable release-owned gate
// which prevents namespace-scoped actors from staging a future hook Job
// before its immutable candidate contract has been installed.
func ParentHookJobOriginGuardPolicyName(releaseNamespace, releaseName string) string {
	return parentHookOriginGuardPrefix + parentWorkloadStableDigest(releaseNamespace, releaseName)
}

func legacyParentHookJobOriginGuardPolicyName(releaseNamespace, releaseName string) string {
	return "ptah-operator-hook-parent-origin-guard-v1-" + parentWorkloadStableDigest(releaseNamespace, releaseName)
}

// ParentHookPodOriginGuardPolicyName returns the stable release-owned gate
// which permits hook Pods only when the built-in Job controller creates them
// through an exact Job ownership and label chain.
func ParentHookPodOriginGuardPolicyName(releaseNamespace, releaseName string) string {
	return parentHookPodOriginPrefix + parentWorkloadStableDigest(releaseNamespace, releaseName)
}

func legacyParentHookPodOriginGuardPolicyName(releaseNamespace, releaseName string) string {
	return "ptah-operator-hook-pod-origin-guard-v1-" + parentWorkloadStableDigest(releaseNamespace, releaseName)
}

// ParentHookJobContractPolicyName returns the append-only exact contract for
// the install, upgrade, and teardown Jobs belonging to one candidate release.
func ParentHookJobContractPolicyName(releaseNamespace, releaseName string, sequence int32, managerImage string) string {
	digest := hookIdentityDigest(releaseNamespace, releaseName, sequence, managerImage)
	return fmt.Sprintf("%s%d-%s", parentHookContractPrefix, sequence, digest[:12])
}

// ParentOriginReadyMarkerName returns the ordinary-manifest marker whose
// presence proves that Helm reached resource application only after the
// pre-upgrade direct admission-convergence barrier completed.
func ParentOriginReadyMarkerName(releaseNamespace, releaseName string) string {
	return parentOriginReadyPrefix + parentWorkloadStableDigest(releaseNamespace, releaseName)
}

func parentWorkloadStableDigest(releaseNamespace, releaseName string) string {
	digest := sha256.Sum256([]byte(releaseNamespace + "\n" + releaseName))
	return fmt.Sprintf("%x", digest)[:12]
}

func parentReplicaSetDenialMessage() string {
	return "Ptah runtime parent guard rejected an unsafe ReplicaSet"
}

func parentHookOriginDenialMessage() string {
	return "Ptah hook parent origin guard rejected an unauthorized Job"
}

func parentHookPodOriginDenialMessage() string {
	return "Ptah hook Pod origin guard rejected an unauthorized Pod"
}

func parentHookContractDenialMessage(sequence int32) string {
	return fmt.Sprintf("Ptah hook parent contract v%d rejected an unsafe Job", sequence)
}

// ParentWorkloadGuard closes the controller-parent boundary in front of the
// Pod-level executable guards. The stable policies cover predictable future
// identities, while the append-only Job contract pins the current candidate's
// executable and privileges exactly.
type ParentWorkloadGuard struct {
	rollout          *RolloutGuard
	identityContract ServiceAccountObjectIdentityContract
	identityErr      error
}

// NewParentWorkloadGuard derives every name and executable argument from the
// already validated rollout identity.
func NewParentWorkloadGuard(rollout *RolloutGuard) *ParentWorkloadGuard {
	guard := &ParentWorkloadGuard{rollout: rollout}
	if rollout != nil {
		guard.identityContract, guard.identityErr = ServiceAccountObjectIdentityContractForRollout(rollout)
	}
	return guard
}

// ReadinessMarkerTarget returns the exact stable readiness marker contract for
// explicit deletion by the guarded final uninstall phase. The ordinary
// resource is retained across rollback and no-hook operations, so Helm's
// release-resource deletion must never be its only cleanup mechanism.
func (g *ParentWorkloadGuard) ReadinessMarkerTarget() (TeardownRetirementMarkerTarget, error) {
	if err := g.validate(); err != nil {
		return TeardownRetirementMarkerTarget{}, err
	}
	expected := g.readinessMarker()
	return TeardownRetirementMarkerTarget{
		Name: expected.Name,
		Verify: func(actual *corev1.ConfigMap) error {
			if actual == nil || actual.Name != expected.Name || actual.Namespace != expected.Namespace || actual.GenerateName != "" ||
				actual.UID == "" || actual.ResourceVersion == "" || actual.DeletionTimestamp != nil || actual.DeletionGracePeriodSeconds != nil ||
				len(actual.OwnerReferences) != 0 || len(actual.Finalizers) != 0 ||
				!reflect.DeepEqual(actual.Annotations, expected.Annotations) || !reflect.DeepEqual(actual.Labels, expected.Labels) ||
				!reflect.DeepEqual(actual.Data, expected.Data) || len(actual.BinaryData) != 0 || actual.Immutable == nil || !*actual.Immutable {
				return fmt.Errorf("parent-origin readiness ConfigMap/%s differs from the exact stable contract", expected.Name)
			}
			return nil
		},
	}, nil
}

// Verify requires all stable and candidate-specific policies and bindings to
// match the compiled contract. It does not depend on admission status and is
// therefore safe in every runtime init container.
func (g *ParentWorkloadGuard) Verify(ctx context.Context) error {
	if err := g.validate(); err != nil {
		return err
	}
	for _, entry := range g.entries() {
		policy, err := g.rollout.Policies.Get(ctx, entry.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s policy: %w", entry.description, err)
		}
		if err := entry.verifyPolicy(policy); err != nil {
			return err
		}
		binding, err := g.rollout.Bindings.Get(ctx, entry.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s binding: %w", entry.description, err)
		}
		if err := entry.verifyBinding(binding); err != nil {
			return err
		}
	}
	return nil
}

// Prepare verifies the immutable objects and waits until the API server has
// type-checked every policy without warnings. A retained v2 hook-origin gate
// protects upgrades before the candidate ServiceAccount appears. A first
// install still requires an exclusive-writer environment until that gate has
// been published; it cannot prove away a namespace writer that was already
// active before the release established its admission boundary.
func (g *ParentWorkloadGuard) Prepare(ctx context.Context) error {
	if err := g.Verify(ctx); err != nil {
		return err
	}
	return g.WaitReady(ctx)
}

// WaitReady waits for all parent policies to be observed and type-checked.
func (g *ParentWorkloadGuard) WaitReady(ctx context.Context) error {
	if err := g.validate(); err != nil {
		return err
	}
	for _, entry := range g.entries() {
		entry := entry
		err := wait.PollUntilContextCancel(ctx, g.rollout.PollEvery, true, func(pollCtx context.Context) (bool, error) {
			policy, err := g.rollout.Policies.Get(pollCtx, entry.name, metav1.GetOptions{})
			if err != nil {
				return false, fmt.Errorf("read %s policy status: %w", entry.description, err)
			}
			if err := entry.verifyPolicy(policy); err != nil {
				return false, err
			}
			if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
				return false, nil
			}
			if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) != 0 {
				return false, fmt.Errorf("%s policy has CEL type-check warnings: %s", entry.description, warnings[0].Warning)
			}
			return true, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type parentGuardEntry struct {
	name          string
	description   string
	policy        *admissionregistrationv1.ValidatingAdmissionPolicy
	binding       *admissionregistrationv1.ValidatingAdmissionPolicyBinding
	verifyPolicy  func(*admissionregistrationv1.ValidatingAdmissionPolicy) error
	verifyBinding func(*admissionregistrationv1.ValidatingAdmissionPolicyBinding) error
}

func (g *ParentWorkloadGuard) entries() []parentGuardEntry {
	replicaSetPolicy := g.replicaSetPolicy()
	replicaSetBinding := g.binding(replicaSetPolicy.Name, false)
	hookOriginPolicy := g.hookJobOriginPolicy()
	hookOriginBinding := g.binding(hookOriginPolicy.Name, false)
	hookPodOriginPolicy := g.hookPodOriginPolicy()
	hookPodOriginBinding := g.binding(hookPodOriginPolicy.Name, false)
	hookContractPolicy := g.hookJobContractPolicy()
	hookContractBinding := g.binding(hookContractPolicy.Name, true)
	return []parentGuardEntry{
		{
			name: replicaSetPolicy.Name, description: "runtime parent guard",
			policy: replicaSetPolicy, binding: replicaSetBinding,
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return g.verifyPolicy(actual, replicaSetPolicy, false)
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return g.verifyBinding(actual, replicaSetBinding, false)
			},
		},
		{
			name: hookOriginPolicy.Name, description: "hook parent origin guard",
			policy: hookOriginPolicy, binding: hookOriginBinding,
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return g.verifyPolicy(actual, hookOriginPolicy, false)
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return g.verifyBinding(actual, hookOriginBinding, false)
			},
		},
		{
			name: hookPodOriginPolicy.Name, description: "hook Pod origin guard",
			policy: hookPodOriginPolicy, binding: hookPodOriginBinding,
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return g.verifyPolicy(actual, hookPodOriginPolicy, false)
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return g.verifyBinding(actual, hookPodOriginBinding, false)
			},
		},
		{
			name: hookContractPolicy.Name, description: "hook parent contract guard",
			policy: hookContractPolicy, binding: hookContractBinding,
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return g.verifyPolicy(actual, hookContractPolicy, true)
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return g.verifyBinding(actual, hookContractBinding, true)
			},
		},
	}
}

func (g *ParentWorkloadGuard) replicaSetPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := ParentReplicaSetGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage)
	controllerSA := g.rollout.ControllerServiceAccountName
	certificateSA := g.rollout.CertificateDeploymentName
	controllerDeployment := g.rollout.ControllerDeploymentName
	certificateDeployment := g.rollout.CertificateDeploymentName
	message := parentReplicaSetDenialMessage()
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name, false),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{"apps"}, APIVersions: []string{"v1"}, Resources: []string{"replicasets"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
						},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "protected-runtime-service-account",
				Expression: fmt.Sprintf(
					`request.namespace == %q && ((has(object.spec.template.spec.serviceAccountName) && object.spec.template.spec.serviceAccountName in [%q, %q]) || (request.operation == "UPDATE" && has(oldObject.spec.template.spec.serviceAccountName) && oldObject.spec.template.spec.serviceAccountName in [%q, %q]))`,
					g.rollout.ReleaseNamespace, controllerSA, certificateSA, controllerSA, certificateSA,
				),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "owner", Expression: `object.metadata.ownerReferences[0]`},
				{Name: "hash", Expression: `object.metadata.labels["pod-template-hash"]`},
				{Name: "isController", Expression: fmt.Sprintf(`object.spec.template.spec.serviceAccountName == %q`, controllerSA)},
				{Name: "expectedDeployment", Expression: fmt.Sprintf(`object.spec.template.spec.serviceAccountName == %q ? %q : %q`, controllerSA, controllerDeployment, certificateDeployment)},
				{Name: "expectedComponent", Expression: fmt.Sprintf(`object.spec.template.spec.serviceAccountName == %q ? "controller" : "certificate-rotation"`, controllerSA)},
				{Name: "isGarbageCollectorCleanup", Expression: replicaSetGarbageCollectorCleanupExpression()},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `!has(request.subResource) || request.subResource == ""`, Message: message},
				{Expression: `request.userInfo.username in ["system:kube-controller-manager", "system:serviceaccount:kube-system:deployment-controller"] || variables.isGarbageCollectorCleanup`, Message: message},
				{Expression: fmt.Sprintf(`has(object.spec.template.spec.serviceAccountName) && object.spec.template.spec.serviceAccountName in [%q, %q]`, controllerSA, certificateSA), Message: message},
				{Expression: `has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1`, Message: message},
				{Expression: `variables.owner.apiVersion == "apps/v1" && variables.owner.kind == "Deployment" && variables.owner.name == variables.expectedDeployment && has(variables.owner.uid) && variables.owner.uid != "" && has(variables.owner.controller) && variables.owner.controller && has(variables.owner.blockOwnerDeletion) && variables.owner.blockOwnerDeletion`, Message: message},
				{Expression: `has(object.metadata.labels) && "pod-template-hash" in object.metadata.labels && variables.hash.matches("^[a-z0-9]{1,10}$")`, Message: message},
				{Expression: `object.metadata.name == variables.expectedDeployment + "-" + variables.hash && (!has(object.metadata.generateName) || object.metadata.generateName == "")`, Message: message},
				{Expression: `has(object.spec.selector.matchLabels) && object.spec.selector.matchLabels.size() == 4 && (!has(object.spec.selector.matchExpressions) || object.spec.selector.matchExpressions.size() == 0)`, Message: message},
				{Expression: `object.spec.selector.matchLabels.all(key, key in ["app.kubernetes.io/name", "app.kubernetes.io/instance", "app.kubernetes.io/component", "pod-template-hash"])`, Message: message},
				{Expression: `has(object.spec.template.metadata.labels) && ["app.kubernetes.io/name", "app.kubernetes.io/instance", "app.kubernetes.io/component", "pod-template-hash"].all(key, key in object.metadata.labels && key in object.spec.selector.matchLabels && key in object.spec.template.metadata.labels && object.metadata.labels[key] == object.spec.selector.matchLabels[key] && object.spec.selector.matchLabels[key] == object.spec.template.metadata.labels[key])`, Message: message},
				{Expression: fmt.Sprintf(`object.spec.selector.matchLabels["app.kubernetes.io/instance"] == %q && object.spec.selector.matchLabels["app.kubernetes.io/component"] == variables.expectedComponent && object.spec.selector.matchLabels["pod-template-hash"] == variables.hash`, g.rollout.ReleaseName), Message: message},
			},
		},
	}
	addAdmissionConvergenceDependencyProbe(
		policy,
		g.rollout.ReleaseNamespace,
		AdmissionConvergenceMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence),
		hookIdentityDigest(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage),
	)
	return policy
}

func replicaSetGarbageCollectorCleanupExpression() string {
	// ResourceVersion is protected by storage optimistic concurrency, while
	// managedFields is API-machinery bookkeeping. Neither grants workload
	// authority, and pinning either here would make legitimate cleanup depend on
	// server-side mutation order.
	return `request.operation == "UPDATE" && request.userInfo.username == "system:serviceaccount:kube-system:generic-garbage-collector" && oldObject != null && object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && object.metadata.uid == oldObject.metadata.uid && has(object.metadata.generateName) == has(oldObject.metadata.generateName) && (!has(object.metadata.generateName) || object.metadata.generateName == oldObject.metadata.generateName) && has(object.metadata.creationTimestamp) == has(oldObject.metadata.creationTimestamp) && (!has(object.metadata.creationTimestamp) || object.metadata.creationTimestamp == oldObject.metadata.creationTimestamp) && object.metadata.generation == oldObject.metadata.generation && has(oldObject.metadata.deletionTimestamp) && has(object.metadata.deletionTimestamp) && object.metadata.deletionTimestamp == oldObject.metadata.deletionTimestamp && has(object.metadata.deletionGracePeriodSeconds) == has(oldObject.metadata.deletionGracePeriodSeconds) && (!has(object.metadata.deletionGracePeriodSeconds) || object.metadata.deletionGracePeriodSeconds == oldObject.metadata.deletionGracePeriodSeconds) && object.spec == oldObject.spec && has(object.status) == has(oldObject.status) && (!has(object.status) || object.status == oldObject.status) && has(object.metadata.labels) == has(oldObject.metadata.labels) && (!has(object.metadata.labels) || object.metadata.labels == oldObject.metadata.labels) && has(object.metadata.annotations) == has(oldObject.metadata.annotations) && (!has(object.metadata.annotations) || object.metadata.annotations == oldObject.metadata.annotations) && has(object.metadata.ownerReferences) == has(oldObject.metadata.ownerReferences) && (!has(object.metadata.ownerReferences) || object.metadata.ownerReferences == oldObject.metadata.ownerReferences) && has(oldObject.metadata.finalizers) && oldObject.metadata.finalizers.filter(finalizer, finalizer == "foregroundDeletion").size() == 1 && ((!has(object.metadata.finalizers) && oldObject.metadata.finalizers.size() == 1) || (has(object.metadata.finalizers) && object.metadata.finalizers == oldObject.metadata.finalizers.filter(finalizer, finalizer != "foregroundDeletion")))`
}

func parentHookExactPrincipalExpression(username string, groups ...string) string {
	parts := []string{
		fmt.Sprintf(`request.userInfo.username == %q`, username),
		fmt.Sprintf(`request.userInfo.groups.size() == %d`, len(groups)),
	}
	for _, group := range groups {
		parts = append(parts, fmt.Sprintf(`%q in request.userInfo.groups`, group))
	}
	return strings.Join(parts, " && ")
}

func parentHookJobControllerPrincipalExpression() string {
	return fmt.Sprintf(
		`(%s) || (%s)`,
		parentHookExactPrincipalExpression("system:kube-controller-manager", "system:authenticated"),
		parentHookExactPrincipalExpression(
			"system:serviceaccount:kube-system:job-controller",
			"system:serviceaccounts",
			"system:serviceaccounts:kube-system",
			"system:authenticated",
		),
	)
}

func parentHookSchedulerPrincipalExpression() string {
	return fmt.Sprintf(
		`(%s) || (%s)`,
		parentHookExactPrincipalExpression("system:kube-scheduler", "system:authenticated"),
		parentHookExactPrincipalExpression(
			"system:serviceaccount:kube-system:kube-scheduler",
			"system:serviceaccounts",
			"system:serviceaccounts:kube-system",
			"system:authenticated",
		),
	)
}

func parentHookNodePrincipalExpression() string {
	return `request.userInfo.username.matches("^system:node:.+$") && request.userInfo.groups.size() == 2 && "system:nodes" in request.userInfo.groups && "system:authenticated" in request.userInfo.groups && has(oldObject.spec.nodeName) && oldObject.spec.nodeName != "" && request.userInfo.username == "system:node:" + oldObject.spec.nodeName`
}

func parentHookAdmissionAuthorityExpression(namespaceGuard string) string {
	return fmt.Sprintf(`(authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").check("create").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").check("create").allowed()) || (authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").name(%q).check("delete").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").name(%q).check("delete").allowed())`, namespaceGuard, namespaceGuard)
}

func parentHookTerminalJobExpression(object string) string {
	return fmt.Sprintf(`has(%[1]s.status.conditions) && %[1]s.status.conditions.exists(condition, condition.status == "True" && condition.type in ["Complete", "Failed"])`, object)
}

func parentHookStatusPreservesIdentityExpression() string {
	return `oldObject != null && object.spec == oldObject.spec && object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && ((!has(object.metadata.generateName) && !has(oldObject.metadata.generateName)) || (has(object.metadata.generateName) && has(oldObject.metadata.generateName) && object.metadata.generateName == oldObject.metadata.generateName)) && has(object.metadata.uid) && has(oldObject.metadata.uid) && object.metadata.uid != "" && object.metadata.uid == oldObject.metadata.uid && has(object.metadata.resourceVersion) && has(oldObject.metadata.resourceVersion) && object.metadata.resourceVersion != "" && object.metadata.resourceVersion == oldObject.metadata.resourceVersion && object.metadata.generation == oldObject.metadata.generation && has(object.metadata.labels) && has(oldObject.metadata.labels) && object.metadata.labels == oldObject.metadata.labels && ((!has(object.metadata.annotations) && !has(oldObject.metadata.annotations)) || (has(object.metadata.annotations) && has(oldObject.metadata.annotations) && object.metadata.annotations == oldObject.metadata.annotations)) && ((!has(object.metadata.ownerReferences) && !has(oldObject.metadata.ownerReferences)) || (has(object.metadata.ownerReferences) && has(oldObject.metadata.ownerReferences) && object.metadata.ownerReferences == oldObject.metadata.ownerReferences)) && ((!has(object.metadata.finalizers) && !has(oldObject.metadata.finalizers)) || (has(object.metadata.finalizers) && has(oldObject.metadata.finalizers) && object.metadata.finalizers == oldObject.metadata.finalizers))`
}

func parentHookTrackingFinalizerRemovalExpression() string {
	return `oldObject != null && object.spec == oldObject.spec && object.status == oldObject.status && object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && object.metadata.uid == oldObject.metadata.uid && has(object.metadata.generateName) == has(oldObject.metadata.generateName) && (!has(object.metadata.generateName) || object.metadata.generateName == oldObject.metadata.generateName) && has(object.metadata.creationTimestamp) == has(oldObject.metadata.creationTimestamp) && (!has(object.metadata.creationTimestamp) || object.metadata.creationTimestamp == oldObject.metadata.creationTimestamp) && object.metadata.generation == oldObject.metadata.generation && has(object.metadata.deletionTimestamp) == has(oldObject.metadata.deletionTimestamp) && (!has(object.metadata.deletionTimestamp) || object.metadata.deletionTimestamp == oldObject.metadata.deletionTimestamp) && has(object.metadata.deletionGracePeriodSeconds) == has(oldObject.metadata.deletionGracePeriodSeconds) && (!has(object.metadata.deletionGracePeriodSeconds) || object.metadata.deletionGracePeriodSeconds == oldObject.metadata.deletionGracePeriodSeconds) && has(object.metadata.labels) && has(oldObject.metadata.labels) && object.metadata.labels == oldObject.metadata.labels && has(object.metadata.annotations) == has(oldObject.metadata.annotations) && (!has(object.metadata.annotations) || object.metadata.annotations == oldObject.metadata.annotations) && has(object.metadata.ownerReferences) && has(oldObject.metadata.ownerReferences) && object.metadata.ownerReferences == oldObject.metadata.ownerReferences && has(oldObject.metadata.finalizers) && oldObject.metadata.finalizers.filter(finalizer, finalizer == "batch.kubernetes.io/job-tracking").size() == 1 && ((!has(object.metadata.finalizers) && oldObject.metadata.finalizers.size() == 1) || (has(object.metadata.finalizers) && object.metadata.finalizers == oldObject.metadata.finalizers.filter(finalizer, finalizer != "batch.kubernetes.io/job-tracking")))`
}

func parentHookPodStatusFields() []string {
	return []string{
		"observedGeneration",
		"phase",
		"conditions",
		"message",
		"reason",
		"nominatedNodeName",
		"hostIP",
		"hostIPs",
		"podIP",
		"podIPs",
		"startTime",
		"initContainerStatuses",
		"containerStatuses",
		"qosClass",
		"ephemeralContainerStatuses",
		"resize",
		"resourceClaimStatuses",
		"extendedResourceClaimStatus",
		"allocatedResources",
		"resources",
		"nodeAllocatableResourceClaimStatuses",
	}
}

func parentHookSchedulerStatusDeltaExpression() string {
	parts := make([]string, 0, len(parentHookPodStatusFields())-1)
	for _, field := range parentHookPodStatusFields() {
		if field == "nominatedNodeName" {
			continue
		}
		parts = append(parts, fmt.Sprintf(
			`((!has(dyn(object.status).%[1]s) && !has(dyn(oldObject.status).%[1]s)) || (has(dyn(object.status).%[1]s) && has(dyn(oldObject.status).%[1]s) && dyn(object.status).%[1]s == dyn(oldObject.status).%[1]s))`,
			field,
		))
	}
	return strings.Join(parts, " && ")
}

func (g *ParentWorkloadGuard) hookImageCheckJobName() string {
	base := g.rollout.HookServiceAccountName
	if len(base) > 51 {
		base = base[:51]
	}
	return strings.TrimSuffix(base, "-") + "-image-check"
}

func (g *ParentWorkloadGuard) hookImageCheckJobPattern() string {
	base, _ := NewServiceAccountOriginGuard(g.rollout).hookServiceAccountBase()
	return "^" + regexp.QuoteMeta(base+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{1,12}-image-check$`
}

func (g *ParentWorkloadGuard) stableConvergenceMarkerPattern() string {
	return "^" + regexp.QuoteMeta(admissionConvergenceMarkerPrefix) + `[1-9][0-9]*-` + admissionConvergenceReleaseDigest(g.rollout.ReleaseNamespace, g.rollout.ReleaseName) + `$`
}

func (g *ParentWorkloadGuard) readinessMarker() *corev1.ConfigMap {
	immutable := true
	return &corev1.ConfigMap{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ParentOriginReadyMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName),
			Namespace: g.rollout.ReleaseNamespace,
			Annotations: map[string]string{
				"helm.sh/resource-policy":          "keep",
				"meta.helm.sh/release-name":        g.rollout.ReleaseName,
				"meta.helm.sh/release-namespace":   g.rollout.ReleaseNamespace,
				parentOriginReadyVersionAnnotation: parentOriginReadyVersion,
			},
			Labels: map[string]string{
				managedByLabel:                parentOriginReadyManagedBy,
				instanceLabel:                 g.rollout.ReleaseName,
				"app.kubernetes.io/component": parentOriginReadyComponent,
			},
		},
		Immutable: &immutable,
		Data:      g.identityContract.MarkerData(),
	}
}

func (g *ParentWorkloadGuard) readinessMarkerMatchExpression() string {
	name := ParentOriginReadyMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	return fmt.Sprintf(
		`request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "configmaps" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && ((request.operation in ["CREATE", "UPDATE"] && request.name == %q) || (request.operation == "DELETE" && oldObject != null && oldObject.metadata.name == %q))`,
		g.rollout.ReleaseNamespace,
		name,
		name,
	)
}

func (g *ParentWorkloadGuard) stableConvergenceMarkerMatchExpression() string {
	pattern := g.stableConvergenceMarkerPattern()
	return fmt.Sprintf(
		`request.resource.group == "" && request.resource.version == "v1" && request.resource.resource == "configmaps" && (!has(request.subResource) || request.subResource == "") && request.namespace == %q && ((request.operation in ["CREATE", "UPDATE"] && request.name.matches(%q)) || (request.operation == "DELETE" && oldObject != null && oldObject.metadata.name.matches(%q)))`,
		g.rollout.ReleaseNamespace,
		pattern,
		pattern,
	)
}

func (g *ParentWorkloadGuard) readinessMarkerShapeExpression(object string, persisted bool) string {
	expected := g.readinessMarker()
	parts := []string{
		fmt.Sprintf(`%s != null`, object),
		fmt.Sprintf(`%s.metadata.name == %q`, object, expected.Name),
		fmt.Sprintf(`%s.metadata.namespace == %q`, object, expected.Namespace),
		fmt.Sprintf(`(!has(%s.metadata.generateName) || %s.metadata.generateName == "")`, object, object),
		fmt.Sprintf(`has(%s.metadata.annotations) && %s.metadata.annotations.size() == 4`, object, object),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, "helm.sh/resource-policy", "keep"),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, "meta.helm.sh/release-name", g.rollout.ReleaseName),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, "meta.helm.sh/release-namespace", g.rollout.ReleaseNamespace),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, parentOriginReadyVersionAnnotation, parentOriginReadyVersion),
		fmt.Sprintf(`has(%s.metadata.labels) && %s.metadata.labels.size() == 3`, object, object),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, managedByLabel, parentOriginReadyManagedBy),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, instanceLabel, g.rollout.ReleaseName),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, "app.kubernetes.io/component", parentOriginReadyComponent),
		fmt.Sprintf(`has(%s.data) && %s.data.size() == %d`, object, object, len(expected.Data)),
		fmt.Sprintf(`(!has(%s.binaryData) || %s.binaryData.size() == 0)`, object, object),
		fmt.Sprintf(`has(%s.immutable) && %s.immutable`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.ownerReferences) || %s.metadata.ownerReferences.size() == 0)`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.finalizers) || %s.metadata.finalizers.size() == 0)`, object, object),
		fmt.Sprintf(`!has(%s.metadata.deletionTimestamp)`, object),
		fmt.Sprintf(`!has(%s.metadata.deletionGracePeriodSeconds)`, object),
	}
	dataKeys := make([]string, 0, len(expected.Data))
	for key := range expected.Data {
		dataKeys = append(dataKeys, key)
	}
	slices.Sort(dataKeys)
	for _, key := range dataKeys {
		parts = append(parts, fmt.Sprintf(`%s.data[%q] == %q`, object, key, expected.Data[key]))
	}
	if persisted {
		parts = append(parts,
			fmt.Sprintf(`has(%s.metadata.uid) && %s.metadata.uid != ""`, object, object),
			fmt.Sprintf(`has(%s.metadata.resourceVersion) && %s.metadata.resourceVersion != ""`, object, object),
		)
	}
	return strings.Join(parts, " && ")
}

func (g *ParentWorkloadGuard) stableConvergenceMarkerShapeExpression(object string, persisted bool) string {
	return "((" + g.stableConvergenceMarkerStateShapeExpression(object, persisted, false) + ") || (" + g.stableConvergenceMarkerStateShapeExpression(object, persisted, true) + "))"
}

func (g *ParentWorkloadGuard) stableConvergenceMarkerStateShapeExpression(object string, persisted, sealed bool) string {
	_, teardownPattern := g.hookServiceAccountPatterns()
	releaseDigest := admissionConvergenceReleaseDigest(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	parts := []string{
		fmt.Sprintf(`%s != null`, object),
		fmt.Sprintf(`%s.metadata.name.matches(%q)`, object, g.stableConvergenceMarkerPattern()),
		fmt.Sprintf(`%s.metadata.name == %q + %s.metadata.annotations[%q] + %q`, object, admissionConvergenceMarkerPrefix, object, ReleaseSequenceAnnotation, "-"+releaseDigest),
		fmt.Sprintf(`%s.metadata.namespace == %q`, object, g.rollout.ReleaseNamespace),
		fmt.Sprintf(`(!has(%s.metadata.generateName) || %s.metadata.generateName == "")`, object, object),
		fmt.Sprintf(`has(%s.metadata.annotations) && %s.metadata.annotations.size() == 10`, object, object),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, "helm.sh/hook", "pre-install,pre-upgrade"),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, "helm.sh/hook-weight", admissionConvergenceMarkerHookWeight),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, "helm.sh/resource-policy", "keep"),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, admissionConvergenceVersionAnnotation, admissionConvergenceContractVersion),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, ReleaseNameAnnotation, g.rollout.ReleaseName),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, ReleaseNamespaceAnnotation, g.rollout.ReleaseNamespace),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches(%q)`, object, ReleaseSequenceAnnotation, `^[1-9][0-9]*$`),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches(%q)`, object, ManagerImageAnnotation, `^[^[:space:]@]+@sha256:[0-9a-f]{64}$`),
		fmt.Sprintf(`%s.metadata.annotations[%q].matches(%q)`, object, admissionConvergenceCleanupAnnotation, teardownPattern),
		fmt.Sprintf(`%s.metadata.annotations[%q] == %q`, object, PredecessorRetirementInventoryVersionAnnotation, PredecessorRetirementInventoryVersion),
		fmt.Sprintf(`has(%s.metadata.labels) && %s.metadata.labels.size() == 3`, object, object),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, managedByLabel, rolloutGuardManagedBy),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, instanceLabel, g.rollout.ReleaseName),
		fmt.Sprintf(`%s.metadata.labels[%q] == %q`, object, "app.kubernetes.io/component", admissionConvergenceComponent),
		fmt.Sprintf(`has(%s.data) && %s.data.size() == %d`, object, object, 2+boolToInt(sealed)),
		fmt.Sprintf(`%s.data[%q] == %s.metadata.annotations[%q]`, object, admissionConvergenceExpectedDataKey, object, ReleaseSequenceAnnotation),
		fmt.Sprintf(`%s.data[%q].matches(%q)`, object, admissionConvergenceAttemptDataKey, `^[0-9a-f]{64}$`),
		fmt.Sprintf(`(!has(%s.binaryData) || %s.binaryData.size() == 0)`, object, object),
		admissionConvergenceMarkerImmutableExpression(object, sealed),
		fmt.Sprintf(`(!has(%s.metadata.ownerReferences) || %s.metadata.ownerReferences.size() == 0)`, object, object),
		fmt.Sprintf(`(!has(%s.metadata.finalizers) || %s.metadata.finalizers.size() == 0)`, object, object),
		fmt.Sprintf(`!has(%s.metadata.deletionTimestamp)`, object),
		fmt.Sprintf(`!has(%s.metadata.deletionGracePeriodSeconds)`, object),
	}
	if persisted {
		parts = append(parts,
			fmt.Sprintf(`has(%s.metadata.uid) && %s.metadata.uid != ""`, object, object),
			fmt.Sprintf(`has(%s.metadata.resourceVersion) && %s.metadata.resourceVersion != ""`, object, object),
		)
	}
	return strings.Join(parts, " && ") + admissionConvergenceMarkerInventoryExpression(object, sealed)
}

func (g *ParentWorkloadGuard) stableConvergenceMarkerUpdateExpression() string {
	oldUnsealed := g.stableConvergenceMarkerStateShapeExpression("oldObject", true, false)
	oldSealed := g.stableConvergenceMarkerStateShapeExpression("oldObject", true, true)
	newSealed := g.stableConvergenceMarkerStateShapeExpression("object", true, true)
	identity := `object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && object.metadata.uid == oldObject.metadata.uid && object.metadata.resourceVersion == oldObject.metadata.resourceVersion && object.metadata.annotations == oldObject.metadata.annotations && object.metadata.labels == oldObject.metadata.labels`
	baseData := fmt.Sprintf(`object.data[%q] == oldObject.data[%q] && object.data[%q] == oldObject.data[%q]`, admissionConvergenceExpectedDataKey, admissionConvergenceExpectedDataKey, admissionConvergenceAttemptDataKey, admissionConvergenceAttemptDataKey)
	return fmt.Sprintf(`(%s) && (%s) && ((%s) || ((%s) && object.data == oldObject.data))`, identity, newSealed, oldUnsealed+" && "+baseData, oldSealed)
}

func (g *ParentWorkloadGuard) protectedHookPodExpression(object, hookPattern, teardownPattern, imageCheckPattern string) string {
	return fmt.Sprintf(`((has(%[1]s.spec.serviceAccountName) && (%[1]s.spec.serviceAccountName.matches(%[2]q) || %[1]s.spec.serviceAccountName.matches(%[3]q))) || (has(%[1]s.metadata.ownerReferences) && %[1]s.metadata.ownerReferences.exists(owner, owner.apiVersion == "batch/v1" && owner.kind == "Job" && owner.name.matches(%[4]q)) && has(%[1]s.metadata.labels) && %[1]s.metadata.labels["app.kubernetes.io/instance"] == %[5]q && %[1]s.metadata.labels["app.kubernetes.io/component"] == "crd-manager-image-check"))`, object, hookPattern, teardownPattern, imageCheckPattern, g.rollout.ReleaseName)
}

func (g *ParentWorkloadGuard) hookPodOriginPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := ParentHookPodOriginGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	hookPattern, teardownPattern := g.hookServiceAccountPatterns()
	imageCheckPattern := g.hookImageCheckJobPattern()
	protectedObject := g.protectedHookPodExpression("object", hookPattern, teardownPattern, imageCheckPattern)
	protectedOldObject := g.protectedHookPodExpression("oldObject", hookPattern, teardownPattern, imageCheckPattern)
	message := parentHookPodOriginDenialMessage()
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name, false),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
							Rule: admissionregistrationv1.Rule{
								APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
							},
						},
					},
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
							Rule: admissionregistrationv1.Rule{
								APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods/status"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
							},
						},
					},
				},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "release-hook-service-account-pattern",
				Expression: fmt.Sprintf(`request.namespace == %q && (((!has(request.subResource) || request.subResource == "") && request.operation in ["CREATE", "UPDATE"] && ((%s) || (request.operation == "UPDATE" && oldObject != null && (%s)))) || (has(request.subResource) && request.subResource == "status" && request.operation == "UPDATE" && oldObject != null && (%s)))`, g.rollout.ReleaseNamespace, protectedObject, protectedOldObject, protectedOldObject),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isCreate", Expression: `request.operation == "CREATE" && (!has(request.subResource) || request.subResource == "")`},
				{Name: "isMainUpdate", Expression: `request.operation == "UPDATE" && (!has(request.subResource) || request.subResource == "")`},
				{Name: "isStatusUpdate", Expression: `request.operation == "UPDATE" && has(request.subResource) && request.subResource == "status"`},
				{Name: "owner", Expression: `object.metadata.ownerReferences[0]`},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `variables.isCreate || variables.isMainUpdate || variables.isStatusUpdate`, Message: message},
				{Expression: `!(variables.isCreate || variables.isMainUpdate) || (` + parentHookJobControllerPrincipalExpression() + `)`, Message: message},
				{Expression: fmt.Sprintf(`!variables.isCreate || (object.metadata.namespace == request.namespace && ((variables.owner.name.matches(%q) && (!has(object.spec.serviceAccountName) || object.spec.serviceAccountName in ["", "default"]) && (!has(object.spec.serviceAccount) || object.spec.serviceAccount in ["", "default"])) || (has(object.spec.serviceAccountName) && (object.spec.serviceAccountName.matches(%q) || object.spec.serviceAccountName.matches(%q)))))`, imageCheckPattern, hookPattern, teardownPattern), Message: message},
				{Expression: `!variables.isCreate || (has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1)`, Message: message},
				{Expression: `!variables.isCreate || (variables.owner.apiVersion == "batch/v1" && variables.owner.kind == "Job" && has(variables.owner.name) && variables.owner.name != "" && has(variables.owner.uid) && variables.owner.uid != "" && has(variables.owner.controller) && variables.owner.controller && has(variables.owner.blockOwnerDeletion) && variables.owner.blockOwnerDeletion)`, Message: message},
				{Expression: `!variables.isCreate || (has(object.metadata.labels) && ["batch.kubernetes.io/job-name", "batch.kubernetes.io/controller-uid"].all(key, key in object.metadata.labels) && object.metadata.labels["batch.kubernetes.io/job-name"] == variables.owner.name && object.metadata.labels["batch.kubernetes.io/controller-uid"] == variables.owner.uid)`, Message: message},
				{Expression: `!variables.isCreate || (` + generatedPodNameValidationExpression("variables.owner.name") + `)`, Message: message},
				{Expression: fmt.Sprintf(`!variables.isCreate || !variables.owner.name.matches(%q) || (object.metadata.labels["app.kubernetes.io/instance"] == %q && object.metadata.labels["app.kubernetes.io/component"] == "crd-manager-image-check")`, imageCheckPattern, g.rollout.ReleaseName), Message: message},
				{Expression: `!variables.isMainUpdate || (` + parentHookTrackingFinalizerRemovalExpression() + `)`, Message: message},
				{Expression: fmt.Sprintf(`!variables.isStatusUpdate || ((%s) || ((%s) && (%s)))`, parentHookNodePrincipalExpression(), parentHookSchedulerPrincipalExpression(), parentHookSchedulerStatusDeltaExpression()), Message: message},
				{Expression: `!variables.isStatusUpdate || (` + parentHookStatusPreservesIdentityExpression() + `)`, Message: message},
				{Expression: fmt.Sprintf(`!variables.isStatusUpdate || ((has(oldObject.spec.serviceAccountName) && (oldObject.spec.serviceAccountName.matches(%q) || oldObject.spec.serviceAccountName.matches(%q))) || (has(oldObject.metadata.ownerReferences) && oldObject.metadata.ownerReferences.size() == 1 && oldObject.metadata.ownerReferences[0].name.matches(%q) && (!has(oldObject.spec.serviceAccountName) || oldObject.spec.serviceAccountName in ["", "default"]) && has(oldObject.metadata.labels) && oldObject.metadata.labels["app.kubernetes.io/instance"] == %q && oldObject.metadata.labels["app.kubernetes.io/component"] == "crd-manager-image-check"))`, hookPattern, teardownPattern, imageCheckPattern, g.rollout.ReleaseName), Message: message},
			},
		},
	}
	return policy
}

// legacyHookPodOriginPolicy freezes the release-stable v1 contract exactly as
// it was emitted before the append-only v2 boundary. It is verify-only: fresh
// installs neither render nor require it, while uninstall can reject a drifted
// retained object before replacing it with the retirement marker.
func (g *ParentWorkloadGuard) legacyHookPodOriginPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := legacyParentHookPodOriginGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	hookPattern, teardownPattern := g.hookServiceAccountPatterns()
	message := parentHookPodOriginDenialMessage()
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.legacyOriginMetadata(name, parentHookPodOriginPolicyWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"pods"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
						},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "release-hook-service-account-pattern",
				Expression: fmt.Sprintf(`request.namespace == %q && has(object.spec.serviceAccountName) && (object.spec.serviceAccountName.matches(%q) || object.spec.serviceAccountName.matches(%q))`, g.rollout.ReleaseNamespace, hookPattern, teardownPattern),
			}},
			Variables: []admissionregistrationv1.Variable{{Name: "owner", Expression: `object.metadata.ownerReferences[0]`}},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `!has(request.subResource) || request.subResource == ""`, Message: message},
				{Expression: `request.userInfo.username in ["system:kube-controller-manager", "system:serviceaccount:kube-system:job-controller"]`, Message: message},
				{Expression: fmt.Sprintf(`object.metadata.namespace == request.namespace && has(object.spec.serviceAccountName) && (object.spec.serviceAccountName.matches(%q) || object.spec.serviceAccountName.matches(%q))`, hookPattern, teardownPattern), Message: message},
				{Expression: `has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1`, Message: message},
				{Expression: `variables.owner.apiVersion == "batch/v1" && variables.owner.kind == "Job" && has(variables.owner.name) && variables.owner.name != "" && has(variables.owner.uid) && variables.owner.uid != "" && has(variables.owner.controller) && variables.owner.controller && has(variables.owner.blockOwnerDeletion) && variables.owner.blockOwnerDeletion`, Message: message},
				{Expression: `has(object.metadata.labels) && ["batch.kubernetes.io/job-name", "batch.kubernetes.io/controller-uid"].all(key, key in object.metadata.labels) && object.metadata.labels["batch.kubernetes.io/job-name"] == variables.owner.name && object.metadata.labels["batch.kubernetes.io/controller-uid"] == variables.owner.uid`, Message: message},
				{Expression: generatedPodNameValidationExpression("variables.owner.name"), Message: message},
			},
		},
	}
}

func (g *ParentWorkloadGuard) hookJobOriginPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := ParentHookJobOriginGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	hookPattern, teardownPattern := g.hookServiceAccountPatterns()
	message := parentHookOriginDenialMessage()
	namespaceGuard := NamespaceDeletionGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	authority := parentHookAdmissionAuthorityExpression(namespaceGuard)
	imageCheckPattern := g.hookImageCheckJobPattern()
	jobMatch := fmt.Sprintf(
		`request.namespace == %q && request.resource.group == "batch" && request.resource.version == "v1" && request.resource.resource == "jobs" && (((request.operation in ["CREATE", "UPDATE"] && (!has(request.subResource) || request.subResource == "")) && ((has(object.spec.template.spec.serviceAccountName) && (object.spec.template.spec.serviceAccountName.matches(%q) || object.spec.template.spec.serviceAccountName.matches(%q))) || (request.operation == "UPDATE" && oldObject != null && has(oldObject.spec.template.spec.serviceAccountName) && (oldObject.spec.template.spec.serviceAccountName.matches(%q) || oldObject.spec.template.spec.serviceAccountName.matches(%q))) || request.name.matches(%q))) || ((request.operation == "DELETE" || (request.operation == "UPDATE" && has(request.subResource) && request.subResource == "status")) && oldObject != null && ((has(oldObject.spec.template.spec.serviceAccountName) && (oldObject.spec.template.spec.serviceAccountName.matches(%q) || oldObject.spec.template.spec.serviceAccountName.matches(%q))) || oldObject.metadata.name.matches(%q))))`,
		g.rollout.ReleaseNamespace, hookPattern, teardownPattern, hookPattern, teardownPattern, imageCheckPattern, hookPattern, teardownPattern, imageCheckPattern,
	)
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name, false),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete},
							Rule: admissionregistrationv1.Rule{
								APIGroups: []string{"batch"}, APIVersions: []string{"v1"}, Resources: []string{"jobs"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
							},
						},
					},
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
							Rule: admissionregistrationv1.Rule{
								APIGroups: []string{"batch"}, APIVersions: []string{"v1"}, Resources: []string{"jobs/status"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
							},
						},
					},
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete},
							Rule: admissionregistrationv1.Rule{
								APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
							},
						},
					},
				},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "release-hook-service-account-pattern",
				Expression: "(" + jobMatch + ") || (" + g.readinessMarkerMatchExpression() + ") || (" + g.stableConvergenceMarkerMatchExpression() + ")",
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isProtectedJob", Expression: `request.resource.group == "batch" && request.resource.version == "v1" && request.resource.resource == "jobs"`},
				{Name: "isMainWrite", Expression: `request.operation in ["CREATE", "UPDATE"] && (!has(request.subResource) || request.subResource == "")`},
				{Name: "isStatusUpdate", Expression: `request.operation == "UPDATE" && has(request.subResource) && request.subResource == "status"`},
				{Name: "isDelete", Expression: `request.operation == "DELETE" && (!has(request.subResource) || request.subResource == "")`},
				{Name: "effectiveName", Expression: `request.operation == "DELETE" && oldObject != null ? oldObject.metadata.name : request.name`},
				{Name: "isReadinessMarker", Expression: fmt.Sprintf(`request.resource.group == "" && request.resource.resource == "configmaps" && variables.effectiveName == %q`, ParentOriginReadyMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName))},
				{Name: "isConvergenceMarker", Expression: fmt.Sprintf(`request.resource.group == "" && request.resource.resource == "configmaps" && variables.effectiveName.matches(%q)`, g.stableConvergenceMarkerPattern())},
				{Name: "isMarker", Expression: `variables.isReadinessMarker || variables.isConvergenceMarker`},
				{Name: "isServiceAccountObjectConvergenceProbe", Expression: serviceAccountObjectGuardProbeRequestExpression(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `(variables.isProtectedJob || variables.isMarker) && (variables.isMainWrite || variables.isStatusUpdate || variables.isDelete)`, Message: message},
				{Expression: fmt.Sprintf(`!variables.isProtectedJob || !variables.isMainWrite || ((has(object.spec.template.spec.serviceAccountName) && (object.spec.template.spec.serviceAccountName.matches(%q) || object.spec.template.spec.serviceAccountName.matches(%q))) || (request.name.matches(%q) && (!has(object.spec.template.spec.serviceAccountName) || object.spec.template.spec.serviceAccountName in ["", "default"]) && (!has(object.spec.template.spec.serviceAccount) || object.spec.template.spec.serviceAccount in ["", "default"])))`, hookPattern, teardownPattern, imageCheckPattern), Message: message},
				{Expression: `!variables.isProtectedJob || !variables.isMainWrite || (` + authority + `)`, Message: message},
				{Expression: fmt.Sprintf(`!variables.isProtectedJob || !(variables.isStatusUpdate || variables.isDelete) || (oldObject != null && ((has(oldObject.spec.template.spec.serviceAccountName) && (oldObject.spec.template.spec.serviceAccountName.matches(%q) || oldObject.spec.template.spec.serviceAccountName.matches(%q))) || (oldObject.metadata.name.matches(%q) && (!has(oldObject.spec.template.spec.serviceAccountName) || oldObject.spec.template.spec.serviceAccountName in ["", "default"]) && (!has(oldObject.spec.template.spec.serviceAccount) || oldObject.spec.template.spec.serviceAccount in ["", "default"]))))`, hookPattern, teardownPattern, imageCheckPattern), Message: message},
				{Expression: `!variables.isProtectedJob || !variables.isStatusUpdate || (` + parentHookJobControllerPrincipalExpression() + `)`, Message: message},
				{Expression: `!variables.isProtectedJob || !variables.isStatusUpdate || (` + parentHookStatusPreservesIdentityExpression() + `)`, Message: message},
				{Expression: `!variables.isProtectedJob || !variables.isDelete || (` + parentHookTerminalJobExpression("oldObject") + `)`, Message: message},
				{Expression: `!variables.isProtectedJob || !variables.isDelete || (` + authority + `)`, Message: message},
				{Expression: `!variables.isMarker || variables.isServiceAccountObjectConvergenceProbe || (` + authority + `)`, Message: message},
				{Expression: `!variables.isServiceAccountObjectConvergenceProbe || request.dryRun == true`, Message: message},
				{Expression: `!variables.isReadinessMarker || !variables.isMainWrite || (request.operation == "CREATE" ? (` + g.readinessMarkerShapeExpression("object", false) + `) : (` + g.readinessMarkerShapeExpression("object", true) + `))`, Message: message},
				{Expression: `!variables.isReadinessMarker || !(request.operation in ["UPDATE", "DELETE"]) || (` + g.readinessMarkerShapeExpression("oldObject", true) + `)`, Message: message},
				{Expression: `!variables.isConvergenceMarker || !variables.isMainWrite || (request.operation == "CREATE" ? (` + g.stableConvergenceMarkerStateShapeExpression("object", false, false) + `) : (` + g.stableConvergenceMarkerUpdateExpression() + `))`, Message: message},
				{Expression: `!variables.isConvergenceMarker || !(request.operation in ["UPDATE", "DELETE"]) || (` + g.stableConvergenceMarkerShapeExpression("oldObject", true) + `)`, Message: message},
				{Expression: `!variables.isMarker || request.operation != "UPDATE" || (object.metadata.uid == oldObject.metadata.uid && object.metadata.resourceVersion == oldObject.metadata.resourceVersion)`, Message: message},
			},
		},
	}
	return policy
}

// legacyHookJobOriginPolicy freezes the release-stable v1 Job contract. The
// retained v1 object is deliberately not adopted or updated: an exact instance
// can coexist with v2 until uninstall, while any semantic drift fails closed.
func (g *ParentWorkloadGuard) legacyHookJobOriginPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := legacyParentHookJobOriginGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	hookPattern, teardownPattern := g.hookServiceAccountPatterns()
	message := parentHookOriginDenialMessage()
	authority := parentHookAdmissionAuthorityExpression(NamespaceDeletionGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName))
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.legacyOriginMetadata(name, parentHookOriginPolicyWeight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{"batch"}, APIVersions: []string{"v1"}, Resources: []string{"jobs"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
						},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "release-hook-service-account-pattern",
				Expression: fmt.Sprintf(
					`request.namespace == %q && ((has(object.spec.template.spec.serviceAccountName) && (object.spec.template.spec.serviceAccountName.matches(%q) || object.spec.template.spec.serviceAccountName.matches(%q))) || (request.operation == "UPDATE" && has(oldObject.spec.template.spec.serviceAccountName) && (oldObject.spec.template.spec.serviceAccountName.matches(%q) || oldObject.spec.template.spec.serviceAccountName.matches(%q))))`,
					g.rollout.ReleaseNamespace, hookPattern, teardownPattern, hookPattern, teardownPattern,
				),
			}},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `!has(request.subResource) || request.subResource == ""`, Message: message},
				{Expression: fmt.Sprintf(`has(object.spec.template.spec.serviceAccountName) && (object.spec.template.spec.serviceAccountName.matches(%q) || object.spec.template.spec.serviceAccountName.matches(%q))`, hookPattern, teardownPattern), Message: message},
				{Expression: authority, Message: message},
			},
		},
	}
}

func (g *ParentWorkloadGuard) legacyOriginEntries() []parentGuardEntry {
	jobPolicy := g.legacyHookJobOriginPolicy()
	jobBinding := g.binding(jobPolicy.Name, false)
	jobBinding.ObjectMeta = g.legacyOriginMetadata(jobPolicy.Name, parentHookOriginBindingWeight)
	podPolicy := g.legacyHookPodOriginPolicy()
	podBinding := g.binding(podPolicy.Name, false)
	podBinding.ObjectMeta = g.legacyOriginMetadata(podPolicy.Name, parentHookPodOriginBindingWeight)
	return []parentGuardEntry{
		{
			name: jobPolicy.Name, description: "legacy hook parent origin guard",
			policy: jobPolicy, binding: jobBinding,
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return g.verifyPolicy(actual, jobPolicy, false)
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return g.verifyBinding(actual, jobBinding, false)
			},
		},
		{
			name: podPolicy.Name, description: "legacy hook Pod origin guard",
			policy: podPolicy, binding: podBinding,
			verifyPolicy: func(actual *admissionregistrationv1.ValidatingAdmissionPolicy) error {
				return g.verifyPolicy(actual, podPolicy, false)
			},
			verifyBinding: func(actual *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
				return g.verifyBinding(actual, podBinding, false)
			},
		},
	}
}

func (g *ParentWorkloadGuard) legacyOriginMetadata(name, weight string) metav1.ObjectMeta {
	metadata := g.metadata(name, false)
	metadata.Annotations["helm.sh/hook"] = "pre-install,pre-upgrade"
	metadata.Annotations["helm.sh/hook-weight"] = weight
	metadata.Annotations["helm.sh/resource-policy"] = "keep"
	return metadata
}

func (g *ParentWorkloadGuard) legacyOriginRetirementPolicy(name, weight string) *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.legacyOriginRetirementMetadata(name, weight),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
					RuleWithOperations: admissionregistrationv1.RuleWithOperations{
						Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
						Rule: admissionregistrationv1.Rule{
							APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
						},
					},
				}},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "retired-parent-origin-marker",
				Expression: fmt.Sprintf(`request.namespace == %q && request.name == %q`, g.rollout.ReleaseNamespace, ParentOriginReadyMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)),
			}},
			Validations: []admissionregistrationv1.Validation{{
				Expression: "true",
				Message:    "Retired parent-origin boundary is inert",
			}},
		},
	}
}

func (g *ParentWorkloadGuard) legacyOriginRetirementBinding(name, weight string) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	binding := g.binding(name, false)
	binding.ObjectMeta = g.legacyOriginRetirementMetadata(name, weight)
	return binding
}

func (g *ParentWorkloadGuard) legacyOriginRetirementMetadata(name, weight string) metav1.ObjectMeta {
	metadata := g.metadata(name, false)
	metadata.Annotations["helm.sh/hook"] = "post-upgrade"
	metadata.Annotations["helm.sh/hook-weight"] = weight
	metadata.Annotations["helm.sh/hook-delete-policy"] = "before-hook-creation,hook-succeeded"
	metadata.Labels["app.kubernetes.io/component"] = "parent-workload-guard-retirement"
	return metadata
}

func (g *ParentWorkloadGuard) legacyOriginRetirementEntries() []parentGuardEntry {
	jobName := legacyParentHookJobOriginGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	jobPolicy := g.legacyOriginRetirementPolicy(jobName, legacyHookOriginRetiredPolicyWeight)
	jobBinding := g.legacyOriginRetirementBinding(jobName, legacyHookOriginRetiredBindingWeight)
	podName := legacyParentHookPodOriginGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	podPolicy := g.legacyOriginRetirementPolicy(podName, legacyHookPodOriginRetiredPolicyWeight)
	podBinding := g.legacyOriginRetirementBinding(podName, legacyHookPodOriginRetiredBindingWeight)
	return []parentGuardEntry{
		{name: jobName, policy: jobPolicy, binding: jobBinding},
		{name: podName, policy: podPolicy, binding: podBinding},
	}
}

func (g *ParentWorkloadGuard) hookJobContractPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := ParentHookJobContractPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage)
	message := parentHookContractDenialMessage(g.rollout.ReleaseSequence)
	imageCheckJob := g.hookImageCheckJobName()
	identityJob := HookIdentityProbeJobName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage)
	preflightJob := g.rollout.hookJobName("preflight")
	reconcileJob := g.rollout.hookJobName("reconcile")
	quiesceJob := g.rollout.hookJobName("teardown-quiesce")
	teardownJob := g.rollout.hookJobName("teardown")
	teardownServiceAccount, _ := TeardownServiceAccountName(g.rollout.HookServiceAccountName, g.rollout.ReleaseSequence)
	contract := g.hookJobContractExpressions(imageCheckJob, identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, teardownServiceAccount)
	validations := make([]admissionregistrationv1.Validation, 0, len(contract)+6)
	for _, expression := range contract {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: `!variables.isMainWrite || (` + expression + `)`,
			Message:    message,
		})
	}
	namespaceGuard := NamespaceDeletionGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	authority := parentHookAdmissionAuthorityExpression(namespaceGuard)
	for _, oldContract := range parentHookOldContractChunks(contract[1:]) {
		validations = append(validations, admissionregistrationv1.Validation{
			Expression: `!(variables.isStatusUpdate || variables.isDelete) || (oldObject != null && (` + oldContract + `))`,
			Message:    message,
		})
	}
	validations = append(validations,
		admissionregistrationv1.Validation{
			Expression: `!variables.isStatusUpdate || (` + parentHookJobControllerPrincipalExpression() + `)`,
			Message:    message,
		},
		admissionregistrationv1.Validation{
			Expression: `!variables.isStatusUpdate || (` + parentHookStatusPreservesIdentityExpression() + `)`,
			Message:    message,
		},
		admissionregistrationv1.Validation{
			Expression: `!variables.isDelete || (` + parentHookTerminalJobExpression("oldObject") + `)`,
			Message:    message,
		},
		admissionregistrationv1.Validation{
			Expression: `!variables.isDelete || (` + authority + `)`,
			Message:    message,
		},
	)
	metadata := g.metadata(name, true)
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metadata,
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionregistrationv1.MatchResources{
				MatchPolicy:       &exact,
				NamespaceSelector: &metav1.LabelSelector{},
				ObjectSelector:    &metav1.LabelSelector{},
				ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update, admissionregistrationv1.Delete},
							Rule: admissionregistrationv1.Rule{
								APIGroups: []string{"batch"}, APIVersions: []string{"v1"}, Resources: []string{"jobs"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
							},
						},
					},
					{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
							Rule: admissionregistrationv1.Rule{
								APIGroups: []string{"batch"}, APIVersions: []string{"v1"}, Resources: []string{"jobs/status"}, Scope: scopePtr(admissionregistrationv1.NamespacedScope),
							},
						},
					},
				},
			},
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "candidate-hook-service-account",
				Expression: fmt.Sprintf(`request.namespace == %q && (request.name in [%q, %q, %q, %q, %q, %q] || (oldObject != null && oldObject.metadata.name in [%q, %q, %q, %q, %q, %q]) || (object != null && has(object.spec.template.spec.serviceAccountName) && object.spec.template.spec.serviceAccountName in [%q, %q]) || (oldObject != null && has(oldObject.spec.template.spec.serviceAccountName) && oldObject.spec.template.spec.serviceAccountName in [%q, %q]))`, g.rollout.ReleaseNamespace, imageCheckJob, identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, imageCheckJob, identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, g.rollout.HookServiceAccountName, teardownServiceAccount, g.rollout.HookServiceAccountName, teardownServiceAccount),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isMainWrite", Expression: `request.operation in ["CREATE", "UPDATE"] && (!has(request.subResource) || request.subResource == "")`},
				{Name: "isStatusUpdate", Expression: `request.operation == "UPDATE" && has(request.subResource) && request.subResource == "status"`},
				{Name: "isDelete", Expression: `request.operation == "DELETE" && (!has(request.subResource) || request.subResource == "")`},
				{Name: "effectiveName", Expression: `request.operation == "DELETE" && oldObject != null ? oldObject.metadata.name : request.name`},
				{Name: "isImageCheck", Expression: fmt.Sprintf(`variables.effectiveName == %q`, imageCheckJob)},
				{Name: "isIdentity", Expression: fmt.Sprintf(`variables.effectiveName == %q`, identityJob)},
				{Name: "isPreflight", Expression: fmt.Sprintf(`variables.effectiveName == %q`, preflightJob)},
				{Name: "isQuiesce", Expression: fmt.Sprintf(`variables.effectiveName == %q`, quiesceJob)},
				{Name: "isTeardown", Expression: fmt.Sprintf(`variables.effectiveName == %q`, teardownJob)},
			},
			Validations: validations,
		},
	}
	addAdmissionConvergenceDependencyProbe(
		policy,
		g.rollout.ReleaseNamespace,
		AdmissionConvergenceMarkerName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence),
		hookIdentityDigest(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage),
	)
	return policy
}

func parentHookOldContractChunks(expressions []string) []string {
	const maxChunkBytes = 80_000

	chunks := make([]string, 0, 4)
	current := ""
	for _, expression := range expressions {
		part := "(" + strings.ReplaceAll(expression, "object.", "oldObject.") + ")"
		separator := ""
		if current != "" {
			separator = " && "
		}
		if current != "" && len(current)+len(separator)+len(part) >= maxChunkBytes {
			chunks = append(chunks, current)
			current = part
			continue
		}
		current += separator + part
	}
	if current != "" {
		chunks = append(chunks, current)
	}
	return chunks
}

func (g *ParentWorkloadGuard) hookJobContractExpressions(imageCheckJob, identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, teardownServiceAccount string) []string {
	pod := "object.spec.template.spec"
	templateMetadata := "object.spec.template.metadata"
	container := pod + ".containers[0]"
	volume := pod + ".volumes[0]"
	sources := volume + ".projected.sources"
	priorityClassExpression := fmt.Sprintf(`!has(%s.priorityClassName)`, pod)
	if g.rollout.PriorityClassName != "" {
		priorityClassExpression = fmt.Sprintf(`(variables.effectiveName == %q && has(%s.priorityClassName) && %s.priorityClassName == %q) || (variables.effectiveName != %q && !has(%s.priorityClassName))`, reconcileJob, pod, pod, g.rollout.PriorityClassName, reconcileJob, pod)
	}
	return []string{
		`!has(request.subResource) || request.subResource == ""`,
		fmt.Sprintf(`variables.effectiveName in [%q, %q, %q, %q, %q, %q] && object.metadata.name == variables.effectiveName && object.metadata.namespace == %q && (!has(object.metadata.generateName) || object.metadata.generateName == "")`, imageCheckJob, identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, g.rollout.ReleaseNamespace),
		`(!has(object.metadata.ownerReferences) || object.metadata.ownerReferences.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0)`,
		fmt.Sprintf(`has(object.metadata.labels) && object.metadata.labels["app.kubernetes.io/instance"] == %q && object.metadata.labels["app.kubernetes.io/component"] == (variables.isImageCheck ? "crd-manager-image-check" : (variables.isIdentity ? "hook-identity-probe" : (variables.isPreflight ? "crd-manager-preflight" : (variables.isQuiesce ? "crd-manager-teardown-quiesce" : (variables.isTeardown ? "crd-manager-teardown" : "crd-manager")))))`, g.rollout.ReleaseName),
		`has(object.metadata.annotations) && object.metadata.annotations.size() == 3 && object.metadata.annotations["helm.sh/hook"] == ((variables.isQuiesce || variables.isTeardown) ? "pre-delete" : "pre-install,pre-upgrade") && object.metadata.annotations["helm.sh/hook-delete-policy"] == ((variables.isQuiesce || variables.isTeardown) ? "before-hook-creation,hook-succeeded" : "before-hook-creation,hook-succeeded,hook-failed") && object.metadata.annotations["helm.sh/hook-weight"] == (variables.isImageCheck ? "-130" : (variables.isIdentity ? "-105" : (variables.isPreflight ? "-60" : (variables.isQuiesce ? "-10" : "0"))))`,
		fmt.Sprintf(`(!has(%s.annotations) || %s.annotations.size() == 0) && (!has(%s.ownerReferences) || %s.ownerReferences.size() == 0) && (!has(%s.finalizers) || %s.finalizers.size() == 0)`, templateMetadata, templateMetadata, templateMetadata, templateMetadata, templateMetadata, templateMetadata),
		`has(object.spec.activeDeadlineSeconds) && object.spec.activeDeadlineSeconds == (variables.isImageCheck ? 120 : ((variables.isIdentity || variables.isPreflight || variables.isQuiesce || variables.isTeardown) ? 210 : 390))`,
		`has(object.spec.backoffLimit) && object.spec.backoffLimit == 0 && (!has(object.spec.backoffLimitPerIndex)) && (!has(object.spec.maxFailedIndexes))`,
		`(!has(object.spec.ttlSecondsAfterFinished)) && (!has(object.spec.podFailurePolicy)) && (!has(object.spec.successPolicy))`,
		`(!has(object.spec.suspend) || !object.spec.suspend) && (!has(object.spec.manualSelector) || !object.spec.manualSelector) && (!has(object.spec.parallelism) || object.spec.parallelism == 1) && (!has(object.spec.completions) || object.spec.completions == 1) && (!has(object.spec.completionMode) || object.spec.completionMode == "NonIndexed")`,
		fmt.Sprintf(`(variables.isImageCheck ? ((!has(%[1]s.serviceAccountName) || %[1]s.serviceAccountName in ["", "default"]) && (!has(%[1]s.serviceAccount) || %[1]s.serviceAccount in ["", "default"])) : (has(%[1]s.serviceAccountName) && %[1]s.serviceAccountName == (variables.isTeardown ? %[2]q : %[3]q) && (!has(%[1]s.serviceAccount) || %[1]s.serviceAccount == (variables.isTeardown ? %[2]q : %[3]q)))) && has(%[1]s.automountServiceAccountToken) && !%[1]s.automountServiceAccountToken`, pod, teardownServiceAccount, g.rollout.HookServiceAccountName),
		fmt.Sprintf(`%s.restartPolicy == "Never" && (!has(%s.nodeName) || %s.nodeName == "") && (!has(%s.nodeSelector) || %s.nodeSelector.size() == 0) && !has(%s.affinity) && (!has(%s.tolerations) || %s.tolerations.size() == 0)`, pod, pod, pod, pod, pod, pod, pod, pod),
		fmt.Sprintf(`(!has(%s.hostNetwork) || !%s.hostNetwork) && (!has(%s.hostPID) || !%s.hostPID) && (!has(%s.hostIPC) || !%s.hostIPC) && (!has(%s.shareProcessNamespace) || !%s.shareProcessNamespace)`, pod, pod, pod, pod, pod, pod, pod, pod),
		fmt.Sprintf(`!has(%s.hostAliases) && !has(%s.hostname) && !has(%s.subdomain) && (!has(%s.setHostnameAsFQDN) || !%s.setHostnameAsFQDN) && !has(%s.runtimeClassName) && (!has(%s.readinessGates) || %s.readinessGates.size() == 0) && (!has(%s.resourceClaims) || %s.resourceClaims.size() == 0) && (!has(%s.schedulingGates) || %s.schedulingGates.size() == 0)`, pod, pod, pod, pod, pod, pod, pod, pod, pod, pod, pod, pod),
		fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.runAsNonRoot) && %[1]s.securityContext.runAsNonRoot && has(%[1]s.securityContext.runAsUser) && %[1]s.securityContext.runAsUser == 65532 && has(%[1]s.securityContext.runAsGroup) && %[1]s.securityContext.runAsGroup == 65532 && !has(%[1]s.securityContext.fsGroup) && !has(%[1]s.securityContext.fsGroupChangePolicy) && !has(%[1]s.securityContext.supplementalGroups) && !has(%[1]s.securityContext.supplementalGroupsPolicy) && !has(%[1]s.securityContext.seLinuxOptions) && !has(%[1]s.securityContext.seLinuxChangePolicy) && !has(%[1]s.securityContext.windowsOptions) && !has(%[1]s.securityContext.appArmorProfile) && has(%[1]s.securityContext.seccompProfile) && %[1]s.securityContext.seccompProfile.type == "RuntimeDefault" && !has(%[1]s.securityContext.seccompProfile.localhostProfile) && (!has(%[1]s.securityContext.sysctls) || %[1]s.securityContext.sysctls.size() == 0)`, pod),
		fmt.Sprintf(`has(%s.containers) && %s.containers.size() == 1 && (!has(%s.initContainers) || %s.initContainers.size() == 0) && (!has(%s.ephemeralContainers) || %s.ephemeralContainers.size() == 0)`, pod, pod, pod, pod, pod, pod),
		fmt.Sprintf(`%s.name == (variables.isImageCheck ? "image-check" : (variables.isIdentity ? "identity-probe" : (variables.isPreflight ? "crd-manager-preflight" : (variables.isQuiesce ? "crd-manager-teardown-quiesce" : (variables.isTeardown ? "crd-manager-teardown" : "crd-manager")))))`, container),
		fmt.Sprintf(`%s.image == %q && %s.command == ["/ptah-crd-manager"] && %s.imagePullPolicy in ["Always", "IfNotPresent", "Never"]`, container, g.rollout.ManagerImage, container, container),
		fmt.Sprintf(`!variables.isImageCheck || %s.args == ["image-check", %q, %q]`, container, "--release-sequence="+strconv.FormatInt(int64(g.rollout.ReleaseSequence), 10), "--manager-image="+g.rollout.ManagerImage),
		fmt.Sprintf(`!variables.isIdentity || %s`, g.rollout.hookArgsValidationExpression(container, "identity-probe")),
		fmt.Sprintf(`!variables.isPreflight || %s`, g.rollout.hookArgsValidationExpression(container, "preflight")),
		fmt.Sprintf(`variables.effectiveName != %q || %s`, reconcileJob, g.rollout.hookArgsValidationExpression(container, "reconcile")),
		fmt.Sprintf(`!variables.isQuiesce || %s`, g.rollout.hookArgsValidationExpression(container, "teardown-quiesce")),
		fmt.Sprintf(`!variables.isTeardown || %s`, g.rollout.hookArgsValidationExpression(container, "teardown")),
		hookContainerNoExecutionSideChannelsExpression(container),
		fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.allowPrivilegeEscalation) && !%[1]s.securityContext.allowPrivilegeEscalation && has(%[1]s.securityContext.readOnlyRootFilesystem) && %[1]s.securityContext.readOnlyRootFilesystem && (!has(%[1]s.securityContext.privileged) || !%[1]s.securityContext.privileged) && !has(%[1]s.securityContext.runAsUser) && !has(%[1]s.securityContext.runAsGroup) && !has(%[1]s.securityContext.runAsNonRoot) && !has(%[1]s.securityContext.procMount) && !has(%[1]s.securityContext.seLinuxOptions) && !has(%[1]s.securityContext.windowsOptions) && !has(%[1]s.securityContext.seccompProfile) && !has(%[1]s.securityContext.appArmorProfile) && has(%[1]s.securityContext.capabilities) && (!has(%[1]s.securityContext.capabilities.add) || %[1]s.securityContext.capabilities.add.size() == 0) && has(%[1]s.securityContext.capabilities.drop) && %[1]s.securityContext.capabilities.drop == ["ALL"]`, container),
		fmt.Sprintf(`has(dyn(%[1]s.resources).requests) && dyn(%[1]s.resources).requests.size() == 2 && quantity(string(dyn(%[1]s.resources).requests["cpu"])).compareTo(quantity("5m")) == 0 && quantity(string(dyn(%[1]s.resources).requests["memory"])).compareTo(quantity("16Mi")) == 0 && has(dyn(%[1]s.resources).limits) && dyn(%[1]s.resources).limits.size() == 1 && quantity(string(dyn(%[1]s.resources).limits["memory"])).compareTo(quantity("32Mi")) == 0 && (!has(dyn(%[1]s.resources).claims) || dyn(%[1]s.resources).claims.size() == 0) && (!has(%[1]s.resizePolicy) || %[1]s.resizePolicy.size() == 0)`, container),
		fmt.Sprintf(`variables.isImageCheck ? (!has(%[1]s.volumeMounts) || %[1]s.volumeMounts.size() == 0) : (has(%[1]s.volumeMounts) && %[1]s.volumeMounts.size() == 1 && %[1]s.volumeMounts[0].name == "api-access" && %[1]s.volumeMounts[0].mountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && has(%[1]s.volumeMounts[0].readOnly) && %[1]s.volumeMounts[0].readOnly && !has(%[1]s.volumeMounts[0].mountPropagation) && !has(%[1]s.volumeMounts[0].subPath) && !has(%[1]s.volumeMounts[0].subPathExpr) && !has(%[1]s.volumeMounts[0].recursiveReadOnly))`, container),
		fmt.Sprintf(`variables.isImageCheck ? (!has(%[1]s.volumes) || %[1]s.volumes.size() == 0) : (has(%[1]s.volumes) && %[1]s.volumes.size() == 1 && %[2]s.name == "api-access" && has(%[2]s.projected) && has(%[2]s.projected.defaultMode) && %[2]s.projected.defaultMode == 420 && %[3]s.size() == 3)`, pod, volume, sources),
		fmt.Sprintf(`variables.isImageCheck || %s.exists(s, has(s.serviceAccountToken) && s.serviceAccountToken.path == "token" && has(s.serviceAccountToken.expirationSeconds) && s.serviceAccountToken.expirationSeconds == 3600 && !has(s.serviceAccountToken.audience))`, sources),
		fmt.Sprintf(`variables.isImageCheck || %s.exists(s, has(s.configMap) && s.configMap.name == "kube-root-ca.crt" && has(s.configMap.items) && s.configMap.items.size() == 1 && s.configMap.items[0].key == "ca.crt" && s.configMap.items[0].path == "ca.crt" && !has(s.configMap.items[0].mode))`, sources),
		fmt.Sprintf(`variables.isImageCheck || %s.exists(s, has(s.downwardAPI) && has(s.downwardAPI.items) && s.downwardAPI.items.size() == 1 && s.downwardAPI.items[0].path == "namespace" && has(s.downwardAPI.items[0].fieldRef) && s.downwardAPI.items[0].fieldRef.apiVersion == "v1" && s.downwardAPI.items[0].fieldRef.fieldPath == "metadata.namespace" && !has(s.downwardAPI.items[0].mode))`, sources),
		fmt.Sprintf(`variables.isImageCheck || %s.all(s, has(s.serviceAccountToken) || has(s.configMap) || has(s.downwardAPI))`, sources),
		fmt.Sprintf(`(!has(%[1]s.imagePullSecrets) || (%[1]s.imagePullSecrets.all(secret, secret.name != "") && %[1]s.imagePullSecrets.all(secret, %[1]s.imagePullSecrets.filter(other, other.name == secret.name).size() == 1)))`, pod),
		fmt.Sprintf(`(!has(%s.dnsPolicy) || %s.dnsPolicy == "ClusterFirst") && !has(%s.dnsConfig) && (!has(%s.schedulerName) || %s.schedulerName == "default-scheduler") && !has(%s.priority) && (%s) && !has(%s.preemptionPolicy)`, pod, pod, pod, pod, pod, pod, priorityClassExpression, pod),
		fmt.Sprintf(`(!has(%s.terminationGracePeriodSeconds) || %s.terminationGracePeriodSeconds == 30) && (!has(%s.enableServiceLinks) || %s.enableServiceLinks) && (!has(%s.topologySpreadConstraints) || %s.topologySpreadConstraints.size() == 0) && !has(dyn(%s).overhead) && !has(%s.os)`, pod, pod, pod, pod, pod, pod, pod, pod),
	}
}

func (g *ParentWorkloadGuard) binding(name string, candidate bool) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(name, candidate),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

func (g *ParentWorkloadGuard) metadata(name string, candidate bool) metav1.ObjectMeta {
	annotations := map[string]string{
		rolloutGuardVersionAnnotation: rolloutGuardVersion,
		ReleaseNameAnnotation:         g.rollout.ReleaseName,
		ReleaseNamespaceAnnotation:    g.rollout.ReleaseNamespace,
	}
	if candidate {
		annotations[ReleaseSequenceAnnotation] = strconv.FormatInt(int64(g.rollout.ReleaseSequence), 10)
		annotations[ManagerImageAnnotation] = g.rollout.ManagerImage
	}
	return metav1.ObjectMeta{
		Name:        name,
		Annotations: annotations,
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.rollout.ReleaseName,
			"app.kubernetes.io/component": parentWorkloadComponent,
		},
	}
}

func (g *ParentWorkloadGuard) verifyPolicy(actual, expected *admissionregistrationv1.ValidatingAdmissionPolicy, candidate bool) error {
	if actual == nil || actual.Name != expected.Name {
		return fmt.Errorf("fixed parent workload guard policy %s is missing", expected.Name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicy", actual.ObjectMeta, expected.Name, candidate); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual.Spec, expected.Spec) {
		return fmt.Errorf("parent workload guard policy %s differs from the immutable contract", expected.Name)
	}
	return nil
}

func (g *ParentWorkloadGuard) verifyBinding(actual, expected *admissionregistrationv1.ValidatingAdmissionPolicyBinding, candidate bool) error {
	if actual == nil || actual.Name != expected.Name {
		return fmt.Errorf("fixed parent workload guard binding %s is missing", expected.Name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicyBinding", actual.ObjectMeta, expected.Name, candidate); err != nil {
		return err
	}
	if !reflect.DeepEqual(actual.Spec, expected.Spec) {
		return fmt.Errorf("parent workload guard binding %s differs from the immutable contract", expected.Name)
	}
	return nil
}

func (g *ParentWorkloadGuard) verifyMetadata(kind string, actual metav1.ObjectMeta, name string, candidate bool) error {
	expected := g.metadata(name, candidate)
	for key, value := range expected.Annotations {
		if actual.Annotations[key] != value {
			return fmt.Errorf("fixed parent workload guard %s/%s has foreign or incomplete ownership", kind, name)
		}
	}
	for key, value := range expected.Labels {
		if actual.Labels[key] != value {
			return fmt.Errorf("fixed parent workload guard %s/%s has foreign or incomplete ownership", kind, name)
		}
	}
	return nil
}

func exactParentGuardObjectMetadata(actual, expected metav1.ObjectMeta) bool {
	return actual.Name == expected.Name && actual.Namespace == expected.Namespace && actual.GenerateName == "" &&
		actual.DeletionTimestamp == nil && actual.DeletionGracePeriodSeconds == nil &&
		len(actual.OwnerReferences) == 0 && len(actual.Finalizers) == 0 &&
		reflect.DeepEqual(actual.Annotations, expected.Annotations) && reflect.DeepEqual(actual.Labels, expected.Labels)
}

func (g *ParentWorkloadGuard) hookServiceAccountPatterns() (string, string) {
	base, _ := NewServiceAccountOriginGuard(g.rollout).hookServiceAccountBase()
	return "^" + regexp.QuoteMeta(base+"-crd-v") + `[1-9][0-9]*-[0-9a-f]{12}$`,
		"^" + regexp.QuoteMeta(base+"-cleanup-v") + `[1-9][0-9]*-[0-9a-f]{12}$`
}

func (g *ParentWorkloadGuard) validate() error {
	if g == nil || g.rollout == nil || g.rollout.Policies == nil || g.rollout.Bindings == nil {
		return fmt.Errorf("parent workload guard policy clients are required")
	}
	if err := g.rollout.validateIdentity(); err != nil {
		return fmt.Errorf("validate parent workload identity: %w", err)
	}
	if g.identityErr != nil {
		return fmt.Errorf("validate parent stable ServiceAccount identity: %w", g.identityErr)
	}
	if g.rollout.PollEvery <= 0 {
		return fmt.Errorf("parent workload guard poll interval must be positive")
	}
	if !regexp.MustCompile(`^.+@sha256:[0-9a-f]{64}$`).MatchString(g.rollout.ManagerImage) {
		return fmt.Errorf("parent workload guard manager image must use an immutable sha256 digest")
	}
	if _, err := NewServiceAccountOriginGuard(g.rollout).hookServiceAccountBase(); err != nil {
		return fmt.Errorf("validate parent hook identity: %w", err)
	}
	return nil
}
