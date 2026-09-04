package crdupgrade

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"regexp"
	"strconv"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	parentReplicaSetGuardPrefix = "ptah-operator-runtime-parent-guard-v1-"
	parentHookOriginGuardPrefix = "ptah-operator-hook-parent-origin-guard-v1-"
	parentHookPodOriginPrefix   = "ptah-operator-hook-pod-origin-guard-v1-"
	parentHookContractPrefix    = "ptah-operator-hook-parent-contract-v"
	parentWorkloadComponent     = "parent-workload-guard"

	parentReplicaSetPolicyWeight     = "-139"
	parentReplicaSetBindingWeight    = "-138"
	parentHookOriginPolicyWeight     = "-137"
	parentHookOriginBindingWeight    = "-136"
	parentHookPodOriginPolicyWeight  = "-135"
	parentHookPodOriginBindingWeight = "-134"
	parentHookContractPolicyWeight   = "-133"
	parentHookContractBindingWeight  = "-132"
)

// ParentReplicaSetGuardPolicyName returns the stable release-owned boundary
// around ReplicaSets that can mint Pods for either long-running runtime
// ServiceAccount.
func ParentReplicaSetGuardPolicyName(releaseNamespace, releaseName string) string {
	return parentReplicaSetGuardPrefix + parentWorkloadStableDigest(releaseNamespace, releaseName)
}

// ParentHookJobOriginGuardPolicyName returns the stable release-owned gate
// which prevents namespace-scoped actors from staging a future hook Job
// before its immutable candidate contract has been installed.
func ParentHookJobOriginGuardPolicyName(releaseNamespace, releaseName string) string {
	return parentHookOriginGuardPrefix + parentWorkloadStableDigest(releaseNamespace, releaseName)
}

// ParentHookPodOriginGuardPolicyName returns the stable release-owned gate
// which permits hook Pods only when the built-in Job controller creates them
// through an exact Job ownership and label chain.
func ParentHookPodOriginGuardPolicyName(releaseNamespace, releaseName string) string {
	return parentHookPodOriginPrefix + parentWorkloadStableDigest(releaseNamespace, releaseName)
}

// ParentHookJobContractPolicyName returns the append-only exact contract for
// the install, upgrade, and teardown Jobs belonging to one candidate release.
func ParentHookJobContractPolicyName(releaseNamespace, releaseName string, sequence int32, managerImage string) string {
	digest := hookIdentityDigest(releaseNamespace, releaseName, sequence, managerImage)
	return fmt.Sprintf("%s%d-%s", parentHookContractPrefix, sequence, digest[:12])
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
	rollout *RolloutGuard
}

// NewParentWorkloadGuard derives every name and executable argument from the
// already validated rollout identity.
func NewParentWorkloadGuard(rollout *RolloutGuard) *ParentWorkloadGuard {
	return &ParentWorkloadGuard{rollout: rollout}
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
// type-checked every policy without warnings. The stable hook-origin gate is
// deliberately installed before any candidate ServiceAccount, so a future
// name cannot be staged through ordinary namespace permissions.
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
	name := ParentReplicaSetGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	controllerSA := g.rollout.ControllerServiceAccountName
	certificateSA := g.rollout.CertificateDeploymentName
	controllerDeployment := g.rollout.ControllerDeploymentName
	certificateDeployment := g.rollout.CertificateDeploymentName
	message := parentReplicaSetDenialMessage()
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
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
}

func replicaSetGarbageCollectorCleanupExpression() string {
	// ResourceVersion is protected by storage optimistic concurrency, while
	// managedFields is API-machinery bookkeeping. Neither grants workload
	// authority, and pinning either here would make legitimate cleanup depend on
	// server-side mutation order.
	return `request.operation == "UPDATE" && request.userInfo.username == "system:serviceaccount:kube-system:generic-garbage-collector" && oldObject != null && object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && object.metadata.uid == oldObject.metadata.uid && has(object.metadata.generateName) == has(oldObject.metadata.generateName) && (!has(object.metadata.generateName) || object.metadata.generateName == oldObject.metadata.generateName) && has(object.metadata.creationTimestamp) == has(oldObject.metadata.creationTimestamp) && (!has(object.metadata.creationTimestamp) || object.metadata.creationTimestamp == oldObject.metadata.creationTimestamp) && object.metadata.generation == oldObject.metadata.generation && has(oldObject.metadata.deletionTimestamp) && has(object.metadata.deletionTimestamp) && object.metadata.deletionTimestamp == oldObject.metadata.deletionTimestamp && has(object.metadata.deletionGracePeriodSeconds) == has(oldObject.metadata.deletionGracePeriodSeconds) && (!has(object.metadata.deletionGracePeriodSeconds) || object.metadata.deletionGracePeriodSeconds == oldObject.metadata.deletionGracePeriodSeconds) && object.spec == oldObject.spec && has(object.status) == has(oldObject.status) && (!has(object.status) || object.status == oldObject.status) && has(object.metadata.labels) == has(oldObject.metadata.labels) && (!has(object.metadata.labels) || object.metadata.labels == oldObject.metadata.labels) && has(object.metadata.annotations) == has(oldObject.metadata.annotations) && (!has(object.metadata.annotations) || object.metadata.annotations == oldObject.metadata.annotations) && has(object.metadata.ownerReferences) == has(oldObject.metadata.ownerReferences) && (!has(object.metadata.ownerReferences) || object.metadata.ownerReferences == oldObject.metadata.ownerReferences) && has(oldObject.metadata.finalizers) && oldObject.metadata.finalizers.filter(finalizer, finalizer == "foregroundDeletion").size() == 1 && ((!has(object.metadata.finalizers) && oldObject.metadata.finalizers.size() == 1) || (has(object.metadata.finalizers) && object.metadata.finalizers == oldObject.metadata.finalizers.filter(finalizer, finalizer != "foregroundDeletion")))`
}

func (g *ParentWorkloadGuard) hookPodOriginPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := ParentHookPodOriginGuardPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName)
	hookPattern, teardownPattern := g.hookServiceAccountPatterns()
	message := parentHookPodOriginDenialMessage()
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
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
	authority := fmt.Sprintf(`(authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").check("create").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").check("create").allowed()) || (authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicies").name(%q).check("delete").allowed() && authorizer.group("admissionregistration.k8s.io").resource("validatingadmissionpolicybindings").name(%q).check("delete").allowed())`, namespaceGuard, namespaceGuard)
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
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

func (g *ParentWorkloadGuard) hookJobContractPolicy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	exact := admissionregistrationv1.Exact
	name := ParentHookJobContractPolicyName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage)
	message := parentHookContractDenialMessage(g.rollout.ReleaseSequence)
	identityJob := HookIdentityProbeJobName(g.rollout.ReleaseNamespace, g.rollout.ReleaseName, g.rollout.ReleaseSequence, g.rollout.ManagerImage)
	preflightJob := g.rollout.hookJobName("preflight")
	reconcileJob := g.rollout.hookJobName("reconcile")
	quiesceJob := g.rollout.hookJobName("teardown-quiesce")
	teardownJob := g.rollout.hookJobName("teardown")
	teardownServiceAccount, _ := TeardownServiceAccountName(g.rollout.HookServiceAccountName, g.rollout.ReleaseSequence)
	validations := make([]admissionregistrationv1.Validation, 0, 40)
	for _, expression := range g.hookJobContractExpressions(identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, teardownServiceAccount) {
		validations = append(validations, admissionregistrationv1.Validation{Expression: expression, Message: message})
	}
	metadata := g.metadata(name, true)
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: metadata,
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
				Name:       "candidate-hook-service-account",
				Expression: fmt.Sprintf(`request.namespace == %q && ((has(object.spec.template.spec.serviceAccountName) && object.spec.template.spec.serviceAccountName in [%q, %q]) || (request.operation == "UPDATE" && has(oldObject.spec.template.spec.serviceAccountName) && oldObject.spec.template.spec.serviceAccountName in [%q, %q]))`, g.rollout.ReleaseNamespace, g.rollout.HookServiceAccountName, teardownServiceAccount, g.rollout.HookServiceAccountName, teardownServiceAccount),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "isIdentity", Expression: fmt.Sprintf(`request.name == %q`, identityJob)},
				{Name: "isPreflight", Expression: fmt.Sprintf(`request.name == %q`, preflightJob)},
				{Name: "isQuiesce", Expression: fmt.Sprintf(`request.name == %q`, quiesceJob)},
				{Name: "isTeardown", Expression: fmt.Sprintf(`request.name == %q`, teardownJob)},
			},
			Validations: validations,
		},
	}
}

func (g *ParentWorkloadGuard) hookJobContractExpressions(identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, teardownServiceAccount string) []string {
	pod := "object.spec.template.spec"
	templateMetadata := "object.spec.template.metadata"
	container := pod + ".containers[0]"
	volume := pod + ".volumes[0]"
	sources := volume + ".projected.sources"
	return []string{
		`!has(request.subResource) || request.subResource == ""`,
		fmt.Sprintf(`request.name in [%q, %q, %q, %q, %q] && object.metadata.name == request.name && object.metadata.namespace == %q && (!has(object.metadata.generateName) || object.metadata.generateName == "")`, identityJob, preflightJob, reconcileJob, quiesceJob, teardownJob, g.rollout.ReleaseNamespace),
		`(!has(object.metadata.ownerReferences) || object.metadata.ownerReferences.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0)`,
		fmt.Sprintf(`has(object.metadata.labels) && object.metadata.labels["app.kubernetes.io/instance"] == %q && object.metadata.labels["app.kubernetes.io/component"] == (variables.isIdentity ? "hook-identity-probe" : (variables.isPreflight ? "crd-manager-preflight" : (variables.isQuiesce ? "crd-manager-teardown-quiesce" : (variables.isTeardown ? "crd-manager-teardown" : "crd-manager"))))`, g.rollout.ReleaseName),
		`has(object.metadata.annotations) && object.metadata.annotations.size() == 3 && object.metadata.annotations["helm.sh/hook"] == ((variables.isQuiesce || variables.isTeardown) ? "pre-delete" : "pre-install,pre-upgrade") && object.metadata.annotations["helm.sh/hook-delete-policy"] == ((variables.isQuiesce || variables.isTeardown) ? "before-hook-creation,hook-succeeded" : "before-hook-creation,hook-succeeded,hook-failed") && object.metadata.annotations["helm.sh/hook-weight"] == (variables.isIdentity ? "-105" : (variables.isPreflight ? "-60" : (variables.isQuiesce ? "-10" : "0")))`,
		fmt.Sprintf(`(!has(%s.annotations) || %s.annotations.size() == 0) && (!has(%s.ownerReferences) || %s.ownerReferences.size() == 0) && (!has(%s.finalizers) || %s.finalizers.size() == 0)`, templateMetadata, templateMetadata, templateMetadata, templateMetadata, templateMetadata, templateMetadata),
		`has(object.spec.activeDeadlineSeconds) && object.spec.activeDeadlineSeconds == (variables.isIdentity ? 120 : 210)`,
		`has(object.spec.backoffLimit) && object.spec.backoffLimit == 0 && (!has(object.spec.backoffLimitPerIndex)) && (!has(object.spec.maxFailedIndexes))`,
		`(!has(object.spec.ttlSecondsAfterFinished)) && (!has(object.spec.podFailurePolicy)) && (!has(object.spec.successPolicy))`,
		`(!has(object.spec.suspend) || !object.spec.suspend) && (!has(object.spec.manualSelector) || !object.spec.manualSelector) && (!has(object.spec.parallelism) || object.spec.parallelism == 1) && (!has(object.spec.completions) || object.spec.completions == 1) && (!has(object.spec.completionMode) || object.spec.completionMode == "NonIndexed")`,
		fmt.Sprintf(`has(%[1]s.serviceAccountName) && %[1]s.serviceAccountName == (variables.isTeardown ? %[2]q : %[3]q) && (!has(%[1]s.serviceAccount) || %[1]s.serviceAccount == (variables.isTeardown ? %[2]q : %[3]q)) && has(%[1]s.automountServiceAccountToken) && !%[1]s.automountServiceAccountToken`, pod, teardownServiceAccount, g.rollout.HookServiceAccountName),
		fmt.Sprintf(`%s.restartPolicy == "Never" && (!has(%s.nodeName) || %s.nodeName == "") && (!has(%s.nodeSelector) || %s.nodeSelector.size() == 0) && !has(%s.affinity) && (!has(%s.tolerations) || %s.tolerations.size() == 0)`, pod, pod, pod, pod, pod, pod, pod, pod),
		fmt.Sprintf(`(!has(%s.hostNetwork) || !%s.hostNetwork) && (!has(%s.hostPID) || !%s.hostPID) && (!has(%s.hostIPC) || !%s.hostIPC) && (!has(%s.shareProcessNamespace) || !%s.shareProcessNamespace)`, pod, pod, pod, pod, pod, pod, pod, pod),
		fmt.Sprintf(`!has(%s.hostAliases) && !has(%s.hostname) && !has(%s.subdomain) && (!has(%s.setHostnameAsFQDN) || !%s.setHostnameAsFQDN) && !has(%s.runtimeClassName) && (!has(%s.readinessGates) || %s.readinessGates.size() == 0) && (!has(%s.resourceClaims) || %s.resourceClaims.size() == 0) && (!has(%s.schedulingGates) || %s.schedulingGates.size() == 0)`, pod, pod, pod, pod, pod, pod, pod, pod, pod, pod, pod, pod),
		fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.runAsNonRoot) && %[1]s.securityContext.runAsNonRoot && has(%[1]s.securityContext.runAsUser) && %[1]s.securityContext.runAsUser == 65532 && has(%[1]s.securityContext.runAsGroup) && %[1]s.securityContext.runAsGroup == 65532 && !has(%[1]s.securityContext.fsGroup) && !has(%[1]s.securityContext.fsGroupChangePolicy) && !has(%[1]s.securityContext.supplementalGroups) && !has(%[1]s.securityContext.supplementalGroupsPolicy) && !has(%[1]s.securityContext.seLinuxOptions) && !has(%[1]s.securityContext.seLinuxChangePolicy) && !has(%[1]s.securityContext.windowsOptions) && !has(%[1]s.securityContext.appArmorProfile) && has(%[1]s.securityContext.seccompProfile) && %[1]s.securityContext.seccompProfile.type == "RuntimeDefault" && !has(%[1]s.securityContext.seccompProfile.localhostProfile) && (!has(%[1]s.securityContext.sysctls) || %[1]s.securityContext.sysctls.size() == 0)`, pod),
		fmt.Sprintf(`has(%s.containers) && %s.containers.size() == 1 && (!has(%s.initContainers) || %s.initContainers.size() == 0) && (!has(%s.ephemeralContainers) || %s.ephemeralContainers.size() == 0)`, pod, pod, pod, pod, pod, pod),
		fmt.Sprintf(`%s.name == (variables.isIdentity ? "identity-probe" : (variables.isPreflight ? "crd-manager-preflight" : (variables.isQuiesce ? "crd-manager-teardown-quiesce" : (variables.isTeardown ? "crd-manager-teardown" : "crd-manager"))))`, container),
		fmt.Sprintf(`%s.image == %q && %s.command == ["/ptah-crd-manager"] && %s.imagePullPolicy in ["Always", "IfNotPresent", "Never"]`, container, g.rollout.ManagerImage, container, container),
		fmt.Sprintf(`!variables.isIdentity || %s`, g.rollout.hookArgsValidationExpression(container, "identity-probe")),
		fmt.Sprintf(`!variables.isPreflight || %s`, g.rollout.hookArgsValidationExpression(container, "preflight")),
		fmt.Sprintf(`request.name != %q || %s`, reconcileJob, g.rollout.hookArgsValidationExpression(container, "reconcile")),
		fmt.Sprintf(`!variables.isQuiesce || %s`, g.rollout.hookArgsValidationExpression(container, "teardown-quiesce")),
		fmt.Sprintf(`!variables.isTeardown || %s`, g.rollout.hookArgsValidationExpression(container, "teardown")),
		hookContainerNoExecutionSideChannelsExpression(container),
		fmt.Sprintf(`has(%[1]s.securityContext) && has(%[1]s.securityContext.allowPrivilegeEscalation) && !%[1]s.securityContext.allowPrivilegeEscalation && has(%[1]s.securityContext.readOnlyRootFilesystem) && %[1]s.securityContext.readOnlyRootFilesystem && (!has(%[1]s.securityContext.privileged) || !%[1]s.securityContext.privileged) && !has(%[1]s.securityContext.runAsUser) && !has(%[1]s.securityContext.runAsGroup) && !has(%[1]s.securityContext.runAsNonRoot) && !has(%[1]s.securityContext.procMount) && !has(%[1]s.securityContext.seLinuxOptions) && !has(%[1]s.securityContext.windowsOptions) && !has(%[1]s.securityContext.seccompProfile) && !has(%[1]s.securityContext.appArmorProfile) && has(%[1]s.securityContext.capabilities) && (!has(%[1]s.securityContext.capabilities.add) || %[1]s.securityContext.capabilities.add.size() == 0) && has(%[1]s.securityContext.capabilities.drop) && %[1]s.securityContext.capabilities.drop == ["ALL"]`, container),
		fmt.Sprintf(`has(dyn(%[1]s.resources).requests) && dyn(%[1]s.resources).requests.size() == 2 && quantity(string(dyn(%[1]s.resources).requests["cpu"])).compareTo(quantity("5m")) == 0 && quantity(string(dyn(%[1]s.resources).requests["memory"])).compareTo(quantity("16Mi")) == 0 && has(dyn(%[1]s.resources).limits) && dyn(%[1]s.resources).limits.size() == 1 && quantity(string(dyn(%[1]s.resources).limits["memory"])).compareTo(quantity("32Mi")) == 0 && (!has(dyn(%[1]s.resources).claims) || dyn(%[1]s.resources).claims.size() == 0) && (!has(%[1]s.resizePolicy) || %[1]s.resizePolicy.size() == 0)`, container),
		fmt.Sprintf(`has(%[1]s.volumeMounts) && %[1]s.volumeMounts.size() == 1 && %[1]s.volumeMounts[0].name == "api-access" && %[1]s.volumeMounts[0].mountPath == "/var/run/secrets/kubernetes.io/serviceaccount" && has(%[1]s.volumeMounts[0].readOnly) && %[1]s.volumeMounts[0].readOnly && !has(%[1]s.volumeMounts[0].mountPropagation) && !has(%[1]s.volumeMounts[0].subPath) && !has(%[1]s.volumeMounts[0].subPathExpr) && !has(%[1]s.volumeMounts[0].recursiveReadOnly)`, container),
		fmt.Sprintf(`has(%s.volumes) && %s.volumes.size() == 1 && %s.name == "api-access" && has(%s.projected) && has(%s.projected.defaultMode) && %s.projected.defaultMode == 420 && %s.size() == 3`, pod, pod, volume, volume, volume, volume, sources),
		fmt.Sprintf(`%s.exists(s, has(s.serviceAccountToken) && s.serviceAccountToken.path == "token" && has(s.serviceAccountToken.expirationSeconds) && s.serviceAccountToken.expirationSeconds == 3600 && !has(s.serviceAccountToken.audience))`, sources),
		fmt.Sprintf(`%s.exists(s, has(s.configMap) && s.configMap.name == "kube-root-ca.crt" && has(s.configMap.items) && s.configMap.items.size() == 1 && s.configMap.items[0].key == "ca.crt" && s.configMap.items[0].path == "ca.crt" && !has(s.configMap.items[0].mode))`, sources),
		fmt.Sprintf(`%s.exists(s, has(s.downwardAPI) && has(s.downwardAPI.items) && s.downwardAPI.items.size() == 1 && s.downwardAPI.items[0].path == "namespace" && has(s.downwardAPI.items[0].fieldRef) && s.downwardAPI.items[0].fieldRef.apiVersion == "v1" && s.downwardAPI.items[0].fieldRef.fieldPath == "metadata.namespace" && !has(s.downwardAPI.items[0].mode))`, sources),
		fmt.Sprintf(`%s.all(s, has(s.serviceAccountToken) || has(s.configMap) || has(s.downwardAPI))`, sources),
		fmt.Sprintf(`(!has(%[1]s.imagePullSecrets) || (%[1]s.imagePullSecrets.all(secret, secret.name != "") && %[1]s.imagePullSecrets.all(secret, %[1]s.imagePullSecrets.filter(other, other.name == secret.name).size() == 1)))`, pod),
		fmt.Sprintf(`(!has(%s.dnsPolicy) || %s.dnsPolicy == "ClusterFirst") && !has(%s.dnsConfig) && (!has(%s.schedulerName) || %s.schedulerName == "default-scheduler") && (!has(%s.priority) || %s.priority == 0) && !has(%s.priorityClassName) && (!has(%s.preemptionPolicy) || %s.preemptionPolicy == "PreemptLowerPriority")`, pod, pod, pod, pod, pod, pod, pod, pod, pod, pod),
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

func (g *ParentWorkloadGuard) hookServiceAccountPatterns() (string, string) {
	base, _ := NewServiceAccountOriginGuard(g.rollout, nil).hookServiceAccountBase()
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
	if g.rollout.PollEvery <= 0 {
		return fmt.Errorf("parent workload guard poll interval must be positive")
	}
	if !regexp.MustCompile(`^.+@sha256:[0-9a-f]{64}$`).MatchString(g.rollout.ManagerImage) {
		return fmt.Errorf("parent workload guard manager image must use an immutable sha256 digest")
	}
	if _, err := NewServiceAccountOriginGuard(g.rollout, nil).hookServiceAccountBase(); err != nil {
		return fmt.Errorf("validate parent hook identity: %w", err)
	}
	return nil
}
