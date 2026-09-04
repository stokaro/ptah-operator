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
	namespaceDeletionGuardNamePrefix = "ptah-operator-namespace-deletion-guard-v1-"
	namespaceDeletionGuardComponent  = "namespace-deletion-guard"
	namespaceDeletionPolicyWeight    = "-160"
	namespaceDeletionBindingWeight   = "-159"
)

// NamespaceDeletionGuardPolicyName returns the versioned, release-owned name
// of the admission boundary that keeps the release Namespace alive. The
// release sequence is intentionally excluded: one stable guard protects all
// compatible updates, while a future incompatible contract gets a new
// versioned prefix and is proven before this version is retired.
func NamespaceDeletionGuardPolicyName(releaseNamespace, releaseName string) string {
	identity := releaseNamespace + "\n" + releaseName
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	return namespaceDeletionGuardNamePrefix + digest[:12]
}

func namespaceDeletionGuardDenialMessage() string {
	return "Ptah namespace deletion guard rejected deletion of the release Namespace"
}

// NamespaceDeletionGuard is a parameterless admission boundary around the
// exact release Namespace. It has no caller exception: safe teardown must
// remove its binding and policy last, after every other cluster-scoped
// admission resource owned by the release has been removed.
type NamespaceDeletionGuard struct {
	Policies         ValidatingAdmissionPolicyReader
	Bindings         ValidatingAdmissionPolicyBindingReader
	ReleaseName      string
	ReleaseNamespace string
	PollEvery        time.Duration
}

// NewNamespaceDeletionGuard copies the stable release identity and policy
// readers from a rollout guard.
func NewNamespaceDeletionGuard(rollout *RolloutGuard) *NamespaceDeletionGuard {
	if rollout == nil {
		return nil
	}
	return &NamespaceDeletionGuard{
		Policies:         rollout.Policies,
		Bindings:         rollout.Bindings,
		ReleaseName:      rollout.ReleaseName,
		ReleaseNamespace: rollout.ReleaseNamespace,
		PollEvery:        rollout.PollEvery,
	}
}

// Verify requires the retained policy and binding to match the compiled,
// parameterless contract exactly.
func (g *NamespaceDeletionGuard) Verify(ctx context.Context) error {
	if err := g.validate(false); err != nil {
		return err
	}
	name := NamespaceDeletionGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	policy, err := g.Policies.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get namespace deletion guard policy: %w", err)
	}
	if err := g.verifyPolicy(policy); err != nil {
		return err
	}
	binding, err := g.Bindings.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get namespace deletion guard binding: %w", err)
	}
	return g.verifyBinding(binding)
}

// WaitReady verifies the immutable contract and waits until the API server
// has observed the policy without CEL type-check warnings.
func (g *NamespaceDeletionGuard) WaitReady(ctx context.Context) error {
	if err := g.validate(true); err != nil {
		return err
	}
	if err := g.Verify(ctx); err != nil {
		return err
	}
	name := NamespaceDeletionGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	return wait.PollUntilContextCancel(ctx, g.PollEvery, true, func(pollCtx context.Context) (bool, error) {
		policy, err := g.Policies.Get(pollCtx, name, metav1.GetOptions{})
		if err != nil {
			return false, fmt.Errorf("read namespace deletion guard policy status: %w", err)
		}
		if err := g.verifyPolicy(policy); err != nil {
			return false, err
		}
		if policy.Status.ObservedGeneration != policy.Generation || policy.Status.TypeChecking == nil {
			return false, nil
		}
		if warnings := policy.Status.TypeChecking.ExpressionWarnings; len(warnings) != 0 {
			return false, fmt.Errorf("namespace deletion guard policy has CEL type-check warnings: %s", warnings[0].Warning)
		}
		return true, nil
	})
}

func (g *NamespaceDeletionGuard) policy() *admissionregistrationv1.ValidatingAdmissionPolicy {
	fail := admissionregistrationv1.Fail
	name := NamespaceDeletionGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	return &admissionregistrationv1.ValidatingAdmissionPolicy{
		TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
		ObjectMeta: g.metadata(name),
		Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
			FailurePolicy:    &fail,
			MatchConstraints: g.matchResources(),
			Validations: []admissionregistrationv1.Validation{{
				Expression: "false",
				Message:    namespaceDeletionGuardDenialMessage(),
			}},
		},
	}
}

func (g *NamespaceDeletionGuard) binding() *admissionregistrationv1.ValidatingAdmissionPolicyBinding {
	name := NamespaceDeletionGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
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

func (g *NamespaceDeletionGuard) matchResources() *admissionregistrationv1.MatchResources {
	exact := admissionregistrationv1.Exact
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &exact,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
			RuleWithOperations: admissionregistrationv1.RuleWithOperations{
				Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Delete},
				Rule: admissionregistrationv1.Rule{
					APIGroups:   []string{""},
					APIVersions: []string{"v1"},
					Resources:   []string{"namespaces"},
					Scope:       scopePtr(admissionregistrationv1.ClusterScope),
				},
			},
			ResourceNames: []string{g.ReleaseNamespace},
		}},
	}
}

func (g *NamespaceDeletionGuard) metadata(name string) metav1.ObjectMeta {
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
			"app.kubernetes.io/component": namespaceDeletionGuardComponent,
		},
	}
}

func (g *NamespaceDeletionGuard) verifyPolicy(policy *admissionregistrationv1.ValidatingAdmissionPolicy) error {
	name := NamespaceDeletionGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	if policy == nil || policy.Name != name {
		return fmt.Errorf("fixed namespace deletion guard policy %s is missing", name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicy", policy.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(policy.Spec, g.policy().Spec) {
		return fmt.Errorf("namespace deletion guard policy spec differs from the immutable contract")
	}
	return nil
}

func (g *NamespaceDeletionGuard) verifyBinding(binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding) error {
	name := NamespaceDeletionGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	if binding == nil || binding.Name != name {
		return fmt.Errorf("fixed namespace deletion guard binding %s is missing", name)
	}
	if err := g.verifyMetadata("ValidatingAdmissionPolicyBinding", binding.ObjectMeta); err != nil {
		return err
	}
	if !reflect.DeepEqual(binding.Spec, g.binding().Spec) {
		return fmt.Errorf("namespace deletion guard binding spec differs from the immutable contract")
	}
	return nil
}

func (g *NamespaceDeletionGuard) verifyMetadata(kind string, metadata metav1.ObjectMeta) error {
	expected := g.metadata(NamespaceDeletionGuardPolicyName(g.ReleaseNamespace, g.ReleaseName))
	if metadata.Name != expected.Name {
		return fmt.Errorf("fixed namespace deletion guard %s has an unexpected name", kind)
	}
	for key, value := range expected.Annotations {
		if metadata.Annotations[key] != value {
			return fmt.Errorf("fixed namespace deletion guard %s has foreign or incomplete ownership", kind)
		}
	}
	for key, value := range expected.Labels {
		if metadata.Labels[key] != value {
			return fmt.Errorf("fixed namespace deletion guard %s has foreign or incomplete ownership", kind)
		}
	}
	return nil
}

func (g *NamespaceDeletionGuard) validate(requirePoll bool) error {
	if g == nil || g.Policies == nil || g.Bindings == nil {
		return fmt.Errorf("namespace deletion guard policy clients are required")
	}
	if g.ReleaseName == "" || g.ReleaseNamespace == "" ||
		g.ReleaseName != strings.TrimSpace(g.ReleaseName) || g.ReleaseNamespace != strings.TrimSpace(g.ReleaseNamespace) {
		return fmt.Errorf("namespace deletion guard release identity is required")
	}
	if requirePoll && g.PollEvery <= 0 {
		return fmt.Errorf("namespace deletion guard poll interval must be positive")
	}
	return nil
}
