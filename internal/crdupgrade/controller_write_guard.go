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
	controllerWriteGuardNamePrefix = "ptah-operator-controller-write-guard-v1-"
	controllerWriteGuardComponent  = "controller-write-guard"
	controllerWritePolicyWeight    = "-158"
	controllerWriteBindingWeight   = "-157"

	activeOperationFinalizer = "operator.ptah.dev/active-operation"
)

// ControllerWriteGuardPolicyName returns the stable, versioned name of the
// release-owned desired-state boundary for the controller ServiceAccount.
// The release sequence is intentionally excluded: a compatible controller
// update must continue to satisfy the same fail-closed contract.
func ControllerWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	identity := releaseNamespace + "\n" + releaseName
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return controllerWriteGuardNamePrefix + digest[:12]
}

func controllerWriteGuardDenialMessage() string {
	return "Ptah controller write guard rejected a desired-state mutation"
}

// ControllerWriteGuard confines main-resource PtahSchema patches made by the
// controller identity to the one finalizer it owns. Status writes use the
// status subresource and therefore do not match this policy.
type ControllerWriteGuard struct {
	Policies                     ValidatingAdmissionPolicyReader
	Bindings                     ValidatingAdmissionPolicyBindingReader
	ReleaseName                  string
	ReleaseNamespace             string
	ControllerServiceAccountName string
	PollEvery                    time.Duration
}

// NewControllerWriteGuard copies the stable release and controller identity
// from the rollout contract.
func NewControllerWriteGuard(rollout *RolloutGuard) *ControllerWriteGuard {
	if rollout == nil {
		return nil
	}
	return &ControllerWriteGuard{
		Policies:                     rollout.Policies,
		Bindings:                     rollout.Bindings,
		ReleaseName:                  rollout.ReleaseName,
		ReleaseNamespace:             rollout.ReleaseNamespace,
		ControllerServiceAccountName: rollout.ControllerServiceAccountName,
		PollEvery:                    rollout.PollEvery,
	}
}

// Verify requires the retained policy and binding to match the compiled
// contract exactly.
func (g *ControllerWriteGuard) Verify(ctx context.Context) error {
	if err := g.validate(false); err != nil {
		return err
	}
	name := ControllerWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	policy, err := g.Policies.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get controller write guard policy: %w", err)
	}
	if err := g.verifyPolicy(policy); err != nil {
		return err
	}
	binding, err := g.Bindings.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get controller write guard binding: %w", err)
	}
	return g.verifyBinding(binding)
}

// WaitReady verifies the immutable contract and waits for successful API
// server CEL type checking before any controller workload can roll out.
func (g *ControllerWriteGuard) WaitReady(ctx context.Context) error {
	if err := g.validate(true); err != nil {
		return err
	}
	if err := g.Verify(ctx); err != nil {
		return err
	}
	name := ControllerWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		policy, err := g.Policies.Get(pollCtx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("read controller write guard policy status: %w", err)
		}
		if err := g.verifyPolicy(policy); err != nil {
			return false, err
		}
		if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
			return false, nil
		}
		if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) != 0 {
			return false, fmt.Errorf("controller write guard policy has CEL type-check warnings: %s", warnings[0].Warning)
		}
		return true, nil
	})
}

func (g *ControllerWriteGuard) policy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	name := ControllerWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	username := "system:serviceaccount:" + g.ReleaseNamespace + ":" + g.ControllerServiceAccountName
	message := controllerWriteGuardDenialMessage()
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(),
			MatchConditions: []admissionregistrationv1.MatchCondition{{
				Name:       "exact-controller-service-account",
				Expression: fmt.Sprintf(`request.userInfo.username == %q`, username),
			}},
			Variables: []admissionregistrationv1.Variable{
				{Name: "oldFinalizers", Expression: `has(oldObject.metadata.finalizers) ? oldObject.metadata.finalizers : []`},
				{Name: "newFinalizers", Expression: `has(object.metadata.finalizers) ? object.metadata.finalizers : []`},
				{Name: "activeFinalizer", Expression: fmt.Sprintf(`%q`, activeOperationFinalizer)},
				{Name: "oldActiveCount", Expression: `variables.oldFinalizers.filter(value, value == variables.activeFinalizer).size()`},
				{Name: "newActiveCount", Expression: `variables.newFinalizers.filter(value, value == variables.activeFinalizer).size()`},
			},
			Validations: []admissionregistrationv1.Validation{
				{Expression: `object.spec == oldObject.spec`, Message: message},
				{Expression: `has(object.status) == has(oldObject.status) && (!has(object.status) || object.status == oldObject.status)`, Message: message},
				{
					Expression: `has(object.metadata.labels) == has(oldObject.metadata.labels) && (!has(object.metadata.labels) || object.metadata.labels == oldObject.metadata.labels) && has(object.metadata.annotations) == has(oldObject.metadata.annotations) && (!has(object.metadata.annotations) || object.metadata.annotations == oldObject.metadata.annotations) && has(object.metadata.ownerReferences) == has(oldObject.metadata.ownerReferences) && (!has(object.metadata.ownerReferences) || object.metadata.ownerReferences == oldObject.metadata.ownerReferences)`,
					Message:    message,
				},
				{
					Expression: `(variables.oldActiveCount <= 1 && variables.newActiveCount <= 1) && ((variables.oldActiveCount == variables.newActiveCount && variables.oldFinalizers == variables.newFinalizers) || (variables.oldActiveCount == 0 && variables.newActiveCount == 1 && variables.newFinalizers.filter(value, value != variables.activeFinalizer) == variables.oldFinalizers) || (variables.oldActiveCount == 1 && variables.newActiveCount == 0 && variables.newFinalizers == variables.oldFinalizers.filter(value, value != variables.activeFinalizer)))`,
					Message:    message,
				},
			},
		},
	}
}

func (g *ControllerWriteGuard) binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	name := ControllerWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	return &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
		ObjectMeta: g.metadata(name),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			MatchResources:    g.matchResources(),
			ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
		},
	}
}

func (g *ControllerWriteGuard) matchResources() *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.MatchResources{
		MatchPolicy: &exact,
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{"operator.ptah.dev"},
					APIVersions: []string{"v1alpha1"},
					Resources:   []string{"ptahschemas"},
					Scope:       scopePtr(admissionregistrationv1.NamespacedScope),
				},
			},
		}},
	}
}

func (g *ControllerWriteGuard) metadata(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name,
		Annotations: map[string]string{
			rolloutGuardVersionAnnotation: rolloutGuardVersion,
			ReleaseNameAnnotation:         g.ReleaseName,
			ReleaseNamespaceAnnotation:    g.ReleaseNamespace,
		},
		Labels: map[string]string{
			managedByLabel:                rolloutGuardManagedBy,
			instanceLabel:                 g.ReleaseName,
			"app.kubernetes.io/component": controllerWriteGuardComponent,
		},
	}
}

func (g *ControllerWriteGuard) verifyPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	name := ControllerWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	if policy == nil || policy.Name != name {
		return fmt.Errorf("fixed controller write guard policy %s is missing", name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicy", policy.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(policy.Spec, g.policy().Spec) {
		return fmt.Errorf("controller write guard policy spec differs from the immutable contract")
	}
	return nil
}

func (g *ControllerWriteGuard) verifyBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	name := ControllerWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	if binding == nil || binding.Name != name {
		return fmt.Errorf("fixed controller write guard binding %s is missing", name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicyBinding", binding.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(binding.Spec, g.binding().Spec) {
		return fmt.Errorf("controller write guard binding spec differs from the immutable contract")
	}
	return nil
}

func (g *ControllerWriteGuard) verifyMetadata(kind string, metadata metav1.ObjectMeta) error {
	expected := g.metadata(ControllerWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName))
	if metadata.Name != expected.Name {
		return fmt.Errorf("fixed controller write guard %s has an unexpected name", kind)
	}
	for key, value := range expected.Annotations {
		if metadata.Annotations[key] != value {
			return fmt.Errorf("fixed controller write guard %s has foreign or incomplete ownership", kind)
		}
	}
	for key, value := range expected.Labels {
		if metadata.Labels[key] != value {
			return fmt.Errorf("fixed controller write guard %s has foreign or incomplete ownership", kind)
		}
	}
	return nil
}

func (g *ControllerWriteGuard) validate(requirePoll bool) error {
	if g == nil || g.Policies == nil || g.Bindings == nil {
		return fmt.Errorf("controller write guard policy clients are required")
	}
	if g.ReleaseName == "" || g.ReleaseNamespace == "" || g.ControllerServiceAccountName == "" ||
		g.ReleaseName != strings.TrimSpace(g.ReleaseName) ||
		g.ReleaseNamespace != strings.TrimSpace(g.ReleaseNamespace) ||
		g.ControllerServiceAccountName != strings.TrimSpace(g.ControllerServiceAccountName) {
		return fmt.Errorf("controller write guard release and ServiceAccount identity is required")
	}
	if requirePoll && g.PollEvery <= 0 {
		return fmt.Errorf("controller write guard poll interval must be positive")
	}
	return nil
}
