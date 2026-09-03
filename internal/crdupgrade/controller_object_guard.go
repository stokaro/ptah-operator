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
	controllerJobWriteGuardNamePrefix  = "ptah-operator-job-write-guard-v1-"
	controllerChunkWriteGuardPrefix    = "ptah-operator-chunk-write-guard-v1-"
	controllerPlanWriteGuardNamePrefix = "ptah-operator-plan-write-guard-v1-"

	controllerJobWriteGuardComponent   = "controller-job-write-guard"
	controllerChunkWriteGuardComponent = "controller-chunk-write-guard"
	controllerPlanWriteGuardComponent  = "controller-plan-write-guard"

	controllerObjectPolicyWeight  = "-152"
	controllerObjectBindingWeight = "-151"
)

// ControllerJobWriteGuardPolicyName returns the stable release-owned name of
// the manager's structural Job write boundary.
func ControllerJobWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return controllerObjectGuardPolicyName(controllerJobWriteGuardNamePrefix, releaseNamespace, releaseName)
}

// ControllerChunkWriteGuardPolicyName returns the stable release-owned name
// of the manager's structural plan-chunk ConfigMap write boundary.
func ControllerChunkWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return controllerObjectGuardPolicyName(controllerChunkWriteGuardPrefix, releaseNamespace, releaseName)
}

// ControllerPlanWriteGuardPolicyName returns the stable release-owned name of
// the manager's structural PtahSchemaPlan write boundary.
func ControllerPlanWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return controllerObjectGuardPolicyName(controllerPlanWriteGuardNamePrefix, releaseNamespace, releaseName)
}

func controllerObjectGuardPolicyName(prefix, releaseNamespace, releaseName string) string {
	identity := releaseNamespace + "\n" + releaseName
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return prefix + digest[:12]
}

type controllerObjectGuardEntry struct {
	name          string
	component     string
	apiGroups     []string
	apiVersions   []string
	resource      string
	operations    []admissionregistrationv1.OperationType
	denialMessage string
	validations   []admissionregistrationv1.Validation
}

// ControllerObjectGuard verifies the typed, parameterless structural
// boundaries around every main-resource object the manager may create or
// update. The validating webhook remains the authoritative reconstruction
// boundary; these policies independently reject broad or privileged shapes.
type ControllerObjectGuard struct {
	Policies                     ValidatingAdmissionPolicyReader
	Bindings                     ValidatingAdmissionPolicyBindingReader
	ReleaseName                  string
	ReleaseNamespace             string
	ControllerServiceAccountName string
	PollEvery                    time.Duration
}

// NewControllerObjectGuard copies the stable release and manager identity
// from the rollout contract.
func NewControllerObjectGuard(rollout *RolloutGuard) *ControllerObjectGuard {
	if rollout == nil {
		return nil
	}
	return &ControllerObjectGuard{
		Policies:                     rollout.Policies,
		Bindings:                     rollout.Bindings,
		ReleaseName:                  rollout.ReleaseName,
		ReleaseNamespace:             rollout.ReleaseNamespace,
		ControllerServiceAccountName: rollout.ControllerServiceAccountName,
		PollEvery:                    rollout.PollEvery,
	}
}

// Verify requires all three retained policy/binding pairs to match the
// compiled typed contracts exactly.
func (g *ControllerObjectGuard) Verify(ctx context.Context) error {
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

// WaitReady verifies the immutable contracts and waits for warning-free CEL
// type checking before the manager receives runtime privileges.
func (g *ControllerObjectGuard) WaitReady(ctx context.Context) error {
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

func (g *ControllerObjectGuard) entries() []controllerObjectGuardEntry {
	entries := []controllerObjectGuardEntry{
		{
			name:          ControllerJobWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName),
			component:     controllerJobWriteGuardComponent,
			apiGroups:     []string{"batch"},
			apiVersions:   []string{"v1"},
			resource:      "jobs",
			operations:    []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			denialMessage: "Ptah controller Job write guard rejected an unsafe workload shape",
		},
		{
			name:          ControllerChunkWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName),
			component:     controllerChunkWriteGuardComponent,
			apiGroups:     []string{""},
			apiVersions:   []string{"v1"},
			resource:      "configmaps",
			operations:    []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
			denialMessage: "Ptah controller chunk write guard rejected an unsafe ConfigMap shape",
		},
		{
			name:          ControllerPlanWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName),
			component:     controllerPlanWriteGuardComponent,
			apiGroups:     []string{"operator.ptah.dev"},
			apiVersions:   []string{"v1alpha1"},
			resource:      "ptahschemaplans",
			operations:    []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
			denialMessage: "Ptah controller plan write guard rejected an unsafe manifest shape",
		},
	}
	entries[0].validations = controllerJobWriteValidations(entries[0].denialMessage)
	entries[1].validations = controllerChunkWriteValidations(entries[1].denialMessage)
	entries[2].validations = controllerPlanWriteValidations(entries[2].denialMessage)
	return entries
}

func (g *ControllerObjectGuard) policy(entry controllerObjectGuardEntry) *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	username := "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.ControllerServiceAccountName
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(entry),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(entry),
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "exact-controller-service-account",
				Expression: fmt.Sprintf(`request.userInfo.username == %q`, username),
			}},
			Validations: entry.validations,
		},
	}
}

func (g *ControllerObjectGuard) binding(entry controllerObjectGuardEntry) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(entry),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        entry.name,
			MatchResources:    g.matchResources(entry),
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

func (g *ControllerObjectGuard) matchResources(entry controllerObjectGuardEntry) *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.MatchResources{
		MatchPolicy: &exact,
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: entry.operations,
				Rule: admissionregistrationv1.Rule{
					APIGroups:   entry.apiGroups,
					APIVersions: entry.apiVersions,
					Resources:   []string{entry.resource},
					Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
				},
			},
		}},
	}
}

func (g *ControllerObjectGuard) metadata(entry controllerObjectGuardEntry) metav1.ObjectMeta {
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

func (g *ControllerObjectGuard) verifyPolicy(entry controllerObjectGuardEntry, policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
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

func (g *ControllerObjectGuard) verifyBinding(entry controllerObjectGuardEntry, binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
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

func (g *ControllerObjectGuard) verifyMetadata(entry controllerObjectGuardEntry, kind string, metadata metav1.ObjectMeta) error {
	expected := g.metadata(entry)
	if metadata.Name != expected.Name {
		return fmt.Errorf("fixed controller object guard %s has an unexpected name", kind)
	}
	for key, value := range expected.Annotations {
		if metadata.Annotations[key] != value {
			return fmt.Errorf("fixed controller object guard %s has foreign or incomplete ownership", kind)
		}
	}
	for key, value := range expected.Labels {
		if metadata.Labels[key] != value {
			return fmt.Errorf("fixed controller object guard %s has foreign or incomplete ownership", kind)
		}
	}
	return nil
}

func (g *ControllerObjectGuard) validate(requirePoll bool) error {
	if g == nil || g.Policies == nil || g.Bindings == nil {
		return fmt.Errorf("controller object guard policy clients are required")
	}
	for description, value := range map[string]string{
		"release name":                       g.ReleaseName,
		"release namespace":                  g.ReleaseNamespace,
		"controller ServiceAccount identity": g.ControllerServiceAccountName,
	} {
		if value == "" || value != strings.TrimSpace(value) {
			return fmt.Errorf("controller object guard %s is required and must not contain surrounding whitespace", description)
		}
	}
	if requirePoll && g.PollEvery <= 0 {
		return fmt.Errorf("controller object guard poll interval must be positive")
	}
	return nil
}

func controllerJobWriteValidations(message string) []admissionregistrationv1.Validation {
	return controllerObjectValidations(message,
		`has(object.metadata.labels) && object.metadata.labels.size() == 5 && ["app.kubernetes.io/managed-by", "app.kubernetes.io/component", "operator.ptah.dev/schema", "operator.ptah.dev/operation", "operator.ptah.dev/operation-id"].all(key, key in object.metadata.labels) && object.metadata.labels["app.kubernetes.io/managed-by"] == "ptah-operator" && object.metadata.labels["app.kubernetes.io/component"] == "schema-operation" && object.metadata.labels["operator.ptah.dev/schema"] != "" && object.metadata.labels["operator.ptah.dev/operation"] in ["resolve", "verify", "observe", "plan", "apply"] && object.metadata.labels["operator.ptah.dev/operation-id"].matches("^[0-9a-f]{16}$") && object.metadata.name.startsWith("ptah-" + object.metadata.labels["operator.ptah.dev/operation"] + "-") && object.metadata.name.matches("^ptah-(resolve|verify|observe|plan|apply)-[a-z0-9]([-a-z0-9.]*[a-z0-9])?-[0-9a-f]{16}$") && (!has(object.metadata.generateName) || object.metadata.generateName == "") && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.dev/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahSchema" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.dev/schema"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion`,
		`(request.operation == "UPDATE" && object.metadata.labels["operator.ptah.dev/operation"] in ["resolve", "verify", "observe", "plan"] && has(object.metadata.annotations) && object.metadata.annotations.size() == 5 && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$")) || (request.operation == "UPDATE" && object.metadata.labels["operator.ptah.dev/operation"] == "apply" && has(object.metadata.annotations) && object.metadata.annotations.size() == 7 && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/plan-fingerprint", "operator.ptah.dev/plan-content-digest", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/plan-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/plan-content-digest"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$")) || (has(object.metadata.annotations) && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/controller-image", "operator.ptah.dev/controller-revision", "operator.ptah.dev/controller-state-version", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations.all(key, key in ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/controller-image", "operator.ptah.dev/controller-revision", "operator.ptah.dev/controller-state-version", "operator.ptah.dev/admission-snapshot-digest", "operator.ptah.dev/plan-fingerprint", "operator.ptah.dev/plan-content-digest"]) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/controller-image"].matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/controller-revision"] != "" && object.metadata.annotations["operator.ptah.dev/controller-state-version"].matches("^[1-9][0-9]*$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$") && ((object.metadata.labels["operator.ptah.dev/operation"] == "apply" && "operator.ptah.dev/plan-fingerprint" in object.metadata.annotations && object.metadata.annotations["operator.ptah.dev/plan-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && "operator.ptah.dev/plan-content-digest" in object.metadata.annotations && object.metadata.annotations["operator.ptah.dev/plan-content-digest"].matches("^sha256:[0-9a-f]{64}$")) || (object.metadata.labels["operator.ptah.dev/operation"] != "apply" && !("operator.ptah.dev/plan-fingerprint" in object.metadata.annotations) && !("operator.ptah.dev/plan-content-digest" in object.metadata.annotations))))`,
		`has(object.spec.parallelism) && object.spec.parallelism == 1 && has(object.spec.completions) && object.spec.completions == 1 && has(object.spec.activeDeadlineSeconds) && object.spec.activeDeadlineSeconds >= 30 && object.spec.activeDeadlineSeconds <= 86400 && has(object.spec.backoffLimit) && object.spec.backoffLimit == 0 && !has(object.spec.backoffLimitPerIndex) && !has(object.spec.maxFailedIndexes) && has(object.spec.manualSelector) && !object.spec.manualSelector && has(object.spec.completionMode) && object.spec.completionMode == "NonIndexed" && has(object.spec.suspend) && !object.spec.suspend && has(object.spec.podReplacementPolicy) && object.spec.podReplacementPolicy == "Failed" && !has(object.spec.podFailurePolicy) && !has(object.spec.successPolicy) && !has(object.spec.managedBy) && ((request.operation == "CREATE" && !has(object.spec.ttlSecondsAfterFinished)) || (request.operation == "UPDATE" && has(object.spec.ttlSecondsAfterFinished) && object.spec.ttlSecondsAfterFinished == 300))`,
		`has(object.spec.template.metadata.labels) && ["app.kubernetes.io/managed-by", "app.kubernetes.io/component", "operator.ptah.dev/schema", "operator.ptah.dev/operation", "operator.ptah.dev/operation-id"].all(key, key in object.spec.template.metadata.labels && object.spec.template.metadata.labels[key] == object.metadata.labels[key]) && object.spec.template.metadata.labels.all(key, key in ["app.kubernetes.io/managed-by", "app.kubernetes.io/component", "operator.ptah.dev/schema", "operator.ptah.dev/operation", "operator.ptah.dev/operation-id", "batch.kubernetes.io/controller-uid", "batch.kubernetes.io/job-name", "controller-uid", "job-name"]) && has(object.spec.template.metadata.annotations) && object.spec.template.metadata.annotations == object.metadata.annotations && has(object.spec.template.spec.activeDeadlineSeconds) && object.spec.template.spec.activeDeadlineSeconds == object.spec.activeDeadlineSeconds && has(object.spec.template.spec.automountServiceAccountToken) && !object.spec.template.spec.automountServiceAccountToken && has(object.spec.template.spec.enableServiceLinks) && !object.spec.template.spec.enableServiceLinks && object.spec.template.spec.serviceAccountName != "" && object.spec.template.spec.restartPolicy == "Never" && (!has(object.spec.template.spec.hostNetwork) || !object.spec.template.spec.hostNetwork) && (!has(object.spec.template.spec.hostPID) || !object.spec.template.spec.hostPID) && (!has(object.spec.template.spec.hostIPC) || !object.spec.template.spec.hostIPC) && (!has(object.spec.template.spec.shareProcessNamespace) || !object.spec.template.spec.shareProcessNamespace) && object.spec.template.spec.containers.size() == 1 && object.spec.template.spec.containers[0].name == "ptah" && ((object.metadata.labels["operator.ptah.dev/operation"] in ["observe", "plan"] && object.spec.template.spec.initContainers.size() == 3) || (!(object.metadata.labels["operator.ptah.dev/operation"] in ["observe", "plan"]) && object.spec.template.spec.initContainers.size() == 1)) && object.spec.template.spec.containers.all(container, has(container.securityContext) && has(container.securityContext.allowPrivilegeEscalation) && !container.securityContext.allowPrivilegeEscalation && has(container.securityContext.privileged) && !container.securityContext.privileged && has(container.securityContext.readOnlyRootFilesystem) && container.securityContext.readOnlyRootFilesystem && has(container.securityContext.runAsNonRoot) && container.securityContext.runAsNonRoot && has(container.securityContext.capabilities) && container.securityContext.capabilities.drop == ["ALL"]) && object.spec.template.spec.initContainers.all(container, has(container.securityContext) && has(container.securityContext.allowPrivilegeEscalation) && !container.securityContext.allowPrivilegeEscalation && has(container.securityContext.privileged) && !container.securityContext.privileged && has(container.securityContext.readOnlyRootFilesystem) && container.securityContext.readOnlyRootFilesystem && has(container.securityContext.runAsNonRoot) && container.securityContext.runAsNonRoot && has(container.securityContext.capabilities) && container.securityContext.capabilities.drop == ["ALL"])`,
		controllerJobPodBoundaryExpression,
		controllerJobSupportedWindowTopLevelExpression("object"),
		controllerJobSupportedWindowVolumeExpression("object"),
		controllerJobSupportedWindowProjectionExpression("object"),
		controllerJobSupportedWindowMountExpression("object"),
		controllerJobSupportedWindowContainerExpression("object"),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowTopLevelExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowVolumeExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowProjectionExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowMountExpression("oldObject")),
		controllerJobPreviousObjectExpression(controllerJobSupportedWindowContainerExpression("oldObject")),
		`request.operation != "CREATE" || !has(object.status)`,
		`request.operation != "UPDATE" || (oldObject != null && !has(oldObject.spec.ttlSecondsAfterFinished) && has(object.spec.ttlSecondsAfterFinished) && object.spec.ttlSecondsAfterFinished == 300 && has(oldObject.status.conditions) && oldObject.status.conditions.exists(condition, condition.status == "True" && condition.type in ["Complete", "Failed"]) && object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && object.metadata.uid == oldObject.metadata.uid && object.metadata.labels == oldObject.metadata.labels && object.metadata.annotations == oldObject.metadata.annotations && object.metadata.ownerReferences == oldObject.metadata.ownerReferences && has(object.metadata.finalizers) == has(oldObject.metadata.finalizers) && (!has(object.metadata.finalizers) || object.metadata.finalizers == oldObject.metadata.finalizers) && has(object.metadata.generateName) == has(oldObject.metadata.generateName) && (!has(object.metadata.generateName) || object.metadata.generateName == oldObject.metadata.generateName) && has(object.metadata.deletionTimestamp) == has(oldObject.metadata.deletionTimestamp) && (!has(object.metadata.deletionTimestamp) || object.metadata.deletionTimestamp == oldObject.metadata.deletionTimestamp) && has(object.status) == has(oldObject.status) && (!has(object.status) || object.status == oldObject.status) && object.spec.parallelism == oldObject.spec.parallelism && object.spec.completions == oldObject.spec.completions && object.spec.activeDeadlineSeconds == oldObject.spec.activeDeadlineSeconds && has(object.spec.podFailurePolicy) == has(oldObject.spec.podFailurePolicy) && (!has(object.spec.podFailurePolicy) || object.spec.podFailurePolicy == oldObject.spec.podFailurePolicy) && has(object.spec.successPolicy) == has(oldObject.spec.successPolicy) && (!has(object.spec.successPolicy) || object.spec.successPolicy == oldObject.spec.successPolicy) && object.spec.backoffLimit == oldObject.spec.backoffLimit && has(object.spec.backoffLimitPerIndex) == has(oldObject.spec.backoffLimitPerIndex) && (!has(object.spec.backoffLimitPerIndex) || object.spec.backoffLimitPerIndex == oldObject.spec.backoffLimitPerIndex) && has(object.spec.maxFailedIndexes) == has(oldObject.spec.maxFailedIndexes) && (!has(object.spec.maxFailedIndexes) || object.spec.maxFailedIndexes == oldObject.spec.maxFailedIndexes) && has(object.spec.selector) == has(oldObject.spec.selector) && (!has(object.spec.selector) || object.spec.selector == oldObject.spec.selector) && object.spec.manualSelector == oldObject.spec.manualSelector && object.spec.template == oldObject.spec.template && object.spec.completionMode == oldObject.spec.completionMode && object.spec.suspend == oldObject.spec.suspend && object.spec.podReplacementPolicy == oldObject.spec.podReplacementPolicy && has(object.spec.managedBy) == has(oldObject.spec.managedBy) && (!has(object.spec.managedBy) || object.spec.managedBy == oldObject.spec.managedBy))`,
	)
}

const controllerJobPodBoundaryExpression = `has(object.spec.template.spec.securityContext) && has(object.spec.template.spec.securityContext.runAsNonRoot) && object.spec.template.spec.securityContext.runAsNonRoot && has(object.spec.template.spec.securityContext.runAsUser) && object.spec.template.spec.securityContext.runAsUser == 65532 && has(object.spec.template.spec.securityContext.runAsGroup) && object.spec.template.spec.securityContext.runAsGroup == 65532 && has(object.spec.template.spec.securityContext.fsGroup) && object.spec.template.spec.securityContext.fsGroup == 65532 && has(object.spec.template.spec.securityContext.fsGroupChangePolicy) && object.spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch" && has(object.spec.template.spec.securityContext.seccompProfile) && object.spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(object.spec.template.spec.securityContext.sysctls) || object.spec.template.spec.securityContext.sysctls.size() == 0) && object.spec.template.spec.volumes.size() >= 2 && object.spec.template.spec.volumes.size() <= 7 && object.spec.template.spec.volumes.all(volume, (volume.name in ["runner", "work", "schema-source", "fetch-work", "registry-ca-snapshot"] && has(volume.emptyDir)) || (volume.name in ["verification-policy", "registry-ca"] && has(volume.configMap)) || (volume.name == "registry-docker-config" && has(volume.secret)) || (volume.name == "plan" && has(volume.projected) && volume.projected.sources.size() >= 1 && volume.projected.sources.all(source, has(source.configMap)))) && object.spec.template.spec.containers.all(container, container.image.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && container.imagePullPolicy == "IfNotPresent" && (!has(container.envFrom) || container.envFrom.size() == 0) && (!has(container.volumeDevices) || container.volumeDevices.size() == 0) && has(container.securityContext.runAsUser) && container.securityContext.runAsUser == 65532 && has(container.securityContext.runAsGroup) && container.securityContext.runAsGroup == 65532 && has(container.securityContext.seccompProfile) && container.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(container.securityContext.capabilities.add) || container.securityContext.capabilities.add.size() == 0)) && object.spec.template.spec.initContainers.all(container, container.image.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && container.imagePullPolicy == "IfNotPresent" && (!has(container.envFrom) || container.envFrom.size() == 0) && (!has(container.volumeDevices) || container.volumeDevices.size() == 0) && has(container.securityContext.runAsUser) && container.securityContext.runAsUser == 65532 && has(container.securityContext.runAsGroup) && container.securityContext.runAsGroup == 65532 && has(container.securityContext.seccompProfile) && container.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(container.securityContext.capabilities.add) || container.securityContext.capabilities.add.size() == 0)) && ((object.metadata.labels["operator.ptah.dev/operation"] in ["observe", "plan"] && object.spec.template.spec.initContainers.map(container, container.name) == ["install-runner", "validate-source-authority", "fetch-schema"]) || (!(object.metadata.labels["operator.ptah.dev/operation"] in ["observe", "plan"]) && object.spec.template.spec.initContainers.map(container, container.name) == ["install-runner"]))`

func controllerJobSupportedWindowTopLevelExpression(root string) string {
	return fmt.Sprintf(
		`!has(dyn(%[1]s.spec).scheduling) && !has(dyn(%[1]s.spec.template.spec).evictionResponders) && !has(dyn(%[1]s.spec.template.spec).workloadRef)`,
		root,
	)
}

func controllerJobSupportedWindowVolumeExpression(root string) string {
	return fmt.Sprintf(
		`%[1]s.spec.template.spec.volumes.all(volume, (!has(volume.emptyDir) || !has(dyn(volume.emptyDir).mode)) && (!has(volume.configMap) || (!has(dyn(volume.configMap).defaultUser) && (!has(volume.configMap.items) || volume.configMap.items.all(item, !has(dyn(item).user))))) && (!has(volume.secret) || (!has(dyn(volume.secret).defaultUser) && (!has(volume.secret.items) || volume.secret.items.all(item, !has(dyn(item).user))))) && (!has(volume.projected) || !has(dyn(volume.projected).defaultUser)) && (!has(volume.downwardAPI) || (!has(dyn(volume.downwardAPI).defaultUser) && (!has(volume.downwardAPI.items) || volume.downwardAPI.items.all(item, !has(dyn(item).user))))))`,
		root,
	)
}

func controllerJobSupportedWindowProjectionExpression(root string) string {
	return fmt.Sprintf(
		`%[1]s.spec.template.spec.volumes.all(volume, !has(volume.projected) || volume.projected.sources.all(source, (!has(source.configMap) || !has(source.configMap.items) || source.configMap.items.all(item, !has(dyn(item).user))) && (!has(source.secret) || !has(source.secret.items) || source.secret.items.all(item, !has(dyn(item).user))) && (!has(source.serviceAccountToken) || !has(dyn(source.serviceAccountToken).user)) && (!has(source.clusterTrustBundle) || !has(dyn(source.clusterTrustBundle).user)) && (!has(source.podCertificate) || !has(dyn(source.podCertificate).user)) && (!has(source.downwardAPI) || !has(source.downwardAPI.items) || source.downwardAPI.items.all(item, !has(dyn(item).user)))))`,
		root,
	)
}

func controllerJobSupportedWindowMountExpression(root string) string {
	return fmt.Sprintf(
		`%[1]s.spec.template.spec.containers.all(container, !has(container.volumeMounts) || container.volumeMounts.all(mount, !has(dyn(mount).bindMountOptions))) && %[1]s.spec.template.spec.initContainers.all(container, !has(container.volumeMounts) || container.volumeMounts.all(mount, !has(dyn(mount).bindMountOptions)))`,
		root,
	)
}

func controllerJobSupportedWindowContainerExpression(root string) string {
	return fmt.Sprintf(
		`(!has(%[1]s.spec.template.spec.ephemeralContainers) || %[1]s.spec.template.spec.ephemeralContainers.size() == 0) && %[1]s.spec.template.spec.containers.all(container, !has(container.lifecycle) && !has(container.livenessProbe) && !has(container.readinessProbe) && !has(container.startupProbe)) && %[1]s.spec.template.spec.initContainers.all(container, !has(container.lifecycle) && !has(container.livenessProbe) && !has(container.readinessProbe) && !has(container.startupProbe))`,
		root,
	)
}

func controllerJobPreviousObjectExpression(expression string) string {
	return `request.operation != "UPDATE" || (oldObject != null && ` + expression + `)`
}

func controllerChunkWriteValidations(message string) []admissionregistrationv1.Validation {
	return controllerObjectValidations(message,
		`has(object.metadata.labels) && object.metadata.labels.size() == 2 && ["operator.ptah.dev/plan", "operator.ptah.dev/schema"].all(key, key in object.metadata.labels && object.metadata.labels[key] != "") && object.metadata.name.matches("^ptah-plan-[0-9a-f]{24}-[0-9]{3}$") && object.metadata.name.startsWith(object.metadata.labels["operator.ptah.dev/plan"] + "-") && (!has(object.metadata.annotations) || object.metadata.annotations.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && (!has(object.metadata.generateName) || object.metadata.generateName == "") && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.dev/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahSchemaPlan" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.dev/plan"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion`,
		`has(object.immutable) && object.immutable && (!has(object.data) || object.data.size() == 0) && has(object.binaryData) && object.binaryData.size() == 1 && "chunk" in object.binaryData && object.binaryData["chunk"].size() >= 1 && object.binaryData["chunk"].size() <= 524288`,
	)
}

func controllerPlanWriteValidations(message string) []admissionregistrationv1.Validation {
	return controllerObjectValidations(message,
		`has(object.metadata.labels) && object.metadata.labels.size() == 1 && "operator.ptah.dev/schema" in object.metadata.labels && object.metadata.labels["operator.ptah.dev/schema"] != "" && object.metadata.name.matches("^ptah-plan-[0-9a-f]{24}$") && (!has(object.metadata.annotations) || object.metadata.annotations.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && (!has(object.metadata.generateName) || object.metadata.generateName == "") && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.dev/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahSchema" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.dev/schema"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion && object.spec.schemaRef.name == object.metadata.labels["operator.ptah.dev/schema"] && object.spec.schemaRef.uid == object.metadata.ownerReferences[0].uid`,
		`object.spec.contractVersion == 3 && object.spec.fingerprint.matches("^sha256:[0-9a-f]{64}$") && object.spec.contentDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.artifactDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.coordinationDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.targetIdentityDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.actualStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && object.spec.desiredStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && object.spec.policyFingerprint.matches("^sha256:[0-9a-f]{64}$") && object.spec.verificationPolicyUID != "" && object.spec.verificationPolicyDigest.matches("^sha256:[0-9a-f]{64}$") && object.spec.executionBindingID.matches("^v1-[0-9a-f]{32}$") && object.spec.controllerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.spec.controllerRevision != "" && object.spec.controllerStateVersion >= 1 && object.spec.ptahVersion != "" && object.spec.executorImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.spec.runnerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.spec.runnerProtocolVersion >= 1 && object.spec.dialect != "" && object.spec.statementCount >= 1 && object.spec.size >= 1 && object.spec.size <= 8388608`,
		`object.spec.chunks.size() >= 1 && object.spec.chunks.size() <= 16 && object.spec.chunks.all(chunk, chunk.key == "chunk" && chunk.name.matches("^ptah-plan-[0-9a-f]{24}-[0-9]{3}$") && chunk.name.startsWith(object.metadata.name + "-") && chunk.index >= 0 && chunk.index < object.spec.chunks.size() && chunk.digest.matches("^sha256:[0-9a-f]{64}$") && chunk.size >= 1 && chunk.size <= 524288)`,
		`!has(object.status)`,
	)
}

func controllerObjectValidations(message string, expressions ...string) []admissionregistrationv1.Validation {
	validations := make([]admissionregistrationv1.Validation, len(expressions))
	for index, expression := range expressions {
		validations[index] = admissionregistrationv1.Validation{Expression: expression, Message: message}
	}
	return validations
}
