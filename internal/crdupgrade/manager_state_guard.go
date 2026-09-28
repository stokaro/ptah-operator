package crdupgrade

import (
	"fmt"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	statusWriteGuardNamePrefix   = "ptah-operator-status-write-guard-"
	unresolvedRunGuardNamePrefix = "ptah-operator-unresolved-run-guard-"
	managerStateGuardComponent   = "manager-state-guard"

	operatorAPIGroup = "operator.ptah.run"
)

// StatusWriteGuardPolicyName returns the name of the release's guard on the
// status subresource of every operator kind.
func StatusWriteGuardPolicyName(releaseNamespace, releaseName string) string {
	return statusWriteGuardNamePrefix + releaseDigest(releaseNamespace, releaseName)
}

// UnresolvedRunGuardPolicyName returns the name of the release's guard on the
// copy of a migration's unresolved-run record.
func UnresolvedRunGuardPolicyName(releaseNamespace, releaseName string) string {
	return unresolvedRunGuardNamePrefix + releaseDigest(releaseNamespace, releaseName)
}

func statusWriteGuardDenialMessage() string {
	return "Ptah status is written only by the operator's manager; settle an unresolved migration run " +
		"with a PtahMigrationRunAcknowledgment"
}

func unresolvedRunGuardDenialMessage() string {
	return "The " + unresolvedRunAnnotation + " annotation is the manager's copy of an unresolved run; " +
		"only the manager changes it, and a PtahMigrationRunAcknowledgment is how a person settles the run"
}

// ManagerStateGuard builds the two policies that keep the state the manager
// decides from in the manager's hands.
//
// Status is where each operator kind records its claims, its admission
// snapshots and the run nobody accounted for, and the Pod-intent and
// controller-write webhooks rebuild intent from it. The first policy refuses a
// write to the status subresource of any kind in the operator's API group
// from anyone but the manager's ServiceAccount. It names the subresource
// rather than the kinds, so a kind added to the group later is covered by the
// rule already there.
//
// The second guards the one thing the manager keeps outside status: the copy
// of a PtahMigration's unresolved-run record on its metadata, which a restore
// that drops status keeps. Anyone may create a PtahMigration that carries it,
// because a restore is exactly that; once the resource exists, only the
// manager adds, changes or removes it. A person settles the run with a
// PtahMigrationRunAcknowledgment, which records who.
//
// This is the only place the policies are written. hack/chartpolicies
// generates templates/manager-state-guard.yaml from them, with the release
// namespace and the controller ServiceAccount left as Helm expressions.
type ManagerStateGuard struct {
	ReleaseName                  string
	ReleaseNamespace             string
	ControllerServiceAccountName string
}

// Policies returns the status guard and the unresolved-run guard.
func (g *ManagerStateGuard) Policies() []AdmissionPolicy {
	return []AdmissionPolicy{g.statusWriteGuard(), g.unresolvedRunGuard()}
}

// managerVariable holds the manager's username in a variable rather than a
// match condition, so the one literal that exempts the manager is a value a
// test can rewrite and watch the verdict follow.
func (g *ManagerStateGuard) managerVariable() admissionregistrationv1.Variable {
	return admissionregistrationv1.Variable{
		Name:       "manager",
		Expression: fmt.Sprintf("%q", "system:serviceaccount:"+g.ReleaseNamespace+":"+g.ControllerServiceAccountName),
	}
}

func (g *ManagerStateGuard) statusWriteGuard() AdmissionPolicy {
	name := StatusWriteGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	anyScope := admissionregistrationv1.AllScopes
	match := managerStateMatch(admissionregistrationv1.NamedRuleWithOperations{
		RuleWithOperations: admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{operatorAPIGroup},
				APIVersions: []string{"*"},
				Resources:   []string{"*/status"},
				Scope:       &anyScope,
			},
		},
	})
	return managerStatePair(name, match, []admissionregistrationv1.Variable{g.managerVariable()},
		admissionregistrationv1.Validation{
			Expression: `request.userInfo.username == variables.manager`,
			Message:    statusWriteGuardDenialMessage(),
		})
}

func (g *ManagerStateGuard) unresolvedRunGuard() AdmissionPolicy {
	name := UnresolvedRunGuardPolicyName(g.ReleaseNamespace, g.ReleaseName)
	namespaced := admissionregistrationv1.NamespacedScope
	match := managerStateMatch(admissionregistrationv1.NamedRuleWithOperations{
		RuleWithOperations: admissionregistrationv1.RuleWithOperations{
			Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
			Rule: admissionregistrationv1.Rule{
				APIGroups:   []string{operatorAPIGroup},
				APIVersions: []string{"*"},
				Resources:   []string{migrationResource},
				Scope:       &namespaced,
			},
		},
	})
	copyOf := func(object string) string {
		return fmt.Sprintf(`has(%[1]s.metadata.annotations) && %[2]q in %[1]s.metadata.annotations ? [%[1]s.metadata.annotations[%[2]q]] : []`,
			object, unresolvedRunAnnotation)
	}
	return managerStatePair(name, match, []admissionregistrationv1.Variable{
		g.managerVariable(),
		{Name: "oldCopy", Expression: copyOf("oldObject")},
		{Name: "newCopy", Expression: copyOf("object")},
	}, admissionregistrationv1.Validation{
		Expression: `request.userInfo.username == variables.manager || variables.newCopy == variables.oldCopy`,
		Message:    unresolvedRunGuardDenialMessage(),
	})
}

func managerStateMatch(rule admissionregistrationv1.NamedRuleWithOperations) *admissionregistrationv1.MatchResources {
	equivalent := admissionregistrationv1.Equivalent
	return &admissionregistrationv1.MatchResources{
		MatchPolicy:       &equivalent,
		NamespaceSelector: &metav1.LabelSelector{},
		ObjectSelector:    &metav1.LabelSelector{},
		ResourceRules:     []admissionregistrationv1.NamedRuleWithOperations{rule},
	}
}

func managerStatePair(
	name string,
	match *admissionregistrationv1.MatchResources,
	variables []admissionregistrationv1.Variable,
	validation admissionregistrationv1.Validation,
) AdmissionPolicy {
	fail := admissionregistrationv1.Fail
	return AdmissionPolicy{
		Policy: &admissionregistrationv1.ValidatingAdmissionPolicy{
			TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicy"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: componentLabels(managerStateGuardComponent)},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
				FailurePolicy:    &fail,
				MatchConstraints: match,
				Variables:        variables,
				Validations:      []admissionregistrationv1.Validation{validation},
			},
		},
		Binding: &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: admissionregistrationv1.SchemeGroupVersion.String(), Kind: "ValidatingAdmissionPolicyBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: componentLabels(managerStateGuardComponent)},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName:        name,
				MatchResources:    match.DeepCopy(),
				ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
			},
		},
	}
}
