package crdupgrade

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	controllerJobWriteGuardNamePrefix  = "ptah-operator-job-write-guard-v2-"
	controllerChunkWriteGuardPrefix    = "ptah-operator-chunk-write-guard-v2-"
	controllerPlanWriteGuardNamePrefix = "ptah-operator-plan-write-guard-v2-"

	controllerJobWriteGuardComponent   = "controller-job-write-guard"
	controllerChunkWriteGuardComponent = "controller-chunk-write-guard"
	controllerPlanWriteGuardComponent  = "controller-plan-write-guard"

	controllerObjectPolicyWeight  = "-152"
	controllerObjectBindingWeight = "-147"
)

// ControllerJobWriteGuardPolicyName returns the stable release-owned name of
// the manager's structural Job write boundary.
func ControllerJobWriteGuardPolicyName(releaseNamespace, releaseName string, releaseSequence int32, managerImage string) string {
	return controllerObjectGuardPolicyName(controllerJobWriteGuardNamePrefix, releaseNamespace, releaseName, releaseSequence, managerImage)
}

// ControllerChunkWriteGuardPolicyName returns the stable release-owned name
// of the manager's structural plan-chunk ConfigMap write boundary.
func ControllerChunkWriteGuardPolicyName(releaseNamespace, releaseName string, releaseSequence int32, managerImage string) string {
	return controllerObjectGuardPolicyName(controllerChunkWriteGuardPrefix, releaseNamespace, releaseName, releaseSequence, managerImage)
}

// ControllerPlanWriteGuardPolicyName returns the stable release-owned name of
// the manager's structural PtahSchemaPlan write boundary.
func ControllerPlanWriteGuardPolicyName(releaseNamespace, releaseName string, releaseSequence int32, managerImage string) string {
	return controllerObjectGuardPolicyName(controllerPlanWriteGuardNamePrefix, releaseNamespace, releaseName, releaseSequence, managerImage)
}

func controllerObjectGuardPolicyName(prefix, releaseNamespace, releaseName string, releaseSequence int32, managerImage string) string {
	return prefix + controllerPrincipalGuardDigest(releaseNamespace, releaseName, releaseSequence, managerImage)
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

// ControllerObjectGuard verifies the typed, activation-parameterized
// structural boundaries around every main-resource object the manager may
// create or update. The validating webhook remains the authoritative
// reconstruction boundary; these policies independently reject broad or
// privileged shapes.
type ControllerObjectGuard struct {
	Policies                             ValidatingAdmissionPolicyReader
	Bindings                             ValidatingAdmissionPolicyBindingReader
	ReleaseName                          string
	ReleaseNamespace                     string
	ControllerServiceAccountName         string
	PreviousControllerServiceAccountName string
	PreviousControllerReleaseSequence    int32
	ReleaseSequence                      int32
	ManagerImage                         string
	PollEvery                            time.Duration
}

// NewControllerObjectGuard copies the stable release and manager identity
// from the rollout contract.
func NewControllerObjectGuard(rollout *RolloutGuard) *ControllerObjectGuard {
	if rollout == nil {
		return nil
	}
	return &ControllerObjectGuard{
		Policies:                             rollout.Policies,
		Bindings:                             rollout.Bindings,
		ReleaseName:                          rollout.ReleaseName,
		ReleaseNamespace:                     rollout.ReleaseNamespace,
		ControllerServiceAccountName:         rollout.ControllerServiceAccountName,
		PreviousControllerServiceAccountName: rollout.PreviousControllerServiceAccountName,
		PreviousControllerReleaseSequence:    rollout.PreviousControllerReleaseSequence,
		ReleaseSequence:                      rollout.ReleaseSequence,
		ManagerImage:                         rollout.ManagerImage,
		PollEvery:                            rollout.PollEvery,
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
			name:          ControllerJobWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage),
			component:     controllerJobWriteGuardComponent,
			apiGroups:     []string{"batch"},
			apiVersions:   []string{"v1"},
			resource:      "jobs",
			operations:    []admissionregistrationv1.OperationType{admissionregistrationv1.Create, admissionregistrationv1.Update},
			denialMessage: "Ptah controller Job write guard rejected an unsafe workload shape",
		},
		{
			name:          ControllerChunkWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage),
			component:     controllerChunkWriteGuardComponent,
			apiGroups:     []string{""},
			apiVersions:   []string{"v1"},
			resource:      "configmaps",
			operations:    []admissionregistrationv1.OperationType{admissionregistrationv1.Create},
			denialMessage: "Ptah controller chunk write guard rejected an unsafe ConfigMap shape",
		},
		{
			name:          ControllerPlanWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage),
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
	validations := make([]admissionregistrationv1.Validation, 0, len(entry.validations)+2)
	validations = append(validations, admissionregistrationv1.Validation{
		Expression: g.activationParameterExpression(),
		Message:    entry.denialMessage,
	}, admissionregistrationv1.Validation{
		Expression: controllerPrincipalAuthorityExpression(
			g.ReleaseNamespace,
			g.ControllerServiceAccountName,
			g.PreviousControllerServiceAccountName,
			g.ReleaseSequence,
			g.PreviousControllerReleaseSequence,
		),
		Message: controllerPrincipalGuardDenialMessage(),
	})
	validations = append(validations, entry.validations...)
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(entry),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			ParamKind:        &admissionregistrationv1.ParamKind{APIVersion: "v1", Kind: "ConfigMap"},
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(entry),
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name: "candidate-or-predecessor-controller-service-account",
				Expression: controllerPrincipalMatchExpression(
					g.ReleaseNamespace,
					g.ControllerServiceAccountName,
					g.PreviousControllerServiceAccountName,
				),
			}},
			Variables:   controllerObjectActivationVariables(g.ReleaseSequence, g.PreviousControllerReleaseSequence),
			Validations: validations,
		},
	}
	addAdmissionConvergenceDependencyProbe(
		policy,
		g.ReleaseNamespace,
		AdmissionConvergenceMarkerName(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence),
		hookIdentityDigest(g.ReleaseNamespace, g.ReleaseName, g.ReleaseSequence, g.ManagerImage),
	)
	return policy
}

func (g *ControllerObjectGuard) binding(entry controllerObjectGuardEntry) *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	deny := admissionregistrationv1.DenyAction
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(entry),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:     entry.name,
			MatchResources: g.matchResources(entry),
			ParamRef: &admissionregistrationv1.ParamRef{
				Name:                    ReleaseActivationName,
				Namespace:               g.ReleaseNamespace,
				ParameterNotFoundAction: &deny,
			},
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
	addAdmissionConvergenceProbeMatchResource(binding.Spec.MatchResources)
	return binding
}

func (g *ControllerObjectGuard) activationParameterExpression() string {
	activation := &ReleaseActivationGuard{ReleaseName: g.ReleaseName, ReleaseNamespace: g.ReleaseNamespace}
	return activation.activationObjectShapeExpression("params")
}

func controllerObjectActivationVariables(releaseSequence, previousReleaseSequence int32) []admissionregistrationv1.Variable {
	return []admissionregistrationv1.Variable{
		{Name: "activeRelease", Expression: decimalCEL("params", activeReleaseDataKey, true)},
		{
			Name: "activeControllerStateString",
			Expression: fmt.Sprintf(
				`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations ? params.metadata.annotations[%q] : ""`,
				ControllerStateVersionAnnotation,
				ControllerStateVersionAnnotation,
			),
		},
		{
			Name: "activeControllerState",
			Expression: fmt.Sprintf(
				`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations && params.metadata.annotations[%q].matches("^[1-9][0-9]*$") ? int(params.metadata.annotations[%q]) : 0`,
				ControllerStateVersionAnnotation,
				ControllerStateVersionAnnotation,
				ControllerStateVersionAnnotation,
			),
		},
		{
			Name: "activeControllerImage",
			Expression: fmt.Sprintf(
				`params != null && has(params.metadata.annotations) && %q in params.metadata.annotations ? params.metadata.annotations[%q] : ""`,
				ManagerImageAnnotation,
				ManagerImageAnnotation,
			),
		},
		{Name: "candidateRelease", Expression: fmt.Sprintf(`%d`, releaseSequence)},
		{Name: "previousRelease", Expression: fmt.Sprintf(`%d`, previousReleaseSequence)},
	}
}

func (g *ControllerObjectGuard) matchResources(entry controllerObjectGuardEntry) *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &exact,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
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
	if g.PreviousControllerServiceAccountName != "" &&
		g.PreviousControllerServiceAccountName != strings.TrimSpace(g.PreviousControllerServiceAccountName) {
		return fmt.Errorf("controller object guard predecessor ServiceAccount identity must not contain surrounding whitespace")
	}
	if g.ReleaseSequence < 1 || g.ManagerImage == "" || g.ManagerImage != strings.TrimSpace(g.ManagerImage) {
		return fmt.Errorf("controller object guard release identity is required")
	}
	if requirePoll && g.PollEvery <= 0 {
		return fmt.Errorf("controller object guard poll interval must be positive")
	}
	return nil
}

func controllerJobWriteValidations(message string) []admissionregistrationv1.Validation {
	validations := controllerObjectValidations(message,
		`has(object.metadata.labels) && object.metadata.labels.size() == 5 && ["app.kubernetes.io/managed-by", "app.kubernetes.io/component", "operator.ptah.dev/schema", "operator.ptah.dev/operation", "operator.ptah.dev/operation-id"].all(key, key in object.metadata.labels) && object.metadata.labels["app.kubernetes.io/managed-by"] == "ptah-operator" && object.metadata.labels["app.kubernetes.io/component"] == "schema-operation" && object.metadata.labels["operator.ptah.dev/schema"] != "" && object.metadata.labels["operator.ptah.dev/operation"] in ["resolve", "verify", "observe", "plan", "apply"] && object.metadata.labels["operator.ptah.dev/operation-id"].matches("^[0-9a-f]{16}$") && object.metadata.name.startsWith("ptah-" + object.metadata.labels["operator.ptah.dev/operation"] + "-") && object.metadata.name.matches("^ptah-(resolve|verify|observe|plan|apply)-[a-z0-9]([-a-z0-9.]*[a-z0-9])?-[0-9a-f]{16}$") && (!has(object.metadata.generateName) || object.metadata.generateName == "") && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.dev/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahSchema" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.dev/schema"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion`,
		`(request.operation == "UPDATE" && object.metadata.labels["operator.ptah.dev/operation"] in ["resolve", "verify", "observe", "plan"] && has(object.metadata.annotations) && object.metadata.annotations.size() == 5 && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$")) || (request.operation == "UPDATE" && object.metadata.labels["operator.ptah.dev/operation"] == "apply" && has(object.metadata.annotations) && object.metadata.annotations.size() == 7 && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/plan-fingerprint", "operator.ptah.dev/plan-content-digest", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/plan-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/plan-content-digest"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$")) || (has(object.metadata.annotations) && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/controller-image", "operator.ptah.dev/controller-revision", "operator.ptah.dev/controller-state-version", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations.all(key, key in ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/controller-image", "operator.ptah.dev/controller-revision", "operator.ptah.dev/controller-state-version", "operator.ptah.dev/admission-snapshot-digest", "operator.ptah.dev/plan-fingerprint", "operator.ptah.dev/plan-content-digest"]) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/controller-image"].matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/controller-revision"] != "" && object.metadata.annotations["operator.ptah.dev/controller-state-version"].matches("^[1-9][0-9]*$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$") && ((object.metadata.labels["operator.ptah.dev/operation"] == "apply" && "operator.ptah.dev/plan-fingerprint" in object.metadata.annotations && object.metadata.annotations["operator.ptah.dev/plan-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && "operator.ptah.dev/plan-content-digest" in object.metadata.annotations && object.metadata.annotations["operator.ptah.dev/plan-content-digest"].matches("^sha256:[0-9a-f]{64}$")) || (object.metadata.labels["operator.ptah.dev/operation"] != "apply" && !("operator.ptah.dev/plan-fingerprint" in object.metadata.annotations) && !("operator.ptah.dev/plan-content-digest" in object.metadata.annotations))))`,
		`has(dyn(object).spec.parallelism) && dyn(object).spec.parallelism == 1 && has(dyn(object).spec.completions) && dyn(object).spec.completions == 1 && has(dyn(object).spec.activeDeadlineSeconds) && dyn(object).spec.activeDeadlineSeconds >= 30 && dyn(object).spec.activeDeadlineSeconds <= 86400 && has(dyn(object).spec.backoffLimit) && dyn(object).spec.backoffLimit == 0 && !has(dyn(object).spec.backoffLimitPerIndex) && !has(dyn(object).spec.maxFailedIndexes) && has(dyn(object).spec.manualSelector) && !dyn(object).spec.manualSelector && has(dyn(object).spec.completionMode) && dyn(object).spec.completionMode == "NonIndexed" && has(dyn(object).spec.suspend) && !dyn(object).spec.suspend && has(dyn(object).spec.podReplacementPolicy) && dyn(object).spec.podReplacementPolicy == "Failed" && !has(dyn(object).spec.podFailurePolicy) && !has(dyn(object).spec.successPolicy) && !has(dyn(object).spec.managedBy) && ((request.operation == "CREATE" && !has(dyn(object).spec.ttlSecondsAfterFinished)) || (request.operation == "UPDATE" && has(dyn(object).spec.ttlSecondsAfterFinished) && dyn(object).spec.ttlSecondsAfterFinished == 300))`,
		`has(dyn(object).spec.template.metadata.labels) && ["app.kubernetes.io/managed-by", "app.kubernetes.io/component", "operator.ptah.dev/schema", "operator.ptah.dev/operation", "operator.ptah.dev/operation-id"].all(key, key in dyn(object).spec.template.metadata.labels && dyn(object).spec.template.metadata.labels[key] == object.metadata.labels[key]) && dyn(object).spec.template.metadata.labels.all(key, key in ["app.kubernetes.io/managed-by", "app.kubernetes.io/component", "operator.ptah.dev/schema", "operator.ptah.dev/operation", "operator.ptah.dev/operation-id", "batch.kubernetes.io/controller-uid", "batch.kubernetes.io/job-name", "controller-uid", "job-name"]) && has(dyn(object).spec.template.metadata.annotations) && dyn(object).spec.template.metadata.annotations == object.metadata.annotations && has(dyn(object).spec.template.spec.activeDeadlineSeconds) && dyn(object).spec.template.spec.activeDeadlineSeconds == dyn(object).spec.activeDeadlineSeconds && has(dyn(object).spec.template.spec.automountServiceAccountToken) && !dyn(object).spec.template.spec.automountServiceAccountToken && has(dyn(object).spec.template.spec.enableServiceLinks) && !dyn(object).spec.template.spec.enableServiceLinks && dyn(object).spec.template.spec.serviceAccountName != "" && dyn(object).spec.template.spec.restartPolicy == "Never" && (!has(dyn(object).spec.template.spec.hostNetwork) || !dyn(object).spec.template.spec.hostNetwork) && (!has(dyn(object).spec.template.spec.hostPID) || !dyn(object).spec.template.spec.hostPID) && (!has(dyn(object).spec.template.spec.hostIPC) || !dyn(object).spec.template.spec.hostIPC) && (!has(dyn(object).spec.template.spec.shareProcessNamespace) || !dyn(object).spec.template.spec.shareProcessNamespace) && dyn(object).spec.template.spec.containers.size() == 1 && dyn(object).spec.template.spec.containers[0].name == "ptah" && ((object.metadata.labels["operator.ptah.dev/operation"] in ["observe", "plan"] && dyn(object).spec.template.spec.initContainers.size() == 3) || (!(object.metadata.labels["operator.ptah.dev/operation"] in ["observe", "plan"]) && dyn(object).spec.template.spec.initContainers.size() == 1)) && dyn(object).spec.template.spec.containers.all(container, has(container.securityContext) && has(container.securityContext.allowPrivilegeEscalation) && !container.securityContext.allowPrivilegeEscalation && (!has(container.securityContext.privileged) || !container.securityContext.privileged) && has(container.securityContext.readOnlyRootFilesystem) && container.securityContext.readOnlyRootFilesystem && has(container.securityContext.runAsNonRoot) && container.securityContext.runAsNonRoot && has(container.securityContext.capabilities) && container.securityContext.capabilities.drop == ["ALL"]) && dyn(object).spec.template.spec.initContainers.all(container, has(container.securityContext) && has(container.securityContext.allowPrivilegeEscalation) && !container.securityContext.allowPrivilegeEscalation && (!has(container.securityContext.privileged) || !container.securityContext.privileged) && has(container.securityContext.readOnlyRootFilesystem) && container.securityContext.readOnlyRootFilesystem && has(container.securityContext.runAsNonRoot) && container.securityContext.runAsNonRoot && has(container.securityContext.capabilities) && container.securityContext.capabilities.drop == ["ALL"])`,
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
		`request.operation != "CREATE" || !has(dyn(object).status) || dyn(dyn(object).status).size() == 0`,
		`request.operation != "UPDATE" || (oldObject != null && !has(dyn(oldObject).spec.ttlSecondsAfterFinished) && has(dyn(object).spec.ttlSecondsAfterFinished) && dyn(object).spec.ttlSecondsAfterFinished == 300 && has(dyn(oldObject).status.conditions) && dyn(oldObject).status.conditions.exists(condition, condition.status == "True" && condition.type in ["Complete", "Failed"]) && object.metadata.name == oldObject.metadata.name && object.metadata.namespace == oldObject.metadata.namespace && object.metadata.uid == oldObject.metadata.uid && object.metadata.labels == oldObject.metadata.labels && object.metadata.annotations == oldObject.metadata.annotations && object.metadata.ownerReferences == oldObject.metadata.ownerReferences && has(object.metadata.finalizers) == has(oldObject.metadata.finalizers) && (!has(object.metadata.finalizers) || object.metadata.finalizers == oldObject.metadata.finalizers) && has(object.metadata.generateName) == has(oldObject.metadata.generateName) && (!has(object.metadata.generateName) || object.metadata.generateName == oldObject.metadata.generateName) && has(object.metadata.deletionTimestamp) == has(oldObject.metadata.deletionTimestamp) && (!has(object.metadata.deletionTimestamp) || object.metadata.deletionTimestamp == oldObject.metadata.deletionTimestamp) && has(dyn(object).status) == has(dyn(oldObject).status) && (!has(dyn(object).status) || dyn(object).status == dyn(oldObject).status) && dyn(object).spec.parallelism == dyn(oldObject).spec.parallelism && dyn(object).spec.completions == dyn(oldObject).spec.completions && dyn(object).spec.activeDeadlineSeconds == dyn(oldObject).spec.activeDeadlineSeconds && has(dyn(object).spec.podFailurePolicy) == has(dyn(oldObject).spec.podFailurePolicy) && (!has(dyn(object).spec.podFailurePolicy) || dyn(object).spec.podFailurePolicy == dyn(oldObject).spec.podFailurePolicy) && has(dyn(object).spec.successPolicy) == has(dyn(oldObject).spec.successPolicy) && (!has(dyn(object).spec.successPolicy) || dyn(object).spec.successPolicy == dyn(oldObject).spec.successPolicy) && dyn(object).spec.backoffLimit == dyn(oldObject).spec.backoffLimit && has(dyn(object).spec.backoffLimitPerIndex) == has(dyn(oldObject).spec.backoffLimitPerIndex) && (!has(dyn(object).spec.backoffLimitPerIndex) || dyn(object).spec.backoffLimitPerIndex == dyn(oldObject).spec.backoffLimitPerIndex) && has(dyn(object).spec.maxFailedIndexes) == has(dyn(oldObject).spec.maxFailedIndexes) && (!has(dyn(object).spec.maxFailedIndexes) || dyn(object).spec.maxFailedIndexes == dyn(oldObject).spec.maxFailedIndexes) && has(dyn(object).spec.selector) == has(dyn(oldObject).spec.selector) && (!has(dyn(object).spec.selector) || dyn(object).spec.selector == dyn(oldObject).spec.selector) && dyn(object).spec.manualSelector == dyn(oldObject).spec.manualSelector && dyn(object).spec.template == dyn(oldObject).spec.template && dyn(object).spec.completionMode == dyn(oldObject).spec.completionMode && dyn(object).spec.suspend == dyn(oldObject).spec.suspend && dyn(object).spec.podReplacementPolicy == dyn(oldObject).spec.podReplacementPolicy && has(dyn(object).spec.managedBy) == has(dyn(oldObject).spec.managedBy) && (!has(dyn(object).spec.managedBy) || dyn(object).spec.managedBy == dyn(oldObject).spec.managedBy))`,
	)
	validations[1].Expression = controllerJobAnnotationContractExpression()
	return validations
}

func controllerJobAnnotationContractExpression() string {
	legacyReadOnly := `object.metadata.labels["operator.ptah.dev/operation"] in ["resolve", "verify", "observe", "plan"] && has(object.metadata.annotations) && object.metadata.annotations.size() == 5 && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$")`
	legacyApply := `object.metadata.labels["operator.ptah.dev/operation"] == "apply" && has(object.metadata.annotations) && object.metadata.annotations.size() == 7 && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/plan-fingerprint", "operator.ptah.dev/plan-content-digest", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/plan-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/plan-content-digest"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$")`
	current := `has(object.metadata.annotations) && ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/controller-image", "operator.ptah.dev/controller-revision", "operator.ptah.dev/controller-state-version", "operator.ptah.dev/admission-snapshot-digest"].all(key, key in object.metadata.annotations) && object.metadata.annotations.all(key, key in ["operator.ptah.dev/operation-id", "operator.ptah.dev/input-fingerprint", "operator.ptah.dev/ptah-version", "operator.ptah.dev/execution-binding-id", "operator.ptah.dev/controller-image", "operator.ptah.dev/controller-revision", "operator.ptah.dev/controller-state-version", "operator.ptah.dev/admission-snapshot-digest", "operator.ptah.dev/plan-fingerprint", "operator.ptah.dev/plan-content-digest"]) && object.metadata.annotations["operator.ptah.dev/operation-id"] != "" && object.metadata.annotations["operator.ptah.dev/input-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/ptah-version"] != "" && object.metadata.annotations["operator.ptah.dev/execution-binding-id"].matches("^v1-[0-9a-f]{32}$") && object.metadata.annotations["operator.ptah.dev/controller-image"].matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && object.metadata.annotations["operator.ptah.dev/controller-revision"] != "" && object.metadata.annotations["operator.ptah.dev/controller-state-version"].matches("^[1-9][0-9]*$") && object.metadata.annotations["operator.ptah.dev/admission-snapshot-digest"].matches("^sha256:[0-9a-f]{64}$") && ((object.metadata.labels["operator.ptah.dev/operation"] == "apply" && "operator.ptah.dev/plan-fingerprint" in object.metadata.annotations && object.metadata.annotations["operator.ptah.dev/plan-fingerprint"].matches("^sha256:[0-9a-f]{64}$") && "operator.ptah.dev/plan-content-digest" in object.metadata.annotations && object.metadata.annotations["operator.ptah.dev/plan-content-digest"].matches("^sha256:[0-9a-f]{64}$")) || (object.metadata.labels["operator.ptah.dev/operation"] != "apply" && !("operator.ptah.dev/plan-fingerprint" in object.metadata.annotations) && !("operator.ptah.dev/plan-content-digest" in object.metadata.annotations)))`
	activeIdentity := `object.metadata.annotations["operator.ptah.dev/controller-image"] == variables.activeControllerImage && object.metadata.annotations["operator.ptah.dev/controller-state-version"] == variables.activeControllerStateString`
	return fmt.Sprintf(
		`((request.operation == "UPDATE" || (request.operation == "CREATE" && variables.activeRelease == variables.previousRelease)) && ((%s) || (%s))) || ((%s) && (request.operation == "UPDATE" || (request.operation == "CREATE" && (%s))))`,
		legacyReadOnly,
		legacyApply,
		current,
		activeIdentity,
	)
}

const controllerJobPodBoundaryExpression = `has(dyn(object).spec.template.spec.securityContext) && has(dyn(object).spec.template.spec.securityContext.runAsNonRoot) && dyn(object).spec.template.spec.securityContext.runAsNonRoot && has(dyn(object).spec.template.spec.securityContext.runAsUser) && dyn(object).spec.template.spec.securityContext.runAsUser == 65532 && has(dyn(object).spec.template.spec.securityContext.runAsGroup) && dyn(object).spec.template.spec.securityContext.runAsGroup == 65532 && has(dyn(object).spec.template.spec.securityContext.fsGroup) && dyn(object).spec.template.spec.securityContext.fsGroup == 65532 && has(dyn(object).spec.template.spec.securityContext.fsGroupChangePolicy) && dyn(object).spec.template.spec.securityContext.fsGroupChangePolicy == "OnRootMismatch" && has(dyn(object).spec.template.spec.securityContext.seccompProfile) && dyn(object).spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(dyn(object).spec.template.spec.securityContext.sysctls) || dyn(object).spec.template.spec.securityContext.sysctls.size() == 0) && dyn(object).spec.template.spec.volumes.size() >= 2 && dyn(object).spec.template.spec.volumes.size() <= 7 && dyn(object).spec.template.spec.volumes.all(volume, (volume.name in ["runner", "work", "schema-source", "fetch-work", "registry-ca-snapshot"] && has(volume.emptyDir)) || (volume.name in ["verification-policy", "registry-ca"] && has(volume.configMap)) || (volume.name == "registry-docker-config" && has(volume.secret)) || (volume.name == "plan" && has(volume.projected) && volume.projected.sources.size() >= 1 && volume.projected.sources.all(source, has(source.configMap)))) && dyn(object).spec.template.spec.containers.all(container, container.image.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && container.imagePullPolicy == "IfNotPresent" && (!has(container.envFrom) || container.envFrom.size() == 0) && (!has(container.volumeDevices) || container.volumeDevices.size() == 0) && has(container.securityContext.runAsUser) && container.securityContext.runAsUser == 65532 && has(container.securityContext.runAsGroup) && container.securityContext.runAsGroup == 65532 && has(container.securityContext.seccompProfile) && container.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(container.securityContext.capabilities.add) || container.securityContext.capabilities.add.size() == 0)) && dyn(object).spec.template.spec.initContainers.all(container, container.image.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && container.imagePullPolicy == "IfNotPresent" && (!has(container.envFrom) || container.envFrom.size() == 0) && (!has(container.volumeDevices) || container.volumeDevices.size() == 0) && has(container.securityContext.runAsUser) && container.securityContext.runAsUser == 65532 && has(container.securityContext.runAsGroup) && container.securityContext.runAsGroup == 65532 && has(container.securityContext.seccompProfile) && container.securityContext.seccompProfile.type == "RuntimeDefault" && (!has(container.securityContext.capabilities.add) || container.securityContext.capabilities.add.size() == 0)) && ((object.metadata.labels["operator.ptah.dev/operation"] in ["observe", "plan"] && dyn(object).spec.template.spec.initContainers.map(container, container.name) == ["install-runner", "validate-source-authority", "fetch-schema"]) || (!(object.metadata.labels["operator.ptah.dev/operation"] in ["observe", "plan"]) && dyn(object).spec.template.spec.initContainers.map(container, container.name) == ["install-runner"]))`

func controllerJobSupportedWindowTopLevelExpression(root string) string {
	return fmt.Sprintf(
		`!has(dyn(dyn(%[1]s).spec).scheduling) && !has(dyn(dyn(%[1]s).spec.template.spec).evictionResponders) && !has(dyn(dyn(%[1]s).spec.template.spec).workloadRef)`,
		root,
	)
}

func controllerJobSupportedWindowVolumeExpression(root string) string {
	return fmt.Sprintf(
		`dyn(%[1]s).spec.template.spec.volumes.all(volume, (!has(volume.emptyDir) || !has(dyn(volume.emptyDir).mode)) && (!has(volume.configMap) || (!has(dyn(volume.configMap).defaultUser) && (!has(volume.configMap.items) || volume.configMap.items.all(item, !has(dyn(item).user))))) && (!has(volume.secret) || (!has(dyn(volume.secret).defaultUser) && (!has(volume.secret.items) || volume.secret.items.all(item, !has(dyn(item).user))))) && (!has(volume.projected) || !has(dyn(volume.projected).defaultUser)) && (!has(volume.downwardAPI) || (!has(dyn(volume.downwardAPI).defaultUser) && (!has(volume.downwardAPI.items) || volume.downwardAPI.items.all(item, !has(dyn(item).user))))))`,
		root,
	)
}

func controllerJobSupportedWindowProjectionExpression(root string) string {
	return fmt.Sprintf(
		`dyn(%[1]s).spec.template.spec.volumes.all(volume, !has(volume.projected) || volume.projected.sources.all(source, (!has(source.configMap) || !has(source.configMap.items) || source.configMap.items.all(item, !has(dyn(item).user))) && (!has(source.secret) || !has(source.secret.items) || source.secret.items.all(item, !has(dyn(item).user))) && (!has(source.serviceAccountToken) || !has(dyn(source.serviceAccountToken).user)) && (!has(source.clusterTrustBundle) || !has(dyn(source.clusterTrustBundle).user)) && (!has(source.podCertificate) || !has(dyn(source.podCertificate).user)) && (!has(source.downwardAPI) || !has(source.downwardAPI.items) || source.downwardAPI.items.all(item, !has(dyn(item).user)))))`,
		root,
	)
}

func controllerJobSupportedWindowMountExpression(root string) string {
	return fmt.Sprintf(
		`dyn(%[1]s).spec.template.spec.containers.all(container, !has(container.volumeMounts) || container.volumeMounts.all(mount, !has(dyn(mount).bindMountOptions))) && dyn(%[1]s).spec.template.spec.initContainers.all(container, !has(container.volumeMounts) || container.volumeMounts.all(mount, !has(dyn(mount).bindMountOptions)))`,
		root,
	)
}

func controllerJobSupportedWindowContainerExpression(root string) string {
	return fmt.Sprintf(
		`(!has(dyn(%[1]s).spec.template.spec.ephemeralContainers) || dyn(%[1]s).spec.template.spec.ephemeralContainers.size() == 0) && dyn(%[1]s).spec.template.spec.containers.all(container, !has(container.lifecycle) && !has(container.livenessProbe) && !has(container.readinessProbe) && !has(container.startupProbe)) && dyn(%[1]s).spec.template.spec.initContainers.all(container, !has(container.lifecycle) && !has(container.livenessProbe) && !has(container.readinessProbe) && !has(container.startupProbe))`,
		root,
	)
}

func controllerJobPreviousObjectExpression(expression string) string {
	return `request.operation != "UPDATE" || (oldObject != null && ` + expression + `)`
}

func controllerChunkWriteValidations(message string) []admissionregistrationv1.Validation {
	return controllerObjectValidations(message,
		`has(object.metadata.labels) && object.metadata.labels.size() == 2 && ["operator.ptah.dev/plan", "operator.ptah.dev/schema"].all(key, key in object.metadata.labels && object.metadata.labels[key] != "") && object.metadata.name.matches("^ptah-plan-[0-9a-f]{24}-[0-9]{3}$") && object.metadata.name.startsWith(object.metadata.labels["operator.ptah.dev/plan"] + "-") && (!has(object.metadata.annotations) || object.metadata.annotations.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && (!has(object.metadata.generateName) || object.metadata.generateName == "") && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.dev/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahSchemaPlan" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.dev/plan"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion`,
		`has(dyn(object).immutable) && dyn(object).immutable && (!has(dyn(object).data) || dyn(object).data.size() == 0) && has(dyn(object).binaryData) && dyn(object).binaryData.size() == 1 && "chunk" in dyn(object).binaryData && dyn(object).binaryData["chunk"].size() >= 1 && dyn(object).binaryData["chunk"].size() <= 524288`,
	)
}

func controllerPlanWriteValidations(message string) []admissionregistrationv1.Validation {
	// Candidate-only v3 fields are accessed through dyn because this policy is
	// installed and type-checked before the predecessor CRD is upgraded. The
	// only identity-free branch is the exact v2 bootstrap contract.
	validations := controllerObjectValidations(message,
		`has(object.metadata.labels) && object.metadata.labels.size() == 1 && "operator.ptah.dev/schema" in object.metadata.labels && object.metadata.labels["operator.ptah.dev/schema"] != "" && object.metadata.name.matches("^ptah-plan-[0-9a-f]{24}$") && (!has(object.metadata.annotations) || object.metadata.annotations.size() == 0) && (!has(object.metadata.finalizers) || object.metadata.finalizers.size() == 0) && (!has(object.metadata.generateName) || object.metadata.generateName == "") && !has(object.metadata.deletionTimestamp) && has(object.metadata.ownerReferences) && object.metadata.ownerReferences.size() == 1 && object.metadata.ownerReferences[0].apiVersion == "operator.ptah.dev/v1alpha1" && object.metadata.ownerReferences[0].kind == "PtahSchema" && object.metadata.ownerReferences[0].name == object.metadata.labels["operator.ptah.dev/schema"] && object.metadata.ownerReferences[0].uid != "" && has(object.metadata.ownerReferences[0].controller) && object.metadata.ownerReferences[0].controller && has(object.metadata.ownerReferences[0].blockOwnerDeletion) && object.metadata.ownerReferences[0].blockOwnerDeletion && dyn(object).spec.schemaRef.name == object.metadata.labels["operator.ptah.dev/schema"] && dyn(object).spec.schemaRef.uid == object.metadata.ownerReferences[0].uid`,
		`dyn(object).spec.contractVersion == 3 && dyn(object).spec.fingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.contentDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.artifactDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.coordinationDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.targetIdentityDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.actualStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.desiredStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.policyFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.verificationPolicyUID != "" && dyn(object).spec.verificationPolicyDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.executionBindingID.matches("^v1-[0-9a-f]{32}$") && has(dyn(dyn(object).spec).controllerImage) && dyn(dyn(object).spec).controllerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && has(dyn(dyn(object).spec).controllerRevision) && dyn(dyn(object).spec).controllerRevision != "" && has(dyn(dyn(object).spec).controllerStateVersion) && dyn(dyn(object).spec).controllerStateVersion >= 1 && dyn(object).spec.ptahVersion != "" && dyn(object).spec.executorImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object).spec.runnerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object).spec.runnerProtocolVersion >= 1 && dyn(object).spec.dialect != "" && dyn(object).spec.statementCount >= 1 && dyn(object).spec.size >= 1 && dyn(object).spec.size <= 8388608`,
		`dyn(object).spec.chunks.size() >= 1 && dyn(object).spec.chunks.size() <= 16 && dyn(object).spec.chunks.all(chunk, chunk.key == "chunk" && chunk.name.matches("^ptah-plan-[0-9a-f]{24}-[0-9]{3}$") && chunk.name.startsWith(object.metadata.name + "-") && chunk.index >= 0 && chunk.index < dyn(object).spec.chunks.size() && chunk.digest.matches("^sha256:[0-9a-f]{64}$") && chunk.size >= 1 && chunk.size <= 524288)`,
		`!has(dyn(object).status)`,
	)
	validations[1].Expression = controllerPlanContractExpression()
	return validations
}

func controllerPlanContractExpression() string {
	common := `dyn(object).spec.fingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.contentDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.artifactDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.coordinationDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.targetIdentityDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.actualStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.desiredStateFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.policyFingerprint.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.verificationPolicyUID != "" && dyn(object).spec.verificationPolicyDigest.matches("^sha256:[0-9a-f]{64}$") && dyn(object).spec.executionBindingID.matches("^v1-[0-9a-f]{32}$") && dyn(object).spec.ptahVersion != "" && dyn(object).spec.executorImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object).spec.runnerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(object).spec.runnerProtocolVersion >= 1 && dyn(object).spec.dialect != "" && dyn(object).spec.statementCount >= 1 && dyn(object).spec.size >= 1 && dyn(object).spec.size <= 8388608`
	legacy := `variables.activeRelease == variables.previousRelease && dyn(object).spec.contractVersion == 2 && !has(dyn(dyn(object).spec).controllerImage) && !has(dyn(dyn(object).spec).controllerRevision) && !has(dyn(dyn(object).spec).controllerStateVersion)`
	current := `dyn(object).spec.contractVersion == 3 && has(dyn(dyn(object).spec).controllerImage) && dyn(dyn(object).spec).controllerImage.matches("^[^[:space:]@]+@sha256:[0-9a-f]{64}$") && dyn(dyn(object).spec).controllerImage == variables.activeControllerImage && has(dyn(dyn(object).spec).controllerRevision) && dyn(dyn(object).spec).controllerRevision != "" && has(dyn(dyn(object).spec).controllerStateVersion) && dyn(dyn(object).spec).controllerStateVersion >= 1 && dyn(dyn(object).spec).controllerStateVersion == variables.activeControllerState`
	return fmt.Sprintf(`(%s) && ((%s) || (%s))`, common, legacy, current)
}

func controllerObjectValidations(message string, expressions ...string) []admissionregistrationv1.Validation {
	validations := make([]admissionregistrationv1.Validation, len(expressions))
	for index, expression := range expressions {
		validations[index] = admissionregistrationv1.Validation{Expression: expression, Message: message}
	}
	return validations
}
